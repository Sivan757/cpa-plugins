package main

import "testing"

// TestConfigureRevertsExpose proves an allowlist clears when the key leaves the
// configuration. Assigning only when the key is present leaves the previous list
// in force, which silently hides models.
func TestConfigureRevertsExpose(t *testing.T) {
	state := newPluginState()
	gateway := &gateway{impl: state}

	if errConfigure := gateway.Configure([]byte("expose:\n  - \"qwen3.8-flash\"\n")); errConfigure != nil {
		t.Fatalf("configure: %v", errConfigure)
	}
	if len(state.expose) != 1 || state.expose[0] != "qwen3.8-flash" {
		t.Fatalf("expose = %v", state.expose)
	}

	if errConfigure := gateway.Configure([]byte("enabled: true\n")); errConfigure != nil {
		t.Fatalf("reconfigure: %v", errConfigure)
	}
	if len(state.expose) != 0 {
		t.Fatalf("expose survived removal: %v", state.expose)
	}
}
