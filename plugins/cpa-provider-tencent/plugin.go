package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// pluginVersion is overridden at build time with -X main.pluginVersion=...
var pluginVersion = "0.2.0"

// providerKey is the single provider key this plugin serves. CPA binds one
// plugin executor to one provider, and all three Tencent credential families
// sit behind it, so overlapping model ids stop competing across plugins.
const providerKey = "tencent"

// pluginName is the management-visible plugin name.
const pluginName = "CLIProxyAPI Tencent Provider"

// gateway implements every capability over the shared credential pool.
type gateway struct {
	*pluginkit.Base
	impl *pluginState
}

// pluginState holds the runtime state shared by all credentials.
type pluginState struct {
	keysByKind map[string]*keyProvider
	cache      *catalogCache
	apiKey     string
	regionHint string

	// rotator carries the CodeBuddy key rotation state: fresh keys are served
	// round-robin and a rejected key is cooled down, exactly as in the
	// standalone CodeBuddy plugin.
	rotator *keyRotator
}

func newPluginState() *pluginState {
	return &pluginState{
		keysByKind: map[string]*keyProvider{},
		cache:      &catalogCache{ttl: defaultCacheTTL},
		rotator:    newKeyRotator(defaultKeyCooldown),
	}
}

// keysFor lazily builds one at-rest key provider per desktop kind, because each
// product seals its credential with its own key.
func (p *pluginState) keysFor(kind *credentialKind) *keyProvider {
	if p.keysByKind[kind.id] == nil {
		p.keysByKind[kind.id] = newKeyProvider(kind.electronBinary())
	}
	return p.keysByKind[kind.id]
}

// electronBinary resolves the app binary for a desktop kind.
func (k *credentialKind) electronBinary() string {
	if fromEnv := getEnv(k.envElectron); fromEnv != "" {
		return fromEnv
	}
	return k.electronPath
}

// newGateway assembles the capability set behind one provider key.
func newGateway() *gateway {
	impl := newPluginState()
	gw := &gateway{impl: impl}
	gw.Base = &pluginkit.Base{
		Identity: pluginkit.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "cpa-plugins",
			GitHubRepository: "https://github.com/mo-n/dsh-workbuddy-connect",
			ConfigFields: []pluginkit.ConfigField{
				{
					Name:        "api_key",
					Type:        pluginkit.FieldString,
					Description: "CodeBuddy API key (ck_...). Optional when the OAuth login is used.",
				},
				{
					Name:        "cache_ttl_seconds",
					Type:        pluginkit.FieldInteger,
					Description: "How long a fetched model catalog is reused before it is read again.",
				},
			},
		},
		ProviderKey:   providerKey,
		InputFormats:  []string{"chat-completions"},
		OutputFormats: []string{"chat-completions"},
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
	if cfg == nil {
		return nil
	}
	g.impl.apiKey = cfg.String("api_key", "")
	if ttl := cfg.Int("cache_ttl_seconds", 0); ttl > 0 {
		g.impl.cache.ttl = time.Duration(ttl) * time.Second
	}
	if cooldown := cfg.Int("key_cooldown_seconds", 0); cooldown > 0 {
		g.impl.rotator.setCooldown(time.Duration(cooldown) * time.Second)
	} else {
		g.impl.rotator.setCooldown(defaultKeyCooldown)
	}
	return nil
}

// ---- QuotaProvider ----

// QuotaIdentifier implements pluginkit.QuotaProvider.
func (g *gateway) QuotaIdentifier() string { return providerKey }

// DescribeQuota implements pluginkit.QuotaProvider.
func (g *gateway) DescribeQuota() pluginkit.QuotaDescribeResponse {
	return pluginkit.QuotaDescribeResponse{
		SupportedProviders: []string{providerKey},
		DisplayName:        "Tencent 订阅额度",
	}
}

// FetchQuota implements pluginkit.QuotaProvider. Each auth record carries its
// own kind, so the report is per credential rather than per plugin.
func (g *gateway) FetchQuota(ctx context.Context, req pluginkit.QuotaFetchRequest) (pluginkit.QuotaFetchResponse, error) {
	kind := kindForRecord(req.StorageJSON, req.Attributes)
	creds, errCreds := g.impl.resolve(ctx, kind)
	if errCreds != nil {
		return pluginkit.QuotaFetchResponse{}, errCreds
	}
	return fetchQuota(ctx, kind, creds)
}

// ResetQuota implements pluginkit.QuotaProvider.
func (g *gateway) ResetQuota(context.Context, pluginkit.QuotaResetRequest) (pluginkit.QuotaResetResponse, error) {
	return pluginkit.QuotaResetResponse{}, pluginkit.ErrUnhandled
}

// ---- ModelProvider ----

// StaticModels implements pluginkit.ModelProvider. Registration runs once for
// the provider, so it returns the union roster from the CN gateway's static
// fallback rather than a per-credential list.
func (g *gateway) StaticModels(ctx context.Context, req pluginkit.StaticModelRequest) (pluginkit.ModelResponse, error) {
	kind := kinds["workbuddy"]
	filtered := pluginkit.FilterModelsForRequest(staticFallback(kind), req.Host, providerKey, nil)
	return pluginkit.ModelResponse{Provider: providerKey, Models: filtered}, nil
}

// ModelsForAuth implements pluginkit.ModelProvider. The host calls this once per
// auth record, so each credential reports the roster it can actually drive.
func (g *gateway) ModelsForAuth(ctx context.Context, req pluginkit.AuthModelRequest) (pluginkit.ModelResponse, error) {
	kind := kindForRecord(req.StorageJSON, req.Attributes)
	creds, errCreds := g.impl.resolve(ctx, kind)
	if errCreds != nil {
		return pluginkit.ModelResponse{Provider: providerKey, Models: staticFallback(kind)}, nil
	}
	models, errModels := g.impl.modelsForKind(ctx, kind, creds)
	if errModels != nil {
		filtered := pluginkit.FilterModelsByExclusion(staticFallback(kind), req.Host.ExcludedModels[providerKey])
		return pluginkit.ModelResponse{Provider: providerKey, Models: filtered}, nil
	}
	filtered := pluginkit.FilterModelsForRequest(models, req.Host, providerKey, req.Attributes)
	return pluginkit.ModelResponse{Provider: providerKey, Models: filtered}, nil
}

// RegisterModels implements pluginkit.ModelProvider.
func (g *gateway) RegisterModels(context.Context, pluginkit.ModelRegistrationRequest) (pluginkit.ModelRegistrationResponse, error) {
	kind := kinds["workbuddy"]
	return pluginkit.ModelRegistrationResponse{Provider: providerKey, Models: pluginkit.FilterModelsByExclusion(staticFallback(kind), nil)}, nil
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
			Path:        "plugins/cpa-provider-tencent/status",
			Description: "Per-credential status for the merged Tencent provider",
		}},
	}, nil
}

// HandleManagement implements pluginkit.ManagementAPI.
func (g *gateway) HandleManagement(ctx context.Context, req pluginkit.ManagementRequest) (pluginkit.ManagementResponse, error) {
	if !strings.HasSuffix(strings.TrimSuffix(req.Path, "/"), "/status") {
		return pluginkit.ManagementResponse{StatusCode: 404, Body: []byte("{\"error\":\"not found\"}")}, nil
	}
	body, _ := json.MarshalIndent(g.statusReport(ctx), "", "  ")
	return pluginkit.JSONManagementResponse(200, body)
}

// statusReport lists every credential kind with its resolution state.
func (g *gateway) statusReport(ctx context.Context) map[string]any {
	report := map[string]any{"providerKey": providerKey, "pluginID": "cpa-provider-tencent"}
	entries := make([]map[string]any, 0, len(kindOrder))
	for _, id := range kindOrder {
		kind := kinds[id]
		entry := map[string]any{
			"kind":        kind.id,
			"displayName": kind.displayName,
			"region":      kind.region,
			"chatBase":    kind.chatBase,
			"desktop":     kind.desktop,
		}
		if creds, errCreds := g.impl.resolve(ctx, kind); errCreds != nil {
			entry["credential"] = "unavailable"
			entry["error"] = errCreds.Error()
		} else {
			entry["credential"] = "ok"
			entry["mode"] = creds.Mode
			entry["uid"] = creds.UID
			entry["enterpriseId"] = creds.EnterpriseID
			entry["expiresAt"] = creds.ExpiresAt.Format(time.RFC3339)
		}
		if models, okCache := g.impl.cache.get(); okCache {
			entry["cachedModels"] = len(models)
		}
		entries = append(entries, entry)
	}
	report["credentials"] = entries
	return report
}
