// Command plugin 是 CLIProxyAPI 的动态库插件，实现 Modelscope 账号级魔粒限流控制。
//
// 限流状态机位于 modelscope-ratelimit/internal/ratelimit（仅用标准库、可单测）。本文件是
// 原生 C-ABI 适配层：把文档约定的插件方法（见 docs/cn/plugin 与 sdk/pluginabi）映射到核心
// helper，并导出 cliproxy_plugin_init；同时提供一个管理资源页（“Modelscope 限流”）展示实时
// 禁用列表。
//
// 构建（需要 CGO）：
//
//	cd plugins/modelscope-ratelimit
//	GOPROXY=https://goproxy.cn,direct go build -buildmode=c-shared -o cmd/plugin/modelscope-ratelimit.so ./cmd/plugin
//	# macOS 是 .dylib，Windows 是 .dll（交叉编译需要 C 交叉编译器）
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

// 宿主回调的函数指针类型（见 loader_unix.go 的 cliproxyHostCall）。
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

// cliproxy_call_host 同步调用一个宿主回调方法，不保存任何指针，返回宿主的返回码（0 表示成
// 功）。
static int cliproxy_call_host(const cliproxy_host_api* h, const char* method,
                              const uint8_t* req, size_t req_len, cliproxy_buffer* resp) {
	if (h == NULL || h->call == NULL) {
		return -1;
	}
	return ((cliproxy_host_call_fn)h->call)(h->host_ctx, method, req, req_len, resp);
}

// cliproxy_free_host 释放宿主返回的缓冲区。
static void cliproxy_free_host(const cliproxy_host_api* h, void* ptr, size_t len) {
	if (h != NULL && h->free_buffer != NULL) {
		((cliproxy_host_free_fn)h->free_buffer)(ptr, len);
	}
}

// cliproxy_copy_host 把宿主 API 结构体复制到插件自有内存，使函数指针在插件生命周期内持续有效；调用方用 free() 释放。
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
	"bytes"
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
	"sync/atomic"
	"time"
	_ "time/tzdata"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
	modelscope "modelscope-ratelimit/internal/ratelimit"
)

// store 在插件的整个生命周期内持有；它的 logger 转发到 host.log，因此状态变化会
// 出现在宿主自己的日志文件里。
var store = modelscope.NewStore(stderrLogger{})

const pluginVersion = "2.4.7"

var pluginHost *C.cliproxy_host_api

//go:embed status_page.html
var statusPageHTML []byte

var (
	refreshMu sync.Mutex
	// pluginCfg 是最近一次生效的插件配置，管理资源页用它决定走服务端解析还是浏览器端解析 key。
	pluginCfg *modelscope.Config
	cfgMu     sync.Mutex

	// resolvedCache 缓存服务端的 key 解析结果，避免页面自动刷新把管理 API 打爆。reconfigure 时失效。
	resolvedMu    sync.Mutex
	resolvedCache resolvedEntry

	// strategyCache 缓存宿主内置的路由策略，避免 scheduler.pick 的兜底路径每个请求都查管理 API。
	// reconfigure 时失效（TTL 与 resolvedCache 相同）。
	strategyMu    sync.Mutex
	strategyCache strategyEntry
)

const (
	resolvedCacheTTL = 60 * time.Second // 解析视图最多每分钟刷新一次
	resolvedErrTTL   = 5 * time.Minute  // 失败后退避，避免触发风控封禁
	strategyCacheTTL = 60 * time.Second // 宿主策略最多每分钟刷新一次
)

type strategyEntry struct {
	strategy string
	at       time.Time
}

type resolvedEntry struct {
	masked     map[string]string
	raw        map[string]string // authID 到原始 API key（即 m_session_id cookie 值）
	maskToRaw  map[string]string // maskedKey 到原始 API key（O(1) 查找）
	maskToSite map[string]string // maskedKey 到站点基础地址（O(1) 查找）
	siteBase   map[string]string // authID 到 Modelscope 站点基础地址（cn/ai）
	baseURL    map[string]string // authID 到上游 OpenAI 兼容基础地址
	// routeToName 把某个 provider 可被宿主路由的模型 ID 映射到真正的上游模型名。路由 ID 就是
	// CLIProxyAPI 注册进模型注册表的那个名字：alias 非空时是 alias，否则是 name（见 sdk/cliproxy
	// 的 buildConfiguredModelInfo：ModelInfo.ID = alias || name）。
	routeToName map[string]map[string]string // provider 名称 到 路由模型 ID 到 上游模型名
	// nameToRoute 是反向索引（小写后的上游模型名或别名 到 路由模型 ID），这样客户端直接请求原始
	// 上游名时也能映射回宿主可路由的那个标识。
	nameToRoute    map[string]map[string]string
	orderedAuthIDs []string            // 稳定的遍历顺序（确定性的 idx）
	dedupedAuthIDs []string            // 每个原始 API key 只保留一个 auth ID（配置顺序里的第一个）
	rawToAuthIDs   map[string][]string // 原始 API key 到共享该账号的所有 auth ID
	providerOrder  map[string][]string // provider 名称到按 config.yaml 里 api-key 顺序排列的 auth ID
	realTotal      int
	unmatched      []string // 配置里但上游找不到的 provider
	err            error
	at             time.Time
}

// logPrefix 给插件输出的每一行打上前缀，方便在共享的宿主日志里 grep 出来。
const logPrefix = "modelscope-ratelimit: "

// hostLogDisabled 在一次失败后就锁死 host.log 转发。这个回调是尽力而为：如果插件一直敲一扇坏掉的
// 门，每条日志都要付出一次失败的 cgo 往返代价。
var hostLogDisabled atomic.Bool

// hostLogCall 做了一层间接，便于测试在没有真实宿主的情况下同时覆盖转发路径和回退路径。
var hostLogCall = callHost

// hostReady 判断宿主的回调表是否已经交付。这里做成间接调用（而不是就地读 cgo 指针），因为测试无法
// 引用 cgo 伪包，这样日志这条接缝也能被测试覆盖。
var hostReady = func() bool { return pluginHost != nil }

// rpcHostLogRequest 与宿主胶水层的 rpcHostLogRequest 保持一致。
type rpcHostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

// emitHostLog 把一行日志转发进 CLIProxyAPI 自己的 logger。
//
// 插件跑在自己的 Go runtime 里，标准库 logger 只会写到插件的 stderr。开启 logging-to-file 之后那
// 只出现在控制台窗口，永远进不了 main.log，管理端的日志接口也就什么都看不到。host.log 能把插件的诊
// 断信息写进同一个滚动文件；带上调用方的 host_callback_id 还能给这行打上宿主请求 id。callbackID 可
// 以为空（后台协程没有请求上下文）。
func emitHostLog(level, callbackID, message string) bool {
	if !hostReady() || hostLogDisabled.Load() {
		return false
	}
	raw, errMarshal := json.Marshal(rpcHostLogRequest{Level: level, Message: message, HostCallbackID: callbackID})
	if errMarshal != nil {
		return false
	}
	if _, errCall := hostLogCall(pluginabi.MethodHostLog, raw); errCall != nil {
		hostLogDisabled.Store(true)
		log.Print(logPrefix + "host.log unavailable, falling back to stderr: " + errCall.Error())
		return false
	}
	return true
}

// logAt 输出一条插件诊断。info 级始终可见；debug 级需要打开宿主的调试日志。在宿主句柄出现之前（早
// 期初始化）或者转发已被关闭时，两者都退回进程自身的 logger。
func logAt(level, callbackID, format string, args ...any) {
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	if !strings.HasPrefix(message, logPrefix) {
		message = logPrefix + message
	}
	if emitHostLog(level, callbackID, message) {
		return
	}
	log.Print(message)
}

func logInfo(format string, args ...any) { logAt("info", "", format, args...) }

// maxLogBody 限制上游错误体有多少内容进入日志。错误文档通常很短，加上限是为了防止异常巨大的响应体
// 把 main.log 撑爆。
const maxLogBody = 300

// logBodyExcerpt 返回响应体的单行、限长摘要，用于诊断。只有错误路径会用到它，因此不会带上正常完成
// 响应的内容。
func logBodyExcerpt(body string) string {
	body = strings.TrimSpace(body)
	body = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(body)
	if len(body) > maxLogBody {
		return body[:maxLogBody] + "..."
	}
	return body
}

func logDebug(format string, args ...any) { logAt("debug", "", format, args...) }

// logInfoRequest 与 logDebugRequest 会带上宿主回调 id，于是这一行就被打上了产生它的那次请求的请求
// id。
func logInfoRequest(callbackID, format string, args ...any) {
	logAt("info", callbackID, format, args...)
}

// configBannerMu 保护下面的横幅记账状态。
var (
	configBannerMu      sync.Mutex
	lastConfigBanner    string
	pendingConfigBanner string
)

// logConfigBanner 在每次配置真正变化时记下生效参数，留待下一次输出。
//
// 它故意不直接打印：宿主在 logging.ConfigureLogOutput 挂上滚动日志文件之前就会执行
// pluginhost.ApplyConfig（经由 configure() 落到这里），所以在 configure 期间写的行只能到控制台，
// 永远进不了 main.log。并且 plugin.register 与 plugin.reconfigure 每次重载都会触发好几次，急着打印
// 还会把这一行重复输出。
func logConfigBanner(cfg *modelscope.Config) {
	if cfg == nil {
		return
	}
	// 这里绝不能包含管理密钥或任何凭据内容。
	line := fmt.Sprintf("configured v%s providers=%s use_responses=%s skip_provider_on_429=%t cooldown=%.1fs host=%s",
		pluginVersion, strings.Join(cfg.Providers, ","), strings.Join(cfg.UseResponses, ","),
		cfg.SkipProviderOn429, cfg.InsufficientQuotaCooldown, cfg.HostBaseURL)
	configBannerMu.Lock()
	if line != lastConfigBanner {
		lastConfigBanner = line
		pendingConfigBanner = line
	}
	configBannerMu.Unlock()
}

// flushConfigBanner 把已记录的横幅恰好输出一次。触发点是 configure 之后第一次运行期宿主调用，以及
// 启动后的账号拉取，因此即使代理没有流量也会报告它加载了什么配置。
func flushConfigBanner() {
	configBannerMu.Lock()
	line := pendingConfigBanner
	pendingConfigBanner = ""
	configBannerMu.Unlock()
	if line != "" {
		logInfo("%s", line)
	}
}

func logDebugRequest(callbackID, format string, args ...any) {
	logAt("debug", callbackID, format, args...)
}

// stderrLogger 是核心状态机的日志出口。名字沿用历史，实际现在走 host.log。
type stderrLogger struct{}

func (stderrLogger) Printf(format string, args ...any) { logInfo(format, args...) }

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	// 保存 host.* 回调指针，供执行器做 HTTP 和流式调用。管理 API 的 key 解析仍然直接用
	// net/http（见 httpDo），以避免状态页自动刷新那条路径上出现 cgo panic。
	pluginHost = C.cliproxy_copy_host(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	// 顶层保险：插件 Go 代码里任何位置的 panic 都不能越过 cgo 边界，否则宿主会把整个插件
	// fuse 掉，浏览器上就是一个 502 "plugin resource handler failed"。这里捕获后返回一个合法的
	// 错误信封。状态页另外还有一层 safeHandleManagement 包裹，保证它退化成一页 200 的 HTML 而不是
	// 错误信封。
	defer func() {
		if rec := recover(); rec != nil {
			logInfo("recovered panic in plugin call: %v", rec)
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

// handleMethod 分发文档里约定的插件方法。返回完整的 {ok,result} / {ok:false,error} 信封字节，
// 传输层失败则返回 Go error（调用方同样会把它包成错误信封）。
func handleMethod(method string, request []byte) ([]byte, error) {
	if method != pluginabi.MethodPluginRegister && method != pluginabi.MethodPluginReconfigure {
		// 出现运行期方法调用说明宿主已经过了启动阶段、日志出口也已挂好，此时可以补写那条被推迟的配置横幅。
		flushConfigBanner()
	}
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
		// 异步流式：这里只返回响应头，数据块通过 host.stream.emit 按请求里的 stream_id 推送。
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
		// 如果响应是 429 + "insufficient balance"，说明该账号魔粒已耗尽。立刻更新账号缓存，让前端不
		// 必等待刷新就能看到真实余额。魔粒在服务端是账号维度的，所以要把核心层的禁用传播给跨 provider
		// 共享同一原始 api key 的所有 auth ID，避免用第二个托管 provider 去撞同一个耗尽账号。
		if r.Failed && r.Failure != nil && modelscope.IsInsufficientBalance(r.Failure.StatusCode, r.Failure.Body) {
			// resolved.masked 是 authID 到 maskKey(apiKey) 的映射，与 accountsCache.results 里存的
			// MaskedKey 同源。若直接用 maskKey(r.AuthID) 得到的是 auth ID 自己的 12 位十六进制后缀，跟
			// api key 的末 12 位永远对不上。
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
		// 请求里带有宿主上下文（BasePath/ResourceBasePath），本插件用不到，只需要声明自己的资源页。
		return okEnvelope(managementRegistration())

	case pluginabi.MethodManagementHandle:
		// 状态页无需鉴权且必须永远能渲染出来：绝不返回错误信封（宿主会把它变成 502）。解析失败或 panic
		// 都退化成一页合法的 200 HTML。
		return okEnvelope(safeHandleManagement(request))

	default:
		// 未知方法：返回信封级错误（不是传输层错误）。
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
	// 每次配置真正变化只打印一行，便于从宿主日志里看出是哪个构建、按什么生效参数在应答。宿主会同时调
	// 用 plugin.register 和 plugin.reconfigure，没有保护的横幅会在一次重载里重复多行。这里绝不打印
	// 管理密钥或任何凭据内容。
	logConfigBanner(cfg)
	// TODO：调试结束后移除。让 Modelscope API 调用改走代理。
	SetMSProxy(cfg.MsProxyURL)
	store.Start()
	// 注入宿主策略取数函数，让非托管兜底候选也遵循宿主自己的路由语义（通过管理 API 获取）。
	store.SetStrategyFetcher(func() string {
		return fetchHostStrategyCached(cfg.HostBaseURL, cfg.ManagementKey)
	})
	store.SetProviderOrderFetcher(func() map[string][]string {
		return fetchProviderOrderCached(cfg.HostBaseURL, cfg.ManagementKey, cfg.Providers)
	})
	// 注入午夜自动签到函数：在跨日边界（PruneAll 的 dayChanged）遍历所有已解析的 auth ID，逐个调
	// /api/v1/users/login/info（激活当日魔粒），再拉取余额和限流以更新核心状态。
	mc := &msClient{}
	store.SetMidnightSignInFunc(func() {
		cfg := currentConfig()
		if cfg == nil || len(cfg.Providers) == 0 || cfg.ManagementKey == "" {
			return
		}
		// 强制重新解析一次，拿到最新的原始 key 与站点基础地址。
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
						logInfo("recovered panic in sign-in worker: %v", rec)
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
		// 刷新账号缓存，让首次打开页面就能拿到数据。用 forceBackgroundRefresh 避免与正在进行的刷新争抢。
		go func() {
			if !forceBackgroundRefresh() {
				return
			}
			defer func() {
				if rec := recover(); rec != nil {
					logInfo("recovered panic in midnight refresh: %v", rec)
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
	// 配置已变：丢掉解析结果和策略缓存，让下一次请求或页面加载重新取数。
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{}
	resolvedMu.Unlock()
	clearAllCaches()
	strategyMu.Lock()
	strategyCache = strategyEntry{}
	strategyMu.Unlock()
	// 启动 10 秒后：给所有账号拉取 login/info（当日签到）和余额，使管理页面在首次访问前就有数据。此
	// 后的刷新只取余额和限流（不再调 login/info）。
	go func() {
		time.Sleep(10 * time.Second)
		// 到这儿宿主的启动日志已经挂好，即使还没有任何请求进来，空闲的代理也会报告它加载的配置。
		flushConfigBanner()
		if !forceBackgroundRefresh() {
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				logInfo("recovered panic in startup fetch: %v", rec)
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

// currentConfig 返回最近一次生效的插件配置（configure 之前为 nil）。
func currentConfig() *modelscope.Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return pluginCfg
}

// ---- 服务端 key 解析（直连 net/http，不走 cgo 回调）----

// resolveHTTPClient 是服务端 key 解析使用的出站客户端。它直连本机管理 API，而不是走
// host.http.do 的 cgo 回调：插件是带自己 Go runtime 的 c-shared 库，宿主回调里的 panic 会跨越
// runtime 边界、插件层的 recover() 无法挽回，结果是插件被 fuse、自动刷新的状态页上表现为 502。
// 直连本机 HTTP 只会返回错误，最多退化成一页引导提示。
var resolveHTTPClient = &http.Client{Timeout: 10 * time.Second}

// httpDo 在插件自己的 runtime 里发起 HTTP 请求，返回状态码和响应体。只用于本机管理 API，因此不需
// 要代理或宿主的传输策略。
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

// maskToken 以安全方式掩码 token：前 6 位 + "..." + 末 4 位。用于 API 响应里的 SdkToken 和
// Token 字段，避免把原始凭据泄露给无需鉴权的状态页。
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

// stableIDGen 完整复刻宿主的 synthesizer.StableIDGenerator，包括按 (kind:short) 维护的碰撞计数
// 器：当两条不同的 api-key 哈希出同一个 12 位十六进制 short 时，会追加 "-N" 后缀（例如
// "1ae0d269c959-1"）。没有这个计数器时，碰撞的 key 会合并成一条掩码条目，带后缀的那条永远不出现在
// key 明细表里。条目按配置顺序处理（管理 API 保持该顺序），并跳过 disabled 的兼容配置，与
// synthesizeOpenAICompat 一致，因此推导出的 ID 与插件实际看到的 auth ID 逐字节相同。
type stableIDGen struct {
	counters map[string]int
}

func newStableIDGen() *stableIDGen {
	return &stableIDGen{counters: make(map[string]int)}
}

// Next 复刻 synthesizer.StableIDGenerator.Next。
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

// resolveKeys 用直连 net/http 拉取 /v0/management/openai-compatibility，返回掩码 key
// （authID 到掩码）、真实配置的 key 数量，以及在上游找不到的配置 provider。auth ID 由每把
// api-key 经 stableIDGen（宿主的 StableIDGenerator 算法）推导，因此不需要再去 join auth-files
// （auth-files 不含配置式 api-key）。管理密钥只留在服务端，任何敏感内容都不会进浏览器。
func resolveKeys(baseURL, mgmtKey string, providers []string) (masked map[string]string, raw map[string]string, siteBase map[string]string, orderedAuthIDs []string, providerOrder map[string][]string, realTotal int, unmatched []string, baseURLs map[string]string, routeToName map[string]map[string]string, nameToRoute map[string]map[string]string, err error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, nil, nil, nil, nil, 0, nil, nil, nil, nil, errors.New("host_base_url is empty")
	}

	sc, body, err := httpDo("GET", base+"/v0/management/openai-compatibility", mgmtKey)
	if err != nil {
		return nil, nil, nil, nil, nil, 0, nil, nil, nil, nil, err
	}
	if sc != 200 {
		return nil, nil, nil, nil, nil, 0, nil, nil, nil, nil, fmt.Errorf("openai-compatibility: HTTP %d", sc)
	}
	var oc struct {
		List []mgmtOpenAICompatEntry `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(body, &oc); err != nil {
		return nil, nil, nil, nil, nil, 0, nil, nil, nil, nil, fmt.Errorf("decode openai-compatibility: %w", err)
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
	routeToName = make(map[string]map[string]string)
	nameToRoute = make(map[string]map[string]string)
	orderedAuthIDs = make([]string, 0)
	providerOrder = make(map[string][]string)
	realTotal = 0
	matched := make(map[string]bool, len(providers))
	idGen := newStableIDGen()
	for _, e := range oc.List {
		nm := strings.ToLower(strings.TrimSpace(e.Name))
		if len(want) > 0 && !want[nm] {
			// 非托管 provider：不收集。Responses 执行器只尝试托管 provider，非托管 provider 由宿主的内置
			// 路径（host.model.execute）按 chat 服务。
			continue
		}
		matched[nm] = true
		// 跳过 disabled 的兼容配置：宿主的合成器也是这样处理（if compat.Disabled { continue }），所以他
		// 们的 key 永远不会获得 auth ID；这里必须同样排除，才能让碰撞计数器的顺序与插件实际观察到的 auth
		// ID 完全一致。
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
		// 建立 路由ID 与 上游模型名 的双向索引。所有配置的模型都进索引，不只是有别名的：宿主设置了 alias
		// 时注册的是 alias，否则注册的是 name，因此插件请求宿主执行时两者绝不可互换使用。
		routes := make(map[string]string)
		toRoute := make(map[string]string)
		for _, m := range e.Models {
			name := strings.TrimSpace(m.Name)
			if name == "" {
				continue
			}
			routeID := strings.TrimSpace(m.Alias)
			if routeID == "" {
				routeID = name
			}
			routes[routeID] = name
			toRoute[strings.ToLower(name)] = routeID
			toRoute[strings.ToLower(routeID)] = routeID
		}
		routeToName[nm] = routes
		nameToRoute[nm] = toRoute
	}

	// 配置里写了、但没有任何上游 openai-compatibility 条目能匹配上的 provider。
	for _, p := range providers {
		if w := strings.ToLower(strings.TrimSpace(p)); w != "" && !matched[w] {
			unmatched = append(unmatched, p)
		}
	}
	return masked, raw, siteBase, orderedAuthIDs, providerOrder, realTotal, unmatched, baseURLs, routeToName, nameToRoute, nil
}

// resolveRefreshMu 保护 resolveRefreshing，即给并发 resolveKeys 调用去重的 singleflight 标记，
// 使负载下的缓存未命中最多只发一次管理 API 请求。调度器热路径是非阻塞的（fetchProviderOrderCached），
// 只有管理页面和午夜签到使用同步的 resolveKeysCached。
var (
	resolveRefreshMu  sync.Mutex
	resolveRefreshing bool
)

// resolvedExpired 判断缓存条目是否已过 TTL。出错条目使用更长的退避 TTL，避免鉴权失败时反复敲管理
// API 的门。
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

// performResolve 执行管理 API 取数并存下结果，返回新条目。同步路径（resolveKeysCached）与异步路
// 径（startResolveRefresh）共用它，保证写缓存的逻辑只有一处。
func performResolve(baseURL, mgmtKey string, providers []string) resolvedEntry {
	masked, raw, siteBase, orderedAuthIDs, providerOrder, realTotal, unmatched, baseURLs, routeToName, nameToRoute, err := resolveKeys(baseURL, mgmtKey, providers)
	dedupedAuthIDs, rawToAuthIDs, maskToRaw, maskToSite := buildDedupMaps(orderedAuthIDs, raw, siteBase)
	refreshed := resolvedEntry{masked: masked, raw: raw, maskToRaw: maskToRaw, maskToSite: maskToSite, siteBase: siteBase, baseURL: baseURLs, routeToName: routeToName, nameToRoute: nameToRoute, orderedAuthIDs: orderedAuthIDs, dedupedAuthIDs: dedupedAuthIDs, rawToAuthIDs: rawToAuthIDs, providerOrder: providerOrder, realTotal: realTotal, unmatched: unmatched, err: err, at: time.Now()}
	resolvedMu.Lock()
	resolvedCache = refreshed
	resolvedMu.Unlock()
	return refreshed
}

// startResolveRefresh 在没有正在运行的刷新时启动一次 singleflight 后台刷新。供调度器热路径使用：
// 它绝不阻塞调用方，调用方立刻拿到过期（或 nil）数据，由异步刷新为后续挑选预热缓存。
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
	// singleflight：只有一个调用方真正去做 HTTP 取数；并发调用方拿到（可能过期的）缓存条目，不再重复
	// 发管理 API 请求。
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

// buildDedupMaps 从解析出的 key 数据构建去重与查找映射。单独抽成纯函数是为了不依赖 HTTP 也能单测。
// 同一把原始 api key 出现在多个 provider（不同 auth ID）时，dedupedAuthIDs、maskToRaw、
// maskToSite 只保留第一次出现的那条（配置顺序里的第一家），而 rawToAuthIDs 会把共享该原始 key 的
// 所有 auth ID 归组，用于禁用传播。
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

// getResolved 返回缓存解析条目的值拷贝。这些 map 在构造之后不再修改，所以拷贝结构体开销很小，还能避
// 免调用方读到正在被全局替换竞争中的字段。
func getResolved() resolvedEntry {
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	return resolvedCache
}

// fetchProviderOrderCached 是调度器热路径上读取“按 provider 的配置顺序 auth ID 列表”（含当前可能被
// 禁用的 key）的入口。调度器把轮询游标索引到这份完整列表上，因此 429 重试和禁用 key 不会打乱轮换顺序。
//
// 它不阻塞：立刻返回缓存列表（过期条目对 key 排序无碍），只在缓存已到期时触发一次 singleflight 异步刷
// 新。冷缓存时返回 nil，于是 pickManaged 退回自己的观测顺序快照，任何请求都不会卡在管理 API 的 HTTP 调用
// 上。
func fetchProviderOrderCached(baseURL, mgmtKey string, providers []string) map[string][]string {
	c := getResolved()
	if resolvedExpired(c) {
		startResolveRefresh(baseURL, mgmtKey, providers)
	}
	return c.providerOrder // 冷启动或出错时为 nil，其余情况可能是过期数据
}

// fetchHostStrategy 查询 GET /v0/management/routing/strategy，返回宿主内置的凭据选择策略
// （"round-robin" 或 "fill-first"）。结果按 strategyCacheTTL 缓存，避免 scheduler.pick 的兜底路径每
// 个请求都打管理 API。任何错误都退回 "round-robin"（宿主自己的默认值），因此插件绝不会卡在策略解析上。
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

// strategyRefreshMu 保护 strategyRefreshing，即后台策略刷新的 singleflight 标记，保证兜底路径不会并发
// 发起多次刷新。
var (
	strategyRefreshMu  sync.Mutex
	strategyRefreshing bool
)

// strategyExpired 判断缓存的策略是否已过 TTL。
func strategyExpired(c strategyEntry) bool {
	if c.at.IsZero() {
		return true
	}
	return c.at.Add(strategyCacheTTL).Before(time.Now())
}

// startStrategyRefresh 在没有正在运行的刷新时启动一次 singleflight 后台策略刷新。非阻塞：调用方立刻拿
// 到过期值或默认值。
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
	// 非阻塞：触发一次异步刷新并立刻返回过期值或默认值，让 scheduler.pick 的兜底路径绝不卡在管理 API 上。
	startStrategyRefresh(baseURL, mgmtKey)
	if c.strategy != "" {
		return c.strategy
	}
	return "round-robin"
}

// pluginRegistration 构造 plugin.register 的返回结果：元数据、配置字段声明和能力声明。
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
				{Name: "providers", Type: pluginapi.ConfigFieldTypeArray, Description: "监控的提供商名称（必填）。填入顺序决定轮换顺序。示例: [\"modelscope\", \"modelscope-ai\"]"},
				{Name: "skip_provider_on_429", Type: pluginapi.ConfigFieldTypeBoolean, Description: " 上游 429 时是否跳过该 provider，打开直接轮换到下一个。"},
				{Name: "use_responses", Type: pluginapi.ConfigFieldTypeString, Description: "打开 Responses API 接口（填上游模型 name，不是）：填 “*” 或 “true” 表示全部模型；填 “false” 表示关闭；填单个上游模型名表示仅该模型；多个模型写在一行即可，例如 [\"ZhipuAI/GLM-5.2\", \"Qwen/Qwen2.5-72B-Instruct\"] 。"},
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

// ---- 管理 API：一个展示实时禁用列表的资源页 ----

// managementRegistration 声明浏览器侧的资源页，它在管理界面里以“Modelscope 限流”这个标签出现。
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

// managementRequest 与 pluginapi.ManagementRequest 保持一致（线上格式兼容）。
type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers,omitempty"`
	Query   map[string][]string `json:"Query,omitempty"`
	Body    []byte              `json:"Body,omitempty"`
}

// managementResponse 与 pluginapi.ManagementResponse 保持一致（线上格式兼容）。
type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

// safeHandleManagement 包住 handleManagement，任何 panic 或请求解析错误都退化成一页合法的 200
// HTML，而不是错误信封。宿主只要看到 management.handle 返回 ok:false 就会渲染 502 "plugin resource
// handler failed"，所以状态页绝不能这么干，哪怕自动刷新途中解析 panic 也一样。
func safeHandleManagement(request []byte) (resp managementResponse) {
	defer func() {
		if rec := recover(); rec != nil {
			logInfo("recovered panic in handleManagement: %v", rec)
			resp = mgmtHTML(statusOrGuidance())
		}
	}()
	var r managementRequest
	if err := json.Unmarshal(request, &r); err != nil {
		// 信封格式不合法：按状态页路由处理，渲染安全视图。
		r = managementRequest{Method: "GET", Path: "/status"}
	}
	return handleManagement(r)
}

// statusOrGuidance 在插件还不可用时渲染配置引导页，否则渲染未解析状态的状态页。它不发起任何 HTTP 调
// 用，因此可以安全地作为 panic 兜底。只在服务端解析被跳过或失败时使用。
func statusOrGuidance() string {
	cfg := currentConfig()
	if cfg == nil || len(cfg.Providers) == 0 || cfg.ManagementKey == "" {
		return modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceMissing, "")
	}
	// panic 兜底：这里无法安全地做解析，所以显示一页引导式错误提示，而不是一页没有数据的状态页（已没有
	// RenderStatusHTML）。
	return modelscope.RenderGuidanceHTML(cfg, modelscope.GuidanceResolutionFailed, "内部错误，请稍后刷新")
}

// handleManagement 提供资源页。资源 GET 不走管理鉴权，因此只渲染非敏感状态。
func handleManagement(r managementRequest) managementResponse {
	// 通过查询参数分流接口：?api=accounts|account|transactions|checkin-all|refresh-balance|stats|config
	api := getQuery(r, "api")
	if api != "" {
		return handleStatusAPI(r, api)
	}
	// 返回内嵌的状态页 HTML（无查询参数时给出 HTML 骨架）
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

// syncFetchAccounts 同步拉取账号数据，并防止并发刷新（例如启动后 10 秒的那个协程）。如果已经有刷新在
// 跑，就等它完成并返回缓存；否则自己拿 refreshMu 去做这次拉取。
func syncFetchAccounts(resolved resolvedEntry) managementResponse {
	if forceBackgroundRefresh() {
		defer markRefreshDone()
		refreshMu.Lock()
		defer refreshMu.Unlock()
		fresh := refreshAccountsCache(resolved)
		return mgmtJSON(map[string]any{"accounts": fresh, "fetchedAt": time.Now().Format("15:04:05")})
	}
	// 已有别的刷新在跑，等它完成。
	refreshMu.Lock()
	refreshMu.Unlock()
	if cached := getStaleAccountsCache(); cached != nil {
		return mgmtJSON(map[string]any{"accounts": cached, "fetchedAt": getAccountsCacheTime()})
	}
	// 等待之后缓存仍为空（说明那次刷新失败），这里直接自己拉取。
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
			// 强制后台刷新，并立刻返回旧数据。前端 5 秒轮询会在数据就绪后拿到新内容。
			stale := getStaleAccountsCache()
			if stale != nil {
				// 有旧数据：立刻返回，并在后台刷新。
				if forceBackgroundRefresh() {
					go func() {
						defer func() {
							if rec := recover(); rec != nil {
								logInfo("recovered panic in refresh: %v", rec)
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
			// 完全没有缓存（首次加载）：加锁同步拉取。
			return syncFetchAccounts(resolved)
		}
		stale := getStaleAccountsCache()
		if stale != nil {
			// 立刻返回旧数据；若已过期则触发后台刷新
			if shouldBackgroundRefresh() {
				go func() {
					defer func() {
						if rec := recover(); rec != nil {
							logInfo("recovered panic in refresh: %v", rec)
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
		// 完全没有缓存：加锁同步拉取。
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
		// 复用缓存里的账号概况填充资料字段，避免再次调用 users/info 和 login/info。
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
		// 并发给所有账号签到，然后刷新账号缓存，让下一次 ?api=accounts 立刻拿到新数据。
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
						logInfo("recovered panic in checkin worker: %v", rec)
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
		// 刷新账号缓存，让下一次 ?api=accounts 立刻返回。
		clearAllCaches()
		if forceBackgroundRefresh() {
			go func() {
				defer func() {
					if rec := recover(); rec != nil {
						logInfo("recovered panic in checkin refresh: %v", rec)
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
		// 并发拉取余额与流水
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
				// 更新账号缓存，让下一次 ?api=accounts 显示真实余额（修正 markAccountExhausted 之前把
				// available=0、schedule=exhausted 写死的问题）。
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
				// 用 SPEND_CONFIRM 流水推算当日已消耗
				today := time.Now().Format("2006-01-02")
				consumed := 0
				for _, rec := range tx.Records {
					if rec.GmtCreated == today && rec.Type == "SPEND_CONFIRM" {
						consumed += rec.TotalAmount
					}
				}
				result["todayConsumed"] = consumed
				// 把当日已消耗写回账号缓存，让下一次 ?api=accounts 反映最新值。
				accountsCacheMu.Lock()
				for i := range accountsCache.results {
					if accountsCache.results[i].MaskedKey == mk {
						accountsCache.results[i].TodayConsumed = consumed
						break
					}
				}
				accountsCacheMu.Unlock()
				// 把 tx.Records 序列化成 []any 以放进 JSON 响应
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

// getQuery 返回某个查询参数的第一个值。
func getQuery(r managementRequest, key string) string {
	if vals, ok := r.Query[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// getQueryInt 以 int 返回某个查询参数的第一个值，缺省时返回 -1。
func getQueryInt(r managementRequest, key string) int {
	s := getQuery(r, key)
	if s == "" {
		return -1
	}
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

// getKeysByMask 按掩码 key（末 12 位）找到原始 session id 与站点基础地址，用预先建好的
// maskToRaw、maskToSite 做 O(1) 查找。
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
		// no-store 保证每 5 秒的自动刷新都会重新取一次；否则浏览器会按启发式规则缓存，状态页看起来是旧的。
		Headers: map[string][]string{
			"content-type":  {"text/html; charset=utf-8"},
			"cache-control": {"no-store, no-cache, must-revalidate"},
			"pragma":        {"no-cache"},
			"expires":       {"0"},
		},
		Body: []byte(html),
	}
}

// okEnvelope 把结果值序列化进成功信封。
func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

// errorEnvelope 构造一个错误信封。
func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return raw
}

// writeResponse 把信封字节拷贝进宿主提供的 cliproxy_buffer。
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

// ---- Responses 透传执行器 ----

// executorRequestWithCallback 与 rpcExecutorRequest 一致，让胶水层能保留 host_callback_id（宿主
// host.http.* 需要它做上下文）和 stream_id（异步 host.stream.emit 需要）。
type executorRequestWithCallback struct {
	pluginapi.ExecutorRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
}

// hostHTTPRequest 是 host.http.do 期望的线上格式。
type hostHTTPRequest struct {
	Method         string              `json:"method,omitempty"`
	URL            string              `json:"url,omitempty"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

// hostHTTPResponse 与 pluginapi.HTTPResponse 的 JSON 结构一致。
type hostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

// hostHTTPStreamResponse 与 rpcHostHTTPStreamResponse 一致。
type hostHTTPStreamResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
}

// hostStreamReadResponse 与 rpcHostHTTPStreamReadResponse 一致。
type hostStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// callHost 调用一次 host.* 回调，并返回解码后的结果字节。
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

// hostHTTPDo 通过宿主的传输策略执行一次非流式上游 HTTP 请求，返回状态码、响应头和响应体。
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

// hostHTTPDoStream 通过宿主的传输策略执行一次流式上游 HTTP 请求，并返回宿主管理的 stream id 供后续读取。
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

// hostHTTPStreamRead 从宿主托管的 HTTP 流里读取一个数据块。
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

// hostHTTPStreamClose 关闭宿主托管的 HTTP 流。
func hostHTTPStreamClose(streamID string) {
	_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, mustJSONMarshal(rpcHostHTTPStreamCloseRequest{StreamID: streamID}))
}

// hostStreamEmit 把执行器的一个流数据块推给宿主的流通道。
func hostStreamEmit(streamID string, payload []byte, errMsg string) error {
	_, err := callHost(pluginabi.MethodHostStreamEmit, mustJSONMarshal(rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
		Error:    errMsg,
	}))
	return err
}

// hostStreamClose 关闭执行器的流通道，可附带一个错误。
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

// rpcHostModelExecutionRequest 与宿主胶水层的同名结构一致：内嵌 HostModelExecutionRequest 外加
// host_callback_id，宿主由此解析调用方上下文，并为非递归的兜底设置
// SkipRouterPluginID/SkipInterceptorPluginID。
type rpcHostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// routeResponsesRequest 把 OpenAI Responses API 请求路由到插件自己的执行器，以便转发给上游
// /v1/responses。Chat Completions 请求继续走宿主的常规 provider 路径（openai-compatibility
// /chat/completions）。
func routeResponsesRequest(req pluginapi.ModelRouteRequest) pluginapi.ModelRouteResponse {
	cfg := currentConfig()
	if cfg == nil || len(cfg.Providers) == 0 {
		return pluginapi.ModelRouteResponse{Handled: false}
	}
	switch strings.ToLower(strings.TrimSpace(req.SourceFormat)) {
	case "openai-response", "responses", "codex":
	default:
		return pluginapi.ModelRouteResponse{Handled: false}
	}
	if !responsesRouteModelEnabled(cfg, req.RequestedModel) {
		return pluginapi.ModelRouteResponse{Handled: false}
	}
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "responses-passthrough",
	}
}

func responsesRouteModelEnabled(cfg *modelscope.Config, requestedModel string) bool {
	if cfg == nil {
		return false
	}
	if len(cfg.UseResponses) == 0 {
		return false
	}
	if cfg.UsesResponses("*") {
		return true
	}
	alias := strings.TrimSpace(requestedModel)
	resolved := ensureResolved()
	mapped := false
	for _, name := range cfg.Providers {
		provider := strings.ToLower(strings.TrimSpace(name))
		if resolved.routeToName == nil {
			break
		}
		// 按上游模型名匹配（use_responses 文档里记录的就是上游名），同时接受宿主的路由 ID 和原始上游名。
		upstream, route := resolveProviderModel(resolved, provider, alias)
		if upstream == "" || route == "" {
			continue
		}
		mapped = true
		if cfg.UsesResponses(upstream) {
			return true
		}
	}
	if mapped {
		return false
	}
	// 没有托管模型映射可查时，别名与上游模型名是等价的。
	return cfg.UsesResponses(alias)
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
	if resolved := getResolved(); resolved.routeToName != nil {
		if upstream, ok := resolved.routeToName[provider][alias]; ok && upstream != "" {
			return upstream
		}
	}
	return alias
}

// resolveProviderModel 把客户端请求的模型翻译成托管 provider 那一步需要的两个不同标识：
//
//   - upstream：直连上游 /v1/responses 时写进请求体的真实模型名（Modelscope API 期望的值）。
//   - route：CLIProxyAPI 能路由的模型 ID（配了 alias 就是 alias，否则是 name）。
//     host.model.execute 是用精确查找在模型注册表里解析 provider 列表的，所以对配了别名的模型
//     传 `upstream` 会得到 model_not_found。
//
// 同一个上游模型在不同 provider 上的别名可以完全不同（modelscope 把
// deepseek-ai/DeepSeek-V4-Pro-0813 注册成 deepseek-v4-pro，而 modelscope-my 把同一个模型注册成
// glm-5.2），因此一家的别名并不是下一家的索引里存在的名字。本地查找落空时，会先经其它托管
// provider 把请求解析成规范的上游模型名，再回到这里查一次。
//
// 当该 provider 有索引、但既不提供请求名也不提供规范名时，两个结果都返回空，执行器必须跳过它。
// 若改为把客户端别名转发出去，只会换来上游 400 "Invalid model id"，而该状态不可重试，于是整个
// 请求被终止，轮换也就不会继续到下一个 provider。
//
// 完全拿不到模型列表的 provider（管理接口出错，或兼容条目没有 models: 段）按自身映射，保留建索
// 引之前的行为。
func resolveProviderModel(resolved resolvedEntry, provider, requested string) (upstream string, route string) {
	p := strings.ToLower(strings.TrimSpace(provider))
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", ""
	}
	routes := resolved.routeToName[p]
	if len(routes) == 0 {
		// 该 provider 没有模型索引：按自身映射，仍可路由。
		return requested, requested
	}
	if upstream, route, found := providerModelLookup(resolved, p, requested); found {
		return upstream, route
	}
	cfg := currentConfig()
	if cfg == nil {
		// 没有 provider 列表可做交叉解析：保持旧形态，即只有直连上游调用还可能服务该模型。
		return requested, ""
	}
	name := canonicalUpstreamModel(resolved, cfg.Providers, requested)
	if name == "" || strings.EqualFold(name, requested) {
		return "", ""
	}
	if upstream, route, found := providerModelLookup(resolved, p, name); found {
		return upstream, route
	}
	// 这个上游模型由别的 provider 提供，这一家不提供。
	return "", ""
}

// providerModelLookup 只针对某个 provider 自己的模型索引解析 requested。found 为 false 表示该家
// 根本没登记这个模型，调用方由此得知这里没有答案，而不是拿到一个形状未知的结果。
func providerModelLookup(resolved resolvedEntry, lowerProvider, requested string) (upstream string, route string, found bool) {
	routes := resolved.routeToName[lowerProvider]
	toRoute := resolved.nameToRoute[lowerProvider]
	if name, ok := routes[requested]; ok && strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name), requested, true
	}
	if routeID, ok := toRoute[strings.ToLower(requested)]; ok {
		if name, okName := routes[routeID]; okName && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name), routeID, true
		}
		return requested, routeID, true
	}
	return "", "", false
}

// canonicalUpstreamModel 按配置顺序找到第一个认识该模型的托管 provider，把客户端请求的模型映射成
// 真实的上游模型名。
func canonicalUpstreamModel(resolved resolvedEntry, providers []string, requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return ""
	}
	for _, provider := range providers {
		p := strings.ToLower(strings.TrimSpace(provider))
		if len(resolved.routeToName[p]) == 0 {
			continue
		}
		if upstream, _, found := providerModelLookup(resolved, p, requested); found && strings.TrimSpace(upstream) != "" {
			return strings.TrimSpace(upstream)
		}
	}
	return ""
}

func upstreamModelForRetryStep(step retryStep, resolved resolvedEntry, alias string) string {
	upstream, _ := resolveProviderModel(resolved, step.provider, alias)
	return upstream
}

func responsesProtocolEnabled(cfg *modelscope.Config, model string) bool {
	return cfg != nil && cfg.UsesResponses(model)
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

// buildRetrySequence 返回有序的托管 provider 步骤表：一把凭据一步，按 providers 配置顺序一家接一家排。
// 命中 Responses 开关的步骤会调用 Store.NextProviderKey 取 key 并直连上游 /v1/responses；未命中的
// provider 仍然每家一次走宿主 chat 兜底（由 chatProviderAttempted 去重），由宿主完成 Responses 与
// Chat 的互转。
//
// 步长取到 key 粒度是有意为之，与 skip_provider_on_429 无关。insufficient balance 这类 429 只
// 禁用一把凭据，所以下一步必须是同一家另一把凭据；只有这一家已经没有可用凭据，才交给下一家。旧实
// 现（开启该开关时）把步骤表压缩成每家一步，于是任何 key 级失败都像是 provider 级失败：请求会在第
// 一家还剩约 140 把可用凭据的情况下跳到第二家，而第二家应答的是另一个上游模型。
//
// 现在该开关只决定池级 insufficient_quota（Chat 的 429 JSON，或 Responses 以 200 + SSE
// response.failed 回送的同一种 code）是否把某家放进 skip 集；两个执行器循环都会尊重 skip 集，丢掉
// 这一家剩下的步骤。Responses 侧 key 级速率拒绝的兜底另见 responsesKeyRateLimitBailAfter。
func buildRetrySequence(resolved resolvedEntry, cfg *modelscope.Config) []retryStep {
	var steps []retryStep
	for _, name := range cfg.Providers {
		key := strings.ToLower(strings.TrimSpace(name))
		count := len(resolved.providerOrder[key])
		if count == 0 {
			// 冷启动或解析失败：仍然给这一家一步，让它的宿主 chat 兜底有机会执行；确实没有凭据可调用时由
			// resolveStep 报失败。
			count = 1
		}
		for i := 0; i < count; i++ {
			steps = append(steps, retryStep{provider: name})
		}
	}
	return steps
}

// resolveStep 用 Store 里该 provider 自己的游标，为一次托管 provider 重试步骤解析出 auth ID、API
// key、基础地址和上游模型名，因此 key 顺序跨请求保持不变。
//
// 游标只允许落在仍然能应答的凭据上：先按核心层的禁用状态把这一家的 key 过滤一遍，再交给
// Store.NextProviderKey，这与 SchedulerPick 在轮换前做的是同一件事。把这一家的完整 key 列表直接
// 交给它会出问题 —— 成功同样会推进游标，所以健康流量跑一段时间后，游标会走进当天已被禁用的 key
// 区间，于是一把死 key 就能吃掉这一家的一步（旧实现每家只有极少的几步时，甚至能吃掉整步），请求
// 在第一家还有可用 key 时就悄悄落到第二家。
func resolveStep(step retryStep, resolved resolvedEntry, alias string) (retryCandidate, bool) {
	providerKey := strings.ToLower(strings.TrimSpace(step.provider))
	ids := resolved.providerOrder[providerKey]
	if len(ids) == 0 {
		return retryCandidate{}, false
	}
	model := upstreamModelForRetryStep(step, resolved, alias)
	if model == "" {
		// 该 provider 完全不提供请求的这个模型。
		return retryCandidate{}, false
	}
	now := store.Now()
	usable := make([]string, 0, len(ids))
	for _, id := range ids {
		if store.IsKeyDisabled(id, model, now) {
			continue
		}
		if strings.TrimSpace(resolved.raw[id]) == "" || strings.TrimSpace(resolved.baseURL[id]) == "" {
			continue
		}
		usable = append(usable, id)
	}
	if len(usable) == 0 {
		return retryCandidate{}, false
	}
	authID := store.NextProviderKey(step.provider, usable)
	if authID == "" {
		return retryCandidate{}, false
	}
	return retryCandidate{
		authID:   authID,
		provider: step.provider,
		apiKey:   resolved.raw[authID],
		baseURL:  resolved.baseURL[authID],
		model:    model,
	}, true
}

func hostModelRequestForCandidate(req executorRequestWithCallback, model string) executorRequestWithCallback {
	out := req
	out.Model = strings.TrimSpace(model)
	return out
}

// hostModelRequestForProvider 构造某个 provider 的嵌套 chat 调用，并用内部标记头把它钉定在该
// provider 上，这样嵌套执行里出现 429 时，宿主不会改用别家凭据应答。
//
// 请求头是复制出来的：调用方的头属于外层请求，后面直连上游和最后不设限的宿主兜底还会继续用它们。
//
// 宿主在执行嵌套调用时仍会跑本插件的 SchedulerPick，所以 provider 顺序和 key 轮换规则与外层一致。
func hostModelRequestForProvider(req executorRequestWithCallback, model, provider string) executorRequestWithCallback {
	out := hostModelRequestForCandidate(req, model)
	if strings.TrimSpace(provider) == "" {
		return out
	}
	headers := make(http.Header, len(out.Headers)+1)
	for key, values := range out.Headers {
		headers[key] = append([]string(nil), values...)
	}
	headers.Set(modelscope.PinnedProviderHeader, provider)
	out.Headers = headers
	return out
}

// providerHasUsableCredential 判断插件自己的调度器是否还会把这一家的请求交给它的某把凭据。
//
// chat 兜底这一步是让宿主去执行*共享*的路由模型，宿主会对所有提供该别名的 provider 重跑
// scheduler.pick。如果首选 provider 已在 skip 集里（池级 insufficient_quota）或者只剩被日禁用的 key，宿主就会用别家
// 凭据应答这次嵌套调用，于是本该走原生 Responses 的后面那家被悄悄降级成 chat，它自己的原生步骤根本
// 没机会执行。这里用与调度器相同的判定条件，保证兜底仍然落在本该试探的那一家上。
func providerHasUsableCredential(provider string, resolved resolvedEntry) bool {
	cfg := currentConfig()
	if cfg == nil {
		return true
	}
	if cfg.SkipProviderOn429 && store.IsProviderSkipped(provider) {
		return false
	}
	ids := resolved.providerOrder[strings.ToLower(strings.TrimSpace(provider))]
	if len(ids) == 0 {
		// 拿不到该家的 key 列表（冷启动解析）：交给宿主判断。
		return true
	}
	now := store.Now()
	for _, id := range ids {
		if !store.IsKeyDisabled(id, "", now) {
			return true
		}
	}
	return false
}

// retryableResponsesStatus 判断上游状态码是否"可在本请求内继续尝试下一家"。
// 其中 405 是偶发性的致命错误：请求体里含有被上游拦截的字符，上游直接回 Method
// Not Allowed。换 key 无用（同一个 body 仍会触发），所以视为不可在本家重试，立即跳
// 到下一家（skip_provider_on_429 开启时把本家摘进 skip 集）。429 与 5xx 仍按常规可
// 重试处理。
func retryableResponsesStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		statusCode == http.StatusMethodNotAllowed ||
		statusCode >= http.StatusInternalServerError
}

// responsesKeyRateLimitBailAfter 是同一次请求内，同一个 provider 连续返回 key 级 Responses
// 额度/速率拒绝（response.failed + rate_limit_exceeded，且 body 不带池级 insufficient_quota）
// 后，在本请求内放弃这一家的阈值。
//
// 这类失败不会禁用任何 key（速率窗口自己会恢复），所以本家的可用凭据数不会因失败而减少；没有
// 阈值时，一次池级速率风暴会把这一家上百把 key 全撞一遍。取 3 是权衡结果：失败真的按 key 分布
// 时连续 3 把都失败已经很少见，而最坏情况只多花 3 个往返。计数只活在这次请求里，绝不写进跨请
// 求的 skip 集 —— 下一个请求仍然先试这一家的下一把 key。
const responsesKeyRateLimitBailAfter = 3

// noteResponsesKeyRateLimit 给该 provider 在本次请求内的连击计数加一，返回是否已到放弃阈值。
func noteResponsesKeyRateLimit(streak map[string]int, providerName string) bool {
	streak[providerName]++
	return streak[providerName] >= responsesKeyRateLimitBailAfter
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

	unsupportedProviders := make(map[string]bool)
	chatProviderAttempted := make(map[string]bool)
	keyRateLimitStreak := make(map[string]int)
	for _, step := range steps {
		providerName := strings.ToLower(strings.TrimSpace(step.provider))
		// 该 provider 一旦回过池级 insufficient_quota（429 JSON 或 Responses 的 200 + SSE 事
		// 件），整家就被摘掉（skip_provider_on_429 只授权做这件事）。key 级失败不会走到这里，
		// 因此剩下的可用 key 仍会继续轮换。
		if unsupportedProviders[providerName] || (cfg.SkipProviderOn429 && store.IsProviderSkipped(step.provider)) {
			unsupportedProviders[providerName] = true
			continue
		}
		candidateModel, routeModel := resolveProviderModel(resolved, step.provider, alias)
		if candidateModel == "" {
			continue
		}
		if !responsesProtocolEnabled(cfg, candidateModel) {
			if chatProviderAttempted[providerName] {
				continue
			}
			// 只在确实能应答的 provider 上走 chat 兜底；否则宿主会用别家凭据满足这一步，那一家的原生协议就丢了。
			if !providerHasUsableCredential(step.provider, resolved) {
				logInfoRequest(req.HostCallbackID, "responses chat fallback skipped provider=%s (in 429 skip set or all credentials disabled)", step.provider)
				continue
			}
			chatProviderAttempted[providerName] = true

			// 该 provider 没有宿主可路由的 ID，就没有 chat 兜底可走：下面的直连上游调用
			// 是唯一路径。
			if routeModel == "" {
				logInfoRequest(req.HostCallbackID, "responses skip provider=%s model=%s (no host-routable model, Responses passthrough disabled for it)", step.provider, alias)
				continue
			}
			// 宿主是用精确 ID 在模型注册表里解析 provider 的，配了别名的兼容模型只以别名注册：因此这里必须发送路
			// 由 ID，绝不发送上游模型名。宿主会自己改写它那把凭据对应的上游名，插件调度器对嵌套调用仍会运行，所以
			// provider 仍然保持配置顺序。
			logDebugRequest(req.HostCallbackID, "responses attempt provider=%s mode=chat-fallback pinned=true model=%s upstream=%s", step.provider, routeModel, candidateModel)
			hostResp, errHost := hostModelExecute(hostModelRequestForProvider(req, routeModel, step.provider))
			if errHost != nil {
				logInfoRequest(req.HostCallbackID, "responses chat fallback failed provider=%s model=%s: %v", step.provider, routeModel, errHost)
				continue
			}
			if hostResp.StatusCode < 200 || hostResp.StatusCode >= 300 {
				logInfoRequest(req.HostCallbackID, "responses chat fallback provider=%s model=%s -> HTTP %d", step.provider, routeModel, hostResp.StatusCode)
				if retryableResponsesStatus(hostResp.StatusCode) {
					continue
				}
				return pluginapi.ExecutorResponse{}, fmt.Errorf("host chat fallback: HTTP %d: %s", hostResp.StatusCode, string(hostResp.Body))
			}
			// 嵌套调用的凭据由宿主自己的调度器挑选，所以这里只能证明首选 provider 接受了该请求；同一个请求 id 下
			// 的 `schedule` 行才写明实际用的是哪个 auth。
			logDebugRequest(req.HostCallbackID, "responses served via=host-chat-fallback preferred_provider=%s model=%s (credential chosen by host scheduler)", step.provider, routeModel)
			return pluginapi.ExecutorResponse{Payload: hostResp.Body, Headers: hostResp.Headers}, nil
		}
		cand, ok := resolveStep(step, resolved, alias)
		if !ok {
			continue
		}
		if store.IsKeyDisabled(cand.authID, cand.model, store.Now()) {
			continue
		}
		upstreamBody := withModel(originalBody, cand.model)
		upstreamReq := responsesUpstreamRequest(req.Headers, cand.apiKey, strings.TrimSuffix(cand.baseURL, "/")+"/responses", upstreamBody)
		logDebugRequest(req.HostCallbackID, "responses attempt provider=%s mode=passthrough model=%s key=%s", cand.provider, cand.model, maskKey(cand.apiKey))
		resp, errDo := hostHTTPDo(upstreamReq, req.HostCallbackID)
		if errDo != nil {
			logInfoRequest(req.HostCallbackID, "responses upstream request failed provider=%s model=%s: %v", cand.provider, cand.model, errDo)
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, errDo.Error(), cand.provider, cand.model))
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			logInfoRequest(req.HostCallbackID, "responses upstream HTTP %d provider=%s model=%s body=%s", resp.StatusCode, cand.provider, cand.model, logBodyExcerpt(string(resp.Body)))
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, resp.StatusCode, string(resp.Body), cand.provider, cand.model))
			if retryableResponsesStatus(resp.StatusCode) {
				if resp.StatusCode == http.StatusMethodNotAllowed && cfg.SkipProviderOn429 {
					unsupportedProviders[providerName] = true
				}
				continue
			}
			return pluginapi.ExecutorResponse{}, fmt.Errorf("upstream responses: HTTP %d: %s", resp.StatusCode, string(resp.Body))
		}
		respBody := string(resp.Body)
		if modelscope.IsResponsesRateLimitExceeded(respBody) {
			// 池级还是 key 级只看 body：ApplyResponsesRateLimit 据此决定是摘整家还是只冷却
			// 这一把 key。这里再补一层本请求内的连击上限，专门兜住 key 级（不禁用 key）那一类。
			poolLevel := modelscope.IsPoolLevelQuotaExhausted(respBody)
			bail := false
			if !poolLevel {
				bail = noteResponsesKeyRateLimit(keyRateLimitStreak, providerName)
			}
			logInfoRequest(req.HostCallbackID, "responses rate limited provider=%s model=%s key=%s pool_level=%v streak=%d/%d body=%s, rotating",
				cand.provider, cand.model, maskKey(cand.apiKey), poolLevel, keyRateLimitStreak[providerName], responsesKeyRateLimitBailAfter, logBodyExcerpt(respBody))
			store.ApplyResponsesRateLimit(cand.authID, "openai-compatible-"+cand.provider, req.Model, respBody, store.Now())
			if bail {
				logInfoRequest(req.HostCallbackID, "responses provider=%s hit %d consecutive key-level rate limits, moving to the next provider for this request only",
					cand.provider, keyRateLimitStreak[providerName])
				unsupportedProviders[providerName] = true
			}
			continue
		}
		if modelscope.IsResponsesFailed(respBody) {
			logInfoRequest(req.HostCallbackID, "responses failed provider=%s model=%s body=%s", cand.provider, cand.model, logBodyExcerpt(respBody))
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, resp.StatusCode, respBody, cand.provider, cand.model))
			continue
		}
		// Modelscope 会把部分拒绝以 HTTP 200 加一段裸错误文档返回。要把它
		// 当成这一家失败了，而不是把一个不是 Response 资源的响应体
		// 交给客户端。
		if modelscope.IsResponsesErrorPayload(respBody) {
			logInfoRequest(req.HostCallbackID, "responses provider=%s model=%s returned an error payload with HTTP 200: %s", cand.provider, cand.model, logBodyExcerpt(respBody))
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, resp.StatusCode, respBody, cand.provider, cand.model))
			continue
		}
		store.OnUsage(modelscope.UsageRecord{
			Provider: "openai-compatible-" + cand.provider,
			AuthID:   cand.authID,
			Model:    cand.model,
			Alias:    req.Model,
		})
		logDebugRequest(req.HostCallbackID, "responses served provider=%s mode=passthrough model=%s", cand.provider, cand.model)
		return pluginapi.ExecutorResponse{Payload: resp.Body, Headers: headerMapToHTTP(resp.Headers)}, nil
	}

	// 所有托管 provider 都已通过 Responses 透传耗尽。交回宿主的内置路径，让非托管
	// provider 按常规 chat 协议服务本次请求（Responses 与 Chat 的转换由宿主完成）。
	// 宿主会跳过本插件的路由器和拦截器（不递归）；插件调度器仍会运行，并跳过上面
	// 被禁用的托管 key。
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
	// responsesStreamPreflightErrorPayload 标记那种以 HTTP 200 建连、但送回
	// 来的是错误文档而不是 SSE 事件的流。
	responsesStreamPreflightErrorPayload
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

	unsupportedProviders := make(map[string]bool)
	chatProviderAttempted := make(map[string]bool)
	keyRateLimitStreak := make(map[string]int)
	for _, step := range steps {
		providerName := strings.ToLower(strings.TrimSpace(step.provider))
		// 该 provider 一旦回过池级 insufficient_quota（429 JSON 或 Responses 的 200 + SSE 事
		// 件），整家就被摘掉（skip_provider_on_429 只授权做这件事）。key 级失败不会走到这里，
		// 因此剩下的可用 key 仍会继续轮换。
		if unsupportedProviders[providerName] || (cfg.SkipProviderOn429 && store.IsProviderSkipped(step.provider)) {
			unsupportedProviders[providerName] = true
			continue
		}
		candidateModel, routeModel := resolveProviderModel(resolved, step.provider, alias)
		if candidateModel == "" {
			continue
		}
		if !responsesProtocolEnabled(cfg, candidateModel) {
			if chatProviderAttempted[providerName] {
				continue
			}
			if !providerHasUsableCredential(step.provider, resolved) {
				logInfoRequest(req.HostCallbackID, "responses stream chat fallback skipped provider=%s (in 429 skip set or all credentials disabled)", step.provider)
				continue
			}
			chatProviderAttempted[providerName] = true

			// 该 provider 没有宿主可路由的 ID，就没有 chat 兜底可走：下面的直连上游流是
			// 唯一路径。
			if routeModel == "" {
				logInfoRequest(req.HostCallbackID, "responses stream skip provider=%s model=%s (no host-routable model, Responses passthrough disabled for it)", step.provider, alias)
				continue
			}
			// 传路由名而不是上游模型名：原因见 executeResponsesPassthrough。
			logDebugRequest(req.HostCallbackID, "responses stream attempt provider=%s mode=chat-fallback pinned=true model=%s upstream=%s", step.provider, routeModel, candidateModel)
			hostStream, errHost := hostModelExecuteStream(hostModelRequestForProvider(req, routeModel, step.provider))
			if errHost != nil {
				logInfoRequest(req.HostCallbackID, "responses chat fallback stream failed provider=%s model=%s: %v", step.provider, routeModel, errHost)
				continue
			}
			if hostStream.StatusCode != http.StatusOK {
				logInfoRequest(req.HostCallbackID, "responses chat fallback stream provider=%s model=%s -> HTTP %d", step.provider, routeModel, hostStream.StatusCode)
				if retryableResponsesStatus(hostStream.StatusCode) {
					hostModelStreamClose(hostStream.StreamID)
					continue
				}
				hostModelStreamClose(hostStream.StreamID)
				return pluginapi.ExecutorStreamResponse{}, fmt.Errorf("host chat fallback stream: HTTP %d", hostStream.StatusCode)
			}
			first, done, errRead := readHostModelStreamChunk(hostStream.StreamID)
			if errRead != nil {
				hostModelStreamClose(hostStream.StreamID)
				continue
			}
			logDebugRequest(req.HostCallbackID, "responses stream served via=host-chat-fallback preferred_provider=%s model=%s (credential chosen by host scheduler)", step.provider, routeModel)
			go forwardHostModelStream(req, hostStream.StreamID, first, done)
			return pluginapi.ExecutorStreamResponse{Headers: hostStream.Headers}, nil
		}
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
		logDebugRequest(req.HostCallbackID, "responses stream attempt provider=%s mode=passthrough model=%s key=%s", cand.provider, cand.model, maskKey(cand.apiKey))
		stream, errStream := hostHTTPDoStream(upstreamReq, req.HostCallbackID)
		if errStream != nil {
			logInfoRequest(req.HostCallbackID, "responses upstream stream open failed provider=%s model=%s: %v", cand.provider, cand.model, errStream)
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, errStream.Error(), cand.provider, cand.model))
			continue
		}
		if stream.StatusCode != 200 {
			bodyBytes, _, errRead := readHostStream(stream.StreamID, false)
			hostHTTPStreamClose(stream.StreamID)
			logInfoRequest(req.HostCallbackID, "responses upstream stream HTTP %d provider=%s model=%s body=%s", stream.StatusCode, cand.provider, cand.model, logBodyExcerpt(string(bodyBytes)))
			if errRead != nil {
				store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, stream.StatusCode, errRead.Error(), cand.provider, cand.model))
			} else {
				store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, stream.StatusCode, string(bodyBytes), cand.provider, cand.model))
			}
			if retryableResponsesStatus(stream.StatusCode) {
				if stream.StatusCode == http.StatusMethodNotAllowed && cfg.SkipProviderOn429 {
					unsupportedProviders[providerName] = true
				}
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
			// 与直连分支同理：池级由 ApplyResponsesRateLimit 摘整家，key 级只冷却这一把 key，
			// 这里补的是 key 级那一类在本请求内的连击上限。
			poolLevel := modelscope.IsPoolLevelQuotaExhausted(preflight.reason)
			bail := false
			if !poolLevel {
				bail = noteResponsesKeyRateLimit(keyRateLimitStreak, providerName)
			}
			logInfoRequest(req.HostCallbackID, "responses stream rate limited provider=%s model=%s key=%s pool_level=%v streak=%d/%d body=%s, rotating",
				cand.provider, cand.model, maskKey(cand.apiKey), poolLevel, keyRateLimitStreak[providerName], responsesKeyRateLimitBailAfter, logBodyExcerpt(preflight.reason))
			hostHTTPStreamClose(stream.StreamID)
			store.ApplyResponsesRateLimit(cand.authID, "openai-compatible-"+cand.provider, req.Model, preflight.reason, store.Now())
			if bail {
				logInfoRequest(req.HostCallbackID, "responses stream provider=%s hit %d consecutive key-level rate limits, moving to the next provider for this request only",
					cand.provider, keyRateLimitStreak[providerName])
				unsupportedProviders[providerName] = true
			}
			continue
		case responsesStreamPreflightFailed:
			logInfoRequest(req.HostCallbackID, "responses stream failed provider=%s model=%s: %s", cand.provider, cand.model, logBodyExcerpt(preflight.reason))
			hostHTTPStreamClose(stream.StreamID)
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, stream.StatusCode, preflight.reason, cand.provider, cand.model))
			continue
		case responsesStreamPreflightErrorPayload:
			// Modelscope 会把部分拒绝以 HTTP 200 加一段裸错误文档返回，而不是 SSE
			// 流。若把它当作已完成的流转发出，交给客户端的就是一个不是 Response 资
			// 源的响应体。
			logInfoRequest(req.HostCallbackID, "responses stream provider=%s model=%s returned an error payload with HTTP 200: %s", cand.provider, cand.model, logBodyExcerpt(preflight.reason))
			hostHTTPStreamClose(stream.StreamID)
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, stream.StatusCode, preflight.reason, cand.provider, cand.model))
			continue
		}
		logDebugRequest(req.HostCallbackID, "responses stream served provider=%s mode=passthrough model=%s", cand.provider, cand.model)
		go forwardResponsesStream(req, cand, stream.StreamID, pluginapi.ExecutorStreamChunk{Payload: preflight.payload}, preflight.done)
		return pluginapi.ExecutorStreamResponse{Headers: headerMapToHTTP(stream.Headers)}, nil
	}

	// 所有托管 provider 已耗尽。交回宿主的内置流式路径，让非托管 provider 用
	// chat 协议服务本次请求。
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
			// 流到结束都没产出任何 SSE data 事件：累积到的是一段普通 JSON 响应体，
			// 因此要把它当作错误文档检查一遍。
			if modelscope.IsResponsesErrorPayload(string(payload)) {
				return responsesStreamPreflightResult{
					status: responsesStreamPreflightErrorPayload,
					done:   true,
					reason: string(payload),
				}, nil
			}
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

// sliceCompleteSSELines 把 raw 接到 pending 之后，返回可立即交给宿主的"完整行前缀"
// （以 \n 结尾，可能含多条完整 SSE 行）和需要留到下一段的不完整尾。宿主对插件发出
// 的每一块按 \n 切行解析 usage、无跨块续行，所以跨块的不完整行必须由调用方暂存，等下
// 一段补全后再发出。final=true 表示流已结束：把全部字节（含末尾无 \n 的最后一行）一
// 次性吐出，宿主按 \n 切时仍能把最后一段当作完整行解析。emit 段被克隆，避免与后续
// append 复用同一底层数组。
func sliceCompleteSSELines(pending, raw []byte, final bool) (emit, rest []byte) {
	b := append(pending, raw...)
	if final {
		return append([]byte(nil), b...), nil
	}
	idx := bytes.LastIndexByte(b, '\n')
	if idx < 0 {
		return nil, b
	}
	return append([]byte(nil), b[:idx+1]...), b[idx+1:]
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

	// SSE 行对齐缓冲：上游按 32KB 原始字节读回，一个 data: 事件可能被切到两块的
	// 边界上；宿主对插件发出的每一块独立按 \n 切行解析 usage、无跨块续行，半行 JSON
	// 解析失败会让 response.completed 里的 usage 整个丢掉。sliceCompleteSSELines 只把
	// 完整 \n 结尾的前缀交给宿主，跨块的不完整行留到下一段；收尾（done / 读错 /
	// chunk.Err）按 final 决定是否吐剩余字节。
	var pending []byte
	flush := func(raw []byte, final bool) bool {
		segment, rest := sliceCompleteSSELines(pending, raw, final)
		pending = rest
		if len(segment) == 0 {
			return true
		}
		// 事件是单行 data: JSON，行对齐后的整段必含完整事件，比旧实现按原始 32KB
		// 块逐段判定更准，也不会漏判跨块的事件。
		if modelscope.IsResponsesRateLimitExceeded(string(segment)) {
			store.ApplyResponsesRateLimit(cand.authID, "openai-compatible-"+cand.provider, req.Model, string(segment), store.Now())
			responsesFailed = true
			failedBody = string(segment)
		} else if modelscope.IsResponsesFailed(string(segment)) {
			responsesFailed = true
			failedBody = string(segment)
		}
		if !emit(segment, "") {
			return false
		}
		return true
	}

	if first.Payload != nil || first.Err != nil {
		if first.Err != nil {
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, first.Err.Error(), cand.provider, cand.model))
			_ = hostStreamEmit(req.StreamID, nil, first.Err.Error())
			hostStreamClose(req.StreamID, first.Err.Error())
			return
		}
		if !flush(first.Payload, done) {
			hostStreamClose(req.StreamID, "host stream emit failed")
			return
		}
	}
	if done {
		// 首个数据块已由 executeResponsesPassthroughStream 预筛过：response.failed 不会走到
		// 这里。flush(final=true) 已把末尾无 \n 的行也吐出。
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
	for {
		chunk, isDone, errRead := readHostStreamChunk(upstreamStreamID)
		if errRead != nil {
			_ = flush(nil, false)
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, errRead.Error(), cand.provider, cand.model))
			_ = hostStreamEmit(req.StreamID, nil, errRead.Error())
			hostStreamClose(req.StreamID, errRead.Error())
			return
		}
		if chunk.Err != nil {
			_ = flush(nil, false)
			store.OnUsage(usageRecordForFailure(cand.authID, req.ExecutorRequest, 0, chunk.Err.Error(), cand.provider, cand.model))
			_ = hostStreamEmit(req.StreamID, nil, chunk.Err.Error())
			hostStreamClose(req.StreamID, chunk.Err.Error())
			return
		}
		if !flush(chunk.Payload, isDone) {
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

// hostModelExecute 把非流式请求交回宿主内置的 provider 路径处理。
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

// hostModelExecuteStream 把流式请求交回宿主内置的 provider 路径处理。
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

// forwardHostModelStream 把宿主托管的模型流转发到面向客户端的流通道上。
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
