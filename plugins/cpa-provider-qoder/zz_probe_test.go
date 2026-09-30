package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

func TestZZFilterQoder(t *testing.T) {
	p := newPluginState()
	p.cfg = &configState{pat: "pt-jAViJdzg4nD8bg50tEBRYKCH_01a0f017-f41d-76c9-b624-e915b722b7fb", region: "china"}
	creds, errCreds := p.resolveCredentials(context.Background(), []byte(`{"type":"qoder"}`))
	if errCreds != nil {
		t.Fatalf("resolve: %v", errCreds)
	}
	entries, _, errCatalog := readCatalog(context.Background(), creds, creds.Identity)
	if errCatalog != nil {
		t.Fatalf("catalog: %v", errCatalog)
	}
	models, routes := buildModels(creds.Region, entries)
	fmt.Printf("built=%d\n", len(models))
	excluded := []string{"deepseek-flash", "kimi-k3", "deepseek-v4-pro", "glm-5.2", "glm-5.3", "glm-5.3-flash", "kimi-k2.8-preview", "minimax-m2.7", "qwen3.7-flash", "qwen3.7-max", "qwen3.7-plus"}
	filtered := pluginkit.FilterModelsByExclusion(models, excluded)
	fmt.Printf("filtered=%d\n", len(filtered))
	for _, m := range filtered {
		fmt.Printf("  kept=%s route=%s\n", m.ID, routes[m.ID])
	}
}
