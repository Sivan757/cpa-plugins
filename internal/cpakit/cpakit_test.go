package cpakit

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestEnvelopeRoundTrip pins the wire contract: a result encodes under ok with
// its raw payload, and a failure encodes the code, message, and status the host
// surfaces to the client.
func TestEnvelopeRoundTrip(t *testing.T) {
	raw := OK(map[string]string{"provider": "demo"})
	var env Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode success envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatal("success envelope reported ok=false")
	}
	var result map[string]string
	if errUnmarshal := json.Unmarshal(env.Result, &result); errUnmarshal != nil {
		t.Fatalf("decode result: %v", errUnmarshal)
	}
	if result["provider"] != "demo" {
		t.Fatalf("result = %v, want the provider key", result)
	}

	failure := Fail("invalid_model", "no such model", http.StatusNotFound)
	if errUnmarshal := json.Unmarshal(failure, &env); errUnmarshal != nil {
		t.Fatalf("decode failure envelope: %v", errUnmarshal)
	}
	if env.OK || env.Error == nil {
		t.Fatal("failure envelope reported ok=true")
	}
	if env.Error.Code != "invalid_model" || env.Error.HTTPStatus != http.StatusNotFound {
		t.Fatalf("failure envelope = %+v, want code and status preserved", env.Error)
	}
}

// TestClassifiedErrorKeepsItsStatus proves a classified failure reaches the host
// with the status that tells it how to react, rather than a generic failure.
func TestClassifiedErrorKeepsItsStatus(t *testing.T) {
	cases := []struct {
		status int
		want   Class
	}{
		{http.StatusUnauthorized, ClassAuth},
		{http.StatusTooManyRequests, ClassRateLimit},
		{http.StatusNotFound, ClassInvalidModel},
		{http.StatusBadGateway, ClassUpstream},
		{http.StatusBadRequest, ClassUnsupported},
	}
	for _, testCase := range cases {
		classified := FromStatus(testCase.status, "upstream said no")
		if classified.Code != string(testCase.want) {
			t.Fatalf("status %d classified as %q, want %q", testCase.status, classified.Code, testCase.want)
		}
		if classified.HTTPStatus != testCase.status {
			t.Fatalf("status %d lost its status: %d", testCase.status, classified.HTTPStatus)
		}
	}
}

// TestRefusalKeepsItsStatus is the property that matters to the host: a
// credential or quota failure must not be flattened into a gateway fault,
// because the host would then retry a request that cannot succeed.
func TestRefusalKeepsItsStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusNotFound, http.StatusBadRequest} {
		classified := FromStatus(status, "refused")
		if classified.HTTPStatus != status {
			t.Fatalf("status %d became %d", status, classified.HTTPStatus)
		}
	}
}

// TestRedactRemovesCredentials pins the redaction boundary: a credential echoed
// in an upstream body must not reach a client-visible message.
func TestRedactRemovesCredentials(t *testing.T) {
	inputs := []string{
		"authorization: Bearer sk-live-abcdefghijklmnop",
		"api_key=ck_0123456789abcdef",
		"personal_token: pt-0123456789abcdef",
		"Cookie: session=0123456789abcdef",
	}
	for _, input := range inputs {
		redacted := Redact(input)
		for _, secret := range []string{"sk-live-abcdefghijklmnop", "ck_0123456789abcdef", "pt-0123456789abcdef"} {
			if strings.Contains(input, secret) && strings.Contains(redacted, secret) {
				t.Fatalf("redaction leaked %q from %q", secret, input)
			}
		}
		if !strings.Contains(redacted, "<redacted>") {
			t.Fatalf("nothing was redacted in %q", input)
		}
	}
}

// TestRedactBoundsMessageLength keeps one upstream error page from filling a
// client response or a log line.
func TestRedactBoundsMessageLength(t *testing.T) {
	long := strings.Repeat("x", maxReported*2)
	redacted := Redact(long)
	if len(redacted) > maxReported+8 {
		t.Fatalf("redacted length %d exceeds the bound", len(redacted))
	}
}

// TestFilterModelsByExclusion pins the exclusion semantics: exact ids, glob
// patterns, and the wildcard.
func TestFilterModelsByExclusion(t *testing.T) {
	models := []ModelInfo{{ID: "glm-5.3"}, {ID: "glm-5.3-flash"}, {ID: "kimi-k3"}}
	filtered := FilterModelsByExclusion(models, []string{"glm-5.3", "kimi-*"})
	if len(filtered) != 1 || filtered[0].ID != "glm-5.3-flash" {
		t.Fatalf("filtered = %+v, want only glm-5.3-flash", filtered)
	}
	if hidden := FilterModelsByExclusion(models, []string{"*"}); len(hidden) != 0 {
		t.Fatalf("wildcard hid %d models, want all", len(hidden))
	}
	if kept := FilterModelsByExclusion(models, nil); len(kept) != len(models) {
		t.Fatalf("an empty exclusion list removed models")
	}
}

// TestConfigAccessors pins the config decoding the host's YAML reaches.
func TestConfigAccessors(t *testing.T) {
	cfg, errParse := ParseConfig([]byte("api_key: ck_live\ncache_ttl_seconds: 90\nfree_enabled: false\nregion: china\napi_keys:\n  - a\n  - b\n"))
	if errParse != nil {
		t.Fatalf("parse config: %v", errParse)
	}
	if got := cfg.String("api_key", ""); got != "ck_live" {
		t.Fatalf("api_key = %q", got)
	}
	if got := cfg.Int("cache_ttl_seconds", 0); got != 90 {
		t.Fatalf("cache_ttl_seconds = %d", got)
	}
	if got := cfg.Bool("free_enabled", true); got {
		t.Fatalf("free_enabled = true, want false")
	}
	if got := cfg.String("region", ""); got != "china" {
		t.Fatalf("region = %q", got)
	}
	if got := cfg.Strings("api_keys"); len(got) != 2 {
		t.Fatalf("api_keys = %v, want two entries", got)
	}
	if got := cfg.String("absent", "fallback"); got != "fallback" {
		t.Fatalf("absent key = %q, want the fallback", got)
	}
	// A comma-separated scalar is accepted as well, because a hand-written
	// config often uses that shape for a key list.
	comma, errComma := ParseConfig([]byte("api_key: a, b, c\n"))
	if errComma != nil {
		t.Fatalf("parse comma config: %v", errComma)
	}
	if got := comma.Strings("api_key"); len(got) != 3 {
		t.Fatalf("comma list = %v, want three entries", got)
	}
}

// TestDecodeExecutorRequestReadsTheEnvelope proves the two envelope fields that
// sit outside the official struct are decoded: the downstream stream id and the
// host callback id.
func TestDecodeExecutorRequestReadsTheEnvelope(t *testing.T) {
	payload := []byte(`{"Model":"glm-5.3","Stream":true,"stream_id":"down-1","host_callback_id":"cb-1"}`)
	req, streamID, callbackID, errDecode := DecodeExecutorRequest(payload)
	if errDecode != nil {
		t.Fatalf("decode executor request: %v", errDecode)
	}
	if req.Model != "glm-5.3" || !req.Stream {
		t.Fatalf("embedded request decoded as %+v", req)
	}
	if streamID != "down-1" || callbackID != "cb-1" {
		t.Fatalf("envelope ids = %q/%q, want down-1/cb-1", streamID, callbackID)
	}
}

// TestIdentifierRefusesAnUnimplementedCapability keeps a plugin from claiming a
// provider key it cannot serve.
func TestIdentifierRefusesAnUnimplementedCapability(t *testing.T) {
	onlyExecutor := Capabilities{Executor: true}
	if _, errMissing := identifierFor(onlyExecutor, "demo", MethodQuotaIdentifier); errMissing != ErrUnhandled {
		t.Fatalf("quota identifier = %v, want ErrUnhandled", errMissing)
	}
	result, errExecutor := identifierFor(onlyExecutor, "demo", MethodExecutorIdentifier)
	if errExecutor != nil {
		t.Fatalf("executor identifier = %v", errExecutor)
	}
	identifier, okIdentifier := result.(map[string]string)
	if !okIdentifier || identifier["identifier"] != "demo" {
		t.Fatalf("identifier = %v, want the provider key", result)
	}
}

// TestStreamResponseIsMarshalable is the regression guard for a plugin result
// that cannot cross the RPC boundary: the host encodes every executor result as
// JSON, so a response carrying a channel fails at the host with "unsupported
// type" rather than in any unit test that only builds the value.
func TestStreamResponseIsMarshalable(t *testing.T) {
	response := BufferedStream(StreamHeaders(), SplitSSE([]byte("data: {\"a\":1}\n\ndata: [DONE]\n\n")))
	if len(response.Chunks) != 1 {
		t.Fatalf("chunks = %d, want the one data frame", len(response.Chunks))
	}
	encoded, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		t.Fatalf("stream response does not encode: %v", errMarshal)
	}
	if !strings.Contains(string(encoded), "text/event-stream") {
		t.Fatalf("encoded stream response lost its headers: %s", encoded)
	}
	// A non-streaming result must encode too.
	if _, errResponse := json.Marshal(ExecutorResponse{Payload: []byte("{}")}); errResponse != nil {
		t.Fatalf("executor response does not encode: %v", errResponse)
	}
}

// TestSplitSSEFramesBareJSON pins the framing contract with the host: chunks
// carry the JSON payload alone, because the host adds its own SSE prefix and
// terminator.
func TestSplitSSEFramesBareJSON(t *testing.T) {
	chunks := SplitSSE([]byte("data: {\"a\":1}\n\ndata: [DONE]\n\n"))
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if strings.HasPrefix(string(chunks[0]), "data:") {
		t.Fatalf("chunk carries an SSE prefix: %q", chunks[0])
	}
	if strings.Contains(string(chunks[0]), "[DONE]") {
		t.Fatal("the host appends the terminator itself")
	}
}

// TestExposeOnlyNarrowsTheRoster pins the allowlist semantics: an unset list
// must not blank a picker, and a set list must be the only way a model reaches
// a client.
func TestExposeOnlyNarrowsTheRoster(t *testing.T) {
	models := []ModelInfo{{ID: "qwen3.8-flash"}, {ID: "mimo-v2.6-flash-free"}, {ID: "deepseek-v4.1-flash"}, {ID: "glm-5.3"}}

	if kept := ExposeOnly(models, nil); len(kept) != len(models) {
		t.Fatalf("an unset allowlist removed models: %d left", len(kept))
	}
	if kept := ExposeOnly(models, []string{"  "}); len(kept) != len(models) {
		t.Fatalf("a blank allowlist removed models: %d left", len(kept))
	}

	kept := ExposeOnly(models, []string{"qwen3.8-flash"})
	if len(kept) != 1 || kept[0].ID != "qwen3.8-flash" {
		t.Fatalf("exact allowlist = %+v", kept)
	}

	free := ExposeOnly(models, []string{"*-free"})
	if len(free) != 1 || free[0].ID != "mimo-v2.6-flash-free" {
		t.Fatalf("suffix allowlist = %+v", free)
	}

	// Two patterns, given as a comma-separated scalar, must both apply.
	both := ExposeOnly(models, []string{"qwen3.8-flash, deepseek-v4.1-flash"})
	if len(both) != 2 {
		t.Fatalf("comma allowlist = %+v", both)
	}

	// A case difference must not hide a model.
	cased := ExposeOnly(models, []string{"QWEN3.8-FLASH"})
	if len(cased) != 1 {
		t.Fatalf("allowlist is case-sensitive: %+v", cased)
	}

	// A pattern matching nothing yields an empty roster, not the full one.
	if none := ExposeOnly(models, []string{"no-such-model"}); len(none) != 0 {
		t.Fatalf("a non-matching allowlist exposed %d models", len(none))
	}
}
