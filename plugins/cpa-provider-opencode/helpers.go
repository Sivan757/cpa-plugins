package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

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
