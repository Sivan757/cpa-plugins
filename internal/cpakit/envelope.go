// Package cpakit implements the CLIProxyAPI native plugin contract on top of
// the official SDK types (CLIProxyAPI/v8/sdk/pluginabi and pluginapi).
//
// The host loads a dynamic library, resolves cliproxy_plugin_init, and exchanges
// JSON envelopes through the function tables the ABI declares. This package owns
// the envelope, the host bridge, the error classification, and the registration
// shape, so a provider plugin only writes its own method dispatch and vendor
// wire logic.
package cpakit

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// emptyResult is the success envelope for a method with no payload.
const emptyResult = "{\"ok\":true,\"result\":{}}"

// encodeFailure is the last-resort envelope, used when even marshalling fails.
const encodeFailure = "{\"ok\":false,\"error\":{\"code\":\"internal_error\",\"message\":\"encode envelope\"}}"

// OK encodes a successful envelope whose result is v.
func OK(v any) []byte {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return Fail("internal_error", "encode result: "+errMarshal.Error())
	}
	return OKRaw(raw)
}

// OKRaw encodes a successful envelope from an already-encoded result.
func OKRaw(result []byte) []byte {
	out, errMarshal := json.Marshal(pluginabi.Envelope{OK: true, Result: json.RawMessage(result)})
	if errMarshal != nil {
		return []byte(encodeFailure)
	}
	return out
}

// Empty encodes the success envelope for a method that returns nothing.
func Empty() []byte { return []byte(emptyResult) }

// Fail encodes a failed envelope with an optional HTTP status.
func Fail(code, message string, httpStatus ...int) []byte {
	status := 0
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	out, errMarshal := json.Marshal(pluginabi.Envelope{
		OK:    false,
		Error: &pluginabi.Error{Code: code, Message: message, HTTPStatus: status},
	})
	if errMarshal != nil {
		return []byte(encodeFailure)
	}
	return out
}

// Envelope is the JSON message exchanged across the C ABI in both directions.
type Envelope = pluginabi.Envelope

// Capabilities mirrors the host's rpcCapabilities wire shape. Only the fields a
// provider plugin can meaningfully declare are listed.
type Capabilities struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool     `json:"management_api"`
	QuotaProvider         bool     `json:"quota_provider"`
}

// Executor model scopes understood by the host.
const (
	ScopeBoth   = "both"
	ScopeStatic = "static"
	ScopeOAuth  = "oauth"
)

// registration is the plugin.register / plugin.reconfigure response body.
type registration struct {
	SchemaVersion uint32 `json:"schema_version"`
	Metadata      any    `json:"metadata"`
	Capabilities  any    `json:"capabilities"`
}
