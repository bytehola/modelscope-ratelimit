package ratelimit

import "testing"

// 线上真实样本：Modelscope 把输入内容审查的拒绝塞进 HTTP 200 + response.failed，
// 并且 error.code 被降级成 server_error，所以判据只能落在 code/message 的特征串上。
const inputInspectionEvent = `data: {"response":{"created_at":1788435242,"error":{"code":"server_error","message":"<400> InternalError.Algo.DataInspectionFailed: Input text data may contain inappropriate content."},"id":"resp_3eb2d055","model":"Qwen/Qwen3.8-Flash-Next","object":"response","output":[],"status":"failed"},"sequence_number":0,"type":"response.failed"}`

func TestIsInputContentInspectionFailed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty", "", false},
		{"real sample event", inputInspectionEvent, true},
		{"bare error document", `{"code":"DataInspectionFailed","message":"Input text data may contain inappropriate content."}`, true},
		{"underscore code", `{"error":{"code":"data_inspection_failed","message":"blocked"}}`, true},
		{"message only", `{"error":{"code":"server_error","message":"input may contain Inappropriate Content."}}`, true},
		{"upstream code in plain text", `InternalError.Algo.DataInspectionFailed`, true},
		// 输出侧被拦下不完全是请求的问题：换一把 key 重新生成有可能过审，仍按轮换处理。
		{"output side inspection", `{"error":{"code":"server_error","message":"<400> InternalError.Algo.DataInspectionFailed: Output text data may contain inappropriate content."}}`, false},
		// 这些失败靠轮换确实能救，绝不能被当成请求侧问题。
		{"key level rate limit", responsesRateLimitBody, false},
		{"burst rate limit", responsesRateLimitBurstBody, false},
		{"pool level quota", `{"type":"response.failed","response":{"error":{"code":"insufficient_quota","message":"quota exceeded"}}}`, false},
		{"generic failed", `{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsInputContentInspectionFailed(tc.body); got != tc.want {
				t.Fatalf("IsInputContentInspectionFailed(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestResponsesFailureMessage(t *testing.T) {
	const wantMsg = "<400> InternalError.Algo.DataInspectionFailed: Input text data may contain inappropriate content."
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty", "", ""},
		{"sse response event", inputInspectionEvent, wantMsg},
		{"event line before data", "event: response.failed\n" + inputInspectionEvent + "\n\n", wantMsg},
		{"bare document", `{"code":"DataInspectionFailed","message":"nope"}`, "nope"},
		{"openai error object", `{"error":{"type":"invalid_request_error","message":"  spaced  "}}`, "spaced"},
		{"response without error", `{"type":"response.created","response":{"id":"resp_1"}}`, ""},
		{"not json", `plain text failure`, ""},
		{"json without message", `{"error":{"code":"server_error"}}`, ""},
		{"trailing frames", inputInspectionEvent + "\n\ndata: {\"type\":\"other\"}\n\n", wantMsg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResponsesFailureMessage(tc.body); got != tc.want {
				t.Fatalf("ResponsesFailureMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestIsInputContentInspectionFailedIsMarkerOnly 钉住判据的前置条件：它只看特征串，不判断
// 响应本身是否失败。调用方必须只在已经判定失败的分支上用它，否则模型自己生成的这些字样会被
// 当成"请求被审查拦下"。
func TestIsInputContentInspectionFailedIsMarkerOnly(t *testing.T) {
	completed := `{"id":"resp_1","object":"response","status":"completed","error":null,"output":[{"content":[{"type":"text","text":"the word inappropriate content showed up in the answer"}]}]}`
	if !IsInputContentInspectionFailed(completed) {
		t.Fatal("the marker match is intentionally body-only")
	}
	// 关键护栏：这段响应不会被送进判据，因为它不是一次失败响应。
	if IsResponsesFailed(completed) || IsResponsesRateLimitExceeded(completed) || IsResponsesErrorPayload(completed) {
		t.Fatal("a completed response must not be classified as a failure by any failure probe")
	}
}
