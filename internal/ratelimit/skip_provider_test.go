package ratelimit

import (
	"errors"
	"testing"
	"time"
)

// skip_provider_on_429 mode: a managed insufficient_quota 429 arms a latching
// flag; every subsequent SchedulerPick skips ALL managed candidates and picks
// non-managed (fast response, no cooldown wait). The flag is latching: it
// stays armed across non-managed failures so all non-managed keys are tried
// consecutively before falling back to managed. Cleared by any 200. The
// managed key that 429'd is NOT daily-disabled (stays available for future
// probes).
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
	if !s.skipManaged.Load() {
		t.Fatal("managed 429 must arm skipManaged")
	}
	if s.isDisabled("m-1", "Qwen-72B", now) {
		t.Fatal("managed 429 key must NOT be daily-disabled in skip_provider_on_429 mode")
	}

	// 2nd pick (retry): flag set -> skip managed, pick non-managed. Flag is
	// latching — NOT consumed.
	r2, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if err != nil || r2.AuthID != "a-1" {
		t.Fatalf("2nd pick (skip managed): expected non-managed a-1, got %+v err=%v", r2, err)
	}
	if !s.skipManaged.Load() {
		t.Fatal("latching flag must stay armed after the skip pick (not consumed)")
	}

	// 3rd pick: flag STILL armed (latching) -> skip managed again, pick
	// non-managed. This is the key difference from one-shot: no interleaving.
	r3, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if err != nil || r3.AuthID != "a-1" {
		t.Fatalf("3rd pick: expected non-managed a-1 (latching flag still armed), got %+v err=%v", r3, err)
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
	if !s.skipManaged.Load() {
		t.Fatal("insufficient_quota 429 must arm skipManaged")
	}

	// Non-insufficient_quota 429 (limit_burst_rate) -> must NOT arm the flag.
	s.skipManaged.Store(false) // reset for next check
	s.OnUsage(UsageRecord{
		AuthID:          "m-2",
		Provider:        "modelscope",
		Model:           "Qwen-72B",
		Alias:           "Qwen-72B",
		Failed:          true,
		Failure:         &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"limit_burst_rate"}}`},
		ResponseHeaders: hdr("0"),
	})
	if s.skipManaged.Load() {
		t.Fatal("limit_burst_rate 429 must NOT arm skipManaged (only insufficient_quota does)")
	}
	// The limit_burst_rate key should be daily-disabled (normal handling).
	if !s.isDisabled("m-2", "Qwen-72B", now) {
		t.Fatal("limit_burst_rate 429 key must be daily-disabled (normal handling, not skip)")
	}
}

// Latching flag: non-managed failure does NOT clear the flag. All non-managed
// keys are tried consecutively before falling back to managed.
func TestSkipProviderLatchingNonManagedFailures(t *testing.T) {
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

	// 1st pick: managed.
	r1, _ := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "b-1", Provider: "aliyun", Priority: 1},
		Candidate{ID: "b-2", Provider: "aliyun", Priority: 1},
	))
	if r1.AuthID != "m-1" {
		t.Fatalf("1st pick: expected m-1, got %s", r1.AuthID)
	}

	// Managed 429 -> arm latching flag.
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})

	// b-1 tried (simulated host retry): candidates shrink to m-2, b-2.
	// Flag is latching -> still skip managed -> pick b-2.
	r2, _ := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "b-2", Provider: "aliyun", Priority: 1},
	))
	if r2.AuthID != "b-2" {
		t.Fatalf("2nd pick: expected non-managed b-2 (latching skip), got %s", r2.AuthID)
	}
	if !s.skipManaged.Load() {
		t.Fatal("flag must stay armed after non-managed pick (latching)")
	}

	// All non-managed exhausted: only managed left -> defer to host.
	r3, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
	))
	if err != nil || r3.Handled {
		t.Fatalf("3rd pick: expected defer (Handled=false) when only managed left, got %+v err=%v", r3, err)
	}
}

// Any 200 (managed or non-managed) clears the latching flag so the next
// request probes the free managed quota again.
func TestSkipProviderAny200ClearsFlag(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	// Arm the flag via managed 429.
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.skipManaged.Load() {
		t.Fatal("flag must be armed after managed 429")
	}

	// Non-managed 200 -> clears flag (prioritise free quota: next request
	// tries managed first).
	s.OnUsage(UsageRecord{
		AuthID:   "a-1",
		Provider: "aliyun",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   false,
	})
	if s.skipManaged.Load() {
		t.Fatal("any 200 must clear the latching flag (non-managed 200 included)")
	}

	// Re-arm, then managed 200 also clears.
	s.SetSkipManaged()
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   false,
	})
	if s.skipManaged.Load() {
		t.Fatal("managed 200 must clear the latching flag")
	}
}

// Non-managed failure does NOT touch the flag (stays armed if previously set).
func TestSkipProviderNonManagedFailureDoesNotClear(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	// Arm the flag.
	s.SetSkipManaged()

	// Non-managed failure (429) -> flag must stay armed.
	s.OnUsage(UsageRecord{
		AuthID:   "a-1",
		Provider: "aliyun",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.skipManaged.Load() {
		t.Fatal("non-managed failure must NOT clear the latching flag")
	}
}

// When all managed candidates are tried (managed==0 in SchedulerPick), the
// latching flag is cleared so the next request starts fresh and probes the
// free managed quota again. This prevents the flag from getting stuck true
// when every candidate (managed + non-managed) fails.
func TestSkipProviderFlagClearedWhenAllManagedTried(t *testing.T) {
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

	// 1st pick: managed m-1.
	r1, _ := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "b-1", Provider: "aliyun", Priority: 1},
	))
	if r1.AuthID != "m-1" {
		t.Fatalf("1st pick: expected m-1, got %s", r1.AuthID)
	}

	// Managed 429 -> arm latching flag.
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.skipManaged.Load() {
		t.Fatal("flag must be armed after managed 429")
	}

	// 2nd pick: m-1 tried -> only non-managed left -> managed==0 -> flag
	// cleared, defer to host.
	r2, err := s.SchedulerPick(mkReq(
		Candidate{ID: "b-1", Provider: "aliyun", Priority: 1},
	))
	if err != nil || r2.Handled {
		t.Fatalf("2nd pick: expected defer (Handled=false), got %+v err=%v", r2, err)
	}
	if s.skipManaged.Load() {
		t.Fatal("flag must be cleared when all managed candidates are tried (managed==0)")
	}
}

// When skip flag is armed and the route has NO non-managed provider, the
// plugin fails fast (returns ErrAllCredentialsDisabled) instead of falling
// through to try remaining managed keys (which would all 429 on the same
// pool). The flag is cleared so the next request probes managed again.
func TestSkipProviderNoNonManagedFailsFast(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider: "modelscope",
			// No non-managed provider in the route.
			Providers:  []string{"modelscope"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}

	// 1st pick: flag false -> normal managed pick.
	r1, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-3", Provider: "modelscope", Priority: 1},
	))
	if err != nil || r1.AuthID != "m-1" {
		t.Fatalf("1st pick: expected m-1, got %+v err=%v", r1, err)
	}

	// Managed 429 -> arm latching flag.
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.skipManaged.Load() {
		t.Fatal("flag must be armed after managed 429")
	}

	// 2nd pick: flag armed, no non-managed -> fail fast + clear flag.
	_, err = s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-3", Provider: "modelscope", Priority: 1},
	))
	if err == nil {
		t.Fatal("2nd pick: expected error (fail fast, no non-managed fallback)")
	}
	if !errors.Is(err, ErrAllCredentialsDisabled) {
		t.Fatalf("2nd pick: expected ErrAllCredentialsDisabled, got %v", err)
	}
	if s.skipManaged.Load() {
		t.Fatal("flag must be cleared after fail-fast error so next request probes managed")
	}

	// 3rd pick: flag cleared -> normal managed pick again (probe recovery).
	// Round-robin cursor advanced past m-1, so the next managed pick is m-2.
	r3, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
	))
	if err != nil || r3.AuthID != "m-2" {
		t.Fatalf("3rd pick: expected m-2 (flag cleared, probe managed), got %+v err=%v", r3, err)
	}
}
