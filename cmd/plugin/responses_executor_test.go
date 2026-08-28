package main

import (
	"bytes"
	"errors"
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
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		masked:        map[string]string{"auth-1": "ms-key-AAAA"},
		raw:           map[string]string{"auth-1": "ms-key-AAAA"},
		baseURL:       map[string]string{"auth-1": "https://api-inference.modelscope.cn/v1"},
		aliasToName:   map[string]map[string]string{"modelscope": {"glm-5.2": "ZhipuAI/GLM-5.2"}},
		providerOrder: map[string][]string{"modelscope": {"auth-1"}},
		at:            time.Now(),
	}
	resolvedMu.Unlock()
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
	pluginCfg.UseResponses = true
	resp := routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "glm-5.2"))
	if !resp.Handled || resp.TargetKind != pluginapiModelRouteTargetSelf {
		t.Fatalf("responses request must route to self, got %+v", resp)
	}
	pluginCfg.UseResponses = false
}

func TestRouteResponsesRequestCodexHandled(t *testing.T) {
	setTestPluginConfig("modelscope")
	pluginCfg.UseResponses = true
	resp := routeResponsesRequest(pluginapiModelRouteRequest("codex", "gpt-5"))
	if !resp.Handled || resp.TargetKind != pluginapiModelRouteTargetSelf {
		t.Fatalf("codex responses request must route to self, got %+v", resp)
	}
	pluginCfg.UseResponses = false
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
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		masked: map[string]string{"auth-1": "ms-key-AAAA", "auth-2": "ms-key-BBBB"},
		raw:    map[string]string{"auth-1": "ms-key-AAAA", "auth-2": "ms-key-BBBB"},
		baseURL: map[string]string{
			"auth-1": "https://api-inference.modelscope.cn/v1",
			"auth-2": "https://api-inference.modelscope.cn/v1",
		},
		aliasToName: map[string]map[string]string{
			"modelscope":    {"glm-5.2": "ZhipuAI/GLM-5.2"},
			"modelscope-ai": {"glm-5.2": "zai-org/GLM-5.2"},
		},
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
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		raw: map[string]string{"auth-1": "k1", "auth-2": "k2", "auth-3": "k3"},
		baseURL: map[string]string{
			"auth-1": "https://a/v1", "auth-2": "https://b/v1", "auth-3": "https://c/v1",
		},
		aliasToName: map[string]map[string]string{
			"modelscope":    {"m": "up/m"},
			"modelscope-ai": {"m": "up/ma"},
		},
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
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		raw:           map[string]string{"a1": "k1", "a2": "k2"},
		baseURL:       map[string]string{"a1": "https://x/v1", "a2": "https://x/v1"},
		aliasToName:   map[string]map[string]string{"modelscope": {"m": "up/m"}},
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
	pluginCfg.UseResponses = true
	resp := routeResponsesRequest(pluginapiModelRouteRequest("openai-response", "glm-5.2"))
	if resp.Handled {
		t.Fatal("responses request must not be handled when providers is empty")
	}
	pluginCfg.UseResponses = false
}

// ---- executor defer logic (stubbed I/O) ----

func setTestResolvedExec() {
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		raw:           map[string]string{"auth-1": "key1", "auth-2": "key2"},
		baseURL:       map[string]string{"auth-1": "https://up/v1", "auth-2": "https://up/v1"},
		aliasToName:   map[string]map[string]string{"modelscope": {"glm-5.2": "ZhipuAI/GLM-5.2"}},
		providerOrder: map[string][]string{"modelscope": {"auth-1", "auth-2"}},
		at:            time.Now(),
	}
	resolvedMu.Unlock()
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
