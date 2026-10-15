package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChatToResponsesCarriesTheSharedFields proves the request conversion keeps
// the model, the token cap, the tool list and the tool result. A field the
// Responses API does not accept must not survive the conversion.
func TestChatToResponsesCarriesTheSharedFields(t *testing.T) {
	chat := map[string]any{
		"model":      "muse-spark-1.3-contributor-free",
		"stream":     false,
		"max_tokens": float64(64),
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
					"name": "read", "arguments": "{\"filePath\":\"/tmp/a\"}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "file body"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "read", "description": "read a file",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"filePath": map[string]any{"type": "string"}}},
			}},
		},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
	}

	converted, errConvert := chatToResponses(chat, "muse-spark-1.3-contributor-free")
	if errConvert != nil {
		t.Fatalf("convert: %v", errConvert)
	}

	// The upstream refuses a non-streaming request on this route.
	if converted["stream"] != true {
		t.Fatalf("stream = %v, want true", converted["stream"])
	}
	if converted["store"] != false {
		t.Fatalf("store = %v, want false", converted["store"])
	}
	if converted["max_output_tokens"] != float64(64) {
		t.Fatalf("max_output_tokens = %v", converted["max_output_tokens"])
	}
	if _, present := converted["max_tokens"]; present {
		t.Fatal("max_tokens must not be carried over; the Responses API names it max_output_tokens")
	}
	if _, present := converted["messages"]; present {
		t.Fatal("messages must not be carried over; the Responses API uses input")
	}

	input, _ := converted["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input has %d items, want 4", len(input))
	}
	first, _ := input[0].(map[string]any)
	if first["role"] != "developer" {
		t.Fatalf("instruction turn role = %v, want developer", first["role"])
	}
	for _, entry := range input {
		item, _ := entry.(map[string]any)
		if item["role"] == "system" {
			t.Fatal("a system role survived the conversion")
		}
	}

	var sawCall, sawOutput bool
	for _, entry := range input {
		item, _ := entry.(map[string]any)
		switch item["type"] {
		case "function_call":
			sawCall = true
			if item["call_id"] != "call_1" || item["name"] != "read" {
				t.Fatalf("function_call item = %v", item)
			}
		case "function_call_output":
			sawOutput = true
			if item["call_id"] != "call_1" {
				t.Fatalf("function_call_output item = %v", item)
			}
		}
	}
	if !sawCall || !sawOutput {
		t.Fatalf("tool call or output missing: call=%v output=%v", sawCall, sawOutput)
	}

	tools, _ := converted["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "read" {
		t.Fatalf("flattened tool name = %v", tool["name"])
	}
	if _, nested := tool["function"]; nested {
		t.Fatal("a flattened tool must not keep a nested function object")
	}
	if tool["parameters"] == nil {
		t.Fatal("the parameter schema was dropped")
	}

	choice, _ := converted["tool_choice"].(map[string]any)
	if choice["name"] != "read" || choice["type"] != "function" {
		t.Fatalf("tool_choice = %v", choice)
	}
}

// TestResponsesTranslatorEmitsChatChunks drives the streaming conversion over a
// recorded event sequence and asserts the chunks a chat client receives.
func TestResponsesTranslatorEmitsChatChunks(t *testing.T) {
	translator := newResponsesTranslator("muse-spark-1.3-contributor-free")

	delta := translator.Translate([]byte(`{"type":"response.output_text.delta","delta":"OK"}`))
	if len(delta) != 1 {
		t.Fatalf("text delta produced %d chunks, want 1", len(delta))
	}
	parts, _ := firstChoice(t, decodeChunk(t, delta[0]))["delta"].(map[string]any)
	if parts["content"] != "OK" {
		t.Fatalf("delta content = %v", parts["content"])
	}
	// chat-completions announces the role on the first delta only.
	if parts["role"] != "assistant" {
		t.Fatalf("first delta role = %v", parts["role"])
	}

	added := translator.Translate([]byte(`{"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","call_id":"call_9","name":"read"}}`))
	if len(added) != 1 {
		t.Fatalf("tool start produced %d chunks", len(added))
	}
	toolDelta, _ := firstChoice(t, decodeChunk(t, added[0]))["delta"].(map[string]any)
	if toolDelta["role"] != nil {
		t.Fatal("the role must be announced only once")
	}
	calls, _ := toolDelta["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", toolDelta["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "call_9" || call["index"] != float64(0) {
		t.Fatalf("tool call = %v", call)
	}

	args := translator.Translate([]byte(`{"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"{\"filePath\":\"/tmp\"}"}`))
	if len(args) != 1 {
		t.Fatalf("argument delta produced %d chunks", len(args))
	}
	argDelta, _ := firstChoice(t, decodeChunk(t, args[0]))["delta"].(map[string]any)
	argCalls, _ := argDelta["tool_calls"].([]any)
	argCall, _ := argCalls[0].(map[string]any)
	function, _ := argCall["function"].(map[string]any)
	if function["arguments"] != `{"filePath":"/tmp"}` {
		t.Fatalf("arguments = %v", function["arguments"])
	}

	completed := translator.Translate([]byte(`{"type":"response.completed","response":{"id":"resp_1","model":"muse-spark-1.3-contributor-free","status":"completed","usage":{"input_tokens":11,"output_tokens":3,"total_tokens":14}}}`))
	if len(completed) != 2 {
		t.Fatalf("completion produced %d chunks, want a finish chunk and a usage chunk", len(completed))
	}
	final := firstChoice(t, decodeChunk(t, completed[0]))
	if final["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", final["finish_reason"])
	}
	usage, _ := decodeChunk(t, completed[1])["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(3) || usage["total_tokens"] != float64(14) {
		t.Fatalf("usage = %v", usage)
	}
}

// TestResponsesStreamFoldsIntoACompletion proves the buffered answer is read from
// the terminal event, which carries the whole output.
func TestResponsesStreamFoldsIntoACompletion(t *testing.T) {
	stream := `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"O"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_2","model":"muse-spark-1.3-contributor-free","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]},{"type":"function_call","call_id":"call_3","name":"read","arguments":"{}"}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}
`

	body, errFold := responsesCompletionFromStream([]byte(stream), "muse-spark-1.3-contributor-free")
	if errFold != nil {
		t.Fatalf("fold: %v", errFold)
	}
	var completion map[string]any
	if errUnmarshal := json.Unmarshal(body, &completion); errUnmarshal != nil {
		t.Fatalf("decode completion: %v", errUnmarshal)
	}
	if completion["object"] != "chat.completion" {
		t.Fatalf("object = %v", completion["object"])
	}
	choice := firstChoice(t, completion)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "OK" {
		t.Fatalf("content = %v", message["content"])
	}
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", message["tool_calls"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage, _ := completion["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(5) {
		t.Fatalf("usage = %v", usage)
	}
}

// TestResponsesStreamReportsAFailure proves a failed stream surfaces the upstream
// reason instead of returning an empty answer. A region block is reported this
// way, so a silent success would hide the real cause.
func TestResponsesStreamReportsAFailure(t *testing.T) {
	stream := `event: response.failed
data: {"type":"response.failed","response":{"id":"resp_3","status":"failed","error":{"message":"This model is not available in your country"}}}
`

	_, errFold := responsesCompletionFromStream([]byte(stream), "muse-spark-1.3-contributor-free")
	if errFold == nil {
		t.Fatal("a failed stream was reported as success")
	}
	if !strings.Contains(errFold.Error(), "not available in your country") {
		t.Fatalf("the error does not carry the upstream reason: %v", errFold)
	}
}

// TestUsesResponsesAPIRoutesOnlyMuseSpark keeps the chat path unchanged for every
// other model, which is what makes this change contained.
func TestUsesResponsesAPIRoutesOnlyMuseSpark(t *testing.T) {
	if !usesResponsesAPI("muse-spark-1.3-contributor-free") {
		t.Fatal("a muse-spark model was not routed to the Responses API")
	}
	if !usesResponsesAPI("MUSE-SPARK-1.2-CONTRIBUTOR-FREE") {
		t.Fatal("the check must ignore case")
	}
	for _, model := range []string{"mimo-v2.6-flash-free", "space-bunny-free", "deepseek-flash", "qwen3.8-flash"} {
		if usesResponsesAPI(model) {
			t.Fatalf("%s must stay on the chat path", model)
		}
	}
}

// TestMuseSparkIsAdvertised proves the catalogue no longer hides the models the
// tier publishes on the Responses API.
func TestMuseSparkIsAdvertised(t *testing.T) {
	entries := []map[string]any{
		{"id": "muse-spark-1.3-contributor-free"},
		{"id": "muse-spark-1.2-contributor-free"},
		{"id": "mimo-v2.6-flash-free"},
		{"id": "glm-5.3"},
	}
	models := buildModelList(entries, tierFree)
	seen := map[string]bool{}
	for _, model := range models {
		seen[model.ID] = true
	}
	if !seen["muse-spark-1.3-contributor-free"] || !seen["muse-spark-1.2-contributor-free"] {
		t.Fatalf("muse-spark models are missing from the catalogue: %v", seen)
	}
	if seen["glm-5.3"] {
		t.Fatal("a paid model leaked into the free-tier catalogue")
	}
}

// decodeChunk decodes one emitted chat chunk.
func decodeChunk(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var chunk map[string]any
	if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
		t.Fatalf("decode chunk %s: %v", raw, errUnmarshal)
	}
	return chunk
}

// firstChoice returns the first choice of a chat object.
func firstChoice(t *testing.T, object map[string]any) map[string]any {
	t.Helper()
	choices, _ := object["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in %v", object)
	}
	choice, _ := choices[0].(map[string]any)
	return choice
}
