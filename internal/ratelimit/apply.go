package ratelimit

import (
	"encoding/json"
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
	// recoverable rate window, so the skip logic must not bypass the
	// daily disable.
	if IsInsufficientBalance(statusCode, body) {
		s.recordSeen([]string{authID})
		s.disableGlobal(authID, model, now)
		return
	}
}

// IsResponsesRateLimitExceeded reports whether a Responses-API event is the
// quota-exhaustion failure previously returned as 429 + insufficient_quota by
// the Chat Completions endpoint. Matching is on the quoted JSON value tokens
// ("response.failed" / "rate_limit_exceeded") so it tolerates whitespace
// after colons and SSE "data:" framing.
func IsResponsesRateLimitExceeded(body string) bool {
	if body == "" {
		return false
	}
	return strings.Contains(body, `"response.failed"`) &&
		strings.Contains(body, `"rate_limit_exceeded"`)
}

// IsResponsesFailed reports whether a Responses-API event is any
// response.failed (not just rate_limit_exceeded). The Responses protocol
// delivers errors as HTTP 200 SSE with a response.failed event, so the
// executor must detect ALL of them and retry the next key/provider instead
// of treating the 200 as a genuine success. Like
// IsResponsesRateLimitExceeded it matches the quoted token so JSON spacing
// and SSE framing do not cause a miss.
func IsResponsesFailed(body string) bool {
	if body == "" {
		return false
	}
	return strings.Contains(body, `"response.failed"`)
}

// responsesErrorProbe reads just enough of a top-level JSON document to tell an
// upstream error body apart from a valid Responses payload. Only top-level
// members are considered, so nested "code"/"message" fields inside tool calls or
// message content cannot produce a false positive.
type responsesErrorProbe struct {
	Object  *string          `json:"object"`
	Error   *json.RawMessage `json:"error"`
	Code    *string          `json:"code"`
	Message *string          `json:"message"`
}

// IsResponsesErrorPayload reports whether a body that arrived with HTTP 200 is
// really an error document rather than a Response resource. Modelscope answers
// some rejections (invalid parameter, unsupported model) with 200 plus a bare
// {"code":...,"message":...}, and the Responses protocol itself keeps
// "error":null on completed objects, so neither a substring test on "error" nor
// the status code is enough on its own. Callers use this to stop presenting
// upstream failures as successful responses.
func IsResponsesErrorPayload(body string) bool {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "{") {
		return false
	}
	// Cheap gate before the decode: real response bodies are large and, unless
	// they mention an error or an error code at all, cannot be error payloads.
	if !strings.Contains(body, `"error"`) && !strings.Contains(body, `"code"`) {
		return false
	}
	var probe responsesErrorProbe
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return false
	}
	if hasJSONErrorValue(probe.Error) {
		return true
	}
	if probe.Object != nil && strings.EqualFold(strings.TrimSpace(*probe.Object), "response") {
		// A Response resource with a null/empty error is a genuine success.
		return false
	}
	return probe.Code != nil && strings.TrimSpace(*probe.Code) != "" &&
		probe.Message != nil && strings.TrimSpace(*probe.Message) != ""
}

// hasJSONErrorValue reports whether an "error" member carries a payload. Null,
// an empty object/array and an empty string all mean "no error".
func hasJSONErrorValue(raw *json.RawMessage) bool {
	if raw == nil {
		return false
	}
	switch strings.TrimSpace(string(*raw)) {
	case "", "null", "{}", "[]", `""`:
		return false
	default:
		return true
	}
}

// ResponsesFirstDataEvent returns the bytes through the end of the first
// complete SSE event that contains a data field. It returns false until the
// field and an event dispatch boundary have both arrived, unless atEOF is true
// (SSE permits the final event to be dispatched by end-of-stream).
func ResponsesFirstDataEvent(body string, atEOF bool) (string, bool) {
	dataStart := firstSSEDataFieldIndex(body)
	if dataStart < 0 {
		return "", false
	}

	for i := dataStart; i < len(body); i++ {
		if body[i] != '\n' {
			continue
		}
		if i+1 < len(body) && body[i+1] == '\n' {
			return body[:i+1], true
		}
		if i+2 < len(body) && body[i+1] == '\r' && body[i+2] == '\n' {
			return body[:i+1], true
		}
	}

	if atEOF {
		return body, true
	}
	return "", false
}

func firstSSEDataFieldIndex(body string) int {
	for start := 0; start < len(body); {
		relEnd := strings.IndexByte(body[start:], '\n')
		end := len(body)
		hasNewline := relEnd >= 0
		if hasNewline {
			end = start + relEnd
		}

		line := strings.TrimSuffix(body[start:end], "\r")
		if strings.HasPrefix(line, "data:") {
			return start
		}
		if !hasNewline {
			break
		}
		start = end + 1
	}
	return -1
}

// isInsufficientQuota reports whether the response represents pool-level
// quota exhaustion: either the legacy Chat Completions 429 +
// "insufficient_quota", or the Responses-API 200 stream event
// response.failed + rate_limit_exceeded.
// Detection is string-based ("insufficient_quota" with surrounding quotes) to
// avoid a full JSON parse in the hot path; the token is distinctive enough.
func isInsufficientQuota(statusCode int, body string) bool {
	if IsResponsesRateLimitExceeded(body) {
		return true
	}
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
	if IsResponsesRateLimitExceeded(req.Body) {
		if s.consumeResponsesRateLimitApplied(authID, s.now()) {
			return ResponseInterceptResponse{}
		}
		s.ApplyResponsesRateLimit(
			authID,
			ProviderFromMetadata(req.Metadata),
			req.RequestedModel,
			req.Body,
			s.now(),
		)
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
		return StreamChunkInterceptResponse{}
	}
	if IsResponsesRateLimitExceeded(req.Body) {
		authID := AuthIDFromMetadata(req.Metadata)
		if s.consumeResponsesRateLimitApplied(authID, s.now()) {
			return StreamChunkInterceptResponse{}
		}
		s.ApplyResponsesRateLimit(
			authID,
			ProviderFromMetadata(req.Metadata),
			req.RequestedModel,
			req.Body,
			s.now(),
		)
	}
	return StreamChunkInterceptResponse{}
}

// applyResponsesRateLimit handles the Responses-API quota failure that now
// arrives as HTTP 200 + response.failed. It mirrors the existing
// insufficient_quota cooldown/skip behavior without parsing JSON.
func (s *Store) ApplyResponsesRateLimit(authID, provider, model, body string, now time.Time) {
	if authID == "" {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	cfg := s.config()
	s.recordSeen([]string{authID})
	s.recordRateLimit()
	if cfg.SkipProviderOn429 {
		if provider != "" {
			s.SetSkipProvider(extractProviderName(provider))
		}
	} else {
		s.ApplyInsufficientQuotaCooldown(authID, model, 200, body, now)
	}
	s.markResponsesRateLimited(authID, now)
	s.markResponsesRateLimitApplied(authID, now)
}

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
	// Extract status/body so ApplyRateLimit can detect "insufficient
	// balance" (magic-cube exhaustion) and globally disable the key for
	// the rest of the day.
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
		// A Responses-API quota failure arrives as HTTP 200, so the usage
		// hook would otherwise misread it as healthy and reset the
		// backoff/skip state set by applyResponsesRateLimit.
		if s.consumeResponsesRateLimited(rec.AuthID, now) {
			return
		}
		s.resetCooldownBackoff()
		s.recordSuccess(rec.AuthID)
		s.recordModelUsage(s.displayName(model))
	}
	// Clear the per-provider skip set on ANY 200 (managed or non-managed) so
	// the next request probes the free managed quota again. This is the
	// "prioritise free quota" strategy: every new request tries managed first,
	// and only on a 429+insufficient_quota does it skip to non-managed.
	// Also reset the within-request cross-provider cursor so the next request
	// always starts from the first managed provider; only per-provider key
	// cursors keep rotating across requests.
	if !rec.Failed {
		s.ClearAllSkippedProviders()
		s.ClearSkipRoundDone()
		s.resetManagedProviderCursor()
	}

	// 429 responses take the executor's error path (the host returns early and
	// never calls the response interceptor), so OnUsage is the authoritative
	// path for detecting 429s. The usage hook fires for EVERY provider (the
	// host's usage manager dispatches each record to all plugins
	// unconditionally), so a non-managed fallback provider (e.g. Aliyun Model
	// Studio) that also returns 429+insufficient_quota must NOT receive a
	// cooldown. Guard with ManagesProvider so only monitored providers
	// are affected.
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
			if cfg.SkipProviderOn429 && isInsufficientQuota(rec.Failure.StatusCode, rec.Failure.Body) {
				s.recordSeen([]string{rec.AuthID})
				providerName := extractProviderName(rec.Provider)
				s.SetSkipProvider(providerName)
				s.log.Printf("modelscope-ratelimit: managed 429, skip provider %s model=%s", providerName, s.displayName(model))
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
	"selected_auth_id", "pinned_auth_id",
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

// providerIDKeys are the metadata keys tried when correlating a stream or
// response interceptor call back to the provider key, for skip_provider_on_429.
var providerIDKeys = []string{
	"provider", "Provider", "provider_key", "auth_provider", "AuthProvider", "auth_provider_key",
}

// ProviderFromMetadata defensively extracts the provider key from the host
// context snapshot attached to a response/stream interception request.
func ProviderFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	for _, k := range providerIDKeys {
		if v, ok := meta[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
