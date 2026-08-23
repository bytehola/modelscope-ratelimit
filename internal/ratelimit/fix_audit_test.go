package ratelimit

import (
	"testing"
	"time"
)

// Fix #2: Status() must report a 401-disabled key across the midnight
// boundary, matching isDisabled(). Previously Status() only used active()
// and omitted the persistent unauthorized disable after the day rolled over.
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

	// Cross midnight (Status() uses the internal clock).
	*cur = time.Date(2026, 7, 9, 0, 1, 0, 0, loc)
	if !s.isDisabled("auth-1", "Qwen-72B", *cur) {
		t.Fatal("isDisabled must still report the 401 key after midnight")
	}
	if st := s.Status(); len(st) != 1 || st[0].Global == nil {
		t.Fatalf("after midnight: Status() must still report the 401 key, got %+v", st)
	}
}

// Fix #3: without a providerOrderFetcher, a 429 retry (candidate list
// shrinks because the host removed the tried key) must pick the next key in
// the remembered host order, not an arbitrary index of the smaller list.
func TestFixFallbackRetryPicksNextInOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "modelscope"))
	// No SetProviderOrderFetcher -> fallback path in pickManaged.

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
	// 429 on a-1: host retries with a-1 excluded from candidates.
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
	// The next key in host order after a-1 is b-2 (NOT c-3, which the old
	// cursor % len(smaller_list) bug produced).
	if r2.AuthID != "b-2" {
		t.Fatalf("retry pick: expected b-2 (next in order), got %q", r2.AuthID)
	}
}
