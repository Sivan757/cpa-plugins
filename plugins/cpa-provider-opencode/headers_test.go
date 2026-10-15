package main

import "testing"

// TestClientHeadersOmitTheRefusedFingerprint is the regression guard for a live
// 403. The free tier refuses a request that carries HTTP-Referer or
// x-session-affinity, with every other value held identical, so neither may
// reappear in the request set.
func TestClientHeadersOmitTheRefusedFingerprint(t *testing.T) {
	headers := clientHeaders("1.19.0", "ses_abc", "msg_xyz")

	for _, name := range []string{"HTTP-Referer", "Referer", "x-session-affinity", "X-Session-Affinity"} {
		if got := headers.Get(name); got != "" {
			t.Fatalf("%s must not be sent; got %q", name, got)
		}
	}
}

// TestClientHeadersCarryTheAcceptedIdentity pins the headers the CLI does send,
// so removing the refused pair does not remove a required one.
func TestClientHeadersCarryTheAcceptedIdentity(t *testing.T) {
	headers := clientHeaders("1.19.0", "ses_abc", "msg_xyz")

	want := map[string]string{
		"User-Agent":            "opencode/1.19.0",
		"Accept":                "application/json",
		"Content-Type":          "application/json",
		"x-opencode-client":     "cli",
		"x-opencode-project":    "global",
		"x-opencode-session":    "ses_abc",
		"x-opencode-session-id": "ses_abc",
		"x-opencode-request":    "msg_xyz",
	}
	for name, expected := range want {
		if got := headers.Get(name); got != expected {
			t.Fatalf("%s = %q, want %q", name, got, expected)
		}
	}
}
