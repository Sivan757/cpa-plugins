package pluginkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// SplitSSE turns a buffered SSE body into the bare JSON chunks the host frames.
//
// The host wraps every executor chunk and appends its own terminator, so a
// chunk must carry the JSON payload alone: keeping the SSE prefix here would
// double it downstream, and forwarding the upstream terminator would end the
// client stream twice.
func SplitSSE(body []byte) []ExecutorStreamChunk {
	chunks := make([]ExecutorStreamChunk, 0, 64)
	for _, payload := range sseDataPayloads(body) {
		chunks = append(chunks, ExecutorStreamChunk{Payload: []byte(payload)})
	}
	return chunks
}

// sseDataPayloads returns the data payload of every event in a buffered body.
func sseDataPayloads(body []byte) []string {
	normalised := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	out := make([]string, 0, 64)
	for _, line := range strings.Split(string(normalised), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		out = append(out, payload)
	}
	return out
}

// AggregateStream folds a buffered SSE body into one non-streaming chat
// completion. Reasoning text is preserved on a dedicated field so a client that
// understands it still receives it.
func AggregateStream(body []byte) ([]byte, error) {
	var (
		id           string
		model        string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		toolCalls    []any
		finishReason = "stop"
		usage        any
		sawEvent     bool
	)
	for _, payload := range sseDataPayloads(body) {
		var event map[string]any
		if errUnmarshal := json.Unmarshal([]byte(payload), &event); errUnmarshal != nil {
			continue
		}
		if message := inBandError(event); message != "" {
			return nil, NewError("upstream_error", "upstream reported an error in the stream: "+message, http.StatusBadGateway)
		}
		sawEvent = true
		if value, ok := event["id"].(string); ok && value != "" {
			id = value
		}
		if value, ok := event["model"].(string); ok && value != "" {
			model = value
		}
		if value, ok := event["created"].(float64); ok {
			created = value
		}
		if value, okPresent := event["usage"]; okPresent && value != nil {
			usage = value
		}
		choices, _ := event["choices"].([]any)
		for _, entry := range choices {
			choice, okChoice := entry.(map[string]any)
			if !okChoice {
				continue
			}
			if reason, okReason := choice["finish_reason"].(string); okReason && reason != "" {
				finishReason = reason
			}
			delta, okDelta := choice["delta"].(map[string]any)
			if !okDelta {
				continue
			}
			if text, okText := delta["content"].(string); okText {
				content.WriteString(text)
			}
			if text, okText := delta["reasoning_content"].(string); okText {
				reasoning.WriteString(text)
			}
			if text, okText := delta["reasoning"].(string); okText {
				reasoning.WriteString(text)
			}
			if calls, okCalls := delta["tool_calls"].([]any); okCalls && len(calls) > 0 {
				toolCalls = append(toolCalls, calls...)
			}
		}
	}
	if !sawEvent {
		return nil, NewError("upstream_error", "upstream returned no stream events", http.StatusBadGateway)
	}

	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if merged := MergeToolCalls(toolCalls); len(merged) > 0 {
		message["tool_calls"] = merged
	}
	out := map[string]any{
		"id":      firstNonEmptyString(id, "chatcmpl-cpa"),
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

// MergeToolCalls joins the incremental tool_call fragments a stream emits.
func MergeToolCalls(fragments []any) []any {
	type accumulator struct {
		index     int
		id        string
		kind      string
		name      string
		arguments strings.Builder
	}
	ordered := make([]*accumulator, 0, 4)
	byIndex := map[int]*accumulator{}
	for _, fragment := range fragments {
		object, okObject := fragment.(map[string]any)
		if !okObject {
			continue
		}
		index := 0
		if value, okIndex := object["index"].(float64); okIndex {
			index = int(value)
		}
		acc, okAcc := byIndex[index]
		if !okAcc {
			acc = &accumulator{index: index}
			byIndex[index] = acc
			ordered = append(ordered, acc)
		}
		if value, okValue := object["id"].(string); okValue && value != "" {
			acc.id = value
		}
		if value, okValue := object["type"].(string); okValue && value != "" {
			acc.kind = value
		}
		if function, okFunction := object["function"].(map[string]any); okFunction {
			if value, okValue := function["name"].(string); okValue && value != "" {
				acc.name = value
			}
			if value, okValue := function["arguments"].(string); okValue {
				acc.arguments.WriteString(value)
			}
		}
	}
	out := make([]any, 0, len(ordered))
	for _, acc := range ordered {
		kind := acc.kind
		if kind == "" {
			kind = "function"
		}
		out = append(out, map[string]any{
			"index": acc.index,
			"id":    acc.id,
			"type":  kind,
			"function": map[string]any{
				"name":      acc.name,
				"arguments": acc.arguments.String(),
			},
		})
	}
	return out
}

// inBandError extracts an error carried inside a successful stream event.
func inBandError(event map[string]any) string {
	if value, okValue := event["error"]; okValue && value != nil {
		switch typed := value.(type) {
		case string:
			return typed
		case map[string]any:
			if message, okMessage := typed["message"].(string); okMessage {
				return message
			}
			if kind, okKind := typed["type"].(string); okKind {
				return kind
			}
		}
	}
	return ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// TruncateBody renders a bounded prefix of an upstream body for an error
// message, so a large error page cannot flood the client or the log.
func TruncateBody(body []byte, limit int) string {
	if limit <= 0 {
		limit = 400
	}
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "..."
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

// SSEForwarder turns an arbitrary byte stream into the bare JSON payloads the
// host frames.
//
// Upstream chunks split wherever the transport delivers them, so an event
// routinely straddles two reads. Buffering until a blank-line terminator is
// what keeps a partial event from being emitted as truncated JSON.
type SSEForwarder struct {
	pending []byte
}

// Feed appends a read and returns every complete event payload it now holds.
func (f *SSEForwarder) Feed(chunk []byte) [][]byte {
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

// Flush returns a trailing event that arrived without its terminating blank
// line, which happens when an upstream closes the stream right after the final
// event.
func (f *SSEForwarder) Flush() [][]byte {
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

// ForwardStream reads an upstream stream and emits each event as it arrives.
//
// Emitting on arrival preserves the upstream timing, where buffering the body
// and slicing it afterwards makes every chunk reach the client in one burst.
func ForwardStream(callback HostCallback, streamID string, stream *HTTPStream) error {
	forwarder := &SSEForwarder{}
	emitted := false
	for {
		chunk, done, errRead := stream.Read()
		if len(chunk) > 0 {
			for _, payload := range forwarder.Feed(chunk) {
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
	for _, payload := range forwarder.Flush() {
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
