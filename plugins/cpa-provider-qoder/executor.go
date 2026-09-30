package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

const (
	executorInputFormat  = "chat-completions"
	executorOutputFormat = "chat-completions"
)

// Execute performs a non-streaming chat completion.
//
// The upstream only speaks streaming, so this buffers the stream and returns the
// final assistant message as one chat completion.
func (g *gateway) Execute(ctx context.Context, req pluginkit.ExecutorRequest) (pluginkit.ExecutorResponse, error) {
	identity := newTurnIdentity(g.impl.identityUID(req.StorageJSON), requestSession(req))
	body, model, errPrepare := prepareChatBody(req, identity)
	if errPrepare != nil {
		return pluginkit.ExecutorResponse{}, errPrepare
	}
	chunks, errCall := g.impl.callChat(ctx, req.StorageJSON, body, model)
	if errCall != nil {
		return pluginkit.ExecutorResponse{}, errCall
	}
	aggregated, errAggregate := aggregateChunks(chunks, model)
	if errAggregate != nil {
		return pluginkit.ExecutorResponse{}, errAggregate
	}
	return pluginkit.ExecutorResponse{
		Payload: aggregated,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// ExecuteStream performs a streaming chat completion.
//
// Chunks are bare JSON. The host wraps each one as "data: <chunk>" and appends
// its own [DONE]; adding either here would double the framing and end the
// client's stream twice.
func (g *gateway) ExecuteStream(ctx context.Context, req pluginkit.ExecutorRequest) (pluginkit.ExecutorStreamResponse, error) {
	identity := newTurnIdentity(g.impl.identityUID(req.StorageJSON), requestSession(req))
	body, model, errPrepare := prepareChatBody(req, identity)
	if errPrepare != nil {
		return pluginkit.ExecutorStreamResponse{}, errPrepare
	}
	chunks, errCall := g.impl.callChat(ctx, req.StorageJSON, body, model)
	if errCall != nil {
		return pluginkit.ExecutorStreamResponse{}, errCall
	}
	payloads := make([]pluginkit.ExecutorStreamChunk, 0, len(chunks))
	for _, chunk := range chunks {
		payloads = append(payloads, pluginkit.ExecutorStreamChunk{Payload: []byte(chunk)})
	}
	return pluginkit.ExecutorStreamResponse{
		Headers: http.Header{
			"Content-Type":  []string{"text/event-stream"},
			"Cache-Control": []string{"no-cache"},
		},
		Chunks: payloads,
	}, nil
}

// CountTokens is not offered by the upstream; the host falls back to its own
// estimator.
func (g *gateway) CountTokens(context.Context, pluginkit.ExecutorRequest) (pluginkit.ExecutorResponse, error) {
	return pluginkit.ExecutorResponse{}, pluginkit.ErrUnhandled
}

// HttpRequest exposes raw HTTP bridging for diagnostics.
//
// The COSY face signs the URL, so a request aimed at it is signed here; anything
// else uses the plain bearer, which is what the openapi face expects.
func (g *gateway) HttpRequest(ctx context.Context, req pluginkit.ExecutorHTTPRequest) (pluginkit.ExecutorHTTPResponse, error) {
	creds, errCreds := g.impl.resolveCredentials(ctx, req.StorageJSON)
	if errCreds != nil {
		return pluginkit.ExecutorHTTPResponse{}, errCreds
	}
	var result *pluginkit.HTTPResult
	var errDo error
	if strings.Contains(req.URL, "/algo") {
		result, errDo = signedRequest(ctx, creds.Region, req.Method, req.URL, creds.Identity, req.Body, req.Headers)
	} else {
		headers := req.Headers
		if headers == nil {
			headers = openAPIHeaders(creds.Identity.AuthToken)
		} else if headers.Get("Authorization") == "" {
			headers.Set("Authorization", "Bearer "+creds.Identity.AuthToken)
		}
		result, errDo = pluginkit.HTTPDo(ctx, req.Method, req.URL, headers, req.Body)
	}
	if errDo != nil {
		return pluginkit.ExecutorHTTPResponse{}, errDo
	}
	return pluginkit.ExecutorHTTPResponse{StatusCode: result.StatusCode, Headers: result.Header, Body: result.Body}, nil
}

// prepareChatBody normalises a client chat request into the upstream's dialect
// and reports the model the request must be routed to.
//
// Qoder runs the request body the upstream already understands, so the standard
// OpenAI shape is passed through rather than converted into the client's private
// envelope. Three adjustments are mandatory:
//
//   - stream must be true: the upstream has no non-streaming mode, and
//     include_usage is the only way the final chunk reports token usage.
//   - a developer-role message is rejected during deserialisation, so it is
//     folded back to system (the same rewrite the other vendor bridges do).
//   - a null content field makes the message invisible to the upstream's
//     tool-pairing validator, so it becomes an empty string; orphaned tool
//     results and unanswered tool calls are repaired the same way, because the
//     strict model families reject the whole request otherwise.
//
// The body also carries the attribution fields the official client sends: they
// are optional for billing but required for the vendor's statistics views.
func prepareChatBody(req pluginkit.ExecutorRequest, identity turnIdentity) ([]byte, string, error) {
	raw := req.Payload
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = req.OriginalRequest
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, "", pluginkit.BadRequestError("no request body was provided to the executor")
	}
	var document map[string]any
	if errUnmarshal := json.Unmarshal(raw, &document); errUnmarshal != nil {
		return nil, "", pluginkit.BadRequestError("decode chat request: " + errUnmarshal.Error())
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		if fromBody, okBody := document["model"].(string); okBody {
			model = strings.TrimSpace(fromBody)
		}
	}
	if model == "" {
		return nil, "", pluginkit.BadRequestError("the chat request does not name a model")
	}
	// Routing reads the X-Model-Key header, so the body and the header must agree.
	document["model"] = model
	document["stream"] = true
	document["stream_options"] = map[string]any{"include_usage": true}

	// Attribution. chat_record_id mirrors request_id in the official client.
	document["request_id"] = identity.RequestID
	document["request_set_id"] = identity.RequestSetID
	document["chat_record_id"] = identity.ChatRecordID
	document["session_id"] = identity.SessionID

	if messages, okMessages := document["messages"].([]any); okMessages {
		document["messages"] = sanitizeToolPairing(messages)
	}

	encoded, errMarshal := json.Marshal(document)
	if errMarshal != nil {
		return nil, "", pluginkit.NewError("internal_error", errMarshal.Error())
	}
	return encoded, model, nil
}

// callChat posts one prepared body to the signed chat route and returns the
// unwrapped chunks.
func (p *pluginState) callChat(ctx context.Context, storageJSON []byte, body []byte, model string) ([]string, error) {
	creds, errCreds := p.resolveCredentials(ctx, storageJSON)
	if errCreds != nil {
		return nil, errCreds
	}
	url := creds.Region.apiBase + qoderChatRoute + qoderChatQuery
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Accept-Encoding", "identity")
	headers.Set("X-Model-Key", model)
	headers.Set("X-Model-Source", "system")

	result, errCall := signedRequest(ctx, creds.Region, http.MethodPost, url, creds.Identity, []byte(qoderEncodeBody(body)), headers)
	if errCall != nil {
		return nil, errCall
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		// A rejected bearer has usually lapsed; drop it so the next call
		// re-exchanges rather than replaying the same failure.
		if result.StatusCode == http.StatusUnauthorized || result.StatusCode == http.StatusForbidden {
			p.tokens.invalidate(creds.PAT, creds.Region)
		}
		return nil, classifyOpenAPIError(result.StatusCode, "chat completion", result.Body)
	}
	return parseQoderStream(result.Body)
}

// streamChunk is the subset of one OpenAI stream chunk this plugin reads.
type streamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index        int             `json:"index"`
		Delta        json.RawMessage `json:"delta"`
		FinishReason *string         `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

type deltaFields struct {
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        json.RawMessage `json:"tool_calls"`
}

// aggregateChunks folds streamed chunks into one non-streaming completion.
//
// Reasoning text is preserved on its own field so a client that understands it
// still receives it, and the requested model replaces the upstream's routing
// placeholder, which always reports "auto".
func aggregateChunks(chunks []string, model string) ([]byte, error) {
	var (
		id           string
		created      int64
		content      strings.Builder
		reasoning    strings.Builder
		toolCalls    []json.RawMessage
		finishReason = "stop"
		usage        json.RawMessage
		sawChunk     bool
	)
	for _, chunk := range chunks {
		var event streamChunk
		if errUnmarshal := json.Unmarshal([]byte(chunk), &event); errUnmarshal != nil {
			continue
		}
		sawChunk = true
		if event.ID != "" {
			id = event.ID
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
	if !sawChunk {
		return nil, pluginkit.NewError("upstream_error", "Qoder returned no stream events", http.StatusBadGateway)
	}

	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if merged := mergeToolCalls(toolCalls); len(merged) > 0 {
		message["tool_calls"] = merged
	}
	out := map[string]any{
		"id":      firstNonEmpty(id, "chatcmpl-qoder"),
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

// mergeToolCalls joins the incremental tool_call fragments the upstream streams.
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
			entry, okEntry := byIndex[call.Index]
			if !okEntry {
				entry = &accumulator{index: call.Index}
				byIndex[call.Index] = entry
				ordered = append(ordered, entry)
			}
			if call.ID != "" {
				entry.id = call.ID
			}
			if call.Type != "" {
				entry.kind = call.Type
			}
			if call.Function.Name != "" {
				entry.name = call.Function.Name
			}
			entry.arguments.WriteString(call.Function.Arguments)
		}
	}
	out := make([]any, 0, len(ordered))
	for _, entry := range ordered {
		kind := entry.kind
		if kind == "" {
			kind = "function"
		}
		out = append(out, map[string]any{
			"index": entry.index,
			"id":    entry.id,
			"type":  kind,
			"function": map[string]any{
				"name":      entry.name,
				"arguments": entry.arguments.String(),
			},
		})
	}
	return out
}
