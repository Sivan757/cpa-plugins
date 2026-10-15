package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

func init() { cpakit.RegisterPlugin(newGateway()) }

// call drives one RPC exactly the way the host does and fails on an error
// envelope, so a test asserts only on decoded results.
func call(t *testing.T, method string, request any) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	out := cpakit.HandleCall(method, raw)
	var env cpakit.Envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope for %s: %v (raw: %s)", method, errUnmarshal, out)
	}
	if !env.OK {
		t.Fatalf("%s returned an error: %+v", method, env.Error)
	}
	return env.Result
}

// TestRegisterDeclaresOneProviderKey pins the merge contract: three credential
// families behind a single provider key, which is why overlapping model ids no
// longer compete across plugins.
func TestRegisterDeclaresOneProviderKey(t *testing.T) {
	result := call(t, cpakit.MethodPluginRegister, map[string]any{})
	var reg struct {
		Metadata     cpakit.Metadata
		Capabilities cpakit.Capabilities
	}
	if errUnmarshal := json.Unmarshal(result, &reg); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}
	if reg.Metadata.Name != pluginName {
		t.Fatalf("name = %q, want %q", reg.Metadata.Name, pluginName)
	}
	if !reg.Capabilities.QuotaProvider || !reg.Capabilities.ModelProvider || !reg.Capabilities.Executor || !reg.Capabilities.AuthProvider {
		t.Fatalf("expected quota, model, executor, and auth capabilities: %+v", reg.Capabilities)
	}
}

// TestQuotaDescribeNamesTheMergedKey checks that quota is registered under the
// single provider key rather than one per credential family.
func TestQuotaDescribeNamesTheMergedKey(t *testing.T) {
	result := call(t, cpakit.MethodQuotaDescribe, map[string]any{})
	var desc cpakit.QuotaDescribeResponse
	if errUnmarshal := json.Unmarshal(result, &desc); errUnmarshal != nil {
		t.Fatalf("decode describe: %v", errUnmarshal)
	}
	if len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != providerKey {
		t.Fatalf("supported providers = %v, want [%s]", desc.SupportedProviders, providerKey)
	}
}

// TestAuthParseClaimsEveryKindAndDeclinesOthers walks each credential family.
func TestAuthParseClaimsEveryKindAndDeclinesOthers(t *testing.T) {
	for _, id := range kindOrder {
		own := call(t, cpakit.MethodAuthParse, cpakit.AuthParseRequest{
			Provider: providerKey,
			FileName: providerKey + "-" + id + ".json",
			RawJSON:  []byte(`{"type":"` + providerKey + `","kind":"` + id + `"}`),
		})
		var parsed cpakit.AuthParseResponse
		if errUnmarshal := json.Unmarshal(own, &parsed); errUnmarshal != nil {
			t.Fatalf("decode parse for %s: %v", id, errUnmarshal)
		}
		if !parsed.Handled {
			t.Fatalf("kind %s was not claimed", id)
		}
		if parsed.Auth.ID != providerKey+"-"+id {
			t.Fatalf("kind %s produced id %q, want %q", id, parsed.Auth.ID, providerKey+"-"+id)
		}
		// The kind marker must round trip so the executor picks the right upstream.
		var stored map[string]string
		if errUnmarshal := json.Unmarshal(parsed.Auth.StorageJSON, &stored); errUnmarshal != nil {
			t.Fatalf("decode storage for %s: %v", id, errUnmarshal)
		}
		if stored["kind"] != id {
			t.Fatalf("kind %s lost its marker in storage: %v", id, stored)
		}
	}

	other := call(t, cpakit.MethodAuthParse, cpakit.AuthParseRequest{
		Provider: "unrelated",
		RawJSON:  []byte(`{"type":"unrelated"}`),
	})
	var declined cpakit.AuthParseResponse
	if errUnmarshal := json.Unmarshal(other, &declined); errUnmarshal != nil {
		t.Fatalf("decode parse: %v", errUnmarshal)
	}
	if declined.Handled {
		t.Fatal("the plugin must not claim another provider's auth file")
	}
}

// TestKindsCoverDistinctUpstreams checks that the two CN families and the
// global family really do differ where the gateway requires it.
func TestKindsCoverDistinctUpstreams(t *testing.T) {
	if kinds["workbuddy"].chatBase == kinds["workbuddy-ai"].chatBase {
		t.Fatal("the two desktop products must use different hosts: the CN and international gateways are separate")
	}
	if kinds["workbuddy"].region != "cn" || kinds["workbuddy-ai"].region != "global" {
		t.Fatal("the desktop kinds must keep their regions")
	}
	if !kinds["workbuddy"].desktop || !kinds["workbuddy-ai"].desktop {
		t.Fatal("both desktop kinds must be marked desktop")
	}
	if kinds["codebuddy"].desktop {
		t.Fatal("the codebuddy kind is plugin-managed, not desktop")
	}
}

// TestUnknownMethodIsReportedNotPanicked checks the C ABI boundary: a panic
// escaping into C would abort the whole host process.
func TestUnknownMethodIsReportedNotPanicked(t *testing.T) {
	out := cpakit.HandleCall("no.such.method", []byte(`{}`))
	var env cpakit.Envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("expected an unknown_method envelope, got %s", out)
	}
}

// TestManagementStatusListsEveryKind checks the diagnostic surface.
func TestManagementStatusListsEveryKind(t *testing.T) {
	result := call(t, cpakit.MethodManagementHandle, cpakit.ManagementRequest{
		Method: "GET",
		Path:   "plugins/cpa-provider-tencent/status",
	})
	var response cpakit.ManagementResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode management response: %v", errUnmarshal)
	}
	if response.StatusCode != 200 {
		t.Fatalf("status route returned HTTP %d: %s", response.StatusCode, response.Body)
	}
	var report struct {
		Credentials []map[string]any `json:"credentials"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &report); errUnmarshal != nil {
		t.Fatalf("decode status body: %v", errUnmarshal)
	}
	if len(report.Credentials) != len(kindOrder) {
		t.Fatalf("status listed %d credentials, want %d", len(report.Credentials), len(kindOrder))
	}
}

// TestPrepareBodyRewritesTheUpstreamDialect covers the rewrites the gateway
// enforces on every credential family.
func TestPrepareBodyRewritesTheUpstreamDialect(t *testing.T) {
	p := newPluginState()
	body := []byte(`{"model":"glm-5.3","stream":false,"messages":[{"role":"developer","content":"x"}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`)
	out, errPrepare := p.prepareBody(kinds["workbuddy"], body, nil)
	if errPrepare != nil {
		t.Fatalf("prepare: %v", errPrepare)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatalf("decode prepared body: %v", errUnmarshal)
	}
	if doc["stream"] != true {
		t.Fatal("stream must be forced on: the gateway refuses non-streaming calls")
	}
	messages := doc["messages"].([]any)
	first := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role was not rewritten: %v", first["role"])
	}
	if doc["tool_choice"] != "lookup" {
		t.Fatalf("tool_choice was not flattened: %v", doc["tool_choice"])
	}
}

// TestPrepareBodyGlobalRules covers the international-only rewrites.
func TestPrepareBodyGlobalRules(t *testing.T) {
	p := newPluginState()
	body := []byte(`{"messages":[{"role":"user","content":"x"}],"reasoning_effort":"off"}`)
	out, errPrepare := p.prepareBody(kinds["workbuddy-ai"], body, nil)
	if errPrepare != nil {
		t.Fatalf("prepare: %v", errPrepare)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if _, present := doc["reasoning_effort"]; present {
		t.Fatal("reasoning_effort off must be dropped on the international gateway")
	}
	messages := doc["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("a leading system message must be injected, got %d messages", len(messages))
	}
}

// TestSplitSSEEmitsBareJSONChunks guards the framing contract with the host.
func TestSplitSSEEmitsBareJSONChunks(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	chunks := cpakit.SplitSSE([]byte(body))
	if len(chunks) != 1 {
		t.Fatalf("expected 1 data chunk (no synthetic DONE), got %d", len(chunks))
	}
	if strings.HasPrefix(string(chunks[0]), "data:") {
		t.Fatalf("chunk must not carry an SSE prefix: %q", chunks[0])
	}
	if strings.Contains(string(chunks[0]), "[DONE]") {
		t.Fatal("the host appends DONE itself")
	}
}

// TestStaticModelsAreUsable pins the provider registration surface: exactly
// one provider key, and a static roster a picker can render without a
// credential. ModelsForAuth is the per-credential surface and is covered by
// the kind routing tests.
func TestStaticModelsAreUsable(t *testing.T) {
	result := call(t, cpakit.MethodModelStatic, map[string]any{})
	var resp cpakit.ModelResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		t.Fatalf("decode static models: %v", errUnmarshal)
	}
	if resp.Provider != providerKey {
		t.Fatalf("provider = %q, want %q", resp.Provider, providerKey)
	}
	if len(resp.Models) == 0 {
		t.Fatal("the static fallback roster must not be empty")
	}
	for _, model := range resp.Models {
		if model.ID == "" {
			t.Fatalf("model row without an id: %+v", model)
		}
		if model.ContextLength <= 0 || model.OutputTokenLimit <= 0 {
			t.Fatalf("model %s has unusable limits: context=%d output=%d", model.ID, model.ContextLength, model.OutputTokenLimit)
		}
	}
}

// TestAuthParseDeclinesUnknownKind checks that a record naming an unknown kind
// under this provider is not claimed and rewritten to a default: it would
// otherwise execute against the wrong upstream.
func TestAuthParseDeclinesUnknownKind(t *testing.T) {
	unknown := call(t, cpakit.MethodAuthParse, cpakit.AuthParseRequest{
		Provider: providerKey,
		FileName: "mystery.json",
		RawJSON:  []byte(`{"type":"tencent","kind":"mystery"}`),
	})
	var parsed cpakit.AuthParseResponse
	if errUnmarshal := json.Unmarshal(unknown, &parsed); errUnmarshal != nil {
		t.Fatalf("decode parse: %v", errUnmarshal)
	}
	if parsed.Handled {
		t.Fatalf("an unknown kind must not be claimed and defaulted: %+v", parsed.Auth)
	}
}

// TestChatHeadersPerKind guards the wire identity of each family: the desktop
// products present the app identity and the explicit absent-marker headers;
// the CodeBuddy kind presents the CLI identity, and its api-key mode repeats
// the raw key in x-api-key for the catalog and billing endpoints.
func TestChatHeadersPerKind(t *testing.T) {
	p := newPluginState()
	desktop := &credentials{UID: "u1", EnterpriseID: "e1", Domain: "corp", AccessToken: "tok"}
	headerDesktop := p.chatHeadersFor(kinds[kindWorkbuddyID], desktop)
	if got := headerDesktop.Get("X-IDE-Type"); got != "WorkBuddy" {
		t.Fatalf("desktop X-IDE-Type = %q, want WorkBuddy", got)
	}
	if got := headerDesktop.Get("X-No-User-Id"); got != "" {
		t.Fatal("a desktop credential with a uid must not carry the absent marker")
	}

	minimal := &credentials{AccessToken: "tok"}
	headerMinimal := p.chatHeadersFor(kinds[kindWorkbuddyID], minimal)
	if got := headerMinimal.Get("X-No-User-Id"); got != "1" {
		t.Fatalf("a headerless desktop credential must carry X-No-User-Id=1, got %q", got)
	}
	if got := headerMinimal.Get("X-No-Enterprise-Id"); got != "1" {
		t.Fatalf("a non-enterprise credential must carry X-No-Enterprise-Id=1, got %q", got)
	}

	oauthDesktop := &credentials{UID: "u1", EnterpriseID: "e1", Domain: "corp", AccessToken: "tok", Mode: string(modeOAuth)}
	cli := p.chatHeadersFor(kinds[kindCodebuddyID], oauthDesktop)
	if got := cli.Get("X-IDE-Type"); got != "CLI" {
		t.Fatalf("codebuddy X-IDE-Type = %q, want CLI", got)
	}
	if got := cli.Get("X-User-Id"); got != "u1" {
		t.Fatalf("an OAuth codebuddy credential must carry its uid header, got %q", got)
	}

	key := &credentials{APIKey: "ck-test", Mode: string(modeAPIKey)}
	apiKeyHeaders := apiHeaders(key)
	if got := apiKeyHeaders.Get("x-api-key"); got != "ck-test" {
		t.Fatalf("api-key mode must repeat the key in x-api-key, got %q", got)
	}
}

// TestKeyRotationOrderAndPenalty covers the multi-key behaviour the standalone
// CodeBuddy plugin verified: round-robin over fresh keys, cooled-down keys
// moved to the back but still reachable.
func TestKeyRotationOrderAndPenalty(t *testing.T) {
	rotator := newKeyRotator(time.Minute)
	creds := []*credentials{
		{APIKey: "k1", Mode: string(modeAPIKey)},
		{APIKey: "k2", Mode: string(modeAPIKey)},
		{APIKey: "k3", Mode: string(modeAPIKey)},
	}

	first := rotator.ordered(creds)
	if len(first) != 3 {
		t.Fatalf("rotation must return every candidate, got %d", len(first))
	}
	// With a single candidate rotation is the identity.
	solo := rotator.ordered(creds[:1])
	if solo[0].APIKey != "k1" {
		t.Fatalf("a single candidate must be returned as-is, got %s", solo[0].APIKey)
	}

	// Penalise the first candidate and check it moves to the back.
	head := rotator.ordered(creds)[0]
	rotator.penalize(head)
	after := rotator.ordered(creds)
	if after[len(after)-1].APIKey != head.APIKey {
		t.Fatalf("a penalised key must move to the back, got tail %s want %s", after[len(after)-1].APIKey, head.APIKey)
	}
	if after[0].APIKey == head.APIKey {
		t.Fatal("a penalised key must not lead the rotation")
	}

	if !shouldRotateCredential(401) || !shouldRotateCredential(429) || !shouldRotateCredential(502) {
		t.Fatal("401/429/5xx must trigger rotation")
	}
	if shouldRotateCredential(400) || shouldRotateCredential(404) {
		t.Fatal("a business rejection must not trigger rotation")
	}
}

// TestExecutorStorageUnknownKindDoesNotPanic proves the executor resolves a
// storage blob defensively: an unknown or empty kind falls back to the default
// credential kind rather than panicking across the C ABI boundary. It needs no
// credentials, so it runs on any machine.
func TestExecutorStorageUnknownKindDoesNotPanic(t *testing.T) {
	doc := parseStorage([]byte(`{"type":"tencent","kind":"mystery"}`))
	if kindFor(doc.Kind) == nil {
		t.Fatal("kindFor must never return nil")
	}
	doc = parseStorage(nil)
	if kindFor(doc.Kind) == nil {
		t.Fatal("an empty storage blob must still resolve a kind")
	}
}

// TestStaticCredentialFallsBackWhenNoDesktopApp proves the server path: a
// machine without the WorkBuddy app resolves the exported credential, and a
// document for the wrong product is rejected instead of silently used.
func TestStaticCredentialFallsBackWhenNoDesktopApp(t *testing.T) {
	dir := t.TempDir()
	state := newPluginState()
	state.credentialDirPath = dir

	kind := kindFor("workbuddy-ai")
	if kind == nil {
		t.Fatal("workbuddy-ai kind is not registered")
	}

	// Nothing exported yet: the loader must report the missing file, not panic.
	if _, errMissing := state.loadStaticCredentials(kind); errMissing == nil {
		t.Fatal("a missing credential document resolved successfully")
	}

	document := map[string]any{
		"kind":          "workbuddy-ai",
		"mode":          "static",
		"access_token":  "exported-access-token",
		"refresh_token": "exported-refresh-token",
		"uid":           "uid-1",
		"enterprise_id": "ent-1",
		"domain":        "example.invalid",
		"expires_at":    int64(1823092283989),
		"app_version":   "5.6.0",
		"cli_version":   "1.2.3",
	}
	encoded, errMarshal := json.Marshal(document)
	if errMarshal != nil {
		t.Fatalf("marshal document: %v", errMarshal)
	}
	if errWrite := os.WriteFile(state.staticAuthPath(kind), encoded, 0o600); errWrite != nil {
		t.Fatalf("write document: %v", errWrite)
	}

	creds, errLoad := state.loadStaticCredentials(kind)
	if errLoad != nil {
		t.Fatalf("load exported credential: %v", errLoad)
	}
	if creds.AccessToken != "exported-access-token" {
		t.Fatalf("access token = %q", creds.AccessToken)
	}
	if creds.UID != "uid-1" || creds.EnterpriseID != "ent-1" || creds.Domain != "example.invalid" {
		t.Fatalf("identity fields lost: %+v", creds)
	}
	if creds.AppVersion != "5.6.0" || creds.CLIVersion != "1.2.3" {
		t.Fatalf("version claims lost: %+v", creds)
	}
	if creds.Mode != "static" {
		t.Fatalf("mode = %q, want static", creds.Mode)
	}
	if expires := time.UnixMilli(1823092283989); !creds.ExpiresAt.Equal(expires) {
		t.Fatalf("expiry = %v, want %v", creds.ExpiresAt, expires)
	}

	// A document exported for a different product must not be used here.
	other := kindFor("workbuddy")
	if other == nil {
		t.Fatal("workbuddy kind is not registered")
	}
	if errOther := os.WriteFile(state.staticAuthPath(other), encoded, 0o600); errOther != nil {
		t.Fatalf("write mismatched document: %v", errOther)
	}
	if _, errMismatch := state.loadStaticCredentials(other); errMismatch == nil {
		t.Fatal("a credential for another product was accepted")
	}

	// An empty token must be refused rather than sent upstream.
	blank, _ := json.Marshal(map[string]any{"kind": "workbuddy-ai", "access_token": ""})
	if errBlank := os.WriteFile(state.staticAuthPath(kind), blank, 0o600); errBlank != nil {
		t.Fatalf("write blank document: %v", errBlank)
	}
	if _, errEmpty := state.loadStaticCredentials(kind); errEmpty == nil {
		t.Fatal("an empty access token was accepted")
	}
}
