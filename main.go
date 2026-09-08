// Package main implements the codex-weekly-usd CLIProxyAPI plugin.
//
// The plugin answers one question per quota window: for every Codex credential,
// what is that window's allowance actually worth in US dollars if the same
// traffic had been billed through the public OpenAI API?
//
// It pairs two facts that arrive together on every request:
//
//	numerator   token counters from UsageRecord.Detail, priced at public rates
//	denominator the used-percentage reported for that window
//
// quota_usd = spend_usd / (used_percent / 100)
//
// A credential can be under more than one limit at once — currently a 5-hour
// window and a weekly one — and the same spend counts against every one of
// them, so each window keeps its own ledger and its own estimate. Windows are
// identified by their length, never by the primary/secondary label they arrive
// under: upstream has already swapped those labels once.
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
	"encoding/json"
	"fmt"
	"strings"
	"unsafe"
)

const (
	abiVersion uint32 = 1
	pluginID          = "codex-weekly-usd"
	pluginName        = "Codex Quota USD"
)

// Stamped at build time so a fork does not have to edit source to identify
// itself:
//
//	go build -ldflags "-X main.pluginVersion=2.7.0 -X main.repository=github.com/owner/repo"
var (
	pluginVersion = "2.7.0"
	pluginAuthor  = "tang12306"
	repository    = "github.com/tang12306/cpa-plugin-codex-weekly-usd"
)

func repositoryURL() string {
	if strings.HasPrefix(repository, "http://") || strings.HasPrefix(repository, "https://") {
		return repository
	}
	return "https://" + repository
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	writeResponse(response, dispatch(C.GoString(method), payload))
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
func cliproxyPluginShutdown() {
	if app := current(); app != nil {
		app.Shutdown()
	}
}

// dispatch routes one ABI call. It never panics: a broken plugin must not take
// the proxy down with it, so every method is wrapped in a recover.
func dispatch(method string, payload []byte) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			out = errorEnvelope("plugin_panic", fmt.Sprintf("%s: %v", method, r))
		}
	}()

	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req lifecycleRequest
		_ = json.Unmarshal(payload, &req)
		configure(req.ConfigYAML)
		return okEnvelope(registration())

	case "plugin.shutdown":
		if app := current(); app != nil {
			app.Shutdown()
		}
		return okEnvelope(json.RawMessage("{}"))

	case "usage.handle":
		if app := current(); app != nil {
			app.HandleUsage(payload)
		}
		return okEnvelope(json.RawMessage("{}"))

	case "management.register":
		return okEnvelope(managementRegistration())

	case "management.handle":
		app := current()
		if app == nil {
			return errorEnvelope("not_ready", "plugin is not configured")
		}
		return okEnvelope(app.HandleManagement(payload))

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method)
	}
}

func registration() json.RawMessage {
	reg := map[string]any{
		"schema_version": 1,
		"metadata": map[string]any{
			"Name":             pluginName,
			"Version":          pluginVersion,
			"Author":           pluginAuthor,
			"GitHubRepository": repositoryURL(),
			"ConfigFields":     configFields(),
		},
		"capabilities": map[string]any{
			"usage_plugin":   true,
			"management_api": true,
		},
	}
	raw, _ := json.Marshal(reg)
	return raw
}

func managementRegistration() json.RawMessage {
	reg := map[string]any{
		// Authenticated Management API routes carry every byte of real data.
		"routes": []map[string]any{
			{"Method": "GET", "Path": "/" + pluginID + "/data"},
			{"Method": "GET", "Path": "/" + pluginID + "/prices"},
			// POST only: this one changes state, so it must not be reachable by
			// following a link. It has to be declared here or the host answers
			// 404 before the plugin is ever consulted - which is what happened
			// from 2.3.0 until 2.4.2, with the handler present and unreachable.
			{"Method": "POST", "Path": "/" + pluginID + "/rotate"},
			{"Method": "POST", "Path": "/" + pluginID + "/refresh"},
		},
		// The resource route is NOT management-authenticated, so it serves only
		// an inert HTML shell. The shell asks the operator for the management
		// key and fetches /v0/management/... itself.
		"resources": []map[string]any{
			{
				"Path":        "/panel",
				"Menu":        "Quota USD",
				"Description": "按 OpenAI 官方 API 价目推算每个凭据每个额度窗口值多少美元（面板支持中英文）。Values each Codex credential's quota windows in USD at public OpenAI API pricing.",
			},
		},
	}
	raw, _ := json.Marshal(reg)
	return raw
}

func okEnvelope(result json.RawMessage) []byte {
	raw, _ := json.Marshal(envelope{OK: true, Result: result})
	return raw
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
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

// hostCall invokes a host callback and unwraps the response envelope.
func hostCall(method string, payload []byte) (json.RawMessage, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}

	rc := C.call_host_api(cMethod, req, C.size_t(len(payload)), &response)
	var body []byte
	if response.ptr != nil {
		body = C.GoBytes(response.ptr, C.int(response.len))
		C.free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("host call %s failed: %s", method, string(body))
	}
	if len(body) == 0 {
		return nil, nil
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode host response for %s: %w", method, err)
	}
	if !env.OK {
		msg := "unknown error"
		if env.Error != nil {
			msg = env.Error.Message
		}
		return nil, fmt.Errorf("host call %s rejected: %s", method, msg)
	}
	return env.Result, nil
}

// hostHTTPGet fetches a URL through the host's HTTP stack instead of dialling
// directly. That matters because the proxy already knows how to reach the
// outside world — operators behind a restricted network configure a proxy once
// in CLIProxyAPI, and this callback inherits it. Callers fall back to a direct
// request when the host declines.
func hostHTTPGet(url string) (int, []byte, error) {
	payload, errMarshal := json.Marshal(map[string]any{
		"Method":  "GET",
		"URL":     url,
		"Headers": map[string][]string{"accept": {"application/json"}},
	})
	if errMarshal != nil {
		return 0, nil, errMarshal
	}
	result, errCall := hostCall("host.http.do", payload)
	if errCall != nil {
		return 0, nil, errCall
	}
	var resp struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return 0, nil, fmt.Errorf("decode host http response: %w", errUnmarshal)
	}
	return resp.StatusCode, resp.Body, nil
}

// hostLog forwards a line to the proxy log so plugin problems land in the same
// journal as everything else.
func hostLog(level, message string) {
	payload, _ := json.Marshal(map[string]any{"level": level, "message": "[" + pluginID + "] " + message})
	_, _ = hostCall("host.log", payload)
}
