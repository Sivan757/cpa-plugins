package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// sessionKey derives the stable session identifier the official CLI sends in
// the body.
//
// The upstream does not require it for billing — a bare OpenAI body is charged
// in real time — but the vendor's own statistics views (the credits heatmap and
// the per-request detail list) are driven by this attribution chain, so without
// it the traffic stays invisible there.
//
// The formula is the official client's: a 16-hex-character digest of the
// constant salt and the subscriber id, joined to the session with a dash. The
// session is hashed rather than forwarded, which keeps a host-side session id
// out of the vendor's records.
func sessionKey(uid, session string) string {
	digest := sha256.Sum256([]byte("qoder-session|" + uid))
	prefix := hex.EncodeToString(digest[:])[:16]
	session = strings.TrimSpace(session)
	if session == "" {
		session = "default"
	}
	return prefix + "-" + session
}

// turnIdentity carries the identifiers one agent run shares. Every request in
// the same turn must present the same request_set_id: the credits panel
// aggregates by it, so a fresh id per request would record one entry per chunk
// round trip instead of one per turn.
type turnIdentity struct {
	RequestID    string
	RequestSetID string
	ChatRecordID string
	SessionID    string
}

// newTurnIdentity builds the identity for one request. The turn is scoped by
// the session key, so a conversation keeps one request_set_id until the session
// changes; that mirrors the official client closely enough for the statistics
// views without reimplementing its turn-boundary heuristics.
func newTurnIdentity(uid, session string) turnIdentity {
	requestID := newUUID()
	return turnIdentity{
		RequestID:    requestID,
		RequestSetID: requestID,
		ChatRecordID: requestID,
		SessionID:    sessionKey(uid, session),
	}
}

// requestSession resolves the session hint for one request.
//
// CPA normalises about thirty session header spellings into a single execution
// session id and passes it to the executor in the metadata bag under
// "execution_session_id"; that is the most stable identity available, so it is
// preferred. The raw inbound headers are the fallback for a client that sends
// none, and an empty result means "no session", which the caller handles.
func requestSession(req pluginkit.ExecutorRequest) string {
	if value, ok := req.Metadata["execution_session_id"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return sessionFromRequest(req)
}
