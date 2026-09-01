package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	modelscope "modelscope-ratelimit/internal/ratelimit"
)

func attemptKey(req pluginapi.HTTPRequest) string {
	return req.Headers.Get("Authorization")
}

// 这里复现线上抓包看到的缺陷：429 insufficient balance 只让一把 key 失效，
// 因此重试应该继续用同一家下一把 key。直接换到第二家时，第一家明明还有
// 可用 key，请求却会被另一个上游模型应答。
func TestExecuteResponsesPassthroughBalanceQuota429RotatesKeyWithinProvider(t *testing.T) {
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

	type attempt struct {
		url  string
		auth string
	}
	var attempts []attempt
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		attempts = append(attempts, attempt{url: req.URL, auth: attemptKey(req)})
		if len(attempts) == 1 {
			return hostHTTPResponse{StatusCode: 429, Body: []byte(`{"error":{"message":"insufficient balance"}}`)}, nil
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
	if len(attempts) != 2 {
		t.Fatalf("expected two upstream attempts, got %+v", attempts)
	}
	if attempts[0].url != "https://alpha.example/v1/responses" || attempts[0].auth != "Bearer key-a1" {
		t.Fatalf("first attempt = %+v, want alpha/key-a1", attempts[0])
	}
	if attempts[1].url != "https://alpha.example/v1/responses" || attempts[1].auth != "Bearer key-a2" {
		t.Fatalf("the retry must stay inside the provider with its next key, got %+v", attempts[1])
	}
}

// 这个用例守住开关的另一半语义：池级 insufficient_quota 意味着这一家所有 key 都会失败，
// 所以要把这一家剩下的 key 步骤丢掉，交给下一家。
func TestExecuteResponsesPassthroughPoolQuota429HandsOffToNextProvider(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"vendor/Shared-Model"}
	setTestResolvedTwoKeyProviders()
	t.Cleanup(func() { store.ClearAllSkippedProviders() })

	origHTTP := hostHTTPDo
	t.Cleanup(func() { hostHTTPDo = origHTTP })

	var attempts []string
	hostHTTPDo = func(req pluginapi.HTTPRequest, _ string) (hostHTTPResponse, error) {
		attempts = append(attempts, req.URL+" "+attemptKey(req))
		if len(attempts) == 1 {
			return hostHTTPResponse{StatusCode: 429, Body: []byte(`{"error":{"code":"insufficient_quota"}}`)}, nil
		}
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-b1"}`)}, nil
	}

	if _, err := executeResponsesPassthrough(responsesTestRequest("shared-a")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected exactly two attempts, got %v", attempts)
	}
	if !strings.HasPrefix(attempts[0], "https://alpha.example/v1/responses") {
		t.Fatalf("first attempt = %q, want alpha", attempts[0])
	}
	if !strings.HasPrefix(attempts[1], "https://beta.example/v1/responses") || !strings.HasSuffix(attempts[1], "Bearer key-b1") {
		t.Fatalf("a pool-level quota 429 must hand off to the next provider, got %q", attempts[1])
	}
}
