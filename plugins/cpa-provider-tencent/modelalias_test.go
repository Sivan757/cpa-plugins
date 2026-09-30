package main

import (
	"encoding/json"
	"testing"
)

// TestDeepSeekFlashIsAdvertisedCanonically pins the normalization: this gateway
// sells DeepSeek Flash as deepseek-v4.1-flash while the rest of the fleet calls
// it deepseek-flash. One model must appear once, under the canonical name, and
// the request must still reach the gateway under the identifier it routes on.
func TestDeepSeekFlashIsAdvertisedCanonically(t *testing.T) {
	if got := advertisedModelID("deepseek-v4.1-flash"); got != "deepseek-flash" {
		t.Fatalf("advertisedModelID = %q, want deepseek-flash", got)
	}
	if got := upstreamModelID("deepseek-flash"); got != "deepseek-v4.1-flash" {
		t.Fatalf("upstreamModelID = %q, want deepseek-v4.1-flash", got)
	}
	// Unrelated ids pass through unchanged in both directions.
	if got := advertisedModelID("GLM-5.3"); got != "glm-5.3" {
		t.Fatalf("advertisedModelID(GLM-5.3) = %q, want glm-5.3", got)
	}
	if got := upstreamModelID("not-in-catalog"); got != "not-in-catalog" {
		t.Fatalf("unknown ids must pass through, got %q", got)
	}
}

// TestPrepareBodyRewritesTheCanonicalModelID covers the wire contract: the
// upstream never sees the canonical alias.
func TestPrepareBodyRewritesTheCanonicalModelID(t *testing.T) {
	p := newPluginState()
	body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}`)
	out, errPrepare := p.prepareBody(kinds["workbuddy"], body, nil)
	if errPrepare != nil {
		t.Fatalf("prepare: %v", errPrepare)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if got, _ := doc["model"].(string); got != "deepseek-v4.1-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4.1-flash", got)
	}
}
