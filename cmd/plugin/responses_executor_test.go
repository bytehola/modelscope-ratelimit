package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	modelscope "modelscope-ratelimit/internal/ratelimit"
)

func setTestPluginConfig(providers ...string) {
	pluginCfg = &modelscope.Config{
		Providers:                 providers,
		InsufficientQuotaCooldown: 0,
	}
	store.Reconfigure(pluginCfg)
}

func setTestResolved() {
	routes := map[string]map[string]string{"modelscope": {"glm-5.2": "ZhipuAI/GLM-5.2"}}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		masked:        map[string]string{"auth-1": "ms-key-AAAA"},
		raw:           map[string]string{"auth-1": "ms-key-AAAA"},
		baseURL:       map[string]string{"auth-1": "https://api-inference.modelscope.cn/v1"},
		routeToName:   routes,
		nameToRoute:   withRouteIndex(routes),
		providerOrder: map[string][]string{"modelscope": {"auth-1"}},
		at:            time.Now(),
	}
	resolvedMu.Unlock()
}

// withRouteIndex 构造 resolveKeys 会和 routeToName 一起建的那份“上游名到路由 ID”反查索引，
// 这样测试夹具与胶水层从管理接口算出来的结果保持一致（路由 ID 有 alias 用 alias，否则用 name）。
func withRouteIndex(routes map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(routes))
	for provider, models := range routes {
		toRoute := make(map[string]string, len(models)*2)
		for routeID, upstream := range models {
			toRoute[strings.ToLower(upstream)] = routeID
			toRoute[strings.ToLower(routeID)] = routeID
		}
		out[provider] = toRoute
	}
	return out
}

func TestRouteResponsesRequestChatNotHandled(t *testing.T) {
	setTestPluginConfig("modelscope")
	resp := routeResponsesRequest(pluginapiModelRouteRequest("openai", "glm-5.2"))
	if resp.Handled {
		t.Fatal("chat completions must not be handled by responses executor")
	}
}

func TestRouteResponsesRequestDisabledByDefault(t *testing.T) {
	setTestPluginConfig("modelscope")
	resp := routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "glm-5.2"))
	if resp.Handled {
		t.Fatal("responses request must not be handled when use_responses is false")
	}
}

func TestRouteResponsesRequestResponsesHandled(t *testing.T) {
	setTestPluginConfig("modelscope")
	pluginCfg.UseResponses = []string{"*"}
	resp := routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "glm-5.2"))
	if !resp.Handled || resp.TargetKind != pluginapiModelRouteTargetSelf {
		t.Fatalf("responses request must route to self, got %+v", resp)
	}
	pluginCfg.UseResponses = nil
}

func TestRouteResponsesRequestCodexHandled(t *testing.T) {
	setTestPluginConfig("modelscope")
	pluginCfg.UseResponses = []string{"*"}
	resp := routeResponsesRequest(pluginapiModelRouteRequest("codex", "gpt-5"))
	if !resp.Handled || resp.TargetKind != pluginapiModelRouteTargetSelf {
		t.Fatalf("codex responses request must route to self, got %+v", resp)
	}
	pluginCfg.UseResponses = nil
}

func TestRouteResponsesRequestMatchesUpstreamModelNotAlias(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	pluginCfg.UseResponses = []string{"ZhipuAI/GLM-5.2"}

	resp := routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "glm-5.2"))
	if !resp.Handled || resp.TargetKind != pluginapiModelRouteTargetSelf {
		t.Fatalf("configured upstream responses model must route by alias, got %+v", resp)
	}

	pluginCfg.UseResponses = []string{"glm-5.2"}
	resp = routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "glm-5.2"))
	if resp.Handled {
		t.Fatal("responses alias must not match once an upstream model mapping exists")
	}

	pluginCfg.UseResponses = []string{"ZhipuAI/GLM-5.2"}
	resp = routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "qwen-72b"))
	if resp.Handled {
		t.Fatal("unconfigured upstream responses model must not be handled")
	}

	pluginCfg.UseResponses = nil
}

func TestResponsesUpstreamRequestSetsAuthAndURL(t *testing.T) {
	clientHeaders := http.Header{
		"X-Test":     []string{"v"},
		"User-Agent": []string{"client-agent/1.0"},
	}
	req := responsesUpstreamRequest(clientHeaders, "sk-secret", "https://api.example.com/v1/responses", []byte(`{}`))
	if req.URL != "https://api.example.com/v1/responses" {
		t.Fatalf("URL = %q", req.URL)
	}
	if got := req.Headers.Get("Authorization"); got != "Bearer sk-secret" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := req.Headers.Get("X-Test"); got != "v" {
		t.Fatalf("X-Test not forwarded: %q", got)
	}
	if got := req.Headers.Get("User-Agent"); got != "client-agent/1.0" {
		t.Fatalf("User-Agent = %q, want client-agent/1.0", got)
	}
	if got := req.Headers.Values("User-Agent"); len(got) != 1 || got[0] != "client-agent/1.0" {
		t.Fatalf("User-Agent values = %v, want only client-agent/1.0", got)
	}
	if got := clientHeaders.Get("User-Agent"); got != "client-agent/1.0" {
		t.Fatalf("caller headers mutated: User-Agent = %q", got)
	}
}

func TestHeaderMapToHTTP(t *testing.T) {
	h := headerMapToHTTP(map[string][]string{"Content-Type": {"application/json"}})
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if headerMapToHTTP(nil) == nil {
		t.Fatal("nil map must return non-nil header")
	}
}

func TestWithModelReplacesModelField(t *testing.T) {
	out := withModel([]byte(`{"model":"glm-5.2","input":"hi"}`), "ZhipuAI/GLM-5.2")
	if !bytes.Contains(out, []byte(`"ZhipuAI/GLM-5.2"`)) || bytes.Contains(out, []byte(`"glm-5.2"`)) {
		t.Fatalf("model field not replaced: %s", out)
	}
}

func TestUpstreamModelResolvesAlias(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolved()
	req := pluginapiExecutorRequest("glm-5.2")
	if got := upstreamModelForAuth("auth-1", req); got != "ZhipuAI/GLM-5.2" {
		t.Fatalf("upstreamModelForAuth = %q, want ZhipuAI/GLM-5.2", got)
	}
}

func setTestResolvedMulti() {
	routes := map[string]map[string]string{
		"modelscope":    {"glm-5.2": "ZhipuAI/GLM-5.2"},
		"modelscope-ai": {"glm-5.2": "zai-org/GLM-5.2"},
	}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		masked: map[string]string{"auth-1": "ms-key-AAAA", "auth-2": "ms-key-BBBB"},
		raw:    map[string]string{"auth-1": "ms-key-AAAA", "auth-2": "ms-key-BBBB"},
		baseURL: map[string]string{
			"auth-1": "https://api-inference.modelscope.cn/v1",
			"auth-2": "https://api-inference.modelscope.cn/v1",
		},
		routeToName: routes,
		nameToRoute: withRouteIndex(routes),
		providerOrder: map[string][]string{
			"modelscope":    {"auth-1"},
			"modelscope-ai": {"auth-2"},
		},
		at: time.Now(),
	}
	resolvedMu.Unlock()
}

func TestUpstreamModelRespectsProviderAlias(t *testing.T) {
	setTestPluginConfig("modelscope", "modelscope-ai")
	setTestResolvedMulti()
	req := pluginapiExecutorRequest("glm-5.2")
	if got := upstreamModelForAuth("auth-1", req); got != "ZhipuAI/GLM-5.2" {
		t.Fatalf("modelscope upstream model = %q", got)
	}
	if got := upstreamModelForAuth("auth-2", req); got != "zai-org/GLM-5.2" {
		t.Fatalf("modelscope-ai upstream model = %q", got)
	}
}

func TestManagedCandidatesFollowsProviderOrder(t *testing.T) {
	setTestPluginConfig("modelscope", "modelscope-ai")
	setTestResolvedMulti()
	cands := managedCandidates(getResolved())
	if len(cands) != 2 || cands[0].ID != "auth-1" || cands[1].ID != "auth-2" {
		t.Fatalf("candidates do not follow configured provider order: %+v", cands)
	}
}

func TestManagedCandidatesBuildsFromResolved(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolved()
	cands := managedCandidates(getResolved())
	if len(cands) != 1 || cands[0].ID != "auth-1" || cands[0].Provider != "openai-compatible-modelscope" {
		t.Fatalf("unexpected candidates: %+v", cands)
	}
}

func TestBuildRetrySequenceOnlyManaged(t *testing.T) {
	setTestPluginConfig("modelscope", "modelscope-ai")
	routes := map[string]map[string]string{
		"modelscope":    {"m": "up/m"},
		"modelscope-ai": {"m": "up/ma"},
	}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		raw: map[string]string{"auth-1": "k1", "auth-2": "k2", "auth-3": "k3"},
		baseURL: map[string]string{
			"auth-1": "https://a/v1", "auth-2": "https://b/v1", "auth-3": "https://c/v1",
		},
		routeToName: routes,
		nameToRoute: withRouteIndex(routes),
		providerOrder: map[string][]string{
			"modelscope":    {"auth-1", "auth-2"},
			"modelscope-ai": {"auth-3"},
		},
		at: time.Now(),
	}
	resolvedMu.Unlock()

	cfg := currentConfig()

	// 两种模式都按 provider 顺序一把凭据一步：key 级失败必须先在本家内轮换，才允许换到下一家。
	want := []string{"modelscope", "modelscope", "modelscope-ai"}
	for _, skip := range []bool{true, false} {
		cfg.SkipProviderOn429 = skip
		steps := buildRetrySequence(getResolved(), cfg)
		got := make([]string, 0, len(steps))
		for _, st := range steps {
			got = append(got, st.provider)
		}
		if len(got) != len(want) {
			t.Fatalf("skip=%v: want %d steps %v, got %v", skip, len(want), want, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("skip=%v: step order = %v, want %v", skip, got, want)
			}
		}
	}

}

func TestProviderNameForAuth(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolved()
	if got := providerNameForAuth("auth-1"); got != "modelscope" {
		t.Fatalf("providerNameForAuth = %q, want modelscope", got)
	}
}

func TestPickResponsesKeyReturnsCredential(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolved()
	req := pluginapiExecutorRequest("glm-5.2")
	authID, apiKey, baseURL, err := pickResponsesKey(req)
	if err != nil {
		t.Fatalf("pickResponsesKey error: %v", err)
	}
	if authID != "auth-1" || apiKey != "ms-key-AAAA" || baseURL != "https://api-inference.modelscope.cn/v1" {
		t.Fatalf("unexpected credential: %q %q %q", authID, apiKey, baseURL)
	}
}

func pluginapiModelRouteRequest(sourceFormat, model string) pluginapi.ModelRouteRequest {
	return pluginapi.ModelRouteRequest{SourceFormat: sourceFormat, RequestedModel: model}
}

func pluginapiExecutorRequest(model string) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{Model: model}
}

var pluginapiModelRouteTargetSelf = pluginapi.ModelRouteTargetSelf

// ---- resolveStep 用例 ----

func TestResolveStepReturnsFirstKey(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolved()
	cand, ok := resolveStep(retryStep{provider: "modelscope"}, getResolved(), "glm-5.2")
	if !ok {
		t.Fatal("resolveStep: expected ok")
	}
	if cand.authID != "auth-1" || cand.apiKey != "ms-key-AAAA" {
		t.Fatalf("resolveStep: wrong credential: %+v", cand)
	}
	if cand.model != "ZhipuAI/GLM-5.2" {
		t.Fatalf("resolveStep: model alias not resolved: %q", cand.model)
	}
	if cand.baseURL != "https://api-inference.modelscope.cn/v1" {
		t.Fatalf("resolveStep: baseURL wrong: %q", cand.baseURL)
	}
}

func TestResolveStepRotatesKeys(t *testing.T) {
	setTestPluginConfig("modelscope")
	routes := map[string]map[string]string{"modelscope": {"m": "up/m"}}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		raw:           map[string]string{"a1": "k1", "a2": "k2"},
		baseURL:       map[string]string{"a1": "https://x/v1", "a2": "https://x/v1"},
		routeToName:   routes,
		nameToRoute:   withRouteIndex(routes),
		providerOrder: map[string][]string{"modelscope": {"a1", "a2"}},
		at:            time.Now(),
	}
	resolvedMu.Unlock()

	c1, ok1 := resolveStep(retryStep{provider: "modelscope"}, getResolved(), "m")
	c2, ok2 := resolveStep(retryStep{provider: "modelscope"}, getResolved(), "m")
	if !ok1 || !ok2 {
		t.Fatal("resolveStep: expected ok")
	}
	if c1.authID == c2.authID {
		t.Fatalf("resolveStep: key did not rotate: %s == %s", c1.authID, c2.authID)
	}
}

func TestResolveStepUnknownProvider(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolved()
	if _, ok := resolveStep(retryStep{provider: "nonexistent"}, getResolved(), "glm-5.2"); ok {
		t.Fatal("resolveStep: expected false for unknown provider")
	}
}

// ---- routeResponsesRequest 边界情况 ----

func TestRouteResponsesRequestEmptyProviders(t *testing.T) {
	setTestPluginConfig()
	pluginCfg.UseResponses = []string{"*"}
	resp := routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "glm-5.2"))
	if resp.Handled {
		t.Fatal("responses request must not be handled when providers is empty")
	}
	pluginCfg.UseResponses = nil
}

// ---- 执行器让位逻辑（打桩 I/O）----

func setTestResolvedExec() {
	routes := map[string]map[string]string{"modelscope": {"glm-5.2": "ZhipuAI/GLM-5.2"}}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		raw:           map[string]string{"auth-1": "key1", "auth-2": "key2"},
		baseURL:       map[string]string{"auth-1": "https://up/v1", "auth-2": "https://up/v1"},
		routeToName:   routes,
		nameToRoute:   withRouteIndex(routes),
		providerOrder: map[string][]string{"modelscope": {"auth-1", "auth-2"}},
		at:            time.Now(),
	}
	resolvedMu.Unlock()
	if pluginCfg != nil {
		pluginCfg.UseResponses = []string{"ZhipuAI/GLM-5.2"}
	}
}

func setTestResolvedMixedResponses() {
	// 对应真实部署：A 家把 zai-org/GLM-5.2 注册在别名 glm-5.2 下（所以 zai-org/GLM-5.2 不是宿主可路由
	// 的模型 ID），同时按原名提供 DeepSeek 那个模型；B 家把 deepseek-ai/DeepSeek-V4-Pro-0813 注册
	// 在同一个别名 glm-5.2 下。
	routes := map[string]map[string]string{
		"modelscope-ai-deepseek2": {
			"glm-5.2":                          "zai-org/GLM-5.2",
			"deepseek-ai/DeepSeek-V4-Pro-0813": "deepseek-ai/DeepSeek-V4-Pro-0813",
		},
		"modelscope-my": {
			"glm-5.2":            "deepseek-ai/DeepSeek-V4-Pro-0813",
			"ZhipuAI/GLM-5.2":    "ZhipuAI/GLM-5.2",
			"Qwen3.8-Flash-Next": "Qwen/Qwen3.8-Flash-Next",
		},
	}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		raw:         map[string]string{"auth-glm": "key-glm", "auth-ds": "key-ds"},
		baseURL:     map[string]string{"auth-glm": "https://glm.example/v1", "auth-ds": "https://ds.example/v1"},
		routeToName: routes,
		nameToRoute: withRouteIndex(routes),
		providerOrder: map[string][]string{
			"modelscope-ai-deepseek2": {"auth-glm"},
			"modelscope-my":           {"auth-ds"},
		},
		at: time.Now(),
	}
	resolvedMu.Unlock()
}

func TestRouteResponsesRequestMixedProvidersUsesUpstreamModel(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my", "modelscope-ai")
	setTestResolvedMixedResponses()
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}

	resp := routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "glm-5.2"))
	if !resp.Handled || resp.TargetKind != pluginapiModelRouteTargetSelf {
		t.Fatalf("glm alias with one Responses-enabled provider must route to self, got %+v", resp)
	}

	resp = routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "zai-org/GLM-5.2"))
	if resp.Handled {
		t.Fatal("zai upstream model must not route to Responses when only DeepSeek is enabled")
	}

	pluginCfg.UseResponses = nil
}

func executorTestRequest(stream bool) executorRequestWithCallback {
	return executorRequestWithCallback{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "glm-5.2",
			SourceFormat:    "openai-response",
			Stream:          stream,
			OriginalRequest: []byte(`{"model":"glm-5.2","input":"hi"}`),
			Headers:         http.Header{},
		},
		HostCallbackID: "test-cb",
		StreamID:       "test-stream",
	}
}

func TestExecuteResponsesPassthroughHostFallbackSuccess(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	httpCalls := 0
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		httpCalls++
		return hostHTTPResponse{StatusCode: 429, Body: []byte(`{"error":"rate_limit"}`)}, nil
	}
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"ok":true}`)}, nil
	}

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("unexpected payload: %s", string(resp.Payload))
	}
	if httpCalls == 0 {
		t.Fatal("managed loop was not tried before host fallback")
	}
}

func TestExecuteResponsesPassthroughHostFallbackError(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		return hostHTTPResponse{StatusCode: 429, Body: []byte(`{"error":"rate_limit"}`)}, nil
	}
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, errors.New("host unavailable")
	}

	_, err := executeResponsesPassthrough(executorTestRequest(false))
	if err == nil {
		t.Fatal("expected error when host fallback fails")
	}
	if !strings.Contains(err.Error(), "host fallback failed") {
		t.Fatalf("error should mention host fallback: %v", err)
	}
}

func TestExecuteResponsesPassthroughMixedProvidersPrefersProviderOrderChatFallback(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	var gotURLs []string
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		gotURLs = append(gotURLs, req.URL)
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-ds"}`)}, nil
	}
	hostFallbackModels := make([]string, 0)
	hostModelExecute = func(req executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		hostFallbackModels = append(hostFallbackModels, req.Model)
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"provider":"zai-chat"}`)}, nil
	}

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"provider":"zai-chat"}` {
		t.Fatalf("unexpected payload: %s", string(resp.Payload))
	}
	// 宿主注册表对于配了别名的兼容模型只认别名，所以 chat 兜底必须带路由 ID（glm-5.2）而不是上游模型名
	// （zai-org/GLM-5.2），后者 host.model.execute 根本解析不出来。
	if len(hostFallbackModels) != 1 || hostFallbackModels[0] != "glm-5.2" {
		t.Fatalf("first provider host fallback must use the host-routable model ID, got %v", hostFallbackModels)
	}
	if len(gotURLs) != 0 {
		t.Fatalf("DeepSeek Responses must not be called after first provider succeeds, got %v", gotURLs)
	}
}

func TestExecuteResponsesPassthroughMixedProvidersFallsBackToResponses(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	var gotURLs []string
	var gotBodies [][]byte
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		gotURLs = append(gotURLs, req.URL)
		gotBodies = append(gotBodies, append([]byte(nil), req.Body...))
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-ds"}`)}, nil
	}
	hostFallbackModels := make([]string, 0)
	hostModelExecute = func(req executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		hostFallbackModels = append(hostFallbackModels, req.Model)
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusTooManyRequests, Body: []byte(`{"error":"rate_limit"}`)}, nil
	}

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"id":"resp-ds"}` {
		t.Fatalf("unexpected payload: %s", string(resp.Payload))
	}
	if len(hostFallbackModels) != 1 || hostFallbackModels[0] != "glm-5.2" {
		t.Fatalf("first provider host fallback must be attempted with the host-routable model ID, got %v", hostFallbackModels)
	}
	if len(gotURLs) != 1 {
		t.Fatalf("upstream calls = %d, want 1 DeepSeek Responses attempt", len(gotURLs))
	}
	if gotURLs[0] != "https://ds.example/v1/responses" {
		t.Fatalf("upstream URL = %q, want DeepSeek provider", gotURLs[0])
	}
	if !strings.Contains(string(gotBodies[0]), `"model":"deepseek-ai/DeepSeek-V4-Pro-0813"`) {
		t.Fatalf("upstream body must use enabled DeepSeek model: %s", string(gotBodies[0]))
	}
}

func TestExecuteResponsesPassthroughManagedSuccessNoFallback(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	modelCalled := false
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-1"}`)}, nil
	}
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		modelCalled = true
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"ok":true}`)}, nil
	}

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"id":"resp-1"}` {
		t.Fatalf("unexpected payload: %s", string(resp.Payload))
	}
	if modelCalled {
		t.Fatal("host fallback must not be called when managed provider succeeds")
	}
}

func TestExecuteResponsesPassthroughNonRetryableErrorNoFallback(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	modelCalled := false
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		return hostHTTPResponse{StatusCode: 400, Body: []byte(`{"error":"bad_request"}`)}, nil
	}
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		modelCalled = true
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{}`)}, nil
	}

	_, err := executeResponsesPassthrough(executorTestRequest(false))
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("error should mention HTTP 400: %v", err)
	}
	if modelCalled {
		t.Fatal("host fallback must not be called on non-retryable managed error")
	}
}

func TestExecuteResponsesPassthrough405SkipsProviderWithoutRotatingKeys(t *testing.T) {
	setTestPluginConfig("modelscope")
	pluginCfg.SkipProviderOn429 = true
	setTestResolvedExec()
	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	httpCalls := 0
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		httpCalls++
		return hostHTTPResponse{StatusCode: http.StatusMethodNotAllowed, Body: []byte(`{"error":"method_not_allowed"}`)}, nil
	}
	modelCalled := false
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		modelCalled = true
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"fallback":true}`)}, nil
	}

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !modelCalled {
		t.Fatal("405 must exhaust responses-only provider and fall back to host chat path")
	}
	if httpCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1; provider must not rotate to another key", httpCalls)
	}
	if string(resp.Payload) != `{"fallback":true}` {
		t.Fatalf("unexpected fallback payload: %s", string(resp.Payload))
	}
}

func TestExecuteResponsesPassthrough405RetriesKeysWhenSkipProviderDisabled(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	httpCalls := 0
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		httpCalls++
		return hostHTTPResponse{StatusCode: http.StatusMethodNotAllowed, Body: []byte(`{"error":"method_not_allowed"}`)}, nil
	}
	modelCalled := false
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		modelCalled = true
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"fallback":true}`)}, nil
	}

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpCalls != 2 {
		t.Fatalf("upstream calls = %d, want 2 with skip_provider_on_429 disabled", httpCalls)
	}
	if !modelCalled {
		t.Fatal("host fallback must be called after every key retried")
	}
	if string(resp.Payload) != `{"fallback":true}` {
		t.Fatalf("unexpected fallback payload: %s", string(resp.Payload))
	}
}

func TestExecuteResponsesPassthroughRateLimitResponseTriggersFallback(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	rlBody := []byte(`{"type":"response.failed","error":{"code":"rate_limit_exceeded"}}`)
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		return hostHTTPResponse{StatusCode: 200, Body: rlBody}, nil
	}
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"ok":true}`)}, nil
	}

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("expected host fallback payload, got: %s", string(resp.Payload))
	}
}

func TestExecuteResponsesPassthroughInvalidParameterTriggersFallback(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	invalidBody := []byte(`{"response":{"status":"failed","error":{"code":"InvalidParameter","message":"<400> InternalError.Algo.InvalidParameter"}},"type":"response.failed"}`)
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		return hostHTTPResponse{StatusCode: 200, Body: invalidBody}, nil
	}
	modelCalled := false
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		modelCalled = true
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"ok":true}`)}, nil
	}

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("expected host fallback payload, got: %s", string(resp.Payload))
	}
	if !modelCalled {
		t.Fatal("response.failed InvalidParameter must exhaust managed providers and call host fallback")
	}
}

func TestExecuteResponsesPassthroughStreamHostFallback(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	t.Cleanup(func() { hostHTTPDoStream = origStream; hostModelExecuteStream = origModelStream })

	streamTried := false
	hostHTTPDoStream = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		streamTried = true
		return hostHTTPStreamResponse{StatusCode: 429}, nil
	}
	origModelRead := hostModelStreamRead
	t.Cleanup(func() { hostModelStreamRead = origModelRead })
	hostModelStreamRead = func(streamID string) (pluginapi.HostModelStreamReadResponse, error) {
		return pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: ok`), Done: true}, nil
	}
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "host-stream-1"}, nil
	}

	_, err := executeResponsesPassthroughStream(executorTestRequest(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !streamTried {
		t.Fatal("managed stream loop was not tried before host fallback")
	}
}

func TestExecuteResponsesPassthroughStreamMixedProvidersOnlyUsesEnabledUpstream(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()

	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	origRead := hostHTTPStreamRead
	t.Cleanup(func() {
		hostHTTPDoStream = origStream
		hostModelExecuteStream = origModelStream
		hostHTTPStreamRead = origRead
	})

	var gotURLs []string
	var gotBodies [][]byte
	hostHTTPDoStream = func(req pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		gotURLs = append(gotURLs, req.URL)
		gotBodies = append(gotBodies, append([]byte(nil), req.Body...))
		return hostHTTPStreamResponse{StatusCode: 429}, nil
	}
	hostHTTPStreamRead = func(string) (hostStreamReadResponse, error) {
		return hostStreamReadResponse{Payload: []byte(`{"error":"rate_limit"}`), Done: true}, nil
	}
	hostFallbackModels := make([]string, 0)
	hostModelExecuteStream = func(req executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		hostFallbackModels = append(hostFallbackModels, req.Model)
		return pluginapi.HostModelStreamResponse{StatusCode: 500}, nil
	}

	_, err := executeResponsesPassthroughStream(executorTestRequest(true))
	if err == nil {
		t.Fatal("expected host fallback stream error after DeepSeek stream candidate fails")
	}
	if len(gotURLs) != 1 {
		t.Fatalf("upstream stream calls = %d, want 1", len(gotURLs))
	}
	// 第一次调用：A 家的 chat 兜底，带宿主可路由的 ID。第二次调用：托管全部耗尽后的兜底，保留客户端原名。
	if len(hostFallbackModels) != 2 || hostFallbackModels[0] != "glm-5.2" || hostFallbackModels[1] != "glm-5.2" {
		t.Fatalf("first provider stream fallback must use the host-routable model ID, got %v", hostFallbackModels)
	}
	if gotURLs[0] != "https://ds.example/v1/responses" {
		t.Fatalf("upstream URL = %q, want DeepSeek provider", gotURLs[0])
	}
	if !strings.Contains(string(gotBodies[0]), `"model":"deepseek-ai/DeepSeek-V4-Pro-0813"`) {
		t.Fatalf("upstream stream body must use enabled DeepSeek model: %s", string(gotBodies[0]))
	}
}

func TestExecuteResponsesPassthroughStreamMixedProvidersPrefersProviderOrder(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()

	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	origRead := hostModelStreamRead
	t.Cleanup(func() {
		hostHTTPDoStream = origStream
		hostModelExecuteStream = origModelStream
		hostModelStreamRead = origRead
	})

	directCalls := 0
	hostHTTPDoStream = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		directCalls++
		return hostHTTPStreamResponse{StatusCode: 200}, nil
	}
	hostModuleRead := func(string) (pluginapi.HostModelStreamReadResponse, error) {
		return pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: zai`), Done: true}, nil
	}
	hostModelStreamRead = hostModuleRead
	hostFallbackModels := make([]string, 0)
	hostModelExecuteStream = func(req executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		hostFallbackModels = append(hostFallbackModels, req.Model)
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "zai-host-stream"}, nil
	}

	_, err := executeResponsesPassthroughStream(executorTestRequest(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hostFallbackModels) != 1 || hostFallbackModels[0] != "glm-5.2" {
		t.Fatalf("first provider stream fallback must use the host-routable model ID, got %v", hostFallbackModels)
	}
	if directCalls != 0 {
		t.Fatalf("DeepSeek stream must not be called after first provider succeeds, calls=%d", directCalls)
	}
}

func TestExecuteResponsesPassthroughStream405SkipsProviderWithoutRotatingKeys(t *testing.T) {
	setTestPluginConfig("modelscope")
	pluginCfg.SkipProviderOn429 = true
	setTestResolvedExec()
	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	t.Cleanup(func() { hostHTTPDoStream = origStream; hostModelExecuteStream = origModelStream })

	streamCalls := 0
	hostHTTPDoStream = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		streamCalls++
		return hostHTTPStreamResponse{StatusCode: http.StatusMethodNotAllowed}, nil
	}
	origRead := hostHTTPStreamRead
	t.Cleanup(func() { hostHTTPStreamRead = origRead })
	hostHTTPStreamRead = func(string) (hostStreamReadResponse, error) {
		return hostStreamReadResponse{Payload: []byte(`{"error":"method_not_allowed"}`), Done: true}, nil
	}

	fallbackCalled := false
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		fallbackCalled = true
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "host-1"}, nil
	}
	origModelRead := hostModelStreamRead
	t.Cleanup(func() { hostModelStreamRead = origModelRead })
	hostModelStreamRead = func(string) (pluginapi.HostModelStreamReadResponse, error) {
		return pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: ok`), Done: true}, nil
	}

	_, err := executeResponsesPassthroughStream(executorTestRequest(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fallbackCalled {
		t.Fatal("405 stream must exhaust responses-only provider and fall back to host chat stream")
	}
	if streamCalls != 1 {
		t.Fatalf("upstream stream calls = %d, want 1; provider must not rotate to another key", streamCalls)
	}
}

func TestExecuteResponsesPassthroughStream405RetriesKeysWhenSkipProviderDisabled(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	t.Cleanup(func() { hostHTTPDoStream = origStream; hostModelExecuteStream = origModelStream })

	streamCalls := 0
	hostHTTPDoStream = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		streamCalls++
		return hostHTTPStreamResponse{StatusCode: http.StatusMethodNotAllowed}, nil
	}
	origRead := hostHTTPStreamRead
	t.Cleanup(func() { hostHTTPStreamRead = origRead })
	hostHTTPStreamRead = func(string) (hostStreamReadResponse, error) {
		return hostStreamReadResponse{Payload: []byte(`{"error":"method_not_allowed"}`), Done: true}, nil
	}

	fallbackCalled := false
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		fallbackCalled = true
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "host-1"}, nil
	}
	origModelRead := hostModelStreamRead
	t.Cleanup(func() { hostModelStreamRead = origModelRead })
	hostModelStreamRead = func(string) (pluginapi.HostModelStreamReadResponse, error) {
		return pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: ok`), Done: true}, nil
	}

	_, err := executeResponsesPassthroughStream(executorTestRequest(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if streamCalls != 2 {
		t.Fatalf("upstream stream calls = %d, want 2 with skip_provider_on_429 disabled", streamCalls)
	}
	if !fallbackCalled {
		t.Fatal("host stream fallback must be called after every key retried")
	}
}

func TestExecuteResponsesPassthroughStreamInvalidParameterTriggersFallback(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	t.Cleanup(func() { hostHTTPDoStream = origStream; hostModelExecuteStream = origModelStream })

	invalidChunk := []byte(`data: {"response":{"status":"failed","error":{"code":"InvalidParameter"}},"type":"response.failed"}`)
	streamTried := false
	streamCalls := 0
	hostHTTPDoStream = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		streamTried = true
		streamCalls++
		id := "up-1"
		if streamCalls > 1 {
			id = "up-2"
		}
		return hostHTTPStreamResponse{StatusCode: 200, StreamID: id}, nil
	}
	origRead := hostHTTPStreamRead
	t.Cleanup(func() { hostHTTPStreamRead = origRead })
	hostHTTPStreamRead = func(streamID string) (hostStreamReadResponse, error) {
		return hostStreamReadResponse{Payload: invalidChunk, Done: true}, nil
	}
	fallbackCalled := false
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		fallbackCalled = true
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "host-1"}, nil
	}
	origModelRead := hostModelStreamRead
	t.Cleanup(func() { hostModelStreamRead = origModelRead })
	hostModelStreamRead = func(streamID string) (pluginapi.HostModelStreamReadResponse, error) {
		return pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: ok`), Done: true}, nil
	}

	_, err := executeResponsesPassthroughStream(executorTestRequest(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !streamTried {
		t.Fatal("managed stream loop was not tried before host fallback")
	}
	if !fallbackCalled {
		t.Fatal("response.failed InvalidParameter in first chunk must trigger host fallback")
	}
}

func TestExecuteResponsesPassthroughStreamLeadingBlankChunksInvalidParameterTriggersFallback(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	t.Cleanup(func() { hostHTTPDoStream = origStream; hostModelExecuteStream = origModelStream })

	leadingBlanks := []byte("\n\n\n\n\n\n")
	invalidChunk := []byte("data: {\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"InvalidParameter\"}},\"type\":\"response.failed\"}\n\n")
	streamCalls := 0
	hostHTTPDoStream = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		streamCalls++
		return hostHTTPStreamResponse{StatusCode: 200, StreamID: "up-managed"}, nil
	}

	origRead := hostHTTPStreamRead
	t.Cleanup(func() { hostHTTPStreamRead = origRead })
	readCalls := 0
	hostHTTPStreamRead = func(streamID string) (hostStreamReadResponse, error) {
		readCalls++
		if readCalls%2 == 1 {
			return hostStreamReadResponse{Payload: leadingBlanks, Done: false}, nil
		}
		return hostStreamReadResponse{Payload: invalidChunk, Done: true}, nil
	}

	fallbackCalled := false
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		fallbackCalled = true
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "host-1"}, nil
	}
	origModelRead := hostModelStreamRead
	t.Cleanup(func() { hostModelStreamRead = origModelRead })
	hostModelStreamRead = func(streamID string) (pluginapi.HostModelStreamReadResponse, error) {
		return pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: ok`), Done: true}, nil
	}

	_, err := executeResponsesPassthroughStream(executorTestRequest(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if streamCalls != 2 {
		t.Fatalf("managed stream calls = %d, want 2 before host fallback", streamCalls)
	}
	if !fallbackCalled {
		t.Fatal("response.failed after leading blank chunks must exhaust managed providers and call host fallback")
	}
	if readCalls != 4 {
		t.Fatalf("hostHTTPStreamRead calls = %d, want 4 (blank+failed for each managed provider)", readCalls)
	}
}

func TestExecuteResponsesPassthroughStreamLeadingBlankChunksSuccess(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	t.Cleanup(func() { hostHTTPDoStream = origStream; hostModelExecuteStream = origModelStream })

	leadingBlanks := []byte("\n\n\n\n\n\n")
	successChunk := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	hostHTTPDoStream = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		return hostHTTPStreamResponse{StatusCode: 200, StreamID: "up-managed"}, nil
	}

	origRead := hostHTTPStreamRead
	t.Cleanup(func() { hostHTTPStreamRead = origRead })
	readCalls := 0
	hostHTTPStreamRead = func(streamID string) (hostStreamReadResponse, error) {
		readCalls++
		if readCalls == 1 {
			return hostStreamReadResponse{Payload: leadingBlanks, Done: false}, nil
		}
		return hostStreamReadResponse{Payload: successChunk, Done: true}, nil
	}

	fallbackCalled := false
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		fallbackCalled = true
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "host-1"}, nil
	}

	_, err := executeResponsesPassthroughStream(executorTestRequest(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if readCalls != 2 {
		t.Fatalf("hostHTTPStreamRead calls = %d, want 2", readCalls)
	}
	if fallbackCalled {
		t.Fatal("leading blank chunks followed by a successful event must not trigger host fallback")
	}
}

func TestExecuteResponsesPassthroughStreamHostFallbackError(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()
	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	t.Cleanup(func() { hostHTTPDoStream = origStream; hostModelExecuteStream = origModelStream })

	hostHTTPDoStream = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		return hostHTTPStreamResponse{StatusCode: 429}, nil
	}
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		return pluginapi.HostModelStreamResponse{}, errors.New("host stream unavailable")
	}

	_, err := executeResponsesPassthroughStream(executorTestRequest(true))
	if err == nil {
		t.Fatal("expected error when host stream fallback fails")
	}
	if !strings.Contains(err.Error(), "host fallback failed") {
		t.Fatalf("error should mention host fallback: %v", err)
	}
}

// 覆盖一种协议降级：provider 1 在 429 之后进了 skip 集，但它的 chat 兜底那一步仍然让宿主去执行这
// 个别名。宿主的调度器于是改用 provider 2 的凭据应答，结果 provider 2 明明开了 Responses，它的模型
// 却走了 /v1/chat/completions，而且属于它自己的原生那步根本没机会执行。
func TestExecuteResponsesPassthroughSkippedProviderKeepsNativeResponsesPath(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()

	store.SetSkipProvider("modelscope-ai-deepseek2")
	t.Cleanup(func() { store.ClearAllSkippedProviders() })

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	chatCalls := 0
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		chatCalls++
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"leaked":"chat"}`)}, nil
	}
	var gotURLs []string
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		gotURLs = append(gotURLs, req.URL)
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-ds"}`)}, nil
	}

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatCalls != 0 {
		t.Fatalf("a skipped provider must not run a chat fallback (the host would answer it with another provider's key), calls=%d", chatCalls)
	}
	if string(resp.Payload) != `{"id":"resp-ds"}` {
		t.Fatalf("payload = %s, want the native Responses result from the enabled provider", string(resp.Payload))
	}
	if len(gotURLs) != 1 || gotURLs[0] != "https://ds.example/v1/responses" {
		t.Fatalf("upstream calls = %v, want only the DeepSeek responses endpoint", gotURLs)
	}
}

// 同一个降级换由日禁用来触发（而不是 skip 集）时的版本。
func TestExecuteResponsesPassthroughDisabledProviderKeepsNativeResponsesPath(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()

	store.DisableGlobal("auth-glm", "glm-5.2", store.Now())

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	chatCalls := 0
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		chatCalls++
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"leaked":"chat"}`)}, nil
	}
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-ds"}`)}, nil
	}

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatCalls != 0 {
		t.Fatalf("a provider with no usable credential must not run a chat fallback, calls=%d", chatCalls)
	}
	if string(resp.Payload) != `{"id":"resp-ds"}` {
		t.Fatalf("payload = %s, want the native Responses result", string(resp.Payload))
	}
}

// 钉住托管 provider 那一步需要的两个模型标识：直连 /v1/responses 用上游模型名，宿主 chat 兜底用可路
// 由 ID（配了别名时就是别名）。
func TestResolveProviderModelSeparatesRouteAndUpstreamModels(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	setTestResolvedMixedResponses()
	resolved := getResolved()

	cases := []struct {
		name         string
		provider     string
		requested    string
		wantUpstream string
		wantRoute    string
	}{
		{
			name: "alias asks by alias", provider: "modelscope-ai-deepseek2", requested: "glm-5.2",
			wantUpstream: "zai-org/GLM-5.2", wantRoute: "glm-5.2",
		},
		{
			name: "alias asks by upstream name", provider: "modelscope-ai-deepseek2", requested: "zai-org/GLM-5.2",
			wantUpstream: "zai-org/GLM-5.2", wantRoute: "glm-5.2",
		},
		{
			name: "model without alias", provider: "modelscope-ai-deepseek2", requested: "deepseek-ai/DeepSeek-V4-Pro-0813",
			wantUpstream: "deepseek-ai/DeepSeek-V4-Pro-0813", wantRoute: "deepseek-ai/DeepSeek-V4-Pro-0813",
		},
		{
			name: "same alias resolves per provider", provider: "modelscope-my", requested: "glm-5.2",
			wantUpstream: "deepseek-ai/DeepSeek-V4-Pro-0813", wantRoute: "glm-5.2",
		},
		{
			// zai-org/GLM-5.2 是另一家的模型：这一家有索引，但索引里没有这个名字，因此没有可发往上游的内容。
			// 旧的返回值（原样给出请求名且没有路由）会让执行器把客户端的模型名发给一家服务不了它的 provider，
			// 随之而来的 400 会终止整个请求。
			name: "provider does not serve model", provider: "modelscope-my", requested: "zai-org/GLM-5.2",
			wantUpstream: "", wantRoute: "",
		},
		{
			// 这就是线上抓包的那种情形：modelscope-my 把客户端请求的那个模型注册成了另一个别名，因此跨
			// provider 解析必须同时找回上游模型名和这一家自己可路由的 ID。
			name: "same model under another alias", provider: "modelscope-ai-deepseek2", requested: "glm-5.2",
			wantUpstream: "zai-org/GLM-5.2", wantRoute: "glm-5.2",
		},
		{
			name: "unknown provider keeps passthrough behaviour", provider: "modelscope-other", requested: "glm-5.2",
			wantUpstream: "glm-5.2", wantRoute: "glm-5.2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, route := resolveProviderModel(resolved, tc.provider, tc.requested)
			if upstream != tc.wantUpstream {
				t.Fatalf("upstream = %q, want %q", upstream, tc.wantUpstream)
			}
			if route != tc.wantRoute {
				t.Fatalf("route = %q, want %q", route, tc.wantRoute)
			}
		})
	}
}

// providers 顺序的回归用例：宿主只以别名注册配了别名的兼容模型，因此带上游模型名的 chat
// 兜底会拿到 model_not_found；修复前第一家会无声地从序列里消失。
func TestExecuteResponsesPassthroughAliasedFirstProviderServes(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()
	routable := getResolved().routeToName["modelscope-ai-deepseek2"]

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	// 行为与宿主一致：按精确的模型 ID 在注册表里解析 provider，未注册的一律拒绝。
	hostModelExecute = func(req executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		if _, ok := routable[req.Model]; !ok {
			return pluginapi.HostModelExecutionResponse{}, errors.New("unknown provider for model " + req.Model)
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"provider":"glm-chat"}`)}, nil
	}
	directCalls := 0
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		directCalls++
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-ds"}`)}, nil
	}

	var logs bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"provider":"glm-chat"}` {
		t.Fatalf("first provider must serve the request, got %s", string(resp.Payload))
	}
	if directCalls != 0 {
		t.Fatalf("DeepSeek passthrough must not run once the first provider serves, calls=%d", directCalls)
	}
}

// 守住静默跳过：chat 兜底失败时既要继续往后一家走，也要把这件事说出来。
func TestExecuteResponsesPassthroughChatFallbackFailureIsLogged(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, errors.New("unknown provider for model zai-org/GLM-5.2")
	}
	var gotURLs []string
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		gotURLs = append(gotURLs, req.URL)
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-ds"}`)}, nil
	}

	var logs bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"id":"resp-ds"}` {
		t.Fatalf("second provider should serve, got %s", string(resp.Payload))
	}
	if len(gotURLs) != 1 || gotURLs[0] != "https://ds.example/v1/responses" {
		t.Fatalf("upstream calls = %v, want only the DeepSeek responses endpoint", gotURLs)
	}
	if logged := logs.String(); !strings.Contains(logged, "responses chat fallback failed") ||
		!strings.Contains(logged, "provider=modelscope-ai-deepseek2") {
		t.Fatalf("chat fallback failure must be logged, got: %s", logged)
	}
}

// 把上面那个非流式的回归搬到 SSE 流式路径上。
func TestExecuteResponsesPassthroughStreamAliasedFirstProviderServes(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()
	routable := getResolved().routeToName["modelscope-ai-deepseek2"]

	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	origModelRead := hostModelStreamRead
	t.Cleanup(func() {
		hostHTTPDoStream = origStream
		hostModelExecuteStream = origModelStream
		hostModelStreamRead = origModelRead
	})

	directCalls := 0
	hostHTTPDoStream = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		directCalls++
		return hostHTTPStreamResponse{StatusCode: http.StatusOK}, nil
	}
	hostModelStreamRead = func(string) (pluginapi.HostModelStreamReadResponse, error) {
		return pluginapi.HostModelStreamReadResponse{Payload: []byte("data: glm"), Done: true}, nil
	}
	hostFallbackModels := make([]string, 0)
	hostModelExecuteStream = func(req executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		hostFallbackModels = append(hostFallbackModels, req.Model)
		if _, ok := routable[req.Model]; !ok {
			return pluginapi.HostModelStreamResponse{}, errors.New("unknown provider for model " + req.Model)
		}
		return pluginapi.HostModelStreamResponse{
			StatusCode: 200,
			StreamID:   "glm-host-stream",
			Headers:    http.Header{"X-Test": []string{"glm-host"}},
		}, nil
	}

	resp, err := executeResponsesPassthroughStream(executorTestRequest(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hostFallbackModels) != 1 || hostFallbackModels[0] != "glm-5.2" {
		t.Fatalf("first provider stream fallback must use the host-routable model ID, got %v", hostFallbackModels)
	}
	if directCalls != 0 {
		t.Fatalf("DeepSeek responses stream must not run once the first provider serves, calls=%d", directCalls)
	}
	if got := resp.Headers.Get("X-Test"); got != "glm-host" {
		t.Fatalf("the first provider stream must be the one handed to the client, headers = %v", resp.Headers)
	}
}

// 覆盖上游用 HTTP 200 返回一份只有 code 和 message 字段的裸错误文档的情况，这是 Modelscope 拒绝某个
// 参数时的真实行为。这种 body 不是 Response 资源，绝不能当成功返回：执行器要继续轮换，最终让宿主路径
// 应答。
func TestExecuteResponsesPassthroughErrorPayloadIsNotSuccess(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	errBody := []byte(`{"code":"InvalidParameter","message":"Invalid 'max_output_tokens': must be greater than or equal to 16, got 8.","request_id":"0099"}`)
	upstreamCalls := 0
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		upstreamCalls++
		return hostHTTPResponse{StatusCode: 200, Body: errBody}, nil
	}
	hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"host":"answered"}`)}, nil
	}

	var logs bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	resp, err := executeResponsesPassthrough(executorTestRequest(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"host":"answered"}` {
		t.Fatalf("error payload must not be returned as a success, got %s", string(resp.Payload))
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstream calls = %d, want both managed keys tried", upstreamCalls)
	}
	if logged := logs.String(); !strings.Contains(logged, "returned an error payload with HTTP 200") ||
		!strings.Contains(logged, "InvalidParameter") {
		t.Fatalf("error payload must be logged, got: %s", logged)
	}
}

// 上面场景的 SSE 版本：流以 200 建连，随后送来一个不带任何 data 事件的 JSON 错误文档，必须当作这个候
// 选失败处理，不能转发出去。
func TestExecuteResponsesPassthroughStreamErrorPayloadTriggersFallback(t *testing.T) {
	setTestPluginConfig("modelscope")
	setTestResolvedExec()

	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	origRead := hostHTTPStreamRead
	origModelRead := hostModelStreamRead
	t.Cleanup(func() {
		hostHTTPDoStream = origStream
		hostModelExecuteStream = origModelStream
		hostHTTPStreamRead = origRead
		hostModelStreamRead = origModelRead
	})

	errBody := []byte(`{"code":"InvalidParameter","message":"model does not support /v1/responses"}`)
	forwarded := 0
	hostHTTPDoStream = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		forwarded++
		return hostHTTPStreamResponse{StatusCode: 200, StreamID: "up-err"}, nil
	}
	hostHTTPStreamRead = func(string) (hostStreamReadResponse, error) {
		return hostStreamReadResponse{Payload: errBody, Done: true}, nil
	}
	fallbackCalled := false
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		fallbackCalled = true
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "host-ok"}, nil
	}
	hostModelStreamRead = func(string) (pluginapi.HostModelStreamReadResponse, error) {
		return pluginapi.HostModelStreamReadResponse{Payload: []byte("data: ok"), Done: true}, nil
	}

	var logs bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	if _, err := executeResponsesPassthroughStream(executorTestRequest(true)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if forwarded == 0 {
		t.Fatal("managed stream loop was not tried")
	}
	if !fallbackCalled {
		t.Fatal("a 200 stream carrying only an error document must fall through to the host path")
	}
	if logged := logs.String(); !strings.Contains(logged, "returned an error payload with HTTP 200") {
		t.Fatalf("stream error payload must be logged, got: %s", logged)
	}
}

// 校验三点：某家的 chat 兜底会告诉宿主必须由哪家应答；这个标记不会漏进调用方自己的请求头；最后那次不
// 设限的宿主兜底保持不钉定。
func TestExecuteResponsesPassthroughChatFallbackPinsProvider(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	type chatCall struct {
		model  string
		pinned string
		client string
	}
	var calls []chatCall
	hostModelExecute = func(req executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		calls = append(calls, chatCall{
			model:  req.Model,
			pinned: req.Headers.Get(modelscope.PinnedProviderHeader),
			client: req.Headers.Get("X-Client-Marker"),
		})
		if len(calls) == 1 {
			return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusTooManyRequests, Body: []byte(`{"error":"rate_limit"}`)}, nil
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"host":"final"}`)}, nil
	}
	hostHTTPDo = func(_ pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		return hostHTTPResponse{StatusCode: http.StatusTooManyRequests, Body: []byte(`{"error":"rate_limit"}`)}, nil
	}

	req := executorTestRequest(false)
	req.Headers = http.Header{"X-Client-Marker": []string{"keep-me"}}

	resp, err := executeResponsesPassthrough(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"host":"final"}` {
		t.Fatalf("payload = %s, want the unconstrained host fallback", string(resp.Payload))
	}
	if len(calls) != 2 {
		t.Fatalf("host chat calls = %d, want the pinned step plus the final fallback: %+v", len(calls), calls)
	}
	if calls[0].pinned != "modelscope-ai-deepseek2" || calls[0].model != "glm-5.2" {
		t.Fatalf("provider step must be pinned to its own provider: %+v", calls[0])
	}
	if calls[0].client != "keep-me" {
		t.Fatalf("client headers must survive the copy: %+v", calls[0])
	}
	if calls[1].pinned != "" {
		t.Fatalf("the all-exhausted host fallback must not be pinned: %+v", calls[1])
	}
	if got := req.Headers.Get(modelscope.PinnedProviderHeader); got != "" {
		t.Fatalf("the executor must not mutate the caller's headers, found %q", got)
	}
}

// 用面板保存的那种取值驱动路由门：该字段是字符串型配置项，到达时永远是一个 YAML 标量。
// 方括号写法和 true 这个关键字都应当照样命中对应模型。
func TestRouteResponsesRequestPanelScalarForms(t *testing.T) {
	setTestResolvedExec()
	cases := []struct {
		name  string
		raw   string
		model string
		want  bool
	}{
		{"bracket_list_hit", `["ZhipuAI/GLM-5.2", "Qwen/Qwen2.5-72B-Instruct"]`, "glm-5.2", true},
		{"bracket_list_miss", `["ZhipuAI/GLM-5.2", "Qwen/Qwen2.5-72B-Instruct"]`, "qwen-72b", false},
		{"comma_list", "ZhipuAI/GLM-5.2, Qwen/Qwen2.5-72B-Instruct", "glm-5.2", true},
		{"keyword_true", "true", "glm-5.2", true},
		{"keyword_false", "false", "glm-5.2", false},
		{"wildcard", "*", "glm-5.2", true},
		{"unknown_word", "responses", "glm-5.2", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := modelscope.ConfigFromMap(map[string]any{
				"providers":     []any{"modelscope"},
				"use_responses": tt.raw,
			})
			if err != nil {
				t.Fatalf("ConfigFromMap: %v", err)
			}
			pluginCfg = cfg
			store.Reconfigure(cfg)
			resp := routeResponsesRequest(pluginapiModelRouteRequest("openai-response", tt.model))
			if resp.Handled != tt.want {
				t.Fatalf("use_responses=%q model=%q handled=%v, want %v", tt.raw, tt.model, resp.Handled, tt.want)
			}
		})
	}
}

// 与线上部署一致：三家 provider 都提供 deepseek-ai/DeepSeek-V4-Pro-0813。
// modelscope-my 则把它挂在别名 glm-5.2 下，因此它的索引查不到 deepseek-v4-pro。
// 只有 modelscope 和 modelscope-my 提供 Qwen/Qwen3.8-Flash-Next。
func setTestResolvedAliasCollision() {
	routes := map[string]map[string]string{
		"modelscope": {
			"glm-5.2":            "ZhipuAI/GLM-5.2",
			"deepseek-v4-pro":    "deepseek-ai/DeepSeek-V4-Pro-0813",
			"Qwen3.8-Flash-Next": "Qwen/Qwen3.8-Flash-Next",
		},
		"modelscope-my": {
			"glm-5.2":            "deepseek-ai/DeepSeek-V4-Pro-0813",
			"ZhipuAI/GLM-5.2":    "ZhipuAI/GLM-5.2",
			"Qwen3.8-Flash-Next": "Qwen/Qwen3.8-Flash-Next",
		},
		"modelscope-ai": {
			"glm-5.2":         "zai-org/GLM-5.2",
			"deepseek-v4-pro": "deepseek-ai/DeepSeek-V4-Pro-0813",
		},
	}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		raw: map[string]string{"auth-cms": "key-ms", "auth-cmy": "key-my", "auth-cai": "key-ai"},
		baseURL: map[string]string{
			"auth-cms": "https://ms.example/v1",
			"auth-cmy": "https://my.example/v1",
			"auth-cai": "https://ai.example/v1",
		},
		routeToName: routes,
		nameToRoute: withRouteIndex(routes),
		providerOrder: map[string][]string{
			"modelscope":    {"auth-cms"},
			"modelscope-my": {"auth-cmy"},
			"modelscope-ai": {"auth-cai"},
		},
		at: time.Now(),
	}
	resolvedMu.Unlock()
}

func responsesTestRequest(model string) executorRequestWithCallback {
	return executorRequestWithCallback{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           model,
			SourceFormat:    "openai-response",
			Stream:          false,
			OriginalRequest: []byte(`{"model":"` + model + `","input":"hi"}`),
			Headers:         http.Header{},
		},
		HostCallbackID: "test-cb",
	}
}

func TestResolveProviderModelCrossProviderAlias(t *testing.T) {
	setTestPluginConfig("modelscope", "modelscope-my", "modelscope-ai")
	setTestResolvedAliasCollision()

	cases := []struct {
		provider     string
		requested    string
		wantUpstream string
		wantRoute    string
	}{
		{"modelscope", "deepseek-v4-pro", "deepseek-ai/DeepSeek-V4-Pro-0813", "deepseek-v4-pro"},
		// 回归点：modelscope-my 也提供同一个模型，只是名字叫 glm-5.2，因此它自己的索引看不到请求的别名。
		{"modelscope-my", "deepseek-v4-pro", "deepseek-ai/DeepSeek-V4-Pro-0813", "glm-5.2"},
		{"modelscope-my", "glm-5.2", "deepseek-ai/DeepSeek-V4-Pro-0813", "glm-5.2"},
		{"modelscope-ai", "glm-5.2", "zai-org/GLM-5.2", "glm-5.2"},
		// 完全不提供该模型的 provider 解析结果为空：把客户端别名转发过去只会换来一个 Invalid model id。
		{"modelscope-ai", "Qwen3.8-Flash-Next", "", ""},
		{"modelscope", "Qwen3.8-Flash-Next", "Qwen/Qwen3.8-Flash-Next", "Qwen3.8-Flash-Next"},
		// 三家都不认识的模型：同样没有可发往上游的名字。
		{"modelscope", "no-such-model", "", ""},
	}
	for _, tt := range cases {
		upstream, route := resolveProviderModel(getResolved(), tt.provider, tt.requested)
		if upstream != tt.wantUpstream || route != tt.wantRoute {
			t.Fatalf("resolveProviderModel(%q, %q) = (%q, %q), want (%q, %q)",
				tt.provider, tt.requested, upstream, route, tt.wantUpstream, tt.wantRoute)
		}
	}
}

// 复现抓包到的失败：第一家答 429，重试第二家时过去会发出客户端别名，因为那一家把同一个上游模型注册成
// 了另一个别名。
func TestExecuteResponsesPassthroughRetryUsesCanonicalModel(t *testing.T) {
	setTestPluginConfig("modelscope", "modelscope-my")
	pluginCfg.SkipProviderOn429 = false
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedAliasCollision()

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	var sentModels []string
	var sentURLs []string
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(req.Body, &body)
		sentModels = append(sentModels, body.Model)
		sentURLs = append(sentURLs, req.URL)
		if len(sentModels) == 1 {
			// 用一个普通 429：写 insufficient balance 会把这把凭据当天禁用，并把该状态泄漏给后续用例。
			return hostHTTPResponse{StatusCode: 429, Body: []byte(`{"error":{"message":"Too many requests"}}`)}, nil
		}
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-second","object":"response"}`)}, nil
	}
	hostModelExecute = func(req executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		t.Fatalf("host chat fallback must not run once the second provider answers, model=%s", req.Model)
		return pluginapi.HostModelExecutionResponse{}, nil
	}

	resp, err := executeResponsesPassthrough(responsesTestRequest("deepseek-v4-pro"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"id":"resp-second","object":"response"}` {
		t.Fatalf("unexpected payload: %s", string(resp.Payload))
	}
	want := "deepseek-ai/DeepSeek-V4-Pro-0813"
	if len(sentModels) != 2 {
		t.Fatalf("expected two upstream attempts, got %v (%v)", sentModels, sentURLs)
	}
	if sentModels[0] != want || sentModels[1] != want {
		t.Fatalf("both attempts must carry the upstream model name, got %v", sentModels)
	}
}

// 校验另一半：不提供该模型的托管 provider 会被跳过，请求仍然能到达提供它的那一家，而不是死在 Invalid
// model id 上。
func TestExecuteResponsesPassthroughSkipsProviderWithoutModel(t *testing.T) {
	// 先试 modelscope-ai，它根本不提供这个 Qwen 模型。
	setTestPluginConfig("modelscope-ai", "modelscope")
	pluginCfg.SkipProviderOn429 = false
	pluginCfg.UseResponses = []string{"*"}
	setTestResolvedAliasCollision()

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	var sentModels []string
	var sentURLs []string
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(req.Body, &body)
		sentModels = append(sentModels, body.Model)
		sentURLs = append(sentURLs, req.URL)
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-ok","object":"response"}`)}, nil
	}
	hostModelExecute = func(req executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		t.Fatalf("a provider that cannot serve the model must be skipped, not answered via host, model=%s", req.Model)
		return pluginapi.HostModelExecutionResponse{}, nil
	}

	resp, err := executeResponsesPassthrough(responsesTestRequest("Qwen3.8-Flash-Next"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Payload) != `{"id":"resp-ok","object":"response"}` {
		t.Fatalf("unexpected payload: %s", string(resp.Payload))
	}
	// 修复前第一步就把客户端别名原样发出去，上游回 400 Invalid model id，而这个不可重试的状态码会让整个请
	// 求终止。
	if len(sentModels) != 1 || sentModels[0] != "Qwen/Qwen3.8-Flash-Next" {
		t.Fatalf("only the serving provider may be called, with the upstream name: %v (%v)", sentModels, sentURLs)
	}
}

// 两家 provider 都提供同一个上游模型，各自用自己的路由 ID；alpha 家有两把凭据。
func setTestResolvedTwoKeyProviders() {
	routes := map[string]map[string]string{
		"alpha": {"shared-a": "vendor/Shared-Model"},
		"beta":  {"shared-b": "vendor/Shared-Model"},
	}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		raw:         map[string]string{"a1": "key-a1", "a2": "key-a2", "b1": "key-b1"},
		baseURL:     map[string]string{"a1": "https://alpha.example/v1", "a2": "https://alpha.example/v1", "b1": "https://beta.example/v1"},
		routeToName: routes,
		nameToRoute: withRouteIndex(routes),
		providerOrder: map[string][]string{
			"alpha": {"a1", "a2"},
			"beta":  {"b1"},
		},
		at: time.Now(),
	}
	resolvedMu.Unlock()
}

// 已禁用的凭据不能被当作这一家的应答，必须先轮换到同一家还活着的 key。
func TestResolveStepAvoidsDisabledKeys(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	setTestResolvedTwoKeyProviders()

	store.DisableGlobal("a1", "vendor/Shared-Model", store.Now())
	cand, ok := resolveStep(retryStep{provider: "alpha"}, getResolved(), "shared-a")
	if !ok {
		t.Fatal("resolveStep must succeed with the provider's remaining key")
	}
	if cand.authID != "a2" {
		t.Fatalf("resolveStep picked %q, want the undisabled key a2", cand.authID)
	}

	// 全部 key 都被禁用：这一家确实没有可用凭据了。
	store.DisableGlobal("a2", "vendor/Shared-Model", store.Now())
	if _, ok := resolveStep(retryStep{provider: "alpha"}, getResolved(), "shared-a"); ok {
		t.Fatal("resolveStep must report no candidate once all keys are disabled")
	}
}

// 这就是被报出来的那个 bug：skip_provider_on_429 下每家只有一步，key 游标在成功时也会前进，于是落在
// 一把当天被禁用的 key 上就会让整家作废，请求因此跳到第二家，而第一家其实还有可用凭据。
func TestExecuteResponsesPassthroughDisabledKeyKeepsFirstProvider(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"vendor/Shared-Model"}
	setTestResolvedTwoKeyProviders()
	store.DisableGlobal("a1", "vendor/Shared-Model", store.Now())

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	type attempt struct {
		url  string
		auth string
	}
	var attempts []attempt
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		attempts = append(attempts, attempt{url: req.URL, auth: req.Headers.Get("Authorization")})
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-alpha"}`)}, nil
	}
	hostModelExecute = func(r executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		t.Fatalf("no host fallback expected, model=%s", r.Model)
		return pluginapi.HostModelExecutionResponse{}, nil
	}

	if _, err := executeResponsesPassthrough(responsesTestRequest("shared-a")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("expected a single upstream attempt, got %+v", attempts)
	}
	if attempts[0].url != "https://alpha.example/v1/responses" || attempts[0].auth != "Bearer key-a2" {
		t.Fatalf("first provider must be served by its remaining live key, got %+v", attempts[0])
	}
}

// TestSliceCompleteSSELinesKeepsCompletedEventIntactAcrossReadBoundary 守住 SSE 行对齐缓冲：
// 上游把 response.completed 那条 data: 行横跨两次原始读取返回时，转交给宿主的每一块仍
// 必须是完整行（宿主逐块按 \n 解析 usage、无跨块续行），否则整次请求的 token 用量丢失。
func TestSliceCompleteSSELinesKeepsCompletedEventIntactAcrossReadBoundary(t *testing.T) {
	completed := []byte("data: " + `{"type":"response.completed","response":{"usage":{"input_tokens":111,"output_tokens":45,"total_tokens":156}}}` + "\n\n")
	boundary := 32 * 1024
	// filler 是一条 SSE 注释行，长度精确把 completed 的 data: 行推到 32KB 边界前 30
	// 字节，使边界落在 completed 的 JSON 内部。
	filler := append([]byte(":"), bytes.Repeat([]byte("x"), boundary-30-3)...)
	filler = append(filler, '\n', '\n')
	chunkA := append(append([]byte{}, filler...), completed[:30]...)
	chunkB := completed[30:]
	rawChunks := [][]byte{chunkA, chunkB}

	// 对照：原始两块各自按 \n 切行解析都拿不到 usage（这正是未对齐时丢 token 的条件）。
	if parseStreamUsagePerChunk(t, rawChunks) {
		t.Fatal("control: raw reads must NOT yield usage when the completed line is split across reads")
	}

	// 经过 sliceCompleteSSELines 行对齐后收集 emit 段（最后一块按 final=true 收尾）。
	var pending []byte
	var emitted [][]byte
	for i, raw := range rawChunks {
		seg, rest := sliceCompleteSSELines(pending, raw, i == len(rawChunks)-1)
		pending = rest
		if len(seg) > 0 {
			emitted = append(emitted, seg)
		}
	}
	if !parseStreamUsagePerChunk(t, emitted) {
		t.Fatalf("aligned emit segments lost usage; got %d segments", len(emitted))
	}
}

// parseStreamUsagePerChunk 复刻宿主 observePluginExecutorStreamUsage 的判定：每块按 \n 切，
// 每行剥 data: 前缀后解析 JSON，命中 response.usage 带 token 字段即为找到。
func parseStreamUsagePerChunk(t *testing.T, chunks [][]byte) bool {
	t.Helper()
	for _, c := range chunks {
		for _, line := range bytes.Split(c, []byte("\n")) {
			j := bytes.TrimSpace(line)
			if bytes.HasPrefix(j, []byte("data:")) {
				j = bytes.TrimSpace(bytes.TrimPrefix(j, []byte("data:")))
			}
			if len(j) == 0 || j[0] != '{' {
				continue
			}
			var obj struct {
				Response struct {
					Usage map[string]any `json:"usage"`
				} `json:"response"`
			}
			if err := json.Unmarshal(j, &obj); err != nil {
				continue
			}
			if u := obj.Response.Usage; len(u) > 0 {
				if _, ok := u["total_tokens"]; ok {
					return true
				}
			}
		}
	}
	return false
}
