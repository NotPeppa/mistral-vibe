// C bridge for the CPA native plugin ABI.
//
// The host dlopen()s the shared library and dlsyms "cliproxy_plugin_init".
// cgo cannot name //export-ed symbols from a preamble, so the entry points are
// declared here and the Go side implements them as //export cpa_go_*.

#include "abi.h"

// Go entry points (//export in main.go).
extern int cpa_go_call(char* method, uint8_t* request, size_t request_len, cliproxy_buffer* response);
extern void cpa_go_free(void* ptr, size_t len);
extern void cpa_go_shutdown(void);

// cliproxy_plugin_call is the symbol the host calls for every RPC. It goes
// straight to the Go entry: indirecting through a function pointer taken from
// Go stalls the runtime on first entry, so this stays a direct call.
int cliproxy_plugin_call(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return cpa_go_call((char*)method, (uint8_t*)request, request_len, response);
}

void cliproxy_plugin_free(void* ptr, size_t len) {
	cpa_go_free(ptr, len);
}

void cliproxy_plugin_shutdown(void) {
	cpa_go_shutdown();
}

// Entry points handed back through the init handshake.
cliproxy_plugin_call_fn plugin_call_entry(void)   { return cliproxy_plugin_call; }
cliproxy_plugin_free_fn plugin_free_entry(void)   { return cliproxy_plugin_free; }
cliproxy_plugin_shutdown_fn plugin_shutdown_entry(void) { return cliproxy_plugin_shutdown; }

// cliproxy_plugin_init is the symbol CPA dlsyms at load time.
extern int cpa_go_init(cliproxy_host_api* host, cliproxy_plugin_api* api);

int cliproxy_plugin_init(const cliproxy_host_api* host, cliproxy_plugin_api* api) {
	return cpa_go_init((cliproxy_host_api*)host, api);
}
