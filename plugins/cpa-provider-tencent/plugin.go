package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// pluginVersion is overridden at build time with -X main.pluginVersion=...
var pluginVersion = "0.4.0"

// providerKey is the single provider key this plugin serves. CPA binds one
// plugin executor to one provider, and all three Tencent credential families
// sit behind it, so overlapping model ids stop competing across plugins.
const providerKey = "tencent"

// pluginName is the management-visible plugin name.
const pluginName = "CLIProxyAPI Tencent Provider"

// gateway implements every capability over the shared credential pool.
type gateway struct {
	impl *pluginState
}

// pluginState holds the runtime state shared by all credentials.
type pluginState struct {
	keysByKind map[string]*keyProvider
	cache      *catalogCache
	apiKey     string
	regionHint string

	// rotator carries the CodeBuddy key rotation state: fresh keys are served
	// round-robin and a rejected key is cooled down.
	rotator *keyRotator

	// expose narrows the advertised roster to matching ids. Empty means no
	// restriction.
	expose []string

	// credentialDirPath overrides where this plugin keeps its own credential
	// documents. Setting it matters in a container: only mounted paths survive a
	// recreation.
	credentialDirPath string
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
	return &gateway{impl: newPluginState()}
}

// ---- Plugin contract ----

// Metadata implements cpakit.Plugin.
func (g *gateway) Metadata() cpakit.Metadata {
	return cpakit.Metadata{
		Name:             pluginName,
		Version:          pluginVersion,
		Author:           "cpa-plugins",
		GitHubRepository: "https://github.com/Sivan757/cpa-plugins",
		ConfigFields: []cpakit.ConfigField{
			{
				Name:        "api_key",
				Type:        cpakit.FieldString,
				Description: "CodeBuddy API key (ck_...). Optional when the OAuth login is used.",
			},
			{
				Name:        "cache_ttl_seconds",
				Type:        cpakit.FieldInteger,
				Description: "How long a fetched model catalog is reused before it is read again.",
			},
			{
				Name:        "credential_dir",
				Type:        cpakit.FieldString,
				Description: "Directory holding this plugin's credential documents. Set it to a mounted path in a container.",
			},
			{
				Name:        "expose",
				Type:        cpakit.FieldArray,
				Description: "Only advertise models matching these patterns (e.g. 'deepseek-v4.1-flash'). Empty advertises everything.",
			},
			{
				Name:        "key_cooldown_seconds",
				Type:        cpakit.FieldInteger,
				Description: "How long a rejected CodeBuddy key is skipped before it is tried again.",
			},
		},
	}
}

// ProviderKey implements cpakit.Plugin.
func (g *gateway) ProviderKey() string { return providerKey }

// Capabilities implements cpakit.Plugin.
func (g *gateway) Capabilities() cpakit.Capabilities {
	return cpakit.Capabilities{
		QuotaProvider:         true,
		AuthProvider:          true,
		ModelProvider:         true,
		Executor:              true,
		ExecutorModelScope:    cpakit.ScopeBoth,
		ExecutorInputFormats:  []string{"chat-completions"},
		ExecutorOutputFormats: []string{"chat-completions"},
		ManagementAPI:         true,
	}
}

// Configure implements cpakit.Plugin.
func (g *gateway) Configure(configYAML []byte) error {
	cfg, errParse := cpakit.ParseConfig(configYAML)
	if errParse != nil {
		return errParse
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
	if patterns := cfg.Strings("expose"); len(patterns) > 0 {
		g.impl.expose = patterns
	}
	if dir := cfg.String("credential_dir", ""); strings.TrimSpace(dir) != "" {
		g.impl.credentialDirPath = strings.TrimSpace(dir)
	}
	cpakit.HostLog(context.Background(), "info", fmt.Sprintf(
		"tencent: configured expose=%v credentialDir=%q", g.impl.expose, g.impl.credentialDir()))
	return nil
}

// Dispatch implements cpakit.Plugin.
func (g *gateway) Dispatch(ctx context.Context, method string, request json.RawMessage) (any, error) {
	switch method {
	// ---- auth ----
	case cpakit.MethodAuthParse:
		req, errDecode := cpakit.Decode[cpakit.AuthParseRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.ParseAuth(ctx, req)
	case cpakit.MethodAuthLoginStart:
		req, errDecode := cpakit.Decode[cpakit.AuthLoginStartRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.StartLogin(ctx, req)
	case cpakit.MethodAuthLoginPoll:
		req, errDecode := cpakit.Decode[cpakit.AuthLoginPollRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.PollLogin(ctx, req)
	case cpakit.MethodAuthRefresh:
		req, errDecode := cpakit.Decode[cpakit.AuthRefreshRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.RefreshAuth(ctx, req)

	// ---- models ----
	case cpakit.MethodModelStatic:
		req, errDecode := cpakit.Decode[cpakit.StaticModelRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.StaticModels(ctx, req)
	case cpakit.MethodModelForAuth:
		req, errDecode := cpakit.Decode[cpakit.AuthModelRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.ModelsForAuth(ctx, req)
	case cpakit.MethodModelRegister:
		req, errDecode := cpakit.Decode[cpakit.ModelRegistrationRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.RegisterModels(ctx, req)

	// ---- quota ----
	case cpakit.MethodQuotaDescribe:
		return g.DescribeQuota(), nil
	case cpakit.MethodQuotaFetch:
		req, errDecode := cpakit.Decode[cpakit.QuotaFetchRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.FetchQuota(ctx, req)
	case cpakit.MethodQuotaReset:
		req, errDecode := cpakit.Decode[cpakit.QuotaResetRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.ResetQuota(ctx, req)

	// ---- execution ----
	case cpakit.MethodExecutorExecute:
		req, streamID, callbackID, errDecode := cpakit.DecodeExecutorRequest(request)
		if errDecode != nil {
			return nil, errDecode
		}
		if req.Stream {
			return g.ExecuteStream(ctx, req, streamID, callbackID)
		}
		return g.Execute(ctx, req)
	case cpakit.MethodExecutorExecuteStream:
		req, streamID, callbackID, errDecode := cpakit.DecodeExecutorRequest(request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.ExecuteStream(ctx, req, streamID, callbackID)
	case cpakit.MethodExecutorCountTokens:
		return cpakit.ExecutorResponse{}, cpakit.ErrUnhandled
	case cpakit.MethodExecutorHTTPRequest:
		req, errDecode := cpakit.Decode[cpakit.ExecutorHTTPRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.HttpRequest(ctx, req)

	// ---- management ----
	case cpakit.MethodManagementRegister:
		return g.RegisterManagement(ctx)
	case cpakit.MethodManagementHandle:
		req, errDecode := cpakit.Decode[cpakit.ManagementRequest](request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.HandleManagement(ctx, req)
	}
	return nil, cpakit.ErrUnhandled
}

// ---- capabilities ----

// DescribeQuota declares the credential family this plugin owns.
func (g *gateway) DescribeQuota() cpakit.QuotaDescribeResponse {
	return cpakit.QuotaDescribeResponse{
		SupportedProviders: []string{providerKey},
		DisplayName:        "Tencent 订阅额度",
	}
}

// FetchQuota reports quota for one credential. Each auth record carries its own
// kind, so the report is per credential rather than per plugin.
func (g *gateway) FetchQuota(ctx context.Context, req cpakit.QuotaFetchRequest) (cpakit.QuotaFetchResponse, error) {
	kind := kindForRecord(req.StorageJSON, req.Attributes)
	creds, errCreds := g.impl.resolve(ctx, kind)
	if errCreds != nil {
		return cpakit.QuotaFetchResponse{}, errCreds
	}
	return fetchQuota(ctx, kind, creds)
}

// ResetQuota is not offered by the upstream.
func (g *gateway) ResetQuota(context.Context, cpakit.QuotaResetRequest) (cpakit.QuotaResetResponse, error) {
	return cpakit.QuotaResetResponse{}, cpakit.ErrUnhandled
}

// StaticModels advertises every kind's live roster minus the models that kind
// excludes: a model stays listed while any credential can still serve it.
func (g *gateway) StaticModels(ctx context.Context, req cpakit.StaticModelRequest) (cpakit.ModelResponse, error) {
	models := visibleRoster(authFileExclusions(req.Host.AuthDir), func(kind *credentialKind) []cpakit.ModelInfo {
		return g.impl.rosterForKind(ctx, kind)
	})
	excluded := cpakit.ExcludedModelsFor(req.Host, providerKey)
	models = cpakit.FilterModelsByExclusion(models, excluded)
	return cpakit.ModelResponse{Provider: providerKey, Models: cpakit.ExposeOnly(models, g.impl.expose)}, nil
}

// ModelsForAuth reports the roster one credential can actually drive.
//
// Per-credential exclusions are deliberately not applied here: the gateway
// already drops them from this credential's routing candidates, and re-applying
// them locally could only make the two lists disagree.
func (g *gateway) ModelsForAuth(ctx context.Context, req cpakit.AuthModelRequest) (cpakit.ModelResponse, error) {
	kind := kindForRecord(req.StorageJSON, req.Attributes)
	creds, errCreds := g.impl.resolve(ctx, kind)
	if errCreds != nil {
		return cpakit.ModelResponse{Provider: providerKey, Models: staticFallback(kind)}, nil
	}
	models, errModels := g.impl.modelsForKind(ctx, kind, creds)
	if errModels != nil {
		filtered := cpakit.FilterModelsByExclusion(staticFallback(kind), req.Host.ExcludedModels[providerKey])
		return cpakit.ModelResponse{Provider: providerKey, Models: cpakit.ExposeOnly(filtered, g.impl.expose)}, nil
	}
	filtered := cpakit.FilterModelsByExclusion(models, cpakit.ExcludedModelsFor(req.Host, providerKey))
	return cpakit.ModelResponse{Provider: providerKey, Models: cpakit.ExposeOnly(filtered, g.impl.expose)}, nil
}

// RegisterModels reports the live rosters. The host only calls this when a
// plugin exposes no ModelProvider, so there is no host summary here and
// per-credential exclusions cannot be read.
func (g *gateway) RegisterModels(ctx context.Context, _ cpakit.ModelRegistrationRequest) (cpakit.ModelRegistrationResponse, error) {
	models := visibleRoster(nil, func(kind *credentialKind) []cpakit.ModelInfo {
		return g.impl.rosterForKind(ctx, kind)
	})
	return cpakit.ModelRegistrationResponse{Provider: providerKey, Models: cpakit.ExposeOnly(models, g.impl.expose)}, nil
}

// RegisterManagement declares the plugin's diagnostic route.
func (g *gateway) RegisterManagement(context.Context) (cpakit.ManagementRegistrationResponse, error) {
	return cpakit.ManagementRegistrationResponse{
		Routes: []cpakit.ManagementRoute{{
			Method:      "GET",
			Path:        "plugins/cpa-provider-tencent/status",
			Description: "Per-credential status for the merged Tencent provider",
		}},
	}, nil
}

// HandleManagement serves the diagnostic route.
func (g *gateway) HandleManagement(ctx context.Context, req cpakit.ManagementRequest) (cpakit.ManagementResponse, error) {
	if !strings.HasSuffix(strings.TrimSuffix(req.Path, "/"), "/status") {
		return cpakit.ManagementResponse{StatusCode: 404, Body: []byte("{\"error\":\"not found\"}")}, nil
	}
	body, _ := json.MarshalIndent(g.statusReport(ctx), "", "  ")
	return cpakit.JSONManagementResponse(200, body)
}

// statusReport lists every credential kind with its resolution state. It never
// echoes a credential: only the identity fields the management UI renders.
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
			entry["error"] = cpakit.Redact(errCreds.Error())
		} else {
			entry["credential"] = "ok"
			entry["mode"] = creds.Mode
			entry["uid"] = creds.UID
			entry["enterpriseId"] = creds.EnterpriseID
			entry["expiresAt"] = creds.ExpiresAt.Format(time.RFC3339)
		}
		entry["cachedModels"] = g.impl.cache.countFor(kind.id)
		entries = append(entries, entry)
	}
	report["credentials"] = entries
	return report
}
