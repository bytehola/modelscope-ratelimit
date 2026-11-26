package ratelimit

import (
	"encoding/json"
	"testing"
	"time"
)

// 本文件守护的是"线上帧"契约，而不是结构体字段本身。
//
// 宿主的拦截请求是用 encoding/json 序列化 pluginapi 结构体得到的，其中响应体字段是
// []byte，编码结果是 base64 字符串。如果插件侧把这些字段声明成 string，就会把 base64
// 密文当成明文做子串匹配，所有判据静默失效。下面的测试刻意走一遍 json.Marshal →
// json.Unmarshal 往返，让这类回归无处可藏。

// hostResponseInterceptFrame 复刻宿主 pluginapi.ResponseInterceptRequest 的线上形状：
// 承载响应体的字段是 []byte，序列化后变成 base64。
type hostResponseInterceptFrame struct {
	SourceFormat    string              `json:"SourceFormat"`
	Model           string              `json:"Model"`
	RequestedModel  string              `json:"RequestedModel"`
	Stream          bool                `json:"Stream"`
	RequestHeaders  map[string][]string `json:"RequestHeaders,omitempty"`
	ResponseHeaders map[string][]string `json:"ResponseHeaders,omitempty"`
	OriginalRequest []byte              `json:"OriginalRequest,omitempty"`
	RequestBody     []byte              `json:"RequestBody,omitempty"`
	Body            []byte              `json:"Body,omitempty"`
	StatusCode      int                 `json:"StatusCode"`
	Metadata        map[string]any      `json:"Metadata,omitempty"`
}

// hostStreamChunkFrame 复刻宿主 pluginapi.StreamChunkInterceptRequest 的线上形状。
// HistoryChunks 是 [][]byte，每项各自 base64。
type hostStreamChunkFrame struct {
	RequestID       string              `json:"RequestID"`
	SourceFormat    string              `json:"SourceFormat"`
	Model           string              `json:"Model"`
	RequestedModel  string              `json:"RequestedModel"`
	RequestHeaders  map[string][]string `json:"RequestHeaders,omitempty"`
	ResponseHeaders map[string][]string `json:"ResponseHeaders,omitempty"`
	OriginalRequest []byte              `json:"OriginalRequest,omitempty"`
	RequestBody     []byte              `json:"RequestBody,omitempty"`
	Body            []byte              `json:"Body,omitempty"`
	HistoryChunks   [][]byte            `json:"HistoryChunks,omitempty"`
	ChunkIndex      int                 `json:"ChunkIndex"`
	Metadata        map[string]any      `json:"Metadata,omitempty"`
}

// decodeResponseFrame 模拟插件入口：宿主字节帧 -> 插件视角的请求结构。
func decodeResponseFrame(t *testing.T, frame hostResponseInterceptFrame) ResponseInterceptRequest {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal host frame: %v", err)
	}
	var req ResponseInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal into plugin struct: %v", err)
	}
	return req
}

// decodeStreamFrame 模拟插件入口：宿主字节帧 -> 插件视角的分块结构。
func decodeStreamFrame(t *testing.T, frame hostStreamChunkFrame) StreamChunkInterceptRequest {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal host frame: %v", err)
	}
	var req StreamChunkInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal into plugin struct: %v", err)
	}
	return req
}

// 线上帧里的 Body 必须是明文，判据才能命中。
func TestWireFrameBodyDecodesToPlaintext(t *testing.T) {
	req := decodeResponseFrame(t, hostResponseInterceptFrame{
		SourceFormat:   "openai-response",
		Model:          "ZhipuAI/GLM-5.2",
		RequestedModel: "glm-5.2",
		StatusCode:     200,
		Body:           []byte(responsesRateLimitBody),
		Metadata:       map[string]any{"auth_id": "auth-1"},
	})

	if got, want := string(req.Body), responsesRateLimitBody; got != want {
		t.Fatalf("decoded Body = %q, want the original plaintext", got)
	}
	if !IsResponsesRateLimitExceeded(string(req.Body)) {
		t.Fatal("rate_limit_exceeded must be detected on a real host frame")
	}
}

// 非流式：真实帧必须触发冷却，证明拦截器路径端到端可用。
func TestOnResponseAppliesCooldownFromWireFrame(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 0.1
	cfg.SkipProviderOn429 = false
	s.Reconfigure(cfg)

	req := decodeResponseFrame(t, hostResponseInterceptFrame{
		Model:          "Qwen-72B",
		RequestedModel: "Qwen-72B",
		StatusCode:     200,
		Body:           []byte(responsesRateLimitBody),
		Metadata:       map[string]any{"auth_id": "auth-1"},
	})
	resp := s.OnResponse(req)

	if len(resp.Body) != 0 || len(resp.Headers) != 0 {
		t.Fatal("response interceptor must stay a no-op on the wire path")
	}
	if s.RateLimitCount() != 1 {
		t.Fatalf("RateLimitCount = %d, want 1", s.RateLimitCount())
	}
	if s.cooldownWaitDuration(now) <= 0 {
		t.Fatal("cooldown must be set from a real host frame")
	}
}

// 流式：header-init 帧携带完整请求体，payload 帧携带分块正文，两者都必须解成明文。
func TestOnStreamChunkAppliesCooldownFromWireFrame(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 0.1
	cfg.SkipProviderOn429 = false
	s.Reconfigure(cfg)

	sseChunk := "event: response.failed\n" +
		"data: " + responsesRateLimitBody + "\n\n"

	req := decodeStreamFrame(t, hostStreamChunkFrame{
		RequestID:       "req-stream-1",
		Model:           "Qwen-72B",
		RequestedModel:  "Qwen-72B",
		OriginalRequest: []byte(`{"model":"Qwen-72B","stream":true}`),
		Body:            []byte(sseChunk),
		HistoryChunks:   [][]byte{[]byte("data: {\"type\":\"response.created\"}\n\n")},
		ChunkIndex:      0,
		Metadata:        map[string]any{"auth_id": "auth-1"},
	})

	if got := string(req.Body); got != sseChunk {
		t.Fatalf("decoded stream Body = %q, want %q", got, sseChunk)
	}
	if req.RequestID != "req-stream-1" {
		t.Fatalf("decoded RequestID = %q", req.RequestID)
	}
	if got := string(req.OriginalRequest); got != `{"model":"Qwen-72B","stream":true}` {
		t.Fatalf("decoded OriginalRequest = %q", got)
	}
	if len(req.HistoryChunks) != 1 {
		t.Fatalf("HistoryChunks length = %d, want 1", len(req.HistoryChunks))
	}
	if got := string(req.HistoryChunks[0]); got != "data: {\"type\":\"response.created\"}\n\n" {
		t.Fatalf("decoded history chunk = %q", got)
	}

	s.OnStreamChunk(req)
	if s.RateLimitCount() != 1 {
		t.Fatalf("RateLimitCount = %d, want 1", s.RateLimitCount())
	}
	if s.cooldownWaitDuration(now) <= 0 {
		t.Fatal("cooldown must be set from a real stream frame")
	}
}

// 池级判据同样必须能穿透 base64：它对 "insufficient_quota" 做子串匹配。
//
// 注意 OnResponse 的这条分支由 IsResponsesRateLimitExceeded 守卫（即必须先判定为速率
// 拒绝），池级只是在那之后进一步区分"摘家还是只冷却这一把 key"。所以这里分两步断言：
// 帧解出来的明文能被池级判据识别，再交给 ApplyResponsesRateLimit 才会摘家。
func TestOnResponsePoolLevelSkipFromWireFrame(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	poolBody := `{"type":"response.failed","response":{"status":"failed","error":{"code":"insufficient_quota","message":"quota exhausted"}}}`
	req := decodeResponseFrame(t, hostResponseInterceptFrame{
		Model:          "Qwen-72B",
		RequestedModel: "Qwen-72B",
		StatusCode:     200,
		Body:           []byte(poolBody),
		Metadata: map[string]any{
			"auth_id":  "auth-1",
			"provider": "openai-compatible-modelscope",
		},
	})

	if !IsPoolLevelQuotaExhausted(string(req.Body)) {
		t.Fatal("pool-level insufficient_quota must survive the base64 decode")
	}
	s.ApplyResponsesRateLimit(
		AuthIDFromMetadata(req.Metadata),
		ProviderFromMetadata(req.Metadata),
		req.RequestedModel,
		string(req.Body),
		now,
	)
	if !s.IsProviderSkipped("modelscope") {
		t.Fatal("pool-level insufficient_quota must skip the provider on the wire path")
	}
}

// 429 + insufficient balance 走的是同一条 OnResponse 入口，也要能穿透 base64。
func TestOnResponseInsufficientBalanceFromWireFrame(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	req := decodeResponseFrame(t, hostResponseInterceptFrame{
		Model:      "Qwen-72B",
		StatusCode: 429,
		Body:       []byte(insufficientBalanceBody),
		Metadata:   map[string]any{"auth_id": "auth-1"},
	})

	s.OnResponse(req)
	if !s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("429 + insufficient balance must disable the credential on the wire path")
	}
}

// 空的线上帧不得 panic，也不得产生任何状态。
func TestOnResponseEmptyWireFrame(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	req := decodeResponseFrame(t, hostResponseInterceptFrame{})
	if req.Body != nil {
		t.Fatalf("omitted Body must decode to nil, got %q", req.Body)
	}
	resp := s.OnResponse(req)
	if len(resp.Body) != 0 {
		t.Fatal("empty frame must produce a no-op result")
	}
	if s.RateLimitCount() != 0 {
		t.Fatalf("RateLimitCount = %d, want 0", s.RateLimitCount())
	}
}
