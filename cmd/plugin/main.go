// Command plugin is the CLIProxyAPI dynamic-library plugin that implements
// Modelscope per-model and global daily rate-limit control.
//
// The rate-limit state machine lives in modelscope-ratelimit/internal/ratelimit
// (stdlib-only, unit-tested). This file is the native C-ABI adapter: it maps
// the documented plugin methods (see docs/cn/plugin and sdk/pluginabi) to the
// core helpers and exports cliproxy_plugin_init. It also exposes a
// management resource page ("Modelscope 限流") showing the live disable list.
//
// Build (CGO required):
//
//	cd plugins/modelscope-ratelimit
//	GOPROXY=https://goproxy.cn,direct go build -buildmode=c-shared -o cmd/plugin/modelscope-ratelimit.so ./cmd/plugin
//	# macOS: .dylib, Windows: .dll (cross-compile needs a C cross-compiler)
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

// Host callback function-pointer types (see loader_unix.go cliproxyHostCall).
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

// cliproxy_call_host invokes a host callback method synchronously. It does not
// retain any pointer. Returns the host's return code (0 = success).
static int cliproxy_call_host(const cliproxy_host_api* h, const char* method,
                              const uint8_t* req, size_t req_len, cliproxy_buffer* resp) {
	if (h == NULL || h->call == NULL) {
		return -1;
	}
	return ((cliproxy_host_call_fn)h->call)(h->host_ctx, method, req, req_len, resp);
}

// cliproxy_free_host frees a buffer returned by the host.
static void cliproxy_free_host(const cliproxy_host_api* h, void* ptr, size_t len) {
	if (h != NULL && h->free_buffer != NULL) {
		((cliproxy_host_free_fn)h->free_buffer)(ptr, len);
	}
}

// cliproxy_copy_host copies the host API struct into plugin-owned memory so the
// function pointers remain valid for the plugin's lifetime. Caller frees with
// free().
static cliproxy_host_api* cliproxy_copy_host(const cliproxy_host_api* src) {
	cliproxy_host_api* dst = (cliproxy_host_api*)malloc(sizeof(cliproxy_host_api));
	if (dst != NULL && src != NULL) {
		*dst = *src;
	}
	return dst;
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
	modelscope "modelscope-ratelimit/internal/ratelimit"
)

// store holds the rate-limit state for the lifetime of the plugin. It uses a
// stderr logger; wire host.log in if your SDK exposes a convenient bridge.
var store = modelscope.NewStore(stderrLogger{})

var (
	// pluginCfg is the last applied plugin config, used by the management
	// resource page to decide server-side vs browser-side key resolution.
	pluginCfg *modelscope.Config
	cfgMu     sync.Mutex

	// resolvedCache memoizes server-side key resolution so the auto-refresh
	// does not hammer the management API. Invalidated on reconfigure.
	resolvedMu    sync.Mutex
	resolvedCache resolvedEntry

	// strategyCache memoizes the host's built-in routing strategy so the
	// scheduler.pick fallback path does not query the management API on every
	// request. Invalidated on reconfigure (same TTL as resolvedCache).
	strategyMu    sync.Mutex
	strategyCache strategyEntry
)

const (
	resolvedCacheTTL = 60 * time.Second // refresh resolved view at most once/min
	resolvedErrTTL   = 5 * time.Minute  // back off on failure to avoid auth-ban
	strategyCacheTTL = 60 * time.Second // refresh host strategy at most once/min
)

type strategyEntry struct {
	strategy string
	at       time.Time
}

type resolvedEntry struct {
	masked        map[string]string
	providerOrder map[string][]string // provider name -> auth IDs in config.yaml api-key order
	realTotal     int
	unmatched     []string // configured providers not found upstream
	err           error
	at            time.Time
}

type stderrLogger struct{}

func (stderrLogger) Printf(format string, args ...any) { log.Printf(format, args...) }

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	// The host parameter carries host.* callback function pointers. We do not
	// use host callbacks: the plugin is a c-shared library with its own Go
	// runtime, so a panic inside a host callback would cross the runtime
	// boundary and be unrecoverable, fusing the plugin (502 on the auto-
	// refreshing status page). Key resolution talks directly to the local
	// management API via net/http instead (see httpDo). The host param is
	// accepted only to satisfy the init ABI.
	_ = host
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
	// Top-level safety net: a panic anywhere in plugin Go code must never cross
	// the cgo boundary, or the host fuses the plugin and browsers see a 502
	// "plugin resource handler failed". Catch it and return a valid error
	// envelope. The status page additionally wraps itself (safeHandleManagement)
	// so it degrades to a 200 HTML page rather than an error envelope.
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("modelscope-ratelimit: recovered panic in plugin call: %v", rec)
			writeResponse(response, errorEnvelope("plugin_panic", fmt.Sprintf("%v", rec)))
		}
	}()
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
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	store.Shutdown()
}

// handleMethod dispatches the documented plugin methods. It returns the full
// {ok,result}/{ok:false,error} envelope bytes plus a Go error for transport
// failures (which are also wrapped as an error envelope by the caller).
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())

	case pluginabi.MethodSchedulerPick:
		var r modelscope.SchedulerPickRequest
		if err := json.Unmarshal(request, &r); err != nil {
			return nil, err
		}
		resp, err := store.SchedulerPick(r)
		if err != nil {
			return nil, err
		}
		return okEnvelope(resp)

	case pluginabi.MethodResponseInterceptAfter:
		var r modelscope.ResponseInterceptRequest
		if err := json.Unmarshal(request, &r); err != nil {
			return nil, err
		}
		return okEnvelope(store.OnResponse(r))

	case pluginabi.MethodResponseInterceptStreamChunk:
		var r modelscope.StreamChunkInterceptRequest
		if err := json.Unmarshal(request, &r); err != nil {
			return nil, err
		}
		return okEnvelope(store.OnStreamChunk(r))

	case pluginabi.MethodUsageHandle:
		var r modelscope.UsageRecord
		if err := json.Unmarshal(request, &r); err != nil {
			return nil, err
		}
		store.OnUsage(r)
		return okEnvelope(struct{}{})

	case pluginabi.MethodManagementRegister:
		// Request carries host context (BasePath/ResourceBasePath) which we
		// don't need; we just declare our resource page.
		return okEnvelope(managementRegistration())

	case pluginabi.MethodManagementHandle:
		// The status page is unauthenticated and must ALWAYS render: never
		// return an error envelope (which the host turns into a 502). Parse
		// failures and panics degrade to a valid 200 HTML page.
		return okEnvelope(safeHandleManagement(request))

	default:
		// Unknown method: return an envelope-level error (not a transport error).
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	var cfgMap map[string]any
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfgMap); err != nil {
			return err
		}
	}
	cfg, err := modelscope.ConfigFromMap(cfgMap)
	if err != nil {
		return err
	}
	store.Reconfigure(cfg)
	store.Start()
	// Inject the host-strategy fetcher so non-managed fallback candidates use
	// the host's own routing semantics (fetched via the management API).
	store.SetStrategyFetcher(func() string {
		return fetchHostStrategyCached(cfg.HostBaseURL, cfg.ManagementKey)
	})
	store.SetProviderOrderFetcher(func() map[string][]string {
		return fetchProviderOrderCached(cfg.HostBaseURL, cfg.ManagementKey, cfg.Providers)
	})
	// Inject the proxy toggler: a non-empty proxyURL sets the global upstream
	// proxy via PUT /v0/management/proxy-url; an empty string clears it via
	// DELETE. Uses the plugin's own net/http client (not host.http.do) so the
	// management call is never routed through the proxy it just enabled.
	store.SetProxyToggler(func(proxyURL string) error {
		c := currentConfig()
		if c == nil {
			return fmt.Errorf("no config")
		}
		endpoint := strings.TrimRight(c.HostBaseURL, "/") + "/v0/management/proxy-url"
		if proxyURL != "" {
			payload := strings.NewReader(fmt.Sprintf(`{"value":%q}`, proxyURL))
			req, err := http.NewRequest(http.MethodPut, endpoint, payload)
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+c.ManagementKey)
			resp, err := resolveHTTPClient.Do(req)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				return fmt.Errorf("management API PUT proxy-url returned %d", resp.StatusCode)
			}
		} else {
			req, err := http.NewRequest(http.MethodDelete, endpoint, nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+c.ManagementKey)
			resp, err := resolveHTTPClient.Do(req)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				return fmt.Errorf("management API DELETE proxy-url returned %d", resp.StatusCode)
			}
		}
		return nil
	})
	// Inject the proxy URL getter: reads the current global proxy URL via
	// GET /v0/management/proxy-url so the plugin can save and later restore
	// the host's original proxy when toggling its own.
	store.SetProxyURLGetter(func() (string, error) {
		c := currentConfig()
		if c == nil {
			return "", fmt.Errorf("no config")
		}
		endpoint := strings.TrimRight(c.HostBaseURL, "/") + "/v0/management/proxy-url"
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+c.ManagementKey)
		resp, err := resolveHTTPClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		if resp.StatusCode >= 300 {
			return "", fmt.Errorf("management API GET proxy-url returned %d", resp.StatusCode)
		}
		var result struct {
			ProxyURL string `json:"proxy-url"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return "", err
		}
		return strings.TrimSpace(result.ProxyURL), nil
	})
	cfgMu.Lock()
	pluginCfg = cfg
	cfgMu.Unlock()
	// Config changed: drop the resolved + strategy caches so the next request
	// / page load refetches.
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{}
	resolvedMu.Unlock()
	strategyMu.Lock()
	strategyCache = strategyEntry{}
	strategyMu.Unlock()
	return nil
}

// currentConfig returns the last applied plugin config (nil before configure).
func currentConfig() *modelscope.Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return pluginCfg
}

// ---- Server-side key resolution (direct net/http, no cgo callback) ----

// resolveHTTPClient is the outbound client for server-side key resolution.
// It talks DIRECTLY to the local management API rather than via the host.http.do
// cgo callback: the plugin is a c-shared library with its own Go runtime, so a
// panic inside a host callback would cross the runtime boundary and be
// unrecoverable by any plugin-level recover(), fusing the plugin and surfacing
// as a 502 on the (auto-refreshing) status page. A direct localhost HTTP call
// can only ever return errors, which degrade to a 200 guidance page.
var resolveHTTPClient = &http.Client{Timeout: 10 * time.Second}

// httpDo issues an HTTP request from the plugin's own runtime and returns the
// status code and body. Used only for the local management API, so no proxy or
// host transport policy is required.
func httpDo(method, url, bearerKey string) (int, []byte, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if bearerKey != "" {
		req.Header.Set("Authorization", "Bearer "+bearerKey)
	}
	resp, err := resolveHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}
func maskKey(k string) string {
	if len(k) <= 12 {
		return k
	}
	return k[len(k)-12:]
}

type mgmtOpenAICompatEntry struct {
	Name          string `json:"name"`
	Disabled      bool   `json:"disabled"`
	BaseURL       string `json:"base-url"`
	APIKeyEntries []struct {
		APIKey   string `json:"api-key"`
		ProxyURL string `json:"proxy-url"`
	} `json:"api-key-entries"`
}

// stableIDGen replicates the host's synthesizer.StableIDGenerator exactly,
// including the per-(kind:short) collision counter that appends a "-N" suffix
// (e.g. "1ae0d269c959-1") when two distinct api-key entries hash to the same
// 12-hex short. Without the counter, colliding keys collapse to one masked
// entry and the suffixed counterpart never appears in the key-detail table.
// Entries are processed in config order (the management API preserves it) and
// disabled compat entries are skipped, matching synthesizeOpenAICompat, so the
// derived IDs are byte-for-byte identical to the real auth IDs the plugin sees
// in scheduler/usage requests.
type stableIDGen struct {
	counters map[string]int
}

func newStableIDGen() *stableIDGen {
	return &stableIDGen{counters: make(map[string]int)}
}

// Next mirrors synthesizer.StableIDGenerator.Next.
func (g *stableIDGen) Next(kind string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write([]byte(strings.TrimSpace(part)))
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if len(digest) < 12 {
		digest = fmt.Sprintf("%012s", digest)
	}
	short := digest[:12]
	key := kind + ":" + short
	index := g.counters[key]
	g.counters[key] = index + 1
	if index > 0 {
		short = fmt.Sprintf("%s-%d", short, index)
	}
	return kind + ":" + short
}

// resolveKeys fetches /v0/management/openai-compatibility via direct net/http
// and returns masked keys (authID -> masked), the real configured key count,
// and the configured providers NOT found upstream. The auth ID is derived from
// each api-key with stableIDGen (the host's StableIDGenerator algorithm), so
// no auth-files join is needed (auth-files omits config-based api-keys). The
// management key stays server-side; nothing sensitive reaches the browser.
func resolveKeys(baseURL, mgmtKey string, providers []string) (masked map[string]string, providerOrder map[string][]string, realTotal int, unmatched []string, err error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, nil, 0, nil, errors.New("host_base_url is empty")
	}

	sc, body, err := httpDo("GET", base+"/v0/management/openai-compatibility", mgmtKey)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	if sc != 200 {
		return nil, nil, 0, nil, fmt.Errorf("openai-compatibility: HTTP %d", sc)
	}
	var oc struct {
		List []mgmtOpenAICompatEntry `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(body, &oc); err != nil {
		return nil, nil, 0, nil, fmt.Errorf("decode openai-compatibility: %w", err)
	}

	want := make(map[string]bool, len(providers))
	for _, p := range providers {
		if w := strings.ToLower(strings.TrimSpace(p)); w != "" {
			want[w] = true
		}
	}

	masked = make(map[string]string)
	providerOrder = make(map[string][]string)
	realTotal = 0
	matched := make(map[string]bool, len(providers))
	idGen := newStableIDGen()
	for _, e := range oc.List {
		nm := strings.ToLower(strings.TrimSpace(e.Name))
		if len(want) > 0 && !want[nm] {
			continue
		}
		matched[nm] = true
		// Skip disabled compat entries: the host synthesizer does the same
		// (if compat.Disabled { continue }), so their keys never receive auth
		// IDs and must be excluded to keep the collision-counter ordering
		// identical to the real auth IDs the plugin observes.
		if e.Disabled {
			continue
		}
		realTotal += len(e.APIKeyEntries)
		providerName := nm
		if providerName == "" {
			providerName = "openai-compatibility"
		}
		idKind := "openai-compatibility:" + providerName
		for _, k := range e.APIKeyEntries {
			if k.APIKey == "" {
				continue
			}
			authID := idGen.Next(idKind, k.APIKey, e.BaseURL, k.ProxyURL)
			masked[authID] = maskKey(k.APIKey)
			providerOrder[nm] = append(providerOrder[nm], authID)
		}
	}

	// Configured providers that no upstream openai-compatibility entry matches.
	for _, p := range providers {
		if w := strings.ToLower(strings.TrimSpace(p)); w != "" && !matched[w] {
			unmatched = append(unmatched, p)
		}
	}
	return masked, providerOrder, realTotal, unmatched, nil
}

func resolveKeysCached(baseURL, mgmtKey string, providers []string) (map[string]string, map[string][]string, int, []string, error) {
	resolvedMu.Lock()
	c := resolvedCache
	resolvedMu.Unlock()
	if !c.at.IsZero() {
		ttl := resolvedCacheTTL
		if c.err != nil {
			ttl = resolvedErrTTL
		}
		if c.at.Add(ttl).After(time.Now()) {
			return c.masked, c.providerOrder, c.realTotal, c.unmatched, c.err
		}
	}
	masked, providerOrder, realTotal, unmatched, err := resolveKeys(baseURL, mgmtKey, providers)
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{masked: masked, providerOrder: providerOrder, realTotal: realTotal, unmatched: unmatched, err: err, at: time.Now()}
	resolvedMu.Unlock()
	return masked, providerOrder, realTotal, unmatched, err
}

// fetchProviderOrderCached returns a map from provider name to auth IDs in
// config.yaml api-key order (including keys that may currently be disabled).
// The scheduler uses this to index the round-robin cursor into the FULL
// config-ordered list, so that 429 retries and disabled keys don't corrupt
// the rotation order. Cached alongside resolveKeysCached.
func fetchProviderOrderCached(baseURL, mgmtKey string, providers []string) map[string][]string {
	_, m, _, _, _ := resolveKeysCached(baseURL, mgmtKey, providers)
	return m
}

// fetchHostStrategy queries GET /v0/management/routing/strategy and returns
// the host's built-in credential selection strategy ("round-robin" or
// "fill-first"). The result is cached for strategyCacheTTL so the scheduler.pick
// fallback path does not hit the management API on every request. On any error
// it defaults to "round-robin" (the host's own default) so the plugin never
// blocks on strategy resolution.
func fetchHostStrategy(baseURL, mgmtKey string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return "round-robin"
	}
	sc, body, err := httpDo("GET", base+"/v0/management/routing/strategy", mgmtKey)
	if err != nil || sc != 200 {
		return "round-robin"
	}
	var resp struct {
		Strategy string `json:"strategy"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "round-robin"
	}
	switch strings.ToLower(strings.TrimSpace(resp.Strategy)) {
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	default:
		return "round-robin"
	}
}

func fetchHostStrategyCached(baseURL, mgmtKey string) string {
	strategyMu.Lock()
	c := strategyCache
	strategyMu.Unlock()
	if !c.at.IsZero() && c.at.Add(strategyCacheTTL).After(time.Now()) {
		return c.strategy
	}
	v := fetchHostStrategy(baseURL, mgmtKey)
	strategyMu.Lock()
	strategyCache = strategyEntry{strategy: v, at: time.Now()}
	strategyMu.Unlock()
	return v
}

// pluginRegistration builds the plugin.register result: metadata, config
// fields and declared capabilities.
func pluginRegistration() any {
	return struct {
		SchemaVersion uint32                   `json:"schema_version"`
		Metadata      pluginapi.Metadata       `json:"metadata"`
		Capabilities  registrationCapabilities `json:"capabilities"`
	}{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "modelscope-ratelimit",
			Version:          "1.3.0",
			Author:           "k452b",
			GitHubRepository: "https://github.com/bytehola/modelscope-ratelimit",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "providers", Type: pluginapi.ConfigFieldTypeArray, Description: "监控的提供商名称（必填）。填入顺序决定优先级：先填入的组额度耗尽之后，才能依次使用后面的"},
				{Name: "host_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "CPA 链接地址，默认 http://127.0.0.1:8317。"},
				{Name: "management_key", Type: pluginapi.ConfigFieldTypeString, Description: "CPA 管理密钥。（必填）"},
				{Name: "credential_strategy", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"round-robin", "fill-first"}, Description: "凭据选择策略（仅对监控的 provider 生效）：round-robin=轮询（默认），fill-first=填充优先"},
				{Name: "insufficient_quota_cooldown", Type: pluginapi.ConfigFieldTypeInteger, Description: "冷却基准秒数，默认 10。连续失败指数退避×2递增，封顶 60 秒。"},
				{Name: "proxy_url", Type: pluginapi.ConfigFieldTypeString, Description: "代理 URL（选填，留空=不启用）。配置后 429 时自动探测代理并全局开启，探测失败回退 insufficient_quota_cooldown。格式：socks5://user:pass@host:port"},
				{Name: "timezone", Type: pluginapi.ConfigFieldTypeString, Description: "每日 00:00 重置所用时区，默认 Asia/Shanghai。（留空即可）"},
				{Name: "model_remaining_header", Type: pluginapi.ConfigFieldTypeString, Description: "单模型剩余次数响应头名。（留空即可）"},
				{Name: "total_remaining_header", Type: pluginapi.ConfigFieldTypeString, Description: "总剩余次数响应头名。（留空即可）"},
				{Name: "disable_threshold", Type: pluginapi.ConfigFieldTypeInteger, Description: "剩余次数 ≤ 此值即禁用，默认 0（耗尽）。"},
				{Name: "managed_models", Type: pluginapi.ConfigFieldTypeArray, Description: "可选：仅监控这些模型名，留空=全部。"},
			},
		},
		Capabilities: registrationCapabilities{
			Scheduler:                 true,
			ResponseInterceptor:       true,
			ResponseStreamInterceptor: true,
			UsagePlugin:               true,
			ManagementAPI:             true,
		},
	}
}

type registrationCapabilities struct {
	Scheduler                 bool `json:"scheduler"`
	ResponseInterceptor       bool `json:"response_interceptor"`
	ResponseStreamInterceptor bool `json:"response_stream_interceptor"`
	UsagePlugin               bool `json:"usage_plugin"`
	ManagementAPI             bool `json:"management_api"`
}

// ---- Management API: one resource page showing the live disable list ----

// managementRegistration declares the browser resource page that appears as a
// side-menu entry "Modelscope 限流" in the management center.
func managementRegistration() any {
	return struct {
		Resources []managementResource `json:"resources"`
	}{
		Resources: []managementResource{
			{
				Path:        "/status",
				Menu:        "Modelscope 限流",
				Description: "对魔搭社区限流监控，提升号池请求速度和可用性。",
			},
		},
	}
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

// managementRequest mirrors pluginapi.ManagementRequest (wire-compatible).
type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers,omitempty"`
	Query   map[string][]string `json:"Query,omitempty"`
	Body    []byte              `json:"Body,omitempty"`
}

// managementResponse mirrors pluginapi.ManagementResponse (wire-compatible).
type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

// safeHandleManagement wraps handleManagement so that any panic or request
// parse error degrades to a valid 200 HTML page instead of an error envelope.
// The host renders a 502 "plugin resource handler failed" whenever the
// management.handle RPC returns ok:false, so the status page must never do
// that — even mid-auto-refresh if resolution panics.
func safeHandleManagement(request []byte) (resp managementResponse) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("modelscope-ratelimit: recovered panic in handleManagement: %v", rec)
			resp = mgmtHTML(statusOrGuidance())
		}
	}()
	var r managementRequest
	if err := json.Unmarshal(request, &r); err != nil {
		// Malformed envelope: assume the status route and render the safe view.
		r = managementRequest{Method: "GET", Path: "/status"}
	}
	return handleManagement(r)
}

// statusOrGuidance renders the configuration guide when the plugin is not yet
// usable, otherwise the unresolved status page. It performs NO HTTP call,
// so it is safe to use as a panic-fallback. Used only when server-side
// resolution is skipped or fails.
func statusOrGuidance() string {
	cfg := currentConfig()
	if cfg == nil || len(cfg.Providers) == 0 || cfg.ManagementKey == "" {
		return modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceMissing, "")
	}
	// Panic fallback: cannot safely resolve here, so show a guidance-style
	// error page instead of an unresolved status page (RenderStatusHTML is gone).
	return modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceResolutionFailed, "内部错误，请稍后刷新")
}

// handleManagement serves the resource page. The resource GET is not
// management-authenticated, so only non-sensitive status is rendered.
//
//   - providers or management_key missing -> guidance (fill them via 编辑配置)
//   - resolution fails (wrong key / host)  -> guidance (fix management_key / host_base_url)
//   - configured providers not upstream    -> guidance (providers setting wrong)
//   - otherwise                            -> full status page (masked keys + remaining + per-model summary)
//
// Key resolution runs server-side via direct net/http, so the management key
// never reaches the browser and the page is immune to browser same-origin /
// third-party port-mismatch issues.
func handleManagement(r managementRequest) managementResponse {
	if r.Method != "GET" || !strings.HasSuffix(r.Path, "/status") {
		return managementResponse{
			StatusCode: 404,
			Headers:    map[string][]string{"content-type": {"text/plain; charset=utf-8"}},
			Body:       []byte("not found"),
		}
	}
	cfg := currentConfig()
	// Not configured yet: guide the operator to set providers + management_key.
	if cfg == nil || len(cfg.Providers) == 0 || cfg.ManagementKey == "" {
		return mgmtHTML(modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceMissing, ""))
	}
	masked, _, realTotal, unmatched, err := resolveKeysCached(cfg.HostBaseURL, cfg.ManagementKey, cfg.Providers)
	if err != nil {
		// Wrong management_key or unreachable host_base_url: guide, don't 502.
		log.Printf("modelscope-ratelimit: server-side key resolution failed: %v", err)
		return mgmtHTML(modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceResolutionFailed, err.Error()))
	}
	if len(unmatched) > 0 {
		// Configured providers don't exist upstream.
		log.Printf("modelscope-ratelimit: configured providers not found: %v", unmatched)
		return mgmtHTML(modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceProviderNotFound, strings.Join(unmatched, ", ")))
	}
	return mgmtHTML(store.RenderStatusHTMLFull(store.Now(), cfg, masked, realTotal))
}

func mgmtHTML(html string) managementResponse {
	return managementResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"content-type": {"text/html; charset=utf-8"}},
		Body:       []byte(html),
	}
}

// okEnvelope marshals a result value into the success envelope.
func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

// errorEnvelope builds an error envelope.
func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return raw
}

// writeResponse copies envelope bytes into the host-provided cliproxy_buffer.
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
