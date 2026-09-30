package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// Execute performs a non-streaming chat completion.
//
// The upstream only speaks streaming, so this buffers the stream and returns
// the final assistant message as one OpenAI chat completion.
func (g *gateway) Execute(ctx context.Context, req pluginkit.ExecutorRequest) (pluginkit.ExecutorResponse, error) {
	kind, creds, errCreds := g.impl.resolveFor(ctx, req)
	if errCreds != nil {
		return pluginkit.ExecutorResponse{}, errCreds
	}
	body, errPrepare := g.impl.prepareBody(kind, req.Payload, req.OriginalRequest)
	if errPrepare != nil {
		return pluginkit.ExecutorResponse{}, errPrepare
	}
	result, errCall := g.impl.callChat(ctx, kind, creds, nil, body)
	if errCall != nil {
		return pluginkit.ExecutorResponse{}, errCall
	}
	aggregated, errAggregate := aggregateStream(result.Body)
	if errAggregate != nil {
		return pluginkit.ExecutorResponse{}, errAggregate
	}
	return pluginkit.ExecutorResponse{
		Payload: aggregated,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// ExecuteStream performs a streaming chat completion, re-emitting the upstream
// SSE as the bare JSON chunks the host frames.
func (g *gateway) ExecuteStream(ctx context.Context, req pluginkit.ExecutorRequest) (pluginkit.ExecutorStreamResponse, error) {
	kind, creds, errCreds := g.impl.resolveFor(ctx, req)
	if errCreds != nil {
		return pluginkit.ExecutorStreamResponse{}, errCreds
	}
	body, errPrepare := g.impl.prepareBody(kind, req.Payload, req.OriginalRequest)
	if errPrepare != nil {
		return pluginkit.ExecutorStreamResponse{}, errPrepare
	}
	result, errCall := g.impl.callChat(ctx, kind, creds, nil, body)
	if errCall != nil {
		return pluginkit.ExecutorStreamResponse{}, errCall
	}
	return pluginkit.ExecutorStreamResponse{
		Headers: http.Header{
			"Content-Type":  []string{"text/event-stream"},
			"Cache-Control": []string{"no-cache"},
		},
		Chunks: splitSSE(result.Body),
	}, nil
}

// CountTokens is not offered by the upstream; the host falls back to its own
// estimator.
func (g *gateway) CountTokens(context.Context, pluginkit.ExecutorRequest) (pluginkit.ExecutorResponse, error) {
	return pluginkit.ExecutorResponse{}, pluginkit.ErrUnhandled
}

// HttpRequest exposes raw HTTP bridging for diagnostics.
func (g *gateway) HttpRequest(ctx context.Context, req pluginkit.ExecutorHTTPRequest) (pluginkit.ExecutorHTTPResponse, error) {
	kind := kindForRecord(req.StorageJSON, req.Attributes)
	creds, errCreds := g.impl.resolve(ctx, kind)
	if errCreds != nil {
		return pluginkit.ExecutorHTTPResponse{}, errCreds
	}
	headers := req.Headers
	if headers == nil {
		headers = http.Header{}
	}
	if creds.APIKey != "" {
		headers.Set("Authorization", "Bearer "+creds.APIKey)
	} else {
		headers.Set("Authorization", "Bearer "+creds.AccessToken)
	}
	result, errDo := pluginkit.HTTPDo(ctx, req.Method, req.URL, headers, req.Body)
	if errDo != nil {
		return pluginkit.ExecutorHTTPResponse{}, errDo
	}
	return pluginkit.ExecutorHTTPResponse{StatusCode: result.StatusCode, Headers: result.Header, Body: result.Body}, nil
}

// resolveFor picks the credential an executor request should use. The request
// carries the auth record's storage payload; a missing kind falls back to the
// desktop CN credential, which is the long-standing default. For the CodeBuddy
// kind the candidates are reordered by the key rotator so multiple API keys
// rotate on 401/403/429/5xx.
func (p *pluginState) resolveFor(ctx context.Context, req pluginkit.ExecutorRequest) (*credentialKind, *credentials, error) {
	kind := kindForRecord(req.StorageJSON, req.AuthAttributes)
	if kind.id != kindCodebuddyID {
		creds, errCreds := p.resolve(ctx, kind)
		if errCreds != nil {
			return nil, nil, errCreds
		}
		return kind, creds, nil
	}
	_, all, errAll := p.resolveAll(ctx, kind)
	if errAll != nil {
		return nil, nil, errAll
	}
	ordered := p.rotator.ordered(all)
	return kind, ordered[0], nil
}

// prepareBody normalises a client chat request into the upstream's dialect.
//
// Every family behind this provider rejects non-streaming requests, so stream
// is always forced on; the other rewrites follow what each gateway refuses.
func (p *pluginState) prepareBody(kind *credentialKind, payload, original []byte) ([]byte, error) {
	raw := payload
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = original
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, pluginkit.BadRequestError("no request body was provided to the executor")
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return nil, pluginkit.BadRequestError("decode chat request: " + errUnmarshal.Error())
	}

	if messages, ok := doc["messages"].([]any); ok {
		// A developer-role message is rejected outright (error 11128).
		for _, entry := range messages {
			message, okMessage := entry.(map[string]any)
			if !okMessage {
				continue
			}
			if role, _ := message["role"].(string); role == "developer" {
				message["role"] = "system"
			}
		}
		// The international product additionally requires a leading system
		// message.
		if kind.region == "global" {
			doc["messages"] = ensureLeadingSystem(messages)
		}
	}

	doc["stream"] = true

	if value, ok := doc["tool_choice"]; ok {
		resolved, keepTools := flattenToolChoice(value)
		if resolved == "" {
			delete(doc, "tool_choice")
			if !keepTools {
				delete(doc, "tools")
				delete(doc, "functions")
			}
		} else {
			doc["tool_choice"] = resolved
		}
	}

	// The international product rejects reasoning_effort "off".
	if kind.region == "global" {
		if effort, _ := doc["reasoning_effort"].(string); effort == "off" {
			delete(doc, "reasoning_effort")
		}
	}

	encoded, errMarshal := json.Marshal(doc)
	if errMarshal != nil {
		return nil, pluginkit.NewError("internal_error", errMarshal.Error())
	}
	return encoded, nil
}

func ensureLeadingSystem(messages []any) []any {
	if len(messages) == 0 {
		return messages
	}
	if first, ok := messages[0].(map[string]any); ok {
		if role, _ := first["role"].(string); role == "system" {
			return messages
		}
	}
	prefixed := make([]any, 0, len(messages)+1)
	prefixed = append(prefixed, map[string]any{"role": "system", "content": "You are a helpful assistant."})
	return append(prefixed, messages...)
}

// flattenToolChoice converts the object form of tool_choice into the string the
// upstream accepts. The boolean reports whether the tool list stays valid.
func flattenToolChoice(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		if typed == "none" {
			return "", false
		}
		return typed, true
	case map[string]any:
		kindName, _ := typed["type"].(string)
		switch kindName {
		case "auto", "required":
			return kindName, true
		case "function":
			if function, okFunction := typed["function"].(map[string]any); okFunction {
				if name, _ := function["name"].(string); name != "" {
					return name, true
				}
			}
			return "", false
		default:
			return "", false
		}
	default:
		return "", false
	}
}

// callChat posts a prepared body to this credential's chat endpoint.
//
// The desktop kinds send the WorkBuddy identity header group in one shot —
// those gateways reject the CLI shape. The CodeBuddy kind sends its own CLI
// header group and rotates across every configured key on the statuses that
// say "this key cannot serve the request" (401/403/429/5xx) and on network
// errors; a business rejection is returned as-is, and the last candidate's
// failure is never retried.
func (p *pluginState) callChat(ctx context.Context, kind *credentialKind, creds *credentials, extra []*credentials, body []byte) (*pluginkit.HTTPResult, error) {
	if kind.id != kindCodebuddyID || (len(extra) <= 1 && creds.Mode != string(modeAPIKey)) {
		headers := p.chatHeadersFor(kind, creds)
		result, errDo := pluginkit.HTTPDo(ctx, http.MethodPost, creds.chatURL(kind), headers, body)
		if errDo != nil {
			return nil, pluginkit.NewError("upstream_unavailable", errDo.Error(), http.StatusBadGateway)
		}
		if result.StatusCode < 200 || result.StatusCode >= 300 {
			return nil, classifyUpstreamError(result.StatusCode, result.Body)
		}
		return result, nil
	}

	candidates := append([]*credentials{creds}, extra...)
	ordered := p.rotator.ordered(dedupeCredentials(candidates))
	endpoint := creds.chatURL(kind)
	for index, cred := range ordered {
		last := index == len(ordered)-1
		result, errDo := pluginkit.HTTPDo(ctx, http.MethodPost, endpoint, codebuddyChatHeaders(cred), body)
		if errDo != nil {
			if last {
				return nil, pluginkit.NewError("upstream_unavailable", "chat request failed: "+errDo.Error(), http.StatusBadGateway)
			}
			p.rotator.penalize(cred)
			pluginkit.HostLog(ctx, "warn", "tencent: chat candidate failed at the network layer; trying the next key")
			continue
		}
		if result.StatusCode >= 200 && result.StatusCode < 300 {
			return result, nil
		}
		if !last && shouldRotateCredential(result.StatusCode) {
			p.rotator.penalize(cred)
			pluginkit.HostLog(ctx, "warn", "tencent: chat candidate rejected; trying the next key")
			continue
		}
		return nil, classifyUpstreamError(result.StatusCode, result.Body)
	}
	return nil, pluginkit.NewError("upstream_unavailable", "every credential candidate failed", http.StatusBadGateway)
}

// dedupeCredentials drops duplicate candidates by rotation key, keeping the
// first occurrence, so a repeated key is not penalised twice.
func dedupeCredentials(creds []*credentials) []*credentials {
	seen := map[string]struct{}{}
	out := make([]*credentials, 0, len(creds))
	for _, cred := range creds {
		if cred == nil {
			continue
		}
		key := cred.rotationKey()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, cred)
	}
	return out
}

// codebuddyChatHeaders builds the header set for one CodeBuddy chat completion:
// the CLI identity plus the credential and account headers. (From the verified
// CodeBuddy plugin.)
func codebuddyChatHeaders(cred *credentials) http.Header {
	headers := credentialHeaders(cred)
	headers.Set("Content-Type", "application/json")
	return headers
}

// chatHeadersFor dispatches on the credential kind: the CodeBuddy family
// presents the CLI identity, while the desktop products present their app
// identity. Sending one group for the other kind is rejected on the catalog
// and misattributes billing on chat.
func (p *pluginState) chatHeadersFor(kind *credentialKind, creds *credentials) http.Header {
	if !kind.desktop {
		return codebuddyChatHeaders(creds)
	}
	return desktopChatHeaders(kind, creds, p.appVersion(kind), cliVersionFromBundle(kind))
}

// classifyUpstreamError maps an upstream failure onto a client-visible error
// with the right HTTP status, so a credential or quota problem is not reported
// as a gateway fault.
func classifyUpstreamError(status int, body []byte) *pluginkit.PluginError {
	text := string(body)
	code := gjsonInt(body, "code")
	switch {
	case status == 402 || containsAny(text, "积分不足", "额度不足", "insufficient credit", "credit not enough"):
		return pluginkit.NewError("insufficient_quota", "credit exhausted: "+truncateBody(body), http.StatusForbidden)
	case status == 401 || containsAny(text, "Offline user session not found", "12153"):
		return pluginkit.NewError("invalid_credential", "the session is no longer valid: "+truncateBody(body), http.StatusUnauthorized)
	case code == 11128:
		return pluginkit.NewError("invalid_request", "upstream rejected a developer-role message: "+truncateBody(body), http.StatusBadRequest)
	case code == 12403:
		return pluginkit.NewError("invalid_request", "upstream rejected the client identity: "+truncateBody(body), http.StatusBadRequest)
	case code == 11133:
		return pluginkit.NewError("invalid_request", "upstream rejected max_tokens or reasoning effort: "+truncateBody(body), http.StatusBadRequest)
	case status == 429:
		return pluginkit.NewError("rate_limited", "rate limited: "+truncateBody(body), http.StatusTooManyRequests)
	case status >= 500:
		return pluginkit.NewError("upstream_unavailable", "upstream error: "+truncateBody(body), http.StatusBadGateway)
	default:
		return pluginkit.NewError("upstream_error", fmt.Sprintf("upstream returned HTTP %d: %s", status, truncateBody(body)), status)
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if bytes.Contains([]byte(haystack), []byte(needle)) {
			return true
		}
	}
	return false
}

// gjsonInt extracts an integer at a top-level or nested "code" key.
func gjsonInt(body []byte, key string) int {
	var probe map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &probe); errUnmarshal != nil {
		return 0
	}
	raw, ok := probe[key]
	if !ok {
		if data, hasData := probe["data"]; hasData {
			var inner map[string]json.RawMessage
			if errInner := json.Unmarshal(data, &inner); errInner == nil {
				raw = inner[key]
			}
		}
	}
	if len(raw) == 0 {
		return 0
	}
	var value int
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
		return 0
	}
	return value
}
