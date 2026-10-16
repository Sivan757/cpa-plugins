package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// freeAuthFileName is the synthetic auth record that carries the anonymous
// tier. The host routes a request to this plugin only when a matching auth
// record exists, so the free tier needs one even though it has no secret.
const freeAuthFileName = "opencode-free.json"

// materializeTiers writes the auth records the configured tiers need, once.
//
// The free tier has no credential to upload, but CPA still requires an auth
// record bound to this provider key before it routes a free model here. A paid
// key configured in the plugin block gets its own record. Existing records are
// left untouched so host-managed metadata survives a reconfigure.
func (p *pluginState) materializeTiers() {
	p.mu.Lock()
	if p.materialized {
		p.mu.Unlock()
		return
	}
	p.materialized = true
	keys := append([]string(nil), p.apiKeys...)
	p.mu.Unlock()

	ctx := context.Background()
	bridge := cpakit.Bridge()
	if bridge == nil {
		return
	}
	entries, errList := bridge.AuthList(ctx)
	if errList != nil {
		cpakit.HostLog(ctx, "warn", "opencode: could not list auth records: "+errList.Error())
		return
	}
	existing := map[string]bool{}
	for _, entry := range entries {
		if name := strings.TrimSpace(entry.Name); name != "" {
			existing[name] = true
		}
		if id := strings.TrimSpace(entry.ID); id != "" {
			existing[id] = true
		}
	}

	if !existing[freeAuthFileName] {
		record, errMarshal := json.Marshal(map[string]any{
			"type":  providerKey,
			"tier":  tierFree,
			"id":    "opencode-free",
			"label": "OpenCode Zen free tier",
		})
		if errMarshal == nil {
			if errSave := bridge.AuthSave(ctx, cpakit.HostAuthSaveRequest{
				Name: freeAuthFileName,
				JSON: record,
			}); errSave != nil {
				cpakit.HostLog(ctx, "warn", "opencode: could not save the free-tier auth record: "+errSave.Error())
			} else {
				cpakit.HostLog(ctx, "info", "opencode: created the free-tier auth record")
			}
		}
	}

	for index, key := range keys {
		digest := sha256.Sum256([]byte(key))
		hash := hex.EncodeToString(digest[:])
		id := "opencode-go-" + hash
		name := id + ".json"
		if existing[name] || existing[id] {
			continue
		}
		record, errMarshal := json.Marshal(map[string]any{
			"type":    providerKey,
			"tier":    tierGo,
			"id":      id,
			"label":   fmt.Sprintf("OpenCode Go key %d (%s)", index+1, hash[:12]),
			"api_key": key,
		})
		if errMarshal != nil {
			continue
		}
		if errSave := bridge.AuthSave(ctx, cpakit.HostAuthSaveRequest{Name: name, JSON: record}); errSave != nil {
			cpakit.HostLog(ctx, "warn", "opencode: could not save a Go auth record: "+errSave.Error())
		}
	}
}

// freeRoot resolves the free-tier upstream root.
func (p *pluginState) freeRoot() string {
	if p.baseURL != "" {
		return p.baseURL
	}
	return freeBaseURL
}

// advertised narrows a catalogue to what the roster may publish: the optional
// expose allowlist, then the models an explicit refusal retired.
//
// The catalogue itself already decides what exists, so a model upstream added is
// advertised and one upstream removed disappears. The retirement list covers the
// models the catalogue still lists but the tier cannot serve.
func (p *pluginState) advertised(models []cpakit.ModelInfo, tier string) []cpakit.ModelInfo {
	if tier == tierGo {
		// A paid model is published only when it is named, so an unset key hides
		// the whole paid catalogue rather than publishing all of it.
		if len(p.exposeGo) == 0 {
			return nil
		}
		return p.unavailable.filter(cpakit.ExposeOnly(models, p.exposeGo), time.Now())
	}
	return p.unavailable.filter(cpakit.ExposeOnly(models, p.expose), time.Now())
}

// noteModelOutcome records what an upstream answer says about a model. A refusal
// retires it; a served answer returns it.
func (p *pluginState) noteModelOutcome(model string, status int, body []byte) {
	if p == nil || strings.TrimSpace(model) == "" {
		return
	}
	if status >= 200 && status < 300 {
		p.unavailable.verify(model, time.Now())
		return
	}
	if reason := retireReasonOf(status, body); reason != "" {
		p.unavailable.retire(model, reason, time.Now())
	}
}

// responsesRoot resolves the endpoint for models served only by the Responses
// API. An explicit base_url override targets the chat root, so the responses
// path is derived from it to keep a local capture endpoint usable.
func (p *pluginState) responsesRoot() string {
	if p.baseURL != "" {
		return p.baseURL + "/responses"
	}
	return responsesBaseURL
}

// freeCatalogueURL resolves where the free-tier model list is read.
//
// The chat base is not the catalogue base: chat posts to .../inference/openai/v1
// while the list lives at .../inference/v1/models. An explicit base_url override
// targets a single root, so the catalogue follows it, which keeps a local capture
// endpoint usable.
func (p *pluginState) freeCatalogueURL() string {
	if p.baseURL != "" {
		return p.baseURL + "/models"
	}
	return freeModelsURL
}

// goRoot resolves the paid-tier upstream root.
func (p *pluginState) goRoot() string {
	if p.goURL != "" {
		return p.goURL
	}
	return goBaseURL
}

// splitKeys turns a comma-separated key list into its members.
func splitKeys(raw string) []string {
	out := make([]string, 0, 2)
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
