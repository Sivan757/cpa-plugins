package main

import (
	"testing"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// TestBuildModelsUsesTheDisplayNameAsTheID pins the picker contract: the
// management center shows the model id as the row title, so the id must be the
// human-readable name and the upstream routing key must travel alongside it.
func TestBuildModelsUsesTheDisplayNameAsTheID(t *testing.T) {
	entries, _, errParse := parseCatalogDocument([]byte(`{"assistant":[{"key":"dmodel","enable":true,"display_name":"DeepSeek-V4-Pro","max_input_tokens":200000,"max_output_tokens":32768}]}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	models, routes := buildModels(regionFor("china"), entries)
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	model := models[0]
	if model.ID != "DeepSeek-V4-Pro" {
		t.Fatalf("id = %q, want the display name as the title", model.ID)
	}
	if model.Name != "dmodel" {
		t.Fatalf("Name = %q, want the upstream routing key", model.Name)
	}
	if routes["DeepSeek-V4-Pro"] != "dmodel" {
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
