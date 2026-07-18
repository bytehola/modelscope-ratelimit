package ratelimit

import (
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Logger is a minimal logging sink. The SDK glue injects a host.log-backed
// implementation; tests inject a no-op or a test recorder.
type Logger interface {
	Printf(format string, args ...any)
}

// NoopLogger discards everything.
type NoopLogger struct{}

func (NoopLogger) Printf(string, ...any) {}

// keyState holds the disable state for a single credential (auth ID).
//
// A disable is always "for the rest of the current calendar day" in the
// configured timezone. The recorded time is when the exhaustion was observed.
// rateData is the last-seen rate-limit snapshot for a (key, model) or a key's
// total quota, day-bounded by "at" so values from a previous day are pruned
// at the midnight reset (the upstream daily quota resets then too).
type rateData struct {
	remaining int
	limit     int
	hasRem    bool
	hasLim    bool
	at        time.Time
}

type keyState struct {
	mu           sync.RWMutex
	global       *time.Time           // global (all-model) disable time, nil if none
	globalReason string               // "" = daily quota disable (resets midnight); "unauthorized" = 401 (until restart)
	models       map[string]time.Time // disabled model -> disable time
	modelData    map[string]*rateData // observed model -> last-seen rate-limit data
	totalData    *rateData            // observed total rate-limit data
	cooldown     *time.Time           // short-term global cooldown expiry (insufficient_quota), nil if none
	successCount int                  // daily successful request count (reset at midnight)
}

// Store is the concurrency-safe rate-limit state shared by all plugin hooks.
type Store struct {
	cfg atomic.Pointer[Config]
	loc atomic.Pointer[time.Location]

	// managedCursors holds per-provider round-robin cursors. Each cursor
	// indexes into that provider's full config-ordered key list so that
	// switching providers (e.g. moda exhausted → modelscope) starts from
	// the correct position in the new provider's list instead of an
	// arbitrary value left over from the previous provider.
	managedCursorsMu sync.Mutex
	managedCursors   map[string]uint64
	// managedOrder remembers the largest host-ordered candidate ID list seen
	// per managed provider. When providerFullOrder is unavailable (fetcher
	// unset or management API unreachable), pickManaged uses this snapshot as
	// the stable "full" list so a 429 retry (which shrinks the candidate
	// group) still scans forward in a fixed order and lands on the next key,
	// instead of indexing a shrunken list with a cursor advanced by the prior
	// pick. Grown from every fresh candidate group; reset on reconfigure.
	managedOrderMu sync.Mutex
	managedOrder   map[string][]string

	// nonManagedRR is the round-robin cursor for non-managed fallback
	// candidates (Case 1). It is separate from the managed cursors so that
	// non-managed rotation never corrupts managed provider ordering.
	nonManagedRR atomic.Uint64

	mu   sync.RWMutex
	keys map[string]*keyState

	// seen tracks every credential ID observed by the scheduler for a managed
	// provider, so "total key count" can be reported even when nothing is
	// disabled yet.
	seenMu sync.RWMutex
	seen   map[string]struct{}

	// aliasUpstream maps a client-requested model alias (the key disables are
	// stored and the scheduler checks under) to the resolved upstream model
	// name, so logs and the status page can display the upstream name while
	// the storage key stays the alias. Populated from usage records, which
	// carry both Alias and Model.
	aliasMu       sync.RWMutex
	aliasUpstream map[string]string

	// strategyFetcher returns the host's built-in routing strategy
	// ("round-robin" or "fill-first"). Injected by the glue layer via the
	// management API. When nil or returning empty, hostStrategy defaults to
	// "round-robin" so managed selection never blocks on strategy resolution.
	strategyFetcher atomic.Pointer[func() string]

	// providerOrderFetcher returns a map from provider name to the full list of
	// auth IDs in config.yaml api-key order (including keys that may currently be
	// disabled or tried). The scheduler indexes its round-robin cursor into this
	// full list and skips absent entries, so 429 retries and disabled keys don't
	// corrupt the rotation order.
	providerOrderFetcher atomic.Pointer[func() map[string][]string]

	// backoffMu guards the global insufficient_quota exponential-backoff
	// level. Modelscope shares a quota across all keys, so the backoff is
	// global (not per-key): each consecutive round of 429+insufficient_quota
	// doubles the cooldown interval, capped at maxInsufficientQuotaCooldown.
	backoffMu       sync.Mutex
	cooldownLevel   int       // current backoff interval (seconds); 0 = idle
	cooldownLevelAt time.Time // when the level was last advanced

	// cooldownCount and cooldownWaitNanos track daily insufficient_quota
	// statistics for the status page: how many times the cooldown was
	// triggered and the total time spent blocking in SchedulerPick. Both
	// reset at the midnight boundary (PruneAll), same as the backoff level.
	cooldownStatsMu   sync.Mutex
	cooldownCount     int64
	cooldownWaitNanos int64 // wall-clock nanoseconds any goroutine was blocking
	cooldownStatsDay  time.Time
	successCount      int64     // daily successful managed requests (status page)
	blockActive       int       // number of goroutines currently in a cooldown sleep
	blockStart        time.Time // when the current block period started (blockActive 0→1)

	// jitterFn returns a random [0, 1s) duration added to each
	// insufficient_quota cooldown so concurrent retries against a shared
	// exhausted quota don't all wake at the same instant (thundering herd).
	// Injectable in tests (return 0) for deterministic timing assertions.
	jitterFn atomic.Pointer[func() time.Duration]

	// Proxy state: when proxy_url is configured, a 429 from a managed
	// provider triggers a 2s wait + proxy probe. If the probe succeeds the
	// global upstream proxy is toggled on via proxyToggler (injected by the
	// C-ABI glue, which calls the management API PUT/DELETE proxy-url). The
	// proxy stays on until a managed provider succeeds or all managed keys
	// are exhausted, then is disabled. A 60s safety timer auto-disables.
	proxyMu             sync.Mutex
	proxyActive         bool
	proxyEnabling       bool   // another goroutine is mid-enable (probe/toggler)
	proxyTriggerPending bool   // set by OnUsage on 429, consumed by SchedulerPick (sync)
	proxyTriggerAuthID  string // authID of the 429 that set the trigger (for setCooldown)
	proxyTriggerModel   string // model of the 429 that set the trigger
	proxyProbeFailed    bool   // probe failed once; subsequent 429s skip probe and use cooldown
	proxyProbed         bool   // probe has been performed at least once (success or fail)
	proxyEnabledAt      time.Time
	proxyTimer          *time.Timer
	originalProxyURL    string // host's proxy URL before plugin enabled its own (for restore)
	proxyToggler        atomic.Pointer[func(proxyURL string) error]
	proxyURLGetter      atomic.Pointer[func() (string, error)]
	// Test seams: an injectable probe (avoids real network during tests) and a
	// proxy-wait override (keeps proxy-mode tests from sleeping 2s). Both are
	// nil/zero in production, falling back to probeProxy / proxyWaitDuration.
	probeFn        atomic.Pointer[func(proxyURL string) bool]
	proxyWaitNanos atomic.Int64

	clock func() time.Time // injectable for tests; defaults to time.Now

	stopMu  sync.Mutex
	started bool
	stop    chan struct{}
	log     Logger
}

// NewStore creates an empty store. The location defaults to UTC until
// Reconfigure is called.
func NewStore(log Logger) *Store {
	if log == nil {
		log = NoopLogger{}
	}
	utc := time.UTC
	s := &Store{
		keys:           make(map[string]*keyState),
		seen:           make(map[string]struct{}),
		aliasUpstream:  make(map[string]string),
		managedCursors: make(map[string]uint64),
		managedOrder:   make(map[string][]string),
		clock:          time.Now,
		stop:           make(chan struct{}),
		log:            log,
	}
	s.loc.Store(utc)
	s.cfg.Store(DefaultConfig())
	df := defaultJitter
	s.jitterFn.Store(&df)
	return s
}

// SetClock overrides the clock used for "now". Intended for tests.
func (s *Store) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// SetLocation overrides the timezone used for the daily boundary. Tests use a
// fixed zone to avoid depending on the system tz database.
func (s *Store) SetLocation(loc *time.Location) {
	if loc != nil {
		s.loc.Store(loc)
	}
}

// SetStrategyFetcher injects a callback that returns the host's built-in
// routing strategy. The glue layer fetches it via the management API
// (GET /v0/management/routing/strategy) with a short cache. When never set or
// the fetch fails, hostStrategy falls back to "round-robin".
func (s *Store) SetStrategyFetcher(f func() string) {
	if f == nil {
		s.strategyFetcher.Store(nil)
		return
	}
	fp := f
	s.strategyFetcher.Store(&fp)
}

// hostStrategy returns the host's built-in credential selection strategy, or
// "round-robin" when unavailable. Non-managed fallback candidates use this so
// the plugin mirrors the host's own rotation semantics instead of imposing a
// fixed order.
func (s *Store) hostStrategy() string {
	if fp := s.strategyFetcher.Load(); fp != nil && *fp != nil {
		if v := (*fp)(); v == "fill-first" {
			return "fill-first"
		}
	}
	return "round-robin"
}

// managedStrategy returns the configured strategy for managed-provider
// selection, validated to "round-robin" (default) or "fill-first".
func (s *Store) managedStrategy() string {
	switch s.config().CredentialStrategy {
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	default:
		return "round-robin"
	}
}

// SetProviderOrderFetcher injects a callback that returns a map from provider
// name to the full config-ordered auth ID list. The glue fetches this via the
// management API with the same cache as key resolution. When unset, pickManaged
// falls back to simple round-robin on the host-ordered candidates.
func (s *Store) SetProviderOrderFetcher(f func() map[string][]string) {
	if f == nil {
		s.providerOrderFetcher.Store(nil)
		return
	}
	fp := f
	s.providerOrderFetcher.Store(&fp)
}

// providerFullOrder returns the full config-ordered auth ID list for one
// provider (including keys currently disabled or tried), or nil when the
// fetcher is unset or the provider is unknown.
func (s *Store) providerFullOrder(providerCfgName string) []string {
	fp := s.providerOrderFetcher.Load()
	if fp == nil || *fp == nil {
		return nil
	}
	m := (*fp)()
	if m == nil {
		return nil
	}
	return m[strings.ToLower(strings.TrimSpace(providerCfgName))]
}

// recordObservedOrder grows the per-provider observed-order snapshot from a
// candidate group. A fresh request carries every key the host can route to
// (in host order); a 429 retry carries a strict subset (the tried key is
// absent). Only a group at least as large as the remembered one extends it,
// preserving the full order so the fallback scan can skip absent entries.
// Safe to call concurrently and outside managedCursorsMu (uses its own lock).
func (s *Store) recordObservedOrder(provider string, group []Candidate) {
	if len(group) == 0 {
		return
	}
	key := strings.ToLower(strings.TrimSpace(provider))
	s.managedOrderMu.Lock()
	defer s.managedOrderMu.Unlock()
	cur := s.managedOrder[key]
	if len(group) < len(cur) {
		// Retry subset: keep the fuller remembered list so the scan has a
		// stable superset to index into.
		return
	}
	// Equal-or-larger group: rebuild preserving prior order, then append any
	// newly seen IDs (handles a key added while the plugin is running).
	seen := make(map[string]bool, len(cur))
	out := make([]string, 0, len(group))
	for _, id := range cur {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, c := range group {
		if !seen[c.ID] {
			seen[c.ID] = true
			out = append(out, c.ID)
		}
	}
	s.managedOrder[key] = out
}

// observedOrder returns a copy of the per-provider observed-order snapshot,
// or nil when none has been recorded yet. Read-only; safe under
// managedCursorsMu.
func (s *Store) observedOrder(provider string) []string {
	key := strings.ToLower(strings.TrimSpace(provider))
	s.managedOrderMu.Lock()
	defer s.managedOrderMu.Unlock()
	src := s.managedOrder[key]
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// pickManaged selects one candidate from a non-empty managed-provider group,
// honoring config.yaml api-key order even across 429 retries and disabled keys.
//
// The round-robin cursor indexes into the FULL config-ordered list (including
// keys that may be disabled or in the host's "tried" retry set). On each call
// it scans forward from the cursor position, skipping any entry that is absent
// from the current candidate group, and lands on the next available key. This
// means a 429 retry (which shrinks the candidate list) still picks the next key
// in config order, not a random one dictated by cursor % len(smaller_list).
//
// fill-first always picks the first available key in config order.
//
// When the fetcher-supplied full-order list is unavailable, the plugin falls
// back to a per-provider observed-order snapshot grown from prior candidate
// groups, so retries still scan a stable superset and skip absent keys. Only
// the very first call (no fetcher, no snapshot yet) degrades to simple
// round-robin on the current group.
func (s *Store) pickManaged(group []Candidate, providerCfgName string) (Candidate, bool) {
	if len(group) == 0 {
		return Candidate{}, false
	}
	strategy := s.managedStrategy()

	// Grow the observed-order snapshot BEFORE acquiring the cursor mutex so
	// the short managedCursorsMu critical section never waits on this map
	// write. The snapshot is the fallback "full" list when the fetcher is
	// unavailable (unset or transient management-API failure).
	s.recordObservedOrder(providerCfgName, group)

	if strategy == "fill-first" {
		// Always pick the first available key in config order.
		full := s.providerFullOrder(providerCfgName)
		if len(full) == 0 {
			full = s.observedOrder(providerCfgName)
		}
		if len(full) > 0 {
			present := make(map[string]bool, len(group))
			byID := make(map[string]Candidate, len(group))
			for _, c := range group {
				present[c.ID] = true
				byID[c.ID] = c
			}
			for _, id := range full {
				if present[id] {
					return byID[id], true
				}
			}
		}
		return group[0], true
	}

	// Fetch the full config-ordered key list BEFORE acquiring the cursor mutex:
	// on a cache miss this calls the management API (up to 10s HTTP timeout),
	// and holding managedCursorsMu during that would block ALL managed picks
	// across all providers. The full list is immutable for the call's
	// duration, so computing it outside the lock is safe.
	full := s.providerFullOrder(providerCfgName)

	// Per-provider cursor, protected by managedCursorsMu so the read-scan-write
	// sequence is atomic. The critical section is short (one scan + one map
	// write) and never does I/O.
	s.managedCursorsMu.Lock()
	defer s.managedCursorsMu.Unlock()

	cursor := s.managedCursors[providerCfgName]

	if len(full) == 0 {
		// No fetcher-supplied order: use the observed-order snapshot grown
		// from prior candidate groups. It is a superset of the current
		// (possibly retry-shrunken) group, so the scan-skip-absent logic
		// below still lands on the next key in the remembered order instead
		// of indexing a shrunken list with a stale cursor.
		full = s.observedOrder(providerCfgName)
	}

	if len(full) > 0 {
		// Full-order path: scan from cursor position in the complete
		// config-ordered list (including disabled/tried keys), skipping
		// absent entries so retries and disabled keys don't break rotation.
		present := make(map[string]bool, len(group))
		byID := make(map[string]Candidate, len(group))
		for _, c := range group {
			present[c.ID] = true
			byID[c.ID] = c
		}
		start := int(cursor % uint64(len(full)))
		found := -1
		for step := 0; step < len(full); step++ {
			pos := (start + step) % len(full)
			if present[full[pos]] {
				found = pos
				break
			}
		}
		if found < 0 {
			return group[0], true
		}
		s.managedCursors[providerCfgName] = uint64(found + 1)
		return byID[full[found]], true
	}

	// First-ever call with no fetcher and no observed snapshot yet: simple
	// round-robin on the current host-ordered group using the per-provider
	// cursor (isolated from other providers). This branch only runs once per
	// provider before the snapshot is populated.
	n := cursor + 1
	s.managedCursors[providerCfgName] = n
	idx := int((n - 1) % uint64(len(group)))
	return group[idx], true
}

// Now returns the current clock value (used by the glue for rendering).
func (s *Store) Now() time.Time { return s.now() }

func (s *Store) location() *time.Location {
	if l := s.loc.Load(); l != nil {
		return l
	}
	return time.UTC
}

func (s *Store) config() *Config {
	if c := s.cfg.Load(); c != nil {
		return c
	}
	return DefaultConfig()
}

func (s *Store) now() time.Time {
	return s.clock()
}

// Reconfigure applies a new configuration (called from plugin.reconfigure and
// the initial plugin.register). It resolves the timezone, falling back to
// UTC+8 (China Standard Time) when the system tz database is unavailable.
func (s *Store) Reconfigure(cfg *Config) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	s.cfg.Store(cfg)
	s.loc.Store(resolveLocation(cfg.Timezone, s.log))
	// Reset rotation cursors: providers may have changed, so stale cursor
	// values from a previous config are meaningless.
	s.managedCursorsMu.Lock()
	s.managedCursors = make(map[string]uint64)
	s.managedCursorsMu.Unlock()
	s.nonManagedRR.Store(0)
	// Reset the observed-order snapshots too: a new provider set or reordered
	// api-keys means the remembered host-ordered lists are stale.
	s.managedOrderMu.Lock()
	s.managedOrder = make(map[string][]string)
	s.managedOrderMu.Unlock()
	// Reset the exponential-backoff level so a changed insufficient_quota_cooldown
	// base takes effect immediately instead of continuing from a stale level.
	s.backoffMu.Lock()
	s.cooldownLevel = 0
	s.cooldownLevelAt = time.Time{}
	s.backoffMu.Unlock()
}

// Start launches the background goroutine that prunes expired disables so the
// state is reset at 00:00 even when no requests arrive. Idempotent.
func (s *Store) Start() {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if s.started {
		return
	}
	s.stop = make(chan struct{})
	s.started = true
	go s.cleaner()
}

// Shutdown stops the background cleaner. Idempotent.
func (s *Store) Shutdown() {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if !s.started {
		return
	}
	s.started = false
	close(s.stop)
}

// cleaner periodically drops disables whose calendar day has rolled over.
func (s *Store) cleaner() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.PruneAll(now)
		}
	}
}

// resolveLocation loads an IANA timezone, falling back to UTC+8.
func resolveLocation(name string, log Logger) *time.Location {
	if name == "" {
		name = "Asia/Shanghai"
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	log.Printf("modelscope-ratelimit: timezone %q unavailable, falling back to UTC+8", name)
	return time.FixedZone("CST", 8*3600)
}

// sameDay reports whether t1 and t2 fall on the same calendar date in loc.
func sameDay(t1, t2 time.Time, loc *time.Location) bool {
	y1, m1, d1 := t1.In(loc).Date()
	y2, m2, d2 := t2.In(loc).Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// active reports whether a disable observed at "at" is still in effect at
// "now": it must be on the same calendar day and not be in the future.
func active(at, now time.Time, loc *time.Location) bool {
	return sameDay(at, now, loc) && !now.Before(at)
}

// getKey returns the keyState for authID without creating it (nil if absent).
func (s *Store) getKey(authID string) *keyState {
	s.mu.RLock()
	ks := s.keys[authID]
	s.mu.RUnlock()
	return ks
}

// getOrCreateKey returns the keyState for authID, creating it if needed.
func (s *Store) getOrCreateKey(authID string) *keyState {
	s.mu.RLock()
	ks, ok := s.keys[authID]
	s.mu.RUnlock()
	if ok {
		return ks
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check under the write lock to handle the race.
	if ks, ok = s.keys[authID]; ok {
		return ks
	}
	ks = &keyState{models: make(map[string]time.Time), modelData: make(map[string]*rateData)}
	s.keys[authID] = ks
	return ks
}

// recordRemaining stores the last-seen rate-limit numbers for a credential,
// day-bounded so stale values clear at the midnight reset. It does not touch
// disable state; ApplyRateLimit disables separately based on the threshold.
func (s *Store) recordRemaining(authID, model string, now time.Time,
	modelRem int, modelOK bool, modelLim int, modelLimOK bool,
	totalRem int, totalOK bool, totalLim int, totalLimOK bool) {
	ks := s.getOrCreateKey(authID)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if modelOK || modelLimOK {
		md := ks.modelData[model]
		if md == nil {
			md = &rateData{}
			ks.modelData[model] = md
		}
		if modelOK {
			md.remaining = modelRem
			md.hasRem = true
		}
		if modelLimOK {
			md.limit = modelLim
			md.hasLim = true
		}
		md.at = now
	}
	if totalOK || totalLimOK {
		td := ks.totalData
		if td == nil {
			td = &rateData{}
			ks.totalData = td
		}
		if totalOK {
			td.remaining = totalRem
			td.hasRem = true
		}
		if totalLimOK {
			td.limit = totalLim
			td.hasLim = true
		}
		td.at = now
	}
}

// deleteKey removes an empty keyState entry.
func (s *Store) deleteKey(authID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ks, ok := s.keys[authID]; ok {
		ks.mu.RLock()
		empty := ks.global == nil && len(ks.models) == 0 && len(ks.modelData) == 0 && ks.totalData == nil && ks.cooldown == nil
		ks.mu.RUnlock()
		if empty {
			delete(s.keys, authID)
		}
	}
}

// isDisabled reports whether the (authID, model) pair is currently disabled.
// A global disable covers all models. Stale records (rolled-over day) do not
// count; the background cleaner removes them eventually.
func (s *Store) isDisabled(authID, model string, now time.Time) bool {
	ks := s.getKey(authID)
	if ks == nil {
		return false
	}
	loc := s.location()
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	// insufficient_quota cooldown is NOT checked here. Instead the scheduler
	// globally blocks (sleeps) for the longest active cooldown before picking,
	// because Modelscope shares a quota across all keys and providers — the
	// cooldown is global, not per-key. After the block the cooldown has
	// expired and the key is available again. Daily disables (global +
	// per-model) are still checked here.
	if ks.global != nil && (ks.globalReason == "unauthorized" || active(*ks.global, now, loc)) {
		return true
	}
	if t, ok := ks.models[model]; ok && active(t, now, loc) {
		return true
	}
	return false
}

// hasAnyDisable reports whether the key has any active global or model disable.
func (s *Store) hasAnyDisable(authID string, now time.Time, loc *time.Location) bool {
	ks := s.getKey(authID)
	if ks == nil {
		return false
	}
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	if ks.cooldown != nil && now.Before(*ks.cooldown) {
		return true
	}
	if ks.global != nil && (ks.globalReason == "unauthorized" || active(*ks.global, now, loc)) {
		return true
	}
	for _, t := range ks.models {
		if active(t, now, loc) {
			return true
		}
	}
	return false
}

// recordSeen registers credential IDs observed by the scheduler so total key
// count can be reported.
func (s *Store) recordSeen(ids []string) {
	if len(ids) == 0 {
		return
	}
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	for _, id := range ids {
		if id != "" {
			s.seen[id] = struct{}{}
		}
	}
}

// recordAlias remembers that alias resolves to the upstream model name, so
// logs and the status page can display the upstream name while disables stay
// keyed by the alias the scheduler checks. Safe to call concurrently; no-op when
// either side is empty or they are identical.
func (s *Store) recordAlias(alias, upstream string) {
	alias = strings.TrimSpace(alias)
	upstream = strings.TrimSpace(upstream)
	if alias == "" || upstream == "" || alias == upstream {
		return
	}
	s.aliasMu.Lock()
	s.aliasUpstream[alias] = upstream
	s.aliasMu.Unlock()
}

// displayName returns the upstream model name for an alias when known, else the
// alias itself. Used only for display (logs, status page); the underlying
// disable storage key is unchanged.
func (s *Store) displayName(model string) string {
	if s == nil {
		return model
	}
	s.aliasMu.RLock()
	upstream, ok := s.aliasUpstream[model]
	s.aliasMu.RUnlock()
	if ok && upstream != "" {
		return upstream
	}
	return model
}

// seenIDs returns a snapshot of all observed credential IDs.
func (s *Store) seenIDs() []string {
	s.seenMu.RLock()
	ids := make([]string, 0, len(s.seen))
	for id := range s.seen {
		ids = append(ids, id)
	}
	s.seenMu.RUnlock()
	return ids
}

// Overview returns overall key counts at "now": total observed, keys with any
// active disable, and available (total - disabled).
func (s *Store) Overview(now time.Time) (total, disabled, available int) {
	ids := s.seenIDs()
	loc := s.location()
	total = len(ids)
	for _, id := range ids {
		if s.hasAnyDisable(id, now, loc) {
			disabled++
		}
	}
	available = total - disabled
	return
}

// Counts returns per-model counts among observed keys at "now".
func (s *Store) Counts(model string, now time.Time) (disabled, available, total int) {
	ids := s.seenIDs()
	total = len(ids)
	for _, id := range ids {
		if s.isDisabled(id, model, now) {
			disabled++
		}
	}
	available = total - disabled
	return
}

// ModelRemaining returns the sum of last-seen remaining requests for a model
// across all keys that are currently active (neither globally nor per-model
// disabled) at "now". Only keys with observed same-day rate-limit data
// contribute; unobserved keys are excluded. Used by the disable log to report
// how much model quota is left on the still-active keys.
func (s *Store) ModelRemaining(model string, now time.Time) int {
	ids := s.seenIDs()
	loc := s.location()
	total := 0
	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.RLock()
		skip := ks.global != nil && active(*ks.global, now, loc)
		if !skip {
			if t, ok := ks.models[model]; ok && active(t, now, loc) {
				skip = true
			}
		}
		rem := 0
		if !skip {
			if md, ok := ks.modelData[model]; ok && sameDay(md.at, now, loc) && md.hasRem {
				rem = md.remaining
			}
		}
		ks.mu.RUnlock()
		total += rem
	}
	return total
}

// disableGlobal marks authID as globally disabled for the rest of the day.
// The earliest observation time is kept. model is the model whose total quota
// exhaustion triggered the disable; it is only used to report the remaining
// model quota on the still-active keys in the log.
func (s *Store) disableGlobal(authID, model string, now time.Time) {
	ks := s.getOrCreateKey(authID)
	loc := s.location()
	ks.mu.Lock()
	if ks.global != nil && active(*ks.global, now, loc) {
		ks.mu.Unlock()
		return // already disabled today; keep earliest time
	}
	t := now
	ks.global = &t
	ks.globalReason = ""
	ks.mu.Unlock()
	s.log.Printf("modelscope-ratelimit: key %s globally disabled", authID)
}

// disableUnauthorized marks authID as globally disabled until plugin restart
// (NOT reset at midnight) because the API key returned 401 (invalid or expired
// secret). The status page shows "密钥失效" via GlobalReason.
func (s *Store) disableUnauthorized(authID string, now time.Time) {
	ks := s.getOrCreateKey(authID)
	loc := s.location()
	ks.mu.Lock()
	if ks.global != nil && (ks.globalReason == "unauthorized" || active(*ks.global, now, loc)) {
		ks.mu.Unlock()
		return // already disabled; keep earliest time
	}
	t := now
	ks.global = &t
	ks.globalReason = "unauthorized"
	ks.mu.Unlock()
	s.log.Printf("modelscope-ratelimit: key %s disabled (unauthorized: secret may be invalid)", authID)
}

// disableModel marks (authID, model) as disabled for the rest of the day.
// The earliest observation time is kept.
func (s *Store) disableModel(authID, model string, now time.Time) {
	ks := s.getOrCreateKey(authID)
	loc := s.location()
	ks.mu.Lock()
	if t, ok := ks.models[model]; ok && active(t, now, loc) {
		ks.mu.Unlock()
		return // already disabled today; keep earliest time
	}
	ks.models[model] = now
	ks.mu.Unlock()
	s.log.Printf("modelscope-ratelimit: key %s model %s disabled",
		authID, s.displayName(model))
}

// maxInsufficientQuotaCooldown is the ceiling for a single insufficient_quota
// cooldown interval (seconds). Exponential backoff doubles each consecutive
// round but never exceeds this value, so SchedulerPick never blocks longer
// than this in one call.
const maxInsufficientQuotaCooldown = 60

// setCooldown temporarily disables a key for all models using exponential
// backoff. Used when a managed key returns HTTP 429 with an
// "insufficient_quota" body (e.g. Aliyun Model Studio quota exhaustion).
//
// Because Modelscope shares a quota across all keys, the backoff level is
// global: each consecutive round of 429s doubles the interval
// (base -> 2x base -> 4x base -> ...), capped at maxInsufficientQuotaCooldown.
// A "round" is detected by whether the previous cooldown window has elapsed:
// multiple keys failing within the same window (shared quota exhausted
// together) keep the current level so the round isn't over-counted, while a
// failure after the window expires advances to the next (doubled) level.
//
// The level resets to the base on a healthy response (quota recovered, see
// ApplyRateLimit) and at the midnight daily boundary (PruneAll).
func (s *Store) setCooldown(authID string, base int, now time.Time) {
	if authID == "" || base <= 0 {
		return
	}
	s.backoffMu.Lock()
	level := s.cooldownLevel
	advanced := false
	switch {
	case level <= 0:
		// First failure since the backoff was reset.
		level = base
		advanced = true
	case now.Before(s.cooldownLevelAt.Add(time.Duration(level)*time.Second + time.Second)):
		// Still inside the previous cooldown window (level + jitter buffer):
		// multiple keys of the same shared quota failing together (one round).
		// Keep the level AND the original window start so the round isn't
		// over-counted or slid forward. The extra second accounts for jitter
		// so a failure while the actual (level+jitter) cooldown is still
		// active isn't mistaken for a new round.
	default:
		// Previous window elapsed - a new round of failures. Double, capped.
		level = min(level*2, maxInsufficientQuotaCooldown)
		advanced = true
	}
	if advanced {
		s.cooldownLevel = level
		s.cooldownLevelAt = now
	}
	s.backoffMu.Unlock()

	// Add jitter (random [0, 1s)) so retries against a shared exhausted quota
	// don't all wake simultaneously, then cap at the maximum.
	dur := time.Duration(level) * time.Second
	if jf := s.jitterFn.Load(); jf != nil && *jf != nil {
		dur += (*jf)()
	}
	if dur > maxInsufficientQuotaCooldown*time.Second {
		dur = maxInsufficientQuotaCooldown * time.Second
	}
	until := now.Add(dur)
	ks := s.getOrCreateKey(authID)
	ks.mu.Lock()
	ks.cooldown = &until
	ks.mu.Unlock()
	s.cooldownStatsMu.Lock()
	s.cooldownCount++
	s.cooldownStatsMu.Unlock()
	s.log.Printf("modelscope-ratelimit: key %s cooldown %s (insufficient_quota)", authID, dur.Truncate(time.Millisecond))
}

// resetCooldownBackoff clears the global exponential-backoff level. Called
// when a healthy response (quota above threshold) is observed, so the next
// insufficient_quota failure restarts from the configured base.
func (s *Store) resetCooldownBackoff() {
	s.backoffMu.Lock()
	s.cooldownLevel = 0
	s.cooldownLevelAt = time.Time{}
	s.backoffMu.Unlock()
}

// CooldownStats returns the daily count of insufficient_quota cooldown
// triggers and the total time spent blocking in SchedulerPick.
func (s *Store) CooldownStats() (count int, wait time.Duration) {
	s.cooldownStatsMu.Lock()
	c, w := s.cooldownCount, s.cooldownWaitNanos
	s.cooldownStatsMu.Unlock()
	return int(c), time.Duration(w)
}

// blockEnter marks the start of a cooldown sleep. When the active count
// transitions from 0 to 1, the wall-clock start time is recorded. Concurrent
// sleeps (count > 1) do not move the start — the union of all blocking
// intervals is tracked, not the sum of individual durations.
func (s *Store) blockEnter() {
	s.cooldownStatsMu.Lock()
	if s.blockActive == 0 {
		s.blockStart = time.Now()
	}
	s.blockActive++
	s.cooldownStatsMu.Unlock()
}

// blockLeave marks the end of a cooldown sleep. When the active count drops
// to 0, the elapsed wall-clock time since blockStart is accumulated. This
// gives the union of all concurrent blocking intervals, avoiding the
// over-counting that would occur if each goroutine's sleep duration were
// summed independently.
func (s *Store) blockLeave() {
	s.cooldownStatsMu.Lock()
	s.blockActive--
	if s.blockActive <= 0 {
		s.blockActive = 0
		if !s.blockStart.IsZero() {
			s.cooldownWaitNanos += int64(time.Since(s.blockStart))
			s.blockStart = time.Time{}
		}
	}
	s.cooldownStatsMu.Unlock()
}

// recordSuccess increments the global and per-key daily success counters.
// Called from OnUsage for managed-provider responses that did not fail.
func (s *Store) recordSuccess(authID string) {
	s.cooldownStatsMu.Lock()
	s.successCount++
	s.cooldownStatsMu.Unlock()
	ks := s.getOrCreateKey(authID)
	ks.mu.Lock()
	ks.successCount++
	ks.mu.Unlock()
}

// SuccessStats returns the daily count of successful managed-provider requests.
func (s *Store) SuccessStats() int {
	s.cooldownStatsMu.Lock()
	n := s.successCount
	s.cooldownStatsMu.Unlock()
	return int(n)
}

// defaultJitter returns a random duration in [0, 1s), used to stagger
// insufficient_quota cooldown retries so they don't thunder-herd.
func defaultJitter() time.Duration {
	return time.Duration(rand.Float64() * float64(time.Second))
}

// SetJitter overrides the jitter source. Tests inject a fixed or zero value
// for deterministic timing assertions; a nil argument restores the default.
func (s *Store) SetJitter(f func() time.Duration) {
	if f == nil {
		df := defaultJitter
		s.jitterFn.Store(&df)
		return
	}
	s.jitterFn.Store(&f)
}

// SetProbeFunc injects a function used in place of the real probeProxy network
// call. Tests use it to simulate a reachable or unreachable proxy without
// hitting https://api-inference.modelscope.cn. A nil argument restores the
// default (real HTTP probe).
func (s *Store) SetProbeFunc(f func(proxyURL string) bool) {
	if f == nil {
		s.probeFn.Store(nil)
		return
	}
	s.probeFn.Store(&f)
}

// SetProxyWait overrides the proxy 2s rotation/probe wait so proxy-mode tests
// run without sleeping. A zero or negative duration restores the default
// (proxyWaitDuration).
func (s *Store) SetProxyWait(d time.Duration) {
	if d <= 0 {
		s.proxyWaitNanos.Store(0)
		return
	}
	s.proxyWaitNanos.Store(int64(d))
}

// proxyWait returns the configured proxy wait, honoring the test override.
func (s *Store) proxyWait() time.Duration {
	if v := s.proxyWaitNanos.Load(); v > 0 {
		return time.Duration(v)
	}
	return proxyWaitDuration
}

// cooldownWaitDuration returns the longest remaining insufficient_quota cooldown
// among ALL keys in the store, or 0 when none are active. It checks all keys
// (not just req.Candidates) because a cooled-down key is in the host's "tried"
// retry set and therefore ABSENT from the candidate list the host passes to
// SchedulerPick. Checking only candidates would miss it entirely. Cooldowns are
// only ever set by ApplyInsufficientQuotaCooldown (managed keys only), so
// scanning the whole store is safe.
func (s *Store) cooldownWaitDuration(now time.Time) time.Duration {
	var maxWait time.Duration
	s.mu.RLock()
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.RLock()
		if ks.cooldown != nil && now.Before(*ks.cooldown) {
			if w := ks.cooldown.Sub(now); w > maxWait {
				maxWait = w
			}
		}
		ks.mu.RUnlock()
	}
	// Defensive cap: each cooldown is <= maxInsufficientQuotaCooldown, but
	// clock skew or a future-dated cooldown must never block beyond the max.
	if maxWait > maxInsufficientQuotaCooldown*time.Second {
		maxWait = maxInsufficientQuotaCooldown * time.Second
	}
	return maxWait
}

// DisableStatus is a snapshot of a key's disable state, for diagnostics.
type DisableStatus struct {
	AuthID string               `json:"auth_id"`
	Global *time.Time           `json:"global_disabled_at,omitempty"`
	Models map[string]time.Time `json:"models_disabled,omitempty"`
}

// Status returns a snapshot of every key with any active disable.
func (s *Store) Status() []DisableStatus {
	now := s.now()
	loc := s.location()
	s.mu.RLock()
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	var out []DisableStatus
	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.RLock()
		st := DisableStatus{AuthID: id, Models: map[string]time.Time{}}
		// A 401 (unauthorized) global disable persists across the midnight
		// boundary (globalReason == "unauthorized"); a daily-quota global
		// disable is only active on its own calendar day. Match isDisabled
		// and Snapshot so Status() never diverges from the scheduler's view.
		if ks.global != nil && (ks.globalReason == "unauthorized" || active(*ks.global, now, loc)) {
			t := *ks.global
			st.Global = &t
		}
		for m, t := range ks.models {
			if active(t, now, loc) {
				st.Models[m] = t
			}
		}
		ks.mu.RUnlock()
		if st.Global != nil || len(st.Models) > 0 {
			out = append(out, st)
		}
	}
	return out
}

// PruneAll removes rolled-over disables for every key. Called by the
// background cleaner and usable on demand.
func (s *Store) PruneAll(now time.Time) {
	loc := s.location()

	// Determine whether the daily boundary has crossed before iterating keys,
	// so per-key daily counters (successCount) can be reset in the same pass.
	s.cooldownStatsMu.Lock()
	dayChanged := !s.cooldownStatsDay.IsZero() && !sameDay(s.cooldownStatsDay, now, loc)
	s.cooldownStatsDay = now
	if dayChanged {
		s.cooldownCount = 0
		s.cooldownWaitNanos = 0
		s.successCount = 0
		s.blockActive = 0
		s.blockStart = time.Time{}
	}
	s.cooldownStatsMu.Unlock()

	s.mu.RLock()
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.Lock()
		if dayChanged {
			ks.successCount = 0
		}
		if ks.cooldown != nil && !now.Before(*ks.cooldown) {
			ks.cooldown = nil
		}
		if ks.global != nil && ks.globalReason != "unauthorized" && !active(*ks.global, now, loc) {
			ks.global = nil
			ks.globalReason = ""
		}
		for m, t := range ks.models {
			if !active(t, now, loc) {
				delete(ks.models, m)
			}
		}
		for m, md := range ks.modelData {
			if !sameDay(md.at, now, loc) {
				delete(ks.modelData, m)
			}
		}
		if ks.totalData != nil && !sameDay(ks.totalData.at, now, loc) {
			ks.totalData = nil
		}
		empty := ks.global == nil && len(ks.models) == 0 && len(ks.modelData) == 0 && ks.totalData == nil && ks.cooldown == nil
		ks.mu.Unlock()
		if empty {
			s.deleteKey(id)
		}
	}

	// Reset the global insufficient_quota backoff level at the daily
	// boundary: the upstream daily quota resets at midnight, so a new day
	// restarts the backoff from the configured base.
	s.backoffMu.Lock()
	if !s.cooldownLevelAt.IsZero() && !sameDay(s.cooldownLevelAt, now, loc) {
		s.cooldownLevel = 0
		s.cooldownLevelAt = time.Time{}
	}
	s.backoffMu.Unlock()

	// Reset daily insufficient_quota statistics at the midnight boundary.
	s.cooldownStatsMu.Lock()
	if !s.cooldownStatsDay.IsZero() && !sameDay(s.cooldownStatsDay, now, loc) {
		s.cooldownCount = 0
		s.cooldownWaitNanos = 0
	}
	s.cooldownStatsDay = now
	s.cooldownStatsMu.Unlock()
}

// ModelView is a render-ready snapshot of one model's rate-limit state for a
// key: last-seen remaining/limit (when known) and whether it is disabled.
type ModelView struct {
	Name       string
	Disabled   bool
	Since      time.Time
	Remaining  int
	Limit      int
	HasRem     bool
	HasLim     bool
	ObservedAt time.Time
}

// KeyView is a render-ready snapshot of one credential's state.
type KeyView struct {
	AuthID         string
	GlobalDisabled bool
	GlobalSince    time.Time
	GlobalReason   string
	Cooldown       bool
	CooldownUntil  time.Time
	Models         []ModelView
	TotalRemaining int
	TotalLimit     int
	HasTotalRem    bool
	HasTotalLim    bool
	SuccessCount   int
}

// Snapshot returns a render-ready view of every observed credential's state at
// "now". Only current-day rate-limit data is included; rolled-over disables
// and stale (previous-day) remaining values are excluded.
func (s *Store) Snapshot(now time.Time) map[string]KeyView {
	loc := s.location()
	s.mu.RLock()
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	out := make(map[string]KeyView, len(ids))
	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.RLock()
		kv := KeyView{AuthID: id}
		if ks.cooldown != nil && now.Before(*ks.cooldown) {
			kv.Cooldown = true
			kv.CooldownUntil = *ks.cooldown
		}
		if ks.global != nil && (ks.globalReason == "unauthorized" || active(*ks.global, now, loc)) {
			kv.GlobalDisabled = true
			kv.GlobalSince = *ks.global
			kv.GlobalReason = ks.globalReason
		}
		for m, md := range ks.modelData {
			if !sameDay(md.at, now, loc) {
				continue
			}
			mv := ModelView{
				Name:       s.displayName(m),
				Remaining:  md.remaining,
				Limit:      md.limit,
				HasRem:     md.hasRem,
				HasLim:     md.hasLim,
				ObservedAt: md.at,
			}
			if t, ok := ks.models[m]; ok && active(t, now, loc) {
				mv.Disabled = true
				mv.Since = t
			}
			kv.Models = append(kv.Models, mv)
		}
		if ks.totalData != nil && sameDay(ks.totalData.at, now, loc) {
			kv.TotalRemaining = ks.totalData.remaining
			kv.TotalLimit = ks.totalData.limit
			kv.HasTotalRem = ks.totalData.hasRem
			kv.HasTotalLim = ks.totalData.hasLim
		}
		kv.SuccessCount = ks.successCount
		ks.mu.RUnlock()
		out[id] = kv
	}
	return out
}
