// harness.c drives the plugin ABI outside of CLIProxyAPI so the accounting can
// be validated against known inputs before the plugin goes anywhere near the
// live proxy. It reads "method<TAB>json" lines and prints each reply.
//
//   gcc -o harness harness.c -ldl && ./harness ./codex-weekly-usd.so script.txt
#include <dlfcn.h>
#include <unistd.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;

typedef int  (*host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*host_free_fn)(void*, size_t);

typedef struct {
    uint32_t abi_version;
    void* host_ctx;
    host_call_fn call;
    host_free_fn free_buffer;
} cliproxy_host_api;

typedef int  (*plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*plugin_free_fn)(void*, size_t);
typedef void (*plugin_shutdown_fn)(void);

typedef struct {
    uint32_t abi_version;
    plugin_call_fn call;
    plugin_free_fn free_buffer;
    plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

typedef int (*plugin_init_fn)(cliproxy_host_api*, cliproxy_plugin_api*);

// A one-model price catalog, base64 encoded because ExecutorHTTPResponse.Body
// is a Go []byte on the wire. Serving it from host.http.do proves the plugin
// really does prefer the host's HTTP stack over dialling out itself:
//   {"openai":{"models":{"harness-model":{"cost":{"input":3,"output":9,"cache_read":0.3}}}}}
#define HARNESS_CATALOG_B64 \
    "eyJvcGVuYWkiOnsibW9kZWxzIjp7Imhhcm5lc3MtbW9kZWwiOnsiY29zdCI6eyJpbnB1dCI6Mywi" \
    "b3V0cHV0Ijo5LCJjYWNoZV9yZWFkIjowLjN9fX19fQ=="

// host_call answers the callbacks the plugin makes. host.log is echoed so
// plugin-side warnings are visible; host.auth.list returns an empty list, which
// exercises the "no credential metadata" path; host.http.do serves the canned
// catalog above.
static int host_call(void* ctx, const char* method, const uint8_t* req, size_t req_len, cliproxy_buffer* out) {
    (void)ctx;
    const char* body;
    if (strcmp(method, "host.log") == 0) {
        fprintf(stderr, "  [host.log] %.*s\n", (int)req_len, (const char*)req);
        body = "{\"ok\":true,\"result\":{}}";
    } else if (strcmp(method, "host.auth.list") == 0) {
        body = "{\"ok\":true,\"result\":{\"files\":[]}}";
    } else if (strcmp(method, "host.http.do") == 0) {
        body = "{\"ok\":true,\"result\":{\"StatusCode\":200,\"Headers\":{},\"Body\":\""
               HARNESS_CATALOG_B64 "\"}}";
    } else {
        body = "{\"ok\":false,\"error\":{\"code\":\"unsupported\",\"message\":\"harness\"}}";
    }
    size_t n = strlen(body);
    out->ptr = malloc(n);
    memcpy(out->ptr, body, n);
    out->len = n;
    return 0;
}

static void host_free(void* ptr, size_t len) { (void)len; free(ptr); }

int main(int argc, char** argv) {
    if (argc < 3) { fprintf(stderr, "usage: %s <plugin.so> <script>\n", argv[0]); return 2; }

    void* handle = dlopen(argv[1], RTLD_NOW);
    if (!handle) { fprintf(stderr, "dlopen: %s\n", dlerror()); return 1; }

    plugin_init_fn init = (plugin_init_fn)dlsym(handle, "cliproxy_plugin_init");
    if (!init) { fprintf(stderr, "dlsym cliproxy_plugin_init: %s\n", dlerror()); return 1; }

    cliproxy_host_api host = { 1, NULL, host_call, host_free };
    cliproxy_plugin_api plugin;
    memset(&plugin, 0, sizeof(plugin));
    if (init(&host, &plugin) != 0) { fprintf(stderr, "plugin init failed\n"); return 1; }
    printf("== plugin abi_version=%u call=%p ==\n", plugin.abi_version, (void*)plugin.call);

    FILE* f = fopen(argv[2], "r");
    if (!f) { perror("open script"); return 1; }

    char* line = NULL;
    size_t cap = 0;
    ssize_t len;
    while ((len = getline(&line, &cap, f)) > 0) {
        while (len > 0 && (line[len-1] == '\n' || line[len-1] == '\r')) line[--len] = 0;
        if (len == 0 || line[0] == '#') continue;

        char* tab = strchr(line, '\t');
        if (!tab) { fprintf(stderr, "bad line: %s\n", line); continue; }
        *tab = 0;
        char* method = line;
        char* payload = tab + 1;

        // Harness-only: let a script wait for plugin background work, such as
        // the startup price fetch, instead of racing it.
        if (strcmp(method, "sleep") == 0) {
            usleep((useconds_t)atoi(payload) * 1000);
            continue;
        }

        cliproxy_buffer out; out.ptr = NULL; out.len = 0;
        int rc = plugin.call(method, (uint8_t*)payload, strlen(payload), &out);
        printf("\n--- %s (rc=%d) ---\n", method, rc);
        if (out.ptr) {
            fwrite(out.ptr, 1, out.len, stdout);
            printf("\n");
            plugin.free_buffer(out.ptr, out.len);
        } else {
            printf("(no response)\n");
        }
    }
    free(line);
    fclose(f);

    if (plugin.shutdown) plugin.shutdown();
    return 0;
}
