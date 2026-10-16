package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// The roster follows the upstream catalogue, so a model upstream added is
// advertised without a configuration change and one upstream removed stops
// being advertised. That covers discovery and the removal upstream performs.
//
// It does not cover a model the catalogue still lists but the tier cannot serve:
// one that answers only on another protocol, or one retired behind the
// catalogue. Two things handle those. A new model is verified once, before it
// reaches the roster. A model that fails in real traffic is retired immediately.
//
// Only an explicit refusal retires a model. A rate limit, a server error and a
// transport failure say nothing about the model, so they never retire it.

const (
	// verdictRetired marks a model upstream explicitly refused.
	verdictRetired = "retired"
	// verdictVerified marks a model that answered, so it is not probed again.
	verdictVerified = "verified"
	// retiredTTL is how long a retirement stands before the model is offered one
	// more time, so a model fixed upstream returns on its own.
	retiredTTL = 6 * time.Hour
	// verifyPerRefresh bounds the probes one catalogue refresh may send. Probes
	// spend the free allowance, so they are rationed.
	verifyPerRefresh = 2
	// verdictFile is where the verdicts are persisted, under credential_dir. A
	// container needs a mounted path to keep them across a restart.
	verdictFile = "opencode-model-verdicts.json"
)

// modelVerdict records what is known about one model.
type modelVerdict struct {
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// availability remembers the verdict for each model.
type availability struct {
	mu       sync.Mutex
	dir      string
	verdicts map[string]modelVerdict
	loaded   bool
}

// retired reports whether a model is retired and the verdict still stands.
func (a *availability) retired(id string, now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	verdict, ok := a.verdicts[id]
	if !ok || verdict.State != verdictRetired {
		return false
	}
	return now.Sub(verdict.CheckedAt) < retiredTTL
}

// settled reports whether a model already has a verdict that still stands.
func (a *availability) settled(id string, now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	verdict, ok := a.verdicts[id]
	if !ok {
		return false
	}
	if verdict.State == verdictVerified {
		return true
	}
	return now.Sub(verdict.CheckedAt) < retiredTTL
}

// retire records that upstream explicitly refused a model.
func (a *availability) retire(id, reason string, now time.Time) {
	if a == nil || strings.TrimSpace(id) == "" {
		return
	}
	a.mu.Lock()
	if a.verdicts == nil {
		a.verdicts = map[string]modelVerdict{}
	}
	previous := a.verdicts[id]
	a.verdicts[id] = modelVerdict{State: verdictRetired, Reason: reason, CheckedAt: now}
	a.mu.Unlock()

	if previous.State == verdictRetired && previous.Reason == reason {
		return
	}
	cpakit.HostLog(nil, "warn", fmt.Sprintf("opencode: model %s left the roster: %s", id, reason))
	a.persist()
}

// verify records that a model answered.
func (a *availability) verify(id string, now time.Time) {
	if a == nil || strings.TrimSpace(id) == "" {
		return
	}
	a.mu.Lock()
	if a.verdicts == nil {
		a.verdicts = map[string]modelVerdict{}
	}
	previous := a.verdicts[id]
	a.verdicts[id] = modelVerdict{State: verdictVerified, CheckedAt: now}
	a.mu.Unlock()

	if previous.State == verdictVerified {
		return
	}
	cpakit.HostLog(nil, "info", "opencode: model "+id+" answered and joined the roster")
	a.persist()
}

// filter drops the models a standing retirement covers.
func (a *availability) filter(models []cpakit.ModelInfo, now time.Time) []cpakit.ModelInfo {
	if a == nil {
		return models
	}
	a.mu.Lock()
	verdicts := make(map[string]modelVerdict, len(a.verdicts))
	for id, verdict := range a.verdicts {
		verdicts[id] = verdict
	}
	a.mu.Unlock()
	if len(verdicts) == 0 {
		return models
	}
	out := make([]cpakit.ModelInfo, 0, len(models))
	for _, model := range models {
		if verdict, ok := verdicts[model.ID]; ok && verdict.State == verdictRetired && now.Sub(verdict.CheckedAt) < retiredTTL {
			continue
		}
		out = append(out, model)
	}
	return out
}

// unverified lists the models that still need one probe, in roster order, capped
// at the per-refresh budget.
func (a *availability) unverified(models []cpakit.ModelInfo, now time.Time) []string {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, verifyPerRefresh)
	for _, model := range models {
		if len(out) >= verifyPerRefresh {
			break
		}
		verdict, ok := a.verdicts[model.ID]
		if !ok {
			out = append(out, model.ID)
			continue
		}
		// A model retired long enough ago gets one more chance.
		if verdict.State == verdictRetired && now.Sub(verdict.CheckedAt) >= retiredTTL {
			out = append(out, model.ID)
		}
	}
	return out
}

// retireReasonOf reports the reason an upstream answer retires a model, or an
// empty string when the answer says nothing about the model.
//
// A 429 is explicitly inconclusive: the free tier answers it both for a burst
// limit and for a drained daily allowance, and neither concerns the model.
func retireReasonOf(status int, body []byte) string {
	if status == http.StatusTooManyRequests {
		return ""
	}
	text := string(body)
	switch {
	case strings.Contains(text, "ModelProtocolUnsupported"):
		return "upstream serves this model on another protocol"
	case strings.Contains(text, "model_not_found"),
		strings.Contains(text, "ModelNotFound"),
		strings.Contains(text, "is not available in the selected"),
		strings.Contains(text, "does not exist or has been retired"):
		return "upstream no longer serves this model"
	case status == http.StatusNotFound:
		return "upstream does not know this model"
	}
	return ""
}

// documentPath resolves where the verdicts are stored.
func (a *availability) documentPath() string {
	if strings.TrimSpace(a.dir) == "" {
		return ""
	}
	return filepath.Join(a.dir, verdictFile)
}

// load reads the persisted verdicts once.
func (a *availability) load() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.loaded {
		a.mu.Unlock()
		return
	}
	a.loaded = true
	a.mu.Unlock()

	path := a.documentPath()
	if path == "" {
		return
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return
	}
	var stored map[string]modelVerdict
	if errUnmarshal := json.Unmarshal(raw, &stored); errUnmarshal != nil {
		cpakit.HostLog(nil, "warn", "opencode: could not read the model verdict record")
		return
	}
	a.mu.Lock()
	a.verdicts = stored
	a.mu.Unlock()
}

// persist writes the verdicts, best effort.
func (a *availability) persist() {
	if a == nil {
		return
	}
	path := a.documentPath()
	if path == "" {
		return
	}
	a.mu.Lock()
	if a.verdicts == nil {
		a.verdicts = map[string]modelVerdict{}
	}
	raw, errMarshal := json.Marshal(a.verdicts)
	a.mu.Unlock()
	if errMarshal != nil {
		return
	}
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		cpakit.HostLog(nil, "warn", "opencode: could not write the model verdict record: "+cpakit.Redact(errWrite.Error()))
	}
}
