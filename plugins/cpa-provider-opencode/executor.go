package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// pluginVersion is overridden at build time with -X main.pluginVersion=...
var pluginVersion = "0.1.0"

// Execute performs a non-streaming chat completion. Zen's free tier refuses a
// request that is not streaming, so the upstream call always streams and this
// path collects the events into one completion.
func (g *gateway) Execute(ctx context.Context, req pluginkit.ExecutorRequest) (pluginkit.ExecutorResponse, error) {
	plan, errPlan := g.impl.planFor(req)
	if errPlan != nil {
		return pluginkit.ExecutorResponse{}, errPlan
	}
	body, errPrepare := g.impl.prepareBody(req, plan)
	if errPrepare != nil {
		return pluginkit.ExecutorResponse{}, errPrepare
	}
	result, errCall := g.impl.callChat(ctx, plan, body)
	if errCall != nil {
		return pluginkit.ExecutorResponse{}, errCall
	}
	aggregated, errAggregate := pluginkit.AggregateStream(result.Body)
	if errAggregate != nil {
		return pluginkit.ExecutorResponse{}, errAggregate
	}
	return pluginkit.ExecutorResponse{
		Payload: aggregated,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// ExecuteStream performs a streaming chat completion, forwarding each upstream
// event to the client as it arrives.
//
// The upstream connection is opened synchronously so a rejected credential or
// parameter is still visible to the host's rotation and request-scoped error
// rules; once a 2xx body is streaming, the events are forwarded on a goroutine
// through the host stream so the upstream's timing survives.
func (g *gateway) ExecuteStream(ctx context.Context, req pluginkit.ExecutorRequest) (pluginkit.ExecutorStreamResponse, error) {
	plan, errPlan := g.impl.planFor(req)
	if errPlan != nil {
		return pluginkit.ExecutorStreamResponse{}, errPlan
	}
	body, errPrepare := g.impl.prepareBody(req, plan)
	if errPrepare != nil {
		return pluginkit.ExecutorStreamResponse{}, errPrepare
	}

	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" || !pluginkit.HostAvailable() {
		result, errCall := g.impl.callChat(ctx, plan, body)
		if errCall != nil {
			return pluginkit.ExecutorStreamResponse{}, errCall
		}
		return pluginkit.ExecutorStreamResponse{
			Headers: streamHeaders(),
			Chunks:  pluginkit.SplitSSE(result.Body),
		}, nil
	}

	stream, errOpen := g.impl.openChatStream(ctx, plan, body)
	if errOpen != nil {
		return pluginkit.ExecutorStreamResponse{}, errOpen
	}

	callback := pluginkit.NewHostCallback(req.HostCallbackID)
	go func() {
		defer stream.Close()
		if errForward := pluginkit.ForwardStream(callback, streamID, stream); errForward != nil {
			callback.CloseStream(streamID, errForward.Error())
			return
		}
		callback.CloseStream(streamID, "")
	}()

	return pluginkit.ExecutorStreamResponse{Headers: streamHeaders()}, nil
}

// streamHeaders is the header set every streamed reply carries.
func streamHeaders() http.Header {
	return http.Header{
		"Content-Type":  []string{"text/event-stream"},
		"Cache-Control": []string{"no-cache"},
	}
}

// CountTokens is not offered by the upstream; the host falls back to its own
// estimator.
func (g *gateway) CountTokens(context.Context, pluginkit.ExecutorRequest) (pluginkit.ExecutorResponse, error) {
	return pluginkit.ExecutorResponse{}, pluginkit.ErrUnhandled
}

// HttpRequest exposes raw HTTP bridging for diagnostics.
func (g *gateway) HttpRequest(ctx context.Context, req pluginkit.ExecutorHTTPRequest) (pluginkit.ExecutorHTTPResponse, error) {
	headers := clientHeaders(g.impl.version, g.impl.sessions.id("bridge"), g.impl.sessions.requestID())
	for key, values := range req.Headers {
		for _, value := range values {
			headers.Set(key, value)
		}
	}
	if key := keyOf(req.Attributes, req.StorageJSON); key != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
	result, errDo := pluginkit.HTTPDo(ctx, req.Method, req.URL, headers, req.Body)
	if errDo != nil {
		return pluginkit.ExecutorHTTPResponse{}, pluginkit.NewError("upstream_unavailable", errDo.Error(), http.StatusBadGateway)
	}
	return pluginkit.ExecutorHTTPResponse{
		StatusCode: result.StatusCode,
		Headers:    result.Header,
		Body:       result.Body,
	}, nil
}

// executionPlan is everything one request needs after routing resolved.
type executionPlan struct {
	tier    string
	baseURL string
	key     string
	model   string
}

// planFor resolves the tier, endpoint, credential and model for one request.
func (p *pluginState) planFor(req pluginkit.ExecutorRequest) (*executionPlan, error) {
	if req.AuthProvider != providerKey {
		return nil, pluginkit.NewError("invalid_credential", "selected auth provider is not opencode", http.StatusUnauthorized)
	}
	tier := tierOf(req.AuthAttributes, req.StorageJSON)
	plan := &executionPlan{tier: tier, model: req.Model, baseURL: freeBaseURL}
	if tier == tierGo {
		plan.baseURL = goBaseURL
		plan.key = keyOf(req.AuthAttributes, req.StorageJSON)
		if plan.key == "" && len(p.apiKeys) > 0 {
			plan.key = p.apiKeys[0]
		}
	}
	if plan.key == "" {
		plan.key = publicCredential
	}
	if plan.model == "" {
		return nil, pluginkit.NewError("invalid_request", "no model was requested", http.StatusBadRequest)
	}
	return plan, nil
}

// prepareBody rewrites the client payload into the shape Zen accepts.
//
// Three rewrites are unconditional because the free tier checks them on the
// wire: streaming is forced on, the session-bound tool declarations are
// completed, and the model id is the upstream id. The remaining rewrites keep
// the payload valid for the endpoint: an assistant turn that carries neither
// content nor tool calls is rejected, and tool-call ids must be non-empty.
func (p *pluginState) prepareBody(req pluginkit.ExecutorRequest, plan *executionPlan) ([]byte, error) {
	source := req.Payload
	if len(source) == 0 {
		source = req.OriginalRequest
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(source, &payload); errUnmarshal != nil {
		return nil, pluginkit.BadRequestError("decode chat payload: " + errUnmarshal.Error())
	}
	payload["model"] = plan.model
	payload["stream"] = true

	if rawTools, okTools := payload["tools"].([]any); okTools {
		payload["tools"] = ensureRequiredTools(rawTools)
	} else if plan.tier == tierFree {
		payload["tools"] = ensureRequiredTools(nil)
	}

	if messages, okMessages := payload["messages"].([]any); okMessages {
		for _, entry := range messages {
			message, okMessage := entry.(map[string]any)
			if !okMessage {
				continue
			}
			role, _ := message["role"].(string)
			if role != "assistant" {
				continue
			}
			content, _ := message["content"]
			toolCalls, hasToolCalls := message["tool_calls"].([]any)
			if !hasToolCalls || len(toolCalls) == 0 {
				// An assistant turn with no content is rejected upstream.
				if content == nil || content == "" {
					message["content"] = " "
				}
				continue
			}
			for _, call := range toolCalls {
				object, okObject := call.(map[string]any)
				if !okObject {
					continue
				}
				if id, _ := object["id"].(string); strings.TrimSpace(id) == "" {
					object["id"] = p.sessions.requestID()
				}
			}
		}
	}

	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, pluginkit.NewError("internal_error", "encode chat payload: "+errMarshal.Error(), http.StatusInternalServerError)
	}
	return encoded, nil
}

// callChat posts a prepared body and returns the buffered response.
func (p *pluginState) callChat(ctx context.Context, plan *executionPlan, body []byte) (*pluginkit.HTTPResult, error) {
	headers := p.requestHeaders(plan, "dsh")
	result, errDo := pluginkit.HTTPDo(ctx, http.MethodPost, plan.baseURL+"/chat/completions", headers, body)
	if errDo != nil {
		return nil, pluginkit.NewError("upstream_unavailable", errDo.Error(), http.StatusBadGateway)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return nil, classifyUpstreamError(result.StatusCode, result.Body)
	}
	return result, nil
}

// openChatStream opens the upstream as a live reader. A non-2xx head is fully
// known before any body byte, so it is read and classified here.
func (p *pluginState) openChatStream(ctx context.Context, plan *executionPlan, body []byte) (*pluginkit.HTTPStream, error) {
	headers := p.requestHeaders(plan, "dsh")
	stream, errOpen := pluginkit.OpenHTTPStreamAmbient(ctx, http.MethodPost, plan.baseURL+"/chat/completions", headers, body)
	if errOpen != nil {
		return nil, pluginkit.NewError("upstream_unavailable", errOpen.Error(), http.StatusBadGateway)
	}
	if stream.StatusCode >= 200 && stream.StatusCode < 300 {
		return stream, nil
	}
	raw, errRead := pluginkit.ReadStreamBody(stream, 0)
	status := stream.StatusCode
	stream.Close()
	if errRead != nil {
		return nil, pluginkit.NewError("upstream_unavailable", errRead.Error(), http.StatusBadGateway)
	}
	return nil, classifyUpstreamError(status, raw)
}

// requestHeaders builds the headers for one chat call, including the
// fingerprint the free tier gates on.
func (p *pluginState) requestHeaders(plan *executionPlan, seed string) http.Header {
	session := p.sessions.id(seed)
	headers := clientHeaders(p.version, session, p.sessions.requestID())
	if plan.tier == tierGo {
		headers.Set("Authorization", "Bearer "+plan.key)
	} else {
		headers.Set("Authorization", "Bearer "+publicCredential)
	}
	return headers
}

// classifyUpstreamError maps an upstream failure onto a client-visible error
// with the right status.
//
// Zen reports non-credential refusals (an ended promotion, a region block, a
// free-tier fingerprint mismatch) as 401 or 403. Reporting those as AUTH is
// wrong, so FreeTierError and UpgradeRequired are surfaced as themselves.
func classifyUpstreamError(status int, body []byte) *pluginkit.PluginError {
	text := string(body)
	switch {
	case strings.Contains(text, "FreeTierError"):
		return pluginkit.NewError("free_tier_refused",
			"OpenCode refused the free tier: the request fingerprint was rejected. "+pluginkit.TruncateBody(body, 0), http.StatusForbidden)
	case strings.Contains(text, "UpgradeRequired"):
		return pluginkit.NewError("client_version_too_old",
			"OpenCode requires a newer CLI version for the free tier. "+pluginkit.TruncateBody(body, 0), http.StatusForbidden)
	case status == http.StatusTooManyRequests:
		return pluginkit.NewError("rate_limited", "rate limited: "+pluginkit.TruncateBody(body, 0), http.StatusTooManyRequests)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return pluginkit.NewError("invalid_credential", "the credential or tier was rejected: "+pluginkit.TruncateBody(body, 0), status)
	case status >= 500:
		return pluginkit.NewError("upstream_unavailable", fmt.Sprintf("upstream error HTTP %d: %s", status, pluginkit.TruncateBody(body, 0)), http.StatusBadGateway)
	default:
		return pluginkit.NewError("upstream_error", fmt.Sprintf("upstream returned HTTP %d: %s", status, pluginkit.TruncateBody(body, 0)), status)
	}
}
