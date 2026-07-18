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

	proxyProbeTimeout  = 10 * time.Second
	proxyWaitDuration  = 1 * time.Second
	proxySafetyTimeout = 60 * time.Second
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

// SnapshotHostProxy records the host's current upstream proxy URL ONCE, at
// configure time, so disableProxy can restore it later. Moving the read off
// the hot 429 path (where reenableProxy used to GET every enable) eliminates
// the hazard where a transient GET failure left originalProxyURL empty and
// caused disable to DELETE a host proxy that actually existed.
//
// Called from the C-ABI glue after proxyURLGetter is injected. If the plugin's
// own proxy is currently active (reload-while-active edge case), the existing
// boot snapshot is kept instead of being overwritten with the plugin's proxy
// URL. A GET failure at configure logs a warning and leaves the snapshot
// empty (disable then clears the plugin proxy, matching "host had none").
func (s *Store) SnapshotHostProxy() {
	s.proxyMu.Lock()
	if s.proxyActive || s.proxyEnabling {
		// Proxy active or mid-enable: keep the prior boot snapshot. During
		// the enable window (proxyEnabling true) the host may already be
		// routed through the plugin proxy while proxyActive is still false,
		// so a GET here would capture the plugin's own URL — keep the prior
		// snapshot instead of overwriting it.
		s.proxyMu.Unlock()
		return
	}
	s.proxyMu.Unlock()

	var current string
	if getter := s.proxyURLGetter.Load(); getter != nil {
		if cur, err := (*getter)(); err != nil {
			s.log.Printf("modelscope-ratelimit: failed to snapshot host proxy at configure: %v (restore will clear plugin proxy on disable)", err)
		} else {
			current = cur
		}
	}
	s.proxyMu.Lock()
	s.originalProxyURL = current
	s.proxyMu.Unlock()
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
		// 60s): the 1s rotation wait is the "1s" portion, and the cooldown
		// block in SchedulerPick adds the backoff portion (10/20/40/60). The
		// first proxy-enable 429 (proxy was inactive) does NOT reach here, so
		// the backoff level stays 0 until the second 429.
		s.log.Printf("modelscope-ratelimit: block %s model=%s (proxy rotation)",
			s.proxyWait(), s.displayName(model))
		time.Sleep(s.proxyWait())
		if authID != "" && cfg.InsufficientQuotaCooldown > 0 {
			s.setCooldown(authID, cfg.InsufficientQuotaCooldown, now)
		}
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
		s.log.Printf("modelscope-ratelimit: block %s model=%s (proxy re-enable after prior probe)",
			s.proxyWait(), s.displayName(model))
		time.Sleep(s.proxyWait())
		return s.reenableProxy(model, now)
	}
	s.proxyEnabling = true
	s.proxyMu.Unlock()

	// First-time probe: wait 2s, then probe the proxy.
	s.log.Printf("modelscope-ratelimit: block %s model=%s (proxy wait before probe)",
		s.proxyWait(), s.displayName(model))
	time.Sleep(s.proxyWait())

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
// enables. The host's original proxy URL is snapshotted once at configure
// (SnapshotHostProxy) and restored from originalProxyURL in disableProxy;
// reenableProxy never reads or writes originalProxyURL.
func (s *Store) reenableProxy(model string, now time.Time) bool {
	cfg := s.config()

	// Enable the global proxy: PUT the plugin's configured proxy URL. The
	// host's original proxy was snapshotted once at configure (SnapshotHostProxy)
	// and is restored from originalProxyURL in disableProxy. reenableProxy
	// never re-reads the management API, so a transient GET failure on the hot
	// 429 path cannot corrupt the restore value.
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
	// originalProxyURL intentionally untouched: snapshotted once at
	// configure (SnapshotHostProxy) and retained across disable/re-enable
	// cycles so every disable can restore the host's original proxy.
	s.proxyMu.Unlock()

	// Increment the daily backoff trigger counter so the status page
	// reflects proxy enables alongside insufficient_quota cooldowns.
	s.cooldownStatsMu.Lock()
	s.cooldownCount++
	s.cooldownStatsMu.Unlock()
	if s.proxyTimer != nil {
		s.proxyTimer.Stop()
	}
	s.proxyTimer = time.AfterFunc(proxySafetyTimeout, func() {
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
