package ratelimit

import "testing"

// TestIsResponsesErrorPayloadClassifiesBodies covers the discriminator the
// Responses executor needs when an upstream answers HTTP 200: a real Response
// resource (which legitimately carries "error":null) must stay a success, while
// a bare error document must not be handed to the client as one.
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

// TestIsResponsesErrorPayloadFlagsFailedResponseObject keeps the ordering
// honest: a response-shaped body whose error member actually carries a payload
// is still an error, even though the object type looks like a Response.
func TestIsResponsesErrorPayloadFlagsFailedResponseObject(t *testing.T) {
	body := `{"object":"response","status":"failed","error":{"code":"server_error","message":"boom"}}`
	if !IsResponsesErrorPayload(body) {
		t.Fatalf("error payload should be flagged: %s", body)
	}
}
