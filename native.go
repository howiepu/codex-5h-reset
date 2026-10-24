package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
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

static cliproxy_host_api stored_host;
static void store_host_api(const cliproxy_host_api* host) { stored_host = *host; }
static int call_host_api(const char* method, const uint8_t* raw, size_t len, cliproxy_buffer* out) {
    if (stored_host.call == NULL) return 1;
    return stored_host.call(stored_host.host_ctx, method, raw, len, out);
}
static void free_host_buffer(void* ptr, size_t len) {
    if (stored_host.free_buffer != NULL && ptr != NULL) stored_host.free_buffer(ptr, len);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"sync"
	"unsafe"
)

var bridgeMu sync.RWMutex

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || host.abi_version != 1 || host.call == nil || host.free_buffer == nil {
		return 1
	}
	bridgeMu.Lock()
	C.store_host_api(host)
	bridgeMu.Unlock()
	plugin.abi_version = 1
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, length C.size_t, response *C.cliproxy_buffer) (code C.int) {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	defer func() {
		if recover() != nil {
			writeEnvelope(response, nil, errors.New("插件内部错误"))
			code = 1
		}
	}()
	if method == nil || length > 2<<20 || (length > 0 && request == nil) {
		writeEnvelope(response, nil, errors.New("插件请求无效或超过 2 MiB"))
		return 1
	}
	raw := C.GoBytes(unsafe.Pointer(request), C.int(length))
	value, err := dispatch(instance, C.GoString(method), raw)
	writeEnvelope(response, value, err)
	if err != nil {
		return 1
	}
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { instance.Stop() }

func writeEnvelope(out *C.cliproxy_buffer, value any, err error) {
	e := envelope{OK: err == nil, Result: value}
	if err != nil {
		e.Result, e.Error = nil, &hostError{Code: "plugin_error", Message: err.Error()}
	}
	raw, marshalErr := json.Marshal(e)
	if marshalErr != nil {
		raw = []byte(`{"ok":false,"error":{"code":"encode_error","message":"响应编码失败"}}`)
	}
	out.ptr, out.len = C.CBytes(raw), C.size_t(len(raw))
}

func callHost(method string, request, response any) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return errors.New("宿主请求编码失败")
	}
	cMethod, cRequest := C.CString(method), C.CBytes(raw)
	defer C.free(unsafe.Pointer(cMethod))
	defer C.free(cRequest)
	bridgeMu.RLock()
	defer bridgeMu.RUnlock()
	var out C.cliproxy_buffer
	code := C.call_host_api(cMethod, (*C.uint8_t)(cRequest), C.size_t(len(raw)), &out)
	if out.ptr != nil {
		defer C.free_host_buffer(out.ptr, out.len)
	}
	if out.ptr == nil || out.len == 0 || out.len > 64<<20 {
		return errors.New("宿主返回的缓冲区无效")
	}
	var e struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *hostError      `json:"error"`
	}
	if err := json.Unmarshal(C.GoBytes(out.ptr, C.int(out.len)), &e); err != nil {
		return errors.New("宿主响应格式错误")
	}
	if !e.OK {
		if e.Error != nil {
			return e.Error
		}
		return errors.New("宿主执行失败")
	}
	if code != 0 {
		return errors.New("宿主调用失败")
	}
	if response == nil {
		return nil
	}
	if err := json.Unmarshal(e.Result, response); err != nil {
		return errors.New("宿主结果格式错误")
	}
	return nil
}
