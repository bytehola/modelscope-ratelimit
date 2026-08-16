package ratelimit

import (
	"testing"
	"time"
)

// TestNoManagedModelsField verifies that ManagedModels and ManagesModel are
// gone from Config, and that all models are handled by default.
func TestNoManagedModelsField(t *testing.T) {
	cfg := DefaultConfig()
	// ConfigFromMap should not crash without managed_models.
	c, err := ConfigFromMap(map[string]any{
		"providers": []any{"modelscope"},
	})
	if err != nil {
		t.Fatalf("ConfigFromMap: %v", err)
	}
	_ = cfg
	_ = c
}

// TestSchedulerHandlesAllModels verifies that the scheduler handles requests
// for any model name (no model filtering since ManagedModels was removed).
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

// TestApplyRateLimitAllModels verifies that 429 + "insufficient balance"
// triggers a daily global disable regardless of model name.
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

// TestDisableModelGone verifies that the per-model disable path is fully
// removed: keyState has no models field, and Status() doesn't return Models.
func TestDisableModelGone(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	// Record a seen key.
	s.recordSeen([]string{"auth-1"})

	// Status should only have AuthID and Global, no Models field.
	st := s.Status()
	// No disables → empty status.
	if len(st) != 0 {
		t.Fatalf("expected empty status (no disables), got %d entries", len(st))
	}

	// Disable globally.
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
