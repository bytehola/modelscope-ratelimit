package ratelimit

import (
	"testing"
	"time"
)

// TestFloatCooldownDefault verifies the default cooldown is 0.1 (float64).
func TestFloatCooldownDefault(t *testing.T) {
	c := DefaultConfig()
	if c.InsufficientQuotaCooldown != 0.1 {
		t.Fatalf("default cooldown = %v, want 0.1", c.InsufficientQuotaCooldown)
	}
}

// TestFloatCooldownParsing verifies that fractional cooldown values in config
// maps are parsed correctly via toFloat64.
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

// TestFloatCooldownFractionalBlock verifies that a 0.1s cooldown actually
// produces a sub-second wait (not truncated to 0 by time.Duration(int)).
func TestFloatCooldownFractionalBlock(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, cur := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 0.1
	s.Reconfigure(cfg)

	// Trigger insufficient_quota 429 on key auth-1.
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})

	// The cooldown should be active (100ms).
	w := s.cooldownWaitDuration(*cur)
	if w <= 0 {
		t.Fatalf("cooldown wait = %v, want > 0", w)
	}
	if w > 500*time.Millisecond {
		t.Fatalf("cooldown wait = %v, want <= 500ms (base 0.1s)", w)
	}

	// Advance past the cooldown.
	*cur = now.Add(200 * time.Millisecond)
	w = s.cooldownWaitDuration(*cur)
	if w > 0 {
		t.Fatalf("cooldown should be expired after 200ms, got %v", w)
	}
}

// TestFloatCooldownLevelDoubles verifies that the exponential backoff
// works correctly with float64 level (0.1 -> 0.2 -> 0.4 -> ...).
func TestFloatCooldownLevelDoubles(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, cur := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 0.1
	cfg.SkipProviderOn429 = false // use cooldown mode
	s.Reconfigure(cfg)

	fail := func(authID string, at time.Time) {
		*cur = at
		s.OnUsage(UsageRecord{
			AuthID: authID, Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
			Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
		})
	}

	// Round 1: base 0.1s
	fail("auth-1", now)
	w := s.cooldownWaitDuration(now)
	if w != 100*time.Millisecond {
		t.Fatalf("round 1: want 100ms, got %s", w)
	}

	// Round 2: 0.2s (double). Must advance past the cooldown expiry.
	*cur = now.Add(200 * time.Millisecond)
	fail("auth-1", *cur)
	w = s.cooldownWaitDuration(*cur)
	if w != 200*time.Millisecond {
		t.Fatalf("round 2: want 200ms, got %s", w)
	}

	// Round 3: 0.4s
	*cur = now.Add(500 * time.Millisecond)
	fail("auth-1", *cur)
	w = s.cooldownWaitDuration(*cur)
	if w != 400*time.Millisecond {
		t.Fatalf("round 3: want 400ms, got %s", w)
	}

	// Healthy response resets the backoff.
	*cur = now.Add(1 * time.Second)
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: false,
	})
	w = s.cooldownWaitDuration(*cur)
	if w > 0 {
		t.Fatalf("after reset: want 0, got %s", w)
	}

	// After reset, next failure starts from base again.
	fail("auth-1", *cur)
	w = s.cooldownWaitDuration(*cur)
	if w != 100*time.Millisecond {
		t.Fatalf("after reset round 1: want 100ms, got %s", w)
	}
}

// TestFloatCooldownZeroSkips verifies that setting the base to 0 disables
// insufficient_quota cooldowns completely: no per-key wait and no cooldown
// statistic increment. Provider/key rotation should continue without sleeps.
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

	// With the unified rotation logic, a 429 does NOT advance the provider
	// cursor. The cursor stays at 0; keys are exhausted within the same
	// provider before moving to the next.
	if got := s.managedProviderStart(len(cfg.Providers)); got != 0 {
		t.Fatalf("429 with cooldown=0 must NOT advance provider cursor, got %d", got)
	}
}
