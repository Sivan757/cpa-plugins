package cpakit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Host call deadlines. A host callback runs inside the host process, so an
// unbounded wait would pin a plugin goroutine for as long as the host takes;
// the deadline turns that into a classified failure the request path can act on.
var (
	callTimeout          = 30 * time.Second
	emitTimeout          = 30 * time.Second
	streamTimeoutGrace   = 3 * time.Second
	shutdownDrainTimeout = 15 * time.Second
)

// HostBridge routes outbound traffic through the host callbacks.
//
// Callers must never place credentials, auth headers, prompts, or tool
// arguments in Log messages or fields.
type HostBridge struct {
	call RawCaller
	// inFlight counts host-callback invocations whose goroutines may still run,
	// including callbacks orphaned past their deadline and the abandon cleanup
	// spawned for them. The shutdown path waits on it because the Unix loader
	// frees the host API and unloads the library as soon as the shutdown export
	// returns.
	inFlight sync.WaitGroup
}

// NewHostBridge wraps the injected raw host caller.
func NewHostBridge(call RawCaller) *HostBridge { return &HostBridge{call: call} }

// httpDoRequest is the wire shape host.http.do accepts.
//
// Field keys are matched case-insensitively by the host decoder, so the Go
// field names are correct as written; the two envelope-level keys below carry
// explicit tags because the host tags them snake_case.
type httpDoRequest struct {
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	Method         string              `json:"method,omitempty"`
	URL            string              `json:"url,omitempty"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
}

// httpDoResponse mirrors pluginapi.HTTPResponse, which carries no JSON tags.
type httpDoResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

// httpDoStreamResponse mirrors the host's rpcHostHTTPStreamResponse.
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

// hostLogRequest mirrors the host's rpcHostLogRequest.
type hostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

// invoke performs one host callback and decodes its result into out.
func (b *HostBridge) invoke(ctx context.Context, method string, payload any, out any) error {
	if b == nil || b.call == nil {
		return fmt.Errorf("host bridge is not installed")
	}
	raw, errCall := b.callWithTimeout(ctx, method, payload)
	if errCall != nil {
		return errCall
	}
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return fmt.Errorf("%s: undecodable host response", method)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("%s: %s", method, Redact(env.Error.Message))
		}
		return fmt.Errorf("%s: host reported failure", method)
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
		return fmt.Errorf("%s: undecodable host result", method)
	}
	return nil
}

// callWithTimeout runs one host callback under the attempt deadline.
func (b *HostBridge) callWithTimeout(ctx context.Context, method string, payload any) ([]byte, error) {
	raw, _, err := b.callWithTimeoutResult(ctx, method, payload, 0, nil)
	return raw, err
}

// callWithTimeoutResult runs one host callback with an optional bounded grace
// window after the deadline and an optional abandon hook for the orphaned
// result.
//
// Every invocation holds one inFlight count from entry until the callback
// goroutine has exited AND its post-work is settled, so shutdown never outruns
// a late cleanup.
func (b *HostBridge) callWithTimeoutResult(ctx context.Context, method string, payload any, grace time.Duration, abandon func(raw []byte)) ([]byte, bool, error) {
	if b == nil || b.call == nil {
		return nil, false, fmt.Errorf("host bridge is not installed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, false, fmt.Errorf("%s: encode request", method)
	}

	type callResult struct {
		raw []byte
		err error
	}
	done := make(chan callResult, 1)
	b.inFlight.Add(1)
	var (
		stateMu  sync.Mutex
		exited   bool
		drained  bool
		released bool
	)
	release := func() {
		if exited && drained && !released {
			released = true
			b.inFlight.Done()
		}
	}
	settle := func() {
		stateMu.Lock()
		drained = true
		release()
		stateMu.Unlock()
	}

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- callResult{err: fmt.Errorf("%s: host callback panicked", method)}
			}
			stateMu.Lock()
			exited = true
			release()
			stateMu.Unlock()
		}()
		raw, errCall := b.call(method, encoded)
		done <- callResult{raw: raw, err: errCall}
	}()

	select {
	case res := <-done:
		settle()
		return res.raw, false, res.err
	case <-ctx.Done():
		if grace > 0 {
			timer := time.NewTimer(grace)
			defer timer.Stop()
			select {
			case res := <-done:
				settle()
				return res.raw, true, res.err
			case <-timer.C:
			}
		}
		if abandon != nil {
			go func() {
				res := <-done
				abandon(res.raw)
				settle()
			}()
		} else {
			go func() {
				<-done
				settle()
			}()
		}
		return nil, false, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// WaitForInFlight blocks until no host callback is pending, or the timeout
// lapses. It reports whether the drain completed.
func (b *HostBridge) WaitForInFlight(timeout time.Duration) bool {
	if b == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		b.inFlight.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// Do performs a buffered upstream request through the host, so the host applies
// its proxy policy and captures its request log.
func (b *HostBridge) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	payload := httpDoRequest{
		HostCallbackID: HostCallbackID(),
		Method:         req.Method,
		URL:            req.URL,
		Headers:        map[string][]string(req.Headers),
		Body:           req.Body,
	}
	var resp httpDoResponse
	if errInvoke := b.invoke(ctx, pluginabi.MethodHostHTTPDo, payload, &resp); errInvoke != nil {
		return pluginapi.HTTPResponse{}, errInvoke
	}
	return pluginapi.HTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    http.Header(resp.Headers),
		Body:       resp.Body,
	}, nil
}

// DoStream opens an upstream request as a host-side stream and returns its
// status, headers, and stream id. A non-empty chunks slice means the host
// already answered with a complete body, which happens for a non-streaming
// error page.
func (b *HostBridge) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (int, http.Header, string, []byte, error) {
	payload := httpDoRequest{
		HostCallbackID: HostCallbackID(),
		Method:         req.Method,
		URL:            req.URL,
		Headers:        map[string][]string(req.Headers),
		Body:           req.Body,
	}
	var resp httpDoStreamResponse
	// A stream open that lands just past the deadline is salvaged: the host
	// registers the upstream stream before it surfaces the status, so a late
	// result must be closed rather than leaked.
	abandon := func(raw []byte) {
		var late httpDoStreamResponse
		if len(raw) > 0 && json.Unmarshal(raw, &late) == nil && late.StreamID != "" {
			_ = b.StreamClose(late.StreamID)
		}
	}
	raw, late, errInvoke := b.callWithTimeoutResult(ctx, pluginabi.MethodHostHTTPDoStream, payload, streamTimeoutGrace, abandon)
	if errInvoke != nil {
		return 0, nil, "", nil, errInvoke
	}
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || !env.OK {
		return 0, nil, "", nil, fmt.Errorf("host.http.do_stream: undecodable host response")
	}
	if len(env.Result) > 0 {
		if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
			return 0, nil, "", nil, fmt.Errorf("host.http.do_stream: undecodable host result")
		}
	}
	_ = late
	var buffered []byte
	for _, chunk := range resp.Chunks {
		buffered = append(buffered, chunk.Payload...)
	}
	return resp.StatusCode, http.Header(resp.Headers), resp.StreamID, buffered, nil
}

// StreamRead reads the next upstream chunk. done reports end of stream.
func (b *HostBridge) StreamRead(streamID string) (payload []byte, errMsg string, done bool, err error) {
	var resp httpStreamReadResponse
	if errInvoke := b.invoke(context.Background(), pluginabi.MethodHostHTTPStreamRead,
		map[string]string{"stream_id": streamID}, &resp); errInvoke != nil {
		return nil, "", true, errInvoke
	}
	return resp.Payload, resp.Error, resp.Done, nil
}

// StreamClose releases an upstream host stream.
func (b *HostBridge) StreamClose(streamID string) error {
	if streamID == "" {
		return nil
	}
	return b.invoke(context.Background(), pluginabi.MethodHostHTTPStreamClose,
		map[string]string{"stream_id": streamID}, nil)
}

// StreamEmit sends one converted event to the downstream client stream, which
// the host identifies by the executor request's stream_id.
func (b *HostBridge) StreamEmit(downStreamID string, payload []byte) error {
	if downStreamID == "" {
		return fmt.Errorf("downstream stream id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), emitTimeout)
	defer cancel()
	return b.invoke(ctx, pluginabi.MethodHostStreamEmit,
		map[string]any{"stream_id": downStreamID, "payload": payload}, nil)
}

// StreamCloseDownstream ends the downstream stream, reporting errMsg as the
// terminating error when it is non-empty.
func (b *HostBridge) StreamCloseDownstream(downStreamID, errMsg string) error {
	if downStreamID == "" {
		return nil
	}
	return b.invoke(context.Background(), pluginabi.MethodHostStreamClose,
		map[string]string{"stream_id": downStreamID, "error": errMsg}, nil)
}

// Log writes one diagnostic line through the host logger.
func (b *HostBridge) Log(level, message string, fields map[string]any) error {
	return b.invoke(context.Background(), pluginabi.MethodHostLog, hostLogRequest{
		HostCallbackID: HostCallbackID(),
		Level:          level,
		Message:        message,
		Fields:         fields,
	}, nil)
}

// AuthList returns the auth records the host currently knows, without modifying
// them.
func (b *HostBridge) AuthList(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if errInvoke := b.invoke(ctx, pluginabi.MethodHostAuthList, map[string]any{}, &resp); errInvoke != nil {
		return nil, errInvoke
	}
	return resp.Files, nil
}

// AuthSave persists one plugin-owned auth record through the host. The host
// watches its auth directory, so the record becomes routable without a restart.
func (b *HostBridge) AuthSave(ctx context.Context, req pluginapi.HostAuthSaveRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("host auth save: name is required")
	}
	if len(req.JSON) == 0 {
		return fmt.Errorf("host auth save: credential payload is required")
	}
	return b.invoke(ctx, pluginabi.MethodHostAuthSave, req, nil)
}
