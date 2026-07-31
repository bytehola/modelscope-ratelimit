package ratelimit

import (
	"testing"
	"time"
)

// skip_provider_on_429 mode: a managed 429 arms a one-shot flag; the next
// SchedulerPick skips ALL managed candidates and jumps to a non-managed
// provider (fast response, no cooldown wait). The flag is consumed by that
// one pick; the next resumes normal managed probing. The managed key that
// 429'd is NOT daily-disabled (stays available for future probes).
func TestSkipProviderOn429JumpsToNonManaged(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider:   "modelscope",
			Providers:  []string{"modelscope", "aliyun"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}

	// 1st pick: no 429 yet -> managed.
	r1, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if err != nil || r1.AuthID != "m-1" {
		t.Fatalf("1st pick: expected managed m-1, got %+v err=%v", r1, err)
	}

	// Managed 429 (insufficient_quota, remaining=0) -> arm the one-shot flag.
	// With SkipProviderOn429, the key must NOT be daily-disabled.
	s.OnUsage(UsageRecord{
		AuthID:          "m-1",
		Provider:        "modelscope",
		Model:           "Qwen-72B",
		Alias:           "Qwen-72B",
		Failed:          true,
		Failure:         &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
		ResponseHeaders: hdr("0"),
	})
	if !s.skipManagedOnce.Load() {
		t.Fatal("managed 429 must arm skipManagedOnce")
	}
	if s.isDisabled("m-1", "Qwen-72B", now) {
		t.Fatal("managed 429 key must NOT be daily-disabled in skip_provider_on_429 mode")
	}

	// 2nd pick (retry): flag set -> skip managed, pick non-managed.
	r2, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if err != nil || r2.AuthID != "a-1" {
		t.Fatalf("2nd pick (skip managed): expected non-managed a-1, got %+v err=%v", r2, err)
	}
	if s.skipManagedOnce.Load() {
		t.Fatal("flag must be consumed after the skip pick")
	}

	// 3rd pick: flag consumed -> back to managed.
	r3, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if err != nil || r3.AuthID != "m-2" {
		t.Fatalf("3rd pick: expected managed m-2 (flag consumed), got %+v err=%v", r3, err)
	}
}

// skip_provider_on_429 arms ONLY on insufficient_quota 429s. Other 429 body
// types (limit_burst_rate, exceeded-today) do NOT arm the flag — they fall
// through to normal daily-disable + cooldown handling.
func TestSkipProviderOn429Any429Body(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	// insufficient_quota 429 -> arm the flag.
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.skipManagedOnce.Load() {
		t.Fatal("insufficient_quota 429 must arm skipManagedOnce")
	}

	// Non-insufficient_quota 429 (limit_burst_rate) -> must NOT arm the flag.
	s.skipManagedOnce.Store(false) // reset for next check
	s.OnUsage(UsageRecord{
		AuthID:          "m-2",
		Provider:        "modelscope",
		Model:           "Qwen-72B",
		Alias:           "Qwen-72B",
		Failed:          true,
		Failure:         &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"limit_burst_rate"}}`},
		ResponseHeaders: hdr("0"),
	})
	if s.skipManagedOnce.Load() {
		t.Fatal("limit_burst_rate 429 must NOT arm skipManagedOnce (only insufficient_quota does)")
	}
	// The limit_burst_rate key should be daily-disabled (normal handling).
	if !s.isDisabled("m-2", "Qwen-72B", now) {
		t.Fatal("limit_burst_rate 429 key must be daily-disabled (normal handling, not skip)")
	}
}
