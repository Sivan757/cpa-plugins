// Code generated from abi/main.go.tmpl by "make sync-abi". DO NOT EDIT.
//
// This file is the C ABI surface of the plugin. The host loads the dynamic
// library, resolves cliproxy_plugin_init, and exchanges JSON envelopes through
// the function tables declared below. Every provider RPC is forwarded into
// internal/cpakit, which owns the envelope, the host bridge, and dispatch.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"errors"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// errHostCall reports a failed host callback round-trip.
var errHostCall = errors.New("host callback failed")

// maxBufferLen bounds buffer lengths before a size_t to C.int conversion.
// Values at or above 2^31 truncate negative, and C.GoBytes would then panic
// outside any recover, taking the host process down.
const maxBufferLen = 1<<31 - 1

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	if host.abi_version != C.uint32_t(pluginabi.ABIVersion) {
		return 1
	}
	C.store_host_api(host)
	cpakit.SetHost(hostCall)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

// hostCall performs one host callback RPC through the stored function table.
func hostCall(method string, request []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPtr *C.uint8_t
	if len(request) > 0 {
		requestPtr = (*C.uint8_t)(C.CBytes(request))
		defer C.free(unsafe.Pointer(requestPtr))
	}
	var response C.cliproxy_buffer
	if rc := C.call_host_api(cMethod, requestPtr, C.size_t(len(request)), &response); rc != 0 {
		return nil, errHostCall
	}
	if response.ptr == nil {
		return cpakit.Empty(), nil
	}
	if int64(response.len) > maxBufferLen {
		C.free_host_buffer(response.ptr, response.len)
		return nil, errors.New("host callback returned an oversized buffer")
	}
	out := C.GoBytes(unsafe.Pointer(response.ptr), C.int(response.len))
	C.free_host_buffer(response.ptr, response.len)
	return out, nil
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeBuffer(response, cpakit.Fail("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		if int64(requestLen) > maxBufferLen {
			writeBuffer(response, cpakit.Fail("invalid_request", "request exceeds the maximum size"))
			return 0
		}
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	writeBuffer(response, cpakit.Handle(C.GoString(method), requestBytes))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// writeBuffer hands one encoded envelope back to the host.
func writeBuffer(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
