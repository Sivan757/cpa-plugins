package cpakit

import (
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ExecutorEnvelope is the host's rpcExecutorRequest: the official
// ExecutorRequest plus the two envelope fields the host adds, which are tagged
// snake_case because every embedded field keeps its Go name.
//
// StreamID is the DOWNSTREAM host-allocated id for executor.execute_stream
// emissions. Ids returned by the stream-read callbacks are UPSTREAM and are
// never interchangeable with it.
type ExecutorEnvelope struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
	// HostCallbackID scopes host callbacks to this request's executor call.
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// ExecutorStreamResponse is the ABI shape of a streaming executor result: the
// same fields the host's rpcExecutorStreamResponse decodes.
//
// It is a slice, not the SDK's channel-carrying type, because this value has to
// survive JSON encoding at the RPC boundary. The host replays the buffer
// downstream.
type ExecutorStreamResponse struct {
	Headers http.Header
	Chunks  []ExecutorStreamChunk
}

// BufferedStream builds a streaming executor response from an already-collected
// body.
func BufferedStream(headers http.Header, chunks [][]byte) ExecutorStreamResponse {
	out := make([]ExecutorStreamChunk, 0, len(chunks))
	for _, payload := range chunks {
		out = append(out, ExecutorStreamChunk{Payload: payload})
	}
	return ExecutorStreamResponse{Headers: headers, Chunks: out}
}

// StreamHeaders is the header set a streamed reply carries.
func StreamHeaders() http.Header {
	return http.Header{
		"Content-Type":  []string{"text/event-stream"},
		"Cache-Control": []string{"no-cache"},
	}
}

// SSEFromStrings converts bare JSON payloads into the chunk slice
// BufferedStream consumes.
func SSEFromStrings(payloads []string) [][]byte {
	out := make([][]byte, 0, len(payloads))
	for _, payload := range payloads {
		out = append(out, []byte(payload))
	}
	return out
}

// DecodeExecutorRequest decodes an executor RPC payload and returns the official
// request, the downstream stream id, and the host callback id that pins later
// host calls to this request.
func DecodeExecutorRequest(request json.RawMessage) (pluginapi.ExecutorRequest, string, string, error) {
	envelope, errDecode := Decode[ExecutorEnvelope](request)
	if errDecode != nil {
		return pluginapi.ExecutorRequest{}, "", "", errDecode
	}
	return envelope.ExecutorRequest, envelope.StreamID, envelope.HostCallbackID, nil
}
