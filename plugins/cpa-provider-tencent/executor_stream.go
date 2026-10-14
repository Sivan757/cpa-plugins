package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// errBodyLimit caps how much of a non-2xx upstream body is read before the
// failure is classified. Error pages are small; a mislabelled stream is not
// worth buffering without bound.
const errBodyLimit = 1 << 20

// openChatStream opens the upstream chat response as a live reader, rotating
// across credential candidates exactly as callChat does.
//
// The distinction that matters: a non-2xx status is fully known once the
// response head arrives, so it is read and classified here rather than inside
// the forwarding goroutine. That keeps credential rotation and the host's
// request-scoped error rules (including the 400 fallback that lands a
// workbuddy-ai rejection on the CN credential) working unchanged.
//
// A 2xx response is returned still streaming. The caller owns it and must Close
// it.
func (p *pluginState) openChatStream(ctx context.Context, callback pluginkit.HostCallback, kind *credentialKind, creds *credentials, extra []*credentials, body []byte) (*pluginkit.HTTPStream, error) {
	candidates := append([]*credentials{creds}, extra...)
	ordered := p.rotator.ordered(dedupeCredentials(candidates))
	endpoint := creds.chatURL(kind)

	for index, cred := range ordered {
		last := index == len(ordered)-1
		stream, errOpen := callback.OpenHTTPStream(ctx, http.MethodPost, endpoint, p.chatHeadersFor(kind, cred), body)
		if errOpen != nil {
			if last {
				return nil, pluginkit.NewError("upstream_unavailable", "chat request failed: "+errOpen.Error(), http.StatusBadGateway)
			}
			p.rotator.penalize(cred)
			pluginkit.HostLog(ctx, "warn", "tencent: stream candidate failed at the network layer; trying the next key")
			continue
		}
		if stream.StatusCode >= 200 && stream.StatusCode < 300 {
			return stream, nil
		}

		// A non-2xx head is a complete, finite answer: drain it so the failure
		// can be classified and, when it is a credential fault, retried.
		raw, errRead := readStreamBody(stream)
		status := stream.StatusCode
		stream.Close()
		if errRead != nil {
			return nil, pluginkit.NewError("upstream_unavailable", "chat request failed: "+errRead.Error(), http.StatusBadGateway)
		}
		if !last && shouldRotateCredential(status) {
			p.rotator.penalize(cred)
			pluginkit.HostLog(ctx, "warn", "tencent: stream candidate rejected; trying the next key")
			continue
		}
		return nil, classifyUpstreamError(status, raw)
	}
	return nil, pluginkit.NewError("upstream_unavailable", "every credential candidate failed", http.StatusBadGateway)
}

// readStreamBody drains a stream into memory, for the finite bodies that carry
// an error or a non-streaming answer.
func readStreamBody(stream *pluginkit.HTTPStream) ([]byte, error) {
	var out bytes.Buffer
	for {
		chunk, done, errRead := stream.Read()
		if len(chunk) > 0 {
			if out.Len()+len(chunk) > errBodyLimit {
				out.Write(chunk[:max(0, errBodyLimit-out.Len())])
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

// sseForwarder turns an arbitrary byte stream into the bare JSON payloads the
// host frames.
//
// Upstream chunks split wherever TCP delivers them, so an event routinely
// straddles two reads. Buffering until a blank-line terminator is what keeps a
// partial event from being emitted as a truncated — and therefore invalid —
// JSON chunk.
type sseForwarder struct {
	pending []byte
}

// feed appends a read and returns every complete event payload it now holds.
func (f *sseForwarder) feed(chunk []byte) [][]byte {
	f.pending = append(f.pending, chunk...)
	var payloads [][]byte
	for {
		index, width := findEventBoundary(f.pending)
		if index < 0 {
			return payloads
		}
		event := f.pending[:index]
		f.pending = f.pending[index+width:]
		if payload := sseEventPayload(event); payload != nil {
			payloads = append(payloads, payload)
		}
	}
}

// flush returns a trailing event that arrived without its terminating blank
// line, which happens when an upstream closes the stream immediately after the
// final event.
func (f *sseForwarder) flush() [][]byte {
	event := f.pending
	f.pending = nil
	if payload := sseEventPayload(event); payload != nil {
		return [][]byte{payload}
	}
	return nil
}

// findEventBoundary locates the end of the next complete SSE event and reports
// the separator width, so both LF and CRLF dialects advance correctly.
func findEventBoundary(buf []byte) (int, int) {
	lf := bytes.Index(buf, []byte("\n\n"))
	crlf := bytes.Index(buf, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case crlf >= 0 && (lf < 0 || crlf < lf):
		return crlf, 4
	default:
		return lf, 2
	}
}

// sseEventPayload extracts the data payload of one event, or nil when the event
// carries none.
//
// [DONE] is dropped: the host appends its own terminator, and forwarding the
// upstream's would end the client's stream twice.
func sseEventPayload(event []byte) []byte {
	var parts []string
	for _, line := range strings.Split(strings.ReplaceAll(string(event), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		parts = append(parts, payload)
	}
	if len(parts) == 0 {
		return nil
	}
	return []byte(strings.Join(parts, "\n"))
}

// forwardStream reads the upstream stream and emits each event as it arrives.
//
// This is the whole point of the async path: emitting on arrival preserves the
// upstream's timing, where buffering the body and slicing it afterwards makes
// every chunk reach the client in one burst.
func forwardStream(callback pluginkit.HostCallback, streamID string, stream *pluginkit.HTTPStream) error {
	forwarder := &sseForwarder{}
	emitted := false
	for {
		chunk, done, errRead := stream.Read()
		if len(chunk) > 0 {
			for _, payload := range forwarder.feed(chunk) {
				if errEmit := callback.Emit(streamID, payload); errEmit != nil {
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
	for _, payload := range forwarder.flush() {
		if errEmit := callback.Emit(streamID, payload); errEmit != nil {
			return errEmit
		}
		emitted = true
	}
	if !emitted {
		return fmt.Errorf("upstream stream produced no events")
	}
	return nil
}
