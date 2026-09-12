package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	modelscope "modelscope-ratelimit/internal/ratelimit"
)

// 上游模型还在加载（503 + model_not_ready）。这是 provider 级可用性拒绝：本家所有 key 都会
// 以同样方式被拒，所以本请求只允许探这一家一次，不能把 3 把 key 挨个撞一遍。
const modelNotReadyResponsesBody = `{"error":{"code":"model_not_ready","message":"模型尚未就绪：8卡加载中","param":null,"type":"model_not_ready"},"request_id":"0c7e7128"}`

// 普通 503（网关/上游抖动）与 key 无关但也可能一次就好：这类仍按常规逐 key 轮换，不摘家。
const plainBadGatewayBody = `{"error":{"code":"internal_server_error","message":"upstream busy"}}`

// runAvailabilityRotation 用给定的上游响应跑一次直连（非流式）透传，返回上游尝试序列。
func runAvailabilityRotation(t *testing.T, statusCode int, body string) []attemptRecord {
	t.Helper()

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
			return hostHTTPResponse{StatusCode: statusCode, Body: []byte(body)}, nil
		}
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-next","object":"response","status":"completed","error":null}`)}, nil
	}
	hostModelExecute = func(r executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
		t.Fatalf("managed providers must serve this request, model=%s", r.Model)
		return pluginapi.HostModelExecutionResponse{}, nil
	}

	if _, err := executeResponsesPassthrough(responsesTestRequest("shared-a")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return attempts
}

func TestExecuteResponsesPassthroughModelNotReadyHandsOffNextProvider(t *testing.T) {
	attempts := runAvailabilityRotation(t, http.StatusServiceUnavailable, modelNotReadyResponsesBody)
	if len(attempts) != 2 {
		t.Fatalf("expected one probe per provider, got [%s]", attemptHosts(attempts))
	}
	if !strings.HasPrefix(attempts[0].host, "https://alpha.example") {
		t.Fatalf("first attempt = %+v, want alpha", attempts[0])
	}
	// 关键断言：alpha 有 3 把 key，但模型未就绪与 key 无关，第二次必须已经在下一家。
	if !strings.HasPrefix(attempts[1].host, "https://beta.example") || attempts[1].auth != "Bearer key-b1" {
		t.Fatalf("503 model_not_ready must drop the whole provider for this request, got %+v", attempts[1])
	}
}

func TestExecuteResponsesPassthroughPlain503StillRotatesKeysWithinProvider(t *testing.T) {
	attempts := runAvailabilityRotation(t, http.StatusBadGateway, plainBadGatewayBody)
	wantSameProviderRetry(t, attempts, 2)
}

// 流式路径同一条判据：503 + model_not_ready 之后本家剩下的 key 步骤必须丢掉。
func TestExecuteResponsesPassthroughStreamModelNotReadyHandsOffNextProvider(t *testing.T) {
	origStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = origStore })

	setTestPluginConfig("alpha", "beta")
	pluginCfg.SkipProviderOn429 = true
	pluginCfg.UseResponses = []string{"vendor/Shared-Model"}
	setTestResolvedThreeKeyAlpha()
	t.Cleanup(func() { store.ClearAllSkippedProviders() })

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
			// 第一家：模型还在加载，非 200 的流在建连阶段就把状态码回回来。
			return hostHTTPStreamResponse{StatusCode: http.StatusServiceUnavailable, StreamID: "up-alpha"}, nil
		}
		return hostHTTPStreamResponse{StatusCode: 200, StreamID: "up-beta"}, nil
	}
	hostHTTPStreamRead = func(streamID string) (hostStreamReadResponse, error) {
		if streamID == "up-alpha" {
			return hostStreamReadResponse{Payload: []byte(modelNotReadyResponsesBody), Done: true}, nil
		}
		return hostStreamReadResponse{Payload: []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-b1\"}}\n\n"), Done: true}, nil
	}
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		t.Fatal("no host stream fallback expected")
		return pluginapi.HostModelStreamResponse{}, nil
	}

	if _, err := executeResponsesPassthroughStream(responsesStreamTestRequest("shared-a")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected one probe per provider, got [%s]", attemptHosts(attempts))
	}
	// alpha 有 3 把 key，但模型未就绪与 key 无关：第二次尝试必须已经在下一家。
	if !strings.HasPrefix(attempts[1].host, "https://beta.example") || attempts[1].auth != "Bearer key-b1" {
		t.Fatalf("503 model_not_ready must drop the whole provider for this request, got %+v", attempts[1])
	}
}
