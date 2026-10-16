package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// TestOnlyAnExplicitRefusalRetiresAModel pins the line between "this model is not
// served" and "this answer says nothing about the model". A rate limit must never
// retire a model: the free tier answers 429 both for a burst limit and for a
// drained daily allowance.
func TestOnlyAnExplicitRefusalRetiresAModel(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"protocol unsupported", http.StatusBadRequest, `{"error":{"type":"ModelProtocolUnsupported"}}`, true},
		{"model not found", http.StatusBadRequest, `{"error":{"type":"model_not_found"}}`, true},
		{"retired upstream", http.StatusBadRequest, `{"message":"does not exist or has been retired"}`, true},
		{"unknown path", http.StatusNotFound, `{"error":"nope"}`, true},
		{"drained allowance", http.StatusTooManyRequests, `{"error":{"type":"FreeUsageLimitError"}}`, false},
		{"burst limit", http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error"}}`, false},
		{"server error", http.StatusBadGateway, `{"error":"endpoint unavailable"}`, false},
		{"refused fingerprint", http.StatusForbidden, `{"type":"FreeTierError"}`, false},
		{"served", http.StatusOK, `{"ok":true}`, false},
	}
	for _, testCase := range cases {
		got := retireReasonOf(testCase.status, []byte(testCase.body)) != ""
		if got != testCase.want {
			t.Fatalf("%s: retired = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

// TestRetiredModelLeavesAndReturns proves a retirement hides a model from the
// roster and expires, so a model fixed upstream comes back on its own.
func TestRetiredModelLeavesAndReturns(t *testing.T) {
	state := newPluginState()
	models := []cpakit.ModelInfo{{ID: "good-free"}, {ID: "broken-free"}}

	now := time.Now()
	state.unavailable.retire("broken-free", "upstream serves this model on another protocol", now)

	advertised := state.advertised(models, tierFree)
	if len(advertised) != 1 || advertised[0].ID != "good-free" {
		t.Fatalf("advertised = %v, want only the served model", advertised)
	}

	// The verdict expires, so the model is offered again.
	expired := now.Add(retiredTTL + time.Minute)
	visible := state.unavailable.filter(models, expired)
	if len(visible) != 2 {
		t.Fatalf("after the verdict expired %d models are visible, want both", len(visible))
	}
}

// TestRefusedModelIsRetiredByRealTraffic proves the roster learns from a request
// instead of relying on a hand-maintained list.
func TestRefusedModelIsRetiredByRealTraffic(t *testing.T) {
	state := newPluginState()
	state.noteModelOutcome("mystery-free", http.StatusBadRequest, []byte(`{"error":{"type":"ModelProtocolUnsupported"}}`))

	models := []cpakit.ModelInfo{{ID: "mystery-free"}}
	if got := state.advertised(models, tierFree); len(got) != 0 {
		t.Fatalf("a refused model is still advertised: %v", got)
	}
}

// TestInconclusiveAnswerKeepsTheModel proves a rate limit leaves the roster alone.
func TestInconclusiveAnswerKeepsTheModel(t *testing.T) {
	state := newPluginState()
	state.noteModelOutcome("busy-free", http.StatusTooManyRequests, []byte(`{"error":{"type":"FreeUsageLimitError"}}`))

	models := []cpakit.ModelInfo{{ID: "busy-free"}}
	if got := state.advertised(models, tierFree); len(got) != 1 {
		t.Fatalf("a rate-limited model was dropped from the roster: %v", got)
	}
}

// TestVerificationIsBoundedPerRefresh proves the probes are rationed, because
// each one spends the free allowance.
func TestVerificationIsBoundedPerRefresh(t *testing.T) {
	state := newPluginState()
	models := []cpakit.ModelInfo{}
	for _, id := range []string{"a-free", "b-free", "c-free", "d-free"} {
		models = append(models, cpakit.ModelInfo{ID: id})
	}
	got := state.unavailable.unverified(models, time.Now())
	if len(got) != verifyPerRefresh {
		t.Fatalf("unverified returned %d models, want the budget of %d", len(got), verifyPerRefresh)
	}
	if got[0] != "a-free" || got[1] != "b-free" {
		t.Fatalf("unverified = %v, want the first models in roster order", got)
	}
}

// TestVerdictsSurviveARestart proves the record is persisted under
// credential_dir, so a restart does not re-probe every model.
func TestVerdictsSurviveARestart(t *testing.T) {
	dir := t.TempDir()

	state := newPluginState()
	state.unavailable.dir = dir
	state.unavailable.retire("broken-free", "upstream no longer serves this model", time.Now())

	reloaded := newPluginState()
	reloaded.unavailable.dir = dir
	reloaded.unavailable.load()

	if !reloaded.unavailable.retired("broken-free", time.Now()) {
		t.Fatal("the retirement did not survive a reload")
	}
}

// TestExposeStillNarrows proves the allowlist remains available as a manual
// override even though the roster follows the catalogue by default.
func TestExposeStillNarrows(t *testing.T) {
	state := newPluginState()
	state.expose = []string{"wanted-free"}
	models := []cpakit.ModelInfo{{ID: "wanted-free"}, {ID: "other-free"}}
	got := state.advertised(models, tierFree)
	if len(got) != 1 || got[0].ID != "wanted-free" {
		t.Fatalf("advertised = %v, want the expose list honoured", got)
	}
}

// TestPaidTierIsOptIn proves the paid catalogue is published only when named.
// The free tier is what this provider exists for, and the paid models were
// previously hidden by hand through the expose list.
func TestPaidTierIsOptIn(t *testing.T) {
	state := newPluginState()
	paid := []cpakit.ModelInfo{{ID: "kimi-k3"}, {ID: "deepseek-v4.1-flash"}}

	if got := state.advertised(paid, tierGo); len(got) != 0 {
		t.Fatalf("the paid tier published %v with no expose_go configured", got)
	}

	state.exposeGo = []string{"kimi-k3"}
	got := state.advertised(paid, tierGo)
	if len(got) != 1 || got[0].ID != "kimi-k3" {
		t.Fatalf("advertised = %v, want only the named paid model", got)
	}
}

// TestFreeTierIsAutomatic proves the free roster needs no configuration: an
// unset expose publishes the whole free catalogue, which is how a model upstream
// added appears on its own.
func TestFreeTierIsAutomatic(t *testing.T) {
	state := newPluginState()
	free := []cpakit.ModelInfo{{ID: "glm-5.3-flash-free"}, {ID: "ll-alpha-free"}}
	if got := state.advertised(free, tierFree); len(got) != 2 {
		t.Fatalf("the free tier published %d models, want the whole catalogue", len(got))
	}
}
