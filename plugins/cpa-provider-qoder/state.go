package main

import (
	"sync"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// defaultCatalogTTL keeps a catalog snapshot briefly so the model-list request
// the host makes immediately before a chat does not hit the upstream twice.
const defaultCatalogTTL = 5 * time.Minute

// configState is the plugin's own configuration block.
type configState struct {
	pat    string
	region string
}

// pluginState holds the runtime state shared by every capability.
type pluginState struct {
	cfg    *configState
	tokens *jobTokenCache
	cache  *catalogCache

	mu        sync.Mutex
	lastError string

	// transportOverride redirects the transport at a local upstream. Tests set
	// it; nothing in production does.
	transportOverride *region
}

func newPluginState() *pluginState {
	return &pluginState{
		cfg:    &configState{},
		tokens: newJobTokenCache(),
		cache:  newCatalogCache(defaultCatalogTTL),
	}
}

// activeRegion maps a resolved region onto the transport's target.
func (p *pluginState) activeRegion(resolved *region) *region {
	if p.transportOverride != nil {
		return p.transportOverride
	}
	if resolved == nil {
		return regionFor(defaultRegionID)
	}
	return resolved
}

// recordError keeps the last failure for the status report. The text is
// redacted because it may quote an upstream body.
func (p *pluginState) recordError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.lastError = ""
		return
	}
	p.lastError = cpakit.Redact(err.Error())
}

func (p *pluginState) recordedError() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastError
}
