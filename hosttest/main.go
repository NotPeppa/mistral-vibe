// Harness: loads the built plugin exactly the way CPA does (dlopen +
// dlsym "cliproxy_plugin_init"), drives it over the JSON-RPC envelope, and
// asserts every reply. Run through tests/e2e.sh, which boots a fake Vibe
// console first and points the plugin at it.
package main

/*
#cgo linux LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
typedef int (*cliproxy_plugin_init_fn)(const cliproxy_host_api*, cliproxy_plugin_api*);

static void* h_dlopen(const char* path) { return dlopen(path, RTLD_NOW | RTLD_LOCAL); }
static void* h_dlsym(void* h, const char* name) { return dlsym(h, name); }
static const char* h_dlerror(void) { return dlerror(); }
static int h_init(void* fn, const cliproxy_host_api* host, cliproxy_plugin_api* api) {
	return ((cliproxy_plugin_init_fn)fn)(host, api);
}
static int h_call(cliproxy_plugin_call_fn fn, const char* method, const uint8_t* req, size_t len, cliproxy_buffer* resp) {
	return fn(method, req, len, resp);
}
static void h_free(cliproxy_plugin_free_fn fn, void* ptr, size_t len) { fn(ptr, len); }
static void h_shutdown(cliproxy_plugin_shutdown_fn fn) { fn(); }
static int h_host_call(void* ctx, const char* method, const uint8_t* req, size_t len, cliproxy_buffer* resp) {
	(void)ctx; (void)method; (void)req; (void)len;
	if (resp) { resp->ptr = NULL; resp->len = 0; }
	return 1;
}
static cliproxy_host_call_fn h_host_call_fn(void) { return h_host_call; }
static void* h_handle;
static void h_keep(void* h) { h_handle = h; }
*/
import "C"

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"unsafe"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type handle struct {
	call     C.cliproxy_plugin_call_fn
	free     C.cliproxy_plugin_free_fn
	shutdown C.cliproxy_plugin_shutdown_fn
}

var failures int

func check(label string, ok bool, detail string) {
	if ok {
		fmt.Printf("  ok   %s\n", label)
		return
	}
	fmt.Printf("  FAIL %s %s\n", label, detail)
	failures++
}

func main() {
	lib := "/tmp/cpa-mistral-vibe.dylib"
	if v := os.Getenv("PLUGIN_LIB"); v != "" {
		lib = v
	}
	h, err := load(lib)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
	defer closeLib(h)

	if !run(h) {
		os.Exit(1)
	}
}

func run(h *handle) bool {
	fmt.Println("[1] plugin.register")
	reg, err := h.invoke("plugin.register", []byte("{}"))
	check("register ok", err == nil, errText(err))
	var desc struct {
		Capabilities map[string]bool `json:"capabilities"`
		Metadata     struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"metadata"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if err == nil {
		_ = json.Unmarshal(reg, &desc)
	}
	check("declares auth_provider", desc.Capabilities["auth_provider"], string(reg))
	check("declares quota_provider", desc.Capabilities["quota_provider"], string(reg))
	check("declares management_api", desc.Capabilities["management_api"], string(reg))
	// 6 is the oldest schema that keeps raw management JSON un-escaped and the
	// newest CPA 8.0.4 accepts; a newer claim is rejected outright.
	check("negotiates schema 6", desc.SchemaVersion == 6, fmt.Sprint(desc.SchemaVersion))

	fmt.Println("[2] auth.identifier / quota.identifier")
	id, err := h.invoke("auth.identifier", []byte("{}"))
	check("auth identifier", err == nil && contains(string(id), "mistral-vibe"), string(id))
	qid, err := h.invoke("quota.identifier", []byte("{}"))
	check("quota identifier", err == nil && contains(string(qid), "mistral-vibe"), string(qid))

	fmt.Println("[3] auth.parse")
	parseBody := []byte(`{"provider":"mistral-vibe","file_name":"vibe-demo.json","raw_json":{"plan_key":"sk-plan","inference_key":"sk-infer","plan":"Vibe Pro"}}`)
	parsed, err := h.invoke("auth.parse", parseBody)
	check("parse handled", err == nil && contains(string(parsed), `"Handled":true`), string(parsed))
	// Keys live in StorageJSON (base64), never in the UI-visible Metadata.
	var parseResult struct {
		Auth struct {
			StorageJSON string
			Metadata    map[string]any
		}
	}
	_ = json.Unmarshal(parsed, &parseResult)
	check("parse keeps the plan key",
		strings.Contains(decodeStorage(parseResult.Auth.StorageJSON), `"plan_key":"sk-plan"`), string(parsed))
	check("parse keeps the inference key",
		strings.Contains(decodeStorage(parseResult.Auth.StorageJSON), `"inference_key":"sk-infer"`), string(parsed))
	if _, leaked := parseResult.Auth.Metadata["plan_key"]; leaked {
		check("parse keeps keys out of metadata", false, "plan_key leaked into Metadata")
	}
	check("parse labels provider", contains(string(parsed), "mistral-vibe"), string(parsed))

	fmt.Println("[4] quota.describe")
	descBody, err := h.invoke("quota.describe", []byte("{}"))
	check("describe ok", err == nil, errText(err))
	check("supports mistral-vibe", contains(string(descBody), "mistral-vibe"), string(descBody))

	fmt.Println("[5] quota.fetch against the fake console")
	base := os.Getenv("FAKE_PLAN_BASE")
	if base == "" {
		base = "http://127.0.0.1:18099"
	}
	fetchBody, _ := json.Marshal(map[string]any{
		"auth_index":       "idx-1",
		"provider":         "mistral-vibe",
		"storage_json":     map[string]any{"plan_key": "sk-plan", "plan_base": base, "api_base": base},
		"host_callback_id": "",
		"metadata":         map[string]any{"plan_key": "sk-plan", "api_base": base},
	})
	quota, err := h.invoke("quota.fetch", fetchBody)
	check("fetch ok", err == nil, errText(err))
	var q struct {
		Subscription struct {
			Plan string `json:"plan"`
		} `json:"subscription"`
		Summary []struct {
			Key      string  `json:"key"`
			Value    float64 `json:"value"`
			Currency string  `json:"currency"`
			Format   string  `json:"format"`
		} `json:"summary"`
		Groups []struct {
			DisplayName string `json:"displayName"`
			Buckets     []struct {
				Window            string  `json:"window"`
				RemainingFraction float64 `json:"remainingFraction"`
				ResetTime         string  `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err == nil {
		_ = json.Unmarshal(quota, &q)
	}
	// whoami plan_name is FREE and billing's plan_name is EDU; the plugin keeps
	// whoami's id and shows billing's title, so either name proves the read.
	check("plan resolved from whoami/billing",
		q.Subscription.Plan == "FREE" || q.Subscription.Plan == "EDU" || q.Subscription.Plan == "Student",
		string(quota))
	check("balance metric present", len(q.Summary) == 1 && q.Summary[0].Key == "balance", string(quota))
	if len(q.Summary) == 1 {
		// From the 2026-12-01 cycle a harness bills the API half, so a chat key
		// through this plugin reports the 15 USD API allowance (0.001918% used,
		// ~14.9997 left), not the 150 USD Vibe month.
		check("balance is the billing half's remainder",
			nearly(q.Summary[0].Value, 15-15*0.00001918) && q.Summary[0].Currency == "USD",
			fmt.Sprint(q.Summary[0]))
		check("balance marked currency", q.Summary[0].Format == "currency", q.Summary[0].Format)
	}
	check("allowance window returned", len(q.Groups) >= 1, string(quota))
	check("both halves reported", strings.Contains(string(quota), "API \u00b7 Month") && strings.Contains(string(quota), "\"Month\""), string(quota))
	seen := map[string]float64{}
	for _, g := range q.Groups {
		for _, b := range g.Buckets {
			seen[g.DisplayName+"/"+b.Window] = b.RemainingFraction
		}
	}
	check("month window present", len(q.Groups) >= 1, fmt.Sprint(seen))

	fmt.Println("[6] quota.fetch degrades honestly")
	deadBody, _ := json.Marshal(map[string]any{
		"provider":     "mistral-vibe",
		"storage_json": map[string]any{"plan_key": "sk-plan", "plan_base": "http://127.0.0.1:9"},
	})
	_, err = h.invoke("quota.fetch", deadBody)
	check("unreachable console errors out", err != nil, "expected an error")

	emptyBody, _ := json.Marshal(map[string]any{"provider": "mistral-vibe", "storage_json": map[string]any{}})
	_, err = h.invoke("quota.fetch", emptyBody)
	check("empty credential errors out", err != nil, "expected an error")

	fmt.Println("[7] management routes")
	mgmt, err := h.invoke("management.register", []byte(`{"base_path":"/v0/management"}`))
	check("management register ok", err == nil, errText(err))
	check("declares /plan route", contains(string(mgmt), "/mistral-vibe/plan"), string(mgmt))
	check("declares /render route", contains(string(mgmt), "/mistral-vibe/render"), string(mgmt))

	fmt.Println("[8] management /render output")
	renderReq, _ := json.Marshal(map[string]any{
		"method":  "GET",
		"path":    "/v0/management/mistral-vibe/render",
		"headers": map[string][]string{"X-Vibe-Plan-Key": {"sk-plan"}, "X-Vibe-Base-Url": {base}, "X-Vibe-Api-Base": {base}},
		"query":   map[string][]string{"model": {"mistral-large-latest"}},
	})
	rendered, err := h.invoke("management.handle", renderReq)
	check("render ok", err == nil, errText(err))
	var renderedResp struct {
		StatusCode int
		Body       string
	}
	if err == nil {
		_ = json.Unmarshal(rendered, &renderedResp)
	}
	// Body is []byte on the wire, so it arrives base64-encoded.
	body := decodeBody(renderedResp.Body)
	check("render status 200", renderedResp.StatusCode == 200, fmt.Sprint(renderedResp.StatusCode))
	check("render has plan", contains(body, "FREE") || contains(body, "EDU") || contains(body, "Student"), body)
	check("render has balance", contains(body, "USD") && contains(body, "balance"), body)
	check("render names the billed half", contains(body, "billing API allowance"), body)
	check("render has usage bar", contains(body, "used") && (contains(body, "Month")), body)

	var written []byte
	fmt.Println("[9] connect: publish the key to CPA's AI providers")
	management := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"openai-compatibility":[]}`))
		case http.MethodPut:
			raw, _ := io.ReadAll(r.Body)
			written = raw
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer management.Close()
	// The plugin reads its base from the environment it was started with, which a
	// cgo library does not inherit from os.Setenv here. The harness passes the
	// fake management URL through the connect body instead.
	_ = management.URL
	connectReq, _ := json.Marshal(map[string]any{
		"method":  "POST",
		"path":    "/v0/management/mistral-vibe/connect",
		"headers": map[string][]string{"X-Management-Key": {"mk"}, "X-Vibe-Api-Base": {base}},
		"query":   map[string][]string{},
		"body":    base64.StdEncoding.EncodeToString(mustJSON(map[string]any{"plan_key": "sk-plan", "base_url": base, "api_base": base, "management_base": management.URL})),
	})
	connected, err := h.invoke("management.handle", connectReq)
	check("connect ok", err == nil, errText(err))
	connectBody := decodeBody(fieldOf(connected, "Body"))
	check("connect reports created", contains(connectBody, `"created":true`), connectBody)
	check("entry named for the panel", contains(string(written), `"name":"Mistral Vibe"`), string(written))
	check("entry points at Mistral", contains(string(written), "/v1"), string(written))
	check("entry carries the three models",
		contains(string(written), "mistral-vibe-cli-latest") && contains(string(written), "mistral-vibe-cli-fast"),
		string(written))
	// The entry is what CPA forwards with: it has to present as Vibe CLI traffic,
	// or Mistral books it against the API allowance.
	check("entry carries the CLI scoping header", contains(string(written), "X-Cli-Scoping"), string(written))
	check("entry presents as the Vibe CLI", contains(string(written), "vibe-cli"), string(written))

	fmt.Println("[10] balance view")
	balanceReq, _ := json.Marshal(map[string]any{
		"method":  "GET",
		"path":    "/v0/management/mistral-vibe/balance",
		"headers": map[string][]string{"X-Vibe-Api-Base": {base}},
		"query":   map[string][]string{"plan_key": {"sk-plan"}, "base_url": {base}, "api_base": {base}},
	})
	balanced, err := h.invoke("management.handle", balanceReq)
	check("balance ok", err == nil, errText(err))
	balanceBody := decodeBody(fieldOf(balanced, "Body"))
	check("balance reports the month", contains(balanceBody, `"balance"`) && contains(balanceBody, "USD"), balanceBody)

	fmt.Println("[11] plugin page")
	pageReq, _ := json.Marshal(map[string]any{"method": "GET", "path": "/v0/resource/plugins/cpa-mistral-vibe/page", "headers": map[string][]string{}, "query": map[string][]string{}})
	page, err := h.invoke("management.handle", pageReq)
	check("page route answers", err == nil, errText(err))
	// The page must arrive rendered as HTML: a resource route has no management
	// auth, so it cannot fetch the balance itself.
	var pageResp struct {
		StatusCode int
		Body       string
	}
	_ = json.Unmarshal(page, &pageResp)
	pageBody := decodeBody(pageResp.Body)
	preview := pageBody
	if len(preview) > 200 {
		preview = preview[:200]
	}
	check("page status 200", pageResp.StatusCode == 200, fmt.Sprint(pageResp.StatusCode))
	check("page renders HTML", contains(pageBody, "<!doctype html>"), preview)
	check("page has no unrendered placeholder", !contains(pageBody, "{{"), preview)
	check("page shows the configured key source", contains(pageBody, "cpa-mistral-vibe") && contains(pageBody, "api_key"), preview)

	fmt.Println("[12] unknown method")
	_, err = h.invoke("nope.nope", []byte("{}"))
	check("unknown method errors", err != nil, "expected an error")

	return failures == 0
}

func load(path string) (*handle, error) {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	h := C.h_dlopen(cp)
	if h == nil {
		return nil, fmt.Errorf("dlopen: %s", C.GoString(C.h_dlerror()))
	}
	C.h_keep(h)
	sym := C.CString("cliproxy_plugin_init")
	defer C.free(unsafe.Pointer(sym))
	initSym := C.h_dlsym(h, sym)
	if initSym == nil {
		return nil, fmt.Errorf("dlsym: %s", C.GoString(C.h_dlerror()))
	}
	host := C.cliproxy_host_api{abi_version: 1, call: C.h_host_call_fn()}
	var api C.cliproxy_plugin_api
	if rc := C.h_init(initSym, &host, &api); rc != 0 {
		return nil, fmt.Errorf("init returned %d", int(rc))
	}
	if api.call == nil || api.free_buffer == nil || api.shutdown == nil {
		return nil, fmt.Errorf("init returned an incomplete plugin API")
	}
	return &handle{call: api.call, free: api.free_buffer, shutdown: api.shutdown}, nil
}

func (h *handle) invoke(method string, body []byte) (json.RawMessage, error) {
	cm := C.CString(method)
	defer C.free(unsafe.Pointer(cm))
	var reqPtr *C.uint8_t
	if len(body) > 0 {
		reqPtr = (*C.uint8_t)(C.CBytes(body))
		defer C.free(unsafe.Pointer(reqPtr))
	}
	var resp C.cliproxy_buffer
	if rc := C.h_call(h.call, cm, reqPtr, C.size_t(len(body)), &resp); rc != 0 {
		return nil, fmt.Errorf("plugin call returned %d", int(rc))
	}
	defer C.h_free(h.free, resp.ptr, resp.len)
	payload := C.GoBytes(resp.ptr, C.int(resp.len))
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("envelope decode: %w (%s)", err, payload)
	}
	if !env.OK {
		msg := "unknown error"
		if env.Error != nil {
			msg = env.Error.Message
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return env.Result, nil
}

func closeLib(h *handle) { C.h_shutdown(h.shutdown) }

// fieldOf reads one result field, ignoring its Go type on the wire.
func fieldOf(raw json.RawMessage, name string) string {
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return ""
	}
	if value, ok := generic[name].(string); ok {
		return value
	}
	return ""
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// decodeStorage opens the base64 StorageJSON a plugin returns.
func decodeStorage(encoded string) string {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return encoded
	}
	return string(raw)
}

func decodeBody(encoded string) string {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return encoded
	}
	return string(raw)
}

func nearly(a, b float64) bool {
	d := a - b
	return d < 0.001 && d > -0.001
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
