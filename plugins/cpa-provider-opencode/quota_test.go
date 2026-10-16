package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

const okStreamBody = `data: {"id":"x","choices":[{"delta":{"content":"OK"}}]}

data: [DONE]

`

const drainedBody = `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded. Please try again later."}}`

const burstBody = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`

// installBudgetHost answers upstream calls with a scripted status per credential.
// Unlike the lane helper it also returns response headers, because a drained
// allowance states its reset instant in Retry-After.
func installBudgetHost(t *testing.T, statusFor func(authorization string) (int, string, http.Header)) *[]string {
	t.Helper()
	attempts := &[]string{}

	cpakit.SetHost(func(method string, payload []byte) ([]byte, error) {
		if method != cpakit.MethodHostHTTPDo {
			return []byte(`{"ok":true}`), nil
		}
		var request struct {
			Headers map[string][]string `json:"headers"`
		}
		if errUnmarshal := json.Unmarshal(payload, &request); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		authorization := http.Header(request.Headers).Get("Authorization")
		*attempts = append(*attempts, authorization)

		status, body, header := statusFor(authorization)
		response := map[string]any{
			"StatusCode": status,
			"Body":       base64.StdEncoding.EncodeToString([]byte(body)),
		}
		if len(header) > 0 {
			headers := map[string][]string{}
			for name, values := range header {
				headers[name] = values
			}
			response["Headers"] = headers
		}
		raw, _ := json.Marshal(map[string]any{"ok": true, "result": response})
		return raw, nil
	})
	t.Cleanup(func() { cpakit.SetHost(nil) })
	return attempts
}

// freeTierPlan builds the two-lane plan the free tier uses.
func freeTierPlan() *executionPlan {
	return &executionPlan{
		tier:     tierFree,
		protocol: protocolChat,
		endpoint: "https://example.invalid/chat/completions",
		model:    "mimo-v2.6-flash-free",
		lanes: []executionLane{
			{label: "anonymous", key: publicCredential, anonymous: true},
			{label: "account", key: "st_account", orgID: "org_x"},
		},
	}
}

// TestDrainedAllowancePausesUpstreamCalls proves a drained daily allowance stops
// costing upstream calls. Measured against the live tier: it answers 429 with a
// FreeUsageLimitError body and a Retry-After that counts down to midnight UTC.
func TestDrainedAllowancePausesUpstreamCalls(t *testing.T) {
	attempts := installBudgetHost(t, func(string) (int, string, http.Header) {
		return http.StatusTooManyRequests, drainedBody, http.Header{"Retry-After": []string{"3600"}}
	})

	state := newPluginState()
	plan := freeTierPlan()

	// The first request tries every lane, because a lane with its own allowance
	// would answer here.
	_, errFirst := state.callChat(context.Background(), cpakit.ExecutorRequest{}, plan, []byte("{}"))
	if errFirst == nil {
		t.Fatal("a drained allowance was reported as success")
	}
	failure, ok := errFirst.(*cpakit.Error)
	if !ok || failure.Code != "free_usage_limit" {
		t.Fatalf("error = %v, want code free_usage_limit", errFirst)
	}
	if got := len(*attempts); got != 2 {
		t.Fatalf("%d attempts on the first request, want one per lane", got)
	}
	// The message must carry the reset instant, because the plugin cannot set a
	// Retry-After response header.
	if !strings.Contains(failure.Message, "resets at") {
		t.Fatalf("message does not state the reset instant: %q", failure.Message)
	}

	// The second request must not reach upstream at all.
	_, errSecond := state.callChat(context.Background(), cpakit.ExecutorRequest{}, plan, []byte("{}"))
	if errSecond == nil {
		t.Fatal("a paused allowance was reported as success")
	}
	if got := len(*attempts); got != 2 {
		t.Fatalf("%d attempts after the pause, want the upstream left alone", got)
	}
	if errSecond.Error() != errFirst.Error() {
		t.Fatalf("second error %q, want the same one", errSecond.Error())
	}
}

// TestDrainedAllowanceStillTriesTheSecondLane proves the pause is recorded only
// when every lane reports the drained state. A lane with its own allowance wins.
func TestDrainedAllowanceStillTriesTheSecondLane(t *testing.T) {
	attempts := installBudgetHost(t, func(authorization string) (int, string, http.Header) {
		if authorization == "Bearer "+publicCredential {
			return http.StatusTooManyRequests, drainedBody, http.Header{"Retry-After": []string{"3600"}}
		}
		return http.StatusOK, okStreamBody, nil
	})

	state := newPluginState()
	plan := freeTierPlan()

	result, errCall := state.callChat(context.Background(), cpakit.ExecutorRequest{}, plan, []byte("{}"))
	if errCall != nil {
		t.Fatalf("callChat: %v", errCall)
	}
	if result.status != http.StatusOK {
		t.Fatalf("status = %d, want the account lane to answer", result.status)
	}
	if got := len(*attempts); got != 2 {
		t.Fatalf("%d attempts, want one per lane", got)
	}

	// A lane answered, so the allowance is not paused: the next request tries again.
	if _, errNext := state.callChat(context.Background(), cpakit.ExecutorRequest{}, plan, []byte("{}")); errNext != nil {
		t.Fatalf("callChat: %v", errNext)
	}
	if got := len(*attempts); got != 4 {
		t.Fatalf("%d attempts, want the pause not to have been recorded", got)
	}
}

// TestSuccessClearsThePause proves the pause ends when the tier answers again, so
// a request served after the reset does not leave the plugin blocked.
func TestSuccessClearsThePause(t *testing.T) {
	allow := false
	attempts := installBudgetHost(t, func(string) (int, string, http.Header) {
		if allow {
			return http.StatusOK, okStreamBody, nil
		}
		return http.StatusTooManyRequests, drainedBody, http.Header{"Retry-After": []string{"1"}}
	})

	state := newPluginState()
	plan := freeTierPlan()
	if _, errFirst := state.callChat(context.Background(), cpakit.ExecutorRequest{}, plan, []byte("{}")); errFirst == nil {
		t.Fatal("expected the drained allowance to fail the first call")
	}
	afterFirst := len(*attempts)

	// Wait out the one-second window the upstream stated.
	time.Sleep(1100 * time.Millisecond)
	allow = true
	if _, errSecond := state.callChat(context.Background(), cpakit.ExecutorRequest{}, plan, []byte("{}")); errSecond != nil {
		t.Fatalf("callChat after the reset: %v", errSecond)
	}
	if len(*attempts) <= afterFirst {
		t.Fatal("the call after the reset did not reach upstream")
	}

	// The success cleared the pause, so a later call is attempted again.
	afterSecond := len(*attempts)
	if _, errThird := state.callChat(context.Background(), cpakit.ExecutorRequest{}, plan, []byte("{}")); errThird != nil {
		t.Fatalf("callChat after success: %v", errThird)
	}
	if len(*attempts) <= afterSecond {
		t.Fatal("the successful call did not clear the pause")
	}
}

// TestUsageLimitResetIsReadFromRetryAfter covers both forms the header can take.
func TestUsageLimitResetIsReadFromRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 16, 7, 53, 4, 0, time.UTC)

	seconds := freeUsageLimitUntil(http.Header{"Retry-After": []string{"58016"}}, now)
	if !seconds.Equal(now.Add(58016 * time.Second)) {
		t.Fatalf("seconds form = %s", seconds)
	}
	// The measured value must land exactly on midnight UTC, which is the daily
	// reset the tier was observed to use.
	if !seconds.Equal(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("58016s from the measurement is not midnight UTC: %s", seconds)
	}

	dateForm := freeUsageLimitUntil(http.Header{"Retry-After": []string{"Fri, 16 Oct 2026 09:00:00 GMT"}}, now)
	if !dateForm.Equal(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("date form = %s", dateForm)
	}

	for _, raw := range []string{"", "0", "-5", "soon"} {
		if got := freeUsageLimitUntil(http.Header{"Retry-After": []string{raw}}, now); !got.IsZero() {
			t.Fatalf("Retry-After %q produced %s, want the zero instant", raw, got)
		}
	}
}

// TestOnlyTheUsageLimitIsTreatedAsDrained keeps a transient burst limit on the
// ordinary rate-limit path, so a momentary 429 does not pause the tier for hours.
func TestOnlyTheUsageLimitIsTreatedAsDrained(t *testing.T) {
	burst := []byte(burstBody)
	if isFreeUsageLimit(http.StatusTooManyRequests, burst) {
		t.Fatal("a burst limit was treated as a drained allowance")
	}
	if !isFreeUsageLimit(http.StatusTooManyRequests, []byte(drainedBody)) {
		t.Fatal("the usage-limit body was not recognised")
	}
	if got := classifyUpstreamError(http.StatusTooManyRequests, burst); got.Code != "rate_limited" {
		t.Fatalf("burst limit code = %q, want rate_limited", got.Code)
	}
}
