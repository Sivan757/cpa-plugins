package pluginkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Plugin is implemented by every CPA plugin built on this package.
type Plugin interface {
	// Metadata describes the plugin for the host registry and management UI.
	Metadata() Metadata
	// Capabilities declares which capabilities this plugin implements. The host
	// only calls the RPC methods whose capability flag is true.
	Capabilities() Capabilities
	// Configure receives the plugin's own config block as raw YAML. The host
	// sends it on plugin.register and again on plugin.reconfigure.
	Configure(configYAML []byte) error
	// Dispatch handles one capability RPC. Returning ErrUnhandled tells the
	// runtime that this plugin does not serve the method.
	Dispatch(ctx context.Context, method string, req json.RawMessage) (any, error)
}

// Runtime drives the JSON envelope protocol for one plugin.
type Runtime struct {
	plugin Plugin
}

// NewRuntime wraps a plugin in a Runtime.
func NewRuntime(p Plugin) *Runtime {
	return &Runtime{plugin: p}
}

// registration is the plugin.register / plugin.reconfigure response body.
type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      Metadata     `json:"metadata"`
	Capabilities  Capabilities `json:"capabilities"`
}

// lifecycleRequest is the plugin.register / plugin.reconfigure request body.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// Handle processes one inbound RPC and returns the encoded envelope. It never
// panics across the C ABI boundary: a panic is converted into an error envelope
// because a panic escaping into C aborts the whole host process.
func (r *Runtime) Handle(method string, request []byte) (out []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = Fail("plugin_panic", fmt.Sprintf("panic handling %s: %v", method, recovered))
		}
	}()

	SetHostCallbackID(callbackIDFromRequest(request))

	result, errDispatch := r.dispatch(method, request)
	if errDispatch != nil {
		if errors.Is(errDispatch, ErrUnhandled) {
			return Fail("unknown_method", "unknown method: "+method)
		}
		return Fail(codeOf(errDispatch), errDispatch.Error(), statusOf(errDispatch))
	}
	if result == nil {
		return []byte(`{"ok":true,"result":{}}`)
	}
	encoded, errEncode := OK(result)
	if errEncode != nil {
		return Fail("internal_error", errEncode.Error())
	}
	return encoded
}

func (r *Runtime) dispatch(method string, request []byte) (any, error) {
	switch method {
	case MethodPluginRegister, MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
				return nil, NewError("invalid_request", "decode lifecycle request: "+errUnmarshal.Error())
			}
		}
		if errConfigure := r.plugin.Configure(req.ConfigYAML); errConfigure != nil {
			return nil, NewError("config_invalid", errConfigure.Error())
		}
		return registration{
			SchemaVersion: SchemaVersion,
			Metadata:      r.plugin.Metadata(),
			Capabilities:  r.plugin.Capabilities(),
		}, nil
	case MethodPluginQuiesce:
		return map[string]any{}, nil
	case MethodPluginShutdown:
		return map[string]any{}, nil
	}

	ctx := context.Background()
	return r.plugin.Dispatch(ctx, method, json.RawMessage(request))
}

// DispatchStandard serves the identifier RPCs shared by all capability types.
// Plugins call it from their own Dispatch before handling anything else; it
// returns (result, true, nil) when it served the method.
func (r *Runtime) DispatchStandard(method string, identifier string) (any, bool) {
	switch method {
	case MethodQuotaIdentifier, MethodAuthIdentifier, MethodExecutorIdentifier:
		return map[string]string{"identifier": identifier}, true
	}
	return nil, false
}

// callbackIDFromRequest extracts the trailing host_callback_id field the RPC
// layer appends to nested callback requests.
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

// Decode is a small helper for capability handlers.
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
