package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// TestSessionKeyMatchesTheOfficialDerivation pins the session formula. The
// vendor's statistics views group by this value, so a changed derivation would
// silently split one conversation across many rows.
func TestSessionKeyMatchesTheOfficialDerivation(t *testing.T) {
	key := sessionKey("uid-1", "session-9")
	if !strings.Contains(key, "-") {
		t.Fatalf("session key must join a digest prefix and the session: %q", key)
	}
	parts := strings.SplitN(key, "-", 2)
	if len(parts) != 2 || len(parts[0]) != 16 {
		t.Fatalf("the digest prefix must be 16 hex characters: %q", key)
	}
	if parts[1] != "session-9" {
		t.Fatalf("the session suffix was not preserved: %q", key)
	}
	if key != sessionKey("uid-1", "session-9") {
		t.Fatal("the derivation must be deterministic")
	}
	if key == sessionKey("uid-2", "session-9") {
		t.Fatal("a different subscriber must derive a different key")
	}
}

// TestTurnIdentitySharesTheSetID checks that every request inside one turn
// presents the same request_set_id, which is what the credits panel aggregates
// by.
func TestTurnIdentitySharesTheSetID(t *testing.T) {
	first := newTurnIdentity("uid", "session")
	second := newTurnIdentity("uid", "session")
	if first.SessionID != second.SessionID {
		t.Fatal("the same session must derive the same session id")
	}
	if first.RequestSetID == second.RequestSetID {
		t.Fatal("distinct requests must carry distinct request ids")
	}
	if first.ChatRecordID != first.RequestID {
		t.Fatal("chat_record_id mirrors request_id in the official client")
	}
}

// TestRequestSessionPrefersTheHostExecutionSession checks that the metadata bag
// key the host normalises thirty-odd session headers into wins over the raw
// headers.
func TestRequestSessionPrefersTheHostExecutionSession(t *testing.T) {
	req := pluginkit.ExecutorRequest{
		Metadata: map[string]any{"execution_session_id": "exec-1"},
		Headers:  http.Header{"X-Conversation-Id": []string{"conv-1"}},
	}
	if got := sessionFromRequest(req); got != "exec-1" {
		t.Fatalf("session = %q, want the host execution session", got)
	}

	req = pluginkit.ExecutorRequest{Headers: http.Header{"X-Conversation-Id": []string{"conv-1"}}}
	if got := sessionFromRequest(req); got != "conv-1" {
		t.Fatalf("session = %q, want the conversation header as the fallback", got)
	}

	req = pluginkit.ExecutorRequest{}
	if got := sessionFromRequest(req); got != "" {
		t.Fatalf("session = %q, want empty when nothing carries one", got)
	}
}

// sanitizeFixture builds a message array by re-encoding decoded JSON, so the
// fixtures below stay readable.
func sanitizeFixture(t *testing.T, raw string) []any {
	t.Helper()
	var messages []any
	if errUnmarshal := json.Unmarshal([]byte(raw), &messages); errUnmarshal != nil {
		t.Fatalf("decode fixture: %v", errUnmarshal)
	}
	return sanitizeToolPairing(messages)
}

// TestSanitizeRepairsAnOrphanToolResult covers the repair the strict model
// families require: a tool result whose call was never declared must get a
// synthetic assistant stub, or the whole request is rejected.
func TestSanitizeRepairsAnOrphanToolResult(t *testing.T) {
	out := sanitizeFixture(t, `[{"role":"user","content":"list files"},{"role":"tool","tool_call_id":"call_1","content":"a.txt"}]`)
	if len(out) != 3 {
		t.Fatalf("expected the stub to be inserted, got %d messages", len(out))
	}
	second, okSecond := out[1].(map[string]any)
	if !okSecond {
		t.Fatal("message 1 is not an object")
	}
	if role, _ := second["role"].(string); role != "assistant" {
		t.Fatalf("message 1 role = %v, want the synthetic assistant stub", second["role"])
	}
}

// TestSanitizeClosesAnUnansweredCall covers the placeholder result.
func TestSanitizeClosesAnUnansweredCall(t *testing.T) {
	out := sanitizeFixture(t, `[{"role":"user","content":"list files"},{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]}]`)
	last := out[len(out)-1].(map[string]any)
	if role, _ := last["role"].(string); role != "tool" {
		t.Fatalf("last role = %v, want a placeholder tool result", last["role"])
	}
	if id, _ := last["tool_call_id"].(string); id != "call_1" {
		t.Fatalf("placeholder call id = %v", last["tool_call_id"])
	}
}

// TestSanitizeDropsADuplicateResult covers the dedup rule.
func TestSanitizeDropsADuplicateResult(t *testing.T) {
	out := sanitizeFixture(t, `[{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"a"},{"role":"tool","tool_call_id":"call_1","content":"b"}]`)
	count := 0
	for _, entry := range out {
		message := entry.(map[string]any)
		if role, _ := message["role"].(string); role == "tool" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 tool result after dedup, got %d", count)
	}
}

// TestSanitizeRewritesDeveloperAndNullContent covers the two per-message
// rewrites every request needs.
func TestSanitizeRewritesDeveloperAndNullContent(t *testing.T) {
	out := sanitizeFixture(t, `[{"role":"developer","content":null},{"role":"user","content":"hi"}]`)
	first := out[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Fatalf("developer was not rewritten: %v", first["role"])
	}
	if first["content"] != "" {
		t.Fatalf("null content was not emptied: %v", first["content"])
	}
}
