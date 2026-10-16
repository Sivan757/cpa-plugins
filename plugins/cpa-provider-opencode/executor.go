package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// Each upstream family reports its own stream shape, so the buffered answer is
	// built by the translation that matches the protocol that was used.
	var aggregated []byte
	var errAggregate error
	if plan.protocol == protocolResponses {
		aggregated, errAggregate = responsesCompletionFromStream(result.Body, plan.model)
	} else {
		aggregated, errAggregate = cpakit.AggregateStream(result.Body)
	}
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

	translator := newResponsesTranslator(plan.model)
	go func() {
		defer stream.Close()
		var errForward error
		if plan.protocol == protocolResponses {
			// The upstream speaks the Responses protocol and the client speaks
			// chat-completions, so each event is converted as it arrives.
			errForward = cpakit.ForwardStreamTransform(callback, streamID, stream, translator.Translate)
		} else {
			errForward = cpakit.ForwardStream(callback, streamID, stream)
		}
		if errForward != nil {
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
// executionLane is one credential the free tier accepts. A lane is tried in
// order and the next one covers the failure of the one before it.
//
// The tier accepts two: the keyless "public" credential, which every OpenCode
// install carries, and an account session token metered against one workspace.
// They are not interchangeable on the wire: the keyless lane refuses the
// x-opencode-request header that the account lane sends.
type executionLane struct {
	// label names the lane in logs.
	label string
	// key is the bearer credential.
	key string
	// orgID is the workspace an account lane is metered against. It travels in
	// its own header, not in the credential.
	orgID string
	// anonymous marks the keyless lane, which omits x-opencode-request.
	anonymous bool
}

type executionPlan struct {
	tier    string
	baseURL string
	key     string
	model   string
	// orgID is the workspace the free tier is metered against. It travels in its
	// own header, not in the credential.
	orgID string
	// protocol is the upstream request and response family: chat-completions for
	// most models, responses for the ones the tier serves only that way.
	protocol string
	// endpoint is the full upstream URL for this request.
	endpoint string
	// lanes are the credentials to try, in order. The first is preferred.
	lanes []executionLane
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
		// The keyless lane is preferred. It needs no account, so there is no
		// credential to rotate and no workspace to burn, and an account that
		// shares this tier no longer takes the traffic that gets it flagged.
		plan.lanes = append(plan.lanes, executionLane{
			label:     "anonymous",
			key:       publicCredential,
			anonymous: true,
		})

		// The account lane covers the keyless lane's failures: its quota is per
		// workspace rather than per source address, so it survives an exhausted
		// shared address.
		token, orgID, errAccount := p.account.credentials(ctx)
		if errAccount != nil {
			return nil, errAccount
		}
		if strings.TrimSpace(token) != "" {
			plan.lanes = append(plan.lanes, executionLane{
				label: "account",
				key:   token,
				orgID: orgID,
			})
		} else {
			cpakit.HostLog(ctx, "warn",
				"opencode: no account credential configured; the free tier has only the keyless lane")
		}
	}
	if len(plan.lanes) == 0 {
		plan.lanes = append(plan.lanes, executionLane{label: "key", key: plan.key})
	}
	plan.key = plan.lanes[0].key
	plan.orgID = plan.lanes[0].orgID
	if plan.model == "" {
		return nil, cpakit.NewError("invalid_request", "no model was requested", http.StatusBadRequest)
	}

	// The tier serves some models only on the Responses API. Route those to their
	// own endpoint and protocol; everything else keeps the chat path.
	plan.protocol = protocolChat
	if usesResponsesAPI(plan.model) {
		plan.protocol = protocolResponses
		plan.endpoint = p.responsesRoot()
	} else {
		plan.endpoint = plan.baseURL + "/chat/completions"
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

	if plan.protocol == protocolResponses {
		converted, errConvert := chatToResponses(payload, plan.model)
		if errConvert != nil {
			return nil, errConvert
		}
		payload = converted
	}

	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, cpakit.NewError("internal_error", "encode chat payload: "+errMarshal.Error(), http.StatusInternalServerError)
	}
	cpakit.HostLog(context.Background(), "info", fmt.Sprintf("opencode: outbound protocol=%s body=%s",
		plan.protocol, cpakit.TruncateBody(encoded, 600)))
	return encoded, nil
}

// callChat posts a prepared body and returns the buffered response.
//
// Each lane is tried in order. A lane that answers non-2xx gives way to the next
// one, because the lanes fail for different reasons: the keyless lane is metered
// per source address, and the account lane per workspace.
func (p *pluginState) callChat(ctx context.Context, req cpakit.ExecutorRequest, plan *executionPlan, body []byte) (*cpakit.HTTPResult, error) {
	if until, blocked := p.limit.remaining(time.Now()); blocked {
		return nil, freeUsageLimitError(until)
	}
	seed := conversationSeed(req)
	var lastError error
	var limitUntil time.Time
	drained := len(plan.lanes) > 0
	for _, lane := range plan.lanes {
		headers := p.requestHeaders(plan, lane, seed)
		result, errDo := cpakit.HTTPDo(ctx, http.MethodPost, plan.endpoint, headers, body)
		if errDo != nil {
			drained = false
			lastError = cpakit.NewError("upstream_unavailable", errDo.Error(), http.StatusBadGateway)
			cpakit.HostLog(ctx, "warn", "opencode: lane "+lane.label+" transport failed: "+cpakit.Redact(errDo.Error()))
			continue
		}
		if result.StatusCode >= 200 && result.StatusCode < 300 {
			p.limit.clear()
			return result, nil
		}
		if isFreeUsageLimit(result.StatusCode, result.Body) {
			// The allowance is shared, but the next lane is still tried once: a
			// lane with its own allowance would answer here, and only when every
			// lane reports the drained state is it recorded.
			if until := freeUsageLimitUntil(result.Header, time.Now()); until.After(limitUntil) {
				limitUntil = until
			}
			lastError = freeUsageLimitError(limitUntil)
		} else {
			drained = false
			lastError = classifyUpstreamError(result.StatusCode, result.Body)
		}
		cpakit.HostLog(ctx, "warn", fmt.Sprintf("opencode: lane %s answered HTTP %d", lane.label, result.StatusCode))
	}
	if drained && !limitUntil.IsZero() {
		p.limit.record(limitUntil)
		cpakit.HostLog(ctx, "warn", "opencode: free tier allowance drained until "+limitUntil.UTC().Format(time.RFC3339)+"; upstream calls are paused")
	}
	return nil, lastError
}

// openChatStream opens the upstream as a live reader. A non-2xx head is fully
// known before any body byte, so it is read and classified here.
func (p *pluginState) openChatStream(ctx context.Context, req cpakit.ExecutorRequest, callback cpakit.HostCallback, plan *executionPlan, body []byte) (*cpakit.HTTPStream, error) {
	if until, blocked := p.limit.remaining(time.Now()); blocked {
		return nil, freeUsageLimitError(until)
	}
	seed := conversationSeed(req)
	var lastError error
	var limitUntil time.Time
	drained := len(plan.lanes) > 0
	for _, lane := range plan.lanes {
		headers := p.requestHeaders(plan, lane, seed)
		stream, errOpen := callback.OpenHTTPStream(ctx, http.MethodPost, plan.endpoint, headers, body)
		if errOpen != nil {
			drained = false
			lastError = cpakit.NewError("upstream_unavailable", errOpen.Error(), http.StatusBadGateway)
			cpakit.HostLog(ctx, "warn", "opencode: lane "+lane.label+" transport failed: "+cpakit.Redact(errOpen.Error()))
			continue
		}
		if stream.StatusCode >= 200 && stream.StatusCode < 300 {
			p.limit.clear()
			return stream, nil
		}
		raw, errRead := cpakit.ReadStreamBody(stream, 0)
		status := stream.StatusCode
		header := stream.Header
		stream.Close()
		if errRead != nil {
			drained = false
			lastError = cpakit.NewError("upstream_unavailable", errRead.Error(), http.StatusBadGateway)
			cpakit.HostLog(ctx, "warn", fmt.Sprintf("opencode: lane %s body read failed", lane.label))
			continue
		}
		if isFreeUsageLimit(status, raw) {
			if until := freeUsageLimitUntil(header, time.Now()); until.After(limitUntil) {
				limitUntil = until
			}
			lastError = freeUsageLimitError(limitUntil)
		} else {
			drained = false
			lastError = classifyUpstreamError(status, raw)
		}
		cpakit.HostLog(ctx, "warn", fmt.Sprintf("opencode: lane %s answered HTTP %d", lane.label, status))
	}
	if drained && !limitUntil.IsZero() {
		p.limit.record(limitUntil)
		cpakit.HostLog(ctx, "warn", "opencode: free tier allowance drained until "+limitUntil.UTC().Format(time.RFC3339)+"; upstream calls are paused")
	}
	return nil, lastError
}

// requestHeaders builds the headers for one chat call, including the
// fingerprint the free tier gates on.
func (p *pluginState) requestHeaders(plan *executionPlan, lane executionLane, seed string) http.Header {
	session := p.sessions.id(seed)
	request := p.sessions.requestID()
	headers := clientHeaders(p.version, session, request)
	if lane.anonymous {
		// The keyless lane refuses x-opencode-request, measured against the live
		// endpoint with every other header held identical: the same request with
		// that header answers 403 and without it answers 200. The account lane
		// accepts it, so it is dropped only here.
		headers.Del("x-opencode-request")
	}
	cpakit.HostLog(context.Background(), "info", fmt.Sprintf(
		"opencode: outbound tier=%s lane=%s url=%s ua=%s session=%s request=%s",
		plan.tier, lane.label, plan.endpoint, "opencode/"+p.version, session, request))
	headers.Set("Authorization", "Bearer "+lane.key)
	if strings.TrimSpace(lane.orgID) != "" {
		// The free tier meters an account against a workspace and requires this
		// header. The keyless lane has no workspace, and the paid tier ignores it.
		headers.Set(orgHeader, lane.orgID)
	}
	return headers
}

// freeUsageLimitError reports a drained free-tier allowance, with the reset
// instant when it is known.
//
// The plugin cannot set response headers, so the reset travels in the message
// instead of a Retry-After header. A caller that reads it knows when the tier
// comes back rather than retrying blind.
func freeUsageLimitError(until time.Time) *cpakit.PluginError {
	message := "the OpenCode free tier allowance is used up for today"
	if !until.IsZero() {
		message += "; it resets at " + until.UTC().Format(time.RFC3339) +
			" (" + until.Local().Format("2006-01-02 15:04 MST") + ")"
	}
	return cpakit.NewError("free_usage_limit", message, http.StatusTooManyRequests)
}

// freeUsageLimitUntil reports the reset instant of a drained allowance, which
// the upstream states as Retry-After in seconds or as an HTTP date.
func freeUsageLimitUntil(header http.Header, now time.Time) time.Time {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return time.Time{}
	}
	if seconds, errSeconds := strconv.Atoi(raw); errSeconds == nil {
		if seconds <= 0 {
			return time.Time{}
		}
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if when, errDate := http.ParseTime(raw); errDate == nil {
		return when
	}
	return time.Time{}
}

// isFreeUsageLimit reports whether a response is a drained free-tier allowance.
func isFreeUsageLimit(status int, body []byte) bool {
	return status == http.StatusTooManyRequests && strings.Contains(string(body), "FreeUsageLimitError")
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
	case status == http.StatusTooManyRequests && strings.Contains(text, "FreeUsageLimitError"):
		// A drained daily allowance, not a transient burst limit. It resets at
		// midnight UTC, which the upstream states in Retry-After.
		return freeUsageLimitError(time.Time{})
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
