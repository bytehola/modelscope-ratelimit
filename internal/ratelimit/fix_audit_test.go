package ratelimit

import (
	"testing"
	"time"
)

// 修复点 2：Status() 必须能跨午夜仍然报告因 401 被禁用的 key，与 isDisabled() 保持一致。以前
// Status() 只用 active() 判断，日切换之后就漏掉了这种持续到重启为止的 unauthorized 禁用。
func TestFixStatusReportsCrossMidnightUnauthorized(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	night := time.Date(2026, 7, 8, 23, 59, 0, 0, loc)
	s, cur := newTestStore(night)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 401, Body: `{"error":"unauthorized"}`},
	})
	if st := s.Status(); len(st) != 1 || st[0].Global == nil {
		t.Fatalf("today: Status() must report the 401 key, got %+v", st)
	}

	// 跨过午夜（Status() 使用内部时钟）。
	*cur = time.Date(2026, 7, 9, 0, 1, 0, 0, loc)
	if !s.isDisabled("auth-1", "Qwen-72B", *cur) {
		t.Fatal("isDisabled must still report the 401 key after midnight")
	}
	if st := s.Status(); len(st) != 1 || st[0].Global == nil {
		t.Fatalf("after midnight: Status() must still report the 401 key, got %+v", st)
	}
}

// 修复点 3：没有 providerOrderFetcher 时，429 重试（候选列表因为宿主剔除了已试过的 key 而变
// 短）必须在记忆中的宿主顺序里选下一把 key，而不是在变短的列表上任意取一个下标。
func TestFixFallbackRetryPicksNextInOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "modelscope"))
	// 不调用 SetProviderOrderFetcher，走 pickManaged 的兜底路径。

	fullReq := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "a-1", Provider: "modelscope", Priority: 1},
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			{ID: "c-3", Provider: "modelscope", Priority: 1},
		},
	}
	r1, err := s.SchedulerPick(fullReq)
	if err != nil || r1.AuthID != "a-1" {
		t.Fatalf("fresh pick: expected a-1, got %+v err=%v", r1, err)
	}
	// a-1 返回 429：宿主重试时已把 a-1 从候选中剔除。
	retryReq := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			{ID: "c-3", Provider: "modelscope", Priority: 1},
		},
	}
	r2, err := s.SchedulerPick(retryReq)
	if err != nil {
		t.Fatalf("retry pick failed: %v", err)
	}
	// 宿主顺序里 a-1 之后的下一把是 b-2，而不是 c-3 —— 旧的 cursor % len(变短的列表) 的 bug
	// 会算出 c-3。
	if r2.AuthID != "b-2" {
		t.Fatalf("retry pick: expected b-2 (next in order), got %q", r2.AuthID)
	}
}
