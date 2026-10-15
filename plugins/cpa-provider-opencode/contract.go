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
var pluginVersion = "0.6.0"

// providerKey is the single provider key this plugin serves. Both credential
// tiers sit behind it, so overlapping model identifiers never compete across
// plugins.
const providerKey = "opencode"

// pluginName is the management-visible plugin name.
const pluginName = "CLIProxyAPI OpenCode Provider"

// gateway is the plugin: one pluginState plus every capability the provider
// declares.
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
		Name:             pluginName,
		Version:          pluginVersion,
		Author:           "cpa-plugins",
		GitHubRepository: "https://github.com/Sivan757/cpa-plugins",
		ConfigFields: []cpakit.ConfigField{
			{
				Name:        "api_key",
				Type:        cpakit.FieldString,
				Description: "OpenCode Go API key (oc_sk_...). Several keys may be given, separated by commas.",
			},
			{
				Name:        "free_enabled",
				Type:        cpakit.FieldBoolean,
				Description: "Advertise the free-tier models (default true).",
			},
			{
				Name:        "session_token",
				Type:        cpakit.FieldString,
				Description: "OpenCode console session token (st_...). The free tier requires it. Read it from a signed-in install or leave it to the refresh token.",
			},
			{
				Name:        "refresh_token",
				Type:        cpakit.FieldString,
				Description: "OpenCode refresh token (rt_...). When set, the session token is renewed automatically before it expires.",
			},
			{
				Name:        "org_id",
				Type:        cpakit.FieldString,
				Description: "Active OpenCode workspace id (wrk_...). The free tier requires it alongside the session token.",
			},
			{
				Name:        "credential_dir",
				Type:        cpakit.FieldString,
				Description: "Directory holding the rotating account credential. Set it to a mounted path in a container.",
			},
			{
				Name:        "user_agent_version",
				Type:        cpakit.FieldString,
				Description: "OpenCode CLI version used for the upstream user agent. Discovered from npm when unset.",
			},
			{
				Name:        "base_url",
				Type:        cpakit.FieldString,
				Description: "Free-tier upstream root (default https://opencode.ai/zen/v1).",
			},
			{
				Name:        "expose",
				Type:        cpakit.FieldArray,
				Description: "Only advertise models matching these patterns (e.g. '*-free'). Empty advertises everything.",
			},
			{
				Name:        "go_base_url",
				Type:        cpakit.FieldString,
				Description: "Paid-tier upstream root (default https://opencode.ai/zen/go/v1).",
			},
		},
	}
}

// ProviderKey implements cpakit.Plugin.
func (g *gateway) ProviderKey() string { return providerKey }

// Capabilities implements cpakit.Plugin. Both tiers speak chat-completions.
func (g *gateway) Capabilities() cpakit.Capabilities {
	return cpakit.Capabilities{
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
	// Every field is recomputed from its default on each configure. A value
	// removed from the configuration must revert: assigning only when a key is
	// present leaves the previous value in place, which makes a field impossible
	// to clear without restarting the core.
	impl := g.impl
	impl.apiKeys = splitKeys(cfg.String("api_key", ""))
	impl.freeEnabled = cfg.Bool("free_enabled", true)
	impl.expose = cfg.Strings("expose")
	impl.credentialDir = strings.TrimSpace(cfg.String("credential_dir", ""))
	impl.baseURL = strings.TrimSuffix(cfg.String("base_url", ""), "/")
	impl.goURL = strings.TrimSuffix(cfg.String("go_base_url", ""), "/")

	// The user agent decides whether the free tier answers at all, so an explicit
	// version wins and the published one is discovered once to fill the gap.
	// Resolving on every configure would put a network call in the reload path.
	if version := strings.TrimSpace(cfg.String("user_agent_version", "")); version != "" {
		impl.version = version
	} else if !impl.versionResolved {
		impl.version = resolveClientVersion(context.Background())
		impl.versionResolved = true
	}

	// The account credential is loaded from disk and then overridden by anything
	// set explicitly, so a persisted rotation survives a reload while an operator
	// can still replace the credential from the config.
	if g.impl.account == nil {
		g.impl.account = &accountAuth{}
	}
	g.impl.account.dir = g.impl.credentialDir
	// A persisted document carries the post-rotation tokens, so it outranks the
	// configuration; the configuration bootstraps the first run.
	persisted := g.impl.account.load()
	g.impl.account.apply(
		cfg.String("session_token", ""),
		cfg.String("refresh_token", ""),
		cfg.String("org_id", ""),
		!persisted,
	)

	cpakit.HostLog(context.Background(), "info", fmt.Sprintf(
		"opencode: configured expose=%v freeEnabled=%t goKeys=%d account=%t refresh=%t org=%q dir=%q",
		g.impl.expose, g.impl.freeEnabled, len(g.impl.apiKeys),
		g.impl.account.configured(), g.impl.account.hasRefresh(), g.impl.account.org(), g.impl.credentialDir))
	g.impl.materializeTiers()
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

// ParseAuth claims an OpenCode credential. The record shape is
// {"type":"opencode","tier":"go","api_key":"oc_sk_..."} for the paid plan and
// {"type":"opencode","tier":"free"} for the anonymous tier.
func (g *gateway) ParseAuth(_ context.Context, req cpakit.AuthParseRequest) (cpakit.AuthParseResponse, error) {
	var document map[string]any
	if len(req.RawJSON) > 0 {
		if errUnmarshal := json.Unmarshal(req.RawJSON, &document); errUnmarshal != nil {
			return cpakit.AuthParseResponse{}, cpakit.BadRequestError("decode opencode credential: " + errUnmarshal.Error())
		}
	}
	if req.Provider != "" && req.Provider != providerKey {
		return cpakit.AuthParseResponse{Handled: false}, nil
	}
	typeName, _ := document["type"].(string)
	providerName, _ := document["provider"].(string)
	if typeName != providerKey && providerName != providerKey {
		return cpakit.AuthParseResponse{Handled: false}, nil
	}
	tier := tierFree
	if rawTier, okTier := document["tier"].(string); okTier && strings.TrimSpace(rawTier) != "" {
		tier = strings.ToLower(strings.TrimSpace(rawTier))
	}
	key, _ := document["api_key"].(string)
	if tier == tierGo && strings.TrimSpace(key) == "" {
		return cpakit.AuthParseResponse{}, cpakit.BadRequestError("opencode Go auth record has no api_key")
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
	return cpakit.AuthParseResponse{Handled: true, Auth: cpakit.AuthData{
		Provider:    providerKey,
		ID:          id,
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: req.RawJSON,
		Attributes:  attributes,
	}}, nil
}

// StartLogin is unsupported: both tiers authenticate with a static credential.
func (g *gateway) StartLogin(context.Context, cpakit.AuthLoginStartRequest) (cpakit.AuthLoginStartResponse, error) {
	return cpakit.AuthLoginStartResponse{}, cpakit.NewError("unsupported", "opencode logins are not interactive; add an auth file instead", 400)
}

// PollLogin is unsupported: there is no interactive flow.
func (g *gateway) PollLogin(context.Context, cpakit.AuthLoginPollRequest) (cpakit.AuthLoginPollResponse, error) {
	return cpakit.AuthLoginPollResponse{}, cpakit.NewError("unsupported", "opencode logins are not interactive", 400)
}

// RefreshAuth is a no-op: both credentials are static.
func (g *gateway) RefreshAuth(_ context.Context, req cpakit.AuthRefreshRequest) (cpakit.AuthRefreshResponse, error) {
	return cpakit.AuthRefreshResponse{Auth: cpakit.AuthData{
		Provider:    req.AuthProvider,
		ID:          req.AuthID,
		StorageJSON: req.StorageJSON,
		Attributes:  req.Attributes,
	}}, nil
}

// StaticModels publishes the free catalogue, which needs no auth record.
func (g *gateway) StaticModels(ctx context.Context, req cpakit.StaticModelRequest) (cpakit.ModelResponse, error) {
	models := g.impl.freeModels(ctx)
	excluded := cpakit.ExcludedModelsFor(req.Host, providerKey)
	models = cpakit.FilterModelsByExclusion(models, excluded)
	return cpakit.ModelResponse{Provider: providerKey, Models: cpakit.ExposeOnly(models, g.impl.expose)}, nil
}

// ModelsForAuth publishes the catalogue the credential's tier may serve.
func (g *gateway) ModelsForAuth(ctx context.Context, req cpakit.AuthModelRequest) (cpakit.ModelResponse, error) {
	tier := tierOf(req.Attributes, req.StorageJSON)
	models := g.impl.modelsForTier(ctx, tier)
	excluded := cpakit.ExcludedModelsFor(req.Host, providerKey)
	models = cpakit.FilterModelsByExclusion(models, excluded)
	return cpakit.ModelResponse{Provider: providerKey, Models: cpakit.ExposeOnly(models, g.impl.expose)}, nil
}

// RegisterModels reports the anonymous catalogue.
func (g *gateway) RegisterModels(ctx context.Context, _ cpakit.ModelRegistrationRequest) (cpakit.ModelRegistrationResponse, error) {
	return cpakit.ModelRegistrationResponse{
		Provider: providerKey,
		Models:   cpakit.ExposeOnly(g.impl.freeModels(ctx), g.impl.expose),
	}, nil
}

// RegisterManagement declares the plugin's diagnostic route.
func (g *gateway) RegisterManagement(context.Context) (cpakit.ManagementRegistrationResponse, error) {
	return cpakit.ManagementRegistrationResponse{
		Routes: []cpakit.ManagementRoute{{
			Method:      "GET",
			Path:        "plugins/cpa-provider-opencode/status",
			Description: "Catalogue and tier status for the OpenCode provider",
		}},
	}, nil
}

// HandleManagement serves the diagnostic route.
func (g *gateway) HandleManagement(ctx context.Context, req cpakit.ManagementRequest) (cpakit.ManagementResponse, error) {
	if !strings.HasSuffix(strings.TrimSuffix(req.Path, "/"), "/status") {
		return cpakit.ManagementResponse{StatusCode: 404, Body: []byte("{\"error\":\"not found\"}")}, nil
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
		// The free tier needs the account credential; report it so an operator can
		// see why a free request would be refused without reading the upstream body.
		"account": g.impl.account.report(),
	}
	body, _ := json.MarshalIndent(report, "", "  ")
	return cpakit.JSONManagementResponse(200, body)
}

var _ = time.Second
