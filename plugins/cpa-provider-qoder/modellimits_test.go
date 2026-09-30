package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// TestAdvertisedLimitsComeFromTheVendorSpec pins the two numbers a host budgets
// with. The catalog publishes only a default context tier, so reading it made a
// 1M-token model look like 200K, and it publishes no output limit at all.
func TestAdvertisedLimitsComeFromTheVendorSpec(t *testing.T) {
	raw, errRead := os.ReadFile("testdata/catalog.json")
	if errRead != nil {
		t.Skipf("catalog fixture unavailable: %v", errRead)
	}
	entries, _, errParse := parseCatalogDocument(raw)
	if errParse != nil {
		t.Fatalf("parse catalog: %v", errParse)
	}
	models, _ := buildModels(regionFor("china"), entries)
	byID := map[string]struct{ ctx, out int64 }{}
	for _, model := range models {
		byID[model.ID] = struct{ ctx, out int64 }{model.ContextLength, model.MaxCompletionTokens}
	}
	for id, want := range map[string]struct{ ctx, out int64 }{
		"qwen3.8-max":    {1_000_000, 131_072},
		"qwen3.8-flash":  {1_000_000, 131_072},
		"deepseek-flash": {1_000_000, 393_216},
	} {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("model %s was not advertised", id)
		}
		if got.ctx != want.ctx {
			t.Fatalf("%s context = %d, want %d (largest advertised tier)", id, got.ctx, want.ctx)
		}
		if got.out != want.out {
			t.Fatalf("%s output cap = %d, want %d (vendor spec)", id, got.out, want.out)
		}
	}
}

// TestTheRequestDeclaresTheAdvertisedBudget covers the other half of the
// contract: advertising 1M is only honest if the request actually asks for that
// tier, because the gateway otherwise falls back to its own default.
func TestTheRequestDeclaresTheAdvertisedBudget(t *testing.T) {
	limits := modelLimits{Window: 1_000_000, Output: 131_072}
	body := []byte(`{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}]}`)
	request := pluginkit.ExecutorRequest{Model: "qwen3.8-max", Payload: body}
	out, _, errPrepare := prepareChatBody(request, newTurnIdentity("uid", "sess"), "qmodel_38max", limits)
	if errPrepare != nil {
		t.Fatalf("prepare: %v", errPrepare)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	parameters, _ := doc["parameters"].(map[string]any)
	if parameters == nil {
		t.Fatal("the request must carry a parameters block")
	}
	if got, _ := parameters["context_length"].(float64); int64(got) != 1_000_000 {
		t.Fatalf("context_length = %v, want 1000000", parameters["context_length"])
	}
	if got, _ := parameters["max_tokens"].(float64); int64(got) != 131_072 {
		t.Fatalf("max_tokens = %v, want 131072", parameters["max_tokens"])
	}
}
