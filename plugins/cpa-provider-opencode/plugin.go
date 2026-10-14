package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// providerKey is the single provider key this plugin serves. Both credential
// tiers sit behind it, so overlapping model identifiers never compete across
// plugins.
const providerKey = "opencode"

// pluginName is the management-visible plugin name.
const pluginName = "CLIProxyAPI OpenCode Provider"

// Upstream roots. The paid Go plan and the anonymous free tier are separate
// gateways that happen to share a model catalogue.
const (
	freeBaseURL = "https://opencode.ai/zen/v1"
	goBaseURL   = "https://opencode.ai/zen/go/v1"
)

// catalogTTL keeps a fetched catalogue briefly so the model-list request the
// host makes immediately before a chat does not hit the upstream twice.
const catalogTTL = 10 * time.Minute

// Tier names as they appear in an auth record.
const (
	tierFree = "free"
	tierGo   = "go"
)

// publicCredential is the literal bearer the anonymous free tier expects.
const publicCredential = "public"

// modelLimits carries the context window and output cap for a model family.
// Values the upstream catalogue does not publish come from live measurement.
type modelLimit struct {
	context int64
	output  int64
}

var modelLimits = map[string]modelLimit{
	"kimi-k3":                     {context: 1048576, output: 131072},
	"mimo-v2.6-flash-free":        {context: 1048576, output: 32768},
	"deepseek-v4.1-flash":         {context: 200000, output: 65536},
	"deepseek-flash":              {context: 200000, output: 65536},
	"deepseek-v4-pro":             {context: 200000, output: 65536},
	"deepseek-v4-flash":           {context: 200000, output: 65536},
	"space-bunny-free":            {context: 200000, output: 32768},
	"nemotron-3.5-lightning-free": {context: 200000, output: 32768},
}

// defaultLimit applies to a model with no measured entry.
var defaultLimit = modelLimit{context: 200000, output: 32768}

// limitFor resolves the declared limits for one upstream model id.
func limitFor(id string) modelLimit {
	if hit, ok := modelLimits[id]; ok {
		return hit
	}
	return defaultLimit
}

// tierOf reads the tier marker out of an auth record. An unknown or absent
// value is the free tier, which is the only one that needs no credential.
func tierOf(attributes map[string]string, storage []byte) string {
	if attributes != nil {
		if tier := strings.TrimSpace(attributes["tier"]); tier != "" {
			return strings.ToLower(tier)
		}
	}
	var document map[string]any
	if len(storage) > 0 && json.Unmarshal(storage, &document) == nil {
		if tier, ok := document["tier"].(string); ok && strings.TrimSpace(tier) != "" {
			return strings.ToLower(tier)
		}
	}
	return tierFree
}

// keyOf reads the Go-plan API key out of an auth record.
func keyOf(attributes map[string]string, storage []byte) string {
	if attributes != nil {
		if key := strings.TrimSpace(attributes["api_key"]); key != "" {
			return key
		}
	}
	var document map[string]any
	if len(storage) > 0 && json.Unmarshal(storage, &document) == nil {
		if key, ok := document["api_key"].(string); ok {
			return strings.TrimSpace(key)
		}
	}
	return ""
}

// pluginState is the runtime state shared by every capability.
type pluginState struct {
	mu       sync.Mutex
	version  string
	sessions *sessionStore

	// cache holds the last catalogue fetch, keyed by tier.
	cacheMu sync.Mutex
	cache   map[string]cachedCatalog

	// config values, refreshed on every plugin.reconfigure.
	apiKeys     []string
	freeEnabled bool

	// materialized guards the one-shot auth-record creation.
	materialized bool

	// baseURL overrides the free-tier upstream root, for testing against a
	// local capture endpoint.
	baseURL string
	// goURL overrides the paid-tier upstream root.
	goURL string
}

type cachedCatalog struct {
	fetchedAt time.Time
	models    []pluginkit.ModelInfo
}

func newPluginState() *pluginState {
	return &pluginState{
		version:     versionFallback,
		sessions:    newSessionStore(),
		cache:       map[string]cachedCatalog{},
		freeEnabled: true,
	}
}

// gateway bundles the capabilities this plugin implements.
type gateway struct {
	*pluginkit.Base
	impl *pluginState
}

func newGateway() *gateway {
	impl := newPluginState()
	gw := &gateway{impl: impl}
	gw.Base = &pluginkit.Base{
		Identity: pluginkit.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "cpa-plugins",
			GitHubRepository: "https://github.com/Sivan757/cpa-plugins",
			ConfigFields: []pluginkit.ConfigField{
				{
					Name:        "api_key",
					Type:        pluginkit.FieldString,
					Description: "OpenCode Go API key (oc_sk_...). Several keys may be given, separated by commas.",
				},
				{
					Name:        "free_enabled",
					Type:        pluginkit.FieldBoolean,
					Description: "Advertise the anonymous free-tier models (default true).",
				},
				{
					Name:        "user_agent_version",
					Type:        pluginkit.FieldString,
					Description: "OpenCode CLI version used for the upstream user agent. Discovered from npm when unset.",
				},
				{
					Name:        "base_url",
					Type:        pluginkit.FieldString,
					Description: "Free-tier upstream root (default https://opencode.ai/zen/v1).",
				},
				{
					Name:        "go_base_url",
					Type:        pluginkit.FieldString,
					Description: "Paid-tier upstream root (default https://opencode.ai/zen/go/v1).",
				},
			},
		},
		ProviderKey:   providerKey,
		InputFormats:  []string{"chat-completions"},
		OutputFormats: []string{"chat-completions"},
		Auth:          gw,
		Models:        gw,
		Exec:          gw,
		Mgmt:          gw,
		OnConfigure:   gw.configure,
	}
	return gw
}

func (g *gateway) configure(cfg *pluginkit.Config) error {
	if cfg == nil {
		return nil
	}
	if raw := cfg.String("api_key", ""); raw != "" {
		g.impl.apiKeys = splitKeys(raw)
	}
	if raw := cfg.String("free_enabled", ""); raw != "" {
		g.impl.freeEnabled = !strings.EqualFold(raw, "false")
	}
	if version := strings.TrimSpace(cfg.String("user_agent_version", "")); version != "" {
		g.impl.version = version
	}
	if base := strings.TrimSpace(cfg.String("base_url", "")); base != "" {
		g.impl.baseURL = strings.TrimSuffix(base, "/")
	}
	if base := strings.TrimSpace(cfg.String("go_base_url", "")); base != "" {
		g.impl.goURL = strings.TrimSuffix(base, "/")
	}
	g.impl.materializeTiers()
	return nil
}

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
	entries, errList := pluginkit.AuthList(ctx)
	if errList != nil {
		pluginkit.HostLog(ctx, "warn", "opencode: could not list auth records: "+errList.Error())
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
			if errSave := pluginkit.AuthSave(ctx, pluginkit.HostAuthSaveRequest{
				Name: freeAuthFileName,
				JSON: record,
			}); errSave != nil {
				pluginkit.HostLog(ctx, "warn", "opencode: could not save the free-tier auth record: "+errSave.Error())
			} else {
				pluginkit.HostLog(ctx, "info", "opencode: created the free-tier auth record")
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
		if errSave := pluginkit.AuthSave(ctx, pluginkit.HostAuthSaveRequest{Name: name, JSON: record}); errSave != nil {
			pluginkit.HostLog(ctx, "warn", "opencode: could not save a Go auth record: "+errSave.Error())
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

// ---- AuthProvider ----

// ParseAuth implements pluginkit.AuthProvider. The record shape is
// {"type":"opencode","tier":"go","api_key":"oc_sk_..."} for the paid plan and
// {"type":"opencode","tier":"free"} for the anonymous tier.
func (g *gateway) ParseAuth(_ context.Context, req pluginkit.AuthParseRequest) (pluginkit.AuthParseResponse, error) {
	var document map[string]any
	if len(req.RawJSON) > 0 {
		if errUnmarshal := json.Unmarshal(req.RawJSON, &document); errUnmarshal != nil {
			return pluginkit.AuthParseResponse{}, pluginkit.BadRequestError("decode opencode credential: " + errUnmarshal.Error())
		}
	}
	if req.Provider != "" && req.Provider != providerKey {
		return pluginkit.AuthParseResponse{Handled: false}, nil
	}
	typeName, _ := document["type"].(string)
	providerName, _ := document["provider"].(string)
	if typeName != providerKey && providerName != providerKey {
		return pluginkit.AuthParseResponse{Handled: false}, nil
	}
	tier := tierFree
	if rawTier, ok := document["tier"].(string); ok && strings.TrimSpace(rawTier) != "" {
		tier = strings.ToLower(strings.TrimSpace(rawTier))
	}
	key, _ := document["api_key"].(string)
	if tier == tierGo && strings.TrimSpace(key) == "" {
		return pluginkit.AuthParseResponse{}, pluginkit.BadRequestError("opencode Go auth record has no api_key")
	}
	attributes := map[string]string{"tier": tier}
	if strings.TrimSpace(key) != "" {
		attributes["api_key"] = strings.TrimSpace(key)
	}
	id, _ := document["id"].(string)
	if id == "" {
		id = req.FileName
	}
	label, _ := document["label"].(string)
	return pluginkit.AuthParseResponse{Handled: true, Auth: pluginkit.AuthData{
		Provider:    providerKey,
		ID:          id,
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: req.RawJSON,
		Attributes:  attributes,
	}}, nil
}

// StartLogin implements pluginkit.AuthProvider. Both tiers authenticate with a
// static credential, so there is no interactive flow.
func (g *gateway) StartLogin(context.Context, pluginkit.AuthLoginStartRequest) (pluginkit.AuthLoginStartResponse, error) {
	return pluginkit.AuthLoginStartResponse{}, pluginkit.NewError("unsupported", "opencode logins are not interactive; add an auth file instead", http.StatusBadRequest)
}

// PollLogin implements pluginkit.AuthProvider.
func (g *gateway) PollLogin(context.Context, pluginkit.AuthLoginPollRequest) (pluginkit.AuthLoginPollResponse, error) {
	return pluginkit.AuthLoginPollResponse{}, pluginkit.NewError("unsupported", "opencode logins are not interactive", http.StatusBadRequest)
}

// RefreshAuth implements pluginkit.AuthProvider. Both credentials are static.
func (g *gateway) RefreshAuth(_ context.Context, req pluginkit.AuthRefreshRequest) (pluginkit.AuthRefreshResponse, error) {
	return pluginkit.AuthRefreshResponse{Auth: pluginkit.AuthData{
		Provider:    req.AuthProvider,
		ID:          req.AuthID,
		StorageJSON: req.StorageJSON,
		Attributes:  req.Attributes,
	}}, nil
}

// ---- ModelProvider ----

// StaticModels implements pluginkit.ModelProvider. The free tier needs no auth
// record at all, so its catalogue is published statically.
func (g *gateway) StaticModels(ctx context.Context, req pluginkit.StaticModelRequest) (pluginkit.ModelResponse, error) {
	models := g.impl.freeModels(ctx)
	excluded := pluginkit.ExcludedModelsFor(req.Host, providerKey)
	return pluginkit.ModelResponse{Provider: providerKey, Models: pluginkit.FilterModelsByExclusion(models, excluded)}, nil
}

// ModelsForAuth implements pluginkit.ModelProvider. A Go auth record publishes
// the paid catalogue; a free record publishes the free one.
func (g *gateway) ModelsForAuth(ctx context.Context, req pluginkit.AuthModelRequest) (pluginkit.ModelResponse, error) {
	tier := tierOf(req.Attributes, req.StorageJSON)
	models := g.impl.modelsForTier(ctx, tier)
	excluded := pluginkit.ExcludedModelsFor(req.Host, providerKey)
	return pluginkit.ModelResponse{Provider: providerKey, Models: pluginkit.FilterModelsByExclusion(models, excluded)}, nil
}

// RegisterModels implements pluginkit.ModelProvider.
func (g *gateway) RegisterModels(ctx context.Context, _ pluginkit.ModelRegistrationRequest) (pluginkit.ModelRegistrationResponse, error) {
	return pluginkit.ModelRegistrationResponse{Provider: providerKey, Models: g.impl.freeModels(ctx)}, nil
}

// modelsForTier returns the catalogue a tier may serve.
func (p *pluginState) modelsForTier(ctx context.Context, tier string) []pluginkit.ModelInfo {
	if tier == tierGo {
		return p.goModels(ctx)
	}
	return p.freeModels(ctx)
}

// freeModels returns the anonymous catalogue: every id ending in -free plus the
// anonymously served all-round model. Models that answer only on the Responses
// endpoint are dropped, because this executor speaks chat-completions.
func (p *pluginState) freeModels(ctx context.Context) []pluginkit.ModelInfo {
	if !p.freeEnabled {
		return nil
	}
	return p.catalog(ctx, tierFree)
}

// goModels returns the paid catalogue.
func (p *pluginState) goModels(ctx context.Context) []pluginkit.ModelInfo {
	return p.catalog(ctx, tierGo)
}

// catalog serves the cached model list for a tier, refetching when the entry
// has aged out. A failed refresh keeps the previous snapshot.
func (p *pluginState) catalog(ctx context.Context, tier string) []pluginkit.ModelInfo {
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
	return fetched
}

// fetchCatalog reads one tier's model list from its gateway.
func (p *pluginState) fetchCatalog(ctx context.Context, tier, version string) []pluginkit.ModelInfo {
	base := p.freeRoot()
	key := ""
	if tier == tierGo {
		base = p.goRoot()
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
	result, errCall := pluginkit.HTTPJSON(ctx, http.MethodGet, base+"/models", headers, nil, &payload)
	if errCall != nil || result == nil || result.StatusCode < 200 || result.StatusCode >= 300 {
		pluginkit.HostLog(ctx, "warn", fmt.Sprintf("opencode: %s catalogue fetch failed", tier))
		return nil
	}
	return buildModelList(payload.Data, tier)
}

// buildModelList filters and annotates the raw catalogue entries.
func buildModelList(entries []map[string]any, tier string) []pluginkit.ModelInfo {
	out := make([]pluginkit.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		id, _ := entry["id"].(string)
		if id == "" {
			continue
		}
		if tier == tierFree && !isFreeModel(id) {
			continue
		}
		if strings.HasPrefix(id, "muse-spark") {
			continue
		}
		limit := limitFor(id)
		out = append(out, pluginkit.ModelInfo{
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

// ---- Executor ----

// ExecutorIdentifier implements pluginkit.Executor.
func (g *gateway) ExecutorIdentifier() string { return providerKey }

// ---- ManagementAPI ----

// RegisterManagement implements pluginkit.ManagementAPI.
func (g *gateway) RegisterManagement(context.Context, pluginkit.ManagementRegistrationRequest) (pluginkit.ManagementRegistrationResponse, error) {
	return pluginkit.ManagementRegistrationResponse{
		Routes: []pluginkit.ManagementRoute{{
			Method:      "GET",
			Path:        "plugins/cpa-provider-opencode/status",
			Description: "Catalogue and tier status for the OpenCode provider",
		}},
	}, nil
}

// HandleManagement implements pluginkit.ManagementAPI.
func (g *gateway) HandleManagement(ctx context.Context, req pluginkit.ManagementRequest) (pluginkit.ManagementResponse, error) {
	if !strings.HasSuffix(strings.TrimSuffix(req.Path, "/"), "/status") {
		return pluginkit.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte("{\"error\":\"not found\"}")}, nil
	}
	free := g.impl.freeModels(ctx)
	paid := g.impl.goModels(ctx)
	report := map[string]any{
		"providerKey":     providerKey,
		"pluginID":        "cpa-provider-opencode",
		"clientVersion":   g.impl.version,
		"freeEnabled":     g.impl.freeEnabled,
		"goKeyConfigured": len(g.impl.apiKeys) > 0,
		"freeModelCount":  len(free),
		"goModelCount":    len(paid),
	}
	body, _ := json.MarshalIndent(report, "", "  ")
	return pluginkit.JSONManagementResponse(http.StatusOK, body)
}
