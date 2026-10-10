package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	modelscope "modelscope-ratelimit/internal/ratelimit"
)

// unverifiedAccountBody 与核心层用例同源：403 表示账号没完成邮箱验证，魔粒不可用。
const unverifiedAccountBody = `{"error":{"message":"For account security, please complete your profile via email verification. ModelScope Magicubes is only available to verified accounts. Please refer to https://modelscope.cn/my/settings/account","request_id":"dadfecb2-6079-4a49-a1b8-922232b6084c"}}`

// setTestResolvedSharedRawKey 让 alpha/a1 与 beta/b1 指向同一把原始 api key，也就是同一个
// Modelscope 账号挂在两家托管 provider 下，用于断言账号级禁用的广播。
func setTestResolvedSharedRawKey() {
	routes := map[string]map[string]string{
		"alpha": {"shared-a": "vendor/Shared-Model"},
		"beta":  {"shared-b": "vendor/Shared-Model"},
	}
	sharedKey := "key-shared"
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		masked:       map[string]string{"a1": maskKey(sharedKey), "b1": maskKey(sharedKey)},
		raw:          map[string]string{"a1": sharedKey, "b1": sharedKey},
		rawToAuthIDs: map[string][]string{sharedKey: {"a1", "b1"}},
		baseURL:      map[string]string{"a1": "https://alpha.example/v1", "b1": "https://beta.example/v1"},
		routeToName:  routes,
		nameToRoute:  withRouteIndex(routes),
		providerOrder: map[string][]string{
			"alpha": {"a1"},
			"beta":  {"b1"},
		},
		at: time.Now(),
	}
	resolvedMu.Unlock()
}

// seedAccountsCache 写入一份账号缓存并返回按 maskedKey 取结果的函数，用完后恢复原缓存。
func seedAccountsCache(t *testing.T, summaries ...accountSummary) func(string) accountSummary {
	t.Helper()
	accountsCacheMu.Lock()
	original := accountsCache
	accountsCache = accountsCacheEntry{results: summaries, at: time.Now()}
	accountsCacheMu.Unlock()
	t.Cleanup(func() {
		accountsCacheMu.Lock()
		accountsCache = original
		accountsCacheMu.Unlock()
	})
	return func(maskedKey string) accountSummary {
		accountsCacheMu.Lock()
		defer accountsCacheMu.Unlock()
		for _, s := range accountsCache.results {
			if s.MaskedKey == maskedKey {
				return s
			}
		}
		t.Fatalf("account %s not found in accounts cache", maskedKey)
		return accountSummary{}
	}
}

// TestUsageHandleUnverifiedAccountDisablesSharedAccountKeys 守住账号级语义：403 + 未验证邮箱
// 不只是禁用当前凭据，还要广播给共享同一原始 api key 的其它 auth ID（同一个账号挂在第二家
// provider 名下会撞回同一条 403），并把账号缓存标成未验证邮箱。
func TestUsageHandleUnverifiedAccountDisablesSharedAccountKeys(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	setTestResolvedSharedRawKey()

	sharedMaskedKey := maskKey("key-shared")
	accountOf := seedAccountsCache(t, accountSummary{
		MaskedKey: sharedMaskedKey,
		Schedule:  "normal",
		Balance:   &balanceJSON{Total: 100, Available: 42, Frozen: 0},
	})

	payload, errMarshal := json.Marshal(modelscope.UsageRecord{
		Provider: "openai-compatible-alpha",
		AuthID:   "a1",
		Model:    "vendor/Shared-Model",
		Alias:    "shared-a",
		Failed:   true,
		Failure:  &modelscope.UsageFailure{StatusCode: 403, Body: unverifiedAccountBody},
	})
	if errMarshal != nil {
		t.Fatalf("marshal usage record: %v", errMarshal)
	}
	if _, err := handleMethod(pluginabi.MethodUsageHandle, payload); err != nil {
		t.Fatalf("usage.handle returned error: %v", err)
	}

	now := store.Now()
	if !store.IsKeyDisabled("a1", "shared-a", now) {
		t.Fatal("the failing credential must be disabled for the day")
	}
	if !store.IsKeyDisabled("b1", "shared-b", now) {
		t.Fatal("an account-level 403 must disable every auth ID sharing the same raw api key")
	}

	account := accountOf(sharedMaskedKey)
	if account.Schedule != "unverified" {
		t.Fatalf("account schedule = %q, want %q", account.Schedule, "unverified")
	}
	// 未验证邮箱不是余额问题，不能把真实余额改写成 0。
	if account.Balance == nil || account.Balance.Available != 42 {
		t.Fatalf("account balance = %+v, want the real available balance 42", account.Balance)
	}
}

// TestUsageHandleInsufficientBalanceKeepsExhaustedSchedule 确认重构成共用广播逻辑之后，魔粒
// 耗尽这条老路径仍然把账号缓存标成 exhausted 并把余额置 0。
func TestUsageHandleInsufficientBalanceKeepsExhaustedSchedule(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	setTestResolvedSharedRawKey()

	sharedMaskedKey := maskKey("key-shared")
	accountOf := seedAccountsCache(t, accountSummary{
		MaskedKey: sharedMaskedKey,
		Schedule:  "normal",
		Balance:   &balanceJSON{Total: 100, Available: 42, Frozen: 0},
	})

	payload, errMarshal := json.Marshal(modelscope.UsageRecord{
		Provider: "openai-compatible-alpha",
		AuthID:   "a1",
		Model:    "vendor/Shared-Model",
		Alias:    "shared-a",
		Failed:   true,
		Failure:  &modelscope.UsageFailure{StatusCode: 429, Body: `{"error":{"message":"insufficient balance"}}`},
	})
	if errMarshal != nil {
		t.Fatalf("marshal usage record: %v", errMarshal)
	}
	if _, err := handleMethod(pluginabi.MethodUsageHandle, payload); err != nil {
		t.Fatalf("usage.handle returned error: %v", err)
	}

	if !store.IsKeyDisabled("b1", "shared-b", store.Now()) {
		t.Fatal("魔粒耗尽仍然要广播给共享同一原始 api key 的其它 auth ID")
	}
	account := accountOf(sharedMaskedKey)
	if account.Schedule != "exhausted" {
		t.Fatalf("account schedule = %q, want %q", account.Schedule, "exhausted")
	}
	if account.Balance == nil || account.Balance.Available != 0 {
		t.Fatalf("account balance = %+v, want exhausted balance 0", account.Balance)
	}
}

// TestExecuteResponsesPassthroughUnverified403RotatesKey 守住本请求内的重试语义：403 + 未验证
// 邮箱是账号级（本插件模型里的 key 级）条件，池里其他账号能服务，所以要继续在本家换下一把
// key，而不是把 403 直接甩给客户端。
func TestExecuteResponsesPassthroughUnverified403RotatesKey(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"vendor/Shared-Model"}
	setTestResolvedTwoKeyProviders()

	origHTTP := hostHTTPDo
	origModel := hostModelExecute
	t.Cleanup(func() { hostHTTPDo = origHTTP; hostModelExecute = origModel })

	var attempts []attemptRecord
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		attempts = append(attempts, recordAttempt(req))
		if len(attempts) == 1 {
			return hostHTTPResponse{StatusCode: 403, Body: []byte(unverifiedAccountBody)}, nil
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
		t.Fatal("an account-level 403 must not put the whole provider in the skip set")
	}
	if !store.IsKeyDisabled("a1", "vendor/Shared-Model", store.Now()) {
		t.Fatal("the unverified credential must be disabled so later requests skip it")
	}
}

// 同一个判定在流式路径上也要成立，那条路径有一份独立的调用点。
func TestExecuteResponsesPassthroughStreamUnverified403RotatesKey(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"vendor/Shared-Model"}
	setTestResolvedTwoKeyProviders()

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
		if len(attempts) == 1 {
			return hostHTTPStreamResponse{StatusCode: 403, StreamID: "up-rejected"}, nil
		}
		return hostHTTPStreamResponse{StatusCode: 200, StreamID: "up-served"}, nil
	}
	hostHTTPStreamRead = func(streamID string) (hostStreamReadResponse, error) {
		if streamID == "up-rejected" {
			return hostStreamReadResponse{Payload: []byte(unverifiedAccountBody), Done: true}, nil
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
	if !store.IsKeyDisabled("a1", "vendor/Shared-Model", store.Now()) {
		t.Fatal("the unverified credential must be disabled so later requests skip it")
	}
}
