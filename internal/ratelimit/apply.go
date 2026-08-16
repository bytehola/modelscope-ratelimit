package ratelimit

import (
	"strings"
	"time"
)

// ApplyRateLimit inspects Modelscope API responses and records the appropriate
// disable. It is the single source of truth shared by the response interceptor,
// the stream interceptor (header-init phase) and the usage hook.
//
//   - 429 + "insufficient balance"  => global disable (magic cube exhausted).
func (s *Store) ApplyRateLimit(authID, model string, headers map[string][]string, statusCode int, body string, now time.Time) {
	if authID == "" {
		// Without an auth id we cannot attribute the disable; the usage hook
		// (which always carries AuthID) will catch it.
		return
	}
	// Daily-disable on 429 + "insufficient balance" (account balance
	// exhausted — not a rate window, the key is genuinely unusable until
	// the balance is topped up). Checked before rate-limit headers because
	// insufficient balance responses may not carry rate-limit headers. Not
	// gated by skip_provider_on_429: balance exhaustion is not a
	// recoverable rate window, so the skip/proxy logic must not bypass the
	// daily disable.
	if IsInsufficientBalance(statusCode, body) {
		s.recordSeen([]string{authID})
		s.disableGlobal(authID, model, now)
		return
	}
}

// isInsufficientQuota reports whether the response is a 429 with an
// "insufficient_quota" error code in the body (e.g. Aliyun Model Studio).
// Detection is string-based ("insufficient_quota" with surrounding quotes) to
// avoid a full JSON parse in the hot path; the token is distinctive enough.
func isInsufficientQuota(statusCode int, body string) bool {
	return statusCode == 429 && strings.Contains(body, `"insufficient_quota"`)
}

// IsInsufficientBalance reports whether the response is a 429 with an
// "insufficient balance" phrase in the body (account balance exhausted).
// Unlike insufficient_quota (a pool-level rate window), insufficient balance
// means the key is genuinely unusable until the balance is topped up, so it
// always triggers a daily global disable regardless of skip_provider_on_429.
// Detection is case-insensitive substring match on "insufficient balance"
// (with a space, not underscore) to catch various JSON layouts.
func IsInsufficientBalance(statusCode int, body string) bool {
	if statusCode != 429 {
		return false
	}
	return strings.Contains(strings.ToLower(body), "insufficient balance")
}

// ApplyInsufficientQuotaCooldown checks for a 429 + insufficient_quota response
// and, if detected, sets a short-term cooldown on the key so the scheduler
// picks a different key for the configured duration. Unlike a daily disable,
// the cooldown auto-expires after X seconds and the key becomes available
// again without waiting for midnight.
func (s *Store) ApplyInsufficientQuotaCooldown(authID, model string, statusCode int, body string, now time.Time) {
	if authID == "" {
		return
	}
	cfg := s.config()
	if cfg.InsufficientQuotaCooldown <= 0 {
		return
	}
	if !isInsufficientQuota(statusCode, body) {
		return
	}
	s.recordSeen([]string{authID})
	s.setCooldown(authID, cfg.InsufficientQuotaCooldown, now)
}

// OnResponse handles response.intercept_after. It parses the response headers
// and updates state, returning a no-op interception result (the plugin only
// observes; it never rewrites Modelscope responses).
func (s *Store) OnResponse(req ResponseInterceptRequest) ResponseInterceptResponse {
	authID := AuthIDFromMetadata(req.Metadata)
	s.ApplyRateLimit(authID, req.Model, req.ResponseHeaders, req.StatusCode, req.Body, s.now())
	// Skip insufficient_quota cooldown when proxy mode is active (proxy_url
	// configured and probe not failed) — the proxy trigger handles 429s
	// instead, and a cooldown here would conflict with the 2s proxy wait.
	cfg := s.config()
	if !(cfg.ProxyURL != "" && !s.IsProxyProbeFailed()) && !cfg.SkipProviderOn429 {
		s.ApplyInsufficientQuotaCooldown(authID, req.Model, req.StatusCode, req.Body, s.now())
	}
	return ResponseInterceptResponse{}
}

// OnStreamChunk handles response.intercept_stream_chunk. Only the header-only
// initialisation call (ChunkIndex == -1) carries the rate-limit headers; data
// chunks are passed through unchanged.
func (s *Store) OnStreamChunk(req StreamChunkInterceptRequest) StreamChunkInterceptResponse {
	if req.ChunkIndex == -1 {
		authID := AuthIDFromMetadata(req.Metadata)
		s.ApplyRateLimit(authID, req.Model, req.ResponseHeaders, 0, "", s.now())
	}
	return StreamChunkInterceptResponse{}
}

// OnUsage handles usage.handle. UsageRecord carries AuthID, Model and the
// response headers together, so this is the authoritative correlation path
// (covering both streaming and non-streaming responses).
// OnUsage handles usage.handle. UsageRecord carries AuthID, Model and the
// response headers together, so this is the authoritative correlation path
// (covering both streaming and non-streaming responses).
//
// Model-name reconciliation: CLIProxyAPI's scheduler (SchedulerPick) receives
// the *client-requested model alias* (e.g. "glm-5.2"), not the resolved
// upstream model name. A usage record, however, carries the resolved upstream
// model in rec.Model (e.g. "ZhipuAI/GLM-5.2") and the requested alias in
// rec.Alias. Because 429 responses take the executor's error path (the host
// returns early and never calls the response interceptor), this hook is the
// only path that records disables from rate-limited responses, and it keyed
// them under the upstream name. The scheduler then checked isDisabled under the
// alias, found nothing, and — once the host's own short-lived cooldown expired
// while this plugin's daily disable persisted — re-routed to an exhausted key,
// hitting upstream 429s anyway. Key disables under the same name the scheduler
// checks (the alias) so isDisabled(id, alias) matches. Fall back to rec.Model
// when no alias is configured (alias == upstream).
func (s *Store) OnUsage(rec UsageRecord) {
	// Record the alias -> upstream mapping first so the disable log (fired
	// below via ApplyRateLimit) can display the upstream name.
	s.recordAlias(rec.Alias, rec.Model)
	model := strings.TrimSpace(rec.Alias)
	if model == "" {
		model = rec.Model
	}
	now := s.now()
	// Extract status/body for ApplyRateLimit's skip_provider_on_429 gate:
	// only insufficient_quota 429s skip the daily disable; other 429s
	// (exceeded-today, limit_burst_rate) still get daily-disabled.
	var statusCode int
	var body string
	if rec.Failed && rec.Failure != nil {
		statusCode = rec.Failure.StatusCode
		body = rec.Failure.Body
	}
	s.ApplyRateLimit(rec.AuthID, model, rec.ResponseHeaders, statusCode, body, now)
	cfg := s.config()
	// A successful (non-failed) response from a managed provider means the
	// shared upstream quota recovered; reset the exponential backoff so the
	// next insufficient_quota failure restarts from the configured base. Only
	// managed providers trigger the reset — a 200 from a non-managed fallback
	// (e.g. Aliyun) must not reset the backoff of an unrelated exhausted
	// managed provider. This is NOT done inside ApplyRateLimit because a
	// 429+insufficient_quota response can carry rate-limit headers with
	// remaining > 0 (per-request rate limiting vs. billing-quota exhaustion),
	// which would wrongly reset the backoff on every failure.
	if !rec.Failed && cfg.ManagesProvider(rec.Provider) {
		s.resetCooldownBackoff()
		s.recordSuccess(rec.AuthID)
		s.recordModelUsage(s.displayName(model))
		// Disable the global proxy if it was active — a managed provider
		// succeeded, so the proxy is no longer needed.
		s.HandleProxyOnSuccess()
	}
	// Clear the per-provider skip set on ANY 200 (managed or non-managed) so
	// the next request probes the free managed quota again. This is the
	// "prioritise free quota" strategy: every new request tries managed first,
	// and only on a 429+insufficient_quota does it skip to non-managed.
	if !rec.Failed {
		s.ClearAllSkippedProviders()
	}

	// 429 responses take the executor's error path (the host returns early and
	// never calls the response interceptor), so OnUsage is the authoritative
	// path for detecting 429s. The usage hook fires for EVERY provider (the
	// host's usage manager dispatches each record to all plugins
	// unconditionally), so a non-managed fallback provider (e.g. Aliyun Model
	// Studio) that also returns 429+insufficient_quota must NOT receive a
	// cooldown or trigger proxy mode. Guard with ManagesProvider so only
	// monitored providers are affected.
	if rec.Failed && rec.Failure != nil {
		if cfg.ManagesProvider(rec.Provider) {
			// Record 429 from managed providers (daily counter for status page).
			if rec.Failure.StatusCode == 429 {
				s.recordRateLimit()
			}
			// 401 Unauthorized: the API key is invalid or expired. Disable the
			// credential globally until plugin restart (not reset at midnight) so
			// the scheduler skips it. The status page shows "密钥失效".
			if rec.Failure.StatusCode == 401 {
				s.recordSeen([]string{rec.AuthID})
				s.disableUnauthorized(rec.AuthID, now)
				return
			}
			// skip_provider_on_429 mode: on an insufficient_quota 429 from a
			// managed provider, add it to the per-provider skip set so
			// subsequent SchedulerPick calls filter out its candidates. Only
			// the provider that 429'd is skipped; other managed providers
			// remain available. Latching: the entry persists across
			// non-managed failures and is cleared by any 200 (OnUsage) or
			// when all managed candidates are tried. Other 429 types
			// (exceeded-today, limit_burst_rate) fall through to normal
			// daily-disable + cooldown handling.
			// When proxy_url is also configured, the skip and proxy modes
			// complement each other: skip provides immediate relief (go to
			// other providers), proxy provides different-IP retry when the
			// skip is cleared (any 200). Both are armed on the same 429 —
			// fall through to the proxy trigger below instead of returning.
			if cfg.SkipProviderOn429 && isInsufficientQuota(rec.Failure.StatusCode, rec.Failure.Body) {
				s.recordSeen([]string{rec.AuthID})
				providerName := extractProviderName(rec.Provider)
				s.SetSkipProvider(providerName)
				s.log.Printf("modelscope-ratelimit: managed 429, skip provider %s model=%s", providerName, s.displayName(model))
				// Fall through: also arm proxy trigger if configured.
			}
			// Proxy mode: on 429+insufficient_quota from a managed provider.
			// When proxy_url is configured and the probe has NOT already
			// failed, set a trigger flag for SchedulerPick to consume (2s
			// wait + probe + enable). When the probe already failed once,
			// fall back to the insufficient_quota_cooldown mechanism.
			if isInsufficientQuota(rec.Failure.StatusCode, rec.Failure.Body) && cfg.ProxyURL != "" && !s.IsProxyProbeFailed() {
				s.SetProxyTrigger(rec.AuthID, model)
				return
			}
			// Cooldown fallback: only when skip mode is off (skip mode
			// handles 429s via the skip set, not cooldowns). The cooldown
			// block in SchedulerPick is also bypassed when skip mode is on.
			if !cfg.SkipProviderOn429 {
				s.ApplyInsufficientQuotaCooldown(rec.AuthID, model, rec.Failure.StatusCode, rec.Failure.Body, now)
			}
		}
	}
}

// authIDKeys are the metadata keys tried, in priority order, when correlating
// a response interceptor call back to a credential. The host context snapshot
// (Metadata) is treated as read-only; if none of these keys are present the
// usage hook still provides the correlation.
var authIDKeys = []string{
	"auth_id", "AuthID", "authID", "authId", "auth",
}

// AuthIDFromMetadata defensively extracts a credential identifier from the
// host context snapshot attached to a response/stream interception request.
func AuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	for _, k := range authIDKeys {
		if v, ok := meta[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
