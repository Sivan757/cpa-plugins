package pluginkit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// ExecutorRequest mirrors pluginapi.ExecutorRequest (no JSON tags on the host
// struct, so the wire keys are the exported Go field names).
type ExecutorRequest struct {
	AuthID          string
	AuthProvider    string
	Model           string
	Format          string
	Stream          bool
	Alt             string
	Headers         http.Header
	Query           url.Values
	OriginalRequest []byte
	SourceFormat    string
	Payload         []byte
	Metadata        map[string]any
	StorageJSON     []byte
	AuthMetadata    map[string]any
	AuthAttributes  map[string]string
}

// ExecutorResponse mirrors pluginapi.ExecutorResponse.
type ExecutorResponse struct {
	Payload  []byte
	Headers  http.Header
	Metadata map[string]any
}

// ExecutorStreamChunk mirrors pluginapi.ExecutorStreamChunk.
type ExecutorStreamChunk struct {
	Payload []byte
	Err     error `json:"-"`
}

// ExecutorStreamResponse mirrors pluginapi.ExecutorStreamResponse. Chunks is
// buffered by the RPC bridge, so a plugin may return a fully materialised slice;
// the host replays it downstream as a stream.
type ExecutorStreamResponse struct {
	Headers http.Header
	Chunks  []ExecutorStreamChunk
}

// ExecutorHTTPRequest mirrors pluginapi.ExecutorHTTPRequest.
type ExecutorHTTPRequest struct {
	AuthID       string
	AuthProvider string
	Method       string
	URL          string
	Headers      http.Header
	Body         []byte
	StorageJSON  []byte
	Metadata     map[string]any
	Attributes   map[string]string
}

// ExecutorHTTPResponse mirrors pluginapi.ExecutorHTTPResponse.
type ExecutorHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Executor performs model execution for one provider.
type Executor interface {
	// ExecutorIdentifier is the provider key this executor serves. It must match
	// the provider key on the auth records the host routes here.
	ExecutorIdentifier() string
	// Execute handles a non-streaming model call.
	Execute(ctx context.Context, req ExecutorRequest) (ExecutorResponse, error)
	// ExecuteStream handles a streaming model call, returning buffered chunks.
	ExecuteStream(ctx context.Context, req ExecutorRequest) (ExecutorStreamResponse, error)
	// CountTokens optionally counts tokens; returning ErrUnhandled is fine.
	CountTokens(ctx context.Context, req ExecutorRequest) (ExecutorResponse, error)
	// HttpRequest optionally exposes raw HTTP bridging.
	HttpRequest(ctx context.Context, req ExecutorHTTPRequest) (ExecutorHTTPResponse, error)
}

// ModelRouteRequest mirrors pluginapi.ModelRouteRequest.
type ModelRouteRequest struct {
	Plugin             Metadata
	PluginID           string
	SourceFormat       string
	RequestedModel     string
	Stream             bool
	Headers            http.Header
	Query              url.Values
	Body               []byte
	Metadata           map[string]any
	AvailableProviders []string
}

// Model route target kinds.
const (
	RouteTargetSelf     = "self"
	RouteTargetExecutor = "executor"
	RouteTargetProvider = "provider"
)

// ModelRouteResponse mirrors pluginapi.ModelRouteResponse.
type ModelRouteResponse struct {
	Handled     bool
	TargetKind  string
	Target      string
	TargetModel string
	Reason      string
}

// ModelRouter decides whether this plugin should serve a model request.
type ModelRouter interface {
	RouteModel(ctx context.Context, req ModelRouteRequest) (ModelRouteResponse, error)
}

// ExecutorStreamChunkJSON is the wire shape of one buffered stream chunk.
type ExecutorStreamChunkJSON struct {
	Payload []byte
}

// RawJSON is a convenience for executors that already have an upstream body.
func RawJSON(v any) []byte {
	encoded, _ := json.Marshal(v)
	return encoded
}
