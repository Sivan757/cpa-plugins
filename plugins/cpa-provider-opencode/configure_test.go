package main

import "testing"

// TestConfigureRevertsRemovedValues is the regression guard for a field that
// could not be cleared: assigning only when a key is present leaves the previous
// value behind, so an override outlived the configuration that set it.
func TestConfigureRevertsRemovedValues(t *testing.T) {
	state := newPluginState()
	state.account = &accountAuth{}
	gateway := &gateway{impl: state}

	full := []byte(`
api_key: "oc_sk_one,oc_sk_two"
free_enabled: false
expose:
  - "*-free"
base_url: "http://127.0.0.1:18999/zen/v1"
go_base_url: "http://127.0.0.1:18999/go/v1"
credential_dir: "/tmp/credentials"
`)
	if errConfigure := gateway.Configure(full); errConfigure != nil {
		t.Fatalf("configure: %v", errConfigure)
	}
	if len(state.apiKeys) != 2 {
		t.Fatalf("apiKeys = %v", state.apiKeys)
	}
	if state.freeEnabled {
		t.Fatal("free_enabled: false was ignored")
	}
	if len(state.expose) != 1 || state.expose[0] != "*-free" {
		t.Fatalf("expose = %v", state.expose)
	}
	if state.baseURL == "" || state.goURL == "" || state.credentialDir != "/tmp/credentials" {
		t.Fatalf("overrides not applied: %q %q %q", state.baseURL, state.goURL, state.credentialDir)
	}

	// Reconfigure with those keys removed. Every one of them must revert.
	minimal := []byte(`
enabled: true
priority: 10
`)
	if errConfigure := gateway.Configure(minimal); errConfigure != nil {
		t.Fatalf("reconfigure: %v", errConfigure)
	}
	if len(state.apiKeys) != 0 {
		t.Fatalf("apiKeys survived removal: %v", state.apiKeys)
	}
	if !state.freeEnabled {
		t.Fatal("free_enabled did not revert to its default")
	}
	if len(state.expose) != 0 {
		t.Fatalf("expose survived removal: %v", state.expose)
	}
	if state.baseURL != "" || state.goURL != "" {
		t.Fatalf("upstream override survived removal: %q %q", state.baseURL, state.goURL)
	}
	if state.credentialDir != "" {
		t.Fatalf("credential_dir survived removal: %q", state.credentialDir)
	}
	if state.freeRoot() != freeBaseURL || state.goRoot() != goBaseURL {
		t.Fatalf("roots did not revert: %q %q", state.freeRoot(), state.goRoot())
	}
}

// TestConfigureAcceptsBooleanFreeEnabled proves a real YAML boolean is honoured,
// not only the string form.
func TestConfigureAcceptsBooleanFreeEnabled(t *testing.T) {
	state := newPluginState()
	state.account = &accountAuth{}
	gateway := &gateway{impl: state}
	if errConfigure := gateway.Configure([]byte("free_enabled: false\n")); errConfigure != nil {
		t.Fatalf("configure: %v", errConfigure)
	}
	if state.freeEnabled {
		t.Fatal("a YAML boolean false was ignored")
	}
}
