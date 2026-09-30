package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// Route fragments are concatenated onto region.apiBase, which already ends in a
// slash. The "/algo" prefix is part of the upstream's signed surface.
const (
	qoderCatalogRoute = "algo/api/v2/model/list?Encode=1"
	qoderChatRoute    = "algo/api/v2/service/pro/sse/agent_chat_generation"
	qoderChatQuery    = "?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
)

// catalogScenePreference is the order in which the response's model scenes are
// tried. The upstream returns several scene arrays with identical content; the
// reference implementations disagree about which one to read, so all three are
// accepted and the rest of the document is scanned as a fallback.
var catalogScenePreference = []string{"assistant", "chat", "developer"}

type catalogContextTier struct {
	TokenCount *int64 `json:"token_count"`
	IsDefault  *bool  `json:"is_default"`
}

type catalogEffort struct {
	Description string `json:"description"`
	IsDefault   bool   `json:"is_default"`
}

type catalogThinkingHalf struct {
	Efforts map[string]catalogEffort `json:"efforts"`
}

// catalogThinkingConfig carries two generations of the reasoning declaration:
// the enabled/disabled pair is the current shape, is_reasoning the legacy one.
type catalogThinkingConfig struct {
	Enabled  *catalogThinkingHalf `json:"enabled"`
	Disabled *catalogThinkingHalf `json:"disabled"`
}

// catalogEntry is one model row. The wire uses snake_case throughout, and both
// the snake_case and camelCase spellings of a few fields are seen in the wild.
type catalogEntry struct {
	Key               string                        `json:"key"`
	Enable            bool                          `json:"enable"`
	DisplayName       string                        `json:"display_name"`
	MaxInputTokens    int64                         `json:"max_input_tokens"`
	MaxOutputTokens   int64                         `json:"max_output_tokens"`
	ContextConfig     map[string]catalogContextTier `json:"context_config"`
	IsReasoning       bool                          `json:"is_reasoning"`
	ThinkingConfig    *catalogThinkingConfig        `json:"thinking_config"`
	Source            string                        `json:"source"`
	PriceFactor       *float64                      `json:"price_factor"`
	OriginalPrice     *float64                      `json:"original_price_factor"`
	OriginPriceFactor *float64                      `json:"originPriceFactor"`
	IsFree            bool                          `json:"is_free"`
	IsFreeCamel       bool                          `json:"isFree"`
	Tags              []string                      `json:"tags"`
	IsVL              bool                          `json:"is_vl"`
}

// reasoningEffortRank orders the effort identifiers the plugin advertises. An
// identifier the map does not know sorts last, preserving the upstream's own
// relative order is impossible without a rank, so unknown ones stay grouped.
var reasoningEffortRank = map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}

const (
	// defaultContextFallback is used when a row declares no usable input budget.
	defaultContextFallback = int64(180_000)
	// defaultOutputFallback matches the reference client's output cap.
	defaultOutputFallback = int64(32_768)
)

// parseCatalogDocument extracts the model rows from a catalog payload and
// reports which scene they came from. The scene name is diagnostic only.
func parseCatalogDocument(raw []byte) ([]catalogEntry, string, error) {
	var scenes map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &scenes); errUnmarshal != nil {
		return nil, "", pluginkit.NewError("upstream_error", "Qoder catalog is not a JSON object: "+errUnmarshal.Error(), http.StatusBadGateway)
	}
	for _, scene := range catalogScenePreference {
		if entries, okEntries := decodeScene(scenes[scene]); okEntries {
			return entries, scene, nil
		}
	}
	// The upstream has renamed scenes before; accept any array that decodes as
	// model rows rather than pinning a name that may disappear.
	names := make([]string, 0, len(scenes))
	for name := range scenes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if containsFold(catalogScenePreference, name) {
			continue
		}
		if entries, okEntries := decodeScene(scenes[name]); okEntries {
			return entries, name, nil
		}
	}
	return nil, "", pluginkit.NewError("upstream_error", "Qoder catalog carried no model scene", http.StatusBadGateway)
}

func decodeScene(raw json.RawMessage) ([]catalogEntry, bool) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, false
	}
	var entries []catalogEntry
	if errUnmarshal := json.Unmarshal(raw, &entries); errUnmarshal != nil {
		return nil, false
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Key) != "" {
			return entries, len(entries) > 0
		}
	}
	return nil, false
}

// readCatalog fetches and parses the model roster for one credential.
func readCatalog(ctx context.Context, creds *credentials, identity cosyIdentity) ([]catalogEntry, string, error) {
	url := creds.Region.apiBase + qoderCatalogRoute
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	result, errCall := signedRequest(ctx, creds.Region, http.MethodGet, url, identity, nil, headers)
	if errCall != nil {
		return nil, "", errCall
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return nil, "", classifyOpenAPIError(result.StatusCode, "model catalog", result.Body)
	}
	entries, scene, errParse := parseCatalogDocument(result.Body)
	if errParse != nil {
		return nil, "", errParse
	}
	return entries, scene, nil
}

// buildModels converts catalog rows into host model metadata.
//
// Only rows that are enabled and carry a usable key become models: an entry the
// upstream would refuse to route must not appear in a picker.
func buildModels(reg *region, entries []catalogEntry) ([]pluginkit.ModelInfo, map[string]string) {
	out := make([]pluginkit.ModelInfo, 0, len(entries))
	routes := make(map[string]string, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for index := range entries {
		entry := &entries[index]
		key := strings.TrimSpace(entry.Key)
		if key == "" || !entry.Enable {
			continue
		}
		displayName := firstNonEmpty(entry.DisplayName, key)
		// The model id is the human-readable display name, lowercased so ids
		// stay uniform across vendors: that is what both the OpenAI /v1/models
		// listing and the management picker show as the title.
		id := strings.ToLower(displayName)
		// Rows whose display name does not identify a vendor are internal
		// routing slots (auto, tier names). They cannot be matched to a
		// provider, so they are not advertised.
		if !identifiableVendor(id) {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		routes[id] = key
		out = append(out, modelFromEntry(reg, entry, id, key))
	}
	return out, routes
}

// vendorPrefixes are the model-family prefixes this plugin can attribute to a
// vendor. A display name outside these families is an internal routing slot
// rather than a sellable model.
var vendorPrefixes = []string{"qwen", "deepseek", "glm", "kimi", "minimax", "hy"}

// identifiableVendor reports whether a model id names a vendor family.
func identifiableVendor(id string) bool {
	for _, prefix := range vendorPrefixes {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

func modelFromEntry(reg *region, entry *catalogEntry, id, routeKey string) pluginkit.ModelInfo {
	contextLength := effectiveContextWindow(entry)

	info := pluginkit.ModelInfo{
		ID:      id,
		Object:  "model",
		OwnedBy: reg.displayName,
		// The multiplier travels on the display name because the picker-facing
		// endpoints preserve display_name but drop description; keeping the id
		// itself clean leaves API callers unaffected.
		DisplayName:         describeModel(entry, id),
		Name:                routeKey,
		Description:         describeModel(entry, id),
		InputTokenLimit:     firstPositive(entry.MaxInputTokens, contextLength),
		OutputTokenLimit:    firstPositive(entry.MaxOutputTokens, defaultOutputFallback),
		ContextLength:       contextLength,
		MaxCompletionTokens: firstPositive(entry.MaxOutputTokens, defaultOutputFallback),
		UserDefined:         false,
	}
	if entry.IsVL {
		info.SupportedInputModalities = []string{"text", "image"}
	} else {
		info.SupportedInputModalities = []string{"text"}
	}
	info.SupportedOutputModalities = []string{"text"}
	info.SupportedParameters = []string{"tools"}
	if thinking := thinkingSupport(entry); thinking != nil {
		info.Thinking = thinking
	}
	return info
}

// effectiveContextWindow prefers the single default context tier, because the
// upstream sizes the request from that tier rather than from max_input_tokens.
func effectiveContextWindow(entry *catalogEntry) int64 {
	if tier, okTier := defaultContextTier(entry.ContextConfig); okTier {
		return tier
	}
	return firstPositive(entry.MaxInputTokens, defaultContextFallback)
}

func defaultContextTier(config map[string]catalogContextTier) (int64, bool) {
	var chosen int64
	count := 0
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		tier := config[key]
		if tier.TokenCount == nil || *tier.TokenCount <= 0 {
			continue
		}
		if tier.IsDefault != nil && *tier.IsDefault {
			chosen = *tier.TokenCount
			count++
		}
	}
	if count == 1 {
		return chosen, true
	}
	return 0, false
}

// contextTierNames lists the selectable tiers smallest-first, for diagnostics.
func contextTierNames(config map[string]catalogContextTier) []string {
	type tier struct {
		name  string
		token int64
	}
	tiers := make([]tier, 0, len(config))
	for name, value := range config {
		if value.TokenCount == nil || *value.TokenCount <= 0 {
			continue
		}
		tiers = append(tiers, tier{name: name, token: *value.TokenCount})
	}
	sort.Slice(tiers, func(left, right int) bool {
		if tiers[left].token != tiers[right].token {
			return tiers[left].token < tiers[right].token
		}
		return tiers[left].name < tiers[right].name
	})
	out := make([]string, 0, len(tiers))
	for _, item := range tiers {
		out = append(out, fmt.Sprintf("%s=%d", item.name, item.token))
	}
	return out
}

// thinkingSupport maps the upstream reasoning declaration onto the host's
// thinking controls.
//
// A model that declares no effort list gets no controls: the plugin never
// invents wire values the upstream has not confirmed.
func thinkingSupport(entry *catalogEntry) *pluginkit.ThinkingSupport {
	enabled := entry.ThinkingConfig
	if enabled == nil || enabled.Enabled == nil || len(enabled.Enabled.Efforts) == 0 {
		return nil
	}
	levels := make([]string, 0, len(enabled.Enabled.Efforts))
	for id := range enabled.Enabled.Efforts {
		if strings.TrimSpace(id) != "" {
			levels = append(levels, id)
		}
	}
	if len(levels) == 0 {
		return nil
	}
	sort.Slice(levels, func(left, right int) bool {
		leftRank, okLeft := reasoningEffortRank[levels[left]]
		if !okLeft {
			leftRank = len(reasoningEffortRank)
		}
		rightRank, okRight := reasoningEffortRank[levels[right]]
		if !okRight {
			rightRank = len(reasoningEffortRank)
		}
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return levels[left] < levels[right]
	})
	return &pluginkit.ThinkingSupport{Levels: levels}
}

// describeModel folds the routing source and the price multiplier into the
// description, which is what a model picker surfaces.
//
// No capability is claimed beyond what the catalog states.
func describeModel(entry *catalogEntry, displayName string) string {
	notes := make([]string, 0, 2)
	if entry.IsFree || entry.IsFreeCamel || containsFold(entry.Tags, "limited_time_free") {
		notes = append(notes, "倍率免费")
	} else if entry.PriceFactor != nil && *entry.PriceFactor == 0 {
		notes = append(notes, "倍率免费")
	} else if entry.PriceFactor != nil {
		notes = append(notes, fmt.Sprintf("倍率 x%g", *entry.PriceFactor))
	}
	if entry.IsVL {
		notes = append(notes, "支持图片")
	}
	if len(notes) == 0 {
		return displayName
	}
	return displayName + " · " + strings.Join(notes, " · ")
}
