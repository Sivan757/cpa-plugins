package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// modelsForTier returns the catalogue a tier may serve.
func (p *pluginState) modelsForTier(ctx context.Context, tier string) []cpakit.ModelInfo {
	if tier == tierGo {
		return p.goModels(ctx)
	}
	return p.freeModels(ctx)
}

// freeModels returns the anonymous catalogue: every id ending in -free plus the
// anonymously served all-round model. Models that answer only on the Responses
// endpoint are dropped, because this executor speaks chat-completions.
func (p *pluginState) freeModels(ctx context.Context) []cpakit.ModelInfo {
	if !p.freeEnabled {
		return nil
	}
	return p.catalog(ctx, tierFree)
}

// goModels returns the paid catalogue.
func (p *pluginState) goModels(ctx context.Context) []cpakit.ModelInfo {
	return p.catalog(ctx, tierGo)
}

// catalog serves the cached model list for a tier, refetching when the entry
// has aged out. A failed refresh keeps the previous snapshot.
func (p *pluginState) catalog(ctx context.Context, tier string) []cpakit.ModelInfo {
	p.cacheMu.Lock()
	entry, ok := p.cache[tier]
	kversion := p.version
	p.cacheMu.Unlock()
	if ok && time.Since(entry.fetchedAt) < catalogTTL {
		return entry.models
	}
	fetched := p.fetchCatalog(ctx, tier, kversion)
	if len(fetched) == 0 {
		return entry.models
	}
	p.cacheMu.Lock()
	p.cache[tier] = cachedCatalog{fetchedAt: time.Now(), models: fetched}
	p.cacheMu.Unlock()

	// A model the catalogue lists for the first time is probed once, so a model
	// that cannot be served never reaches the roster. The probe is skipped while
	// the allowance is drained, because every answer would be inconclusive.
	if tier == tierFree {
		p.verifyNewModels(ctx, fetched)
	}
	return fetched
}

// verifyNewModels probes the free models that have no verdict yet.
//
// The probe is a real request with the smallest possible answer, because the
// tier only reveals whether a model is servable by serving it. A refusal retires
// the model; an inconclusive answer leaves it for a later refresh.
func (p *pluginState) verifyNewModels(ctx context.Context, models []cpakit.ModelInfo) {
	now := time.Now()
	if _, drained := p.limit.remaining(now); drained {
		return
	}
	for _, id := range p.unavailable.unverified(models, now) {
		probe := map[string]any{
			"model":      id,
			"stream":     true,
			"max_tokens": 1,
			"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
			"tools":      ensureRequiredTools(nil),
		}
		plan, errPlan := p.planFor(ctx, cpakit.ExecutorRequest{
			AuthProvider:   providerKey,
			AuthAttributes: map[string]string{"tier": tierFree},
			Model:          id,
		})
		if errPlan != nil {
			continue
		}
		if plan.protocol == protocolResponses {
			converted, errConvert := chatToResponses(probe, id)
			if errConvert != nil {
				continue
			}
			probe = converted
		}
		body, errMarshal := json.Marshal(probe)
		if errMarshal != nil {
			continue
		}
		result, errDo := cpakit.HTTPDo(ctx, http.MethodPost, plan.endpoint, p.requestHeaders(plan, plan.lanes[0], "verify-"+id), body)
		if errDo != nil {
			cpakit.HostLog(ctx, "warn", "opencode: probe of "+id+" failed to reach upstream")
			continue
		}
		if result.StatusCode >= 200 && result.StatusCode < 300 {
			p.unavailable.verify(id, time.Now())
			continue
		}
		if reason := retireReasonOf(result.StatusCode, result.Body); reason != "" {
			p.unavailable.retire(id, reason, time.Now())
			continue
		}
		cpakit.HostLog(ctx, "info", fmt.Sprintf("opencode: probe of %s was inconclusive (HTTP %d); it stays unverified", id, result.StatusCode))
	}
}

// fetchCatalog reads one tier's model list from its gateway.
func (p *pluginState) fetchCatalog(ctx context.Context, tier, version string) []cpakit.ModelInfo {
	base := p.freeCatalogueURL()
	key := ""
	if tier == tierGo {
		// The Go base serves both its catalogue and its chat.
		base = p.goRoot() + "/models"
		if len(p.apiKeys) > 0 {
			key = p.apiKeys[0]
		}
	}
	headers := clientHeaders(version, p.sessions.id("catalog-"+tier), p.sessions.requestID())
	headers.Set("Accept", "application/json")
	if key != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
	var payload struct {
		Data []map[string]any
	}
	result, errCall := cpakit.HTTPJSON(ctx, http.MethodGet, base, headers, nil, &payload)
	if errCall != nil || result == nil || result.StatusCode < 200 || result.StatusCode >= 300 {
		cpakit.HostLog(ctx, "warn", fmt.Sprintf("opencode: %s catalogue fetch failed", tier))
		return nil
	}
	return buildModelList(payload.Data, tier)
}

// buildModelList filters and annotates the raw catalogue entries.
func buildModelList(entries []map[string]any, tier string) []cpakit.ModelInfo {
	out := make([]cpakit.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		id, _ := entry["id"].(string)
		if id == "" {
			continue
		}
		if tier == tierFree && !isFreeModel(id) {
			continue
		}
		limit := limitFor(id)
		out = append(out, cpakit.ModelInfo{
			ID:                        id,
			Name:                      id,
			DisplayName:               id,
			OwnedBy:                   "OpenCode Zen",
			ContextLength:             limit.context,
			InputTokenLimit:           limit.context,
			MaxCompletionTokens:       limit.output,
			OutputTokenLimit:          limit.output,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		})
	}
	return out
}

// isFreeModel reports whether an id belongs to the anonymous free tier.
func isFreeModel(id string) bool {
	return strings.HasSuffix(id, "-free") || id == "big-pickle"
}
