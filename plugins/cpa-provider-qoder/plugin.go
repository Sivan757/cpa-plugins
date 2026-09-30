package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// pluginVersion is overridden at build time with -X main.pluginVersion=...
var pluginVersion = "0.1.0"

// defaultCatalogTTL keeps a catalog snapshot briefly so the model-list request
// the host makes immediately before a chat does not hit the upstream twice.
const defaultCatalogTTL = 5 * time.Minute

// configState is the plugin's own configuration block.
type configState struct {
	pat    string
	region string
}

// pluginState holds the runtime state shared by every capability.
type pluginState struct {
	cfg    *configState
	tokens *jobTokenCache
	cache  *catalogCache

	mu        sync.Mutex
	lastError string

	// transportOverride redirects the transport at a local upstream. Tests set
	// it; nothing in production does.
	transportOverride *region
}

func newPluginState() *pluginState {
	return &pluginState{
		cfg:    &configState{},
		tokens: newJobTokenCache(),
		cache:  newCatalogCache(defaultCatalogTTL),
	}
}

// activeRegion maps a resolved region onto the transport's target.
func (p *pluginState) activeRegion(resolved *region) *region {
	if p.transportOverride != nil {
		return p.transportOverride
	}
	if resolved == nil {
		return regionFor(defaultRegionID)
	}
	return resolved
}

func (p *pluginState) recordError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.lastError = ""
		return
	}
	p.lastError = err.Error()
}

func (p *pluginState) recordedError() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastError
}

// gateway bundles the capabilities this plugin implements.
type gateway struct {
	*pluginkit.Base
	impl *pluginState
}

// newGateway assembles the plugin's capability set.
func newGateway() *gateway {
	impl := newPluginState()
	gw := &gateway{impl: impl}
	gw.Base = &pluginkit.Base{
		Identity: pluginkit.Metadata{
			Name:             "CLIProxyAPI Qoder Provider",
			Version:          pluginVersion,
			Author:           "cpa-plugins",
			GitHubRepository: "https://github.com/mo-n/dsh-provider-qoder",
			ConfigFields: []pluginkit.ConfigField{
				{
					Name:        "pat",
					Type:        pluginkit.FieldString,
					Description: `Qoder personal access token. Create one in the Qoder web console under account integrations.`,
				},
				{
					Name:        "region",
					Type:        pluginkit.FieldEnum,
					EnumValues:  []string{"global", "china"},
					Description: `Which Qoder deployment this credential belongs to: global (qoder.sh) or china (qoder.com.cn).`,
				},
				{
					Name:        "cache_ttl_seconds",
					Type:        pluginkit.FieldInteger,
					Description: `How long a fetched model catalog is reused before it is read again.`,
				},
			},
		},
		ProviderKey:   providerKey,
		InputFormats:  []string{executorInputFormat},
		OutputFormats: []string{executorOutputFormat},
		Quota:         gw,
		Auth:          gw,
		Models:        gw,
		Exec:          gw,
		Mgmt:          gw,
		OnConfigure:   gw.configure,
	}
	return gw
}

func (g *gateway) configure(cfg *pluginkit.Config) error {
	p := g.impl
	p.cfg.pat = strings.TrimSpace(cfg.String("pat", ""))
	p.cfg.region = strings.TrimSpace(cfg.String("region", ""))
	if ttl := cfg.Int("cache_ttl_seconds", 0); ttl > 0 {
		p.cache.setTTL(time.Duration(ttl) * time.Second)
	}
	return nil
}

// ownsProvider reports whether a provider key names this provider.
//
// An empty key means the host did not say which credential it is asking about,
// and this plugin is the only one that can answer, so it counts as ours.
func (g *gateway) ownsProvider(provider string) bool {
	trimmed := strings.TrimSpace(provider)
	return trimmed == "" || strings.EqualFold(trimmed, providerKey)
}

// ---- QuotaProvider ----

// QuotaIdentifier implements pluginkit.QuotaProvider.
func (g *gateway) QuotaIdentifier() string { return providerKey }

// DescribeQuota implements pluginkit.QuotaProvider.
func (g *gateway) DescribeQuota() pluginkit.QuotaDescribeResponse {
	return pluginkit.QuotaDescribeResponse{
		SupportedProviders: []string{providerKey},
		DisplayName:        "Qoder 额度",
	}
}

// FetchQuota implements pluginkit.QuotaProvider.
func (g *gateway) FetchQuota(ctx context.Context, req pluginkit.QuotaFetchRequest) (pluginkit.QuotaFetchResponse, error) {
	if !g.ownsProvider(req.Provider) {
		return pluginkit.QuotaFetchResponse{}, pluginkit.ErrUnhandled
	}
	response, errQuota := g.impl.fetchQuota(ctx, req.StorageJSON)
	g.impl.recordError(errQuota)
	if errQuota != nil {
		return pluginkit.QuotaFetchResponse{}, errQuota
	}
	return response, nil
}

// ResetQuota implements pluginkit.QuotaProvider. Qoder exposes no reset call.
func (g *gateway) ResetQuota(context.Context, pluginkit.QuotaResetRequest) (pluginkit.QuotaResetResponse, error) {
	return pluginkit.QuotaResetResponse{}, pluginkit.ErrUnhandled
}

// ---- ModelProvider ----

// StaticModels implements pluginkit.ModelProvider.
func (g *gateway) StaticModels(context.Context, pluginkit.StaticModelRequest) (pluginkit.ModelResponse, error) {
	return pluginkit.ModelResponse{
		Provider: providerKey,
		Models:   mustStaticRoutes(regionFor(g.impl.cfg.region)),
	}, nil
}

// ModelsForAuth implements pluginkit.ModelProvider.
//
// When the catalog is unreachable this returns the static roster: an empty model
// list would remove the provider from the picker entirely, which is worse than a
// conservative list.
func (g *gateway) ModelsForAuth(ctx context.Context, req pluginkit.AuthModelRequest) (pluginkit.ModelResponse, error) {
	models, errModels := g.impl.modelsForAuth(ctx, req.StorageJSON)
	if errModels != nil {
		g.impl.recordError(errModels)
		return pluginkit.ModelResponse{
			Provider: providerKey,
			Models:   mustStaticRoutes(regionFor(firstNonEmpty(req.Attributes["region"], g.impl.cfg.region))),
		}, nil
	}
	return pluginkit.ModelResponse{Provider: providerKey, Models: models}, nil
}

// RegisterModels implements pluginkit.ModelProvider.
func (g *gateway) RegisterModels(context.Context, pluginkit.ModelRegistrationRequest) (pluginkit.ModelRegistrationResponse, error) {
	return pluginkit.ModelRegistrationResponse{
		Provider: providerKey,
		Models:   mustStaticRoutes(regionFor(g.impl.cfg.region)),
	}, nil
}

// ---- Executor ----

// ExecutorIdentifier implements pluginkit.Executor.
func (g *gateway) ExecutorIdentifier() string { return providerKey }

// ---- ManagementAPI ----

// RegisterManagement implements pluginkit.ManagementAPI.
func (g *gateway) RegisterManagement(context.Context, pluginkit.ManagementRegistrationRequest) (pluginkit.ManagementRegistrationResponse, error) {
	return pluginkit.ManagementRegistrationResponse{
		// The host prefixes a relative path with the management base, so this
		// lands on /v0/management/plugins/cpa-provider-qoder/status. A bare
		// "status" would occupy a global path shared by every plugin.
		Routes: []pluginkit.ManagementRoute{{
			Method:      "GET",
			Path:        "plugins/cpa-provider-qoder/status",
			Description: "Credential, endpoint, codec, and catalog diagnostics for this provider",
		}},
	}, nil
}

// HandleManagement implements pluginkit.ManagementAPI.
func (g *gateway) HandleManagement(ctx context.Context, req pluginkit.ManagementRequest) (pluginkit.ManagementResponse, error) {
	if !strings.HasSuffix(strings.TrimSuffix(req.Path, "/"), "/status") {
		return pluginkit.ManagementResponse{StatusCode: 404, Body: []byte(`{"error":"not found"}`)}, nil
	}
	body, errMarshal := json.MarshalIndent(g.statusReport(ctx), "", "  ")
	if errMarshal != nil {
		return pluginkit.ManagementResponse{StatusCode: 500, Body: []byte(`{"error":"encode diagnostics"}`)}, nil
	}
	return pluginkit.JSONManagementResponse(200, body)
}

// statusReport is the diagnostic payload served on the plugin status route.
//
// It never echoes the personal access token: only where it came from and whether
// the exchange succeeded.
func (g *gateway) statusReport(ctx context.Context) map[string]any {
	p := g.impl
	active := regionFor(p.cfg.region)
	report := map[string]any{
		"providerKey":      providerKey,
		"configuredRegion": active.id,
		"displayName":      active.displayName,
		"cacheTTLSeconds":  int(p.cache.ttlSeconds()),
		"cachedModels":     p.cache.count(),
		"codec":            codecSelfCheck(),
		"endpoints": map[string]string{
			"apiBase": active.apiBase,
			"openAPI": active.openAPI,
			"catalog": active.apiBase + qoderCatalogRoute,
			"chat":    active.apiBase + qoderChatRoute + qoderChatQuery,
		},
	}
	resolved, errResolve := resolveCredential(nil, p.cfg)
	if errResolve != nil {
		report["credential"] = "unconfigured"
		report["credentialHint"] = errResolve.Error()
		report["lastError"] = p.recordedError()
		return report
	}
	report["credential"] = "configured"
	report["credentialSource"] = string(resolved.Source)
	bearer, errBearer := p.tokens.get(ctx, resolved.PAT, p.activeRegion(resolved.Region))
	if errBearer != nil {
		report["jobToken"] = "unavailable"
		report["jobTokenError"] = errBearer.Error()
	} else {
		report["jobToken"] = "ok"
		report["jobTokenExpiresAt"] = bearer.ExpiresAt.UTC().Format(time.RFC3339)
		report["authenticated"] = bearer.UserID != ""
	}
	report["lastError"] = p.recordedError()
	return report
}

// codecSelfCheck proves the body codec round-trips, which is the one internal
// transform whose failure mode is an opaque upstream rejection.
func codecSelfCheck() string {
	probe := []byte(`{"probe":true}`)
	decoded, errDecode := qoderDecodeBody(qoderEncodeBody(probe))
	if errDecode != nil || !bytes.Equal(decoded, probe) {
		return "broken"
	}
	return "ok"
}

// identityUID returns the subscriber id used to salt the session key. It reads
// the cached job token when one has been exchanged; the plugin-level machine id
// is the fallback, which keeps the session stable across a token refresh.
func (p *pluginState) identityUID(storageJSON []byte) string {
	// The PAT is the natural salt when one is configured: it is stable per
	// subscriber and does not change across token refreshes.
	if p.cfg != nil && p.cfg.pat != "" {
		return p.cfg.pat
	}
	var doc struct {
		PAT string `json:"pat"`
	}
	if errUnmarshal := json.Unmarshal(storageJSON, &doc); errUnmarshal == nil && doc.PAT != "" {
		return doc.PAT
	}
	p.tokens.mu.Lock()
	for _, bearer := range p.tokens.entries {
		if bearer.UserID != "" {
			p.tokens.mu.Unlock()
			return bearer.UserID
		}
	}
	p.tokens.mu.Unlock()
	return defaultMachineID()
}
