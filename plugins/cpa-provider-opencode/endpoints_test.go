package main

import "testing"

// TestUpstreamRootsMatchThePublishedContract pins the endpoints to the ones the
// Console API documentation names. The older /zen/... roots still answer, so a
// regression here would not fail any request; it would quietly rebuild a
// dependency on a path that is not the documented contract.
func TestUpstreamRootsMatchThePublishedContract(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"free chat", freeBaseURL, "https://opencode.ai/inference/openai/v1"},
		{"free catalogue", freeModelsURL, "https://opencode.ai/inference/v1/models"},
		{"go base", goBaseURL, "https://opencode.ai/inference/go/v1"},
	}
	for _, testCase := range cases {
		if testCase.got != testCase.want {
			t.Fatalf("%s = %q, want %q", testCase.name, testCase.got, testCase.want)
		}
	}
}

// TestCatalogueURLDiffersFromTheChatRoot proves the two free-tier paths stay
// distinct: chat lives under /inference/openai/v1 and the model list under
// /inference/v1. Using the chat root for the list answers 404.
func TestCatalogueURLDiffersFromTheChatRoot(t *testing.T) {
	state := newPluginState()
	if got := state.freeCatalogueURL(); got != freeModelsURL {
		t.Fatalf("catalogue URL = %q, want %q", got, freeModelsURL)
	}
	if got := state.freeRoot(); got != freeBaseURL {
		t.Fatalf("chat root = %q, want %q", got, freeBaseURL)
	}
	if state.freeRoot() == state.freeCatalogueURL() {
		t.Fatal("the chat root and the catalogue URL must differ")
	}

	// An explicit override targets one root, so the catalogue follows it. That is
	// what keeps a local capture endpoint usable.
	state.baseURL = "http://127.0.0.1:18999/zen/v1"
	if got := state.freeCatalogueURL(); got != "http://127.0.0.1:18999/zen/v1/models" {
		t.Fatalf("overridden catalogue URL = %q", got)
	}
	if got := state.freeRoot(); got != "http://127.0.0.1:18999/zen/v1" {
		t.Fatalf("overridden chat root = %q", got)
	}
}
