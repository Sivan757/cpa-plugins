package main

import (
	"fmt"
	"testing"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

func TestZZProbeFilter(t *testing.T) {
	// The host config carries the exclusion list under the provider key.
	host := pluginkit.HostConfigSummary{
		ExcludedModels: map[string][]string{
			"tencent": {"glm-5.1", "kimi-k2.6", "minimax-m2.7"},
		},
	}
	models := staticFallback(kinds["workbuddy"])
	fmt.Printf("before=%d\n", len(models))
	filtered := pluginkit.FilterModelsByExclusion(models, pluginkit.ExcludedModelsFor(host, providerKey))
	fmt.Printf("after=%d\n", len(filtered))
	for _, m := range filtered {
		fmt.Printf("  kept: %s\n", m.ID)
	}
}
