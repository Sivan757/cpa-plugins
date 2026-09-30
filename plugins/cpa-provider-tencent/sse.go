package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// splitSSE turns a buffered SSE body into the bare JSON chunks the host frames.
//
// The host wraps every executor chunk as "data: <chunk>\n\n" and appends its
// own [DONE], so a chunk must carry the JSON payload alone: including an SSE
// prefix here would produce a doubled one downstream, and re-sending [DONE]
// would terminate the stream twice.
//
// The upstream body is read in full because the host HTTP client buffers, so
// these are framing units rather than live network reads.
func splitSSE(body []byte) []pluginkit.ExecutorStreamChunk {
	normalised := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	chunks := make([]pluginkit.ExecutorStreamChunk, 0, 64)
	for _, line := range strings.Split(string(normalised), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		chunks = append(chunks, pluginkit.ExecutorStreamChunk{Payload: []byte(payload)})
	}
	return chunks
}

// streamEvent is the subset of one OpenAI stream chunk this plugin reads.
type streamEvent struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int             `json:"index"`
		Delta        json.RawMessage `json:"delta"`
		FinishReason *string         `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// deltaFields are the assistant-message fields carried by stream deltas.
type deltaFields struct {
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        json.RawMessage `json:"tool_calls"`
	FunctionCall     json.RawMessage `json:"function_call"`
}

// aggregateStream folds a buffered SSE body into one non-streaming chat
// completion. Reasoning text is preserved on a dedicated field so a client that
// understands it still receives it.
func aggregateStream(body []byte) ([]byte, error) {
	normalised := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	var (
		id           string
		model        string
		created      int64
		content      strings.Builder
		reasoning    strings.Builder
		toolCalls    []json.RawMessage
		finishReason = "stop"
		usage        json.RawMessage
		sawEvent     bool
	)
	for _, line := range strings.Split(string(normalised), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event streamEvent
		if errUnmarshal := json.Unmarshal([]byte(payload), &event); errUnmarshal != nil {
			continue
		}
		// Some deployments report failures in-band with HTTP 200.
		if message := inBandError(payload); message != "" {
			return nil, pluginkit.NewError("upstream_error", "WorkBuddy reported an error in the stream: "+message, 502)
		}
		sawEvent = true
		if event.ID != "" {
			id = event.ID
		}
		if event.Model != "" {
			model = event.Model
		}
		if event.Created != 0 {
			created = event.Created
		}
		if len(event.Usage) > 0 && string(event.Usage) != "null" {
			usage = event.Usage
		}
		for _, choice := range event.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
			if len(choice.Delta) == 0 {
				continue
			}
			var delta deltaFields
			if errDelta := json.Unmarshal(choice.Delta, &delta); errDelta != nil {
				continue
			}
			content.WriteString(delta.Content)
			reasoning.WriteString(delta.ReasoningContent)
			if len(delta.ToolCalls) > 0 && string(delta.ToolCalls) != "null" {
				toolCalls = append(toolCalls, delta.ToolCalls)
			}
		}
	}
	if !sawEvent {
		return nil, pluginkit.NewError("upstream_error", "WorkBuddy returned no stream events", 502)
	}

	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	// The upstream emits an empty tool_calls array in every delta. Only a
	// non-empty merge is a real tool call; forwarding "[]" would claim the
	// assistant called tools when it did not.
	if merged := mergeToolCalls(toolCalls); len(merged) > 0 {
		message["tool_calls"] = merged
	}
	out := map[string]any{
		"id":      firstNonEmpty(id, "chatcmpl-workbuddy"),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if len(usage) > 0 {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

// mergeToolCalls joins the incremental tool_call fragments the upstream emits.
func mergeToolCalls(fragments []json.RawMessage) []any {
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
		var calls []struct {
			Index    int    `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if errUnmarshal := json.Unmarshal(fragment, &calls); errUnmarshal != nil {
			continue
		}
		for _, call := range calls {
			acc, okAcc := byIndex[call.Index]
			if !okAcc {
				acc = &accumulator{index: call.Index}
				byIndex[call.Index] = acc
				ordered = append(ordered, acc)
			}
			if call.ID != "" {
				acc.id = call.ID
			}
			if call.Type != "" {
				acc.kind = call.Type
			}
			if call.Function.Name != "" {
				acc.name = call.Function.Name
			}
			acc.arguments.WriteString(call.Function.Arguments)
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

// inBandError extracts an error string carried inside a stream chunk.
func inBandError(payload string) string {
	var probe struct {
		Code  json.RawMessage `json:"code"`
		Error json.RawMessage `json:"error"`
		Msg   string          `json:"msg"`
	}
	if errUnmarshal := json.Unmarshal([]byte(payload), &probe); errUnmarshal != nil {
		return ""
	}
	if len(probe.Error) > 0 && string(probe.Error) != "null" {
		return strings.Trim(string(probe.Error), "\"")
	}
	if len(probe.Code) > 0 {
		var numeric int
		if errNumeric := json.Unmarshal(probe.Code, &numeric); errNumeric == nil && numeric != 0 {
			return fmt.Sprintf("code %d %s", numeric, probe.Msg)
		}
	}
	return ""
}
