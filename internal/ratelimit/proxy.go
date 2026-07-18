package ratelimit

import (
	"net/http"
	"net/url"
	"time"
)

// Proxy mode constants. When proxy_url is configured in the plugin config,
// a 429 from a managed provider triggers proxy mode instead of the
// insufficient_quota_cooldown mechanism.
const (
	// proxyProbeURL is the endpoint used to verify proxy reachability. Any
	// HTTP response (even 401/429) means the proxy is working; a timeout or
	// connection error means the proxy is unavailable.
	proxyProbeURL = "https://api-inference.modelscope.cn"

	proxyProbeTimeout = 10 * time.Second
	proxyWaitDuration = 1 * time.Second
	// proxySafetyTimeout auto-disables a forgotten proxy after this long with
	// no 429/200 activity. It must be STRICTLY GREATER than
	// maxInsufficientQuotaCooldown (the longest a cooldown block can sleep):
	// otherwise, at the 60s backoff cap, the safety timer fires DURING the
	// cooldown block, disabling the proxy mid-block. After the block the next
	// 429 re-enables the proxy via the no-cooldown re-enable path, and since
	// the old cooldown expired, the following request isn't blocked — bursting
	// two requests. 2x the cap gives a 60s quiescent margin after the block.
	proxySafetyTimeout = 2 * maxInsufficientQuotaCooldown * time.Second
)

// SetProxyToggler injects the function that sets or clears the global
// upstream proxy URL via the CLIProxyAPI management API. A non-empty
// proxyURL sets it (PUT /v0/management/proxy-url); an empty string clears
// it (DELETE). Called once from the C-ABI glue during configuration.
func (s *Store) SetProxyToggler(fn func(proxyURL string) error) {
	s.proxyToggler.Store(&fn)
}

// SetProxyURLGetter injects the function that reads the current global
// proxy URL from the management API (GET /v0/management/proxy-url).
// Used to save the host's original proxy so it can be restored when the
// plugin disables its own proxy.
func (s *Store) SetProxyURLGetter(fn func() (string, error)) {
	s.proxyURLGetter.Store(&fn)
}

// snapshotHostProxyOnce records the host's current upstream proxy URL the
// first time the plugin is about to enable its own proxy. It is called from
// reenableProxy, which runs on the first 429 — by then the host API server is
// already serving requests (unlike configure, which fires before the server
// listens, so a configure-time GET always fails with connection refused).
//
// Idempotent: once the snapshot succeeds (originalSnapshotted=true) it is
// never re-read, so later re-enables reuse the stored value. Returns true if
// a snapshot is available (already taken or just taken); false if the GET
// failed or no getter is injected, in which case reenableProxy must NOT
// enable the proxy — a missing snapshot would make disableProxy DELETE the
// host's real proxy instead of restoring it.
func (s *Store) snapshotHostProxyOnce() bool {
	s.proxyMu.Lock()
	if s.originalSnapshotted {
		s.proxyMu.Unlock()
		return true
	}
	s.proxyMu.Unlock()

	// reenableProxy is the proxyEnabling owner, so no concurrent caller
	// reaches here; the flag check above plus the write below under proxyMu
	// are sufficient.
	var current string
	ok := false
	if getter := s.proxyURLGetter.Load(); getter != nil {
		if v, err := (*getter)(); err == nil {
			current = v
			ok = true
		} else {
			s.log.Printf("modelscope-ratelimit: failed to snapshot host proxy: %v (proxy enable deferred)", err)
		}
	}
	s.proxyMu.Lock()
	if ok {
		s.originalProxyURL = current
		s.originalSnapshotted = true
	}
	s.proxyMu.Unlock()
	return ok
}

// IsProxyActive reports whether the global proxy is currently enabled.
func (s *Store) IsProxyActive() bool {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	return s.proxyActive
}

// IsProxyProbeFailed reports whether a previous proxy probe failed. When
// true, subsequent 429s skip the probe and fall back to the
// insufficient_quota_cooldown mechanism. The flag persists until plugin
// reload (disabled proxies do NOT reset it, so a failed probe stays
// failed).
func (s *Store) IsProxyProbeFailed() bool {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	return s.proxyProbeFailed
}

// OriginalProxyURL returns the host's proxy URL snapshotted at configure
// time (the value disableProxy restores). Exposed for tests/status.
func (s *Store) OriginalProxyURL() string {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	return s.originalProxyURL
}

// SetProxyTrigger is called from OnUsage (which runs asynchronously and
// cannot block the retry loop) when a 429 is detected from a managed
// provider and proxy_url is configured. It just sets a flag — the actual
// 2s wait + probe + enable happens in ConsumeProxyTrigger, called from
// SchedulerPick which IS in the synchronous retry loop.
func (s *Store) SetProxyTrigger(authID, model string) {
	s.proxyMu.Lock()
	s.proxyTriggerPending = true
	s.proxyTriggerAuthID = authID
	s.proxyTriggerModel = model
	s.proxyMu.Unlock()
}

// ConsumeProxyTrigger is called from SchedulerPick (synchronous, in the
// retry loop). If a 429 trigger is pending, it waits 2s, probes the proxy,
// and enables it if reachable. If the proxy is already active, it just
// waits 2s for key rotation. Returns true if proxy mode is active.
func (s *Store) ConsumeProxyTrigger(model string, now time.Time) bool {
	cfg := s.config()
	if cfg.ProxyURL == "" {
		return false
	}

	s.proxyMu.Lock()
	if !s.proxyTriggerPending {
		active := s.proxyActive
		s.proxyMu.Unlock()
		// No pending 429 trigger. If the proxy is already active, proceed
		// immediately to the next key (no artificial rotation delay): no 429
		// has been observed since the proxy was enabled, so there is nothing
		// to back off from. Otherwise no-op.
		return active
	}
	s.proxyTriggerPending = false
	authID := s.proxyTriggerAuthID
	if s.proxyActive {
		s.proxyMu.Unlock()
		// A subsequent 429 while the proxy is already active means the new IP
		// did not resolve the quota exhaustion. Escalate via the shared
		// insufficient_quota exponential backoff (base -> x2 -> ... capped at
		// 60s). No separate rotation sleep here: the proxy is already up, so
		// the cooldown block in SchedulerPick blocks for the backoff duration
		// (base insufficient_quota_cooldown + jitter) directly — the 2nd 429
		// thus waits ~insufficient_quota_cooldown, not 1s+cooldown.
		// The first proxy-enable 429 (proxy was inactive) does NOT reach here,
		// so the backoff level stays 0 until the second 429.
		if authID != "" && cfg.InsufficientQuotaCooldown > 0 {
			s.setCooldown(authID, cfg.InsufficientQuotaCooldown, now)
		}
		// A 429 while the proxy is active means the proxy is still needed;
		// reset the safety timer so it only fires after proxySafetyTimeout of
		// NO 429 activity (quiescence), not a fixed 60s from the initial
		// enable. Otherwise the proxy cycles off mid-exhaustion every 60s,
		// resetting the backoff each cycle.
		s.proxyMu.Lock()
		if s.proxyTimer != nil {
			s.proxyTimer.Reset(s.proxySafetyTimeoutDur())
		} else {
			s.proxyTimer = time.AfterFunc(s.proxySafetyTimeoutDur(), func() {
				s.disableProxy("safety timeout")
			})
		}
		s.proxyMu.Unlock()
		return true
	}
	// Probe already failed once — don't re-probe; fall back to cooldown.
	if s.proxyProbeFailed {
		s.proxyMu.Unlock()
		return false
	}
	// An enable is already in flight (another SchedulerPick goroutine is
	// mid-probe or mid-re-enable). Wait for it instead of racing into
	// reenableProxy ourselves: this guard must come BEFORE the proxyProbed
	// branch because proxyProbed is sticky (true after the first successful
	// probe), so without this ordering every concurrent caller would take the
	// re-enable path and issue a duplicate PUT to the management API.
	if s.proxyEnabling {
		s.proxyMu.Unlock()
		time.Sleep(s.proxyWait())
		return s.IsProxyActive()
	}
	// Already probed successfully before (proxy was disabled on success/
	// cleanup). Skip probe, directly re-enable the proxy via the toggler.
	// proxyEnabling is false here (checked above), so this goroutine owns the
	// enable and concurrent callers wait via the branch above.
	if s.proxyProbed {
		s.proxyEnabling = true
		s.proxyMu.Unlock()
		// Re-enable immediately (no artificial rotation delay): the proxy was
		// already probed successfully, so just PUT it back and proceed.
		return s.reenableProxy(model, now)
	}
	s.proxyEnabling = true
	s.proxyMu.Unlock()

	// First-time probe: probe immediately (no artificial pre-wait), then
	// enable. The request proceeds right after the proxy is up.
	if !s.probeProxy(cfg.ProxyURL) {
		s.log.Printf("modelscope-ratelimit: proxy probe failed, falling back to insufficient_quota_cooldown")
		s.proxyMu.Lock()
		s.proxyEnabling = false
		s.proxyProbed = true
		s.proxyProbeFailed = true
		s.proxyMu.Unlock()
		return false
	}

	s.proxyMu.Lock()
	s.proxyProbed = true
	s.proxyMu.Unlock()

	return s.reenableProxy(model, now)
}

// reenableProxy enables the global proxy via the management API (PUT of the
// plugin's configured proxy URL). Shared by first-time and subsequent
// enables. The host's original proxy URL is snapshotted once (lazily, on the
// first enable via snapshotHostProxyOnce) and restored from originalProxyURL
// in disableProxy; later enables reuse the stored snapshot without re-GET.
func (s *Store) reenableProxy(model string, now time.Time) bool {
	cfg := s.config()

	// Snapshot the host's current proxy URL the first time we enable ours.
	// Done here (first 429, server up) not at configure (server not up yet).
	// If unavailable, do NOT enable: a missing snapshot would make disable
	// DELETE the host's real proxy instead of restoring it.
	if !s.snapshotHostProxyOnce() {
		s.proxyMu.Lock()
		s.proxyEnabling = false
		s.proxyMu.Unlock()
		return false
	}

	// Enable the global proxy: PUT the plugin's configured proxy URL. The
	// host's original proxy was snapshotted once above (snapshotHostProxyOnce,
	// on the first enable) and is restored from originalProxyURL in
	// disableProxy. reenableProxy never re-reads the management API on
	// subsequent enables, so the restore value stays stable.
	fn := s.proxyToggler.Load()
	if fn != nil {
		if err := (*fn)(cfg.ProxyURL); err != nil {
			s.log.Printf("modelscope-ratelimit: failed to enable proxy: %v", err)
			s.proxyMu.Lock()
			s.proxyEnabling = false
			s.proxyMu.Unlock()
			return false
		}
	}

	s.proxyMu.Lock()
	s.proxyActive = true
	s.proxyEnabling = false
	s.proxyProbeFailed = false
	s.proxyEnabledAt = now
	// originalProxyURL intentionally untouched here: snapshotted once on the
	// first enable and retained across disable/re-enable cycles so every
	// disable can restore the host's original proxy.
	s.proxyMu.Unlock()

	// Increment the daily backoff trigger counter so the status page
	// reflects proxy enables alongside insufficient_quota cooldowns.
	s.cooldownStatsMu.Lock()
	s.cooldownCount++
	s.cooldownStatsMu.Unlock()
	if s.proxyTimer != nil {
		s.proxyTimer.Stop()
	}
	s.proxyTimer = time.AfterFunc(s.proxySafetyTimeoutDur(), func() {
		s.disableProxy("safety timeout")
	})

	s.log.Printf("modelscope-ratelimit: proxy enabled model=%s", s.displayName(model))
	return true
}

// HandleProxyOnSuccess disables the proxy when a managed provider succeeds.
// Called from OnUsage on !rec.Failed && ManagesProvider.
func (s *Store) HandleProxyOnSuccess() {
	s.disableProxy("managed success")
}

// DisableProxyIfActive disables the proxy if it is currently active.
// Called from SchedulerPick when hasManaged == false (all managed keys
// exhausted, falling through to non-managed providers).
func (s *Store) DisableProxyIfActive() {
	s.disableProxy("no managed candidates")
}

// DisableProxyOnReconfigure disables an active proxy when the plugin is
// reconfigured with proxy_url removed, restoring the host's original proxy
// URL snapshotted at configure. Called from the C-ABI glue in configure
// when the new config has an empty proxy_url. Safe to call when the proxy
// is inactive (no-op).
func (s *Store) DisableProxyOnReconfigure() {
	s.disableProxy("proxy_url removed in reconfigure")
}

// disableProxy turns off the global proxy if it is currently active, and
// restores the host's original proxy URL (if any) that was saved when the
// plugin's proxy was enabled.
func (s *Store) disableProxy(reason string) {
	s.proxyMu.Lock()
	if !s.proxyActive {
		s.proxyMu.Unlock()
		return
	}
	s.proxyActive = false
	original := s.originalProxyURL
	if s.proxyTimer != nil {
		s.proxyTimer.Stop()
		s.proxyTimer = nil
	}
	s.proxyMu.Unlock()

	// Restore the host's original proxy URL: if it was non-empty, PUT it
	// back; if empty, DELETE (clear the plugin's proxy entirely).
	fn := s.proxyToggler.Load()
	if fn != nil {
		if err := (*fn)(original); err != nil {
			s.log.Printf("modelscope-ratelimit: failed to restore proxy: %v", err)
		}
	}
	s.log.Printf("modelscope-ratelimit: proxy disabled (%s)", reason)
}

// probeProxy makes an HTTP GET to the probe URL through the configured proxy.
// Any HTTP response (even 401/429) means the proxy is reachable; a timeout
// or connection error means the proxy is unavailable.
func (s *Store) probeProxy(proxyURLStr string) bool {
	// Test seam: if a probe function was injected, use it instead of a real
	// HTTP request so unit tests never touch the network.
	if pf := s.probeFn.Load(); pf != nil && *pf != nil {
		return (*pf)(proxyURLStr)
	}
	u, err := url.Parse(proxyURLStr)
	if err != nil {
		return false
	}
	transport := &http.Transport{
		Proxy: http.ProxyURL(u),
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   proxyProbeTimeout,
	}
	resp, err := client.Get(proxyProbeURL)
	if err != nil {
		transport.CloseIdleConnections()
		return false
	}
	resp.Body.Close()
	transport.CloseIdleConnections()
	return true
}
