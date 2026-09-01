package main

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	modelscope "modelscope-ratelimit/internal/ratelimit"
)

// 上游以 HTTP 200 + SSE response.failed 回送的 key 级速率/额度拒绝。
const keyLevelResponsesEvent = `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"Request rate increased too quickly."}}}`

// 池级额度耗尽（同一种 200 + response.failed 形态，但 code 是 insufficient_quota）。
const poolLevelResponsesEvent = `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"insufficient_quota","message":"Allocated quota exceeded."}}}`

// setTestResolvedThreeKeyAlpha 给 alpha 三家 3 把 key、beta 1 把 key，两家提供同一个上游模型，
// 用于断言"先在本家逐 key 轮换、本家用尽才换 provider"。
func setTestResolvedThreeKeyAlpha() {
	routes := map[string]map[string]string{
		"alpha": {"shared-a": "vendor/Shared-Model"},
		"beta":  {"shared-b": "vendor/Shared-Model"},
	}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		raw:         map[string]string{"a1": "key-a1", "a2": "key-a2", "a3": "key-a3", "b1": "key-b1"},
		baseURL:     map[string]string{"a1": "https://alpha.example/v1", "a2": "https://alpha.example/v1", "a3": "https://alpha.example/v1", "b1": "https://beta.example/v1"},
		routeToName: routes,
		nameToRoute: withRouteIndex(routes),
		providerOrder: map[string][]string{
			"alpha": {"a1", "a2", "a3"},
			"beta":  {"b1"},
		},
		at: time.Now(),
	}
	resolvedMu.Unlock()
}

type attemptRecord struct {
	host string
	auth string
}

func recordAttempt(req pluginapi.HTTPRequest) attemptRecord {
	return attemptRecord{host: req.URL, auth: attemptKey(req)}
}

func attemptHosts(attempts []attemptRecord) string {
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		parts = append(parts, a.host+" "+a.auth)
	}
	return strings.Join(parts, ", ")
}

func wantSameProviderRetry(t *testing.T, attempts []attemptRecord, minAttempts int) {
	t.Helper()
	if len(attempts) < minAttempts {
		t.Fatalf("expected at least %d upstream attempts, got [%s]", minAttempts, attemptHosts(attempts))
	}
	for i, a := range attempts[:minAttempts] {
		if !strings.HasPrefix(a.host, "https://alpha.example") {
			t.Fatalf("attempt %d left the first provider while it still had usable keys: [%s]", i+1, attemptHosts(attempts))
		}
	}
	for i := 1; i < minAttempts; i++ {
		if attempts[i].auth == attempts[i-1].auth {
			t.Fatalf("attempt %d reused key %s instead of rotating: [%s]", i, attempts[i].auth, attemptHosts(attempts))
		}
	}
}

// 线上缺陷的回归用例：Responses 透传遇到 200 + response.failed(rate_limit_exceeded) 时，只算这一
// 把 key 被拒（它不禁用任何 key），下一步必须还是同一家另一把 key，而不是第二家的另一个上游模型。
func TestExecuteResponsesPassthroughKeyLevelRateLimitRotatesKey(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"vendor/Shared-Model"}
	setTestResolvedThreeKeyAlpha()

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	var attempts []attemptRecord
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		attempts = append(attempts, recordAttempt(req))
		if len(attempts) == 1 {
			return hostHTTPResponse{StatusCode: 200, Body: []byte(keyLevelResponsesEvent)}, nil
		}
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-a2"}`)}, nil
	}
	hostModelExecute = func(r executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		t.Fatalf("no host fallback expected, model=%s", r.Model)
		return pluginapi.HostModelExecutionResponse{}, nil
	}

	if _, err := executeResponsesPassthrough(responsesTestRequest("shared-a")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantSameProviderRetry(t, attempts, 2)
	if store.IsProviderSkipped("alpha") {
		t.Fatal("a key-level responses rate limit must not put the provider in the skip set")
	}
}

// 同一个判定在流式路径上也要成立：这条路径线上跑的就是 /v1/responses 流式。
func TestExecuteResponsesPassthroughStreamKeyLevelRateLimitRotatesKey(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"vendor/Shared-Model"}
	setTestResolvedThreeKeyAlpha()

	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	origRead := hostHTTPStreamRead
	t.Cleanup(func() {
		hostHTTPDoStream = origStream
		hostModelExecuteStream = origModelStream
		hostHTTPStreamRead = origRead
	})

	var attempts []attemptRecord
	hostHTTPDoStream = func(req pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		attempts = append(attempts, recordAttempt(req))
		return hostHTTPStreamResponse{StatusCode: 200, StreamID: "up-managed"}, nil
	}
	hostHTTPStreamRead = func(string) (hostStreamReadResponse, error) {
		if len(attempts) == 1 {
			return hostStreamReadResponse{Payload: []byte(keyLevelResponsesEvent + "\n\n"), Done: true}, nil
		}
		return hostStreamReadResponse{Payload: []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-a2\"}}\n\n"), Done: true}, nil
	}
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		t.Fatal("no host stream fallback expected")
		return pluginapi.HostModelStreamResponse{}, nil
	}

	if _, err := executeResponsesPassthroughStream(responsesStreamTestRequest("shared-a")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantSameProviderRetry(t, attempts, 2)
	if store.IsProviderSkipped("alpha") {
		t.Fatal("a key-level responses rate limit must not put the provider in the skip set")
	}
}

func responsesStreamTestRequest(model string) executorRequestWithCallback {
	req := responsesTestRequest(model)
	req.Stream = true
	req.StreamID = "test-stream"
	return req
}

// 只有池级 insufficient_quota 才允许把整家摘掉：这时本家剩下的 key 步骤被丢掉，直接交给下一家。
func TestExecuteResponsesPassthroughPoolLevelResponsesEventHandsOffToNextProvider(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"vendor/Shared-Model"}
	setTestResolvedThreeKeyAlpha()
	t.Cleanup(func() { store.ClearAllSkippedProviders() })

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	var attempts []attemptRecord
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		attempts = append(attempts, recordAttempt(req))
		if len(attempts) == 1 {
			return hostHTTPResponse{StatusCode: 200, Body: []byte(poolLevelResponsesEvent)}, nil
		}
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-b1"}`)}, nil
	}
	hostModelExecute = func(r executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		t.Fatalf("no host fallback expected, model=%s", r.Model)
		return pluginapi.HostModelExecutionResponse{}, nil
	}

	if _, err := executeResponsesPassthrough(responsesTestRequest("shared-a")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected exactly two attempts, got [%s]", attemptHosts(attempts))
	}
	if !strings.HasPrefix(attempts[0].host, "https://alpha.example") {
		t.Fatalf("first attempt = %+v, want alpha", attempts[0])
	}
	if !strings.HasPrefix(attempts[1].host, "https://beta.example") || attempts[1].auth != "Bearer key-b1" {
		t.Fatalf("a pool-level responses event must hand off to the next provider, got %+v", attempts[1])
	}
}

// key 级速率拒绝不禁用任何 key，所以本家可用凭据数不会自己减少。没有连击上限时，一次池级速率风
// 暴会把这一家所有 key 撞一遍；到阈值后只在本请求内放弃这一家，且绝不写跨请求的 skip 集。
func TestExecuteResponsesPassthroughStreamKeyLevelRateLimitStreakBailsWithinRequest(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"vendor/Shared-Model"}
	setTestResolvedThreeKeyAlpha()

	origStream := hostHTTPDoStream
	origModelStream := hostModelExecuteStream
	origRead := hostHTTPStreamRead
	t.Cleanup(func() {
		hostHTTPDoStream = origStream
		hostModelExecuteStream = origModelStream
		hostHTTPStreamRead = origRead
	})

	var attempts []attemptRecord
	hostHTTPDoStream = func(req pluginapi.HTTPRequest, _ string) (hostHTTPStreamResponse, error) {
		attempts = append(attempts, recordAttempt(req))
		return hostHTTPStreamResponse{StatusCode: 200, StreamID: "up-managed"}, nil
	}
	hostHTTPStreamRead = func(string) (hostStreamReadResponse, error) {
		return hostStreamReadResponse{Payload: []byte(keyLevelResponsesEvent + "\n\n"), Done: true}, nil
	}
	fallbackCalled := false
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		fallbackCalled = true
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "host-1"}, nil
	}
	origModelRead := hostModelStreamRead
	t.Cleanup(func() { hostModelStreamRead = origModelRead })
	hostModelStreamRead = func(string) (pluginapi.HostModelStreamReadResponse, error) {
		return pluginapi.HostModelStreamReadResponse{Payload: []byte("data: ok"), Done: true}, nil
	}

	if _, err := executeResponsesPassthroughStream(responsesStreamTestRequest("shared-a")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	alpha := 0
	beta := 0
	for _, a := range attempts {
		if strings.HasPrefix(a.host, "https://alpha.example") {
			alpha++
		} else {
			beta++
		}
	}
	if alpha != responsesKeyRateLimitBailAfter {
		t.Fatalf("alpha attempts = %d, want the streak cap %d: [%s]", alpha, responsesKeyRateLimitBailAfter, attemptHosts(attempts))
	}
	if beta != 1 {
		t.Fatalf("beta attempts = %d, want 1 (it has a single key): [%s]", beta, attemptHosts(attempts))
	}
	if !fallbackCalled {
		t.Fatal("host stream fallback expected once managed providers are done for this request")
	}
	if store.IsProviderSkipped("alpha") {
		t.Fatal("the streak cap is request-scoped and must not latch a cross-request provider skip")
	}
}
