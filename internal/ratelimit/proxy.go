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
	proxyWaitDuration  = 2 * time.Second
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

// IsProxyActive reports whether the global proxy is currently enabled.
func (s *Store) IsProxyActive() bool {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	return s.proxyActive
}

// IsProxyProbeFailed reports whether a previous proxy probe failed. When
// true, subsequent 429s skip the probe and fall back to the
// insufficient_quota_cooldown mechanism. Resets when the proxy is
// successfully enabled or disabled.
func (s *Store) IsProxyProbeFailed() bool {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	return s.proxyProbeFailed
}

// SetProxyTrigger is called from OnUsage (which runs asynchronously and
// cannot block the retry loop) when a 429 is detected from a managed
// provider and proxy_url is configured. It just sets a flag — the actual
// 2s wait + probe + enable happens in ConsumeProxyTrigger, called from
// SchedulerPick which IS in the synchronous retry loop.
func (s *Store) SetProxyTrigger() {
	s.proxyMu.Lock()
	s.proxyTriggerPending = true
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
		// No pending trigger. If proxy is already active, wait 2s for
		// rotation; otherwise no-op.
		if active {
			s.log.Printf("modelscope-ratelimit: block %s model=%s (proxy rotation)",
				proxyWaitDuration, s.displayName(model))
			time.Sleep(proxyWaitDuration)
			return true
		}
		return false
	}
	s.proxyTriggerPending = false
	if s.proxyActive {
		s.proxyMu.Unlock()
		s.log.Printf("modelscope-ratelimit: block %s model=%s (proxy rotation)",
			proxyWaitDuration, s.displayName(model))
		time.Sleep(proxyWaitDuration)
		return true
	}
	// Probe already failed once — don't re-probe; fall back to cooldown.
	if s.proxyProbeFailed {
		s.proxyMu.Unlock()
		return false
	}
	// Already probed successfully before (proxy was disabled on success/
	// cleanup). Skip probe, directly re-enable the proxy via the toggler.
	if s.proxyProbed {
		s.proxyMu.Unlock()
		s.log.Printf("modelscope-ratelimit: block %s model=%s (proxy re-enable after prior probe)",
			proxyWaitDuration, s.displayName(model))
		time.Sleep(proxyWaitDuration)
		return s.reenableProxy(model, now)
	}
	if s.proxyEnabling {
		s.proxyMu.Unlock()
		time.Sleep(proxyWaitDuration)
		return s.IsProxyActive()
	}
	s.proxyEnabling = true
	s.proxyMu.Unlock()

	// First-time probe: wait 2s, then probe the proxy.
	s.log.Printf("modelscope-ratelimit: block %s model=%s (proxy wait before probe)",
		proxyWaitDuration, s.displayName(model))
	time.Sleep(proxyWaitDuration)

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

// reenableProxy saves the host's original proxy URL and enables the global
// proxy via the management API. Shared by first-time and subsequent enables.
func (s *Store) reenableProxy(model string, now time.Time) bool {
	cfg := s.config()

	// Save the host's current proxy URL so it can be restored on disable.
	var original string
	if getter := s.proxyURLGetter.Load(); getter != nil {
		if cur, err := (*getter)(); err == nil {
			original = cur
		}
	}

	// Enable the global proxy: set it to the plugin's configured proxy URL.
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
	s.originalProxyURL = original
	if s.proxyTimer != nil {
		s.proxyTimer.Stop()
	}
	s.proxyTimer = time.AfterFunc(proxySafetyTimeout, func() {
		s.disableProxy("safety timeout")
	})
	s.proxyMu.Unlock()

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
	s.originalProxyURL = ""
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
