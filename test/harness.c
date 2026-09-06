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

#define AUTH_LIST_ENVELOPE "{\"ok\":true,\"result\":{\"files\":%s}}"
#define RESULT_ENVELOPE    "{\"ok\":true,\"result\":%s}"

// Fixtures are whole result objects held in files, spliced into the envelope
// without being parsed. Keeping JSON handling out of the harness is deliberate:
// a fixture loader with its own parser is a second implementation to get wrong,
// and the point of this file is to be obviously correct.
//
//   HARNESS_AUTH_LIST  the files array returned by host.auth.list
//   HARNESS_AUTH_DIR   host.auth.get reads <dir>/<auth_index>.json
//   HARNESS_PROBE_DIR  host.http.do reads <dir>/<bearer token>.json
//   HARNESS_SAVE_LOG   host.auth.save appends each request here, one per line
static char* slurp(const char* path, size_t* out_len) {
    FILE* f = fopen(path, "rb");
    if (!f) return NULL;
    fseek(f, 0, SEEK_END);
    long n = ftell(f);
    fseek(f, 0, SEEK_SET);
    if (n < 0) { fclose(f); return NULL; }
    char* buf = (char*)malloc((size_t)n + 1);
    if (!buf) { fclose(f); return NULL; }
    size_t got = fread(buf, 1, (size_t)n, f);
    fclose(f);
    buf[got] = 0;
    if (out_len) *out_len = got;
    return buf;
}

// field copies the value that follows `needle` up to the next `stop` character.
// Enough to pull one flat string out of a request without a parser.
static int field(const char* hay, size_t hay_len, const char* needle, char stop,
                 char* out, size_t cap) {
    const char* found = NULL;
    size_t nlen = strlen(needle);
    for (size_t i = 0; i + nlen <= hay_len; i++) {
        if (memcmp(hay + i, needle, nlen) == 0) { found = hay + i + nlen; break; }
    }
    if (!found) return 0;
    size_t i = 0;
    while (found[i] && found[i] != stop && i + 1 < cap) { out[i] = found[i]; i++; }
    out[i] = 0;
    return i > 0;
}

// fixture builds a reply from <dir>/<key>.json, or returns NULL when the caller
// should fall back to its default.
static char* fixture(const char* dir_env, const char* key) {
    const char* dir = getenv(dir_env);
    if (!dir || !*dir || !key || !*key) return NULL;
    char path[1024];
    snprintf(path, sizeof(path), "%s/%s.json", dir, key);
    size_t len = 0;
    char* body = slurp(path, &len);
    if (!body) return NULL;
    size_t need = len + sizeof(RESULT_ENVELOPE) + 8;
    char* out = (char*)malloc(need);
    snprintf(out, need, RESULT_ENVELOPE, body);
    free(body);
    return out;
}

// host_call answers the callbacks the plugin makes. host.log is echoed so
// plugin-side warnings are visible. Everything else is served from the fixture
// environment above, falling back to the canned price catalog and an empty auth
// list so the accounting tests keep working untouched.
static int host_call(void* ctx, const char* method, const uint8_t* req, size_t req_len, cliproxy_buffer* out) {
    (void)ctx;
    const char* body;
    char* owned = NULL;
    char key[2048];

    if (strcmp(method, "host.log") == 0) {
        fprintf(stderr, "  [host.log] %.*s\n", (int)req_len, (const char*)req);
        body = "{\"ok\":true,\"result\":{}}";
    } else if (strcmp(method, "host.auth.list") == 0) {
        const char* files = getenv("HARNESS_AUTH_LIST");
        if (files == NULL || *files == 0) files = "[]";
        size_t need = strlen(files) + 64;
        owned = (char*)malloc(need);
        snprintf(owned, need, AUTH_LIST_ENVELOPE, files);
        body = owned;
    } else if (strcmp(method, "host.auth.get") == 0) {
        if (field((const char*)req, req_len, "\"auth_index\":\"", '"', key, sizeof(key)))
            owned = fixture("HARNESS_AUTH_DIR", key);
        body = owned ? owned : "{\"ok\":false,\"error\":{\"code\":\"not_found\",\"message\":\"no such auth\"}}";
    } else if (strcmp(method, "host.auth.save") == 0) {
        // The write itself is the thing under test, so it is recorded verbatim
        // rather than acted on: no fixture file is ever modified.
        const char* log = getenv("HARNESS_SAVE_LOG");
        if (log && *log) {
            FILE* f = fopen(log, "a");
            if (f) { fwrite(req, 1, req_len, f); fputc('\n', f); fclose(f); }
        }
        body = "{\"ok\":true,\"result\":{\"name\":\"saved\",\"path\":\"/dev/null\"}}";
    } else if (strcmp(method, "host.http.do") == 0) {
        // A probe carries a bearer token and is answered per credential; the
        // price fetch carries none and gets the catalog.
        if (field((const char*)req, req_len, "Bearer ", '"', key, sizeof(key)))
            owned = fixture("HARNESS_PROBE_DIR", key);
        body = owned ? owned
                     : "{\"ok\":true,\"result\":{\"StatusCode\":200,\"Headers\":{},\"Body\":\""
                       HARNESS_CATALOG_B64 "\"}}";
    } else {
        body = "{\"ok\":false,\"error\":{\"code\":\"unsupported\",\"message\":\"harness\"}}";
    }
    size_t n = strlen(body);
    out->ptr = malloc(n);
    memcpy(out->ptr, body, n);
    out->len = n;
    free(owned);
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
        // the startup price fetch or a rotator sweep, instead of racing it.
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
