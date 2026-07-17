package ratelimit

import (
	"strconv"
	"strings"
	"time"
)

// headerInt returns the first integer value of the named header. Header
// lookup is case-insensitive (HTTP headers are case-insensitive, and upstream
// proxies may rewrite casing). present is false when the header is absent or
// not a valid integer.
func headerInt(headers map[string][]string, name string) (value int, present bool) {
	if name == "" {
		return 0, false
	}
	values := lookupHeader(headers, name)
	if len(values) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil {
		return 0, false
	}
	return n, true
}

// lookupHeader finds all values for a header name, case-insensitively.
func lookupHeader(headers map[string][]string, name string) []string {
	if len(headers) == 0 {
		return nil
	}
	target := strings.ToLower(name)
	for k, vs := range headers {
		if strings.ToLower(k) == target {
			return vs
		}
	}
	return nil
}

// ApplyRateLimit inspects Modelscope rate-limit response headers and records
// the appropriate disable. It is the single source of truth shared by the
// response interceptor, the stream interceptor (header-init phase) and the
// usage hook.
//
//   - No rate-limit headers present  => not a Modelscope response, no-op.
//   - Total remaining exhausted      => global disable (all models).
//   - Model remaining exhausted      => per-model disable only.
func (s *Store) ApplyRateLimit(authID, model string, headers map[string][]string, now time.Time) {
	if authID == "" {
		// Without an auth id we cannot attribute the disable; the usage hook
		// (which always carries AuthID) will catch it.
		return
	}
	cfg := s.config()
	if !cfg.ManagesModel(model) {
		return
	}
	modelRem, modelOK := headerInt(headers, cfg.ModelRemainingHeader)
	totalRem, totalOK := headerInt(headers, cfg.TotalRemainingHeader)
	modelLim, modelLimOK := headerInt(headers, cfg.ModelLimitHeader)
	totalLim, totalLimOK := headerInt(headers, cfg.TotalLimitHeader)
	if !modelOK && !totalOK {
		return // not a rate-limited Modelscope response
	}
	// Presence of Modelscope rate-limit headers identifies a Modelscope
	// response; record this credential so total/available counts stay
	// accurate even when scheduler.pick does not fire for this provider.
	s.recordSeen([]string{authID})
	// Store the last-seen remaining/limit so the status page can show per-key
	// and per-model availability (day-bounded; cleared at the midnight reset).
	s.recordRemaining(authID, model, now,
		modelRem, modelOK, modelLim, modelLimOK,
		totalRem, totalOK, totalLim, totalLimOK)
	// Global exhaustion takes precedence and covers every model.
	if totalOK && totalRem <= cfg.DisableThreshold {
		s.disableGlobal(authID, model, now)
		return
	}
	if modelOK && modelRem <= cfg.DisableThreshold {
		s.disableModel(authID, model, now)
		return
	}
	// No disable triggered: remaining is above threshold. The backoff reset
	// is NOT done here — a 429+insufficient_quota response can still carry
	// rate-limit headers with remaining > 0 (normal per-request rate limiting
	// vs. billing-quota exhaustion), which would erroneously reset the
	// exponential backoff on every failure. The reset is gated on success
	// (rec.Failed == false) in OnUsage instead.
}

// isInsufficientQuota reports whether the response is a 429 with an
// "insufficient_quota" error code in the body (e.g. Aliyun Model Studio).
// Detection is string-based ("insufficient_quota" with surrounding quotes) to
// avoid a full JSON parse in the hot path; the token is distinctive enough.
func isInsufficientQuota(statusCode int, body string) bool {
	return statusCode == 429 && strings.Contains(body, `"insufficient_quota"`)
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
	if !cfg.ManagesModel(model) {
		return
	}
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
	s.ApplyRateLimit(authID, req.Model, req.ResponseHeaders, s.now())
	// Skip insufficient_quota cooldown when proxy mode is active (proxy_url
	// configured and probe not failed) — the proxy trigger handles 429s
	// instead, and a cooldown here would conflict with the 2s proxy wait.
	cfg := s.config()
	if !(cfg.ProxyURL != "" && !s.IsProxyProbeFailed()) {
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
		s.ApplyRateLimit(authID, req.Model, req.ResponseHeaders, s.now())
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
	// Record the alias -> upstream mapping first so disableModel's log (fired
	// below via ApplyRateLimit) can display the upstream name.
	s.recordAlias(rec.Alias, rec.Model)
	model := strings.TrimSpace(rec.Alias)
	if model == "" {
		model = rec.Model
	}
	now := s.now()
	s.ApplyRateLimit(rec.AuthID, model, rec.ResponseHeaders, now)
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
		// Disable the global proxy if it was active — a managed provider
		// succeeded, so the proxy is no longer needed.
		s.HandleProxyOnSuccess()
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
			// 401 Unauthorized: the API key is invalid or expired. Disable
			// the credential globally for the rest of the day so the
			// scheduler skips it. The status page shows "密钥失效".
			if rec.Failure.StatusCode == 401 {
				s.recordSeen([]string{rec.AuthID})
				s.disableUnauthorized(rec.AuthID, now)
				return
			}
			// Proxy mode: on 429+insufficient_quota from a managed provider.
			// When proxy_url is configured and the probe has NOT already
			// failed, set a trigger flag for SchedulerPick to consume (2s
			// wait + probe + enable). When the probe already failed once,
			// fall back to the insufficient_quota_cooldown mechanism.
			if isInsufficientQuota(rec.Failure.StatusCode, rec.Failure.Body) && cfg.ProxyURL != "" && !s.IsProxyProbeFailed() {
				s.SetProxyTrigger()
				return
			}
			s.ApplyInsufficientQuotaCooldown(rec.AuthID, model, rec.Failure.StatusCode, rec.Failure.Body, now)
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
