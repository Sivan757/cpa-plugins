package cpakit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// HTTPResult is a fully buffered upstream response.
type HTTPResult struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// HTTPDo performs an upstream request through the host, so the host applies its
// proxy policy and captures its request log.
func HTTPDo(ctx context.Context, method, url string, headers http.Header, body []byte) (*HTTPResult, error) {
	bridge := Bridge()
	if bridge == nil {
		return nil, fmt.Errorf("host bridge is not installed")
	}
	response, errDo := bridge.Do(ctx, pluginapi.HTTPRequest{
		Method:  method,
		URL:     url,
		Headers: headers,
		Body:    body,
	})
	if errDo != nil {
		return nil, errDo
	}
	return &HTTPResult{StatusCode: response.StatusCode, Header: response.Headers, Body: response.Body}, nil
}

// HTTPJSON performs a JSON request and decodes the response into out.
func HTTPJSON(ctx context.Context, method, url string, headers http.Header, requestBody any, out any) (*HTTPResult, error) {
	var payload []byte
	if requestBody != nil {
		encoded, errMarshal := json.Marshal(requestBody)
		if errMarshal != nil {
			return nil, fmt.Errorf("marshal request body: %w", errMarshal)
		}
		payload = encoded
	}
	if headers == nil {
		headers = http.Header{}
	}
	if headers.Get("Content-Type") == "" && len(payload) > 0 {
		headers.Set("Content-Type", "application/json")
	}
	result, errDo := HTTPDo(ctx, method, url, headers, payload)
	if errDo != nil {
		return nil, errDo
	}
	if out != nil && len(result.Body) > 0 {
		if errUnmarshal := json.Unmarshal(result.Body, out); errUnmarshal != nil {
			return result, fmt.Errorf("decode %s: %w (body: %s)", url, errUnmarshal, TruncateBody(result.Body, 400))
		}
	}
	return result, nil
}

// HostCallback is the host callback identity of one in-flight executor call.
//
// Host calls made while handling that call use the ambient callback id, which is
// only correct on the request's own goroutine. A streaming executor returns
// before it has finished forwarding, so its forward goroutine runs after that
// ambient value was overwritten by the next RPC. Carrying the id in a value pins
// those later calls to the right request.
type HostCallback struct {
	id     string
	bridge *HostBridge
}

// NewHostCallback captures the callback id a request arrived with.
func NewHostCallback(id string) HostCallback {
	return HostCallback{id: id, bridge: Bridge()}
}

// Valid reports whether this handle can address host callbacks.
func (c HostCallback) Valid() bool { return c.bridge != nil }

func (c HostCallback) callbackID() string {
	if c.id != "" {
		return c.id
	}
	return HostCallbackID()
}

// HTTPStream is a live upstream host stream whose body is read incrementally.
type HTTPStream struct {
	// StatusCode and Header describe the response head. They are valid as soon
	// as the stream opens, before any body byte is read.
	StatusCode int
	Header     http.Header

	bridge   *HostBridge
	streamID string
	queued   [][]byte
	closed   sync.Once
}

// Read returns the next body fragment. The boolean reports end of stream.
func (s *HTTPStream) Read() ([]byte, bool, error) {
	if s == nil || s.bridge == nil {
		return nil, true, nil
	}
	if len(s.queued) > 0 {
		next := s.queued[0]
		s.queued = s.queued[1:]
		return next, false, nil
	}
	if s.streamID == "" {
		return nil, true, nil
	}
	payload, errMsg, done, errRead := s.bridge.StreamRead(s.streamID)
	if errRead != nil {
		return nil, true, errRead
	}
	if errMsg != "" {
		return nil, true, fmt.Errorf("%s", errMsg)
	}
	return payload, done, nil
}

// Close releases the upstream stream. It is safe to call more than once.
func (s *HTTPStream) Close() {
	if s == nil || s.bridge == nil {
		return
	}
	s.closed.Do(func() { _ = s.bridge.StreamClose(s.streamID) })
}

// OpenHTTPStream opens an upstream request as a host stream. A non-2xx head is
// returned with its body already buffered, because a finite error page has to be
// classified before the caller decides to retry.
func (c HostCallback) OpenHTTPStream(ctx context.Context, method, url string, headers http.Header, body []byte) (*HTTPStream, error) {
	bridge := c.bridge
	if bridge == nil {
		bridge = Bridge()
	}
	if bridge == nil {
		return nil, fmt.Errorf("host bridge is not installed")
	}
	status, header, streamID, buffered, errOpen := bridge.DoStream(ctx, pluginapi.HTTPRequest{
		Method:  method,
		URL:     url,
		Headers: headers,
		Body:    body,
	})
	if errOpen != nil {
		return nil, errOpen
	}
	stream := &HTTPStream{StatusCode: status, Header: header, bridge: bridge, streamID: streamID}
	if len(buffered) > 0 {
		stream.queued = [][]byte{buffered}
	}
	return stream, nil
}

// OpenHTTPStreamAmbient opens a stream through the ambient callback id.
func OpenHTTPStreamAmbient(ctx context.Context, method, url string, headers http.Header, body []byte) (*HTTPStream, error) {
	return NewHostCallback("").OpenHTTPStream(ctx, method, url, headers, body)
}

// Emit sends one chunk to the downstream host stream identified by streamID,
// which the host passes to an async executor on the request.
func (c HostCallback) Emit(streamID string, payload []byte) error {
	bridge := c.bridge
	if bridge == nil {
		bridge = Bridge()
	}
	if bridge == nil {
		return fmt.Errorf("host bridge is not installed")
	}
	return bridge.StreamEmit(streamID, payload)
}

// CloseStream ends the downstream host stream, reporting errMsg as the
// terminating error when it is non-empty.
func (c HostCallback) CloseStream(streamID, errMsg string) {
	bridge := c.bridge
	if bridge == nil {
		bridge = Bridge()
	}
	if bridge == nil {
		return
	}
	_ = bridge.StreamCloseDownstream(streamID, errMsg)
}

// ForwardStream reads an upstream stream and emits each complete SSE event
// downstream as it arrives, preserving the upstream's timing.
func ForwardStream(callback HostCallback, downStreamID string, stream *HTTPStream) error {
	if stream == nil {
		return fmt.Errorf("upstream stream is required")
	}
	forwarder := &SSEForwarder{}
	emitted := false
	for {
		chunk, done, errRead := stream.Read()
		if len(chunk) > 0 {
			for _, payload := range forwarder.Feed(chunk) {
				if errEmit := callback.Emit(downStreamID, payload); errEmit != nil {
					return errEmit
				}
				emitted = true
			}
		}
		if errRead != nil {
			return errRead
		}
		if done {
			break
		}
	}
	for _, payload := range forwarder.Flush() {
		if errEmit := callback.Emit(downStreamID, payload); errEmit != nil {
			return errEmit
		}
		emitted = true
	}
	if !emitted {
		return fmt.Errorf("upstream stream produced no events")
	}
	return nil
}

// ForwardStreamTransform forwards an upstream stream, passing every complete SSE
// payload through transform, which returns the payloads to emit for that event.
//
// A transform returning nothing drops the event. That is how a protocol
// translation discards the upstream events the downstream protocol has no
// equivalent for. Returned payloads are emitted as given, so a transform must
// produce the downstream shape itself.
func ForwardStreamTransform(callback HostCallback, downStreamID string, stream *HTTPStream, transform func([]byte) [][]byte) error {
	if stream == nil {
		return fmt.Errorf("upstream stream is required")
	}
	forwarder := &SSEForwarder{}
	emitted := false
	for {
		chunk, done, errRead := stream.Read()
		if len(chunk) > 0 {
			for _, payload := range forwarder.Feed(chunk) {
				for _, translated := range transform(payload) {
					if errEmit := callback.Emit(downStreamID, translated); errEmit != nil {
						return errEmit
					}
					emitted = true
				}
			}
		}
		if errRead != nil {
			return errRead
		}
		if done {
			break
		}
	}
	for _, payload := range forwarder.Flush() {
		for _, translated := range transform(payload) {
			if errEmit := callback.Emit(downStreamID, translated); errEmit != nil {
				return errEmit
			}
			emitted = true
		}
	}
	if !emitted {
		return fmt.Errorf("upstream stream produced no events")
	}
	return nil
}

// ReadStreamBody drains a stream into memory, for the finite bodies that carry
// an error or a non-streaming answer.
func ReadStreamBody(stream *HTTPStream, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = 1 << 20
	}
	var out bytes.Buffer
	for {
		chunk, done, errRead := stream.Read()
		if len(chunk) > 0 {
			if out.Len()+len(chunk) > limit {
				out.Write(chunk[:max(0, limit-out.Len())])
				return out.Bytes(), nil
			}
			out.Write(chunk)
		}
		if errRead != nil {
			if out.Len() > 0 {
				return out.Bytes(), nil
			}
			return nil, errRead
		}
		if done {
			return out.Bytes(), nil
		}
	}
}
