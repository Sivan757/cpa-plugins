package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// catalogCache keeps one short-lived catalog snapshot so a model list request
// immediately followed by a chat request does not hit /v3/config twice.
type catalogCache struct {
	mu       sync.Mutex
	models   []pluginkit.ModelInfo
	loadedAt time.Time
	ttl      time.Duration
}

func (c *catalogCache) get() ([]pluginkit.ModelInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.models) == 0 || time.Since(c.loadedAt) > c.ttl {
		return nil, false
	}
	return c.models, true
}

func (c *catalogCache) put(models []pluginkit.ModelInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = models
	c.loadedAt = time.Now()
}

// modelsForAuth discovers the live model roster for one credential.
func (p *pluginState) modelsForKind(ctx context.Context, kind *credentialKind, creds *credentials) ([]pluginkit.ModelInfo, error) {
	if cached, ok := p.cache.get(); ok {
		return cached, nil
	}
	appVersion := p.appVersion(kind)
	doc, errCatalog := readCatalog(ctx, kind, creds, appVersion)
	if errCatalog != nil {
		return nil, errCatalog
	}
	models := buildModels(kind, doc, time.Now())
	if len(models) == 0 {
		return nil, pluginkit.NewError("upstream_error", "the model catalog contained no usable models", 502)
	}
	p.cache.put(models)
	return models, nil
}

// buildModels converts a catalog document into host model metadata.
//
// The roster the plugin advertises is the intersection of the models the
// account may call (agents[name=cli].models) and the rows that carry usable
// token limits, preserving the roster's order.
func buildModels(v *credentialKind, doc *catalogDocument, now time.Time) []pluginkit.ModelInfo {
	byID := make(map[string]modelRow, len(doc.Models))
	for _, row := range doc.Models {
		if row.ID != "" {
			byID[row.ID] = row
		}
	}
	roster := cliRoster(doc)
	order := roster
	if len(order) == 0 {
		order = make([]string, 0, len(doc.Models))
		for _, row := range doc.Models {
			order = append(order, row.ID)
		}
	}

	out := make([]pluginkit.ModelInfo, 0, len(order))
	seen := make(map[string]struct{}, len(order))
	for _, id := range order {
		if _, dup := seen[id]; dup {
			continue
		}
		row, ok := byID[id]
		if !ok || row.Disabled {
			continue
		}
		if row.MaxInputTokens <= 0 || row.MaxOutputTokens <= 0 {
			continue
		}
		seen[id] = struct{}{}

		displayName := row.Name
		if displayName == "" {
			displayName = id
		}
		contextLength := row.MaxInputTokens
		if v.region == "global" && row.ContextWindow != nil && row.ContextWindow.DefaultLength > 0 {
			contextLength = row.ContextWindow.DefaultLength
		}

		info := pluginkit.ModelInfo{
			ID:                  id,
			Object:              "model",
			OwnedBy:             v.displayName,
			DisplayName:         displayName,
			Name:                id,
			Description:         firstNonEmpty(row.DescriptionZh, row.DescriptionEn),
			InputTokenLimit:     row.MaxInputTokens,
			OutputTokenLimit:    row.MaxOutputTokens,
			ContextLength:       contextLength,
			MaxCompletionTokens: row.MaxOutputTokens,
			UserDefined:         false,
		}
		if row.SupportsImages && !row.DisabledMultimodal {
			info.SupportedInputModalities = []string{"text", "image"}
		} else {
			info.SupportedInputModalities = []string{"text"}
		}
		info.SupportedOutputModalities = []string{"text"}
		if row.SupportsToolCall {
			info.SupportedParameters = append(info.SupportedParameters, "tools")
		}
		if thinking := thinkingSupport(row); thinking != nil {
			info.Thinking = thinking
		}
		// The picker-facing endpoints preserve display_name but drop
		// description, so the multiplier rides on the display name while the
		// id stays lowercase and clean for API callers.
		info.DisplayName = decorateDescription(row, doc, now)
		info.Description = info.DisplayName
		out = append(out, info)
	}
	return out
}

// thinkingSupport maps the upstream reasoning declaration onto the host's
// thinking controls. A model that declares no effort list gets no controls: the
// plugin never invents wire values the upstream has not confirmed.
func thinkingSupport(row modelRow) *pluginkit.ThinkingSupport {
	if !row.SupportsReasoning || row.Reasoning == nil {
		return nil
	}
	levels := make([]string, 0, len(row.Reasoning.SupportedEfforts))
	for _, effort := range row.Reasoning.SupportedEfforts {
		if effort != "" {
			levels = append(levels, effort)
		}
	}
	if len(levels) == 0 {
		return nil
	}
	return &pluginkit.ThinkingSupport{
		Levels:      levels,
		ZeroAllowed: row.Reasoning.CanDisableThinking,
	}
}

// decorateDescription folds the credit multiplier and any active promotion into
// the model description, which is what CPA surfaces in model listings.
func decorateDescription(row modelRow, doc *catalogDocument, now time.Time) string {
	description := firstNonEmpty(row.DescriptionZh, row.DescriptionEn)
	var notes []string
	if multiplier, free := normalizeCredits(row.Credits); multiplier != "" {
		if free {
			multiplier += " free"
		}
		notes = append(notes, multiplier)
	}
	if promo := activePromotion(doc, row.ID, now); promo != nil {
		switch {
		case promo.Discount != nil && promo.Discount.Factor != nil:
			notes = append(notes, formatCredits(*promo.Discount.Factor))
		case promo.Badge != nil && promo.Badge.Label != "":
			notes = append(notes, promo.Badge.Label)
		}
	}
	if len(notes) == 0 {
		return description
	}
	joined := strings.Join(notes, " ")
	if description == "" {
		return joined
	}
	return joined + " · " + description
}

// cliRoster extracts the CLI-callable model identifiers from the catalog.
func cliRoster(doc *catalogDocument) []string {
	for _, agent := range doc.Agents {
		if strings.EqualFold(agent.Name, "cli") {
			return agent.Models
		}
	}
	return nil
}

// staticFallback is the last-resort roster used when the catalog is
// unreachable, so a model picker never goes empty. It carries no metrics the
// plugin cannot verify: limits are conservative placeholders.
func staticFallback(v *credentialKind) []pluginkit.ModelInfo {
	var ids []string
	if v.region == "cn" {
		ids = []string{
			"hy4-preview", "hy3", "hy3-x", "deepseek-v4.1-flash", "glm-5.3", "glm-5.3-flash",
			"glm-5.2", "glm-5.1", "glm-5v-turbo", "minimax-m3", "minimax-m2.7", "kimi-k3-1",
			"kimi-k2.8-preview", "kimi-k2.7", "kimi-k2.6", "deepseek-v4-pro",
		}
	} else {
		ids = []string{
			"default-model", "fast-model", "balanced-model", "primary-model", "deep-model",
			"hy4-preview-f", "hy3", "deepseek-v4.1-flash", "gpt-6-astra", "gpt-5.6-sol",
			"gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "gpt-5.3-codex",
			"gemini-3.5-flash", "glm-5.3", "glm-5.2", "kimi-k3", "kimi-k2.6",
		}
	}
	out := make([]pluginkit.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginkit.ModelInfo{
			ID:                        id,
			Object:                    "model",
			OwnedBy:                   v.displayName,
			DisplayName:               id,
			Name:                      id,
			ContextLength:             200000,
			InputTokenLimit:           200000,
			OutputTokenLimit:          32000,
			MaxCompletionTokens:       32000,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
			Description:               "静态兜底清单：" + v.displayName + " 目录不可达",
		})
	}
	return out
}
