package cpakit

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// SplitSSE turns a buffered SSE body into the bare JSON chunks the host frames.
//
// The host wraps every executor chunk and appends its own terminator, so a chunk
// must carry the JSON payload alone: keeping the SSE prefix here would double it
// downstream, and forwarding the upstream terminator would end the client
// stream twice.
func SplitSSE(body []byte) [][]byte {
	out := make([][]byte, 0, 64)
	for _, payload := range sseDataPayloads(body) {
		out = append(out, []byte(payload))
	}
	return out
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
		if message := InBandError(event); message != "" {
			return nil, &Error{Class: ClassUpstream, Message: Redact("upstream reported an error in the stream: " + message), StatusCode: http.StatusBadGateway}
		}
		sawEvent = true
		if value, okValue := event["id"].(string); okValue && value != "" {
			id = value
		}
		if value, okValue := event["model"].(string); okValue && value != "" {
			model = value
		}
		if value, okValue := event["created"].(float64); okValue {
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
		return nil, &Error{Class: ClassUpstream, Message: "upstream returned no stream events", StatusCode: http.StatusBadGateway}
	}

	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	// The upstream emits an empty tool_calls array in every delta. Only a
	// non-empty merge is a real tool call.
	if merged := MergeToolCalls(toolCalls); len(merged) > 0 {
		message["tool_calls"] = merged
	}
	out := map[string]any{
		"id":      FirstNonEmpty(id, "chatcmpl-cpa"),
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

// InBandError extracts an error carried inside a successful stream event.
func InBandError(event map[string]any) string {
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

// FirstNonEmpty returns the first value that is not blank.
func FirstNonEmpty(values ...string) string {
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

// SSEForwarder turns an arbitrary byte stream into the bare JSON payloads the
// host frames.
//
// Upstream chunks split wherever the transport delivers them, so an event
// routinely straddles two reads. Buffering until a blank-line terminator keeps a
// partial event from being emitted as truncated JSON.
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

// PumpUpstream reads an upstream host stream, converts each complete event, and
// emits the results downstream until the upstream closes.
//
// convert returns the events to emit and whether the conversion reported a
// terminal condition. Both streams are closed before returning, so a caller can
// run this in a goroutine and forget it.
func (b *HostBridge) PumpUpstream(downStreamID, upStreamID string, convert func(payload []byte) ([][]byte, bool, error)) error {
	forwarder := &SSEForwarder{}
	converterDone := false
	var pumpErr error

	read := func(payload []byte) error {
		if len(payload) == 0 {
			return nil
		}
		for _, event := range forwarder.Feed(payload) {
			events, done, errConvert := convert(event)
			if errConvert != nil {
				return errConvert
			}
			for _, out := range events {
				if errEmit := b.StreamEmit(downStreamID, out); errEmit != nil {
					return errEmit
				}
			}
			if done {
				converterDone = true
				return nil
			}
		}
		return nil
	}

	for {
		payload, errMsg, done, errRead := b.StreamRead(upStreamID)
		if errRead != nil {
			pumpErr = errRead
			break
		}
		if errMsg != "" {
			pumpErr = &Error{Class: ClassUpstream, Message: Redact(errMsg), StatusCode: http.StatusBadGateway}
			break
		}
		if errPump := read(payload); errPump != nil {
			pumpErr = errPump
			break
		}
		if converterDone || done {
			break
		}
	}
	if pumpErr == nil && !converterDone {
		for _, event := range forwarder.Flush() {
			events, _, errConvert := convert(event)
			if errConvert != nil {
				pumpErr = errConvert
				break
			}
			for _, out := range events {
				if errEmit := b.StreamEmit(downStreamID, out); errEmit != nil {
					pumpErr = errEmit
					break
				}
			}
		}
	}

	_ = b.StreamClose(upStreamID)
	errMessage := ""
	if pumpErr != nil {
		errMessage = Redact(pumpErr.Error())
	}
	_ = b.StreamCloseDownstream(downStreamID, errMessage)
	return pumpErr
}
