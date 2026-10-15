package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// Execute performs a non-streaming chat completion. Zen's free tier refuses a
// request that is not streaming, so the upstream call always streams and this
// path collects the events into one completion.
func (g *gateway) Execute(ctx context.Context, req cpakit.ExecutorRequest) (cpakit.ExecutorResponse, error) {
	plan, errPlan := g.impl.planFor(ctx, req)
	if errPlan != nil {
		return cpakit.ExecutorResponse{}, errPlan
	}
	body, errPrepare := g.impl.prepareBody(req, plan)
	if errPrepare != nil {
		return cpakit.ExecutorResponse{}, errPrepare
	}
	result, errCall := g.impl.callChat(ctx, req, plan, body)
	if errCall != nil {
		return cpakit.ExecutorResponse{}, errCall
	}
	aggregated, errAggregate := cpakit.AggregateStream(result.Body)
	if errAggregate != nil {
		return cpakit.ExecutorResponse{}, errAggregate
	}
	return cpakit.ExecutorResponse{
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
func (g *gateway) ExecuteStream(ctx context.Context, req cpakit.ExecutorRequest, streamID string, hostCallbackID string) (cpakit.ExecutorStreamResponse, error) {
	plan, errPlan := g.impl.planFor(ctx, req)
	if errPlan != nil {
		return cpakit.ExecutorStreamResponse{}, errPlan
	}
	body, errPrepare := g.impl.prepareBody(req, plan)
	if errPrepare != nil {
		return cpakit.ExecutorStreamResponse{}, errPrepare
	}

	// Without a host stream there is nowhere to emit progressively, so fall back
	// to the buffered reply rather than dropping output.
	if strings.TrimSpace(streamID) == "" {
		result, errCall := g.impl.callChat(ctx, req, plan, body)
		if errCall != nil {
			return cpakit.ExecutorStreamResponse{}, errCall
		}
		return cpakit.BufferedStream(cpakit.StreamHeaders(), cpakit.SplitSSE(result.Body)), nil
	}

	callback := cpakit.NewHostCallback(hostCallbackID)
	stream, errOpen := g.impl.openChatStream(ctx, req, callback, plan, body)
	if errOpen != nil {
		return cpakit.ExecutorStreamResponse{}, errOpen
	}

	go func() {
		defer stream.Close()
		if errForward := cpakit.ForwardStream(callback, streamID, stream); errForward != nil {
			callback.CloseStream(streamID, cpakit.Redact(errForward.Error()))
			return
		}
		callback.CloseStream(streamID, "")
	}()

	return cpakit.ExecutorStreamResponse{Headers: cpakit.StreamHeaders()}, nil
}

// CountTokens is not offered by the upstream; the host falls back to its own
// estimator.
func (g *gateway) CountTokens(context.Context, cpakit.ExecutorRequest) (cpakit.ExecutorResponse, error) {
	return cpakit.ExecutorResponse{}, cpakit.ErrUnhandled
}

// HttpRequest exposes raw HTTP bridging for diagnostics.
func (g *gateway) HttpRequest(ctx context.Context, req cpakit.ExecutorHTTPRequest) (cpakit.ExecutorHTTPResponse, error) {
	headers := clientHeaders(g.impl.version, g.impl.sessions.id("bridge"), g.impl.sessions.requestID())
	for key, values := range req.Headers {
		for _, value := range values {
			headers.Set(key, value)
		}
	}
	if key := keyOf(req.Attributes, req.StorageJSON); key != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
	result, errDo := cpakit.HTTPDo(ctx, req.Method, req.URL, headers, req.Body)
	if errDo != nil {
		return cpakit.ExecutorHTTPResponse{}, cpakit.NewError("upstream_unavailable", errDo.Error(), http.StatusBadGateway)
	}
	return cpakit.ExecutorHTTPResponse{
		StatusCode: result.StatusCode,
		Headers:    result.Header,
		Body:       result.Body,
	}, nil
}

// upstreamSessionHeaders are the inbound headers that identify one downstream
// conversation. The first present value wins, in the order OpenCode's own
// clients and the harnesses that drive them emit them.
var upstreamSessionHeaders = []string{
	"X-Session-Affinity",
	"X-Opencode-Session",
	"X-Session-Id",
	"X-Claude-Code-Session-Id",
	"Session-Id",
}

// conversationSeed resolves the identity that must stay constant for every turn
// of one conversation.
//
// Zen routes its prompt cache by the session identifier and meters free-tier
// usage per session, so a per-request value would defeat the cache and fragment
// the meter. The precedence mirrors the reference provider: the host's canonical
// session identity first, then an explicit inbound header, then a digest of the
// conversation's opening user message, which keeps a client that sends no
// session header stable across its turns.
func conversationSeed(req cpakit.ExecutorRequest) string {
	if req.Metadata != nil {
		if id, okID := req.Metadata["canonical_session_id"].(string); okID {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				return "canonical:" + trimmed
			}
		}
	}
	for _, name := range upstreamSessionHeaders {
		if value := strings.TrimSpace(req.Headers.Get(name)); value != "" {
			return "header:" + value
		}
	}
	return "content:" + openingMessageDigest(req)
}

// openingMessageDigest hashes the first user message of the conversation, which
// is the part of the payload that does not change as the history grows.
func openingMessageDigest(req cpakit.ExecutorRequest) string {
	source := req.Payload
	if len(source) == 0 {
		source = req.OriginalRequest
	}
	var payload struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"input"`
	}
	if errUnmarshal := json.Unmarshal(source, &payload); errUnmarshal != nil {
		return "unavailable"
	}
	for _, message := range payload.Messages {
		if message.Role == "user" && len(message.Content) > 0 {
			digest := sha256.Sum256(message.Content)
			return hex.EncodeToString(digest[:16])
		}
	}
	for _, item := range payload.Input {
		if item.Role == "user" && len(item.Content) > 0 {
			digest := sha256.Sum256(item.Content)
			return hex.EncodeToString(digest[:16])
		}
	}
	return "unavailable"
}

// executionPlan is everything one request needs after routing resolved.
type executionPlan struct {
	tier    string
	baseURL string
	key     string
	model   string
	// orgID is the workspace the free tier is metered against. It travels in its
	// own header, not in the credential.
	orgID string
}

// planFor resolves the tier, endpoint, credential and model for one request.
func (p *pluginState) planFor(ctx context.Context, req cpakit.ExecutorRequest) (*executionPlan, error) {
	if req.AuthProvider != providerKey {
		return nil, cpakit.NewError("invalid_credential", "selected auth provider is not opencode", http.StatusUnauthorized)
	}
	tier := tierOf(req.AuthAttributes, req.StorageJSON)
	plan := &executionPlan{tier: tier, model: req.Model, baseURL: p.freeRoot()}
	if tier == tierGo {
		plan.baseURL = p.goRoot()
		plan.key = keyOf(req.AuthAttributes, req.StorageJSON)
		if plan.key == "" && len(p.apiKeys) > 0 {
			plan.key = p.apiKeys[0]
		}
	}
	if tier == tierFree {
		// The free tier stopped accepting the anonymous "public" credential. The
		// account session token and its workspace id are the accepted pair; when
		// neither is configured the request keeps the old credential so the
		// upstream refusal names the real cause instead of failing locally.
		token, orgID, errAccount := p.account.credentials(ctx)
		if errAccount != nil {
			return nil, errAccount
		}
		plan.key = token
		plan.orgID = orgID
		if strings.TrimSpace(token) == "" {
			cpakit.HostLog(ctx, "warn",
				"opencode: no account credential configured; the free tier refuses the anonymous credential with FreeTierError")
		}
	}
	if plan.key == "" {
		plan.key = publicCredential
	}
	if plan.model == "" {
		return nil, cpakit.NewError("invalid_request", "no model was requested", http.StatusBadRequest)
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
func (p *pluginState) prepareBody(req cpakit.ExecutorRequest, plan *executionPlan) ([]byte, error) {
	source := req.Payload
	if len(source) == 0 {
		source = req.OriginalRequest
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(source, &payload); errUnmarshal != nil {
		return nil, cpakit.BadRequestError("decode chat payload: " + errUnmarshal.Error())
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
		return nil, cpakit.NewError("internal_error", "encode chat payload: "+errMarshal.Error(), http.StatusInternalServerError)
	}
	cpakit.HostLog(context.Background(), "info", fmt.Sprintf("opencode: outbound body=%s", cpakit.TruncateBody(encoded, 600)))
	return encoded, nil
}

// callChat posts a prepared body and returns the buffered response.
func (p *pluginState) callChat(ctx context.Context, req cpakit.ExecutorRequest, plan *executionPlan, body []byte) (*cpakit.HTTPResult, error) {
	headers := p.requestHeaders(plan, conversationSeed(req))
	result, errDo := cpakit.HTTPDo(ctx, http.MethodPost, plan.baseURL+"/chat/completions", headers, body)
	if errDo != nil {
		return nil, cpakit.NewError("upstream_unavailable", errDo.Error(), http.StatusBadGateway)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return nil, classifyUpstreamError(result.StatusCode, result.Body)
	}
	return result, nil
}

// openChatStream opens the upstream as a live reader. A non-2xx head is fully
// known before any body byte, so it is read and classified here.
func (p *pluginState) openChatStream(ctx context.Context, req cpakit.ExecutorRequest, callback cpakit.HostCallback, plan *executionPlan, body []byte) (*cpakit.HTTPStream, error) {
	headers := p.requestHeaders(plan, conversationSeed(req))
	stream, errOpen := callback.OpenHTTPStream(ctx, http.MethodPost, plan.baseURL+"/chat/completions", headers, body)
	if errOpen != nil {
		return nil, cpakit.NewError("upstream_unavailable", errOpen.Error(), http.StatusBadGateway)
	}
	if stream.StatusCode >= 200 && stream.StatusCode < 300 {
		return stream, nil
	}
	raw, errRead := cpakit.ReadStreamBody(stream, 0)
	status := stream.StatusCode
	stream.Close()
	if errRead != nil {
		return nil, cpakit.NewError("upstream_unavailable", errRead.Error(), http.StatusBadGateway)
	}
	return nil, classifyUpstreamError(status, raw)
}

// requestHeaders builds the headers for one chat call, including the
// fingerprint the free tier gates on.
func (p *pluginState) requestHeaders(plan *executionPlan, seed string) http.Header {
	session := p.sessions.id(seed)
	request := p.sessions.requestID()
	headers := clientHeaders(p.version, session, request)
	cpakit.HostLog(context.Background(), "info", fmt.Sprintf(
		"opencode: outbound tier=%s url=%s ua=%s session=%s request=%s",
		plan.tier, plan.baseURL+"/chat/completions", "opencode/"+p.version, session, request))
	headers.Set("Authorization", "Bearer "+plan.key)
	if strings.TrimSpace(plan.orgID) != "" {
		// The free tier meters against a workspace and requires this header. The
		// paid tier ignores it, but it is only ever set for the free tier.
		headers.Set(orgHeader, plan.orgID)
	}
	return headers
}

// classifyUpstreamError maps an upstream failure onto a client-visible error
// with the right status.
//
// Zen reports non-credential refusals (an ended promotion, a region block, a
// free-tier fingerprint mismatch) as 401 or 403. Reporting those as AUTH is
// wrong, so FreeTierError and UpgradeRequired are surfaced as themselves.
func classifyUpstreamError(status int, body []byte) *cpakit.PluginError {
	text := string(body)
	switch {
	case strings.Contains(text, "FreeTierError"):
		return cpakit.NewError("free_tier_refused",
			"OpenCode refused the free tier: the request fingerprint was rejected. "+cpakit.TruncateBody(body, 0), http.StatusForbidden)
	case strings.Contains(text, "UpgradeRequired"):
		return cpakit.NewError("client_version_too_old",
			"OpenCode requires a newer CLI version for the free tier. "+cpakit.TruncateBody(body, 0), http.StatusForbidden)
	case status == http.StatusTooManyRequests:
		return cpakit.NewError("rate_limited", "rate limited: "+cpakit.TruncateBody(body, 0), http.StatusTooManyRequests)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return cpakit.NewError("invalid_credential", "the credential or tier was rejected: "+cpakit.TruncateBody(body, 0), status)
	case status >= 500:
		return cpakit.NewError("upstream_unavailable", fmt.Sprintf("upstream error HTTP %d: %s", status, cpakit.TruncateBody(body, 0)), http.StatusBadGateway)
	default:
		return cpakit.NewError("upstream_error", fmt.Sprintf("upstream returned HTTP %d: %s", status, cpakit.TruncateBody(body, 0)), status)
	}
}
