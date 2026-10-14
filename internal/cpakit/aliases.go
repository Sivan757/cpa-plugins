package cpakit

// This file re-exports the official SDK types under cpakit, so a provider
// plugin imports one package while the values it builds and passes are the
// host's own types. The aliases are exact: the official structs carry the same
// fields and JSON tags the provider code was written against.
//
// A Go alias (type X = Y) is used rather than a defined type, so values pass
// through to the host without conversion.

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ---- lifecycle and method names ----

// Method names accepted by the host.
const (
	MethodPluginRegister    = pluginabi.MethodPluginRegister
	MethodPluginQuiesce     = pluginabi.MethodPluginQuiesce
	MethodPluginReconfigure = pluginabi.MethodPluginReconfigure
	MethodPluginShutdown    = pluginabi.MethodPluginShutdown

	MethodModelRegister = pluginabi.MethodModelRegister
	MethodModelStatic   = pluginabi.MethodModelStatic
	MethodModelForAuth  = pluginabi.MethodModelForAuth

	MethodAuthIdentifier = pluginabi.MethodAuthIdentifier
	MethodAuthParse      = pluginabi.MethodAuthParse
	MethodAuthLoginStart = pluginabi.MethodAuthLoginStart
	MethodAuthLoginPoll  = pluginabi.MethodAuthLoginPoll
	MethodAuthRefresh    = pluginabi.MethodAuthRefresh

	MethodExecutorIdentifier    = pluginabi.MethodExecutorIdentifier
	MethodExecutorExecute       = pluginabi.MethodExecutorExecute
	MethodExecutorExecuteStream = pluginabi.MethodExecutorExecuteStream
	MethodExecutorCountTokens   = pluginabi.MethodExecutorCountTokens
	MethodExecutorHTTPRequest   = pluginabi.MethodExecutorHTTPRequest

	MethodManagementRegister = pluginabi.MethodManagementRegister
	MethodManagementHandle   = pluginabi.MethodManagementHandle

	MethodQuotaIdentifier = pluginabi.MethodQuotaIdentifier
	MethodQuotaDescribe   = pluginabi.MethodQuotaDescribe
	MethodQuotaFetch      = pluginabi.MethodQuotaFetch
	MethodQuotaReset      = pluginabi.MethodQuotaReset

	MethodHostHTTPDo          = pluginabi.MethodHostHTTPDo
	MethodHostHTTPDoStream    = pluginabi.MethodHostHTTPDoStream
	MethodHostHTTPStreamRead  = pluginabi.MethodHostHTTPStreamRead
	MethodHostHTTPStreamClose = pluginabi.MethodHostHTTPStreamClose
	MethodHostStreamEmit      = pluginabi.MethodHostStreamEmit
	MethodHostStreamClose     = pluginabi.MethodHostStreamClose
	MethodHostLog             = pluginabi.MethodHostLog
	MethodHostAuthList        = pluginabi.MethodHostAuthList
	MethodHostAuthGet         = pluginabi.MethodHostAuthGet
	MethodHostAuthSave        = pluginabi.MethodHostAuthSave
)

// ABIVersion and SchemaVersion of the native contract this package implements.
const (
	ABIVersion    = pluginabi.ABIVersion
	SchemaVersion = pluginabi.SchemaVersion
)

// ---- registration ----

// Metadata describes the plugin to the host registry and management UI.
type Metadata = pluginapi.Metadata

// ConfigField describes one plugin-owned configuration key.
type ConfigField = pluginapi.ConfigField

// Config field kinds understood by the management UI.
const (
	FieldString  = pluginapi.ConfigFieldTypeString
	FieldNumber  = pluginapi.ConfigFieldTypeNumber
	FieldInteger = pluginapi.ConfigFieldTypeInteger
	FieldBoolean = pluginapi.ConfigFieldTypeBoolean
	FieldEnum    = pluginapi.ConfigFieldTypeEnum
	FieldArray   = pluginapi.ConfigFieldTypeArray
	FieldObject  = pluginapi.ConfigFieldTypeObject
)

// ---- models ----

// ModelInfo is one advertised model.
type ModelInfo = pluginapi.ModelInfo

// ThinkingSupport declares a model's reasoning-effort control.
type ThinkingSupport = pluginapi.ThinkingSupport

// ModelProvider request and response shapes.
type (
	StaticModelRequest        = pluginapi.StaticModelRequest
	AuthModelRequest          = pluginapi.AuthModelRequest
	ModelRegistrationRequest  = pluginapi.ModelRegistrationRequest
	ModelResponse             = pluginapi.ModelResponse
	ModelRegistrationResponse = pluginapi.ModelRegistrationResponse
	HostConfigSummary         = pluginapi.HostConfigSummary
	ModelAlias                = pluginapi.ModelAlias
)

// ---- auth ----

// AuthData is one credential record.
type AuthData = pluginapi.AuthData

// AuthProvider request and response shapes.
type (
	AuthParseRequest       = pluginapi.AuthParseRequest
	AuthParseResponse      = pluginapi.AuthParseResponse
	AuthLoginStartRequest  = pluginapi.AuthLoginStartRequest
	AuthLoginStartResponse = pluginapi.AuthLoginStartResponse
	AuthLoginPollRequest   = pluginapi.AuthLoginPollRequest
	AuthLoginPollResponse  = pluginapi.AuthLoginPollResponse
	AuthRefreshRequest     = pluginapi.AuthRefreshRequest
	AuthRefreshResponse    = pluginapi.AuthRefreshResponse
	HostAuthFileEntry      = pluginapi.HostAuthFileEntry
	HostAuthSaveRequest    = pluginapi.HostAuthSaveRequest
)

// ---- executor ----

// ExecutorRequest is one model execution request.
//
// StreamID is not part of the official struct: the host's RPC envelope carries
// it alongside, so the runtime decodes it separately through ExecutorEnvelope.
type ExecutorRequest = pluginapi.ExecutorRequest

// ExecutorResponse is a non-streaming executor result.
type ExecutorResponse = pluginapi.ExecutorResponse

// ExecutorStreamChunk is one buffered stream chunk.
type ExecutorStreamChunk = pluginapi.ExecutorStreamChunk

// ExecutorStreamResponse is deliberately NOT the SDK's own type.
//
// The SDK carries its chunks on a channel, which an in-process caller can drain,
// but a plugin result is marshalled to JSON at the RPC boundary and a channel
// has no JSON form. The host's rpcExecutorStreamResponse carries an array, so
// that is the shape a plugin returns. See executor.go.

// ExecutorHTTPRequest and ExecutorHTTPResponse back the raw bridging path.
type (
	ExecutorHTTPRequest  = pluginapi.ExecutorHTTPRequest
	ExecutorHTTPResponse = pluginapi.ExecutorHTTPResponse
)

// ---- quota ----

// Quota request and response shapes.
type (
	QuotaDescribeResponse = pluginapi.QuotaDescribeResponse
	QuotaDescribeRequest  = pluginapi.QuotaDescribeRequest
	QuotaSubscription     = pluginapi.QuotaSubscription
	QuotaGroup            = pluginapi.QuotaGroup
	QuotaBucket           = pluginapi.QuotaBucket
	QuotaMetric           = pluginapi.QuotaMetric
	QuotaFetchRequest     = pluginapi.QuotaFetchRequest
	QuotaFetchResponse    = pluginapi.QuotaFetchResponse
	QuotaResetRequest     = pluginapi.QuotaResetRequest
	QuotaResetResponse    = pluginapi.QuotaResetResponse
)

// ---- management ----

// Management request and response shapes.
type (
	ManagementRegistrationRequest  = pluginapi.ManagementRegistrationRequest
	ManagementRegistrationResponse = pluginapi.ManagementRegistrationResponse
	ManagementRequest              = pluginapi.ManagementRequest
	ManagementResponse             = pluginapi.ManagementResponse
	ManagementRoute                = pluginapi.ManagementRoute
	ResourceRoute                  = pluginapi.ResourceRoute
)
