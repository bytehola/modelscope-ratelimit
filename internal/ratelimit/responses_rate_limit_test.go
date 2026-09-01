package ratelimit

import (
	"testing"
	"time"
)

const responsesRateLimitBurstBody = `{"response":{"created_at":1787753225,"error":{"code":"rate_limit_exceeded","message":"Request rate increased too quickly. To ensure system stability, please adjust your client logic to scale requests more smoothly over time."},"id":"resp_b8609720-18ba-9452-a0c5-71fb8aaea5a7","model":"ZhipuAI/GLM-5.2","object":"response","output":[],"parallel_tool_calls":false,"status":"failed","tool_choice":"auto","tools":[]},"sequence_number":0,"type":"response.failed"}`
const responsesRateLimitBody = `{"response":{"created_at":1787730297,"error":{"code":"rate_limit_exceeded","message":"Allocated quota exceeded, please increase your quota limit."},"id":"resp_1","model":"ZhipuAI/GLM-5.2","object":"response","output":[],"status":"failed"},"sequence_number":0,"type":"response.failed"}`

func TestIsResponsesRateLimitExceededBurstRate(t *testing.T) {
	if !IsResponsesRateLimitExceeded(responsesRateLimitBurstBody) {
		t.Fatal("expected burst rate_limit_exceeded response.failed to be detected")
	}
}

func TestIsResponsesRateLimitExceeded(t *testing.T) {
	if !IsResponsesRateLimitExceeded(responsesRateLimitBody) {
		t.Fatal("expected response.failed/rate_limit_exceeded to be detected")
	}
	if !IsResponsesRateLimitExceeded(`{"type": "response.failed", "error": {"code": "rate_limit_exceeded"}}`) {
		t.Fatal("spaced JSON response.failed/rate_limit_exceeded must be detected")
	}
	if IsResponsesRateLimitExceeded(`{"type":"response.completed"}`) {
		t.Fatal("must not detect non-failure events")
	}
	if IsResponsesRateLimitExceeded("") {
		t.Fatal("empty body must not be detected")
	}
}

func TestIsResponsesFailed(t *testing.T) {
	if !IsResponsesFailed(responsesRateLimitBody) {
		t.Fatal("rate_limit_exceeded response.failed must be detected as response.failed")
	}
	if !IsResponsesFailed(responsesRateLimitBurstBody) {
		t.Fatal("burst rate_limit_exceeded response.failed must be detected")
	}
	if !IsResponsesFailed(`{"type":"response.failed","error":{"code":"InvalidParameter"}}`) {
		t.Fatal("non-rate-limit response.failed must be detected")
	}
	if !IsResponsesFailed(`data: {"type": "response.failed", "error": {"code": "InvalidParameter"}}`) {
		t.Fatal("SSE-framed spaced response.failed must be detected")
	}
	if IsResponsesFailed(`{"type":"response.completed"}`) {
		t.Fatal("response.completed must not be detected")
	}
	if IsResponsesFailed("") {
		t.Fatal("empty body must not be detected")
	}
}

func TestResponsesFirstDataEventLeadingBlankLines(t *testing.T) {
	body := "\n\n\n\n\n\ndata: {\"type\":\"response.failed\",\"error\":{\"code\":\"InvalidParameter\"}}\n\n"
	event, ok := ResponsesFirstDataEvent(body, false)
	if !ok {
		t.Fatal("leading blank lines followed by a complete data event must be detected")
	}
	if !IsResponsesFailed(event) {
		t.Fatalf("detection window must contain response.failed: %q", event)
	}
}

func TestResponsesFirstDataEventWaitsForDispatch(t *testing.T) {
	body := "\n\n\n\n\n\ndata: {\"type\":\"response.failed\""
	if _, ok := ResponsesFirstDataEvent(body, false); ok {
		t.Fatal("partial data event must not be considered complete before dispatch boundary")
	}
	event, ok := ResponsesFirstDataEvent(body, true)
	if !ok {
		t.Fatal("partial data event at EOF must be considered complete")
	}
	if !IsResponsesFailed(event) {
		t.Fatalf("EOF detection window must contain response.failed: %q", event)
	}
}

func TestResponsesFirstDataEventCRLF(t *testing.T) {
	body := "event: response.failed\r\ndata: {\"type\":\"response.failed\"}\r\n\r\n"
	event, ok := ResponsesFirstDataEvent(body, false)
	if !ok {
		t.Fatal("CRLF data event must be detected")
	}
	if !IsResponsesFailed(event) {
		t.Fatalf("detection window must contain response.failed: %q", event)
	}
}

func TestOnStreamChunkResponsesRateLimitCooldown(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 0.1
	cfg.SkipProviderOn429 = false
	s.Reconfigure(cfg)

	s.OnStreamChunk(StreamChunkInterceptRequest{
		ChunkIndex:     0,
		Model:          "Qwen-72B",
		RequestedModel: "Qwen-72B",
		Body:           responsesRateLimitBody,
		Metadata:       map[string]any{"auth_id": "auth-1"},
	})

	if s.RateLimitCount() != 1 {
		t.Fatalf("RateLimitCount = %d, want 1", s.RateLimitCount())
	}
	wait := s.cooldownWaitDuration(now)
	if wait <= 0 {
		t.Fatal("expected cooldown to be set after responses rate_limit_exceeded")
	}

	// 随后的用量钩子不能把这趟 200 的流当成健康响应。
	s.OnUsage(UsageRecord{
		Provider: "openai-compatible-modelscope",
		AuthID:   "auth-1",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
	})
	if wait = s.cooldownWaitDuration(now); wait <= 0 {
		t.Fatal("cooldown was cleared by the 200 usage hook")
	}
	if s.SuccessStats() != 0 {
		t.Fatal("must not count a responses failure as success")
	}
}

// Responses 以 200 + response.failed(rate_limit_exceeded) 回送的拒绝只代表这一把 key，不允许把整家摘掉：
// 否则请求会在第一家还剩大量可用 key 时提前跳到下一家，而下一家应答的是另一个上游模型。
func TestOnStreamChunkResponsesRateLimitKeyLevelDoesNotSkipProvider(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	s.OnStreamChunk(StreamChunkInterceptRequest{
		ChunkIndex:     0,
		Model:          "Qwen-72B",
		RequestedModel: "Qwen-72B",
		Body:           responsesRateLimitBody,
		Metadata: map[string]any{
			"auth_id":  "auth-1",
			"provider": "openai-compatible-modelscope",
		},
	})

	if s.IsProviderSkipped("modelscope") {
		t.Fatal("a key-level responses rate_limit_exceeded must not skip the whole provider")
	}
	if s.RateLimitCount() != 1 {
		t.Fatalf("RateLimitCount = %d, want 1", s.RateLimitCount())
	}
	// 这把 key 也不能被日禁用：速率窗口自己会恢复，本家其余 key 照常轮换。
	if s.IsKeyDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("key-level responses rate limit must not disable the credential for the day")
	}
}

// 池级信号（body 里带 "insufficient_quota"）才有资格触发 skip_provider_on_429。
func TestApplyResponsesRateLimitPoolLevelSkipsProvider(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	body := `{"type":"response.failed","response":{"status":"failed","error":{"code":"insufficient_quota"}}}`
	if !IsResponsesFailed(body) || !IsPoolLevelQuotaExhausted(body) {
		t.Fatal("fixture must be a pool-level response.failed")
	}
	s.ApplyResponsesRateLimit("auth-1", "openai-compatible-modelscope", "Qwen-72B", body, now)
	if !s.IsProviderSkipped("modelscope") {
		t.Fatal("pool-level insufficient_quota must skip the provider")
	}

	// 这条 200 的用量钩子不能清空跳过集合。
	s.OnUsage(UsageRecord{
		Provider: "openai-compatible-modelscope",
		AuthID:   "auth-1",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
	})
	if !s.IsProviderSkipped("modelscope") {
		t.Fatal("skip set was cleared by the 200 usage hook")
	}
}

func TestIsPoolLevelQuotaExhausted(t *testing.T) {
	if !IsPoolLevelQuotaExhausted(`{"error":{"code":"insufficient_quota"}}`) {
		t.Fatal("chat 429 insufficient_quota must be pool level")
	}
	if IsPoolLevelQuotaExhausted(responsesRateLimitBody) {
		t.Fatal("rate_limit_exceeded alone is a key-level window, not pool level")
	}
	if IsPoolLevelQuotaExhausted(`{"error":{"message":"the phrase insufficient_quota appears unquoted in prose"}}`) {
		t.Fatal("unquoted prose mention must not match")
	}
}

func TestOnResponseResponsesRateLimitCooldown(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 0.1
	s.Reconfigure(cfg)

	resp := s.OnResponse(ResponseInterceptRequest{
		Model:          "Qwen-72B",
		RequestedModel: "Qwen-72B",
		StatusCode:     200,
		Body:           responsesRateLimitBody,
		Metadata:       map[string]any{"auth_id": "auth-1"},
	})
	if resp.Body != "" || len(resp.Headers) != 0 {
		t.Fatal("response interceptor must return a no-op result")
	}
	wait := s.cooldownWaitDuration(now)
	if wait <= 0 {
		t.Fatal("expected cooldown after responses rate_limit_exceeded")
	}
}
