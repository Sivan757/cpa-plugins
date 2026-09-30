package main

import (
	"testing"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// TestBuildModelsUsesTheLowercaseDisplayNameAsTheID pins the picker contract: the
// management center shows the model id as the row title, so the id must be the
// human-readable name and the upstream routing key must travel alongside it.
func TestBuildModelsUsesTheLowercaseDisplayNameAsTheID(t *testing.T) {
	entries, _, errParse := parseCatalogDocument([]byte(`{"assistant":[{"key":"dmodel","enable":true,"display_name":"DeepSeek-V4-Pro","max_input_tokens":200000,"max_output_tokens":32768}]}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	models, routes := buildModels(regionFor("china"), entries)
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	model := models[0]
	if model.ID != "deepseek-v4-pro" {
		t.Fatalf("id = %q, want the display name as the title", model.ID)
	}
	if model.Name != "dmodel" {
		t.Fatalf("Name = %q, want the upstream routing key", model.Name)
	}
	if routes["deepseek-v4-pro"] != "dmodel" {
		t.Fatalf("route map = %v, want DeepSeek-V4-Pro -> dmodel", routes)
	}
}

// TestRouteKeyResolvesThroughTheCache checks the executor's id-to-key mapping,
// including the pass-through for an unknown id.
func TestRouteKeyResolvesThroughTheCache(t *testing.T) {
	p := newPluginState()
	p.cache.put(
		[]pluginkit.ModelInfo{{ID: "DeepSeek-V4-Pro", Name: "dmodel"}},
		map[string]string{"DeepSeek-V4-Pro": "dmodel"},
	)
	if got := p.routeKey("DeepSeek-V4-Pro"); got != "dmodel" {
		t.Fatalf("routeKey = %q, want dmodel", got)
	}
	if got := p.routeKey("not-in-catalog"); got != "not-in-catalog" {
		t.Fatalf("an unknown id must pass through, got %q", got)
	}
}

// TestStaticFallbackRoutesIdsToThemselves checks the fallback roster.
func TestStaticFallbackRoutesIdsToThemselves(t *testing.T) {
	models, routes := staticFallback(regionFor("china"))
	if len(models) == 0 {
		t.Fatal("the fallback roster must not be empty")
	}
	for _, model := range models {
		if routes[model.ID] != model.ID {
			t.Fatalf("fallback id %q does not route to itself", model.ID)
		}
	}
}

// TestBuildModelsHidesRowsWithoutAnIdentifiableVendor covers the internal
// routing slots (auto, tier names): without a vendor they cannot be attributed,
// so they are not advertised.
func TestBuildModelsHidesRowsWithoutAnIdentifiableVendor(t *testing.T) {
	entries, _, errParse := parseCatalogDocument([]byte(`{"assistant":[
		{"key":"auto","enable":true,"display_name":"Auto"},
		{"key":"cmodel","enable":true,"display_name":"C"},
		{"key":"efficient","enable":true,"display_name":"Efficient"},
		{"key":"ultimate","enable":true,"display_name":"Ultimate"},
		{"key":"performance","enable":true,"display_name":"Performance"},
		{"key":"lite","enable":true,"display_name":"Lite"},
		{"key":"dfmodel","enable":true,"display_name":"DeepSeek-Flash"}
	]}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	models, routes := buildModels(regionFor("china"), entries)
	if len(models) != 1 {
		t.Fatalf("expected only the vendor-attributable row, got %d: %+v", len(models), models)
	}
	if models[0].ID != "deepseek-flash" {
		t.Fatalf("id = %q, want deepseek-flash", models[0].ID)
	}
	if routes["deepseek-flash"] != "dfmodel" {
		t.Fatalf("route = %q, want dfmodel", routes["deepseek-flash"])
	}
}

// TestBuildModelsLowercasesMixedCaseDisplayNames covers requirement 1.
func TestBuildModelsLowercasesMixedCaseDisplayNames(t *testing.T) {
	entries, _, errParse := parseCatalogDocument([]byte(`{"assistant":[
		{"key":"gmodel","enable":true,"display_name":"GLM-5.3"},
		{"key":"kmodel_latest","enable":true,"display_name":"Kimi-K3"},
		{"key":"mmodel","enable":true,"display_name":"MiniMax-M2.7"},
		{"key":"qmodel_38max","enable":true,"display_name":"Qwen3.8-Max"}
	]}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	models, _ := buildModels(regionFor("china"), entries)
	want := []string{"glm-5.3", "kimi-k3", "minimax-m2.7", "qwen3.8-max"}
	if len(models) != len(want) {
		t.Fatalf("expected %d models, got %d", len(want), len(models))
	}
	for index, model := range models {
		if model.ID != want[index] {
			t.Fatalf("model %d id = %q, want %q", index, model.ID, want[index])
		}
	}
}
