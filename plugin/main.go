package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
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
*/
import "C"

import (
	"encoding/json"
	"log"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const pluginVersion = "0.1.0"

var (
	serviceMu sync.Mutex
	service   *Service
	// serviceUp marks that the first configure ran: intercepts before
	// registration must pass through, not silently activate a default-config
	// service (which would write memory to an operator-unconfigured data_dir).
	serviceUp bool
	// serviceDown marks that shutdown ran: late intercept calls must pass
	// through, not resurrect a default-config service that silently ignores
	// the operator's configured data_dir and knobs.
	serviceDown bool
)

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
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	RequestInterceptor     bool `json:"request_interceptor"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
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
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	serviceMu.Lock()
	defer serviceMu.Unlock()
	if service != nil {
		// Held across the close: a concurrent configure must not open scope
		// files on a fresh Store while this Store's closes are in flight.
		// Intercepts already pass through on serviceDown.
		service.Shutdown()
		service = nil
	}
	serviceDown = true
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodRequestInterceptBefore:
		return interceptRequestBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		return interceptRequestAfter(request)
	case pluginabi.MethodResponseInterceptAfter:
		return interceptResponse(request)
	case pluginabi.MethodResponseInterceptStreamChunk:
		return interceptStreamChunk(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg, err := LoadConfigYAML(req.ConfigYAML)
	if err != nil {
		return err
	}
	serviceMu.Lock()
	serviceDown = false
	serviceUp = true
	if service == nil {
		service = NewService(cfg)
		log.Printf("cortext-cpa-plugin: registered v%s engine=%s (scope=%s, data_dir=%s) %s",
			pluginVersion, engineFlavor, cfg.MemoryScope, cfg.DataDir, metricsSummaryLine())
	} else {
		service.Reconfigure(cfg)
		log.Printf("cortext-cpa-plugin: reconfigured v%s engine=%s (scope=%s, enabled=%v) %s",
			pluginVersion, engineFlavor, cfg.MemoryScope, cfg.Enabled, metricsSummaryLine())
	}
	svc := service
	serviceMu.Unlock()
	// Prewarm detached: a native first open can download/assemble release
	// assets for minutes; the register/reconfigure RPC must not block on it.
	// Failures are best-effort and surface again on first real use. ForScope's
	// openMu keeps a concurrent first request from doubling the download.
	go svc.Prewarm()
	return nil
}

// currentService returns nil before registration and after shutdown:
// interceptors then pass traffic through untouched instead of building state
// with default config the operator never chose.
func currentService() *Service {
	serviceMu.Lock()
	defer serviceMu.Unlock()
	if service == nil || !serviceUp || serviceDown {
		return nil
	}
	return service
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			// Version embeds engine flavor so operators can tell stub vs native
			// from the registration surface (basename is always cortext for CPA config).
			Name:             "cortext",
			Version:          pluginVersion + "+" + engineFlavor,
			Author:           "augmem",
			GitHubRepository: "https://github.com/augmem/cortext-cpa-plugin",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable Cortext memory interceptors."},
				{Name: "data_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Directory for per-scope SQLite stores."},
				{Name: "memory_scope", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"session", "agent", "global"}, Description: "Isolation boundary for memory stores."},
				{Name: "focus", Type: pluginapi.ConfigFieldTypeNumber, Description: "Cortext focus parameter."},
				{Name: "sensitivity", Type: pluginapi.ConfigFieldTypeNumber, Description: "Cortext sensitivity parameter."},
				{Name: "stability", Type: pluginapi.ConfigFieldTypeNumber, Description: "Cortext stability parameter."},
				{Name: "recall_limit", Type: pluginapi.ConfigFieldTypeInteger, Description: "Max memory lines injected per request."},
				{Name: "ingest_assistant", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Durable-ingest assistant responses."},
				{Name: "ingest_reasoning", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Durable-ingest reasoning stream segments."},
				{Name: "interrupt_gate", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Stage mid-stream recall for the next request."},
				{Name: "auto_consolidate", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Consolidate on the engine's consolidation_state hint (fallback: durable-write cadence) and at shutdown."},
				{Name: "consolidate_every", Type: pluginapi.ConfigFieldTypeInteger, Description: "Fallback cadence: durable ingests per scope between consolidate runs when no hint is emitted (default 25)."},
				{Name: "window_messages", Type: pluginapi.ConfigFieldTypeInteger, Description: "If >0, keep only the last N non-system messages outbound."},
				{Name: "session_header", Type: pluginapi.ConfigFieldTypeString, Description: "Request header for session isolation key."},
				{Name: "agent_header", Type: pluginapi.ConfigFieldTypeString, Description: "Request header for agent isolation key."},
			},
		},
		Capabilities: registrationCapability{
			RequestInterceptor:     true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
		},
	}
}

func interceptRequestBefore(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if svc := currentService(); svc != nil {
		return okEnvelope(svc.HandleRequestBeforeAuth(req))
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

func interceptRequestAfter(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if svc := currentService(); svc != nil {
		return okEnvelope(svc.HandleRequestAfterAuth(req))
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

func interceptResponse(raw []byte) ([]byte, error) {
	var req pluginapi.ResponseInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if svc := currentService(); svc != nil {
		return okEnvelope(svc.HandleResponse(req))
	}
	return okEnvelope(pluginapi.ResponseInterceptResponse{})
}

func interceptStreamChunk(raw []byte) ([]byte, error) {
	var req pluginapi.StreamChunkInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if svc := currentService(); svc != nil {
		return okEnvelope(svc.HandleStreamChunk(req))
	}
	return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
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
