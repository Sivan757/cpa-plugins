package pluginkit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// StreamIdleTimeout bounds how long a stream may go without producing a byte.
// It catches an upstream that has stopped responding without capping a
// legitimately long answer, which a whole-request deadline would.
const StreamIdleTimeout = 120 * time.Second

// HTTPStream is a live upstream response whose body is read incrementally.
//
// The buffered client (HTTPDo) collects the entire body before the plugin sees
// any of it, which erases the upstream's timing: a caller measuring decode speed
// against arrival times sees every chunk land in one burst. This reader keeps
// the host's proxy policy and request logging while preserving chunk timing, so
// a streaming executor can forward each chunk as it arrives.
type HTTPStream struct {
	// StatusCode and Header describe the response head. They are valid as soon
	// as the stream opens, before any body byte is read.
	StatusCode int
	Header     http.Header

	read   func() ([]byte, bool, error)
	close  func()
	closed sync.Once
}

// Read returns the next body fragment. The boolean reports end of stream, which
// arrives with a nil payload once the upstream body is exhausted.
//
// Read blocks until a fragment is available, so a caller loop paces itself
// against the upstream instead of spinning.
func (s *HTTPStream) Read() ([]byte, bool, error) {
	if s == nil || s.read == nil {
		return nil, true, nil
	}
	return s.read()
}

// Close releases the stream. It is safe to call more than once, and callers
// should always defer it so an abandoned stream does not leak the host-side
// reader goroutine.
func (s *HTTPStream) Close() {
	if s == nil || s.close == nil {
		return
	}
	s.closed.Do(s.close)
}

// HostCallback is the host callback identity of one in-flight executor call.
//
// Host calls made while handling that call use the process-global callback id,
// which is correct only on the request's own goroutine. A streaming executor
// returns before it has finished forwarding, so its forwarding goroutine runs
// after that global has been overwritten by the next RPC. Carrying the id in a
// value keeps those later calls pinned to the right request.
type HostCallback struct {
	id string
}

// NewHostCallback captures the callback id a request arrived with. An empty id
// yields a handle that falls back to the local HTTP client.
func NewHostCallback(id string) HostCallback { return HostCallback{id: id} }

// Valid reports whether this handle can address host callbacks.
func (c HostCallback) Valid() bool { return c.id != "" }

// callbackID returns the id to send, preferring this handle and falling back to
// the ambient one so a caller that never captured a handle still works on the
// request goroutine.
func (c HostCallback) callbackID() string {
	if c.id != "" {
		return c.id
	}
	return HostCallbackID()
}

// httpDoStreamResponse mirrors the host's rpcHostHTTPStreamResponse.
//
// Unlike the buffered httpDoResponse — whose wire keys come from pluginapi's
// untagged field names — this envelope is tagged snake_case by the host. The
// nested chunk keeps Go names because pluginapi.HTTPStreamChunk is untagged.
type httpDoStreamResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
	Chunks     []struct {
		Payload []byte `json:"Payload,omitempty"`
	} `json:"chunks,omitempty"`
}

// httpStreamReadResponse mirrors the host's rpcHostHTTPStreamReadResponse.
type httpStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// OpenHTTPStream performs an upstream HTTP request and returns a reader over
// its body, using this handle's request context for host-side proxy policy.
//
// Callers must Close the returned stream.
func (c HostCallback) OpenHTTPStream(ctx context.Context, method, url string, headers http.Header, body []byte) (*HTTPStream, error) {
	if HostAvailable() {
		req := httpDoRequest{
			HostCallbackID: c.callbackID(),
			Method:         method,
			URL:            url,
			Headers:        map[string][]string(headers),
			Body:           body,
		}
		var resp httpDoStreamResponse
		if errCall := callHost(MethodHostHTTPDoStream, req, &resp); errCall == nil {
			header := http.Header(resp.Headers)
			// An upstream that answered with a complete body — including a
			// non-streaming error page — arrives pre-loaded rather than live.
			if len(resp.Chunks) > 0 {
				queued := make([][]byte, 0, len(resp.Chunks))
				for _, chunk := range resp.Chunks {
					queued = append(queued, chunk.Payload)
				}
				var mu sync.Mutex
				return &HTTPStream{
					StatusCode: resp.StatusCode,
					Header:     header,
					read: func() ([]byte, bool, error) {
						mu.Lock()
						defer mu.Unlock()
						if len(queued) == 0 {
							return nil, true, nil
						}
						next := queued[0]
						queued = queued[1:]
						return next, false, nil
					},
					close: func() {},
				}, nil
			}
			if resp.StreamID == "" {
				return nil, fmt.Errorf("host http stream: empty stream_id for %s", url)
			}
			streamID := resp.StreamID
			var mu sync.Mutex
			return &HTTPStream{
				StatusCode: resp.StatusCode,
				Header:     header,
				read: func() ([]byte, bool, error) {
					mu.Lock()
					defer mu.Unlock()
					var chunk httpStreamReadResponse
					if errRead := callHost(MethodHostHTTPStreamRead, map[string]string{"stream_id": streamID}, &chunk); errRead != nil {
						return nil, true, errRead
					}
					if chunk.Error != "" {
						return nil, true, fmt.Errorf("%s", chunk.Error)
					}
					if chunk.Done {
						return chunk.Payload, true, nil
					}
					return chunk.Payload, false, nil
				},
				close: func() {
					_ = callHost(MethodHostHTTPStreamClose, map[string]string{"stream_id": streamID}, nil)
				},
			}, nil
		}
		// Fall through to the local client so a host bridge change cannot take
		// quota and catalog reads down with it.
	}
	return directHTTPStream(ctx, method, url, headers, body)
}

// OpenHTTPStreamAmbient opens a stream through the ambient host callback, for
// callers that run on the request goroutine and hold no explicit handle.
func OpenHTTPStreamAmbient(ctx context.Context, method, url string, headers http.Header, body []byte) (*HTTPStream, error) {
	return NewHostCallback("").OpenHTTPStream(ctx, method, url, headers, body)
}

func directHTTPStream(ctx context.Context, method, url string, headers http.Header, body []byte) (*HTTPStream, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, url, reader)
	if errRequest != nil {
		return nil, fmt.Errorf("build request: %w", errRequest)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// No whole-request deadline: a streaming answer may legitimately outlive any
	// fixed budget, and cancelling is the caller's context to decide.
	client := &http.Client{}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("http %s %s: %w", method, url, errDo)
	}
	buf := make([]byte, 32*1024)
	type readResult struct {
		data []byte
		err  error
	}
	results := make(chan readResult, 1)
	go func() {
		for {
			n, errRead := resp.Body.Read(buf)
			if n > 0 {
				out := make([]byte, n)
				copy(out, buf[:n])
				select {
				case results <- readResult{data: out}:
				case <-ctx.Done():
					return
				}
			}
			if errRead != nil {
				select {
				case results <- readResult{err: errRead}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	return &HTTPStream{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		read: func() ([]byte, bool, error) {
			// An idle deadline bounds a wedged upstream without capping a
			// legitimately long answer, which a whole-request budget would.
			timer := time.NewTimer(StreamIdleTimeout)
			defer timer.Stop()
			select {
			case res := <-results:
				if res.err == io.EOF {
					return nil, true, nil
				}
				if res.err != nil {
					return nil, true, res.err
				}
				return res.data, false, nil
			case <-timer.C:
				_ = resp.Body.Close()
				return nil, true, fmt.Errorf("upstream stream idle for %s", StreamIdleTimeout)
			case <-ctx.Done():
				_ = resp.Body.Close()
				return nil, true, ctx.Err()
			}
		},
		close: func() { _ = resp.Body.Close() },
	}, nil
}

// Emit sends one chunk to the host stream identified by streamID, which the
// host passes to an async executor through ExecutorRequest.StreamID.
func (c HostCallback) Emit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	return callHost(MethodHostStreamEmit, map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	}, nil)
}

// CloseStream ends the host stream, reporting errMsg as the terminating error
// when it is non-empty.
func (c HostCallback) CloseStream(streamID, errMsg string) {
	if streamID == "" {
		return
	}
	_ = callHost(MethodHostStreamClose, map[string]string{
		"stream_id": streamID,
		"error":     errMsg,
	}, nil)
}
