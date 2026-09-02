package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	modelscope "modelscope-ratelimit/internal/ratelimit"
)

// 线上样本：客户端内容被上游审查拦下，Responses 以 HTTP 200 + response.failed 回送，
// 且 error.code 被降级成 server_error（流式第一帧就是它，sequence_number 为 0）。
const inputInspectionStreamEvent = `{"response":{"created_at":1788435242,"error":{"code":"server_error","message":"<400> InternalError.Algo.DataInspectionFailed: Input text data may contain inappropriate content."},"id":"resp_3eb2d055","model":"Qwen/Qwen3.8-Flash-Next","object":"response","output":[],"status":"failed"},"sequence_number":0,"type":"response.failed"}`

// 同一种拒绝的非流式外壳：顶层直接是一个 status=failed 的 Response 资源。
const inputInspectionFailedBody = `{"id":"resp_3eb2d055","object":"response","status":"failed","error":{"code":"server_error","message":"<400> InternalError.Algo.DataInspectionFailed: Input text data may contain inappropriate content."},"output":[]}`

// 输出侧被拦下：换一把 key 重新生成有可能过审，所以这类仍按常规轮换，不报错给客户端。
const outputInspectionFailedEvent = `{"response":{"error":{"code":"server_error","message":"<400> InternalError.Algo.DataInspectionFailed: Output text data may contain inappropriate content."},"status":"failed"},"type":"response.failed"}`

// 与内容无关的普通 response.failed：轮换仍然有意义。
const genericResponsesFailedEvent = `{"response":{"error":{"code":"server_error","message":"boom"},"status":"failed"},"type":"response.failed"}`

// wantInputRejectedFault 断言执行器抛出的是"请求级错误"：状态码 400，且 message 是一段
// 合法的 OpenAI 风格错误文档（宿主对合法 JSON 的错误文本会原样透传给客户端）。
func wantInputRejectedFault(t *testing.T, err error) {
	t.Helper()
	var fault *requestFaultError
	if !errors.As(err, &fault) || fault == nil {
		t.Fatalf("expected *requestFaultError, got %T: %v", err, err)
	}
	if fault.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", fault.StatusCode())
	}
	var doc clientErrorDocument
	if errDecode := json.Unmarshal([]byte(fault.message), &doc); errDecode != nil {
		t.Fatalf("client message must be a JSON error document: %v (%s)", errDecode, fault.message)
	}
	if doc.Error.Type != "invalid_request_error" || doc.Error.Code != contentInspectionErrorCode {
		t.Fatalf("client error = %+v, want invalid_request_error/%s", doc.Error, contentInspectionErrorCode)
	}
	if !strings.Contains(doc.Error.Message, "Input text data may contain inappropriate content") {
		t.Fatalf("client message should carry the upstream reason, got %q", doc.Error.Message)
	}
	// 路由细节与凭据绝不进客户端消息。
	if strings.Contains(fault.message, "Bearer") || strings.Contains(fault.message, "key-a1") {
		t.Fatalf("client message leaked credentials: %s", fault.message)
	}
}

func TestExecuteResponsesPassthroughInputInspectionReportsToClient(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"response.failed event", inputInspectionStreamEvent},
		{"failed response object", inputInspectionFailedBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				return hostHTTPResponse{StatusCode: 200, Body: []byte(tc.body)}, nil
			}
			hostModelExecute = func(r executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
				t.Fatalf("a client-side rejection must not fall through to the host, model=%s", r.Model)
				return pluginapi.HostModelExecutionResponse{}, nil
			}

			_, err := executeResponsesPassthrough(responsesTestRequest("shared-a"))
			wantInputRejectedFault(t, err)
			if len(attempts) != 1 {
				t.Fatalf("expected exactly one upstream attempt, got [%s]", attemptHosts(attempts))
			}
			// 请求侧的问题不是凭据的问题：不冷却、不禁用、不摘家。
			if store.IsKeyDisabled("a1", "vendor/Shared-Model", store.Now()) {
				t.Fatal("the rejected key must stay schedulable")
			}
			if store.IsProviderSkipped("alpha") || store.IsProviderSkipped("beta") {
				t.Fatal("a client-side rejection must not skip any provider")
			}
		})
	}
}

func TestExecuteResponsesPassthroughStreamInputInspectionReportsToClient(t *testing.T) {
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
		return hostStreamReadResponse{Payload: []byte("data: " + inputInspectionStreamEvent + "\n\n"), Done: true}, nil
	}
	hostModelExecuteStream = func(_ executorRequestWithCallback) (pluginapi.HostModelStreamResponse, error) {
		t.Fatal("a client-side rejection must not fall through to the host stream")
		return pluginapi.HostModelStreamResponse{}, nil
	}

	_, err := executeResponsesPassthroughStream(responsesStreamTestRequest("shared-a"))
	wantInputRejectedFault(t, err)
	if len(attempts) != 1 {
		t.Fatalf("expected exactly one upstream attempt, got [%s]", attemptHosts(attempts))
	}
	if store.IsKeyDisabled("a1", "vendor/Shared-Model", store.Now()) {
		t.Fatal("the rejected key must stay schedulable")
	}
}

// 只有"输入内容被拦下"才终止轮换。另外两类必须照旧换下一把 key：普通 response.failed
// 与输出侧审查拒绝（换 key 重新生成有可能过审）。
func TestExecuteResponsesPassthroughOtherFailuresStillRotate(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"generic response.failed", genericResponsesFailedEvent},
		{"output side inspection", outputInspectionFailedEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
					return hostHTTPResponse{StatusCode: 200, Body: []byte(tc.body)}, nil
				}
				return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp-a2","object":"response","status":"completed","error":null}`)}, nil
			}
			hostModelExecute = func(_ executorRequestWithCallback) (pluginapi.HostModelExecutionResponse, error) {
				t.Fatal("no host fallback expected")
				return pluginapi.HostModelExecutionResponse{}, nil
			}

			if _, err := executeResponsesPassthrough(responsesTestRequest("shared-a")); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			wantSameProviderRetry(t, attempts, 2)
		})
	}
}

// 请求级错误要带着状态码越过 C-ABI：宿主据此把 400 原样回给客户端，并且不去冷却/轮换
// 其他凭据。普通错误仍然写成无状态码的 plugin_error，行为不变。
func TestPluginErrorEnvelopeKeepsRequestFaultStatus(t *testing.T) {
	fault := inputContentRejectedError(inputInspectionStreamEvent)
	var envelope pluginabi.Envelope
	if errDecode := json.Unmarshal(pluginErrorEnvelope(fault), &envelope); errDecode != nil {
		t.Fatalf("envelope decode: %v", errDecode)
	}
	if envelope.OK || envelope.Error == nil {
		t.Fatalf("want an error envelope, got %s", string(mustJSONMarshal(envelope)))
	}
	if envelope.Error.Code != contentInspectionErrorCode {
		t.Fatalf("envelope code = %q, want %q", envelope.Error.Code, contentInspectionErrorCode)
	}
	if envelope.Error.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("envelope http_status = %d, want 400", envelope.Error.HTTPStatus)
	}

	var plain pluginabi.Envelope
	if errDecode := json.Unmarshal(pluginErrorEnvelope(errors.New("dial failed")), &plain); errDecode != nil {
		t.Fatalf("envelope decode: %v", errDecode)
	}
	if plain.Error == nil || plain.Error.Code != "plugin_error" || plain.Error.HTTPStatus != 0 {
		t.Fatalf("plain errors must keep the old envelope, got %+v", plain.Error)
	}
}
