package main

import (
	"bytes"
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

// withRouteIndex derives the upstream-name -> route-ID index that resolveKeys
// builds alongside routeToName, so test fixtures stay faithful to what the glue
// computes from the management API (route ID = alias when set, else name).
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

	// skip ON: one step per managed provider, no non-managed steps.
	cfg.SkipProviderOn429 = true
	steps := buildRetrySequence(getResolved(), cfg)
	if len(steps) != 2 {
		t.Fatalf("skip ON: want 2 managed steps, got %+v", steps)
	}
	if steps[0].provider != "modelscope" || steps[1].provider != "modelscope-ai" {
		t.Fatalf("skip ON: provider order wrong: %+v", steps)
	}

	// skip OFF: one step per managed key (3 total), no non-managed steps.
	cfg.SkipProviderOn429 = false
	steps = buildRetrySequence(getResolved(), cfg)
	if len(steps) != 3 {
		t.Fatalf("skip OFF: want 3 managed steps, got %d (%+v)", len(steps), steps)
	}
	for _, s := range steps {
		t.Logf("step provider=%s", s.provider)
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

// ---- resolveStep tests ----

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

// ---- routeResponsesRequest edge cases ----

func TestRouteResponsesRequestEmptyProviders(t *testing.T) {
	setTestPluginConfig()
	pluginCfg.UseResponses = []string{"*"}
	resp := routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "glm-5.2"))
	if resp.Handled {
		t.Fatal("responses request must not be handled when providers is empty")
	}
	pluginCfg.UseResponses = nil
}

// ---- executor defer logic (stubbed I/O) ----

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
	// Mirrors the real deployment: provider A registers zai-org/GLM-5.2 under the
	// alias glm-5.2 (so "zai-org/GLM-5.2" is NOT a host-routable model ID) and
	// also serves the DeepSeek model by name; provider B registers
	// deepseek-ai/DeepSeek-V4-Pro-0813 under the same alias glm-5.2.
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
	// The host registry only knows the alias for an aliased compat model, so the
	// chat fallback must carry the route ID ("glm-5.2") and not the upstream
	// name ("zai-org/GLM-5.2"), which host.model.execute cannot resolve.
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
	// First call: provider A's chat fallback with the host-routable ID. Second
	// call: the all-managed-exhausted fallback, which keeps the client model.
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

// TestExecuteResponsesPassthroughSkippedProviderKeepsNativeResponsesPath covers
// a protocol downgrade: provider 1 sits in the skip set after a 429, yet its
// chat-fallback step still asks the host to execute the alias. The host's
// scheduler then answers with provider 2's credential, so provider 2's model is
// served over /v1/chat/completions even though it is Responses-enabled and its
// own native step was never reached.
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

// TestExecuteResponsesPassthroughDisabledProviderKeepsNativeResponsesPath is the
// same downgrade reached through daily disables instead of the skip set.
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

// TestResolveProviderModelSeparatesRouteAndUpstreamModels pins the two model
// identifiers a managed-provider step needs: the upstream name for direct
// /v1/responses calls, and the host-routable ID (alias when one is configured)
// for host.model.execute chat fallbacks.
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
			name: "provider does not serve model", provider: "modelscope-my", requested: "zai-org/GLM-5.2",
			wantUpstream: "zai-org/GLM-5.2", wantRoute: "",
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

// TestExecuteResponsesPassthroughAliasedFirstProviderServes is the
// providers-order regression: CLIProxyAPI registers an aliased
// openai-compatibility model under the alias only, so a chat fallback that
// carries the upstream name comes back as model_not_found and the first
// configured provider used to vanish from the sequence without a trace.
func TestExecuteResponsesPassthroughAliasedFirstProviderServes(t *testing.T) {
	setTestPluginConfig("modelscope-ai-deepseek2", "modelscope-my")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"deepseek-ai/DeepSeek-V4-Pro-0813"}
	setTestResolvedMixedResponses()
	routable := getResolved().routeToName["modelscope-ai-deepseek2"]

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	// Behaves like the host: resolve providers from the model registry by exact
	// model ID, and reject anything not registered.
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

// TestExecuteResponsesPassthroughChatFallbackFailureIsLogged guards the silent
// skip: a chat fallback that fails must still fall through to the next managed
// provider, but it must say so.
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

// TestExecuteResponsesPassthroughStreamAliasedFirstProviderServes mirrors the
// non-streaming regression for the SSE path.
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

// TestExecuteResponsesPassthroughErrorPayloadIsNotSuccess covers the upstream
// that answers HTTP 200 with a bare {"code":...,"message":...} document (real
// Modelscope behaviour for a rejected parameter). That body is not a Response
// resource, so it must not be returned as a success: the executor has to keep
// rotating and ultimately let the host path answer.
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

// TestExecuteResponsesPassthroughStreamErrorPayloadTriggersFallback is the SSE
// twin: a stream that opens 200 and then delivers a JSON error document without
// any data: event must be treated as a failed candidate, not forwarded.
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

// TestExecuteResponsesPassthroughChatFallbackPinsProvider checks that a
// provider's chat-fallback step tells the host which provider must answer, that
// the marker does not leak into the caller's own header set, and that the final
// unconstrained host fallback stays unpinned.
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
