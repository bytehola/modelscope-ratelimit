package ratelimit

import (
	"testing"
	"time"
)

// proxyCfg returns a config with the given providers, proxy mode enabled and a
// short insufficient_quota cooldown (for fallback assertions).
func proxyCfg(proxyURL string, providers ...string) *Config {
	c := cfgWith(providers...)
	c.ProxyURL = proxyURL
	c.InsufficientQuotaCooldown = 5
	return c
}

// setProxyActiveForTest flips the internal proxy-active flag (white-box) so
// disable-path tests can run without driving the full enable flow.
func setProxyActiveForTest(s *Store, original string) {
	s.proxyMu.Lock()
	s.proxyActive = true
	s.proxyProbed = true
	s.originalProxyURL = original
	s.proxyMu.Unlock()
}

// insufficientQuotaBody is a 429 body carrying the insufficient_quota marker.
func insufficientQuotaBody() string {
	return `{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`
}

// --- OnUsage guard --------------------------------------------------------

// When proxy mode is active (proxy_url configured, probe not failed), a 429
// from a managed provider must set the proxy trigger and NOT set a cooldown.
// This is the core of "no conflict": proxy mode replaces the cooldown.
func TestProxyOnUsageSetsTriggerNoCooldown(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))

	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody()},
	})

	if !s.proxyTriggerPending {
		t.Fatal("proxy mode must set proxyTriggerPending on 429+insufficient_quota")
	}
	if w := s.cooldownWaitDuration(now); w > 0 {
		t.Fatalf("proxy mode must not set a cooldown, got %s", w)
	}
	if s.IsProxyActive() {
		t.Fatal("OnUsage must not enable the proxy synchronously (probe happens in scheduler)")
	}
}

// When the probe already failed, OnUsage must fall back to the
// insufficient_quota cooldown and NOT set the proxy trigger.
func TestProxyProbeFailedOnUsageFallsBackToCooldown(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	s.proxyMu.Lock()
	s.proxyProbeFailed = true
	s.proxyMu.Unlock()

	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody()},
	})

	if s.proxyTriggerPending {
		t.Fatal("probe-failed mode must not set the proxy trigger")
	}
	if w := s.cooldownWaitDuration(now); w <= 0 {
		t.Fatalf("probe-failed mode must fall back to a cooldown, got %s", w)
	}
}

// A 401 from a managed provider disables the credential globally (secret
// invalid), regardless of proxy mode. No trigger, no cooldown.
func TestProxyOnUsage401Disables(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))

	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 401, Body: `{"error":"unauthorized"}`},
	})

	if s.proxyTriggerPending {
		t.Fatal("401 must not set the proxy trigger")
	}
	if w := s.cooldownWaitDuration(now); w > 0 {
		t.Fatalf("401 must not set a cooldown, got %s", w)
	}
	if !s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("401 must globally disable the credential")
	}
}

// A plain 429 (no insufficient_quota marker) triggers neither proxy mode nor
// a cooldown: only billing-quota exhaustion (insufficient_quota) is handled.
func TestProxyOnUsagePlain429NoTriggerNoCooldown(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))

	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":"rate_limited"}`},
	})

	if s.proxyTriggerPending {
		t.Fatal("plain 429 must not set the proxy trigger")
	}
	if w := s.cooldownWaitDuration(now); w > 0 {
		t.Fatalf("plain 429 must not set a cooldown, got %s", w)
	}
}

// A 429+insufficient_quota from a NON-managed provider (e.g. a paid fallback)
// must be ignored entirely: no trigger, no cooldown.
func TestProxyOnUsageNonManagedNoEffect(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))

	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "aliyun-bailian", // not monitored
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody()},
	})

	if s.proxyTriggerPending {
		t.Fatal("non-managed provider must not set the proxy trigger")
	}
	if w := s.cooldownWaitDuration(now); w > 0 {
		t.Fatalf("non-managed provider must not set a cooldown, got %s", w)
	}
}

// A successful managed response disables an active proxy (quota recovered).
func TestProxyOnUsageSuccessDisablesProxy(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))

	var toggled []string
	s.SetProxyToggler(func(u string) error { toggled = append(toggled, u); return nil })
	setProxyActiveForTest(s, "orig-proxy")

	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   false,
	})

	if s.IsProxyActive() {
		t.Fatal("managed success must disable the proxy")
	}
	if len(toggled) != 1 || toggled[0] != "orig-proxy" {
		t.Fatalf("must restore original proxy, got %v", toggled)
	}
}

// --- OnResponse guard -----------------------------------------------------

// OnResponse must not apply an insufficient_quota cooldown when proxy mode is
// active (proxy_url configured, probe not failed), even if it somehow receives
// a 429 body. Without proxy configured, the cooldown IS applied.
func TestProxyOnResponseGuardSkipsCooldownWhenProxyActive(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))

	t.Run("proxy_active_skips_cooldown", func(t *testing.T) {
		s, _ := newTestStore(now)
		s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))

		s.OnResponse(ResponseInterceptRequest{
			Model:      "Qwen-72B",
			StatusCode: 429,
			Body:       insufficientQuotaBody(),
			Metadata:   map[string]any{"auth_id": "auth-1"},
		})

		if w := s.cooldownWaitDuration(now); w > 0 {
			t.Fatalf("proxy active must skip cooldown on OnResponse, got %s", w)
		}
	})

	t.Run("no_proxy_applies_cooldown", func(t *testing.T) {
		s, _ := newTestStore(now)
		s.Reconfigure(proxyCfg("", "modelscope"))

		s.OnResponse(ResponseInterceptRequest{
			Model:      "Qwen-72B",
			StatusCode: 429,
			Body:       insufficientQuotaBody(),
			Metadata:   map[string]any{"auth_id": "auth-1"},
		})

		if w := s.cooldownWaitDuration(now); w <= 0 {
			t.Fatalf("without proxy the cooldown must be applied, got %s", w)
		}
	})
}

// --- SchedulerPick cooldown-skip / fallback ------------------------------

// With proxy mode active (probe OK), SchedulerPick must NOT block on a
// pre-existing cooldown even though one is planted in state.
func TestSchedulerSkipsCooldownWhenProxyConfigured(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))

	// Plant a 5s cooldown directly (proxy mode would never set one, but this
	// proves the scheduler skip, not the absence of a cooldown).
	s.setCooldown("auth-1", 5, now)
	if w := s.cooldownWaitDuration(now); w <= 0 {
		t.Fatalf("fixture: cooldown must exist, got %s", w)
	}

	req := SchedulerPickRequest{
		Provider:  "modelscope",
		Providers: []string{"modelscope"},
		Model:     "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
		},
	}

	start := time.Now()
	resp, err := s.SchedulerPick(req)
	elapsed := time.Since(start)
	if err != nil || !resp.Handled || resp.AuthID != "auth-1" {
		t.Fatalf("pick failed: %+v err=%v", resp, err)
	}
	// A cooldown exists in state, yet the pick returned instantly -> the block
	// was skipped because proxy mode is active.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("proxy mode must skip the cooldown block, took %s", elapsed)
	}
}

// When the probe failed, SchedulerPick must fall back to blocking on the
// cooldown (the insufficient_quota mechanism).
func TestSchedulerBlocksCooldownWhenProbeFailed(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := proxyCfg("http://127.0.0.1:8888", "modelscope")
	cfg.InsufficientQuotaCooldown = 1 // keep the block short
	s.Reconfigure(cfg)
	s.proxyMu.Lock()
	s.proxyProbeFailed = true
	s.proxyMu.Unlock()

	s.setCooldown("auth-1", 1, now)

	req := SchedulerPickRequest{
		Provider:  "modelscope",
		Providers: []string{"modelscope"},
		Model:     "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
		},
	}

	start := time.Now()
	resp, err := s.SchedulerPick(req)
	elapsed := time.Since(start)
	if err != nil || !resp.Handled || resp.AuthID != "auth-1" {
		t.Fatalf("pick failed: %+v err=%v", resp, err)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("probe-failed must block on cooldown, took %s", elapsed)
	}
}

// --- ConsumeProxyTrigger full path ---------------------------------------

// With a pending trigger, a (faked) reachable probe and injected toggler,
// ConsumeProxyTrigger (driven via SchedulerPick) must enable the global proxy.
func TestConsumeProxyTriggerProbesAndEnables(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	s.SetProxyWait(time.Millisecond) // keep the test fast
	probed := false
	s.SetProbeFunc(func(u string) bool { probed = true; return true })

	var toggled []string
	getterCalled := false
	s.SetProxyToggler(func(u string) error { toggled = append(toggled, u); return nil })
	s.SetProxyURLGetter(func() (string, error) { getterCalled = true; return "orig-proxy", nil })
	defer s.DisableProxyIfActive()

	// Set the trigger the way OnUsage would.
	s.SetProxyTrigger()

	req := SchedulerPickRequest{
		Provider:  "modelscope",
		Providers: []string{"modelscope"},
		Model:     "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
		},
	}
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled {
		t.Fatalf("pick failed: %+v err=%v", resp, err)
	}
	if !probed {
		t.Fatal("probe must be invoked on first trigger")
	}
	if !s.IsProxyActive() {
		t.Fatal("proxy must be active after a successful probe")
	}
	if len(toggled) != 1 || toggled[0] != "http://127.0.0.1:8888" {
		t.Fatalf("toggler must set the configured proxy URL, got %v", toggled)
	}
	if !getterCalled {
		t.Fatal("getter must read the host's original proxy for restore")
	}
}

// A failed probe sets proxyProbeFailed and leaves the proxy inactive, so
// subsequent 429s fall back to the cooldown.
func TestConsumeProxyTriggerProbeFailureFallsBack(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	s.SetProxyWait(time.Millisecond)
	s.SetProbeFunc(func(u string) bool { return false })
	var toggled []string
	s.SetProxyToggler(func(u string) error { toggled = append(toggled, u); return nil })

	s.SetProxyTrigger()

	req := SchedulerPickRequest{
		Provider:  "modelscope",
		Providers: []string{"modelscope"},
		Model:     "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
		},
	}
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "auth-1" {
		t.Fatalf("pick must still return a candidate after probe failure: %+v err=%v", resp, err)
	}
	if !s.IsProxyProbeFailed() {
		t.Fatal("a failed probe must set IsProxyProbeFailed")
	}
	if s.IsProxyActive() {
		t.Fatal("proxy must remain inactive after a failed probe")
	}
	if len(toggled) != 0 {
		t.Fatalf("toggler must not be called on probe failure, got %v", toggled)
	}

	// A subsequent 429 now falls back to the cooldown path.
	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody()},
	})
	if s.proxyTriggerPending {
		t.Fatal("after probe failure a 429 must not re-set the proxy trigger")
	}
	if w := s.cooldownWaitDuration(now); w <= 0 {
		t.Fatalf("after probe failure a 429 must set a cooldown, got %s", w)
	}
}

// --- Proxy disable paths -------------------------------------------------

// HandleProxyOnSuccess disables an active proxy and restores the original.
func TestHandleProxyOnSuccessDisables(t *testing.T) {
	s, _ := newTestStore(time.Now())
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	var toggled []string
	s.SetProxyToggler(func(u string) error { toggled = append(toggled, u); return nil })
	setProxyActiveForTest(s, "orig-proxy")

	s.HandleProxyOnSuccess()

	if s.IsProxyActive() {
		t.Fatal("HandleProxyOnSuccess must disable the proxy")
	}
	if len(toggled) != 1 || toggled[0] != "orig-proxy" {
		t.Fatalf("must restore original proxy, got %v", toggled)
	}
}

// DisableProxyIfActive is a no-op when the proxy is inactive.
func TestDisableProxyIfActiveNoopWhenInactive(t *testing.T) {
	s, _ := newTestStore(time.Now())
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	called := false
	s.SetProxyToggler(func(u string) error { called = true; return nil })

	s.DisableProxyIfActive()

	if called {
		t.Fatal("toggler must not be called when proxy is inactive")
	}
}
