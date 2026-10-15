package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// Models served only by the Responses API need this translation.
//
// The free tier exposes muse-spark on /responses and refuses it on
// /chat/completions, while a CPA client asks in chat-completions. The plugin
// therefore converts the request on the way out and the reply on the way back,
// so one client protocol reaches both upstream families.
//
// The request shape follows what the OpenCode CLI sends: input instead of
// messages, max_output_tokens instead of max_tokens, function tools flattened,
// and a system turn carried as developer. The reply is read from the Responses
// event stream and re-emitted as chat.completions chunks.

// responsesProtocol is the upstream protocol for one plan.
const (
	protocolChat      = "chat-completions"
	protocolResponses = "responses"
)

// usesResponsesAPI reports whether a model is served only by the Responses API.
func usesResponsesAPI(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "muse-spark")
}

// chatToResponses converts a chat-completions payload into a Responses payload.
//
// The conversion is deliberately lossless for the fields the two protocols
// share: text, images, tool declarations, tool calls and tool results. A field
// with no Responses equivalent is dropped rather than guessed at.
func chatToResponses(payload map[string]any, model string) (map[string]any, error) {
	out := map[string]any{
		"model":  model,
		"stream": true,
		// The tier is stateless; nothing is stored server-side.
		"store": false,
	}

	if value, okValue := firstNumber(payload, "max_completion_tokens", "max_tokens"); okValue {
		out["max_output_tokens"] = value
	}

	messages, _ := payload["messages"].([]any)
	input := make([]any, 0, len(messages))
	for _, entry := range messages {
		message, okMessage := entry.(map[string]any)
		if !okMessage {
			continue
		}
		items, errConvert := chatMessageToResponsesItems(message)
		if errConvert != nil {
			return nil, errConvert
		}
		input = append(input, items...)
	}
	out["input"] = input

	if tools, okTools := payload["tools"].([]any); okTools && len(tools) > 0 {
		out["tools"] = flattenTools(tools)
	}
	if choice, okChoice := payload["tool_choice"]; okChoice {
		if flat := flattenToolChoice(choice); flat != nil {
			out["tool_choice"] = flat
		}
	}
	return out, nil
}

// chatMessageToResponsesItems converts one chat message into the one or more
// Responses input items it represents.
func chatMessageToResponsesItems(message map[string]any) ([]any, error) {
	role, _ := message["role"].(string)
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer":
		// Responses names the instruction turn developer. The CLI sends it that
		// way, and a system role is refused.
		return []any{map[string]any{
			"role":    "developer",
			"content": responsesContent(message["content"], false),
		}}, nil

	case "tool", "function":
		// A tool result becomes a function_call_output item.
		callID, _ := message["tool_call_id"].(string)
		if strings.TrimSpace(callID) == "" {
			callID = "call_" + fmt.Sprintf("%d", time.Now().UnixNano())
		}
		return []any{map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  textOf(message["content"]),
		}}, nil

	case "assistant":
		items := make([]any, 0, 2)
		content := responsesContent(message["content"], true)
		hasContent := false
		if text := flattenText(content); strings.TrimSpace(text) != "" {
			hasContent = true
		}
		if hasContent {
			items = append(items, map[string]any{
				"role":    "assistant",
				"content": content,
			})
		}
		// Tool calls are sibling items, not a field on the assistant message.
		if calls, okCalls := message["tool_calls"].([]any); okCalls {
			for _, entry := range calls {
				call, okCall := entry.(map[string]any)
				if !okCall {
					continue
				}
				function, _ := call["function"].(map[string]any)
				name, _ := function["name"].(string)
				arguments, _ := function["arguments"].(string)
				callID, _ := call["id"].(string)
				if strings.TrimSpace(name) == "" {
					continue
				}
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   callID,
					"name":      name,
					"arguments": arguments,
				})
			}
		}
		if len(items) == 0 {
			// The upstream rejects an empty assistant turn.
			items = append(items, map[string]any{"role": "assistant", "content": " "})
		}
		return items, nil

	default:
		return []any{map[string]any{
			"role":    "user",
			"content": responsesContent(message["content"], false),
		}}, nil
	}
}

// responsesContent converts chat content into Responses content parts.
//
// assistant selects the output part names; a request uses input_text and
// input_image. A bare string becomes a single text part.
func responsesContent(raw any, assistant bool) any {
	textType, imageType := "input_text", "input_image"
	if assistant {
		textType, imageType = "output_text", "output_text"
	}

	switch typed := raw.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		parts := make([]any, 0, len(typed))
		for _, entry := range typed {
			part, okPart := entry.(map[string]any)
			if !okPart {
				if text, okText := entry.(string); okText {
					parts = append(parts, map[string]any{"type": textType, "text": text})
				}
				continue
			}
			switch kind, _ := part["type"].(string); kind {
			case "text", "input_text", "output_text":
				text, _ := part["text"].(string)
				parts = append(parts, map[string]any{"type": textType, "text": text})
			case "image_url", "input_image":
				// Chat nests the URL; Responses takes it as a plain string.
				switch image := part["image_url"].(type) {
				case string:
					parts = append(parts, map[string]any{"type": imageType, "image_url": image})
				case map[string]any:
					if url, _ := image["url"].(string); url != "" {
						parts = append(parts, map[string]any{"type": imageType, "image_url": url})
					}
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	default:
		return fmt.Sprintf("%v", raw)
	}
}

// flattenTools converts chat tool declarations into the Responses form, which
// carries name, description and parameters at the top level of the entry.
func flattenTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, entry := range tools {
		object, okObject := entry.(map[string]any)
		if !okObject {
			continue
		}
		function, okFunction := object["function"].(map[string]any)
		if !okFunction {
			// Already flat.
			if name, _ := object["name"].(string); strings.TrimSpace(name) != "" {
				out = append(out, object)
			}
			continue
		}
		flat := map[string]any{"type": "function"}
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value, okValue := function[key]; okValue {
				flat[key] = value
			}
		}
		if name, _ := flat["name"].(string); strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, flat)
	}
	return out
}

// flattenToolChoice converts a chat tool_choice into the Responses form.
func flattenToolChoice(choice any) any {
	switch typed := choice.(type) {
	case string:
		// Both protocols accept auto, none and required.
		return typed
	case map[string]any:
		if function, okFunction := typed["function"].(map[string]any); okFunction {
			if name, _ := function["name"].(string); strings.TrimSpace(name) != "" {
				return map[string]any{"type": "function", "name": name}
			}
		}
		if name, _ := typed["name"].(string); strings.TrimSpace(name) != "" {
			return map[string]any{"type": "function", "name": name}
		}
		return nil
	default:
		return nil
	}
}

// firstNumber returns the first numeric field present under any of the names.
func firstNumber(payload map[string]any, names ...string) (any, bool) {
	for _, name := range names {
		if value, okValue := payload[name]; okValue {
			switch value.(type) {
			case float64, int, int64, json.Number:
				return value, true
			}
		}
	}
	return nil, false
}

// textOf renders message content as plain text.
func textOf(raw any) string {
	switch typed := raw.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, entry := range typed {
			if part, okPart := entry.(map[string]any); okPart {
				if text, _ := part["text"].(string); text != "" {
					builder.WriteString(text)
				}
				continue
			}
			if text, okText := entry.(string); okText {
				builder.WriteString(text)
			}
		}
		return builder.String()
	default:
		return fmt.Sprintf("%v", raw)
	}
}

// flattenText returns the concatenated text of a Responses content value.
func flattenText(raw any) string { return textOf(raw) }

// ---- response translation: Responses to chat-completions ----

// responsesObjectToChatCompletion converts a complete Responses object into a
// chat.completion body. The non-streaming path reads this from the terminal
// event of the upstream stream, which carries the whole answer, so no delta
// merging is needed.
func responsesObjectToChatCompletion(response map[string]any, model string) ([]byte, error) {
	if response == nil {
		return nil, cpakit.NewError("upstream_error", "the upstream returned no response object", http.StatusBadGateway)
	}

	var content string
	reasoning := ""
	toolCalls := make([]any, 0, 2)
	for _, entry := range asSlice(response["output"]) {
		item, okItem := entry.(map[string]any)
		if !okItem {
			continue
		}
		switch kind, _ := item["type"].(string); kind {
		case "message":
			for _, partEntry := range asSlice(item["content"]) {
				part, okPart := partEntry.(map[string]any)
				if !okPart {
					continue
				}
				switch partType, _ := part["type"].(string); partType {
				case "output_text", "text":
					if text, _ := part["text"].(string); text != "" {
						content += text
					}
				case "refusal":
					if text, _ := part["refusal"].(string); text != "" {
						content += text
					}
				}
			}
		case "reasoning":
			for _, summaryEntry := range asSlice(item["summary"]) {
				summary, okSummary := summaryEntry.(map[string]any)
				if !okSummary {
					continue
				}
				if text, _ := summary["text"].(string); text != "" {
					reasoning += text
				}
			}
		case "function_call":
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			callID, _ := item["call_id"].(string)
			if strings.TrimSpace(callID) == "" {
				callID, _ = item["id"].(string)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": arguments,
				},
			})
		}
	}

	message := map[string]any{"role": "assistant"}
	if content != "" {
		message["content"] = content
	} else {
		message["content"] = ""
	}
	if reasoning != "" {
		// Kept under the name the client already understands for thinking output.
		message["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	completion := map[string]any{
		"id":      firstString(response, "id", "response_id"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   firstNonBlank(firstString(response, "model"), model),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReasonFor(response, len(toolCalls) > 0),
		}},
	}
	if usage := usageFromResponses(response["usage"]); usage != nil {
		completion["usage"] = usage
	}
	return json.Marshal(completion)
}

// responsesTranslator turns a Responses event stream into chat.completions
// chunks. It holds the little state the conversion needs: which tool call each
// output item maps to, and whether any tool call was seen.
type responsesTranslator struct {
	model string
	// toolIndex maps a Responses item id to the chat tool-call index.
	toolIndex map[string]int
	// toolOrder preserves the index assignment order.
	toolOrder []string
	sawTool   bool
	roleSent  bool
}

func newResponsesTranslator(model string) *responsesTranslator {
	return &responsesTranslator{model: model, toolIndex: map[string]int{}}
}

// Translate converts one Responses event into the chat chunks it maps to. An
// event with no chat equivalent yields nothing.
func (t *responsesTranslator) Translate(payload []byte) [][]byte {
	var event map[string]any
	if errUnmarshal := json.Unmarshal(payload, &event); errUnmarshal != nil {
		return nil
	}
	kind, _ := event["type"].(string)
	switch kind {
	case "response.output_text.delta":
		text, _ := event["delta"].(string)
		if text == "" {
			return nil
		}
		return t.chunk(map[string]any{"content": text}, nil, "")

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		text, _ := event["delta"].(string)
		if text == "" {
			return nil
		}
		return t.chunk(map[string]any{"reasoning_content": text}, nil, "")

	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		itemType, _ := item["type"].(string)
		if itemType != "function_call" {
			return nil
		}
		itemID, _ := item["id"].(string)
		name, _ := item["name"].(string)
		callID, _ := item["call_id"].(string)
		if strings.TrimSpace(callID) == "" {
			callID = itemID
		}
		index := len(t.toolOrder)
		t.toolOrder = append(t.toolOrder, itemID)
		t.toolIndex[itemID] = index
		t.sawTool = true
		delta := map[string]any{"tool_calls": []any{map[string]any{
			"index": index,
			"id":    callID,
			"type":  "function",
			"function": map[string]any{
				"name":      name,
				"arguments": "",
			},
		}}}
		return t.chunk(delta, nil, "")

	case "response.function_call_arguments.delta":
		itemID, _ := event["item_id"].(string)
		index, okIndex := t.toolIndex[itemID]
		if !okIndex {
			return nil
		}
		text, _ := event["delta"].(string)
		if text == "" {
			return nil
		}
		delta := map[string]any{"tool_calls": []any{map[string]any{
			"index":    index,
			"function": map[string]any{"arguments": text},
		}}}
		return t.chunk(delta, nil, "")

	case "response.completed":
		response, _ := event["response"].(map[string]any)
		finish := finishReasonFor(response, t.sawTool)
		chunks := t.chunk(map[string]any{}, nil, finish)
		// A chat stream reports usage on its final chunk.
		if usage := usageFromResponses(response["usage"]); usage != nil {
			chunks = append(chunks, t.usageChunk(usage))
		}
		return chunks

	case "response.incomplete":
		response, _ := event["response"].(map[string]any)
		return t.chunk(map[string]any{}, nil, finishReasonFor(response, t.sawTool))

	case "response.failed", "error", "response.error":
		// The failure is reported by the caller through the stream close, which
		// carries the upstream message; nothing is emitted here.
		return nil
	}
	return nil
}

// chunk builds one chat.completion chunk carrying delta.
func (t *responsesTranslator) chunk(delta map[string]any, finish *string, finishReason string) [][]byte {
	if !t.roleSent {
		// chat-completions announces the role once, on the first delta.
		delta = withRole(delta)
		t.roleSent = true
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	} else {
		choice["finish_reason"] = nil
	}
	encoded, errMarshal := json.Marshal(map[string]any{
		"id":      "chatcmpl-" + t.model,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   t.model,
		"choices": []any{choice},
	})
	if errMarshal != nil {
		return nil
	}
	return [][]byte{encoded}
}

// usageChunk emits the usage-only final chunk chat clients expect.
func (t *responsesTranslator) usageChunk(usage map[string]any) []byte {
	encoded, errMarshal := json.Marshal(map[string]any{
		"id":      "chatcmpl-" + t.model,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   t.model,
		"choices": []any{},
		"usage":   usage,
	})
	if errMarshal != nil {
		return nil
	}
	return encoded
}

// withRole adds the assistant role without disturbing existing fields.
func withRole(delta map[string]any) map[string]any {
	out := map[string]any{"role": "assistant"}
	for key, value := range delta {
		out[key] = value
	}
	return out
}

// finishReasonFor maps a Responses status onto a chat finish_reason.
func finishReasonFor(response map[string]any, sawTool bool) string {
	if sawTool {
		return "tool_calls"
	}
	if response == nil {
		return "stop"
	}
	switch status, _ := response["status"].(string); status {
	case "incomplete":
		// Truncation by the output cap is the only incomplete cause chat names.
		return "length"
	case "failed":
		return "stop"
	default:
		return "stop"
	}
}

// usageFromResponses maps a Responses usage object onto chat token accounting.
func usageFromResponses(raw any) map[string]any {
	usage, okUsage := raw.(map[string]any)
	if !okUsage {
		return nil
	}
	out := map[string]any{}
	if value, okValue := usage["input_tokens"]; okValue {
		out["prompt_tokens"] = value
	}
	if value, okValue := usage["output_tokens"]; okValue {
		out["completion_tokens"] = value
	}
	if value, okValue := usage["total_tokens"]; okValue {
		out["total_tokens"] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// responsesCompletionFromStream folds a whole Responses event stream into one
// chat.completion body, for a client that asked for a buffered answer.
func responsesCompletionFromStream(raw []byte, model string) ([]byte, error) {
	var failure string
	for _, payload := range cpakit.SplitSSE(raw) {
		var event map[string]any
		if errUnmarshal := json.Unmarshal(payload, &event); errUnmarshal != nil {
			continue
		}
		switch kind, _ := event["type"].(string); kind {
		case "response.completed":
			return responsesObjectToChatCompletion(asMap(event["response"]), model)
		case "response.incomplete":
			// An incomplete stream still carries the partial answer.
			if response := asMap(event["response"]); response != nil {
				return responsesObjectToChatCompletion(response, model)
			}
		case "response.failed", "error", "response.error":
			failure = firstNonBlank(
				firstString(event, "message"),
				firstString(asMap(event["error"]), "message"),
				nestedErrorMessage(asMap(event["response"])),
			)
		}
	}
	if failure != "" {
		return nil, cpakit.NewError("upstream_error", "the upstream failed the response: "+cpakit.Redact(failure), http.StatusBadGateway)
	}
	return nil, cpakit.NewError("upstream_error", "the upstream returned no completed response event", http.StatusBadGateway)
}

// asSlice returns a value as a slice, or nil.
func asSlice(raw any) []any {
	slice, _ := raw.([]any)
	return slice
}

// asMap returns a value as a map, or nil.
func asMap(raw any) map[string]any {
	object, _ := raw.(map[string]any)
	return object
}

// firstString returns the first non-blank string field among the names.
func firstString(source map[string]any, names ...string) string {
	if source == nil {
		return ""
	}
	for _, name := range names {
		if value, _ := source[name].(string); strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// nestedErrorMessage extracts the failure message from a failed response
// object, whose error field is itself an object.
func nestedErrorMessage(response map[string]any) string {
	if response == nil {
		return ""
	}
	if inner := asMap(response["error"]); inner != nil {
		if message := firstNonBlank(firstString(inner, "message"), firstString(inner, "type")); message != "" {
			return message
		}
	}
	if message := firstString(response, "error"); message != "" {
		return message
	}
	return ""
}

// firstNonBlank returns the first value that is not empty after trimming.
func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
