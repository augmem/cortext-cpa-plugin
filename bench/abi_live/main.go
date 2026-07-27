// abi_live loads the real c-shared plugin dylib via the CPA cliproxy_plugin_init
// ABI (same entrypoint CLIProxyAPI uses) and drives multi-format intercept calls.
//
// Usage:
//
//	make build
//	go run ./bench/abi_live -plugin bin/cortext.dylib -out $SCRATCH/live
package main

/*
#cgo linux LDFLAGS: -ldl
#cgo darwin LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

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

typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

typedef int (*cliproxy_plugin_init_fn)(const cliproxy_host_api*, cliproxy_plugin_api*);

static int host_call_noop(void* ctx, const char* method, const uint8_t* req, size_t len, cliproxy_buffer* out) {
	(void)ctx; (void)method; (void)req; (void)len;
	if (out) { out->ptr = NULL; out->len = 0; }
	return 0;
}
static void host_free_noop(void* ptr, size_t len) { (void)ptr; (void)len; }

static void fill_host_api(cliproxy_host_api* api) {
	api->abi_version = 1;
	api->host_ctx = NULL;
	api->call = host_call_noop;
	api->free_buffer = host_free_noop;
}

static void* open_lib(const char* path) { return dlopen(path, RTLD_NOW | RTLD_LOCAL); }
static void* sym(void* h, const char* n) { return dlsym(h, n); }
static const char* dlerr(void) { return dlerror(); }
static int close_lib(void* h) { return dlclose(h); }
static int call_init(void* fn, const cliproxy_host_api* host, cliproxy_plugin_api* plugin) {
	return ((cliproxy_plugin_init_fn)fn)(host, plugin);
}
static int call_plugin(cliproxy_plugin_call_fn fn, const char* method, const uint8_t* req, size_t n, cliproxy_buffer* out) {
	return fn(method, req, n, out);
}
static void free_plugin(cliproxy_plugin_free_fn fn, void* p, size_t n) { fn(p, n); }
static void shut_plugin(cliproxy_plugin_shutdown_fn fn) { fn(); }
*/
import "C"

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

func main() {
	pluginPath := flag.String("plugin", "bin/cortext.dylib", "path to cortext c-shared plugin")
	outDir := flag.String("out", "bench/out", "directory for live proof artifacts")
	dataDir := flag.String("data", "", "plugin data_dir (default: out/data)")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err)
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(*outDir, "data")
	}
	_ = os.MkdirAll(*dataDir, 0o755)

	absPlugin, err := filepath.Abs(*pluginPath)
	if err != nil {
		fatal(err)
	}
	if _, err := os.Stat(absPlugin); err != nil {
		fatal(fmt.Errorf("plugin not found: %w", err))
	}

	client, err := openPlugin(absPlugin)
	if err != nil {
		fatal(err)
	}
	defer client.close()

	cfgYAML := fmt.Sprintf(`
enabled: true
memory_scope: session
data_dir: %q
focus: 0.45
stability: 0.5
recall_limit: 12
ingest_assistant: true
interrupt_gate: false
auto_consolidate: false
`, *dataDir)
	regRaw, err := client.call("plugin.register", mustJSON(map[string]any{
		"config_yaml": []byte(cfgYAML),
	}))
	if err != nil {
		fatal(fmt.Errorf("register: %w", err))
	}
	writeFile(filepath.Join(*outDir, "register.json"), regRaw)

	type fmtCase struct {
		Name   string
		Format string
		Seed   string
		Probe  string
	}
	cases := []fmtCase{
		{
			Name:   "openai",
			Format: "openai",
			Seed:   `{"messages":[{"role":"user","content":"Live fact: coral-narwhal-55 is secret."}]}`,
			Probe:  `{"messages":[{"role":"user","content":"coral-narwhal"}]}`,
		},
		{
			Name:   "claude",
			Format: "claude",
			Seed:   `{"system":"s","messages":[{"role":"user","content":"Live fact: coral-narwhal-55 is secret."}]}`,
			Probe:  `{"system":"s","messages":[{"role":"user","content":"coral-narwhal"}]}`,
		},
		{
			Name:   "gemini",
			Format: "gemini",
			Seed:   `{"contents":[{"role":"user","parts":[{"text":"Live fact: coral-narwhal-55 is secret."}]}]}`,
			Probe:  `{"contents":[{"role":"user","parts":[{"text":"coral-narwhal"}]}]}`,
		},
	}

	report := map[string]any{"plugin": absPlugin, "formats": map[string]any{}}
	formats := report["formats"].(map[string]any)
	allOK := true

	for _, tc := range cases {
		session := "live-" + tc.Name
		headers := map[string][]string{"X-Cortext-Session": {session}}
		// seed — field names match pluginapi.RequestInterceptRequest (Go default JSON tags).
		_, err := client.call("request.intercept_before", mustJSON(map[string]any{
			"SourceFormat": tc.Format,
			"Body":         []byte(tc.Seed),
			"Headers":      headers,
		}))
		if err != nil {
			formats[tc.Name] = map[string]any{"ok": false, "error": err.Error()}
			allOK = false
			continue
		}
		// probe
		probeRaw, err := client.call("request.intercept_before", mustJSON(map[string]any{
			"SourceFormat": tc.Format,
			"Body":         []byte(tc.Probe),
			"Headers":      headers,
		}))
		if err != nil {
			formats[tc.Name] = map[string]any{"ok": false, "error": err.Error()}
			allOK = false
			continue
		}
		writeFile(filepath.Join(*outDir, tc.Name+"_probe.json"), probeRaw)
		body := extractResultBody(probeRaw)
		writeFile(filepath.Join(*outDir, tc.Name+"_body.json"), body)
		ok := strings.Contains(string(body), "<cortext_memory>") && strings.Contains(string(body), "coral-narwhal-55")
		formats[tc.Name] = map[string]any{"ok": ok, "body_len": len(body)}
		if !ok {
			allOK = false
		}
	}

	// Isolation: session A fact must not appear in session B.
	_, _ = client.call("request.intercept_before", mustJSON(map[string]any{
		"SourceFormat": "openai",
		"Body":         []byte(`{"messages":[{"role":"user","content":"isolated-token-mango-88 only for A"}]}`),
		"Headers":      map[string][]string{"X-Cortext-Session": {"iso-A"}},
	}))
	isoRaw, err := client.call("request.intercept_before", mustJSON(map[string]any{
		"SourceFormat": "openai",
		"Body":         []byte(`{"messages":[{"role":"user","content":"isolated-token-mango"}]}`),
		"Headers":      map[string][]string{"X-Cortext-Session": {"iso-B"}},
	}))
	if err != nil {
		report["isolation"] = map[string]any{"ok": false, "error": err.Error()}
		allOK = false
	} else {
		isoBody := extractResultBody(isoRaw)
		leaked := strings.Contains(string(isoBody), "mango-88")
		report["isolation"] = map[string]any{"ok": !leaked, "leaked": leaked}
		if leaked {
			allOK = false
		}
		writeFile(filepath.Join(*outDir, "isolation_B.json"), isoBody)
	}

	// Durability: reopen by shutdown+init again against same data dir.
	client.close()
	client2, err := openPlugin(absPlugin)
	if err != nil {
		fatal(err)
	}
	defer client2.close()
	_, err = client2.call("plugin.register", mustJSON(map[string]any{
		"config_yaml": []byte(cfgYAML),
	}))
	if err != nil {
		fatal(err)
	}
	durRaw, err := client2.call("request.intercept_before", mustJSON(map[string]any{
		"SourceFormat": "openai",
		"Body":         []byte(`{"messages":[{"role":"user","content":"coral-narwhal"}]}`),
		"Headers":      map[string][]string{"X-Cortext-Session": {"live-openai"}},
	}))
	if err != nil {
		report["durability"] = map[string]any{"ok": false, "error": err.Error()}
		allOK = false
	} else {
		durBody := extractResultBody(durRaw)
		ok := strings.Contains(string(durBody), "coral-narwhal-55")
		report["durability"] = map[string]any{"ok": ok}
		if !ok {
			allOK = false
		}
		writeFile(filepath.Join(*outDir, "durability.json"), durBody)
	}

	report["ok"] = allOK
	sum, _ := json.MarshalIndent(report, "", "  ")
	writeFile(filepath.Join(*outDir, "summary.json"), sum)
	fmt.Println(string(sum))
	if !allOK {
		os.Exit(1)
	}
	_ = http.StatusOK
}

type pluginClient struct {
	handle unsafe.Pointer
	api    C.cliproxy_plugin_api
	host   C.cliproxy_host_api
}

func openPlugin(path string) (*pluginClient, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	h := C.open_lib(cPath)
	if h == nil {
		return nil, fmt.Errorf("dlopen: %s", C.GoString(C.dlerr()))
	}
	cSym := C.CString("cliproxy_plugin_init")
	defer C.free(unsafe.Pointer(cSym))
	initFn := C.sym(h, cSym)
	if initFn == nil {
		C.close_lib(h)
		return nil, fmt.Errorf("missing cliproxy_plugin_init: %s", C.GoString(C.dlerr()))
	}
	pc := &pluginClient{handle: h}
	C.fill_host_api(&pc.host)
	if rc := C.call_init(initFn, &pc.host, &pc.api); rc != 0 {
		C.close_lib(h)
		return nil, fmt.Errorf("cliproxy_plugin_init rc=%d", int(rc))
	}
	if pc.api.call == nil {
		C.close_lib(h)
		return nil, fmt.Errorf("plugin api.call is nil")
	}
	return pc, nil
}

func (p *pluginClient) call(method string, req []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var out C.cliproxy_buffer
	var reqPtr *C.uint8_t
	if len(req) > 0 {
		reqPtr = (*C.uint8_t)(unsafe.Pointer(&req[0]))
	}
	rc := C.call_plugin(p.api.call, cMethod, reqPtr, C.size_t(len(req)), &out)
	if rc != 0 {
		return nil, fmt.Errorf("call %s rc=%d", method, int(rc))
	}
	if out.ptr == nil || out.len == 0 {
		return nil, nil
	}
	raw := C.GoBytes(out.ptr, C.int(out.len))
	if p.api.free_buffer != nil {
		C.free_plugin(p.api.free_buffer, out.ptr, out.len)
	}
	// Envelope: {"ok":true,"result":...}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return raw, nil
	}
	if !env.OK {
		if env.Error != nil {
			return raw, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return raw, fmt.Errorf("ok=false")
	}
	return raw, nil
}

func (p *pluginClient) close() {
	if p == nil {
		return
	}
	if p.api.shutdown != nil {
		C.shut_plugin(p.api.shutdown)
	}
	if p.handle != nil {
		C.close_lib(p.handle)
		p.handle = nil
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		fatal(err)
	}
	return b
}

func extractResultBody(envelope []byte) []byte {
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		return envelope
	}
	// RequestInterceptResponse has exported fields without json tags → "Body".
	var res struct {
		Body []byte `json:"Body"`
	}
	if err := json.Unmarshal(env.Result, &res); err == nil && len(res.Body) > 0 {
		return res.Body
	}
	var resLower struct {
		Body []byte `json:"body"`
	}
	if err := json.Unmarshal(env.Result, &resLower); err == nil {
		return resLower.Body
	}
	return env.Result
}

func writeFile(path string, b []byte) {
	_ = os.WriteFile(path, b, 0o644)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
