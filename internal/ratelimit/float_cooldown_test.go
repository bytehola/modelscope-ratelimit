package ratelimit

import (
	"testing"
	"time"
)

// TestFloatCooldownDefault 验证默认冷却是 0.1（float64）。
func TestFloatCooldownDefault(t *testing.T) {
	c := DefaultConfig()
	if c.InsufficientQuotaCooldown != 0.1 {
		t.Fatalf("default cooldown = %v, want 0.1", c.InsufficientQuotaCooldown)
	}
}

// TestFloatCooldownParsing 验证配置 map 里的小数冷却值能通过 toFloat64 正确解析。
func TestFloatCooldownParsing(t *testing.T) {
	t.Run("integer", func(t *testing.T) {
		c, err := ConfigFromMap(map[string]any{"insufficient_quota_cooldown": 3})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if c.InsufficientQuotaCooldown != 3.0 {
			t.Fatalf("cooldown = %v, want 3.0", c.InsufficientQuotaCooldown)
		}
	})
	t.Run("fractional", func(t *testing.T) {
		c, err := ConfigFromMap(map[string]any{"insufficient_quota_cooldown": 0.5})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if c.InsufficientQuotaCooldown != 0.5 {
			t.Fatalf("cooldown = %v, want 0.5", c.InsufficientQuotaCooldown)
		}
	})
	t.Run("string", func(t *testing.T) {
		c, err := ConfigFromMap(map[string]any{"insufficient_quota_cooldown": "0.25"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if c.InsufficientQuotaCooldown != 0.25 {
			t.Fatalf("cooldown = %v, want 0.25", c.InsufficientQuotaCooldown)
		}
	})
}

// TestFloatCooldownFractionalBlock 验证 0.1 秒的冷却确实会产生亚秒级等待，而不会被
// time.Duration(int) 截断成 0。
func TestFloatCooldownFractionalBlock(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, cur := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 0.1
	s.Reconfigure(cfg)

	// 让 auth-1 触发一次 insufficient_quota 的 429。
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})

	// 此时冷却应当有效（100ms）。
	w := s.cooldownWaitDuration(*cur)
	if w <= 0 {
		t.Fatalf("cooldown wait = %v, want > 0", w)
	}
	if w > 500*time.Millisecond {
		t.Fatalf("cooldown wait = %v, want <= 500ms (base 0.1s)", w)
	}

	// 把时间推进到冷却结束之后。
	*cur = now.Add(200 * time.Millisecond)
	w = s.cooldownWaitDuration(*cur)
	if w > 0 {
		t.Fatalf("cooldown should be expired after 200ms, got %v", w)
	}
}

// TestFloatCooldownLevelDoubles 验证指数退避在 float64 档位下工作正常
// （0.1 -> 0.2 -> 0.4 -> ...）。
func TestFloatCooldownLevelDoubles(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, cur := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 0.1
	cfg.SkipProviderOn429 = false // 使用退避冷却模式
	s.Reconfigure(cfg)

	fail := func(authID string, at time.Time) {
		*cur = at
		s.OnUsage(UsageRecord{
			AuthID: authID, Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
			Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
		})
	}

	// 第 1 轮：基准 0.1s
	fail("auth-1", now)
	w := s.cooldownWaitDuration(now)
	if w != 100*time.Millisecond {
		t.Fatalf("round 1: want 100ms, got %s", w)
	}

	// 第 2 轮：0.2s（翻倍）。必须把时间推进到冷却到期之后。
	*cur = now.Add(200 * time.Millisecond)
	fail("auth-1", *cur)
	w = s.cooldownWaitDuration(*cur)
	if w != 200*time.Millisecond {
		t.Fatalf("round 2: want 200ms, got %s", w)
	}

	// 第 3 轮：0.4s
	*cur = now.Add(500 * time.Millisecond)
	fail("auth-1", *cur)
	w = s.cooldownWaitDuration(*cur)
	if w != 400*time.Millisecond {
		t.Fatalf("round 3: want 400ms, got %s", w)
	}

	// 健康响应会重置退避。
	*cur = now.Add(1 * time.Second)
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: false,
	})
	w = s.cooldownWaitDuration(*cur)
	if w > 0 {
		t.Fatalf("after reset: want 0, got %s", w)
	}

	// 重置之后，下一次失败重新从基准值开始。
	fail("auth-1", *cur)
	w = s.cooldownWaitDuration(*cur)
	if w != 100*time.Millisecond {
		t.Fatalf("after reset round 1: want 100ms, got %s", w)
	}
}

// TestFloatCooldownZeroSkips 验证把基准值设为 0 就彻底关闭 insufficient_quota 冷却：既不等待，
// 也不累加冷却统计。provider/key 轮换应当在没有睡眠的情况下继续。
func TestFloatCooldownZeroSkips(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope", "modelscope-ai")
	cfg.InsufficientQuotaCooldown = 0
	cfg.SkipProviderOn429 = false
	s.Reconfigure(cfg)

	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})

	if w := s.cooldownWaitDuration(now); w != 0 {
		t.Fatalf("cooldown base 0 must skip the wait, got %s", w)
	}
	if count, _ := s.CooldownStats(); count != 0 {
		t.Fatalf("cooldown base 0 must not increment cooldown stats, got %d", count)
	}

	// 按统一的轮换逻辑，429 不会推进跨 provider 游标：游标停在 0，先把同一家里的 key 试完再换
	// 下一家。
	if got := s.managedProviderStart(len(cfg.Providers)); got != 0 {
		t.Fatalf("429 with cooldown=0 must NOT advance provider cursor, got %d", got)
	}
}
