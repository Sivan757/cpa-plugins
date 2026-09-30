package main

import (
	"net/http"
	"strings"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// pluginkitSessionSource is the slice of a request this plugin reads a session
// hint from. It exists as an interface so the extraction is unit-testable.
type pluginkitSessionSource interface {
	HeaderGet(key string) string
}

// headerSource adapts an http.Header.
type headerSource struct{ header http.Header }

func (h headerSource) HeaderGet(key string) string { return h.header.Get(key) }

// sessionFromRequest extracts a session hint from the executor request: first
// the metadata bag key the host normalises thirty-odd session headers into,
// then the raw inbound headers.
func sessionFromRequest(req pluginkit.ExecutorRequest) string {
	if value, ok := req.Metadata["execution_session_id"].(string); ok {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	for _, key := range []string{"X-Session-Id", "X-Conversation-Id", "Session-Id", "X-Request-Id"} {
		if value := req.Headers.Get(key); value != "" {
			return value
		}
	}
	return ""
}
