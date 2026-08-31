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

// ErrPinnedProviderUnavailable is returned when a nested execution pinned one
// managed provider (see PinnedProviderHeader) and that provider has no
// credential it could serve the request with. The caller's own step must fail
// rather than fall through to another provider, which is the whole point of the
// pin; returning Handled=false would let the host pick someone else's key.
var ErrPinnedProviderUnavailable = errors.New("pinned provider has no available credential")

// SchedulerPick implements scheduler.pick. It cooperates with CLIProxyAPI's
// multi-account rotation: disabled key/model pairs are skipped and the
// request is routed to the next available account.
//
// Provider scope is determined per-candidate, not from the top-level
// req.Provider: in CLIProxyAPI's "mixed" scheduling path the request-level
// Provider is empty while each Candidate.Provider carries the real runtime
// key (e.g. "openai-compatible-modelscope").
//
// Provider rotation: both skip_provider_on_429 modes use the within-request
// cross-provider cursor in Providers config order. Each rate-limit failure
// advances the cursor so the next pick starts from the next provider.
// When skip_provider_on_429 is ON, the provider is also added to the skip
// set so its candidates are filtered out until the next 200. When OFF, only
// the cursor advances (no skip set); the cooldown blocks and then the next
// provider is tried. Any 200 resets the cursor to 0 so the next request
// starts from the first provider; per-provider key cursors keep rotating
// across requests.
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
	now := s.now()

	// Request-scoped pin from the Responses executor's nested chat call. When set,
	// every stage below only considers that provider: no cooldown-block decision
	// for others, no non-managed fallback, and no cross-provider rotation.
	pinned := cfg.PinnedProvider(req)
	pinnedIdx := -1
	if pinned != "" {
		pinnedIdx = cfg.ProviderIndex(pinned)
	} else if raw := headerValue(req.Options.Headers, PinnedProviderHeader); raw != "" && !isInternalExecution(req.Options.Metadata) {
		// Either a client guessed the header name (correctly ignored) or the host
		// stopped flagging nested executions as internally originated, which would
		// silently switch the protocol downgrade back on. Say so loudly.
		s.log.Printf("modelscope-ratelimit: ignoring pin provider=%s without internal-source metadata", raw)
	}

	// Pre-scan: are there any managed candidates in this request? When all
	// managed keys are in the host's "tried" set (absent from candidates), the
	// request is falling through to a non-managed provider — the global
	// cooldown block must not delay that fallback.
	hasManaged := false
	for _, c := range req.Candidates {
		if pinnedIdx >= 0 && cfg.ProviderIndex(c.Provider) != pinnedIdx {
			continue
		}
		if cfg.ManagesProvider(c.Provider) {
			hasManaged = true
			break
		}
	}

	// Global insufficient-quota blocking: Modelscope shares a quota across all
	// keys and across managed providers (e.g. moda + modelscope), so a cooldown
	// on ANY managed key blocks ALL managed scheduling. This prevents
	// rapid-fire 429s on an exhausted shared quota. After the sleep the
	// cooldown has expired and keys are available again. Only applied when
	// there are managed candidates to retry (hasManaged); a request that has
	// already exhausted every managed key and is falling through to a
	// non-managed provider is never blocked.
	// skip_provider_on_429 mode bypasses the cooldown block entirely: it uses a
	// one-shot flag (not per-key cooldowns), so leftover cooldowns from before
	// the mode was enabled must not block the skip check (which runs after this
	// block). Blocking here creates a window where the flag is consumed/cleared
	// by a concurrent pick or a managed 200, so the skip never fires.
	if hasManaged && cfg.InsufficientQuotaCooldown > 0 && !cfg.SkipProviderOn429 {
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
		if pinnedIdx >= 0 && cfg.ProviderIndex(c.Provider) != pinnedIdx {
			// The nested call is reserved for the pinned provider; other managed
			// and non-managed providers get their own step in the executor loop.
			continue
		}
		if !cfg.ManagesProvider(c.Provider) {
			nonManaged = append(nonManaged, c)
			continue
		}
		managed++
		seenIDs = append(seenIDs, c.ID)
		if s.isDisabled(c.ID, req.Model, now) {
			continue
		}
		// skip_provider_on_429: filter out candidates from providers that
		// recently returned 429+insufficient_quota. Per-provider: only the
		// provider that 429'd is skipped, other managed providers remain.
		if cfg.SkipProviderOn429 && s.IsProviderSkipped(extractProviderName(c.Provider)) {
			continue
		}
		idx := cfg.ProviderIndex(c.Provider)
		availableByProvider[idx] = append(availableByProvider[idx], c)
		availableCount++
	}
	s.recordSeen(seenIDs)

	// A pinned step must not quietly become somebody else's step: fail so the
	// executor moves on to the next provider's own (possibly native Responses)
	// attempt.
	if pinnedIdx >= 0 && availableCount == 0 {
		return SchedulerPickResponse{}, fmt.Errorf("%w: provider=%q model=%q", ErrPinnedProviderUnavailable, cfg.Providers[pinnedIdx], s.displayName(req.Model))
	}

	// No managed candidate => out of scope. Defer to the built-in scheduler.
	if managed == 0 {
		// All managed candidates are tried/unavailable for this managed
		// model. If any provider is still in the skip set (no 200 cleared
		// it), clear the set so the next request starts fresh — managed
		// may have recovered. This prevents the set from getting stuck when
		// every candidate (managed + non-managed) fails: the host returns an
		// error without calling the plugin, so this is the last chance to
		// clear.
		if s.HasSkippedProviders() {
			s.ClearAllSkippedProviders()
			s.SetSkipRoundDone()
		}
		return SchedulerPickResponse{Handled: false}, nil
	}

	disabled := managed - availableCount
	if availableCount == 0 {
		// All managed candidates are disabled or skipped. Try a non-managed
		// fallback before hard-blocking the request.

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
		// When skip_provider_on_429 is active and some managed providers are
		// in the skip set, clear the set before failing so the next request
		// probes managed again (insufficient_quota is pool-level; the rate
		// window may have recovered).
		if s.HasSkippedProviders() {
			s.ClearAllSkippedProviders()
			s.SetSkipRoundDone()
			s.log.Printf("modelscope-ratelimit: skip managed on 429, no fallback model=%s", s.displayName(req.Model))
			return SchedulerPickResponse{}, fmt.Errorf("%w: model=%q (insufficient_quota, no non-managed fallback)", ErrAllCredentialsDisabled, s.displayName(req.Model))
		}
		s.log.Printf("modelscope-ratelimit: all disabled model=%s 已限: %d/ 可用: %d / 总共: %d",
			s.displayName(req.Model), disabled, 0, managed)
		return SchedulerPickResponse{}, fmt.Errorf("%w: model=%q", ErrAllCredentialsDisabled, s.displayName(req.Model))
	}

	var picked Candidate
	// Provider rotation:
	//   - skip_provider_on_429 OFF: start from the within-request cross-
	//     provider cursor (advanced by OnUsage on each insufficient_quota
	//     429, reset to 0 on any 200). Within the chosen provider,
	//     pickManaged indexes the per-provider key cursor, which keeps
	//     rotating across requests.
	//   - skip_provider_on_429 ON: strict config order (first provider with
	//     available candidates). The per-provider skip set already filters
	//     providers that recently 429'd.
	start := s.managedProviderStart(len(cfg.Providers))
	for step := 0; step < len(cfg.Providers); step++ {
		i := (start + step) % len(cfg.Providers)
		if candidates, ok := availableByProvider[i]; ok && len(candidates) > 0 {
			ids := make([]string, 0, len(candidates))
			byID := make(map[string]Candidate, len(candidates))
			for _, candidate := range candidates {
				ids = append(ids, candidate.ID)
				byID[candidate.ID] = candidate
			}
			s.recordObservedOrder(cfg.Providers[i], candidates)
			authID := s.NextProviderKey(cfg.Providers[i], ids)
			if selected, ok := byID[authID]; ok {
				picked = selected
				break
			}
			picked = candidates[0]
			break
		}
	}

	// Log the rate-limit situation only when something is actually disabled,
	// to avoid noise on healthy traffic.
	if disabled > 0 {
		rem := s.ModelRemaining(req.Model, now)
		s.log.Printf("modelscope-ratelimit: schedule model=%s 可用魔粒: %d / 已限: %d/ 可用: %d / 总共: %d -> %s",
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
