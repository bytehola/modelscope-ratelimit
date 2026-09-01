package ratelimit

import (
	"testing"
	"time"
)

// TestRateLimitCountIncrementsOn429 验证托管 provider 返回 429 时 rateLimitCount 计数会累加。
func TestRateLimitCountIncrementsOn429(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	// 还没有 429。
	if n := s.RateLimitCount(); n != 0 {
		t.Fatalf("initial count = %d, want 0", n)
	}

	// 托管 provider 返回 429 + insufficient_quota。
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if n := s.RateLimitCount(); n != 1 {
		t.Fatalf("after 1st 429: count = %d, want 1", n)
	}

	// 另一把 key 再返回一次 429。
	s.OnUsage(UsageRecord{
		AuthID: "auth-2", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if n := s.RateLimitCount(); n != 2 {
		t.Fatalf("after 2nd 429: count = %d, want 2", n)
	}
}

// TestRateLimitCountInsufficientBalanceAlsoCounts 验证 429 + "insufficient balance" 同样会累加
// 计数（它仍然是一个 429）。
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

// TestRateLimitCountNonManaged429DoesNotCount 验证非托管 provider 的 429 不会累加计数。
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

// TestRateLimitCount401DoesNotCount 验证 401 响应不会累加 429 计数。
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

// TestRateLimitCountSuccessDoesNotCount 验证成功（非失败）响应不会累加 429 计数。
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

// TestRateLimitCountResetsAtMidnight 验证该计数在跨日时会被重置。
func TestRateLimitCountResetsAtMidnight(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	night := time.Date(2026, 7, 8, 23, 59, 0, 0, loc)
	s, cur := newTestStore(night)
	s.Reconfigure(cfgWith("modelscope"))
	// 先设置好 cooldownStatsDay，让 PruneAll 能识别出日期变化。
	s.PruneAll(night)

	// 制造两次 429。
	for i := 0; i < 2; i++ {
		s.OnUsage(UsageRecord{
			AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
			Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
		})
	}
	if n := s.RateLimitCount(); n != 2 {
		t.Fatalf("before midnight: count = %d, want 2", n)
	}

	// 跨过午夜。
	*cur = time.Date(2026, 7, 9, 0, 1, 0, 0, loc)
	s.PruneAll(*cur)

	if n := s.RateLimitCount(); n != 0 {
		t.Fatalf("after midnight: count = %d, want 0", n)
	}
}
