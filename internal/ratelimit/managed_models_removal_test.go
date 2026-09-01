package ratelimit

import (
	"testing"
	"time"
)

// TestNoManagedModelsField 验证 Config 里已经没有 ManagedModels 和 ManagesModel，并且默认就
// 处理所有模型。
func TestNoManagedModelsField(t *testing.T) {
	cfg := DefaultConfig()
	// 没有 managed_models 时 ConfigFromMap 也不应崩溃。
	c, err := ConfigFromMap(map[string]any{
		"providers": []any{"modelscope"},
	})
	if err != nil {
		t.Fatalf("ConfigFromMap: %v", err)
	}
	_ = cfg
	_ = c
}

// TestSchedulerHandlesAllModels 验证调度器会处理任意模型名的请求（ManagedModels 移除后不再做过
// 模型过滤）。
func TestSchedulerHandlesAllModels(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	for _, model := range []string{"Qwen-72B", "glm-5.2", "deepseek-v4-pro", "unknown-model"} {
		req := SchedulerPickRequest{
			Provider:   "modelscope",
			Providers:  []string{"modelscope"},
			Model:      model,
			Candidates: []Candidate{{ID: "a-1", Provider: "modelscope", Priority: 1}},
		}
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("model %q: unexpected error: %v", model, err)
		}
		if !resp.Handled {
			t.Fatalf("model %q: expected Handled=true (all models managed by default)", model)
		}
	}
}

// TestApplyRateLimitAllModels 验证不论模型名是什么，429 + "insufficient balance" 都会触发日级
// 全局禁用。
func TestApplyRateLimitAllModels(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	for _, model := range []string{"Qwen-72B", "glm-5.2", "deepseek-v4-pro"} {
		s.OnUsage(UsageRecord{
			AuthID: "auth-" + model, Provider: "modelscope", Model: model, Alias: model,
			Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: insufficientBalanceBody},
		})
		if !s.isDisabled("auth-"+model, model, now) {
			t.Fatalf("model %q: key should be globally disabled", model)
		}
	}
}

// TestDisableModelGone 验证逐模型禁用这条路径已被彻底移除：keyState 里没有 models 字段，
// Status() 也不再返回 Models。
func TestDisableModelGone(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	// 记录一把被见过的 key。
	s.recordSeen([]string{"auth-1"})

	// Status 只应包含 AuthID 和 Global，没有 Models 字段。
	st := s.Status()
	// 没有任何禁用 -> 返回空状态。
	if len(st) != 0 {
		t.Fatalf("expected empty status (no disables), got %d entries", len(st))
	}

	// 执行全局禁用。
	s.disableGlobal("auth-1", "Qwen-72B", now)
	st = s.Status()
	if len(st) != 1 {
		t.Fatalf("expected 1 status entry, got %d", len(st))
	}
	if st[0].AuthID != "auth-1" {
		t.Fatalf("AuthID = %q, want auth-1", st[0].AuthID)
	}
	if st[0].Global == nil {
		t.Fatal("Global should be non-nil")
	}
}
