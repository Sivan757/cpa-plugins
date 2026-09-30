package pluginkit

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// hostCallFn is installed by the generated cgo glue. It performs one host
// callback RPC using the live cliproxy_host_api function table.
type hostCallFn func(method string, request []byte) ([]byte, error)

var (
	hostMu     sync.RWMutex
	hostCall   hostCallFn
	hostCtxID  string
	pluginLogF func(level, message string)
)

// SetHostCall installs the host callback bridge. Called once by the C ABI glue.
func SetHostCall(fn hostCallFn) {
	hostMu.Lock()
	defer hostMu.Unlock()
	hostCall = fn
}

// SetHostCallbackID records the host_callback_id of the RPC call currently being
// served. Host callbacks that may nest a model execution must echo it back so
// the host can skip this plugin's own interceptors for the nested request.
func SetHostCallbackID(id string) {
	hostMu.Lock()
	defer hostMu.Unlock()
	hostCtxID = id
}

// HostCallbackID returns the callback id of the RPC call currently being served.
func HostCallbackID() string {
	hostMu.RLock()
	defer hostMu.RUnlock()
	return hostCtxID
}

// HostAvailable reports whether the C ABI host bridge has been installed.
func HostAvailable() bool {
	hostMu.RLock()
	defer hostMu.RUnlock()
	return hostCall != nil
}

// callHost invokes a host callback RPC and decodes its result into out.
func callHost(method string, request any, out any) error {
	hostMu.RLock()
	fn := hostCall
	hostMu.RUnlock()
	if fn == nil {
		return fmt.Errorf("host bridge is not installed")
	}
	payload, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return fmt.Errorf("marshal host request %s: %w", method, errMarshal)
	}
	respRaw, errCall := fn(method, payload)
	if errCall != nil {
		return fmt.Errorf("host call %s: %w", method, errCall)
	}
	var env Envelope
	if errUnmarshal := json.Unmarshal(respRaw, &env); errUnmarshal != nil {
		return fmt.Errorf("decode host envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return env.Error
		}
		return fmt.Errorf("host call %s failed", method)
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
		return fmt.Errorf("decode host result %s: %w", method, errUnmarshal)
	}
	return nil
}

// HostLog writes a diagnostic line through the host logger, falling back to the
// process log when the bridge is unavailable.
func HostLog(ctx context.Context, level, message string) {
	_ = ctx
	hostMu.RLock()
	fn := hostCall
	hostMu.RUnlock()
	if fn == nil {
		return
	}
	payload, errMarshal := json.Marshal(map[string]any{"level": level, "message": message})
	if errMarshal != nil {
		return
	}
	_, _ = fn(MethodHostLog, payload)
}
