// Shared C declarations for the CPA native plugin ABI (abi.c <-> main.go).
#ifndef CPA_MISTRAL_VIBE_ABI_H
#define CPA_MISTRAL_VIBE_ABI_H

#include <stddef.h>
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

typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

// Go -> C: the entry points handed back through the init handshake.
cliproxy_plugin_call_fn plugin_call_entry(void);
cliproxy_plugin_free_fn plugin_free_entry(void);
cliproxy_plugin_shutdown_fn plugin_shutdown_entry(void);

// C -> host: the exported symbols CPA dlsyms.
int cliproxy_plugin_call(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response);
void cliproxy_plugin_free(void* ptr, size_t len);
void cliproxy_plugin_shutdown(void);




#endif
