package ratelimit

import "testing"

// TestIsResponsesErrorPayloadClassifiesBodies 覆盖 Responses 执行器在上游返回 HTTP 200 时需要的
// 判别逻辑：真正的 Response 资源（本来就带 "error":null）必须仍算成功，而裸的错误文档绝不能当作
// 成功响应交给客户端。
func TestIsResponsesErrorPayloadClassifiesBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty", "", false},
		{"completed response", `{"id":"resp_1","object":"response","status":"completed","error":null,"output":[]}`, false},
		{"host translated response", `{"id":"chatcmpl-1","object":"response","error":null,"usage":{"input_tokens":1}}`, false},
		{"response with empty error object", `{"object":"response","error":{}}`, false},
		{"response with null error spaced", `{ "object" : "response", "error" : null }`, false},
		{"dashscope style code message", `{"code":"InvalidParameter","message":"Invalid 'max_output_tokens': must be greater than or equal to 16, got 8.","request_id":"0099"}`, true},
		{"openai style error object", `{"error":{"message":"model does not exist","type":"invalid_request_error","code":"model_not_found"}}`, true},
		{"plain error string", `{"error":"rate_limit"}`, true},
		{"code without message", `{"code":"Throttling"}`, false},
		{"model list", `{"object":"list","data":[{"id":"glm-5.2","object":"model"}]}`, false},
		{"nested code and message only", `{"object":"response","output":[{"content":[{"type":"text","code":"c","message":"m"}]}]}`, false},
		{"not json", `plain text failure`, false},
		{"sse frame", "data: {\"error\":{\"code\":\"x\"}}\n\n", false},
		{"no error tokens at all", `{"id":"resp_2","object":"response","status":"completed","output":[{"type":"message"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsResponsesErrorPayload(tc.body); got != tc.want {
				t.Fatalf("IsResponsesErrorPayload(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestIsResponsesErrorPayloadFlagsFailedResponseObject 保证判断顺序是对的：形状像 response 的
// 响应体只要 error 成员真的带了内容，就仍然是错误，哪怕 object 类型看起来是个 Response。
func TestIsResponsesErrorPayloadFlagsFailedResponseObject(t *testing.T) {
	body := `{"object":"response","status":"failed","error":{"code":"server_error","message":"boom"}}`
	if !IsResponsesErrorPayload(body) {
		t.Fatalf("error payload should be flagged: %s", body)
	}
}
