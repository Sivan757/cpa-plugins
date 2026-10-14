package cpakit

import (
	"context"
	"sync"
)

// RawCaller performs one host callback round-trip: method name plus JSON
// request bytes in, JSON envelope bytes out. The generated cgo glue installs
// the real implementation; tests install a fake.
type RawCaller func(method string, payload []byte) ([]byte, error)

var (
	hostMu     sync.RWMutex
	hostBridge *HostBridge
	hostCtxID  string
)

// SetHost installs the host callback bridge. Called once by the ABI glue, and
// again by tests that drive a plugin without the host.
func SetHost(call RawCaller) *HostBridge {
	bridge := NewHostBridge(call)
	hostMu.Lock()
	hostBridge = bridge
	hostMu.Unlock()
	return bridge
}

// Bridge returns the installed host bridge, or nil when none is installed. The
// returned value is safe to call: every method degrades to a local fallback or
// a redacted error when the bridge is missing.
func Bridge() *HostBridge {
	hostMu.RLock()
	defer hostMu.RUnlock()
	return hostBridge
}

// SetHostCallbackID records the host_callback_id of the RPC currently being
// served. Nested host calls must echo it so the host can attribute them to this
// plugin and skip its own interceptors for the nested request.
func SetHostCallbackID(id string) {
	hostMu.Lock()
	hostCtxID = id
	hostMu.Unlock()
}

// HostCallbackID returns the callback id of the RPC currently being served.
func HostCallbackID() string {
	hostMu.RLock()
	defer hostMu.RUnlock()
	return hostCtxID
}

// HostLog writes a diagnostic line through the host logger. It is a no-op when
// no bridge is installed, so logging never blocks a request path.
func HostLog(ctx context.Context, level, message string) {
	bridge := Bridge()
	if bridge == nil {
		return
	}
	_ = bridge.Log(level, message, nil)
}
