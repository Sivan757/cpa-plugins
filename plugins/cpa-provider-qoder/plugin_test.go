package main

import (
	"encoding/json"
	"testing"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

func init() { pluginkit.RegisterPlugin(newGateway()) }

// call drives one RPC exactly the way the host does and fails on an error
// envelope, so a test asserts only on decoded results.
func call(t *testing.T, method string, request any) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	out := pluginkit.HandleCall(method, raw)
	var env pluginkit.Envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope for %s: %v (raw: %s)", method, errUnmarshal, out)
	}
	if !env.OK {
		t.Fatalf("%s returned an error: %+v", method, env.Error)
	}
	return env.Result
}

func TestRegisterDeclaresExpectedCapabilities(t *testing.T) {
	result := call(t, pluginkit.MethodPluginRegister, map[string]any{})
	var reg struct {
		SchemaVersion uint32
		Metadata      pluginkit.Metadata
		Capabilities  pluginkit.Capabilities
	}
	if errUnmarshal := json.Unmarshal(result, &reg); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}
	if reg.Metadata.Name == "" || reg.Metadata.Version == "" {
		t.Fatalf("incomplete metadata: %+v", reg.Metadata)
	}
	// The plugin is configured with a PAT, so it must not claim an OAuth
	// login flow it cannot drive.
	if !reg.Capabilities.QuotaProvider || !reg.Capabilities.ModelProvider || !reg.Capabilities.Executor {
		t.Fatalf("expected quota, model, and executor capabilities: %+v", reg.Capabilities)
	}
	if len(reg.Capabilities.ExecutorInputFormats) == 0 || len(reg.Capabilities.ExecutorOutputFormats) == 0 {
		t.Fatalf("executor must declare its protocols: %+v", reg.Capabilities)
	}
}

func TestQuotaDescribeNamesThisProvider(t *testing.T) {
	result := call(t, pluginkit.MethodQuotaDescribe, map[string]any{})
	var desc pluginkit.QuotaDescribeResponse
	if errUnmarshal := json.Unmarshal(result, &desc); errUnmarshal != nil {
		t.Fatalf("decode describe: %v", errUnmarshal)
	}
	if len(desc.SupportedProviders) == 0 || desc.SupportedProviders[0] != providerKey {
		t.Fatalf("supported providers = %v, want [%s]", desc.SupportedProviders, providerKey)
	}
}

func TestAuthParseClaimsOwnTypeAndDeclinesOthers(t *testing.T) {
	own := call(t, pluginkit.MethodAuthParse, pluginkit.AuthParseRequest{
		Provider: providerKey,
		FileName: providerKey + ".json",
		RawJSON:  []byte(`{"type":"` + providerKey + `","pat":"pat-test"}`),
	})
	var parsed pluginkit.AuthParseResponse
	if errUnmarshal := json.Unmarshal(own, &parsed); errUnmarshal != nil {
		t.Fatalf("decode parse: %v", errUnmarshal)
	}
	if !parsed.Handled {
		t.Fatal("the plugin must claim an auth file whose type is its provider key")
	}
	// Unlike the desktop-app providers, a PAT has no app to re-read it from, so
	// it is deliberately persisted in the host auth record. Assert the round
	// trip instead of a mask, and that no other secret travels with it.
	var stored map[string]any
	if errUnmarshal := json.Unmarshal(parsed.Auth.StorageJSON, &stored); errUnmarshal != nil {
		t.Fatalf("decode storage JSON: %v", errUnmarshal)
	}
	if stored["type"] != providerKey {
		t.Fatalf("storage type = %v, want %s", stored["type"], providerKey)
	}
	if stored["pat"] != "pat-test" {
		t.Fatal("the PAT must round trip into the stored credential")
	}
	if _, hasToken := stored["job_token"]; hasToken {
		t.Fatal("the exchanged job token must not be persisted; it is short-lived")
	}

	other := call(t, pluginkit.MethodAuthParse, pluginkit.AuthParseRequest{
		Provider: "unrelated",
		RawJSON:  []byte(`{"type":"unrelated"}`),
	})
	var declined pluginkit.AuthParseResponse
	if errUnmarshal := json.Unmarshal(other, &declined); errUnmarshal != nil {
		t.Fatalf("decode parse: %v", errUnmarshal)
	}
	if declined.Handled {
		t.Fatal("the plugin must not claim another provider's auth file")
	}
}

// The static path deliberately returns an empty roster: a static list carries
// no credential, so per-account excluded models cannot be applied there and
// would re-advertise models the user hid.
func TestStaticModelsAreDeliberatelyEmpty(t *testing.T) {
	result := call(t, pluginkit.MethodModelStatic, map[string]any{})
	var resp pluginkit.ModelResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		t.Fatalf("decode static models: %v", errUnmarshal)
	}
	if resp.Provider != providerKey {
		t.Fatalf("provider = %q, want %q", resp.Provider, providerKey)
	}
	if len(resp.Models) != 0 {
		t.Fatalf("the static roster must be empty (exclusions apply on for_auth), got %d models", len(resp.Models))
	}
}
func TestManagementStatusIsSelfDescribing(t *testing.T) {
	// The route is registered under the plugin id, matching the library name.
	result := call(t, pluginkit.MethodManagementHandle, pluginkit.ManagementRequest{
		Method: "GET",
		Path:   "plugins/cpa-provider-qoder/status",
	})
	var response pluginkit.ManagementResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode management response: %v", errUnmarshal)
	}
	if response.StatusCode != 200 {
		t.Fatalf("status route returned HTTP %d: %s", response.StatusCode, response.Body)
	}
	var report map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &report); errUnmarshal != nil {
		t.Fatalf("decode status body: %v", errUnmarshal)
	}
	// The report keys the provider identity as providerKey, and must always say
	// whether a usable credential was resolved.
	if report["providerKey"] != providerKey {
		t.Fatalf("status providerKey = %v, want %s", report["providerKey"], providerKey)
	}
	if _, hasCredentialState := report["credential"]; !hasCredentialState {
		t.Fatalf("status must report credential state: %v", report)
	}
	if hint, _ := report["credentialHint"].(string); report["credential"] == "unconfigured" && hint == "" {
		t.Fatal("an unconfigured credential must carry a hint explaining how to fix it")
	}
}

func TestUnknownMethodIsReportedNotPanicked(t *testing.T) {
	out := pluginkit.HandleCall("no.such.method", []byte(`{}`))
	var env pluginkit.Envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("expected an unknown_method envelope, got %s", out)
	}
}

// TestCodecRoundTrip checks the WAF body encoding, which must survive a
// round trip because the upstream echoes it back encoded.
func TestCodecRoundTrip(t *testing.T) {
	payload := []byte(`{"model":"q37f","stream":true}`)
	encoded := qoderEncodeBody(payload)
	if encoded == "" {
		t.Fatal("encode produced nothing")
	}
	decoded, errDecode := qoderDecodeBody(encoded)
	if errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if string(decoded) != string(payload) {
		t.Fatalf("round trip mismatch: %q != %q", decoded, payload)
	}
}

// TestQuotaDescribeMustNotClaimAFabricatedValue pins the honesty rule learned
// from the upstream: quota for a personal account lives in addOnQuota, so a
// provider that only read userQuota would report zero while the account holds
// credit.
func TestFetchQuotaRequiresACredential(t *testing.T) {
	out := pluginkit.HandleCall(pluginkit.MethodQuotaFetch, []byte(`{"auth_index":"missing"}`))
	var env pluginkit.Envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.OK {
		t.Fatal("a quota fetch without a usable credential must not succeed")
	}
}
