package main

import (
	"context"
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// catalogCache keeps one short-lived catalog snapshot so the model-list request
// the host makes before a chat does not hit the upstream twice.
type catalogCache struct {
	mu     sync.Mutex
	models []pluginkit.ModelInfo
	// routes maps the advertised model id to the upstream routing key.
	routes   map[string]string
	loadedAt time.Time
	ttl      time.Duration
}

func newCatalogCache(ttl time.Duration) *catalogCache {
	return &catalogCache{ttl: ttl}
}

func (c *catalogCache) get() ([]pluginkit.ModelInfo, map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.models) == 0 || c.ttl <= 0 || time.Since(c.loadedAt) > c.ttl {
		return nil, nil, false
	}
	return c.models, c.routes, true
}

func (c *catalogCache) put(models []pluginkit.ModelInfo, routes map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = models
	c.routes = routes
	c.loadedAt = time.Now()
}

func (c *catalogCache) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.models)
}

// setTTL resizes the cache window from the plugin config.
func (c *catalogCache) setTTL(ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl = ttl
}

// ttlSeconds reports the configured window for diagnostics.
func (c *catalogCache) ttlSeconds() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttl.Seconds()
}

// modelsForAuth discovers the live roster for one credential.
func (p *pluginState) modelsForAuth(ctx context.Context, storageJSON []byte) ([]pluginkit.ModelInfo, error) {
	if cached, _, okCache := p.cache.get(); okCache {
		return cached, nil
	}
	creds, errCreds := p.resolveCredentials(ctx, storageJSON)
	if errCreds != nil {
		return nil, errCreds
	}
	entries, _, errCatalog := readCatalog(ctx, creds, creds.Identity)
	if errCatalog != nil {
		return nil, errCatalog
	}
	models, routes := buildModels(creds.Region, entries)
	if len(models) == 0 {
		return nil, pluginkit.NewError("upstream_error", "the Qoder catalog contained no enabled models", 502)
	}
	p.cache.put(models, routes)
	return models, nil
}

// staticFallback is the last-resort roster used when the catalog is unreachable.
//
// An empty list would remove the provider from the picker entirely, which is
// worse than a conservative list. The identifiers come from the reference
// client's built-in defaults and carry placeholder limits because the plugin
// cannot verify real ones offline.
func staticFallback(reg *region) ([]pluginkit.ModelInfo, map[string]string) {
	ids := []struct {
		id      string
		context int64
	}{
		{"qwen3.8-max", 1_000_000},
		{"qwen3.7-plus", 200_000},
		{"deepseek-v4-pro", 200_000},
		{"deepseek-flash", 180_000},
		{"glm-5.3", 200_000},
		{"glm-5.2", 200_000},
		{"kimi-k3", 260_000},
		{"minimax-m2.7", 200_000},
	}
	out := make([]pluginkit.ModelInfo, 0, len(ids))
	routes := make(map[string]string, len(ids))
	for _, item := range ids {
		routes[item.id] = item.id
		out = append(out, pluginkit.ModelInfo{
			ID:                        item.id,
			Object:                    "model",
			OwnedBy:                   reg.displayName,
			DisplayName:               item.id,
			Name:                      item.id,
			Description:               "静态兜底清单：Qoder 目录不可达",
			ContextLength:             item.context,
			InputTokenLimit:           item.context,
			OutputTokenLimit:          defaultOutputFallback,
			MaxCompletionTokens:       defaultOutputFallback,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
			SupportedParameters:       []string{"tools"},
		})
	}
	return out, routes
}

// mustStaticRoutes is the StaticModels/RegisterModels shape of staticFallback:
// those RPCs return only the roster, so the route map is folded back into the
// ids (which are already the routing keys for the fallback list).
func mustStaticRoutes(reg *region) []pluginkit.ModelInfo {
	models, _ := staticFallback(reg)
	return models
}

// routeKey maps an advertised model id to the upstream routing key. Unknown ids
// pass through unchanged so an explicit client request still reaches the
// gateway and surfaces the upstream's own error.
func (p *pluginState) routeKey(id string) string {
	if _, routes, okCache := p.cache.get(); okCache {
		if key, okKey := routes[id]; okKey && key != "" {
			return key
		}
	}
	return id
}
