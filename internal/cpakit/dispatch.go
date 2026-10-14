package cpakit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Plugin is the per-provider half of the contract. A provider implements only
// its own methods; registration, quiesce, and shutdown are handled here because
// their shape is identical for every plugin.
type Plugin interface {
	// Metadata describes the plugin for the host registry and management UI.
	Metadata() pluginapi.Metadata
	// ProviderKey is the credential provider key. The host matches auth records
	// to this plugin by provider key, and the identifier RPCs must answer with
	// it, so it must stay stable across releases.
	ProviderKey() string
	// Capabilities declares which capability flags the host should honour.
	Capabilities() Capabilities
	// Configure receives the plugin's own config block as raw YAML on every
	// register and reconfigure.
	Configure(configYAML []byte) error
	// Dispatch handles one capability RPC. Returning ErrUnhandled tells the
	// runtime this plugin does not serve the method.
	Dispatch(ctx context.Context, method string, request json.RawMessage) (any, error)
}

// ErrUnhandled reports a method this plugin does not implement.
var ErrUnhandled = errors.New("method not handled by plugin")

// HandleCall is the entry point the generated C ABI glue uses: one method plus
// request bytes in, one encoded envelope out. It is exported so tests can drive
// a plugin exactly the way the host does.
func HandleCall(method string, request []byte) []byte { return Handle(method, request) }

// Runtime drives the JSON envelope protocol for one plugin.
type Runtime struct {
	plugin Plugin
}

// NewRuntime wraps a plugin.
func NewRuntime(p Plugin) *Runtime { return &Runtime{plugin: p} }

// lifecycleRequest is the plugin.register / plugin.reconfigure request body.
// The host sends the plugin config block as raw YAML.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// Handle processes one inbound RPC and returns the encoded envelope. It never
// panics: a panic is converted into an error envelope, because a panic escaping
// into C aborts the host process.
func Handle(method string, request []byte) (out []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = Fail("plugin_panic", fmt.Sprintf("panic handling %s: %v", method, recovered))
		}
	}()

	runtime := NewRuntime(registered())
	SetHostCallbackID(callbackIDFromRequest(request))

	result, errDispatch := runtime.dispatch(method, request)
	if errDispatch != nil {
		if errors.Is(errDispatch, ErrUnhandled) {
			return Fail("unknown_method", "unknown method: "+method)
		}
		return EnvelopeFor(errDispatch)
	}
	if result == nil {
		return Empty()
	}
	return OK(result)
}

func (r *Runtime) dispatch(method string, request []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
				return nil, NewError("invalid_request", "decode lifecycle request: "+errUnmarshal.Error())
			}
		}
		if errConfigure := r.plugin.Configure(req.ConfigYAML); errConfigure != nil {
			return nil, errConfigure
		}
		return registration{
			SchemaVersion: pluginabi.SchemaVersion,
			Metadata:      r.plugin.Metadata(),
			Capabilities:  r.plugin.Capabilities(),
		}, nil
	case pluginabi.MethodPluginQuiesce:
		return map[string]any{}, nil
	case pluginabi.MethodPluginShutdown:
		return r.shutdown(), nil
	case pluginabi.MethodQuotaIdentifier, pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return identifierFor(r.plugin.Capabilities(), r.plugin.ProviderKey(), method)
	}

	ctx := context.Background()
	return r.plugin.Dispatch(ctx, method, json.RawMessage(request))
}

// shutdown stops accepting work and drains host callbacks still in flight.
//
// On Unix the host frees its API table and unloads the library as soon as this
// RPC returns, so a goroutine still calling into the host would crash the
// process. The drain makes that window explicit.
func (r *Runtime) shutdown() any {
	bridge := Bridge()
	if bridge != nil && !bridge.WaitForInFlight(shutdownDrainTimeout) {
		_ = bridge.Log("warn", "shutdown proceeding with host callbacks still in flight", nil)
	}
	return map[string]any{}
}

// identifierFor answers the shared identifier RPCs, but only for a capability
// the plugin actually implements. Answering otherwise would claim a provider key
// the plugin cannot serve.
func identifierFor(caps Capabilities, providerKey, method string) (any, error) {
	switch method {
	case pluginabi.MethodQuotaIdentifier:
		if !caps.QuotaProvider {
			return nil, ErrUnhandled
		}
	case pluginabi.MethodAuthIdentifier:
		if !caps.AuthProvider {
			return nil, ErrUnhandled
		}
	case pluginabi.MethodExecutorIdentifier:
		if !caps.Executor {
			return nil, ErrUnhandled
		}
	}
	return map[string]string{"identifier": providerKey}, nil
}

// callbackIDFromRequest extracts the trailing host_callback_id the RPC layer
// appends to nested callback requests.
func callbackIDFromRequest(request []byte) string {
	if len(request) == 0 {
		return ""
	}
	var probe struct {
		HostCallbackID string `json:"host_callback_id"`
	}
	if errUnmarshal := json.Unmarshal(request, &probe); errUnmarshal != nil {
		return ""
	}
	return probe.HostCallbackID
}

// NewError builds a classified error with an HTTP status.
func NewError(code, message string, httpStatus ...int) *Error {
	status := 0
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	return &Error{Class: Class(code), Message: message, StatusCode: status}
}

// ErrUnsupported is returned by capability methods a provider does not supply.
var ErrUnsupported = errors.New("capability not supported by this provider")

// Decode is a helper for capability handlers.
func Decode[T any](raw json.RawMessage) (T, error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return out, NewError("invalid_request", errUnmarshal.Error())
	}
	return out, nil
}
