package ratelimit

import (
	"testing"
	"time"
)

// TestRateLimitCountIncrementsOn429 verifies that the rateLimitCount counter
// increments when a managed provider returns 429.
func TestRateLimitCountIncrementsOn429(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	// No 429 yet.
	if n := s.RateLimitCount(); n != 0 {
		t.Fatalf("initial count = %d, want 0", n)
	}

	// 429 + insufficient_quota from a managed provider.
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if n := s.RateLimitCount(); n != 1 {
		t.Fatalf("after 1st 429: count = %d, want 1", n)
	}

	// Another 429 from a different key.
	s.OnUsage(UsageRecord{
		AuthID: "auth-2", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if n := s.RateLimitCount(); n != 2 {
		t.Fatalf("after 2nd 429: count = %d, want 2", n)
	}
}

// TestRateLimitCountInsufficientBalanceAlsoCounts verifies that 429 +
// "insufficient balance" also increments the counter (it's still a 429).
func TestRateLimitCountInsufficientBalanceAlsoCounts(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: insufficientBalanceBody},
	})
	if n := s.RateLimitCount(); n != 1 {
		t.Fatalf("after insufficient balance 429: count = %d, want 1", n)
	}
}

// TestRateLimitCountNonManaged429DoesNotCount verifies that 429 from a
// non-managed provider does NOT increment the counter.
func TestRateLimitCountNonManaged429DoesNotCount(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		AuthID: "auth-aliyun", Provider: "aliyun", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if n := s.RateLimitCount(); n != 0 {
		t.Fatalf("non-managed 429: count = %d, want 0", n)
	}
}

// TestRateLimitCount401DoesNotCount verifies that 401 responses do NOT
// increment the 429 counter.
func TestRateLimitCount401DoesNotCount(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 401, Body: `{"error":"unauthorized"}`},
	})
	if n := s.RateLimitCount(); n != 0 {
		t.Fatalf("401: count = %d, want 0", n)
	}
}

// TestRateLimitCountSuccessDoesNotCount verifies that successful (non-failed)
// responses do NOT increment the 429 counter.
func TestRateLimitCountSuccessDoesNotCount(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: false,
	})
	if n := s.RateLimitCount(); n != 0 {
		t.Fatalf("success: count = %d, want 0", n)
	}
}

// TestRateLimitCountResetsAtMidnight verifies that the counter resets at the
// daily boundary.
func TestRateLimitCountResetsAtMidnight(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	night := time.Date(2026, 7, 8, 23, 59, 0, 0, loc)
	s, cur := newTestStore(night)
	s.Reconfigure(cfgWith("modelscope"))
	// Prime cooldownStatsDay so PruneAll detects the day change.
	s.PruneAll(night)

	// Generate two 429s.
	for i := 0; i < 2; i++ {
		s.OnUsage(UsageRecord{
			AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
			Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
		})
	}
	if n := s.RateLimitCount(); n != 2 {
		t.Fatalf("before midnight: count = %d, want 2", n)
	}

	// Cross midnight.
	*cur = time.Date(2026, 7, 9, 0, 1, 0, 0, loc)
	s.PruneAll(*cur)

	if n := s.RateLimitCount(); n != 0 {
		t.Fatalf("after midnight: count = %d, want 0", n)
	}
}
