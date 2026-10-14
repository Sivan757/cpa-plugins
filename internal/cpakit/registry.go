package cpakit

import "sync"

var (
	registryMu sync.RWMutex
	pluginImpl Plugin
)

// RegisterPlugin installs the process-wide plugin implementation. One dynamic
// library hosts exactly one plugin, so a single slot is enough.
func RegisterPlugin(p Plugin) {
	registryMu.Lock()
	defer registryMu.Unlock()
	pluginImpl = p
}

func registered() Plugin {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return pluginImpl
}
