package ratelimit

import (
	"testing"
	"time"
)

// 线上真实样本：上游 vLLM 还在加载模型时回 503 + model_not_ready。这是 provider/模型层面的
// 可用性拒绝，与用哪把 key 无关，本家所有 key 都会以同样的方式被拒。
const modelNotReadyBody = `{"error":{"code":"model_not_ready","message":"模型尚未就绪：🟠 8卡加载中 — 正在使用 TP=8 加载模型。请查看环境诊断和 vLLM 日志。","param":null,"type":"model_not_ready"},"request_id":"0c7e7128-ae90-4ea7-b62f-6cab0647671d"}`

func TestIsModelNotReady(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"real sample 503", 503, modelNotReadyBody, true},
		{"spaced json", 503, `{ "error" : { "code" : "model_not_ready" } }`, true},
		{"type field only", 503, `{"error":{"type":"model_not_ready"}}`, true},
		// 状态码必须写死 503：502/504 多出在网关与链路上，换一把 key 就可能正常，摘家会误伤。
		{"500 same body", 500, modelNotReadyBody, false},
		{"502 same body", 502, modelNotReadyBody, false},
		{"504 same body", 504, modelNotReadyBody, false},
		{"429 same body", 429, modelNotReadyBody, false},
		{"200 same body", 200, modelNotReadyBody, false},
		// token 必须带引号：正文里出现这个词不算。
		{"503 phrase inside longer text", 503, `{"error":{"message":"the model_not_ready code was mentioned"}}`, false},
		{"503 unrelated error", 503, `{"error":{"code":"internal_server_error","message":"boom"}}`, false},
		{"503 empty body", 503, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsModelNotReady(tc.status, tc.body); got != tc.want {
				t.Fatalf("IsModelNotReady(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// 摘家判据把 405 与 503 + model_not_ready 归成一类（都是"这一家服务不了当前请求"）；额度类
// 的池级 insufficient_quota 不走这个判据，它由 IsPoolLevelQuotaExhausted 单独判定。
func TestIsProviderAvailabilityRejection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"405 any body", 405, "", true},
		{"503 model not ready", 503, modelNotReadyBody, true},
		{"503 other code", 503, `{"error":{"code":"ServiceUnavailable"}}`, false},
		{"429 pool quota", 429, `{"error":{"code":"insufficient_quota"}}`, false},
		{"200 ok", 200, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsProviderAvailabilityRejection(tc.status, tc.body); got != tc.want {
				t.Fatalf("IsProviderAvailabilityRejection(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// chat 接口是这条规则的实战场景：宿主自己打上游，插件只在用量钩子里看到结果，所以摘家必须
// 在 OnUsage 里生效。
func TestSkipProviderOn503ModelNotReadyJumpsProvider(t *testing.T) {
	now := time.Date(2026, 9, 12, 21, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "openai-compatible-modelscope",
		Model:    "ZhipuAI/GLM-5.3-Flash",
		Alias:    "glm-5.3-flash",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 503, Body: modelNotReadyBody},
	})
	if !s.IsProviderSkipped("modelscope") {
		t.Fatal("managed 503 model_not_ready must add provider to skip set (same handling as 405)")
	}
	// 模型没加载好不是这把 key 的问题：不禁用、不算限流、不进冷却。
	if s.isDisabled("m-1", "glm-5.3-flash", now) {
		t.Fatal("503 model_not_ready must not daily-disable the key")
	}
	if n := s.RateLimitCount(); n != 0 {
		t.Fatalf("503 model_not_ready must not count as a rate limit, got %d", n)
	}

	// 下一家任意 200 清空 skip 集：模型加载完之后下一次请求就能重新探到这一家。
	s.OnUsage(UsageRecord{
		AuthID:   "my-1",
		Provider: "openai-compatible-modelscope-my",
		Model:    "ZhipuAI/GLM-5.3-Flash",
		Alias:    "glm-5.3-flash",
	})
	if s.IsProviderSkipped("modelscope") {
		t.Fatal("any 200 must clear the skip set so the loading provider gets probed again")
	}
}

// 两个不收 skip 的边界：开关关闭时不摘家（回到逐 key 轮换的老行为）；非托管 provider 回同样的
// 503 也不该被摘——它本来就不在托管池里。
func TestSkipProviderOn503ModelNotReadyBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 12, 21, 0, 0, 0, time.FixedZone("CST", 8*3600))

	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = false
	s.Reconfigure(cfg)
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "openai-compatible-modelscope",
		Model:    "ZhipuAI/GLM-5.3-Flash",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 503, Body: modelNotReadyBody},
	})
	if s.IsProviderSkipped("modelscope") {
		t.Fatal("skip_provider_on_429 off must not skip any provider")
	}

	s2, _ := newTestStore(now)
	cfg2 := cfgWith("modelscope")
	cfg2.SkipProviderOn429 = true
	s2.Reconfigure(cfg2)
	s2.OnUsage(UsageRecord{
		AuthID:   "kp-1",
		Provider: "openai-compatible-快跑ai",
		Model:    "ZhipuAI/GLM-5.3-Flash",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 503, Body: modelNotReadyBody},
	})
	if s2.IsProviderSkipped("快跑ai") {
		t.Fatal("a non-managed provider must never be put in the skip set")
	}
}
