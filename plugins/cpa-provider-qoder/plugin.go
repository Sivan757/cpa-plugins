package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// pluginVersion is overridden at build time with -X main.pluginVersion=...
var pluginVersion = "0.2.0"

// providerKey is the single credential provider key this plugin serves.
//
// Both Qoder deployments share one key on purpose. The region is part of the
// credential (each auth record carries its own), while the host binds one
// executor per key, so a single library serves a qoder.sh account and a
// qoder.com.cn account at the same time. Splitting the key would force a user
// who holds both to add two providers that differ only by a field.
const providerKey = "qoder"

// providerDisplayName is the management-visible provider name.
const providerDisplayName = "CLIProxyAPI Qoder Provider"

// gateway is the plugin. It holds one pluginState and serves every method the
// provider declares.
type gateway struct {
	impl *pluginState
}

func newGateway() *gateway {
	return &gateway{impl: newPluginState()}
}

// ---- Plugin contract ----

// Metadata implements cpakit.Plugin.
func (g *gateway) Metadata() cpakit.Metadata {
	return cpakit.Metadata{
		Name:             providerDisplayName,
		Version:          pluginVersion,
		Author:           "cpa-plugins",
		GitHubRepository: "https://github.com/Sivan757/cpa-plugins",
		ConfigFields: []cpakit.ConfigField{
			{
				Name:        "pat",
				Type:        cpakit.FieldString,
				Description: "Qoder personal access token. Create one in the Qoder web console under account integrations.",
			},
			{
				Name:        "region",
				Type:        cpakit.FieldEnum,
				EnumValues:  []string{"global", "china"},
				Description: "Which Qoder deployment this credential belongs to: global (qoder.sh) or china (qoder.com.cn).",
			},
			{
				Name:        "expose",
				Type:        cpakit.FieldArray,
				Description: "Only advertise models matching these patterns (e.g. 'qwen3.8-flash'). Empty advertises everything.",
			},
			{
				Name:        "cache_ttl_seconds",
				Type:        cpakit.FieldInteger,
				Description: "How long a fetched model catalog is reused before it is read again.",
			},
		},
	}
}

// ProviderKey implements cpakit.Plugin.
func (g *gateway) ProviderKey() string { return providerKey }

// Capabilities implements cpakit.Plugin. The credential, model, execution,
// quota, and management surfaces are all served by this plugin.
func (g *gateway) Capabilities() cpakit.Capabilities {
	return cpakit.Capabilities{
		QuotaProvider:         true,
		AuthProvider:          true,
		ModelProvider:         true,
		Executor:              true,
		ExecutorModelScope:    cpakit.ScopeBoth,
		ExecutorInputFormats:  []string{executorInputFormat},
		ExecutorOutputFormats: []string{executorOutputFormat},
		ManagementAPI:         true,
	}
}

// Configure implements cpakit.Plugin.
func (g *gateway) Configure(configYAML []byte) error {
	cfg, errParse := cpakit.ParseConfig(configYAML)
	if errParse != nil {
		return errParse
	}
	p := g.impl
	p.cfg.pat = strings.TrimSpace(cfg.String("pat", ""))
	p.cfg.region = strings.TrimSpace(cfg.String("region", ""))
	if ttl := cfg.Int("cache_ttl_seconds", 0); ttl > 0 {
		p.cache.setTTL(time.Duration(ttl) * time.Second)
	}
	if patterns := cfg.Strings("expose"); len(patterns) > 0 {
		p.expose = patterns
	}
	return nil
}

// Dispatch implements cpakit.Plugin. Every method the host may send is routed
// here; an unimplemented method reports itself as unhandled rather than failing.
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
		req, streamID, _, errDecode := cpakit.DecodeExecutorRequest(request)
		if errDecode != nil {
			return nil, errDecode
		}
		// A stream-flagged request may arrive on either method; the stream path
		// owns it once the host has allocated a downstream stream.
		if req.Stream {
			return g.ExecuteStream(ctx, req, streamID)
		}
		return g.Execute(ctx, req)
	case cpakit.MethodExecutorExecuteStream:
		req, streamID, _, errDecode := cpakit.DecodeExecutorRequest(request)
		if errDecode != nil {
			return nil, errDecode
		}
		return g.ExecuteStream(ctx, req, streamID)
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
		DisplayName:        "Qoder 额度",
	}
}

// FetchQuota returns normalised quota for one credential.
func (g *gateway) FetchQuota(ctx context.Context, req cpakit.QuotaFetchRequest) (cpakit.QuotaFetchResponse, error) {
	if !g.ownsProvider(req.Provider) {
		return cpakit.QuotaFetchResponse{}, cpakit.ErrUnhandled
	}
	response, errQuota := g.impl.fetchQuota(ctx, req.StorageJSON)
	g.impl.recordError(errQuota)
	if errQuota != nil {
		return cpakit.QuotaFetchResponse{}, errQuota
	}
	return response, nil
}

// ResetQuota is not offered by Qoder.
func (g *gateway) ResetQuota(context.Context, cpakit.QuotaResetRequest) (cpakit.QuotaResetResponse, error) {
	return cpakit.QuotaResetResponse{}, cpakit.ErrUnhandled
}

// StaticModels returns an empty roster on purpose: the static path carries no
// credential, so per-account exclusions cannot be applied there and a static
// roster would re-advertise models the user hid. Registration runs through
// model.for_auth, which carries the credential and therefore the exclusions.
func (g *gateway) StaticModels(context.Context, cpakit.StaticModelRequest) (cpakit.ModelResponse, error) {
	return cpakit.ModelResponse{Provider: providerKey}, nil
}

// ModelsForAuth reports the roster one credential can actually drive.
//
// When the catalog is unreachable this falls back to the static roster: an empty
// model list would remove the provider from the picker entirely, which is worse
// than a conservative list.
func (g *gateway) ModelsForAuth(ctx context.Context, req cpakit.AuthModelRequest) (cpakit.ModelResponse, error) {
	models, errModels := g.impl.modelsForAuth(ctx, req, req.StorageJSON)
	if errModels != nil {
		g.impl.recordError(errModels)
		return cpakit.ModelResponse{
			Provider: providerKey,
			Models:   cpakit.ExposeOnly(mustStaticRoutes(regionFor(firstNonEmpty(req.Attributes["region"], g.impl.cfg.region))), g.impl.expose),
		}, nil
	}
	return cpakit.ModelResponse{Provider: providerKey, Models: cpakit.ExposeOnly(models, g.impl.expose)}, nil
}

// RegisterModels contributes nothing: ModelProvider already registers the live
// roster, and this legacy path re-adds every model without applying exclusions.
func (g *gateway) RegisterModels(context.Context, cpakit.ModelRegistrationRequest) (cpakit.ModelRegistrationResponse, error) {
	return cpakit.ModelRegistrationResponse{Provider: providerKey}, nil
}

// RegisterManagement declares the plugin's diagnostic route.
func (g *gateway) RegisterManagement(context.Context) (cpakit.ManagementRegistrationResponse, error) {
	return cpakit.ManagementRegistrationResponse{
		// The host prefixes a relative path with the management base, so this
		// lands on /v0/management/plugins/cpa-provider-qoder/status. A bare
		// "status" would occupy a global path shared by every plugin.
		Routes: []cpakit.ManagementRoute{{
			Method:      "GET",
			Path:        "plugins/cpa-provider-qoder/status",
			Description: "Credential, endpoint, codec, and catalog diagnostics for this provider",
		}},
	}, nil
}

// HandleManagement serves the diagnostic route.
func (g *gateway) HandleManagement(ctx context.Context, req cpakit.ManagementRequest) (cpakit.ManagementResponse, error) {
	if !strings.HasSuffix(strings.TrimSuffix(req.Path, "/"), "/status") {
		return cpakit.ManagementResponse{StatusCode: 404, Body: []byte("{\"error\":\"not found\"}")}, nil
	}
	body, errMarshal := json.MarshalIndent(g.statusReport(ctx), "", "  ")
	if errMarshal != nil {
		return cpakit.ManagementResponse{StatusCode: 500, Body: []byte("{\"error\":\"encode diagnostics\"}")}, nil
	}
	return cpakit.JSONManagementResponse(200, body)
}

// ---- helpers ----

// ownsProvider reports whether a provider key names this provider.
//
// An empty key means the host did not say which credential it is asking about,
// and this plugin is the only one that can answer, so it counts as ours.
func (g *gateway) ownsProvider(provider string) bool {
	trimmed := strings.TrimSpace(provider)
	return trimmed == "" || strings.EqualFold(trimmed, providerKey)
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
	probe := []byte("{\"probe\":true}")
	decoded, errDecode := qoderDecodeBody(qoderEncodeBody(probe))
	if errDecode != nil || !bytes.Equal(decoded, probe) {
		return "broken"
	}
	return "ok"
}

// identityUID returns the subscriber id used to salt the session key.
func (p *pluginState) identityUID(storageJSON []byte) string {
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
