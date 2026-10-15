package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// hostAttempt records one upstream call the plugin made through the host.
type hostAttempt struct {
	authorization string
	requestHeader string
	url           string
}

// installFakeHost serves the plugin's upstream calls from a table. The keyless
// credential is answered with status, and every other credential with 200, so a
// test can drive the lane order without touching the network.
func installFakeHost(t *testing.T, anonymousStatus int) *[]hostAttempt {
	t.Helper()
	attempts := &[]hostAttempt{}

	cpakit.SetHost(func(method string, payload []byte) ([]byte, error) {
		if method != cpakit.MethodHostHTTPDo {
			// Log and other callbacks need no result.
			return []byte(`{"ok":true}`), nil
		}
		var request struct {
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
		}
		if errUnmarshal := json.Unmarshal(payload, &request); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		// Header names arrive canonicalised, so read them through http.Header
		// instead of indexing the map with the lowercase spelling.
		headers := http.Header(request.Headers)
		first := func(name string) string { return headers.Get(name) }
		*attempts = append(*attempts, hostAttempt{
			authorization: first("Authorization"),
			requestHeader: first("x-opencode-request"),
			url:           request.URL,
		})

		status := 200
		body := `data: {"id":"x","choices":[{"delta":{"content":"OK"}}]}
data: [DONE]
`
		if first("Authorization") == "Bearer "+publicCredential {
			status = anonymousStatus
			body = `{"type":"error","error":{"type":"FreeTierError","message":"refused"}}`
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(body))
		return []byte(fmt.Sprintf(`{"ok":true,"result":{"StatusCode":%d,"Body":%q}}`, status, encoded)), nil
	})
	t.Cleanup(func() { cpakit.SetHost(nil) })
	return attempts
}

// TestFreeTierFallsBackToTheAccountLane proves the free tier prefers the keyless
// credential and covers its refusal with the account. It also proves the two
// lanes differ on the wire: the refused attempt must not carry the correlation
// header the keyless lane rejects.
func TestFreeTierFallsBackToTheAccountLane(t *testing.T) {
	attempts := installFakeHost(t, 403)

	state := newPluginState()
	plan := &executionPlan{
		tier:     tierFree,
		protocol: protocolChat,
		endpoint: "https://example.invalid/chat/completions",
		model:    "mimo-v2.6-flash-free",
		lanes: []executionLane{
			{label: "anonymous", key: publicCredential, anonymous: true},
			{label: "account", key: "st_account", orgID: "org_x"},
		},
	}

	result, errCall := state.callChat(context.Background(), cpakit.ExecutorRequest{}, plan, []byte(`{}`))
	if errCall != nil {
		t.Fatalf("callChat: %v", errCall)
	}
	if result.StatusCode != 200 {
		t.Fatalf("status = %d, want the account lane to answer 200", result.StatusCode)
	}

	if len(*attempts) != 2 {
		t.Fatalf("%d attempts, want the keyless lane then the account lane", len(*attempts))
	}
	keyless, account := (*attempts)[0], (*attempts)[1]

	if keyless.authorization != "Bearer "+publicCredential {
		t.Fatalf("first attempt credential = %q, want the keyless credential", keyless.authorization)
	}
	if keyless.requestHeader != "" {
		t.Fatalf("the keyless attempt carried x-opencode-request = %q; the keyless lane refuses it", keyless.requestHeader)
	}
	if account.authorization != "Bearer st_account" {
		t.Fatalf("second attempt credential = %q, want the account token", account.authorization)
	}
	if account.requestHeader == "" {
		t.Fatal("the account attempt must carry x-opencode-request")
	}
}

// TestFreeTierStopsAtTheFirstWorkingLane proves a healthy keyless lane is not
// followed by an account call, so the account is not spent when it is not needed.
func TestFreeTierStopsAtTheFirstWorkingLane(t *testing.T) {
	attempts := installFakeHost(t, 200)

	state := newPluginState()
	plan := &executionPlan{
		tier:     tierFree,
		protocol: protocolChat,
		endpoint: "https://example.invalid/chat/completions",
		model:    "mimo-v2.6-flash-free",
		lanes: []executionLane{
			{label: "anonymous", key: publicCredential, anonymous: true},
			{label: "account", key: "st_account", orgID: "org_x"},
		},
	}

	if _, errCall := state.callChat(context.Background(), cpakit.ExecutorRequest{}, plan, []byte(`{}`)); errCall != nil {
		t.Fatalf("callChat: %v", errCall)
	}
	if len(*attempts) != 1 {
		t.Fatalf("%d attempts, want only the keyless lane", len(*attempts))
	}
	if (*attempts)[0].authorization != "Bearer "+publicCredential {
		t.Fatalf("credential = %q", (*attempts)[0].authorization)
	}
}

// TestFreeTierReportsTheLastLaneFailure proves both lanes failing surfaces an
// error rather than a silent empty answer.
func TestFreeTierReportsTheLastLaneFailure(t *testing.T) {
	installFakeHost(t, 403)

	state := newPluginState()
	plan := &executionPlan{
		tier:     tierFree,
		protocol: protocolChat,
		endpoint: "https://example.invalid/chat/completions",
		model:    "mimo-v2.6-flash-free",
		lanes: []executionLane{
			{label: "anonymous", key: publicCredential, anonymous: true},
		},
	}

	if _, errCall := state.callChat(context.Background(), cpakit.ExecutorRequest{}, plan, []byte(`{}`)); errCall == nil {
		t.Fatal("a refused lane was reported as success")
	}
}
