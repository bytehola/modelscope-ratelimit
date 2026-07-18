package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// Fix #1: after a disable -> re-enable -> disable cycle, every disable must
// still restore the host's original proxy URL. disableProxy previously cleared
// originalProxyURL after one use and reenableProxy never re-snapshotted it, so
// the second disable restored "" (DELETE) instead of the host original.
func TestFixProxyOriginalSurvivesReenableCycle(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	s.SetProxyWait(time.Millisecond)
	s.SetProbeFunc(func(string) bool { return true })
	var mu sync.Mutex
	var toggled []string
	s.SetProxyToggler(func(u string) error {
		mu.Lock()
		defer mu.Unlock()
		toggled = append(toggled, u)
		return nil
	})
	s.SetProxyURLGetter(func() (string, error) { return "host-orig", nil })

	// Enable (first-time probe path).
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	if !s.ConsumeProxyTrigger("Qwen-72B", now) {
		t.Fatal("1st enable must succeed")
	}
	s.HandleProxyOnSuccess() // 1st disable -> restore host-orig

	// Re-enable (proxyProbed=true -> direct re-enable path).
	s.SetProxyTrigger("auth-1", "Qwen-72B")
	if !s.ConsumeProxyTrigger("Qwen-72B", now) {
		t.Fatal("2nd enable must succeed")
	}
	s.HandleProxyOnSuccess() // 2nd disable -> must STILL restore host-orig

	mu.Lock()
	defer mu.Unlock()
	// Expected: enable(plugin) -> restore(host-orig) -> enable(plugin) -> restore(host-orig).
	if len(toggled) != 4 {
		t.Fatalf("toggler calls = %v, want 4", toggled)
	}
	if toggled[1] != "host-orig" || toggled[3] != "host-orig" {
		t.Fatalf("both disables must restore host-orig, got %v", toggled)
	}
}

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

// Fix #4: when an enable is already in flight (proxyEnabling=true), a
// concurrent ConsumeProxyTrigger with a pending trigger must wait and return
// IsProxyActive(), NOT race into reenableProxy and issue a duplicate PUT.
func TestFixProxyEnablingGuardsConcurrentReenable(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(proxyCfg("http://127.0.0.1:8888", "modelscope"))
	s.SetProxyWait(time.Millisecond)
	s.SetProbeFunc(func(string) bool { return true })
	called := false
	s.SetProxyToggler(func(string) error { called = true; return nil })

	// Simulate an enable already in flight: proxyProbed=true (so the re-enable
	// branch would be taken) AND proxyEnabling=true (the guard). A pending
	// trigger must now wait and return IsProxyActive() (false), without
	// touching the toggler.
	s.proxyMu.Lock()
	s.proxyProbed = true
	s.proxyEnabling = true
	s.proxyMu.Unlock()
	s.SetProxyTrigger("auth-1", "Qwen-72B")

	if s.ConsumeProxyTrigger("Qwen-72B", now) {
		t.Fatal("with proxyEnabling in flight and proxy inactive, must return false (waited)")
	}
	if called {
		t.Fatal("toggler must not be called while another enable is in flight")
	}
}
