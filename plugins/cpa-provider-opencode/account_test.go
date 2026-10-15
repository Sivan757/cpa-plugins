package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// TestPlaceholderToolsDeclareProperties is the regression guard for a live 403.
//
// The free tier refuses the whole request when a required tool declares an empty
// parameter schema, even with the account credential and workspace id correct.
// The property names are not checked upstream, but the tool name is, so each
// required tool needs a non-empty schema under its own name.
func TestPlaceholderToolsDeclareProperties(t *testing.T) {
	for _, name := range requiredTools {
		tool := placeholderTool(name)
		function, okFunction := tool["function"].(map[string]any)
		if !okFunction {
			t.Fatalf("%s: no function object", name)
		}
		if function["name"] != name {
			t.Fatalf("%s: declared as %v", name, function["name"])
		}
		parameters, okParameters := function["parameters"].(map[string]any)
		if !okParameters {
			t.Fatalf("%s: no parameters", name)
		}
		properties, okProperties := parameters["properties"].(map[string]any)
		if !okProperties || len(properties) == 0 {
			t.Fatalf("%s: declares no properties, which the free tier refuses", name)
		}
	}
}

// TestEnsureRequiredToolsFillsBothNames proves padding adds a property-bearing
// declaration for each missing required tool and leaves a qualifying list alone.
func TestEnsureRequiredToolsFillsBothNames(t *testing.T) {
	filled := ensureRequiredTools(nil)
	if len(filled) != len(requiredTools) {
		t.Fatalf("padded to %d tools, want %d", len(filled), len(requiredTools))
	}
	for _, entry := range filled {
		object, _ := entry.(map[string]any)
		function, _ := object["function"].(map[string]any)
		parameters, _ := function["parameters"].(map[string]any)
		properties, _ := parameters["properties"].(map[string]any)
		if len(properties) == 0 {
			t.Fatalf("%v was padded without properties", function["name"])
		}
	}

	// A caller-supplied list that already qualifies keeps its own entries.
	existing := []any{map[string]any{"type": "function", "function": map[string]any{"name": "bash", "parameters": map[string]any{"properties": map[string]any{"command": map[string]any{"type": "string"}}}}}}
	merged := ensureRequiredTools(existing)
	if len(merged) != len(existing)+1 {
		t.Fatalf("merged to %d tools, want %d", len(merged), len(existing)+1)
	}
	if merged[0].(map[string]any)["function"].(map[string]any)["name"] != "bash" {
		t.Fatal("the caller's declaration must keep its position")
	}
}

// TestFreeTierHeadersCarryAccountCredential pins the wire shape the free tier
// accepts: the session token as the bearer plus the workspace id in its own
// header.
func TestFreeTierHeadersCarryAccountCredential(t *testing.T) {
	state := newPluginState()
	state.account = &accountAuth{access: "st_test_token", orgID: "wrk_test_org", expires: time.Now().Add(24 * time.Hour)}
	plan := &executionPlan{tier: tierFree, baseURL: state.freeRoot(), key: "st_test_token", orgID: "wrk_test_org", model: "mimo-v2.6-flash-free"}

	headers := state.requestHeaders(plan, "conversation")
	if got := headers.Get("Authorization"); got != "Bearer st_test_token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get(orgHeader); got != "wrk_test_org" {
		t.Fatalf("%s = %q", orgHeader, got)
	}
	if got := headers.Get("x-opencode-session"); got == "" {
		t.Fatal("the session identifier is missing")
	}
}

// TestPlanForUsesTheAccountCredential proves planFor resolves the free tier to
// the account token rather than the retired anonymous one.
func TestPlanForUsesTheAccountCredential(t *testing.T) {
	state := newPluginState()
	state.account = &accountAuth{access: "st_from_state", orgID: "wrk_from_state", expires: time.Now().Add(24 * time.Hour)}
	state.freeEnabled = true

	plan, errPlan := state.planFor(context.Background(), cpakit.ExecutorRequest{
		AuthProvider:   providerKey,
		AuthAttributes: map[string]string{"tier": tierFree},
		Model:          "mimo-v2.6-flash-free",
	})
	if errPlan != nil {
		t.Fatalf("planFor: %v", errPlan)
	}
	if plan.key != "st_from_state" {
		t.Fatalf("key = %q, want the account token", plan.key)
	}
	if plan.orgID != "wrk_from_state" {
		t.Fatalf("orgID = %q", plan.orgID)
	}
	if plan.baseURL != freeBaseURL {
		t.Fatalf("baseURL = %q", plan.baseURL)
	}
}

// TestPlanForKeepsTheAnonymousFallback proves an unconfigured account does not
// fail locally: the request reaches upstream so the refusal names the real cause.
func TestPlanForKeepsTheAnonymousFallback(t *testing.T) {
	state := newPluginState()
	state.account = &accountAuth{}
	plan, errPlan := state.planFor(context.Background(), cpakit.ExecutorRequest{
		AuthProvider:   providerKey,
		AuthAttributes: map[string]string{"tier": tierFree},
		Model:          "mimo-v2.6-flash-free",
	})
	if errPlan != nil {
		t.Fatalf("planFor: %v", errPlan)
	}
	if plan.key != publicCredential {
		t.Fatalf("key = %q, want the anonymous fallback", plan.key)
	}
}

// TestGoTierIsUnaffectedByTheAccountCredential proves the paid plan still uses
// its own API key.
func TestGoTierIsUnaffectedByTheAccountCredential(t *testing.T) {
	state := newPluginState()
	state.account = &accountAuth{access: "st_state", orgID: "wrk_state", expires: time.Now().Add(24 * time.Hour)}
	plan, errPlan := state.planFor(context.Background(), cpakit.ExecutorRequest{
		AuthProvider:   providerKey,
		AuthAttributes: map[string]string{"tier": tierGo, "api_key": "oc_sk_paid"},
		Model:          "deepseek-v4.1-flash",
	})
	if errPlan != nil {
		t.Fatalf("planFor: %v", errPlan)
	}
	if plan.key != "oc_sk_paid" {
		t.Fatalf("go key = %q", plan.key)
	}
	if plan.baseURL != goBaseURL {
		t.Fatalf("go baseURL = %q", plan.baseURL)
	}
}

// usePlainTransport points the renewal call at a real local server instead of the
// host bridge, which exists only inside CPA. It restores the previous transport
// when the test ends.
func usePlainTransport(t *testing.T) {
	t.Helper()
	previous := accountHTTPJSON
	accountHTTPJSON = func(ctx context.Context, method, url string, headers http.Header, requestBody any, out any) (*cpakit.HTTPResult, error) {
		var body []byte
		if requestBody != nil {
			encoded, errMarshal := json.Marshal(requestBody)
			if errMarshal != nil {
				return nil, errMarshal
			}
			body = encoded
		}
		request, errRequest := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if errRequest != nil {
			return nil, errRequest
		}
		for key, values := range headers {
			for _, value := range values {
				request.Header.Set(key, value)
			}
		}
		response, errDo := http.DefaultClient.Do(request)
		if errDo != nil {
			return nil, errDo
		}
		defer response.Body.Close()
		payload, errRead := io.ReadAll(response.Body)
		if errRead != nil {
			return nil, errRead
		}
		if out != nil && len(payload) > 0 {
			if errUnmarshal := json.Unmarshal(payload, out); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		return &cpakit.HTTPResult{StatusCode: response.StatusCode, Header: response.Header, Body: payload}, nil
	}
	t.Cleanup(func() { accountHTTPJSON = previous })
}

// TestAccountRefreshRotatesAndPersists drives a renewal against a fake console
// endpoint and proves both the new pair and the expiry reach disk, because the
// upstream invalidates the old refresh token on every rotation.
func TestAccountRefreshRotatesAndPersists(t *testing.T) {
	usePlainTransport(t)
	var sawRequest map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s", request.Method)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		if errDecode := json.NewDecoder(request.Body).Decode(&sawRequest); errDecode != nil {
			t.Errorf("decode request: %v", errDecode)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"st_rotated","refresh_token":"rt_rotated","token_type":"Bearer","expires_in":2592000}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	auth := &accountAuth{dir: dir, refresh: "rt_original", orgID: "wrk_org", expires: time.Now().Add(-time.Hour)}
	if errRenew := auth.renewAgainst(context.Background(), server.URL); errRenew != nil {
		t.Fatalf("renew: %v", errRenew)
	}

	if sawRequest["grant_type"] != "refresh_token" {
		t.Fatalf("grant_type = %v", sawRequest["grant_type"])
	}
	if sawRequest["refresh_token"] != "rt_original" {
		t.Fatalf("refresh_token = %v", sawRequest["refresh_token"])
	}
	if sawRequest["client_id"] != refreshClientID {
		t.Fatalf("client_id = %v", sawRequest["client_id"])
	}

	access, org, errCredentials := auth.credentials(context.Background())
	if errCredentials != nil {
		t.Fatalf("credentials: %v", errCredentials)
	}
	if access != "st_rotated" {
		t.Fatalf("access = %q", access)
	}
	if org != "wrk_org" {
		t.Fatalf("org = %q", org)
	}

	// The rotated pair must be on disk: the last rotation invalidated the token
	// it replaced, so losing this file loses the account.
	raw, errRead := os.ReadFile(filepath.Join(dir, "opencode-account.json"))
	if errRead != nil {
		t.Fatalf("read persisted credential: %v", errRead)
	}
	var doc accountDocument
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		t.Fatalf("decode persisted credential: %v", errUnmarshal)
	}
	if doc.AccessToken != "st_rotated" || doc.RefreshToken != "rt_rotated" {
		t.Fatalf("persisted tokens = %q / %q", doc.AccessToken, doc.RefreshToken)
	}
	if doc.OrgID != "wrk_org" {
		t.Fatalf("persisted org = %q", doc.OrgID)
	}
	if doc.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("persisted expiry is not in the future: %d", doc.ExpiresAt)
	}

	// A second load from the same directory must pick the rotated pair up, which
	// is what makes a restart survive a rotation.
	reloaded := &accountAuth{dir: dir}
	reloaded.load()
	if reloaded.refresh != "rt_rotated" || reloaded.access != "st_rotated" {
		t.Fatalf("reloaded = %q / %q", reloaded.access, reloaded.refresh)
	}
}

// TestAccountRefreshFailureIsReported proves a refused renewal surfaces the
// upstream status instead of silently sending a dead token.
func TestAccountRefreshFailureIsReported(t *testing.T) {
	usePlainTransport(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":"invalid_grant","error_description":"The grant is invalid"}`))
	}))
	defer server.Close()

	auth := &accountAuth{dir: t.TempDir(), refresh: "rt_dead"}
	errRenew := auth.renewAgainst(context.Background(), server.URL)
	if errRenew == nil {
		t.Fatal("a refused renewal was reported as success")
	}
	if !strings.Contains(errRenew.Error(), "400") {
		t.Fatalf("error does not carry the status: %v", errRenew)
	}
	// With no usable token the request must fail rather than send a dead bearer.
	if _, _, errCredentials := auth.credentials(context.Background()); errCredentials == nil {
		t.Fatal("credentials succeeded with no token and no usable refresh token")
	}
}

// TestAccountApplyPrefersConfiguredValues proves an operator can replace a
// credential from the configuration while a persisted rotation still loads.
func TestAccountApplyPrefersConfiguredValues(t *testing.T) {
	dir := t.TempDir()
	seed := accountDocument{Type: providerKey, AccessToken: "st_disk", RefreshToken: "rt_disk", OrgID: "wrk_disk"}
	encoded, _ := json.Marshal(seed)
	if errWrite := os.WriteFile(filepath.Join(dir, "opencode-account.json"), encoded, 0o600); errWrite != nil {
		t.Fatalf("seed: %v", errWrite)
	}

	auth := &accountAuth{dir: dir}
	if !auth.load() {
		t.Fatal("load reported no credential")
	}
	if auth.access != "st_disk" || auth.orgID != "wrk_disk" {
		t.Fatalf("load = %q / %q", auth.access, auth.orgID)
	}

	// The file outranks the configuration, because it holds the rotation. A stale
	// configured refresh token must not replace it.
	auth.apply("st_config", "rt_config", "wrk_config", false)
	if auth.access != "st_disk" || auth.refresh != "rt_disk" || auth.orgID != "wrk_disk" {
		t.Fatalf("persisted credential was overwritten: %q / %q / %q", auth.access, auth.refresh, auth.orgID)
	}

	// A gap the file leaves is still filled from the configuration.
	auth.apply("", "", "wrk_filled", false)
	if auth.orgID != "wrk_disk" {
		t.Fatalf("a present value was replaced: %q", auth.orgID)
	}
	sparse := &accountAuth{dir: t.TempDir(), refresh: ""}
	sparse.apply("", "rt_from_config", "wrk_from_config", false)
	if sparse.refresh != "rt_from_config" || sparse.orgID != "wrk_from_config" {
		t.Fatalf("gaps were not filled: %q / %q", sparse.refresh, sparse.orgID)
	}

	// On a first run there is no file, so the configuration wins outright.
	fresh := &accountAuth{dir: t.TempDir()}
	if fresh.load() {
		t.Fatal("an empty directory reported a credential")
	}
	fresh.apply("st_config", "rt_config", "wrk_config", true)
	if fresh.access != "st_config" || fresh.refresh != "rt_config" {
		t.Fatalf("first-run configuration was ignored: %q / %q", fresh.access, fresh.refresh)
	}
}
