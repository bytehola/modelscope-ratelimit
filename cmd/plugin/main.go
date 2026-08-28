// Command plugin is the CLIProxyAPI dynamic-library plugin that implements
// Modelscope account-level magic-cube rate-limit control.
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
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
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

const pluginVersion = "2.3.1"

var pluginHost *C.cliproxy_host_api

//go:embed status_page.html
var statusPageHTML []byte

var (
	refreshMu sync.Mutex
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
	masked         map[string]string
	raw            map[string]string            // authID -> raw API key (m_session_id cookie value)
	maskToRaw      map[string]string            // maskedKey -> raw API key (O(1) lookup)
	maskToSite     map[string]string            // maskedKey -> site base URL (O(1) lookup)
	siteBase       map[string]string            // authID -> Modelscope site base URL (cn/ai)
	baseURL        map[string]string            // authID -> upstream OpenAI-compatible base URL
	aliasToName    map[string]map[string]string // provider name -> client alias -> upstream model name
	orderedAuthIDs []string                     // stable iteration order (deterministic idx)
	dedupedAuthIDs []string                     // one auth ID per raw API key (first in config order)
	rawToAuthIDs   map[string][]string          // raw API key -> auth IDs sharing this account
	providerOrder  map[string][]string          // provider name -> auth IDs in config.yaml api-key order
	realTotal      int
	unmatched      []string // configured providers not found upstream
	err            error
	at             time.Time
}

type stderrLogger struct{}

func (stderrLogger) Printf(format string, args ...any) { log.Printf(format, args...) }

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	// Store host.* callback pointers for executor HTTP/streaming. Management
	// API key resolution still uses net/http direct (see httpDo) to avoid
	// cgo panics on the status-page auto-refresh path.
	pluginHost = C.cliproxy_copy_host(host)
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

	case pluginabi.MethodModelRoute:
		var r pluginapi.ModelRouteRequest
		if err := json.Unmarshal(request, &r); err != nil {
			return nil, err
		}
		return okEnvelope(routeResponsesRequest(r))

	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(struct {
			Identifier string `json:"identifier"`
		}{Identifier: "modelscope-ratelimit"})

	case pluginabi.MethodExecutorExecute:
		var r executorRequestWithCallback
		if err := json.Unmarshal(request, &r); err != nil {
			return nil, err
		}
		resp, err := executeResponsesPassthrough(r)
		if err != nil {
			return nil, err
		}
		return okEnvelope(resp)

	case pluginabi.MethodExecutorExecuteStream:
		var r executorRequestWithCallback
		if err := json.Unmarshal(request, &r); err != nil {
			return nil, err
		}
		resp, err := executeResponsesPassthroughStream(r)
		if err != nil {
			return nil, err
		}
		// Async streaming: return headers only; chunks are pushed via
		// host.stream.emit using the stream_id from the request.
		return okEnvelope(struct {
			Headers http.Header `json:"headers,omitempty"`
		}{Headers: resp.Headers})

	case pluginabi.MethodExecutorCountTokens:
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"total_tokens":0}`)})

	case pluginabi.MethodExecutorHTTPRequest:
		return errorEnvelope("not_implemented", "executor.http_request is not supported"), nil

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
		// If the response was 429 + "insufficient balance", the account's
		// magic cube is exhausted. Update the accounts cache immediately so
		// the frontend reflects the real balance without waiting for refresh.
		// Magic cubes are account-scoped on the server, so propagate the core
		// disable to every auth ID that shares the same raw API key across
		// providers. This avoids trying the same exhausted account through a
		// second managed provider.
		if r.Failed && r.Failure != nil && modelscope.IsInsufficientBalance(r.Failure.StatusCode, r.Failure.Body) {
			// resolved.masked maps authID -> maskKey(apiKey), which is the
			// same MaskedKey stored in accountsCache.results. Using
			// maskKey(r.AuthID) directly would give the auth ID's 12-hex
			// suffix, which never matches the API key's last 12 chars.
			resolved := getResolved()
			if mk, ok := resolved.masked[r.AuthID]; ok && mk != "" {
				markAccountExhausted(mk)
			}
			model := strings.TrimSpace(r.Alias)
			if model == "" {
				model = r.Model
			}
			rawKey, ok := resolved.raw[r.AuthID]
			if ok {
				now := store.Now()
				for _, otherAuthID := range resolved.rawToAuthIDs[rawKey] {
					if otherAuthID != r.AuthID {
						store.DisableGlobal(otherAuthID, model, now)
					}
				}
			}
		}
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
	// TODO: remove after debugging. Routes Modelscope API calls through proxy.
	SetMSProxy(cfg.MsProxyURL)
	store.Start()
	// Inject the host-strategy fetcher so non-managed fallback candidates use
	// the host's own routing semantics (fetched via the management API).
	store.SetStrategyFetcher(func() string {
		return fetchHostStrategyCached(cfg.HostBaseURL, cfg.ManagementKey)
	})
	store.SetProviderOrderFetcher(func() map[string][]string {
		return fetchProviderOrderCached(cfg.HostBaseURL, cfg.ManagementKey, cfg.Providers)
	})
	// Inject the midnight auto sign-in function: at the daily boundary
	// (PruneAll dayChanged), iterate all resolved auth IDs, call
	// /api/v1/users/login/info for each (activates daily magic cube),
	// then fetch balance and rate-limit to update core state.
	mc := &msClient{}
	store.SetMidnightSignInFunc(func() {
		cfg := currentConfig()
		if cfg == nil || len(cfg.Providers) == 0 || cfg.ManagementKey == "" {
			return
		}
		// Force a fresh resolve so we have the latest raw keys + site base URLs.
		resolveKeysCached(cfg.HostBaseURL, cfg.ManagementKey, cfg.Providers)
		resolved := getResolved()
		if resolved.raw == nil {
			return
		}
		now := time.Now()

		var swg sync.WaitGroup
		sem := make(chan struct{}, 20)
		for _, authID := range resolved.dedupedAuthIDs {
			sessionID := resolved.raw[authID]
			sb := resolved.siteBase[authID]
			aid := authID
			sid := sessionID
			sbCopy := sb
			swg.Add(1)
			sem <- struct{}{}
			go func() {
				defer swg.Done()
				defer func() { <-sem }()
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("modelscope-ratelimit: recovered panic in sign-in worker: %v", rec)
					}
				}()
				sc, _, err := mc.getWithRetry(sbCopy, "/api/v1/users/login/info", sid)
				if err != nil || sc != 200 {
					return
				}
				store.RecordCheckIn(aid, now)
				var bSc, rlSc int
				var bRaw, rlRaw []byte
				var bErr, rlErr error
				var bWg sync.WaitGroup
				bWg.Add(2)
				go func() {
					defer bWg.Done()
					bSc, bRaw, bErr = mc.getWithRetry(sbCopy, "/openapi/v1/magicubes/balance", sid)
				}()
				go func() {
					defer bWg.Done()
					rlSc, rlRaw, rlErr = mc.getWithRetry(sbCopy, "/api/v1/inference/rate-limit", sid)
				}()
				bWg.Wait()
				if bErr == nil && bSc == 200 {
					var b modelscope.MagicCubeBalance
					if json.Unmarshal(bRaw, &b) == nil {
						store.RecordMagicCube(aid, b.AvailableBalance, b.TotalBalance, b.FrozenAmount, now)
					}
				}
				if rlErr == nil && rlSc == 200 {
					var rl modelscope.InferenceRateLimit
					if json.Unmarshal(rlRaw, &rl) == nil {
						store.RecordUsagePerDay(aid, rl.CurrentUsagePerDay)
					}
				}
			}()
		}
		swg.Wait()
		// Refresh the accounts cache so the first page load is instant.
		// Use forceBackgroundRefresh to avoid racing with an in-progress refresh.
		go func() {
			if !forceBackgroundRefresh() {
				return
			}
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("modelscope-ratelimit: recovered panic in midnight refresh: %v", rec)
				}
				markRefreshDone()
			}()
			refreshMu.Lock()
			defer refreshMu.Unlock()
			refreshAccountsCache(getResolved())
		}()
	})
	cfgMu.Lock()
	pluginCfg = cfg
	cfgMu.Unlock()
	// Config changed: drop the resolved + strategy caches so the next request
	// / page load refetches.
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{}
	resolvedMu.Unlock()
	clearAllCaches()
	strategyMu.Lock()
	strategyCache = strategyEntry{}
	strategyMu.Unlock()
	// 10s after startup: fetch login/info (daily sign-in) + balance for all
	// accounts so the management page has data ready before first access.
	// After this, refreshes only fetch balance + rate-limit (no login/info).
	go func() {
		time.Sleep(10 * time.Second)
		if !forceBackgroundRefresh() {
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("modelscope-ratelimit: recovered panic in startup fetch: %v", rec)
			}
			markRefreshDone()
		}()
		refreshMu.Lock()
		defer refreshMu.Unlock()
		resolveKeysCached(cfg.HostBaseURL, cfg.ManagementKey, cfg.Providers)
		refreshAccountsCache(getResolved())
	}()
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

// maskToken masks a token for safe display: first 6 + "..." + last 4.
// Used for SdkToken and Token fields in API responses to prevent
// raw credential leakage to the unauthenticated status page.
func maskToken(k string) string {
	if len(k) <= 10 {
		return k
	}
	return k[:6] + "..." + k[len(k)-4:]
}

type mgmtOpenAICompatEntry struct {
	Name          string `json:"name"`
	Disabled      bool   `json:"disabled"`
	BaseURL       string `json:"base-url"`
	APIKeyEntries []struct {
		APIKey   string `json:"api-key"`
		ProxyURL string `json:"proxy-url"`
	} `json:"api-key-entries"`
	Models []struct {
		Name  string `json:"name"`
		Alias string `json:"alias"`
	} `json:"models"`
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
func resolveKeys(baseURL, mgmtKey string, providers []string) (masked map[string]string, raw map[string]string, siteBase map[string]string, orderedAuthIDs []string, providerOrder map[string][]string, realTotal int, unmatched []string, baseURLs map[string]string, aliasToName map[string]map[string]string, err error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, nil, nil, nil, nil, 0, nil, nil, nil, errors.New("host_base_url is empty")
	}

	sc, body, err := httpDo("GET", base+"/v0/management/openai-compatibility", mgmtKey)
	if err != nil {
		return nil, nil, nil, nil, nil, 0, nil, nil, nil, err
	}
	if sc != 200 {
		return nil, nil, nil, nil, nil, 0, nil, nil, nil, fmt.Errorf("openai-compatibility: HTTP %d", sc)
	}
	var oc struct {
		List []mgmtOpenAICompatEntry `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(body, &oc); err != nil {
		return nil, nil, nil, nil, nil, 0, nil, nil, nil, fmt.Errorf("decode openai-compatibility: %w", err)
	}

	want := make(map[string]bool, len(providers))
	for _, p := range providers {
		if w := strings.ToLower(strings.TrimSpace(p)); w != "" {
			want[w] = true
		}
	}

	masked = make(map[string]string)
	raw = make(map[string]string)
	siteBase = make(map[string]string)
	baseURLs = make(map[string]string)
	aliasToName = make(map[string]map[string]string)
	orderedAuthIDs = make([]string, 0)
	providerOrder = make(map[string][]string)
	realTotal = 0
	matched := make(map[string]bool, len(providers))
	idGen := newStableIDGen()
	for _, e := range oc.List {
		nm := strings.ToLower(strings.TrimSpace(e.Name))
		if len(want) > 0 && !want[nm] {
			// Non-managed provider: not collected. The Responses executor
			// only tries managed providers; non-managed providers are served
			// by the host's built-in path (host.model.execute) via chat.
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
			raw[authID] = k.APIKey
			siteBase[authID] = siteBaseFromURL(e.BaseURL)
			baseURLs[authID] = e.BaseURL
			orderedAuthIDs = append(orderedAuthIDs, authID)
			providerOrder[nm] = append(providerOrder[nm], authID)
		}
		providerModels := make(map[string]string)
		for _, m := range e.Models {
			if m.Alias != "" && m.Name != "" {
				providerModels[strings.TrimSpace(m.Alias)] = strings.TrimSpace(m.Name)
			}
		}
		aliasToName[nm] = providerModels
	}

	// Configured providers that no upstream openai-compatibility entry matches.
	for _, p := range providers {
		if w := strings.ToLower(strings.TrimSpace(p)); w != "" && !matched[w] {
			unmatched = append(unmatched, p)
		}
	}
	return masked, raw, siteBase, orderedAuthIDs, providerOrder, realTotal, unmatched, baseURLs, aliasToName, nil
}

// resolveRefreshMu guards resolveRefreshing, the singleflight flag that
// deduplicates concurrent resolveKeys calls so a cache miss under load fires
// at most one management-API request. The scheduler hot path is non-blocking
// (fetchProviderOrderCached); only the management page / midnight sign-in use
// the synchronous resolveKeysCached.
var (
	resolveRefreshMu  sync.Mutex
	resolveRefreshing bool
)

// resolvedExpired reports whether the cached entry is past its TTL. Error
// entries use a longer backoff TTL to avoid hammering the management API on
// auth failures.
func resolvedExpired(c resolvedEntry) bool {
	if c.at.IsZero() {
		return true
	}
	ttl := resolvedCacheTTL
	if c.err != nil {
		ttl = resolvedErrTTL
	}
	return c.at.Add(ttl).Before(time.Now())
}

// performResolve does the management-API fetch and stores the result,
// returning the fresh entry. Shared by the sync (resolveKeysCached) and async
// (startResolveRefresh) paths so the store logic lives in one place.
func performResolve(baseURL, mgmtKey string, providers []string) resolvedEntry {
	masked, raw, siteBase, orderedAuthIDs, providerOrder, realTotal, unmatched, baseURLs, aliasToName, err := resolveKeys(baseURL, mgmtKey, providers)
	dedupedAuthIDs, rawToAuthIDs, maskToRaw, maskToSite := buildDedupMaps(orderedAuthIDs, raw, siteBase)
	refreshed := resolvedEntry{masked: masked, raw: raw, maskToRaw: maskToRaw, maskToSite: maskToSite, siteBase: siteBase, baseURL: baseURLs, aliasToName: aliasToName, orderedAuthIDs: orderedAuthIDs, dedupedAuthIDs: dedupedAuthIDs, rawToAuthIDs: rawToAuthIDs, providerOrder: providerOrder, realTotal: realTotal, unmatched: unmatched, err: err, at: time.Now()}
	resolvedMu.Lock()
	resolvedCache = refreshed
	resolvedMu.Unlock()
	return refreshed
}

// startResolveRefresh launches a singleflight background refresh if one is not
// already running. Used by the scheduler hot path: it never blocks the caller,
// which returns stale (or nil) data immediately and lets the async refresh
// warm the cache for subsequent picks.
func startResolveRefresh(baseURL, mgmtKey string, providers []string) {
	resolveRefreshMu.Lock()
	if resolveRefreshing {
		resolveRefreshMu.Unlock()
		return
	}
	resolveRefreshing = true
	resolveRefreshMu.Unlock()
	go func() {
		defer func() {
			resolveRefreshMu.Lock()
			resolveRefreshing = false
			resolveRefreshMu.Unlock()
		}()
		performResolve(baseURL, mgmtKey, providers)
	}()
}

func resolveKeysCached(baseURL, mgmtKey string, providers []string) (map[string]string, map[string][]string, int, []string, error) {
	c := getResolved()
	if !resolvedExpired(c) {
		return c.masked, c.providerOrder, c.realTotal, c.unmatched, c.err
	}
	// Singleflight: only one caller performs the HTTP fetch; concurrent
	// callers get the (possibly stale) cached entry instead of firing
	// duplicate management-API requests.
	resolveRefreshMu.Lock()
	if resolveRefreshing {
		resolveRefreshMu.Unlock()
		return c.masked, c.providerOrder, c.realTotal, c.unmatched, c.err
	}
	resolveRefreshing = true
	resolveRefreshMu.Unlock()
	defer func() {
		resolveRefreshMu.Lock()
		resolveRefreshing = false
		resolveRefreshMu.Unlock()
	}()
	c = performResolve(baseURL, mgmtKey, providers)
	return c.masked, c.providerOrder, c.realTotal, c.unmatched, c.err
}

// buildDedupMaps constructs the deduplication and lookup maps from the
// resolved key data. It is extracted as a pure function so it can be
// unit-tested without HTTP. When the same raw API key appears in multiple
// providers (different auth IDs), only the first occurrence (first provider in
// config order) is kept in dedupedAuthIDs / maskToRaw / maskToSite, while
// rawToAuthIDs groups every auth ID that shares the raw key for disable
// propagation.
func buildDedupMaps(orderedAuthIDs []string, raw map[string]string, siteBase map[string]string) (dedupedAuthIDs []string, rawToAuthIDs map[string][]string, maskToRaw map[string]string, maskToSite map[string]string) {
	maskToRaw = make(map[string]string, len(raw))
	maskToSite = make(map[string]string, len(raw))
	rawToAuthIDs = make(map[string][]string, len(raw))
	seenKeys := make(map[string]bool, len(raw))
	dedupedAuthIDs = make([]string, 0, len(orderedAuthIDs))
	for _, authID := range orderedAuthIDs {
		sid := raw[authID]
		rawToAuthIDs[sid] = append(rawToAuthIDs[sid], authID)
		if seenKeys[sid] {
			continue
		}
		seenKeys[sid] = true
		dedupedAuthIDs = append(dedupedAuthIDs, authID)
		mk := maskKey(sid)
		maskToRaw[mk] = sid
		maskToSite[mk] = siteBase[authID]
	}
	return dedupedAuthIDs, rawToAuthIDs, maskToRaw, maskToSite
}

// getResolved returns a value copy of the cached resolved entry. The maps are
// immutable after construction, so copying the struct is cheap and prevents a
// caller from reading fields of a racing global replacement.
func getResolved() resolvedEntry {
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	return resolvedCache
}

// fetchProviderOrderCached is the scheduler.hot-path reader for the per-
// provider config-ordered auth-ID list (including keys that may currently be
// disabled). The scheduler indexes the round-robin cursor into this FULL list
// so 429 retries and disabled keys don't corrupt the rotation order.
//
// It is NON-BLOCKING: it returns the cached list immediately (stale entries
// are fine for key ordering) and only triggers a singleflight async refresh
// when the cache is expired. On a cold cache it returns nil so pickManaged
// falls back to its observed-order snapshot — a request never stalls on a
// management-API HTTP call.
func fetchProviderOrderCached(baseURL, mgmtKey string, providers []string) map[string][]string {
	c := getResolved()
	if resolvedExpired(c) {
		startResolveRefresh(baseURL, mgmtKey, providers)
	}
	return c.providerOrder // nil when cold/error; stale otherwise
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

// strategyRefreshMu guards strategyRefreshing, the singleflight flag for
// background strategy refreshes so the fallback path never fires concurrent
// management-API requests.
var (
	strategyRefreshMu  sync.Mutex
	strategyRefreshing bool
)

// strategyExpired reports whether the cached strategy is past its TTL.
func strategyExpired(c strategyEntry) bool {
	if c.at.IsZero() {
		return true
	}
	return c.at.Add(strategyCacheTTL).Before(time.Now())
}

// startStrategyRefresh launches a singleflight background strategy refresh if
// one is not already running. Non-blocking: callers return the stale/default
// strategy immediately.
func startStrategyRefresh(baseURL, mgmtKey string) {
	strategyRefreshMu.Lock()
	if strategyRefreshing {
		strategyRefreshMu.Unlock()
		return
	}
	strategyRefreshing = true
	strategyRefreshMu.Unlock()
	go func() {
		defer func() {
			strategyRefreshMu.Lock()
			strategyRefreshing = false
			strategyRefreshMu.Unlock()
		}()
		v := fetchHostStrategy(baseURL, mgmtKey)
		strategyMu.Lock()
		strategyCache = strategyEntry{strategy: v, at: time.Now()}
		strategyMu.Unlock()
	}()
}

func fetchHostStrategyCached(baseURL, mgmtKey string) string {
	strategyMu.Lock()
	c := strategyCache
	strategyMu.Unlock()
	if !strategyExpired(c) {
		return c.strategy
	}
	// Non-blocking: trigger an async refresh and return stale-or-default so
	// the scheduler.pick fallback path never stalls on a management-API call.
	startStrategyRefresh(baseURL, mgmtKey)
	if c.strategy != "" {
		return c.strategy
	}
	return "round-robin"
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
			Version:          pluginVersion,
			Author:           "k452b",
			GitHubRepository: "https://github.com/bytehola/modelscope-ratelimit",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "providers", Type: pluginapi.ConfigFieldTypeArray, Description: "监控的提供商名称（必填）。填入顺序决定轮换顺序：skip_provider_on_429 关闭时，单次请求内 429 后切下一个 provider，任意 200 后下一次请求回到第一个 provider；只有 key 在各 provider 内跨请求轮换。"},
				{Name: "skip_provider_on_429", Type: pluginapi.ConfigFieldTypeBoolean, Description: "关闭时（默认）：429+insufficient_quota 单次请求内切下一个 provider + 全局退避冷却，任意 200 回到第一个 provider；开启时：429 后直接用非托管模型兜底。"},
				{Name: "use_responses", Type: pluginapi.ConfigFieldTypeBoolean, Description: "开启后将 OpenAI Responses API 请求透传到上游 /v1/responses，并复用现有限流、禁用、统计与 key 轮换；默认关闭。"},
				{Name: "host_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "CPA 链接地址，默认 http://127.0.0.1:8317。"},
				{Name: "management_key", Type: pluginapi.ConfigFieldTypeString, Description: "CPA 管理密钥。（必填）"},
				{Name: "credential_strategy", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"round-robin", "fill-first"}, Description: "凭据选择策略（仅对监控的 provider 生效）：round-robin=轮询（默认），fill-first=填充优先"},
				{Name: "insufficient_quota_cooldown", Type: pluginapi.ConfigFieldTypeNumber, Description: "冷却基准秒数（支持小数），默认 0.1。连续失败指数退避×2递增，封顶 60 秒，无抖动。设为 0 完全跳过冷却（429 后立即轮换，不阻塞）。"},
				{Name: "timezone", Type: pluginapi.ConfigFieldTypeString, Description: "每日 00:00 重置所用时区，默认 Asia/Shanghai。（留空即可）"},
				{Name: "ms_proxy_url", Type: pluginapi.ConfigFieldTypeString, Description: "调试用：Modelscope API 代理地址（如 http://127.0.0.1:8888），留空=直连。"},
			},
		},
		Capabilities: registrationCapabilities{
			Scheduler:                 true,
			ResponseInterceptor:       true,
			ResponseStreamInterceptor: true,
			UsagePlugin:               true,
			ManagementAPI:             true,
			ModelRouter:               true,
			Executor:                  true,
			ExecutorModelScope:        "both",
			ExecutorInputFormats:      []string{"responses"},
			ExecutorOutputFormats:     []string{"responses"},
		},
	}
}

type registrationCapabilities struct {
	Scheduler                 bool     `json:"scheduler"`
	ResponseInterceptor       bool     `json:"response_interceptor"`
	ResponseStreamInterceptor bool     `json:"response_stream_interceptor"`
	UsagePlugin               bool     `json:"usage_plugin"`
	ManagementAPI             bool     `json:"management_api"`
	ModelRouter               bool     `json:"model_router"`
	Executor                  bool     `json:"executor"`
	ExecutorModelScope        string   `json:"executor_model_scope"`
	ExecutorInputFormats      []string `json:"executor_input_formats"`
	ExecutorOutputFormats     []string `json:"executor_output_formats"`
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
//   - otherwise                            -> full status page (masked keys, magic balance, usage and account status)
//
// Key resolution runs server-side via direct net/http, so the management key
// never reaches the browser and the page is immune to browser same-origin /
// third-party port-mismatch issues.
func handleManagement(r managementRequest) managementResponse {
	// Route API endpoints via query parameter: ?api=accounts|account|transactions|checkin-all
	api := getQuery(r, "api")
	if api != "" {
		return handleStatusAPI(r, api)
	}
	// Serve the embedded status page HTML (no query → HTML shell)
	path := r.Path
	if r.Method != "GET" || !strings.HasSuffix(path, "/status") {
		return managementResponse{StatusCode: 404, Headers: map[string][]string{"content-type": {"text/plain; charset=utf-8"}}, Body: []byte("not found")}
	}
	cfg := currentConfig()
	if cfg == nil || len(cfg.Providers) == 0 || cfg.ManagementKey == "" {
		return mgmtHTML(modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceMissing, ""))
	}
	resolved := getResolved()
	if resolved.err != nil || resolved.at.IsZero() {
		masked, _, _, unmatched, err := resolveKeysCached(cfg.HostBaseURL, cfg.ManagementKey, cfg.Providers)
		if err != nil {
			return mgmtHTML(modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceResolutionFailed, err.Error()))
		}
		if len(unmatched) > 0 {
			return mgmtHTML(modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceProviderNotFound, strings.Join(unmatched, ", ")))
		}
		_ = masked
	}
	if len(resolved.unmatched) > 0 {
		return mgmtHTML(modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceProviderNotFound, strings.Join(resolved.unmatched, ", ")))
	}
	return mgmtHTML(string(statusPageHTML))
}

// syncFetchAccounts fetches account data synchronously, guarding against
// concurrent refreshes (e.g. the 10s startup goroutine). If another refresh
// is already running, waits for it and returns the cache; otherwise acquires
// refreshMu and does the fetch.
func syncFetchAccounts(resolved resolvedEntry) managementResponse {
	if forceBackgroundRefresh() {
		defer markRefreshDone()
		refreshMu.Lock()
		defer refreshMu.Unlock()
		fresh := refreshAccountsCache(resolved)
		return mgmtJSON(map[string]any{"accounts": fresh, "fetchedAt": time.Now().Format("15:04:05")})
	}
	// Another refresh is running. Wait for it to complete.
	refreshMu.Lock()
	refreshMu.Unlock()
	if cached := getStaleAccountsCache(); cached != nil {
		return mgmtJSON(map[string]any{"accounts": cached, "fetchedAt": getAccountsCacheTime()})
	}
	// Cache still empty after wait (other refresh failed) — direct fetch.
	fresh := refreshAccountsCache(resolved)
	return mgmtJSON(map[string]any{"accounts": fresh, "fetchedAt": time.Now().Format("15:04:05")})
}

func handleStatusAPI(r managementRequest, api string) managementResponse {
	cfg := currentConfig()
	if cfg == nil || len(cfg.Providers) == 0 || cfg.ManagementKey == "" {
		return mgmtJSONError(403, "not configured")
	}
	resolved := getResolved()
	if resolved.err != nil || resolved.at.IsZero() || resolved.raw == nil {
		_, _, _, _, err := resolveKeysCached(cfg.HostBaseURL, cfg.ManagementKey, cfg.Providers)
		if err != nil {
			return mgmtJSONError(502, err.Error())
		}
		resolved = getResolved()
	}
	mc := &msClient{}
	switch api {
	case "config":
		return mgmtJSON(map[string]any{
			"version":       pluginVersion,
			"use_responses": cfg.UseResponses,
		})
	case "accounts":
		refresh := getQuery(r, "refresh")
		if refresh == "1" {
			// Force a background refresh and return stale data immediately.
			// The frontend's 5s polling will pick up fresh data when ready.
			stale := getStaleAccountsCache()
			if stale != nil {
				// Have stale data: return it immediately, refresh in background.
				if forceBackgroundRefresh() {
					go func() {
						defer func() {
							if rec := recover(); rec != nil {
								log.Printf("modelscope-ratelimit: recovered panic in refresh: %v", rec)
							}
							markRefreshDone()
						}()
						refreshMu.Lock()
						defer refreshMu.Unlock()
						refreshAccountsCache(getResolved())
					}()
				}
				return mgmtJSON(map[string]any{"accounts": stale, "fetchedAt": getAccountsCacheTime()})
			}
			// No cache at all (first load): fetch synchronously with guard.
			return syncFetchAccounts(resolved)
		}
		stale := getStaleAccountsCache()
		if stale != nil {
			// Return stale data immediately; trigger background refresh if expired
			if shouldBackgroundRefresh() {
				go func() {
					defer func() {
						if rec := recover(); rec != nil {
							log.Printf("modelscope-ratelimit: recovered panic in refresh: %v", rec)
						}
						markRefreshDone()
					}()
					refreshMu.Lock()
					defer refreshMu.Unlock()
					refreshAccountsCache(getResolved())
				}()
			}
			return mgmtJSON(map[string]any{"accounts": stale, "fetchedAt": getAccountsCacheTime()})
		}
		// No cache at all: fetch synchronously with guard.
		return syncFetchAccounts(resolved)
	case "account":
		mk := getQuery(r, "key")
		if mk == "" {
			return mgmtJSONError(400, "missing key")
		}
		sessionID, sb := getKeysByMask(resolved, mk)
		if sessionID == "" {
			return mgmtJSONError(404, "key not found")
		}
		site := "cn"
		if strings.Contains(sb, "modelscope.ai") {
			site = "ai"
		}
		if d, ok := getCachedDetail(mk); ok {
			return mgmtJSON(d)
		}
		// Look up the cached account summary to populate profile fields
		// without calling users/info + login/info again.
		var src *accountSummary
		accountsCacheMu.Lock()
		for i := range accountsCache.results {
			if accountsCache.results[i].MaskedKey == mk {
				s := accountsCache.results[i]
				src = &s
				break
			}
		}
		accountsCacheMu.Unlock()
		d := mc.fetchAccountDetailConcurrent(0, sessionID, sb, site, src)
		setCachedDetail(mk, d)
		return mgmtJSON(d)
	case "transactions":
		mk := getQuery(r, "key")
		if mk == "" {
			return mgmtJSONError(400, "missing key")
		}
		sessionID, sb := getKeysByMask(resolved, mk)
		if sessionID == "" {
			return mgmtJSONError(404, "key not found")
		}
		page := getQueryInt(r, "page")
		if page <= 0 {
			page = 1
		}
		pageSize := getQueryInt(r, "page_size")
		if pageSize <= 0 {
			pageSize = 50
		}
		data, err := mc.fetchTransactions(sb, sessionID, page, pageSize)
		if err != nil {
			return mgmtJSON(map[string]any{"error": err.Error(), "records": []any{}})
		}
		return mgmtJSONRaw(data)
	case "checkin-all":
		// Sign in all accounts (concurrent), then refresh the accounts cache
		// so the next ?api=accounts call returns fresh data instantly.
		type checkinResult struct {
			idx int
			ok  bool
			sc  int
		}
		var (
			cmu  sync.Mutex
			cwg  sync.WaitGroup
			cout = make([]checkinResult, 0, len(resolved.dedupedAuthIDs))
			csem = make(chan struct{}, 20)
		)
		ci := 0
		for _, authID := range resolved.dedupedAuthIDs {
			sessionID := resolved.raw[authID]
			sb := resolved.siteBase[authID]
			i := ci
			sid := sessionID
			sbCopy := sb
			ci++
			cwg.Add(1)
			csem <- struct{}{}
			go func() {
				defer cwg.Done()
				defer func() { <-csem }()
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("modelscope-ratelimit: recovered panic in checkin worker: %v", rec)
					}
				}()
				sc, _, err := mc.getWithRetry(sbCopy, "/api/v1/users/login/info", sid)
				cmu.Lock()
				cout = append(cout, checkinResult{i, err == nil && sc == 200, sc})
				cmu.Unlock()
			}()
		}
		cwg.Wait()
		sort.Slice(cout, func(a, b int) bool { return cout[a].idx < cout[b].idx })
		results := make([]map[string]any, len(cout))
		ok := 0
		for i, r := range cout {
			results[i] = map[string]any{"success": r.ok, "status": r.sc}
			if r.ok {
				ok++
			}
		}
		// Refresh the accounts cache so the next ?api=accounts is instant.
		clearAllCaches()
		if forceBackgroundRefresh() {
			go func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("modelscope-ratelimit: recovered panic in checkin refresh: %v", rec)
					}
					markRefreshDone()
				}()
				refreshMu.Lock()
				defer refreshMu.Unlock()
				refreshAccountsCache(resolved)
			}()
		}
		return mgmtJSON(results)
	case "refresh-balance":
		mk := getQuery(r, "key")
		if mk == "" {
			return mgmtJSONError(400, "missing key")
		}
		sessionID, sb := getKeysByMask(resolved, mk)
		if sessionID == "" {
			return mgmtJSONError(404, "key not found")
		}
		mc := &msClient{}
		// Fetch balance + transactions concurrently
		var (
			bSc, txSc   int
			bRaw, txRaw []byte
			bErr, txErr error
			bwg         sync.WaitGroup
		)
		bwg.Add(2)
		go func() {
			defer bwg.Done()
			bSc, bRaw, bErr = mc.getWithRetry(sb, "/openapi/v1/magicubes/balance", sessionID)
		}()
		go func() {
			defer bwg.Done()
			txSc, txRaw, txErr = mc.getWithRetry(sb, "/openapi/v1/magicubes/transactions?page=1&page_size=50", sessionID)
		}()
		bwg.Wait()
		result := map[string]any{"balance": nil, "todayConsumed": 0, "records": []any{}}
		if bErr == nil && bSc == 200 {
			var b modelscope.MagicCubeBalance
			if json.Unmarshal(bRaw, &b) == nil {
				result["balance"] = map[string]any{"total": b.TotalBalance, "available": b.AvailableBalance, "frozen": b.FrozenAmount}
				// Update the accounts cache so the next ?api=accounts
				// reflects the real balance (fixes markAccountExhausted
				// having set available=0 / schedule=exhausted).
				accountsCacheMu.Lock()
				for i := range accountsCache.results {
					if accountsCache.results[i].MaskedKey == mk {
						accountsCache.results[i].Balance = &balanceJSON{
							Total: b.TotalBalance, Available: b.AvailableBalance, Frozen: b.FrozenAmount,
						}
						if b.AvailableBalance <= 0 {
							accountsCache.results[i].Schedule = "exhausted"
						} else {
							accountsCache.results[i].Schedule = "normal"
						}
						break
					}
				}
				accountsCacheMu.Unlock()
			}
		}
		if txErr == nil && txSc == 200 {
			var tx modelscope.TransactionsResp
			if json.Unmarshal(txRaw, &tx) == nil {
				// Calculate todayConsumed from SPEND_CONFIRM records
				today := time.Now().Format("2006-01-02")
				consumed := 0
				for _, rec := range tx.Records {
					if rec.GmtCreated == today && rec.Type == "SPEND_CONFIRM" {
						consumed += rec.TotalAmount
					}
				}
				result["todayConsumed"] = consumed
				// Update the accounts cache with today's consumed amount
				// so the next ?api=accounts reflects the fresh value.
				accountsCacheMu.Lock()
				for i := range accountsCache.results {
					if accountsCache.results[i].MaskedKey == mk {
						accountsCache.results[i].TodayConsumed = consumed
						break
					}
				}
				accountsCacheMu.Unlock()
				// Serialize tx.Records as []any for JSON response
				recsJSON, _ := json.Marshal(tx.Records)
				var recs []any
				if json.Unmarshal(recsJSON, &recs) == nil {
					result["records"] = recs
				}
				result["page"] = tx.Page
				result["pageSize"] = tx.PageSize
				result["total"] = tx.Total
			}
		}
		return mgmtJSON(result)
	case "stats":
		cdCount, cdWait := store.CooldownStats()
		return mgmtJSON(map[string]any{
			"successCount":   store.SuccessStats(),
			"rateLimitCount": store.RateLimitCount(),
			"cooldownCount":  cdCount,
			"cooldownWait":   cdWait.Seconds(),
			"totalKeys":      len(resolved.raw),
			"modelUsage":     store.ModelUsageStats(),
		})
	}
	return mgmtJSONError(404, "unknown api: "+api)
}

// getQuery returns the first value of a query parameter.
func getQuery(r managementRequest, key string) string {
	if vals, ok := r.Query[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// getQueryInt returns the first value of a query parameter as an int, or -1 if absent.
func getQueryInt(r managementRequest, key string) int {
	s := getQuery(r, key)
	if s == "" {
		return -1
	}
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

// getKeysByMask finds the raw session ID and site base by masked key (last 12 chars).
// O(1) lookup using pre-built maskToRaw + maskToSite maps.
func getKeysByMask(r resolvedEntry, maskedKey string) (string, string) {
	return r.maskToRaw[maskedKey], r.maskToSite[maskedKey]
}

func mgmtJSON(v any) managementResponse {
	raw, _ := json.Marshal(v)
	return managementResponse{StatusCode: 200, Headers: map[string][]string{"content-type": {"application/json; charset=utf-8"}}, Body: raw}
}

func mgmtJSONRaw(raw json.RawMessage) managementResponse {
	return managementResponse{StatusCode: 200, Headers: map[string][]string{"content-type": {"application/json; charset=utf-8"}}, Body: raw}
}

func mgmtJSONError(code int, msg string) managementResponse {
	raw, _ := json.Marshal(map[string]string{"error": msg})
	return managementResponse{StatusCode: code, Headers: map[string][]string{"content-type": {"application/json; charset=utf-8"}}, Body: raw}
}

func mgmtHTML(html string) managementResponse {
	return managementResponse{
		StatusCode: 200,
		// no-store forces a fresh refetch on every 5s auto-refresh; without it
		// the browser heuristically caches and the status page looks stale.
		Headers: map[string][]string{
			"content-type":  {"text/html; charset=utf-8"},
			"cache-control": {"no-store, no-cache, must-revalidate"},
			"pragma":        {"no-cache"},
			"expires":       {"0"},
		},
		Body: []byte(html),
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

// ---- Responses passthrough executor ----

// executorRequestWithCallback mirrors rpcExecutorRequest so the glue layer can
// preserve host_callback_id (needed for host.http.* context) and stream_id
// (needed for async host.stream.emit).
type executorRequestWithCallback struct {
	pluginapi.ExecutorRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
}

// hostHTTPRequest is the wire shape expected by host.http.do.
type hostHTTPRequest struct {
	Method         string              `json:"method,omitempty"`
	URL            string              `json:"url,omitempty"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

// hostHTTPResponse mirrors pluginapi.HTTPResponse JSON shape.
type hostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

// hostHTTPStreamResponse mirrors rpcHostHTTPStreamResponse.
type hostHTTPStreamResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
}

// hostStreamReadResponse mirrors rpcHostHTTPStreamReadResponse.
type hostStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// callHost invokes a host.* callback and returns the decoded result bytes.
func callHost(method string, payload []byte) ([]byte, error) {
	if pluginHost == nil {
		return nil, errors.New("host callback API is unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}
	var resp C.cliproxy_buffer
	if rc := C.cliproxy_call_host(pluginHost, cMethod, req, C.size_t(len(payload)), &resp); rc != 0 {
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if resp.ptr == nil {
		return nil, fmt.Errorf("host callback %s returned no data", method)
	}
	raw := C.GoBytes(resp.ptr, C.int(resp.len))
	C.cliproxy_free_host(pluginHost, resp.ptr, resp.len)
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode host callback %s envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("host callback %s: %s", method, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s returned ok:false", method)
	}
	return env.Result, nil
}

// hostHTTPDo performs a non-streaming upstream HTTP request through the host
// transport/proxy/request-log policy.
var hostHTTPDo = func(req pluginapi.HTTPRequest, callbackID string) (hostHTTPResponse, error) {
	payload, err := json.Marshal(hostHTTPRequest{
		Method:         req.Method,
		URL:            req.URL,
		Headers:        map[string][]string(req.Headers),
		Body:           req.Body,
		HostCallbackID: callbackID,
	})
	if err != nil {
		return hostHTTPResponse{}, err
	}
	raw, err := callHost(pluginabi.MethodHostHTTPDo, payload)
	if err != nil {
		return hostHTTPResponse{}, err
	}
	var out hostHTTPResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return hostHTTPResponse{}, fmt.Errorf("decode host.http.do response: %w", err)
	}
	return out, nil
}

// hostHTTPDoStream performs a streaming upstream HTTP request through the host
// transport policy and returns the host-managed stream id for later reads.
var hostHTTPDoStream = func(req pluginapi.HTTPRequest, callbackID string) (hostHTTPStreamResponse, error) {
	payload, err := json.Marshal(hostHTTPRequest{
		Method:         req.Method,
		URL:            req.URL,
		Headers:        map[string][]string(req.Headers),
		Body:           req.Body,
		HostCallbackID: callbackID,
	})
	if err != nil {
		return hostHTTPStreamResponse{}, err
	}
	raw, err := callHost(pluginabi.MethodHostHTTPDoStream, payload)
	if err != nil {
		return hostHTTPStreamResponse{}, err
	}
	var out hostHTTPStreamResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return hostHTTPStreamResponse{}, fmt.Errorf("decode host.http.do_stream response: %w", err)
	}
	if out.StreamID == "" {
		return hostHTTPStreamResponse{}, errors.New("host.http.do_stream returned no stream_id")
	}
	return out, nil
}

// hostHTTPStreamRead reads one chunk from a host-managed HTTP stream.
var hostHTTPStreamRead = func(streamID string) (hostStreamReadResponse, error) {
	raw, err := callHost(pluginabi.MethodHostHTTPStreamRead, mustJSONMarshal(rpcHostHTTPStreamReadRequest{StreamID: streamID}))
	if err != nil {
		return hostStreamReadResponse{}, err
	}
	var out hostStreamReadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return hostStreamReadResponse{}, fmt.Errorf("decode host.http.stream_read response: %w", err)
	}
	return out, nil
}

// hostHTTPStreamClose closes a host-managed HTTP stream.
func hostHTTPStreamClose(streamID string) {
	_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, mustJSONMarshal(rpcHostHTTPStreamCloseRequest{StreamID: streamID}))
}

// hostStreamEmit pushes one executor stream chunk to the host stream bridge.
func hostStreamEmit(streamID string, payload []byte, errMsg string) error {
	_, err := callHost(pluginabi.MethodHostStreamEmit, mustJSONMarshal(rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
		Error:    errMsg,
	}))
	return err
}

// hostStreamClose closes the executor stream bridge with an optional error.
func hostStreamClose(streamID, errMsg string) {
	_, _ = callHost(pluginabi.MethodHostStreamClose, mustJSONMarshal(rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    errMsg,
	}))
}

func mustJSONMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

type rpcHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// rpcHostModelExecutionRequest mirrors the host glue layer's
// rpcHostModelExecutionRequest: the embedded HostModelExecutionRequest plus
// host_callback_id, so the host can resolve the caller context and set
// SkipRouterPluginID/SkipInterceptorPluginID for non-recursive fallback.
type rpcHostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// routeResponsesRequest routes OpenAI Responses API requests to the plugin's
// own executor so they can be forwarded to the upstream /v1/responses
// endpoint. Chat Completions requests fall through to the host's normal
// provider path (openai-compatibility /chat/completions).
func routeResponsesRequest(req pluginapi.ModelRouteRequest) pluginapi.ModelRouteResponse {
	cfg := currentConfig()
	if cfg == nil || len(cfg.Providers) == 0 {
		return pluginapi.ModelRouteResponse{Handled: false}
	}
	if !cfg.UseResponses {
		return pluginapi.ModelRouteResponse{Handled: false}
	}
	switch strings.ToLower(strings.TrimSpace(req.SourceFormat)) {
	case "openai-response", "responses", "codex":
		return pluginapi.ModelRouteResponse{
			Handled:    true,
			TargetKind: pluginapi.ModelRouteTargetSelf,
			Reason:     "responses-passthrough",
		}
	default:
		return pluginapi.ModelRouteResponse{Handled: false}
	}
}

func ensureResolved() resolvedEntry {
	cfg := currentConfig()
	if cfg == nil {
		return getResolved()
	}
	c := getResolved()
	if resolvedExpired(c) {
		resolveKeysCached(cfg.HostBaseURL, cfg.ManagementKey, cfg.Providers)
		c = getResolved()
	}
	return c
}

func managedCandidates(resolved resolvedEntry) []modelscope.Candidate {
	cfg := currentConfig()
	if cfg == nil || resolved.providerOrder == nil {
		return nil
	}
	var out []modelscope.Candidate
	for _, name := range cfg.Providers {
		providerKey := strings.ToLower(strings.TrimSpace(name))
		for _, authID := range resolved.providerOrder[providerKey] {
			out = append(out, modelscope.Candidate{
				ID:       authID,
				Provider: "openai-compatible-" + name,
				Priority: 1,
				Status:   "available",
			})
		}
	}
	return out
}

func pickResponsesKey(req pluginapi.ExecutorRequest) (authID, apiKey, baseURL string, err error) {
	resolved := ensureResolved()
	if resolved.err != nil {
		return "", "", "", fmt.Errorf("resolve keys: %w", resolved.err)
	}
	if len(resolved.raw) == 0 {
		return "", "", "", errors.New("no openai-compatibility keys resolved")
	}
	cfg := currentConfig()
	pickReq := modelscope.SchedulerPickRequest{
		Provider:   cfg.Providers[0],
		Providers:  cfg.Providers,
		Model:      req.Model,
		Stream:     req.Stream,
		Candidates: managedCandidates(resolved),
	}
	resp, err := store.SchedulerPick(pickReq)
	if err != nil {
		return "", "", "", err
	}
	if !resp.Handled || resp.AuthID == "" {
		return "", "", "", errors.New("scheduler returned no credential")
	}
	authID = resp.AuthID
	apiKey = resolved.raw[authID]
	baseURL = resolved.baseURL[authID]
	if apiKey == "" || baseURL == "" {
		return "", "", "", fmt.Errorf("credential %s missing key or base-url", authID)
	}
	return authID, apiKey, baseURL, nil
}

func usageRecordForFailure(authID string, req pluginapi.ExecutorRequest, statusCode int, body string, provider string, model string) modelscope.UsageRecord {
	return modelscope.UsageRecord{
		Provider: "openai-compatible-" + provider,
		AuthID:   authID,
		Model:    model,
		Alias:    strings.TrimSpace(req.Model),
		Failed:   true,
		Failure:  &modelscope.UsageFailure{StatusCode: statusCode, Body: body},
	}
}

func providerNameForAuth(authID string) string {
	cfg := currentConfig()
	if cfg == nil || len(cfg.Providers) == 0 {
		return ""
	}
	resolved := getResolved()
	for _, name := range cfg.Providers {
		providerKey := strings.ToLower(strings.TrimSpace(name))
		for _, id := range resolved.providerOrder[providerKey] {
			if id == authID {
				return name
			}
		}
	}
	return cfg.Providers[0]
}

func upstreamModelForAuth(authID string, req pluginapi.ExecutorRequest) string {
	alias := strings.TrimSpace(req.Model)
	provider := strings.ToLower(strings.TrimSpace(providerNameForAuth(authID)))
	if provider == "" {
		return alias
	}
	if resolved := getResolved(); resolved.aliasToName != nil {
		if upstream, ok := resolved.aliasToName[provider][alias]; ok && upstream != "" {
			return upstream
		}
	}
	return alias
}

func withModel(body []byte, model string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body
	}
	obj["model"] = model
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

type retryStep struct {
	provider string
}

type retryCandidate struct {
	authID   string
	provider string
	apiKey   string
	baseURL  string
	model    string
}

// buildRetrySequence returns an ordered list of managed-provider steps.
// Only managed providers are tried via the Responses passthrough executor;
// each step calls Store.NextProviderKey to resolve the actual auth ID from the
// per-provider cursor, so key order persists across requests. When all
// managed providers are exhausted the executor defers to the host's built-in
// path (host.model.execute) so non-managed providers serve the request through
// the normal chat protocol with Responses<->Chat translation.
//
// skip ON: try one key per managed provider.
// skip OFF: exhaust all keys per managed provider.
func buildRetrySequence(resolved resolvedEntry, cfg *modelscope.Config) []retryStep {
	var steps []retryStep
	if cfg.SkipProviderOn429 {
		for _, name := range cfg.Providers {
			steps = append(steps, retryStep{provider: name})
		}
	} else {
		for _, name := range cfg.Providers {
			key := strings.ToLower(strings.TrimSpace(name))
			for i := 0; i < len(resolved.providerOrder[key]); i++ {
				steps = append(steps, retryStep{provider: name})
			}
		}
	}
	return steps
}

// resolveStep resolves the auth ID, API key, base URL, and upstream model
// name for a managed-provider retry step using the per-provider cursor from
// the Store, so key order persists across requests.
func resolveStep(step retryStep, resolved resolvedEntry, alias string) (retryCandidate, bool) {
	providerKey := strings.ToLower(strings.TrimSpace(step.provider))
	ids := resolved.providerOrder[providerKey]
	if len(ids) == 0 {
		return retryCandidate{}, false
	}
	authID := store.NextProviderKey(step.provider, ids)
	if authID == "" {
		return retryCandidate{}, false
	}
	apiKey := resolved.raw[authID]
	baseURL := resolved.baseURL[authID]
	model := alias
	if models := resolved.aliasToName[step.provider]; models != nil {
		if upstream, ok := models[alias]; ok && upstream != "" {
			model = upstream
		}
	}
	return retryCandidate{
		authID:   authID,
		provider: step.provider,
		apiKey:   apiKey,
		baseURL:  baseURL,
		model:    model,
	}, true
}

func executeResponsesPassthrough(req executorRequestWithCallback) (pluginapi.ExecutorResponse, error) {
	cfg := currentConfig()
	resolved := ensureResolved()
	steps := buildRetrySequence(resolved, cfg)
	alias := strings.TrimSpace(req.Model)

	originalBody := req.OriginalRequest
	if len(originalBody) == 0 {
		originalBody = req.Payload
	}

	for _, step := range steps {
		cand, ok := resolveStep(step, resolved, alias)
		if !ok {
			continue
		}
		if store.IsKeyDisabled(cand.authID, cand.model, store.Now()) {
			continue
		}
		upstreamBody := withModel(originalBody, cand.model)
		upstreamReq := responsesUpstreamRequest(req.Headers, cand.apiKey, strings.TrimSuffix(cand.baseURL, "/")+"/responses", upstreamBody)
		resp, errDo := hostHTTPDo(upstreamReq, req.HostCallbackID)
		if errDo != nil {
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, errDo.Error(), cand.provider, cand.model))
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, resp.StatusCode, string(resp.Body), cand.provider, cand.model))
			if resp.StatusCode == 429 || resp.StatusCode >= 500 {
				continue
			}
			return pluginapi.ExecutorResponse{}, fmt.Errorf("upstream responses: HTTP %d: %s", resp.StatusCode, string(resp.Body))
		}
		if modelscope.IsResponsesRateLimitExceeded(string(resp.Body)) {
			store.ApplyResponsesRateLimit(cand.authID, "openai-compatible-"+cand.provider, req.Model, string(resp.Body), store.Now())
			continue
		}
		if modelscope.IsResponsesFailed(string(resp.Body)) {
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, resp.StatusCode, string(resp.Body), cand.provider, cand.model))
			continue
		}
		store.OnUsage(modelscope.UsageRecord{
			Provider: "openai-compatible-" + cand.provider,
			AuthID:   cand.authID,
			Model:    cand.model,
			Alias:    req.Model,
		})
		return pluginapi.ExecutorResponse{Payload: resp.Body, Headers: headerMapToHTTP(resp.Headers)}, nil
	}

	// All managed providers exhausted via Responses passthrough. Defer to the
	// host's built-in path so non-managed providers serve the request through
	// the normal chat protocol (Responses<->Chat translation handled by host).
	// The host skips this plugin's router/interceptors (non-recursive); the
	// plugin's scheduler still runs and skips managed keys disabled above.
	hostResp, err := hostModelExecute(req)
	if err != nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("responses managed keys exhausted, host fallback failed: %w", err)
	}
	if hostResp.StatusCode < 200 || hostResp.StatusCode >= 300 {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("host fallback: HTTP %d: %s", hostResp.StatusCode, string(hostResp.Body))
	}
	return pluginapi.ExecutorResponse{Payload: hostResp.Body, Headers: hostResp.Headers}, nil
}

const responsesStreamPreflightMaxBytes = 8 << 20

type responsesStreamPreflightStatus uint8

const (
	responsesStreamPreflightOK responsesStreamPreflightStatus = iota
	responsesStreamPreflightFailed
	responsesStreamPreflightRateLimited
)

type responsesStreamPreflightResult struct {
	status  responsesStreamPreflightStatus
	payload []byte
	done    bool
	reason  string
}

func executeResponsesPassthroughStream(req executorRequestWithCallback) (pluginapi.ExecutorStreamResponse, error) {
	cfg := currentConfig()
	resolved := ensureResolved()
	steps := buildRetrySequence(resolved, cfg)
	alias := strings.TrimSpace(req.Model)

	originalBody := req.OriginalRequest
	if len(originalBody) == 0 {
		originalBody = req.Payload
	}

	for _, step := range steps {
		cand, ok := resolveStep(step, resolved, alias)
		if !ok {
			continue
		}
		if store.IsKeyDisabled(cand.authID, cand.model, store.Now()) {
			continue
		}
		upstreamBody := withModel(originalBody, cand.model)
		upstreamReq := responsesUpstreamRequest(req.Headers, cand.apiKey, strings.TrimSuffix(cand.baseURL, "/")+"/responses", upstreamBody)
		upstreamReq.Headers.Set("Accept", "text/event-stream")
		stream, errStream := hostHTTPDoStream(upstreamReq, req.HostCallbackID)
		if errStream != nil {
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, errStream.Error(), cand.provider, cand.model))
			continue
		}
		if stream.StatusCode != 200 {
			bodyBytes, _, errRead := readHostStream(stream.StreamID, false)
			hostHTTPStreamClose(stream.StreamID)
			if errRead != nil {
				store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, stream.StatusCode, errRead.Error(), cand.provider, cand.model))
			} else {
				store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, stream.StatusCode, string(bodyBytes), cand.provider, cand.model))
			}
			if stream.StatusCode == 429 || stream.StatusCode >= 500 {
				continue
			}
			return pluginapi.ExecutorStreamResponse{}, fmt.Errorf("upstream responses stream: HTTP %d: %s", stream.StatusCode, string(bodyBytes))
		}

		first, done, errRead := readHostStreamChunk(stream.StreamID)
		if errRead != nil {
			hostHTTPStreamClose(stream.StreamID)
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, errRead.Error(), cand.provider, cand.model))
			continue
		}
		preflight, errPreflight := preflightResponsesStream(stream.StreamID, first, done)
		if errPreflight != nil {
			hostHTTPStreamClose(stream.StreamID)
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, errPreflight.Error(), cand.provider, cand.model))
			continue
		}
		switch preflight.status {
		case responsesStreamPreflightRateLimited:
			hostHTTPStreamClose(stream.StreamID)
			store.ApplyResponsesRateLimit(cand.authID, "openai-compatible-"+cand.provider, req.Model, preflight.reason, store.Now())
			continue
		case responsesStreamPreflightFailed:
			hostHTTPStreamClose(stream.StreamID)
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, stream.StatusCode, preflight.reason, cand.provider, cand.model))
			continue
		}
		go forwardResponsesStream(req, cand, stream.StreamID, pluginapi.ExecutorStreamChunk{Payload: preflight.payload}, preflight.done)
		return pluginapi.ExecutorStreamResponse{Headers: headerMapToHTTP(stream.Headers)}, nil
	}

	// All managed providers exhausted. Defer to the host's built-in streaming
	// path so non-managed providers serve the request via chat protocol.
	hostStream, err := hostModelExecuteStream(req)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, fmt.Errorf("responses stream managed keys exhausted, host fallback failed: %w", err)
	}
	if hostStream.StatusCode != 200 {
		hostModelStreamClose(hostStream.StreamID)
		return pluginapi.ExecutorStreamResponse{}, fmt.Errorf("host fallback stream: HTTP %d", hostStream.StatusCode)
	}
	first, done, errRead := readHostModelStreamChunk(hostStream.StreamID)
	if errRead != nil {
		hostModelStreamClose(hostStream.StreamID)
		return pluginapi.ExecutorStreamResponse{}, fmt.Errorf("host fallback stream read: %w", errRead)
	}
	go forwardHostModelStream(req, hostStream.StreamID, first, done)
	return pluginapi.ExecutorStreamResponse{Headers: hostStream.Headers}, nil
}

func responsesUpstreamRequest(headers http.Header, apiKey, url string, body []byte) pluginapi.HTTPRequest {
	out := pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: make(http.Header),
		Body:    body,
	}
	if headers != nil {
		for k, vs := range headers {
			if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Content-Length") {
				continue
			}
			for _, v := range vs {
				out.Headers.Add(k, v)
			}
		}
	}
	out.Headers.Set("Content-Type", "application/json")
	out.Headers.Set("Authorization", "Bearer "+apiKey)
	return out
}

func headerMapToHTTP(src map[string][]string) http.Header {
	if src == nil {
		return http.Header{}
	}
	return http.Header(src)
}

func readHostStreamChunk(streamID string) (chunk pluginapi.ExecutorStreamChunk, done bool, err error) {
	raw, err := hostHTTPStreamRead(streamID)
	if err != nil {
		return pluginapi.ExecutorStreamChunk{}, false, err
	}
	chunk = pluginapi.ExecutorStreamChunk{Payload: append([]byte(nil), raw.Payload...)}
	if raw.Error != "" {
		chunk.Err = errors.New(raw.Error)
	}
	if chunk.Err != nil {
		return chunk, raw.Done, nil
	}
	return chunk, raw.Done, nil
}

func preflightResponsesStream(streamID string, first pluginapi.ExecutorStreamChunk, done bool) (responsesStreamPreflightResult, error) {
	if first.Err != nil {
		return responsesStreamPreflightResult{
			status: responsesStreamPreflightFailed,
			done:   done,
			reason: first.Err.Error(),
		}, nil
	}

	payload := append([]byte(nil), first.Payload...)
	for {
		detectBody, complete := modelscope.ResponsesFirstDataEvent(string(payload), done)
		if complete {
			result := responsesStreamPreflightResult{
				status:  responsesStreamPreflightOK,
				payload: payload,
				done:    done,
				reason:  detectBody,
			}
			switch {
			case modelscope.IsResponsesRateLimitExceeded(detectBody):
				result.status = responsesStreamPreflightRateLimited
			case modelscope.IsResponsesFailed(detectBody):
				result.status = responsesStreamPreflightFailed
			}
			return result, nil
		}
		if done {
			return responsesStreamPreflightResult{
				status:  responsesStreamPreflightOK,
				payload: payload,
				done:    true,
			}, nil
		}
		if len(payload) >= responsesStreamPreflightMaxBytes {
			return responsesStreamPreflightResult{
				status:  responsesStreamPreflightOK,
				payload: payload,
				done:    done,
			}, nil
		}

		chunk, isDone, errRead := readHostStreamChunk(streamID)
		if errRead != nil {
			return responsesStreamPreflightResult{}, errRead
		}
		if chunk.Err != nil {
			return responsesStreamPreflightResult{
				status: responsesStreamPreflightFailed,
				done:   isDone,
				reason: chunk.Err.Error(),
			}, nil
		}
		if len(chunk.Payload) == 0 && !isDone {
			continue
		}
		payload = append(payload, chunk.Payload...)
		done = isDone
	}
}

func forwardResponsesStream(req executorRequestWithCallback, cand retryCandidate, upstreamStreamID string, first pluginapi.ExecutorStreamChunk, done bool) {
	defer hostHTTPStreamClose(upstreamStreamID)
	emitted := false
	responsesFailed := false
	failedBody := ""
	emit := func(payload []byte, errMsg string) bool {
		if e := hostStreamEmit(req.StreamID, payload, errMsg); e != nil {
			if !emitted && errMsg == "" {
				_ = hostStreamEmit(req.StreamID, nil, e.Error())
			}
			return false
		}
		emitted = true
		return true
	}

	if first.Payload != nil || first.Err != nil {
		if first.Err != nil {
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, first.Err.Error(), cand.provider, cand.model))
			_ = hostStreamEmit(req.StreamID, nil, first.Err.Error())
			hostStreamClose(req.StreamID, first.Err.Error())
			return
		}
		if !emit(first.Payload, "") {
			hostStreamClose(req.StreamID, "host stream emit failed")
			return
		}
	}
	if done {
		// first chunk is pre-filtered by executeResponsesPassthroughStream:
		// response.failed (rate-limit or other) never reaches here, so a
		// single-chunk stream is always a genuine success.
		store.OnUsage(modelscope.UsageRecord{
			Provider: "openai-compatible-" + cand.provider,
			AuthID:   cand.authID,
			Model:    cand.model,
			Alias:    req.Model,
		})
		hostStreamClose(req.StreamID, "")
		return
	}
	for {
		chunk, isDone, errRead := readHostStreamChunk(upstreamStreamID)
		if errRead != nil {
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, errRead.Error(), cand.provider, cand.model))
			_ = hostStreamEmit(req.StreamID, nil, errRead.Error())
			hostStreamClose(req.StreamID, errRead.Error())
			return
		}
		if chunk.Err != nil {
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, chunk.Err.Error(), cand.provider, cand.model))
			_ = hostStreamEmit(req.StreamID, nil, chunk.Err.Error())
			hostStreamClose(req.StreamID, chunk.Err.Error())
			return
		}
		if modelscope.IsResponsesRateLimitExceeded(string(chunk.Payload)) {
			store.ApplyResponsesRateLimit(cand.authID, "openai-compatible-"+cand.provider, req.Model, string(chunk.Payload), store.Now())
			responsesFailed = true
			failedBody = string(chunk.Payload)
		} else if modelscope.IsResponsesFailed(string(chunk.Payload)) {
			responsesFailed = true
			failedBody = string(chunk.Payload)
		}
		if !emit(chunk.Payload, "") {
			hostStreamClose(req.StreamID, "host stream emit failed")
			return
		}
		if isDone {
			if responsesFailed {
				store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 200, failedBody, cand.provider, cand.model))
			} else {
				store.OnUsage(modelscope.UsageRecord{
					Provider: "openai-compatible-" + cand.provider,
					AuthID:   cand.authID,
					Model:    cand.model,
					Alias:    req.Model,
				})
			}
			hostStreamClose(req.StreamID, "")
			return
		}
	}
}

func readHostStream(streamID string, stopAtFirst bool) ([]byte, bool, error) {
	var out []byte
	for {
		chunk, done, err := readHostStreamChunk(streamID)
		if err != nil {
			return out, done, err
		}
		if chunk.Err != nil {
			out = append(out, []byte(chunk.Err.Error())...)
			return out, true, nil
		}
		out = append(out, chunk.Payload...)
		if stopAtFirst || done {
			return out, done, nil
		}
	}
}

// hostModelExecute defers a non-streaming request to the host built-in provider path.
var hostModelExecute = func(req executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
	originalBody := req.OriginalRequest
	if len(originalBody) == 0 {
		originalBody = req.Payload
	}
	payload, err := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: req.SourceFormat,
			Model:         req.Model,
			Stream:        false,
			Body:          originalBody,
			Headers:       req.Headers,
			Query:         req.Query,
			Alt:           req.Alt,
		},
		HostCallbackID: req.HostCallbackID,
	})
	if err != nil {
		return pluginapi.HostModelExecutionResponse{}, err
	}
	raw, err := callHost(pluginabi.MethodHostModelExecute, payload)
	if err != nil {
		return pluginapi.HostModelExecutionResponse{}, err
	}
	var out pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return pluginapi.HostModelExecutionResponse{}, fmt.Errorf("decode host.model.execute response: %w", err)
	}
	return out, nil
}

// hostModelExecuteStream defers a streaming request to the host built-in provider path.
var hostModelExecuteStream = func(req executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
	originalBody := req.OriginalRequest
	if len(originalBody) == 0 {
		originalBody = req.Payload
	}
	payload, err := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: req.SourceFormat,
			Model:         req.Model,
			Stream:        true,
			Body:          originalBody,
			Headers:       req.Headers,
			Query:         req.Query,
			Alt:           req.Alt,
		},
		HostCallbackID: req.HostCallbackID,
	})
	if err != nil {
		return pluginapi.HostModelStreamResponse{}, err
	}
	raw, err := callHost(pluginabi.MethodHostModelExecuteStream, payload)
	if err != nil {
		return pluginapi.HostModelStreamResponse{}, err
	}
	var out pluginapi.HostModelStreamResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return pluginapi.HostModelStreamResponse{}, fmt.Errorf("decode host.model.execute_stream response: %w", err)
	}
	if out.StreamID == "" {
		return pluginapi.HostModelStreamResponse{}, errors.New("host.model.execute_stream returned no stream_id")
	}
	return out, nil
}

var hostModelStreamRead = func(streamID string) (pluginapi.HostModelStreamReadResponse, error) {
	raw, err := callHost(pluginabi.MethodHostModelStreamRead, mustJSONMarshal(pluginapi.HostModelStreamReadRequest{StreamID: streamID}))
	if err != nil {
		return pluginapi.HostModelStreamReadResponse{}, err
	}
	var out pluginapi.HostModelStreamReadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return pluginapi.HostModelStreamReadResponse{}, fmt.Errorf("decode host.model.stream_read response: %w", err)
	}
	return out, nil
}

func hostModelStreamClose(streamID string) {
	_, _ = callHost(pluginabi.MethodHostModelStreamClose, mustJSONMarshal(pluginapi.HostModelStreamCloseRequest{StreamID: streamID}))
}

func readHostModelStreamChunk(streamID string) (chunk pluginapi.ExecutorStreamChunk, done bool, err error) {
	raw, err := hostModelStreamRead(streamID)
	if err != nil {
		return pluginapi.ExecutorStreamChunk{}, false, err
	}
	chunk = pluginapi.ExecutorStreamChunk{Payload: append([]byte(nil), raw.Payload...)}
	if raw.Error != "" {
		chunk.Err = errors.New(raw.Error)
	}
	return chunk, raw.Done, nil
}

// forwardHostModelStream bridges a host-managed model stream to the client stream bridge.
func forwardHostModelStream(req executorRequestWithCallback, upstreamStreamID string, first pluginapi.ExecutorStreamChunk, done bool) {
	defer hostModelStreamClose(upstreamStreamID)
	emitted := false
	emit := func(payload []byte, errMsg string) bool {
		if e := hostStreamEmit(req.StreamID, payload, errMsg); e != nil {
			if !emitted && errMsg == "" {
				_ = hostStreamEmit(req.StreamID, nil, e.Error())
			}
			return false
		}
		emitted = true
		return true
	}

	if first.Payload != nil || first.Err != nil {
		if first.Err != nil {
			_ = hostStreamEmit(req.StreamID, nil, first.Err.Error())
			hostStreamClose(req.StreamID, first.Err.Error())
			return
		}
		if !emit(first.Payload, "") {
			hostStreamClose(req.StreamID, "host stream emit failed")
			return
		}
	}
	if done {
		hostStreamClose(req.StreamID, "")
		return
	}
	for {
		chunk, isDone, errRead := readHostModelStreamChunk(upstreamStreamID)
		if errRead != nil {
			_ = hostStreamEmit(req.StreamID, nil, errRead.Error())
			hostStreamClose(req.StreamID, errRead.Error())
			return
		}
		if chunk.Err != nil {
			_ = hostStreamEmit(req.StreamID, nil, chunk.Err.Error())
			hostStreamClose(req.StreamID, chunk.Err.Error())
			return
		}
		if !emit(chunk.Payload, "") {
			hostStreamClose(req.StreamID, "host stream emit failed")
			return
		}
		if isDone {
			hostStreamClose(req.StreamID, "")
			return
		}
	}
}
