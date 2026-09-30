package pluginkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Base is the shared dispatch layer every vendor plugin embeds. A vendor
// implements the capability interfaces it supports, fills in the descriptor,
// and Base routes the RPC methods to it.
//
// Base implements Plugin, so a vendor only supplies the descriptor and the
// capability implementations.
type Base struct {
	// Identity describes this plugin. Set once during construction.
	Identity Metadata
	// ProviderKey is the credential provider key. The host matches auth records
	// to this plugin by provider key, so it must be stable.
	ProviderKey string
	// InputFormats and OutputFormats declare the protocols the executor speaks.
	InputFormats  []string
	OutputFormats []string

	// Quota is the optional quota provider.
	Quota QuotaProvider
	// Auth is the optional auth provider.
	Auth AuthProvider
	// Models is the optional model provider.
	Models ModelProvider
	// Exec is the optional executor.
	Exec Executor
	// Router is the optional model router.
	Router ModelRouter
	// Mgmt is the optional management API provider.
	Mgmt ManagementAPI

	// OnConfigure receives the raw plugin config on register and reconfigure.
	OnConfigure func(cfg *Config) error

	config *Config
}

// Config returns the most recently delivered configuration.
func (b *Base) Config() *Config {
	if b.config == nil {
		return &Config{values: map[string]string{}, nested: map[string]map[string]string{}}
	}
	return b.config
}

// Metadata implements Plugin.
func (b *Base) Metadata() Metadata { return b.Identity }

// Capabilities implements Plugin.
func (b *Base) Capabilities() Capabilities {
	caps := Capabilities{
		QuotaProvider:         b.Quota != nil,
		AuthProvider:          b.Auth != nil,
		ModelProvider:         b.Models != nil,
		ModelRouter:           b.Router != nil,
		ManagementAPI:         b.Mgmt != nil,
		Executor:              b.Exec != nil,
		ExecutorModelScope:    ScopeBoth,
		ExecutorInputFormats:  b.InputFormats,
		ExecutorOutputFormats: b.OutputFormats,
	}
	if b.Exec == nil {
		caps.ExecutorModelScope = ""
		caps.ExecutorInputFormats = nil
		caps.ExecutorOutputFormats = nil
	}
	return caps
}

// Configure implements Plugin.
func (b *Base) Configure(configYAML []byte) error {
	cfg, errParse := ParseConfig(configYAML)
	if errParse != nil {
		return errParse
	}
	b.config = cfg
	if b.OnConfigure != nil {
		return b.OnConfigure(cfg)
	}
	return nil
}

// Dispatch implements Plugin.
func (b *Base) Dispatch(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if result, handled := (&Runtime{}).DispatchStandard(method, b.ProviderKey); handled && b.capabilityForIdentifier(method) {
		return result, nil
	}

	switch method {
	// ---- quota ----
	case MethodQuotaDescribe:
		if b.Quota == nil {
			return nil, ErrUnhandled
		}
		return b.Quota.DescribeQuota(), nil
	case MethodQuotaFetch:
		if b.Quota == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[QuotaFetchRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Quota.FetchQuota(ctx, req)
	case MethodQuotaReset:
		if b.Quota == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[QuotaResetRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Quota.ResetQuota(ctx, req)

	// ---- auth ----
	case MethodAuthParse:
		if b.Auth == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[AuthParseRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Auth.ParseAuth(ctx, req)
	case MethodAuthLoginStart:
		if b.Auth == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[AuthLoginStartRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Auth.StartLogin(ctx, req)
	case MethodAuthLoginPoll:
		if b.Auth == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[AuthLoginPollRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Auth.PollLogin(ctx, req)
	case MethodAuthRefresh:
		if b.Auth == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[AuthRefreshRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Auth.RefreshAuth(ctx, req)

	// ---- models ----
	case MethodModelStatic:
		if b.Models == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[StaticModelRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Models.StaticModels(ctx, req)
	case MethodModelForAuth:
		if b.Models == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[AuthModelRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Models.ModelsForAuth(ctx, req)
	case MethodModelRegister:
		if b.Models == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[ModelRegistrationRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Models.RegisterModels(ctx, req)

	// ---- routing and execution ----
	case MethodModelRoute:
		if b.Router == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[ModelRouteRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Router.RouteModel(ctx, req)
	case MethodExecutorExecute:
		if b.Exec == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[ExecutorRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Exec.Execute(ctx, req)
	case MethodExecutorExecuteStream:
		if b.Exec == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[ExecutorRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Exec.ExecuteStream(ctx, req)
	case MethodExecutorCountTokens:
		if b.Exec == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[ExecutorRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Exec.CountTokens(ctx, req)
	case MethodExecutorHTTPRequest:
		if b.Exec == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[ExecutorHTTPRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Exec.HttpRequest(ctx, req)

	// ---- management ----
	case MethodManagementRegister:
		if b.Mgmt == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[ManagementRegistrationRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Mgmt.RegisterManagement(ctx, req)
	case MethodManagementHandle:
		if b.Mgmt == nil {
			return nil, ErrUnhandled
		}
		req, errDecode := Decode[ManagementRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		return b.Mgmt.HandleManagement(ctx, req)
	}

	return nil, ErrUnhandled
}

// capabilityForIdentifier guards the shared identifier RPCs: they must only be
// answered when the matching capability is actually implemented, otherwise the
// host would think this plugin owns a provider key it cannot serve.
func (b *Base) capabilityForIdentifier(method string) bool {
	switch method {
	case MethodQuotaIdentifier:
		return b.Quota != nil
	case MethodAuthIdentifier:
		return b.Auth != nil
	case MethodExecutorIdentifier:
		return b.Exec != nil
	}
	return false
}

// ModelProvider is implemented by plugins that contribute model metadata.
type ModelProvider interface {
	StaticModels(ctx context.Context, req StaticModelRequest) (ModelResponse, error)
	ModelsForAuth(ctx context.Context, req AuthModelRequest) (ModelResponse, error)
	RegisterModels(ctx context.Context, req ModelRegistrationRequest) (ModelRegistrationResponse, error)
}

// AuthProvider is implemented by plugins that own credential lifecycle.
type AuthProvider interface {
	ParseAuth(ctx context.Context, req AuthParseRequest) (AuthParseResponse, error)
	StartLogin(ctx context.Context, req AuthLoginStartRequest) (AuthLoginStartResponse, error)
	PollLogin(ctx context.Context, req AuthLoginPollRequest) (AuthLoginPollResponse, error)
	RefreshAuth(ctx context.Context, req AuthRefreshRequest) (AuthRefreshResponse, error)
}

// NoOpModelProvider supplies empty model responses for the registration RPCs a
// plugin does not need. Embed it to satisfy ModelProvider with only
// ModelsForAuth implemented.
type NoOpModelProvider struct{}

// StaticModels returns no static models.
func (NoOpModelProvider) StaticModels(context.Context, StaticModelRequest) (ModelResponse, error) {
	return ModelResponse{}, nil
}

// RegisterModels registers nothing.
func (NoOpModelProvider) RegisterModels(context.Context, ModelRegistrationRequest) (ModelRegistrationResponse, error) {
	return ModelRegistrationResponse{}, nil
}

// ErrUnsupported is returned by optional capability methods.
var ErrUnsupported = errors.New("capability not supported by this provider")

// NotFoundError builds a 404 plugin error.
func NotFoundError(what string) *PluginError {
	return NewError("not_found", what+" not found", http.StatusNotFound)
}

// BadRequestError builds a 400 plugin error.
func BadRequestError(message string) *PluginError {
	return NewError("invalid_request", message, http.StatusBadRequest)
}

// Wrapf adds context to an error while preserving any PluginError inside it.
func Wrapf(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(format+": %w", append(args, err)...)
}
