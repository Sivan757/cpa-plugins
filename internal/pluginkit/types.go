// Package pluginkit implements the CLIProxyAPI (CPA) native plugin contract in
// pure Go with no dependency on the CPA source tree.
//
// The CPA plugin ABI is a C function table that exchanges JSON envelopes. Every
// JSON shape below mirrors the host-side structs in
// CLIProxyAPI/sdk/pluginapi/types.go so that a plugin built against this package
// stays compatible. Field names matter: some host structs carry JSON tags and
// some do not, and Go falls back to the Go field name when no tag is present.
package pluginkit

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the RPC schema version this package implements.
//
// Version 6 preserves raw JSON bodies for plugin management responses, which we
// want so that quota and model payloads are not HTML-escaped.
const SchemaVersion uint32 = 6

// ABIVersion is the native C ABI shape version.
const ABIVersion uint32 = 1

// Metadata mirrors pluginapi.Metadata. The host struct has no JSON tags, so the
// wire keys are the exported Go field names.
type Metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []ConfigField
}

// ConfigField mirrors pluginapi.ConfigField (no JSON tags on the host side).
type ConfigField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}

// ConfigFieldType values understood by the management UI.
const (
	FieldString  = "string"
	FieldNumber  = "number"
	FieldInteger = "integer"
	FieldBoolean = "boolean"
	FieldEnum    = "enum"
	FieldArray   = "array"
	FieldObject  = "object"
)

// Capabilities mirrors rpcCapabilities, which does carry snake_case JSON tags.
type Capabilities struct {
	QuotaProvider  bool `json:"quota_provider,omitempty"`
	AuthProvider   bool `json:"auth_provider,omitempty"`
	ModelProvider  bool `json:"model_provider,omitempty"`
	ModelRegistrar bool `json:"model_registrar,omitempty"`
	ModelRouter    bool `json:"model_router,omitempty"`
	Executor       bool `json:"executor,omitempty"`

	ExecutorModelScope    string   `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`

	ManagementAPI bool `json:"management_api,omitempty"`
}

// Executor model scopes.
const (
	ScopeBoth   = "both"
	ScopeStatic = "static"
	ScopeOAuth  = "oauth"
)

// ModelInfo mirrors pluginapi.ModelInfo (no JSON tags on the host side).
type ModelInfo struct {
	ID          string
	Object      string
	Created     int64
	OwnedBy     string
	Type        string
	DisplayName string
	Name        string
	Version     string
	Description string

	InputTokenLimit  int64
	OutputTokenLimit int64

	SupportedGenerationMethods []string
	ContextLength              int64
	MaxCompletionTokens        int64
	SupportedParameters        []string
	SupportedInputModalities   []string
	SupportedOutputModalities  []string

	Thinking    *ThinkingSupport
	UserDefined bool
}

// ThinkingSupport mirrors pluginapi.ThinkingSupport.
type ThinkingSupport struct {
	Min            int
	Max            int
	ZeroAllowed    bool
	DynamicAllowed bool
	Levels         []string
}

// ModelResponse mirrors pluginapi.ModelResponse.
type ModelResponse struct {
	Provider   string
	Models     []ModelInfo
	AuthUpdate *AuthData `json:",omitempty"`
}

// ModelRegistrationRequest mirrors pluginapi.ModelRegistrationRequest.
type ModelRegistrationRequest struct {
	Plugin Metadata
}

// ModelRegistrationResponse mirrors pluginapi.ModelRegistrationResponse.
type ModelRegistrationResponse struct {
	Provider string
	Models   []ModelInfo
}

// StaticModelRequest mirrors pluginapi.StaticModelRequest.
type StaticModelRequest struct {
	Plugin Metadata
	Host   HostConfigSummary
}

// AuthModelRequest mirrors pluginapi.AuthModelRequest.
type AuthModelRequest struct {
	Plugin       Metadata
	AuthID       string
	AuthProvider string
	StorageJSON  []byte
	Metadata     map[string]any
	Attributes   map[string]string
	Host         HostConfigSummary
}

// HostConfigSummary mirrors pluginapi.HostConfigSummary (no JSON tags).
type HostConfigSummary struct {
	AuthDir          string
	ProxyURL         string
	ForceModelPrefix bool
	OAuthModelAlias  map[string][]ModelAlias
	ExcludedModels   map[string][]string
}

// ModelAlias mirrors pluginapi.ModelAlias.
type ModelAlias struct {
	Name  string
	Alias string
}

// AuthData mirrors pluginapi.AuthData (no JSON tags).
type AuthData struct {
	Provider         string
	ID               string
	FileName         string
	Label            string
	Prefix           string
	ProxyURL         string
	Disabled         bool
	StorageJSON      []byte
	Metadata         map[string]any
	Attributes       map[string]string
	NextRefreshAfter time.Time
}

// AuthParseRequest mirrors pluginapi.AuthParseRequest.
type AuthParseRequest struct {
	Provider string
	Path     string
	FileName string
	RawJSON  []byte
	Host     HostConfigSummary
}

// AuthParseResponse mirrors pluginapi.AuthParseResponse.
type AuthParseResponse struct {
	Handled bool
	Auth    AuthData
	Auths   []AuthData
}

// AuthLoginStartRequest mirrors pluginapi.AuthLoginStartRequest.
type AuthLoginStartRequest struct {
	Provider string
	BaseURL  string
	Host     HostConfigSummary
	Metadata map[string]any
}

// AuthLoginStartResponse mirrors pluginapi.AuthLoginStartResponse.
type AuthLoginStartResponse struct {
	Provider  string
	URL       string
	State     string
	ExpiresAt time.Time
	Metadata  map[string]any
}

// AuthLoginPollRequest mirrors pluginapi.AuthLoginPollRequest.
type AuthLoginPollRequest struct {
	Provider string
	State    string
	Host     HostConfigSummary
	Metadata map[string]any
}

// AuthLoginStatus values.
const (
	LoginPending = "pending"
	LoginSuccess = "success"
	LoginError   = "error"
)

// AuthLoginPollResponse mirrors pluginapi.AuthLoginPollResponse.
type AuthLoginPollResponse struct {
	Status  string
	Message string
	Auth    AuthData
	Auths   []AuthData
}

// AuthRefreshRequest mirrors pluginapi.AuthRefreshRequest.
type AuthRefreshRequest struct {
	AuthID       string
	AuthProvider string
	StorageJSON  []byte
	Metadata     map[string]any
	Attributes   map[string]string
	Host         HostConfigSummary
}

// AuthRefreshResponse mirrors pluginapi.AuthRefreshResponse.
type AuthRefreshResponse struct {
	Auth             AuthData
	NextRefreshAfter time.Time
}

// QuotaProviderInterface method names.
const (
	MethodPluginRegister    = "plugin.register"
	MethodPluginQuiesce     = "plugin.quiesce"
	MethodPluginReconfigure = "plugin.reconfigure"
	MethodPluginShutdown    = "plugin.shutdown"

	MethodModelRegister = "model.register"
	MethodModelStatic   = "model.static"
	MethodModelForAuth  = "model.for_auth"
	MethodModelRoute    = "model.route"

	MethodAuthIdentifier = "auth.identifier"
	MethodAuthParse      = "auth.parse"
	MethodAuthLoginStart = "auth.login.start"
	MethodAuthLoginPoll  = "auth.login.poll"
	MethodAuthRefresh    = "auth.refresh"

	MethodExecutorIdentifier    = "executor.identifier"
	MethodExecutorExecute       = "executor.execute"
	MethodExecutorExecuteStream = "executor.execute_stream"
	MethodExecutorCountTokens   = "executor.count_tokens"
	MethodExecutorHTTPRequest   = "executor.http_request"

	MethodQuotaIdentifier = "quota.identifier"
	MethodQuotaDescribe   = "quota.describe"
	MethodQuotaFetch      = "quota.fetch"
	MethodQuotaReset      = "quota.reset"

	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"

	MethodHostHTTPDo       = "host.http.do"
	MethodHostHTTPDoStream = "host.http.do_stream"
	MethodHostLog          = "host.log"
	MethodHostAuthList     = "host.auth.list"
	MethodHostAuthGet      = "host.auth.get"
	MethodHostAuthSave     = "host.auth.save"
	MethodHostStreamEmit   = "host.stream.emit"
	MethodHostStreamClose  = "host.stream.close"
	MethodHostModelExecute = "host.model.execute"
)

// rawMessage keeps the marshalling helpers below honest about intent.
type rawMessage = json.RawMessage
