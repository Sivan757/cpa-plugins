package pluginkit

import "sync"

var (
	registryMu sync.RWMutex
	registered Plugin
)

// RegisterPlugin installs the process-wide plugin implementation. A plugin
// dynamic library hosts exactly one plugin, so a single slot is enough.
func RegisterPlugin(p Plugin) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registered = p
}

// Registered returns the installed plugin implementation.
func Registered() Plugin {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registered
}

// HandleCall is the single entry point the generated C ABI glue uses: it turns
// one method + request pair into one encoded envelope.
func HandleCall(method string, request []byte) []byte {
	plugin := Registered()
	if plugin == nil {
		return Fail("plugin_not_ready", "no plugin implementation registered")
	}
	SetHostCallbackID(callbackIDFromRequest(request))
	return NewRuntime(plugin).Handle(method, request)
}
