package main

import "testing"

// TestConfigureRevertsExposeAndCredentialDir proves both keys clear when they
// leave the configuration. A stale credential_dir keeps the plugin reading a
// directory the operator no longer points at.
func TestConfigureRevertsExposeAndCredentialDir(t *testing.T) {
	state := newPluginState()
	gateway := &gateway{impl: state}

	full := []byte("expose:\n  - \"deepseek-flash\"\ncredential_dir: \"/tmp/creds\"\n")
	if errConfigure := gateway.Configure(full); errConfigure != nil {
		t.Fatalf("configure: %v", errConfigure)
	}
	if len(state.expose) != 1 || state.expose[0] != "deepseek-flash" {
		t.Fatalf("expose = %v", state.expose)
	}
	if state.credentialDirPath != "/tmp/creds" {
		t.Fatalf("credentialDirPath = %q", state.credentialDirPath)
	}

	if errConfigure := gateway.Configure([]byte("enabled: true\n")); errConfigure != nil {
		t.Fatalf("reconfigure: %v", errConfigure)
	}
	if len(state.expose) != 0 {
		t.Fatalf("expose survived removal: %v", state.expose)
	}
	if state.credentialDirPath != "" {
		t.Fatalf("credential_dir survived removal: %q", state.credentialDirPath)
	}
}
