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

// BufferedStream builds a streaming executor response from an already-collected
// body. The v8 stream response carries a channel, so the chunks are replayed
// from a goroutine that closes the channel when it finishes.
func BufferedStream(headers http.Header, chunks [][]byte) pluginapi.ExecutorStreamResponse {
	out := make(chan pluginapi.ExecutorStreamChunk, len(chunks))
	for _, payload := range chunks {
		out <- pluginapi.ExecutorStreamChunk{Payload: payload}
	}
	close(out)
	return pluginapi.ExecutorStreamResponse{Headers: headers, Chunks: out}
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
// request plus the downstream stream id.
func DecodeExecutorRequest(request json.RawMessage) (pluginapi.ExecutorRequest, string, error) {
	envelope, errDecode := Decode[ExecutorEnvelope](request)
	if errDecode != nil {
		return pluginapi.ExecutorRequest{}, "", errDecode
	}
	return envelope.ExecutorRequest, envelope.StreamID, nil
}
