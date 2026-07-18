package ratelimit

import (
	"fmt"
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
	s.originalSnapshotted = true
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
	s.SetProxyTrigger("auth-1", "Qwen-72B")

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
		t.Fatal("first enable must snapshot the host proxy via GET (lazy, on first enable)")
	}
	// The boot snapshot must survive enable unchanged.
	if got := s.OriginalProxyURL(); got != "orig-proxy" {
		t.Fatalf("originalProxyURL = %q after enable, want orig-proxy", got)
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

	s.SetProxyTrigger("auth-1", "Qwen-72B")

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

// The first 429 (proxy enable) must NOT apply an artificial pre-wait: probe
// and enable immediately so the request proceeds without delay. proxyWait
// only affects the concurrent-waiter path now, so a large SetProxyWait must
// NOT slow the first enable.
func TestProxyFirstEnableNoArtificialWait(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	s.SetProxyWait(2 * time.Second) // would block 2s if a pre-wait remained
	s.SetProbeFunc(func(string) bool { return true })
	s.SetProxyToggler(func(string) error { return nil })
	s.SetProxyURLGetter(func() (string, error) { return "host-orig", nil })
	defer s.DisableProxyIfActive()

	s.SetProxyTrigger("auth-1", "Qwen-72B")
	start := time.Now()
	if !s.ConsumeProxyTrigger("Qwen-72B", now) {
		t.Fatal("1st enable must succeed")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("1st enable must have no artificial pre-wait, took %s", d)
	}
	if !s.IsProxyActive() {
		t.Fatal("proxy must be active after 1st enable")
	}
}

// Fix: a 429 arriving right at the previous cooldown's expiry (after the
// cooldown block slept through it) must ESCALATE, not be absorbed as the same
// round. Previously the round check used cooldownLevelAt+level+1s, so a retry
// at expiry (level < level+1s) was wrongly kept — making the escalation
// 10,10,20,40 instead of 10,20,40,60.
func TestProxyBackoffEscalatesAtExpiry(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	t0 := time.Date(2026, 7, 8, 10, 0, 0, 0, loc)
	s, cur := newTestStore(t0) // jitter = 0
	cfg := proxyCfg("http://127.0.0.1:8888", "modelscope")
	cfg.InsufficientQuotaCooldown = 10
	s.Reconfigure(cfg)
	s.SetProxyWait(time.Millisecond)
	s.SetProbeFunc(func(string) bool { return true })
	s.SetProxyToggler(func(string) error { return nil })
	s.SetProxyURLGetter(func() (string, error) { return "host-orig", nil })
	defer s.DisableProxyIfActive()

	// 1st 429: enable proxy, no cooldown.
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	if !s.ConsumeProxyTrigger("Qwen-72B", t0) {
		t.Fatal("1st 429 must enable the proxy")
	}
	if w := s.cooldownWaitDuration(t0); w > 0 {
		t.Fatalf("1st 429 must set no cooldown, got %s", w)
	}

	// 2nd 429 (proxy active): level -> base 10s.
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	s.ConsumeProxyTrigger("Qwen-72B", *cur)
	if w := s.cooldownWaitDuration(*cur); w != 10*time.Second {
		t.Fatalf("2nd 429 cooldown = %s, want 10s", w)
	}

	// 3rd 429 arriving exactly at the 10s expiry (as the cooldown block
	// would, after sleeping through it): must escalate to 20s.
	*cur = (*cur).Add(10 * time.Second)
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	s.ConsumeProxyTrigger("Qwen-72B", *cur)
	if w := s.cooldownWaitDuration(*cur); w != 20*time.Second {
		t.Fatalf("3rd 429 at expiry must escalate to 20s, got %s", w)
	}

	// 4th at the 20s expiry -> 40s.
	*cur = (*cur).Add(20 * time.Second)
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	s.ConsumeProxyTrigger("Qwen-72B", *cur)
	if w := s.cooldownWaitDuration(*cur); w != 40*time.Second {
		t.Fatalf("4th 429 at expiry must escalate to 40s, got %s", w)
	}
}

// Fix: a 429 while the proxy is active resets the 60s safety timer so it only
// fires after a quiescent period (no 429s). Without the reset, the timer
// fires 60s from the initial enable regardless of ongoing 429s, cycling the
// proxy off mid-exhaustion and resetting the backoff each cycle.
func TestProxySafetyTimerResetsOnActive429(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	s.SetProxyWait(time.Millisecond)
	s.SetProbeFunc(func(string) bool { return true })
	s.SetProxyToggler(func(string) error { return nil })
	s.SetProxyURLGetter(func() (string, error) { return "host-orig", nil })
	// Short safety timeout so the test runs fast.
	s.SetProxySafetyTimeout(80 * time.Millisecond)
	defer s.DisableProxyIfActive()

	// 1st 429: enable proxy.
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	if !s.ConsumeProxyTrigger("Qwen-72B", now) {
		t.Fatal("1st 429 must enable the proxy")
	}

	// Halfway to the safety timeout, a 2nd 429 resets it.
	time.Sleep(40 * time.Millisecond)
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	s.ConsumeProxyTrigger("Qwen-72B", now)
	if !s.IsProxyActive() {
		t.Fatal("proxy must stay active after 2nd 429")
	}

	// 60ms later: without the reset the timer (80ms from enable, 40ms elapsed
	// before the 2nd 429) would have fired at ~80ms; with the reset it fires
	// ~80ms after the 2nd 429, so at 40+60=100ms the proxy is still on.
	time.Sleep(60 * time.Millisecond)
	if !s.IsProxyActive() {
		t.Fatal("proxy must stay active — 2nd 429 must have reset the safety timer")
	}
}

// Invariant: the proxy safety timeout must exceed the max cooldown
// (maxInsufficientQuotaCooldown). At the 60s backoff cap the cooldown block
// sleeps the full cap; if the safety timer (reset on each 429) were <= the
// cap, it would fire DURING the block, disabling the proxy mid-block. After
// the block the next 429 re-enables via the no-cooldown re-enable path, and
// since the old cooldown expired, the following request isn't blocked —
// bursting two requests. safety > cap guarantees the timer can't fire during
// any cooldown block.
func TestProxySafetyTimeoutExceedsMaxCooldown(t *testing.T) {
	if proxySafetyTimeout <= maxInsufficientQuotaCooldown*time.Second {
		t.Fatalf("proxySafetyTimeout %s must exceed max cooldown %s to avoid mid-block disable",
			proxySafetyTimeout, maxInsufficientQuotaCooldown*time.Second)
	}
}

// --- 401 x proxy-mode interaction (post-merge) ---------------------------

// With proxy mode configured, a 401 disable must still persist across the
// midnight boundary (proxy mode does not weaken the persistent 401 semantics).
func TestProxy401PersistsAcrossMidnight(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	night := time.Date(2026, 7, 8, 23, 59, 0, 0, loc)
	s, _ := newTestStore(night)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))

	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 401, Body: `{"error":"unauthorized"}`},
	})
	if s.proxyTriggerPending {
		t.Fatal("401 must not set the proxy trigger")
	}

	nextDay := time.Date(2026, 7, 9, 0, 1, 0, 0, loc)
	s.PruneAll(nextDay)
	if !s.isDisabled("auth-1", "Qwen-72B", nextDay) {
		t.Fatal("401 disable must persist across midnight even with proxy configured")
	}
}

// SchedulerPick with proxy configured must skip a 401-disabled key and route
// to a healthy one instead (the unauthorized disable is honored by the
// scheduler, not bypassed by proxy mode).
func TestProxySchedulerSkipsUnauthorizedKey(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))

	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 401, Body: `{"error":"unauthorized"}`},
	})

	req := SchedulerPickRequest{
		Provider:  "modelscope",
		Providers: []string{"modelscope"},
		Model:     "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
			{ID: "auth-2", Provider: "modelscope", Priority: 1},
		},
	}
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled {
		t.Fatalf("pick failed: %+v err=%v", resp, err)
	}
	if resp.AuthID != "auth-2" {
		t.Fatalf("scheduler must skip the 401-disabled key, picked %q", resp.AuthID)
	}
}

// A successful managed response (which resets the cooldown backoff and turns
// the proxy off) must NOT clear another key's persistent 401 disable.
func TestProxySuccessDoesNotClearUnauthorizedDisable(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	var toggled []string
	s.SetProxyToggler(func(u string) error { toggled = append(toggled, u); return nil })
	setProxyActiveForTest(s, "orig")

	// auth-1 is 401-disabled.
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 401, Body: `{"error":"unauthorized"}`},
	})

	// auth-2 succeeds (managed) -> resets backoff, disables proxy.
	s.OnUsage(UsageRecord{
		AuthID: "auth-2", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: false, ResponseHeaders: hdr("100"),
	})

	if !s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("managed success on another key must not clear the 401 disable")
	}
	if s.IsProxyActive() {
		t.Fatal("managed success must disable the active proxy")
	}
}

// When the proxy is already active (probe succeeded), a 401 from a managed
// provider must still disable the credential persistently and must NOT fire
// the proxy trigger (401 is checked before the proxy-trigger guard).
func TestProxy401WhileActiveDisablesNotTriggers(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	setProxyActiveForTest(s, "orig")

	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 401, Body: `{"error":"unauthorized"}`},
	})

	if s.proxyTriggerPending {
		t.Fatal("401 must not set the proxy trigger even when proxy is active")
	}
	if w := s.cooldownWaitDuration(now); w > 0 {
		t.Fatalf("401 must not set a cooldown, got %s", w)
	}
	if !s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("401 must disable the credential even with proxy active")
	}
	if !s.IsProxyActive() {
		t.Fatal("a 401 alone must not turn the active proxy off (only success does)")
	}
}

// --- proxy + insufficient_quota backoff combination ----------------------

// Backoff progression: the first proxy-enable 429 sets no cooldown (level 0);
// each subsequent 429 while the proxy is active escalates the shared
// insufficient_quota backoff (base -> x2 -> x2 ...). A managed 200 resets the
// level and disables the proxy, so the next 429 starts over at the enable.
func TestProxyBackoffProgression(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	t0 := time.Date(2026, 7, 8, 10, 0, 0, 0, loc)
	s, cur := newTestStore(t0)
	cfg := proxyCfg("http://127.0.0.1:8888", "modelscope")
	cfg.InsufficientQuotaCooldown = 1 // base 1s for fast progression
	s.Reconfigure(cfg)
	s.SetProxyWait(time.Millisecond)
	s.SetProbeFunc(func(string) bool { return true })
	var toggled []string
	s.SetProxyToggler(func(u string) error { toggled = append(toggled, u); return nil })
	s.SetProxyURLGetter(func() (string, error) { return "orig", nil })

	// 1st 429: proxy inactive -> probe + enable. No backoff, no cooldown.
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	if !s.ConsumeProxyTrigger("Qwen-72B", t0) {
		t.Fatal("1st 429 must enable the proxy")
	}
	if !s.IsProxyActive() {
		t.Fatal("proxy must be active after 1st 429")
	}
	if w := s.cooldownWaitDuration(t0); w > 0 {
		t.Fatalf("1st 429 must not set a cooldown, got %s", w)
	}

	// helper to advance the clock clear of the previous cooldown+window.
	advance := func(d time.Duration) { *cur = cur.Add(d) }

	// 2nd 429 (proxy active): 1s rotation + setCooldown -> base (1s).
	advance(3 * time.Second)
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	s.ConsumeProxyTrigger("Qwen-72B", *cur)
	if w := s.cooldownWaitDuration(*cur); w != time.Second {
		t.Fatalf("2nd 429 cooldown = %s, want 1s", w)
	}

	// 3rd 429: x2 -> 2s.
	advance(3 * time.Second)
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	s.ConsumeProxyTrigger("Qwen-72B", *cur)
	if w := s.cooldownWaitDuration(*cur); w != 2*time.Second {
		t.Fatalf("3rd 429 cooldown = %s, want 2s", w)
	}

	// 4th 429: x2 -> 4s.
	advance(5 * time.Second)
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	s.ConsumeProxyTrigger("Qwen-72B", *cur)
	if w := s.cooldownWaitDuration(*cur); w != 4*time.Second {
		t.Fatalf("4th 429 cooldown = %s, want 4s", w)
	}

	// 200 from a managed provider: reset the backoff LEVEL (so the next 429
	// starts from base) and disable the proxy. The per-key cooldown is left
	// to expire naturally — in production the 200 arrives only after the
	// scheduler already slept through it.
	advance(5 * time.Second) // clear the 4s cooldown + window
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: false, ResponseHeaders: hdr("100"),
	})
	if s.IsProxyActive() {
		t.Fatal("200 must disable the proxy")
	}

	// Post-reset 1st 429: re-enable (no backoff, level stays 0).
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	if !s.ConsumeProxyTrigger("Qwen-72B", *cur) {
		t.Fatal("post-reset 429 must re-enable the proxy")
	}
	if !s.IsProxyActive() {
		t.Fatal("proxy must be active again after reset+429")
	}
	if w := s.cooldownWaitDuration(*cur); w > 0 {
		t.Fatalf("post-reset 1st 429 must not set a cooldown, got %s", w)
	}

	// Post-reset 2nd 429: must restart at base (1s), NOT continue at 8s —
	// this proves the 200 reset the backoff level.
	advance(3 * time.Second)
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	s.ConsumeProxyTrigger("Qwen-72B", *cur)
	if w := s.cooldownWaitDuration(*cur); w != time.Second {
		t.Fatalf("post-reset 2nd 429 cooldown = %s, want 1s (base, level was reset)", w)
	}
	s.DisableProxyIfActive()
}

// Via SchedulerPick: the first 429 (proxy enable) returns quickly (no backoff
// sleep); the second 429 blocks for ~insufficient_quota_cooldown (the backoff).
func TestProxyFirstNoBackoffSecondBackoff(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	t0 := time.Date(2026, 7, 8, 10, 0, 0, 0, loc)
	s, cur := newTestStore(t0)
	cfg := proxyCfg("http://127.0.0.1:8888", "modelscope")
	cfg.InsufficientQuotaCooldown = 1
	s.Reconfigure(cfg)
	s.SetProxyWait(time.Millisecond)
	s.SetProbeFunc(func(string) bool { return true })
	s.SetProxyToggler(func(string) error { return nil })
	s.SetProxyURLGetter(func() (string, error) { return "", nil })
	defer s.DisableProxyIfActive()

	req := SchedulerPickRequest{
		Provider: "modelscope", Providers: []string{"modelscope"}, Model: "Qwen-72B",
		Candidates: []Candidate{{ID: "auth-1", Provider: "modelscope", Priority: 1}},
	}

	// 1st 429 -> enable proxy; pick should be fast (probe+enable, no backoff).
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody()},
	})
	start := time.Now()
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "auth-1" {
		t.Fatalf("1st pick failed: %+v err=%v", resp, err)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("1st 429 must not block on backoff, took %s", time.Since(start))
	}

	// advance clock clear of the (absent) cooldown + window before 2nd 429.
	*cur = cur.Add(3 * time.Second)

	// 2nd 429 (proxy active) -> must block ~1s (the backoff base).
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody()},
	})
	start = time.Now()
	resp, err = s.SchedulerPick(req)
	if err != nil || !resp.Handled {
		t.Fatalf("2nd pick failed: %+v err=%v", resp, err)
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("2nd 429 must block ~1s on backoff, took %s", d)
	}
}

// --- snapshotHostProxyOnce (lazy, on first enable) ---------------------

// snapshotHostProxyOnce is idempotent: the first call reads the host proxy
// via GET and stores it; subsequent calls return immediately without
// re-reading — the lazy-once contract that keeps the hot path GET-free
// while still snapshotting only after the server is up (first 429).
func TestSnapshotHostProxyOnceOnlyGetsOnce(t *testing.T) {
	s, _ := newTestStore(time.Now())
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	calls := 0
	s.SetProxyURLGetter(func() (string, error) { calls++; return "host-orig", nil })

	if !s.snapshotHostProxyOnce() {
		t.Fatal("first snapshotHostProxyOnce must succeed")
	}
	if got := s.OriginalProxyURL(); got != "host-orig" {
		t.Fatalf("snapshot = %q, want host-orig", got)
	}
	if calls != 1 {
		t.Fatalf("getter called %d times, want 1", calls)
	}

	// Second call must NOT re-GET (flag set).
	if !s.snapshotHostProxyOnce() {
		t.Fatal("second snapshotHostProxyOnce must return true without re-GET")
	}
	if calls != 1 {
		t.Fatalf("getter called %d times after 2nd, want 1 (once)", calls)
	}
}

// A failed host-proxy GET must NOT enable the proxy: without a snapshot,
// disableProxy would DELETE the host's real proxy instead of restoring it.
// snapshotHostProxyOnce returns false, leaves the flag unset (so the next
// enable retries), and reenableProxy must skip the PUT entirely.
func TestSnapshotGetterFailureDefersEnable(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	s.SetProxyWait(time.Millisecond)
	s.SetProbeFunc(func(string) bool { return true })
	s.SetProxyToggler(func(string) error {
		t.Fatal("toggler must not run when host-proxy snapshot is unavailable")
		return nil
	})
	s.SetProxyURLGetter(func() (string, error) { return "", fmt.Errorf("management API down") })

	// First-time enable path: probe OK, but snapshot GET fails -> reenableProxy
	// must NOT enable the proxy.
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	if s.ConsumeProxyTrigger("Qwen-72B", now) {
		t.Fatal("enable must be deferred when host-proxy snapshot is unavailable")
	}
	if s.IsProxyActive() {
		t.Fatal("proxy must remain inactive when snapshot failed")
	}
	if got := s.OriginalProxyURL(); got != "" {
		t.Fatalf("originalProxyURL = %q, want empty on GET failure", got)
	}
}

// Reload that removes proxy_url while the proxy is active must proactively
// disable the proxy and restore the host's original proxy URL (the boot
// snapshot), instead of leaving the plugin proxy enabled with no config.
func TestDisableProxyOnReconfigureRestoresHostProxy(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	s.SetProxyWait(time.Millisecond)
	s.SetProbeFunc(func(string) bool { return true })

	var toggled []string
	s.SetProxyToggler(func(u string) error { toggled = append(toggled, u); return nil })
	s.SetProxyURLGetter(func() (string, error) { return "host-orig", nil })

	// Enable the proxy (first-time probe path).
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	if !s.ConsumeProxyTrigger("Qwen-72B", now) {
		t.Fatal("enable must succeed")
	}
	if !s.IsProxyActive() {
		t.Fatal("proxy must be active after enable")
	}
	if got := toggled[len(toggled)-1]; got != "http://127.0.0.1:8888" {
		t.Fatalf("enable PUT = %q, want plugin proxy", got)
	}

	// Reload: new config drops proxy_url entirely.
	s.Reconfigure(cfgWith("modelscope"))
	s.DisableProxyOnReconfigure()

	if s.IsProxyActive() {
		t.Fatal("proxy must be disabled after proxy_url removed")
	}
	if got := toggled[len(toggled)-1]; got != "host-orig" {
		t.Fatalf("reload restore = %q, want host-orig", got)
	}
}

// The lazy host-proxy snapshot (taken once on first enable) must survive
// disable/re-enable cycles: disableProxy must NOT clear originalProxyURL, so
// a later re-enable (which does not re-GET) and disable still restores the
// host's original proxy. This locks in the snapshot-once design — without
// it, the second disable would DELETE (empty snapshot) instead of restoring.
func TestProxySnapshotSurvivesDisableReenableCycle(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	s.SetProxyWait(time.Millisecond)
	s.SetProbeFunc(func(string) bool { return true })

	var toggled []string
	s.SetProxyToggler(func(u string) error { toggled = append(toggled, u); return nil })
	s.SetProxyURLGetter(func() (string, error) { return "host-orig", nil })

	// 1st enable + disable (managed success).
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	if !s.ConsumeProxyTrigger("Qwen-72B", now) {
		t.Fatal("1st enable must succeed")
	}
	s.HandleProxyOnSuccess() // disable -> restore host-orig
	if s.IsProxyActive() {
		t.Fatal("proxy must be off after success")
	}
	// Snapshot must NOT be cleared by disable.
	if got := s.OriginalProxyURL(); got != "host-orig" {
		t.Fatalf("snapshot cleared after disable = %q, want host-orig (must persist)", got)
	}
	if got := toggled[len(toggled)-1]; got != "host-orig" {
		t.Fatalf("1st disable restore = %q, want host-orig", got)
	}

	// 2nd enable (re-enable after prior probe: no re-GET) + disable.
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	if !s.ConsumeProxyTrigger("Qwen-72B", now) {
		t.Fatal("2nd enable must succeed")
	}
	if got := s.OriginalProxyURL(); got != "host-orig" {
		t.Fatalf("snapshot lost after re-enable = %q, want host-orig", got)
	}
	s.HandleProxyOnSuccess()
	if got := toggled[len(toggled)-1]; got != "host-orig" {
		t.Fatalf("2nd disable restore = %q, want host-orig (snapshot must persist across cycle)", got)
	}
}

// DisableProxyOnReconfigure is a no-op when the proxy is inactive (e.g. a
// reload that removes proxy_url before the proxy was ever enabled).
func TestDisableProxyOnReconfigureNoopWhenInactive(t *testing.T) {
	s, _ := newTestStore(time.Now())
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	called := false
	s.SetProxyToggler(func(string) error { called = true; return nil })

	s.DisableProxyOnReconfigure()

	if called {
		t.Fatal("toggler must not be called when proxy is inactive")
	}
}
