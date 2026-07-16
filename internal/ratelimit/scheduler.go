package ratelimit

import (
	"errors"
	"fmt"
	"time"
)

// ErrAllCredentialsDisabled is returned by SchedulerPick when every candidate
// for the requested model is rate-limited for the rest of the day AND no
// non-managed provider can serve as a fallback.
var ErrAllCredentialsDisabled = errors.New("all credentials disabled for model (daily rate limit exhausted)")

// SchedulerPick implements scheduler.pick. It cooperates with CLIProxyAPI's
// multi-account rotation: disabled key/model pairs are skipped and the
// request is routed to the next available account.
//
// Provider scope is determined per-candidate, not from the top-level
// req.Provider: in CLIProxyAPI's "mixed" scheduling path the request-level
// Provider is empty while each Candidate.Provider carries the real runtime
// key (e.g. "openai-compatible-modelscope").
//
// Provider preference: among managed providers, the plugin respects the order
// of the Providers config field — the first-listed provider is always picked
// before the second, and so on. Only when every key from an earlier provider is
// disabled (daily rate-limit exhausted) does the plugin fall through to the
// next provider. This lets an operator express "use moda first, then
// modelscope" purely by listing them in that order.
//
// Fallback to non-managed providers: when every managed candidate for the
// model is disabled, the plugin must NOT hard-block the request if
// non-managed providers are still available. The host's
// availableAuthsForRouteModel only passes the highest-priority candidate
// group, so non-managed candidates at a lower priority may be absent from
// req.Candidates even though they exist. Two cases are handled:
//   - Non-managed candidates are present in the list (same priority): pick the
//     highest-priority one directly (Handled=true) so no request is wasted on
//     a disabled managed key.
//   - Non-managed candidates are absent but req.Providers lists a non-managed
//     provider: defer (Handled=false) so the host retries through its built-in
//     rotation and eventually reaches the lower-priority non-managed keys.
//
// Only when no non-managed provider exists at all does the plugin return
// ErrAllCredentialsDisabled, so the host fails fast without wasting requests
// on disabled managed keys.
func (s *Store) SchedulerPick(req SchedulerPickRequest) (SchedulerPickResponse, error) {
	cfg := s.config()

	if !cfg.ManagesModel(req.Model) {
		return SchedulerPickResponse{Handled: false}, nil
	}

	now := s.now()

	// Pre-scan: are there any managed candidates in this request? When all
	// managed keys are in the host's "tried" set (absent from candidates), the
	// request is falling through to a non-managed provider — the global
	// cooldown block must not delay that fallback.
	hasManaged := false
	for _, c := range req.Candidates {
		if cfg.ManagesProvider(c.Provider) {
			hasManaged = true
			break
		}
	}

	// Proxy cleanup: when no managed candidates remain (all tried or
	// unavailable), the request is falling through to non-managed providers.
	// Disable the global proxy so non-managed traffic goes direct.
	if !hasManaged {
		s.DisableProxyIfActive()
	}

	// Global insufficient-quota blocking: Modelscope shares a quota across all
	// keys and across managed providers (e.g. moda + modelscope), so a cooldown
	// on ANY managed key blocks ALL managed scheduling. This prevents
	// rapid-fire 429s on an exhausted shared quota. After the sleep the
	// cooldown has expired and keys are available again. Only applied when
	// there are managed candidates to retry (hasManaged); a request that has
	// already exhausted every managed key and is falling through to a
	// non-managed provider is never blocked.
	if hasManaged && cfg.InsufficientQuotaCooldown > 0 {
		if wait := s.cooldownWaitDuration(now); wait > 0 {
			// Record the max cooldown expiry before sleeping. After the sleep,
			// a concurrent request may have set a NEW (longer) cooldown on a
			// different key. The re-check compares the post-sleep max expiry to
			// the pre-sleep one: if it moved forward, a new cooldown was set
			// during the sleep and we block for the remaining time.
			// cooldownWaitDuration returns expiry.Sub(now), so time already
			// elapsed during the first sleep is not re-waited.
			expiryBefore := now.Add(wait)
			s.log.Printf("modelscope-ratelimit: block %s model=%s (insufficient_quota cooldown)",
				wait.Truncate(time.Millisecond), s.displayName(req.Model))
			s.blockEnter()
			time.Sleep(wait)
			s.blockLeave()
			now = s.now()
			if wait2 := s.cooldownWaitDuration(now); wait2 > 0 {
				if expiryAfter := now.Add(wait2); expiryAfter.After(expiryBefore) {
					s.log.Printf("modelscope-ratelimit: block %s model=%s (insufficient_quota cooldown, new during sleep)",
						wait2.Truncate(time.Millisecond), s.displayName(req.Model))
					s.blockEnter()
					time.Sleep(wait2)
					s.blockLeave()
					now = s.now()
				}
			}
		}
	}

	// Partition candidates into managed (in-scope) and non-managed. Available
	// managed candidates are grouped by their position in cfg.Providers so the
	// plugin can prefer earlier-listed providers.
	availableByProvider := make(map[int][]Candidate)
	nonManaged := make([]Candidate, 0, len(req.Candidates))
	managed := 0
	availableCount := 0
	seenIDs := make([]string, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		if !cfg.ManagesProvider(c.Provider) {
			nonManaged = append(nonManaged, c)
			continue
		}
		managed++
		seenIDs = append(seenIDs, c.ID)
		if s.isDisabled(c.ID, req.Model, now) {
			continue
		}
		idx := cfg.ProviderIndex(c.Provider)
		availableByProvider[idx] = append(availableByProvider[idx], c)
		availableCount++
	}
	s.recordSeen(seenIDs)

	// No managed candidate => out of scope. Defer to the built-in scheduler.
	if managed == 0 {
		return SchedulerPickResponse{Handled: false}, nil
	}

	disabled := managed - availableCount
	if availableCount == 0 {
		// All managed candidates are disabled. Try a non-managed fallback before
		// hard-blocking the request.

		// Case 1: non-managed candidates are visible. Pick among the
		// highest-priority group using the host's own routing strategy (fetched
		// via the management API) so the plugin mirrors the host's rotation
		// semantics for non-managed fallback, instead of imposing a fixed order.
		if group := highestPriorityGroup(nonManaged); len(group) > 0 {
			picked, _ := s.pickByStrategy(group, s.hostStrategy())
			s.log.Printf("modelscope-ratelimit: all managed disabled, fallback model=%s -> %s",
				s.displayName(req.Model), picked.ID)
			return SchedulerPickResponse{AuthID: picked.ID, Handled: true}, nil
		}

		// Case 2: no non-managed candidate visible, but the route accepts
		// non-managed providers. They may be at a lower priority that the host
		// filtered out, or in host cooldown. Defer so the host built-in
		// rotation can eventually reach them.
		if routeHasNonManagedProvider(req.Providers, cfg) {
			return SchedulerPickResponse{Handled: false}, nil
		}

		// Case 3: no non-managed provider exists at all. Fail explicitly.
		s.log.Printf("modelscope-ratelimit: all disabled model=%s 已限: %d/ 可用: %d / 总共: %d",
			s.displayName(req.Model), disabled, 0, managed)
		return SchedulerPickResponse{}, fmt.Errorf("%w: model=%q", ErrAllCredentialsDisabled, s.displayName(req.Model))
	}

	// Pick from the first provider (in cfg.Providers order) that has available
	// candidates. This implements strict preference by config order: the
	// first-listed provider is always used before the second, and so on.
	// Within the chosen provider, pickManaged indexes the round-robin cursor
	// into the full config-ordered key list (including disabled/tried keys),
	// so 429 retries and disabled keys don't corrupt the rotation order.
	var picked Candidate
	for i := 0; i < len(cfg.Providers); i++ {
		if candidates, ok := availableByProvider[i]; ok && len(candidates) > 0 {
			picked, _ = s.pickManaged(candidates, cfg.Providers[i])
			break
		}
	}

	// Log the rate-limit situation only when something is actually disabled,
	// to avoid noise on healthy traffic.
	if disabled > 0 {
		rem := s.ModelRemaining(req.Model, now)
		s.log.Printf("modelscope-ratelimit: schedule model=%s 剩余请求次数: %d / 已限: %d/ 可用: %d / 总共: %d -> %s",
			s.displayName(req.Model), rem, disabled, availableCount, managed, picked.ID)
	}

	return SchedulerPickResponse{AuthID: picked.ID, Handled: true}, nil
}

// pickByStrategy selects one non-managed candidate from a non-empty slice
// according to the given strategy: "fill-first" always returns the first
// candidate, "round-robin" (default) rotates via the store's non-managed
// cursor (separate from managed per-provider cursors so non-managed
// rotation never corrupts managed ordering). ok=false only when empty.
func (s *Store) pickByStrategy(candidates []Candidate, strategy string) (Candidate, bool) {
	if len(candidates) == 0 {
		return Candidate{}, false
	}
	if strategy == "fill-first" {
		return candidates[0], true
	}
	// Add(1) returns the post-increment value (first call → 1); subtract 1 so
	// the first pick lands on index 0 and rotation follows the slice order.
	n := s.nonManagedRR.Add(1)
	idx := int((n - 1) % uint64(len(candidates)))
	return candidates[idx], true
}

// highestPriorityGroup returns the candidates that share the largest Priority
// value. The host's availableAuthsForRouteModel only passes the highest-priority
// candidate group, but when multiple non-managed providers appear at the same
// priority they all arrive together — this filters to that top tier so
// pickByStrategy can rotate among them.
func highestPriorityGroup(candidates []Candidate) []Candidate {
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0].Priority
	for _, c := range candidates[1:] {
		if c.Priority > best {
			best = c.Priority
		}
	}
	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Priority == best {
			out = append(out, c)
		}
	}
	return out
}

// routeHasNonManagedProvider reports whether req.Providers contains any
// provider key that is not in the plugin managed Providers set.
func routeHasNonManagedProvider(routeProviders []string, cfg *Config) bool {
	for _, p := range routeProviders {
		if !cfg.ManagesProvider(p) {
			return true
		}
	}
	return false
}
