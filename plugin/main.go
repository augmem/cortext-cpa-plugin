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
		service.Shutdown()
		service = nil
	}
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
	defer serviceMu.Unlock()
	if service == nil {
		service = NewService(cfg)
		log.Printf("cortext-cpa-plugin: registered v%s (scope=%s, data_dir=%s)", pluginVersion, cfg.MemoryScope, cfg.DataDir)
	} else {
		service.Reconfigure(cfg)
		log.Printf("cortext-cpa-plugin: reconfigured (scope=%s, enabled=%v)", cfg.MemoryScope, cfg.Enabled)
	}
	return nil
}

func currentService() *Service {
	serviceMu.Lock()
	defer serviceMu.Unlock()
	if service == nil {
		service = NewService(DefaultConfig())
	}
	return service
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "cortext",
			Version:          pluginVersion,
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
				{Name: "auto_consolidate", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Run consolidate after durable writes."},
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
	return okEnvelope(currentService().HandleRequestBeforeAuth(req))
}

func interceptRequestAfter(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(currentService().HandleRequestAfterAuth(req))
}

func interceptResponse(raw []byte) ([]byte, error) {
	var req pluginapi.ResponseInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(currentService().HandleResponse(req))
}

func interceptStreamChunk(raw []byte) ([]byte, error) {
	var req pluginapi.StreamChunkInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(currentService().HandleStreamChunk(req))
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
