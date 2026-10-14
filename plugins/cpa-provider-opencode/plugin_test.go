// Helpers and tests for the plugin's request handling. The identifier format
// is pinned separately in identity_test.go.
package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// TestEnsureRequiredTools pins the free-tier shape rule: bash and read must be
// present by name, an already-qualifying list must not be rewritten, and the
// padding must stay argument-free.
func TestEnsureRequiredTools(t *testing.T) {
	filled := ensureRequiredTools(nil)
	if len(filled) != 2 {
		t.Fatalf("padded tool count = %d, want 2", len(filled))
	}
	for _, name := range []string{"bash", "read"} {
		found := false
		for _, entry := range filled {
			if toolName(entry) == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("padded list is missing %q", name)
		}
	}

	// A capitalised declaration does not satisfy the gate, so the lowercase
	// stand-in is appended alongside it.
	mixed := []any{map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}}}
	if len(ensureRequiredTools(mixed)) != 3 {
		t.Fatalf("capitalised tool name wrongly satisfied the gate")
	}

	// An already-qualifying list keeps its length and order so the upstream
	// prompt cache is not invalidated.
	qualifying := []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "bash"}},
		map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
		map[string]any{"type": "function", "function": map[string]any{"name": "glob"}},
	}
	unchanged := ensureRequiredTools(qualifying)
	if len(unchanged) != 3 {
		t.Fatalf("qualifying list was rewritten to %d entries", len(unchanged))
	}
	if toolName(unchanged[2]) != "glob" {
		t.Fatalf("declared tool order changed: %q is no longer third", toolName(unchanged[2]))
	}
}

// TestPrepareBodyForcesStreamAndModel pins the two rewrites the free tier
// checks on the wire.
func TestPrepareBodyForcesStreamAndModel(t *testing.T) {
	state := newPluginState()
	payload := []byte(`{"model":"stale-id","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	plan := &executionPlan{tier: tierFree, baseURL: freeBaseURL, model: "mimo-v2.6-flash-free", key: publicCredential}
	prepared, errPrepare := state.prepareBody(pluginkit.ExecutorRequest{Payload: payload}, plan)
	if errPrepare != nil {
		t.Fatalf("prepareBody: %v", errPrepare)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(prepared, &decoded); errUnmarshal != nil {
		t.Fatalf("decode prepared body: %v", errUnmarshal)
	}
	if decoded["stream"] != true {
		t.Fatalf("stream = %v, want true", decoded["stream"])
	}
	if decoded["model"] != "mimo-v2.6-flash-free" {
		t.Fatalf("model = %v, want the upstream id", decoded["model"])
	}
	tools, okTools := decoded["tools"].([]any)
	if !okTools || len(tools) != 2 {
		t.Fatalf("free-tier request carries %d tools, want the two placeholders", len(tools))
	}
}

// TestClassifyUpstreamError pins the refusal mapping: a free-tier refusal must
// not be reported as an invalid credential, which would cool the whole pool.
func TestClassifyUpstreamError(t *testing.T) {
	refusal := classifyUpstreamError(http.StatusForbidden, []byte(`{"type":"error","error":{"type":"FreeTierError"}}`))
	if refusal.Code != "free_tier_refused" {
		t.Fatalf("FreeTierError mapped to %q, want free_tier_refused", refusal.Code)
	}
	if refusal.HTTPStatus == http.StatusUnauthorized {
		t.Fatalf("free-tier refusal must not surface as 401")
	}
	upgrade := classifyUpstreamError(http.StatusForbidden, []byte(`{"error":{"type":"UpgradeRequired"}}`))
	if upgrade.Code != "client_version_too_old" {
		t.Fatalf("UpgradeRequired mapped to %q, want client_version_too_old", upgrade.Code)
	}
	limited := classifyUpstreamError(http.StatusTooManyRequests, []byte(`{"error":{"type":"FreeUsageLimitError"}}`))
	if limited.Code != "rate_limited" {
		t.Fatalf("429 mapped to %q, want rate_limited", limited.Code)
	}
}

// TestTierAndKeyResolution pins how an auth record selects its tier.
func TestTierAndKeyResolution(t *testing.T) {
	goRecord := []byte(`{"type":"opencode","tier":"go","api_key":"oc_sk_example"}`)
	if tier := tierOf(nil, goRecord); tier != tierGo {
		t.Fatalf("tier = %q, want go", tier)
	}
	if key := keyOf(nil, goRecord); key != "oc_sk_example" {
		t.Fatalf("key = %q, want the record value", key)
	}
	freeRecord := []byte(`{"type":"opencode"}`)
	if tier := tierOf(nil, freeRecord); tier != tierFree {
		t.Fatalf("absent tier marker resolved to %q, want free", tier)
	}
}
