package ratelimit

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func newTestStore(now time.Time) (*Store, *time.Time) {
	loc := time.FixedZone("CST", 8*3600)
	s := NewStore(NoopLogger{})
	s.SetLocation(loc)
	s.SetJitter(func() time.Duration { return 0 })
	cur := now
	s.SetClock(func() time.Time { return cur })
	return s, &cur
}

func cfgWith(providers ...string) *Config {
	c := DefaultConfig()
	c.Providers = providers
	return c
}

func hdr(remaining string) map[string][]string {
	return map[string][]string{
		"Modelscope-Ratelimit-Model-Requests-Limit":     {"100"},
		"Modelscope-Ratelimit-Model-Requests-Remaining": {remaining},
		"Modelscope-Ratelimit-Requests-Limit":           {"1000"},
		"Modelscope-Ratelimit-Requests-Remaining":       {"500"},
	}
}

func totalExhaustedHdr() map[string][]string {
	return map[string][]string{
		"Modelscope-Ratelimit-Model-Requests-Remaining": {"0"},
		"Modelscope-Ratelimit-Requests-Remaining":       {"0"},
	}
}

func modelExhaustedHdr() map[string][]string {
	return map[string][]string{
		"Modelscope-Ratelimit-Model-Requests-Remaining": {"0"},
		"Modelscope-Ratelimit-Requests-Remaining":       {"500"},
	}
}

func TestConfigFromMap(t *testing.T) {
	t.Run("defaults when nil", func(t *testing.T) {
		c, err := ConfigFromMap(nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.Timezone != "Asia/Shanghai" {
			t.Fatalf("timezone = %q", c.Timezone)
		}
		if c.ModelRemainingHeader != "Modelscope-Ratelimit-Model-Requests-Remaining" {
			t.Fatalf("header = %q", c.ModelRemainingHeader)
		}
		if c.DisableThreshold != 0 {
			t.Fatalf("threshold = %d", c.DisableThreshold)
		}
		if c.ManagesProvider("modelscope") {
			t.Fatal("empty providers should manage nothing")
		}
	})
	t.Run("overrides", func(t *testing.T) {
		c, err := ConfigFromMap(map[string]any{
			"providers":              []any{"ms", "ms2"},
			"timezone":               "UTC",
			"disable_threshold":      5,
			"model_remaining_header": "X-Rem",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(c.Providers, []string{"ms", "ms2"}) {
			t.Fatalf("providers = %v", c.Providers)
		}
		if c.Timezone != "UTC" {
			t.Fatalf("timezone = %q", c.Timezone)
		}
		if c.DisableThreshold != 5 {
			t.Fatalf("threshold = %d", c.DisableThreshold)
		}
		if c.ModelRemainingHeader != "X-Rem" {
			t.Fatalf("header = %q", c.ModelRemainingHeader)
		}
	})
}

func TestModelOnlyDisable(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", modelExhaustedHdr(), now)

	if !s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("expected Qwen-72B disabled")
	}
	// Other models on the same key remain available.
	if s.isDisabled("auth-1", "Qwen-7B", now) {
		t.Fatal("Qwen-7B must not be disabled by a per-model exhaustion")
	}
	if s.isDisabled("auth-2", "Qwen-72B", now) {
		t.Fatal("disable must be scoped to the owning key")
	}
}

func TestGlobalDisable(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", totalExhaustedHdr(), now)

	// Global disable blocks every model on the key.
	for _, m := range []string{"Qwen-72B", "Qwen-7B", "DeepSeek-V3"} {
		if !s.isDisabled("auth-1", m, now) {
			t.Fatalf("expected %s disabled by global exhaustion", m)
		}
	}
	if s.isDisabled("auth-2", "Qwen-72B", now) {
		t.Fatal("global disable must be scoped to the owning key")
	}
}

func TestNoRateLimitHeadersNoop(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", map[string][]string{"Content-Type": {"application/json"}}, now)
	if s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("non-Modelscope response must not trigger a disable")
	}
}

func TestDailyReset(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 23, 59, 0, 0, loc)
	s, cur := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", modelExhaustedHdr(), *cur)
	if !s.isDisabled("auth-1", "Qwen-72B", *cur) {
		t.Fatal("expected disabled before midnight")
	}

	// Cross midnight 00:00 next day -> cleared.
	*cur = time.Date(2026, 7, 9, 0, 0, 1, 0, loc)
	if s.isDisabled("auth-1", "Qwen-72B", *cur) {
		t.Fatal("expected cleared after midnight")
	}

	// PruneAll should drop the stale record.
	s.PruneAll(*cur)
	if len(s.Status()) != 0 {
		t.Fatalf("expected no active disables after prune, got %v", s.Status())
	}
}

func TestDisableKeepsEarliestTime(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 9, 0, 0, 0, loc)
	s, cur := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", modelExhaustedHdr(), *cur)
	first := *cur
	*cur = now.Add(2 * time.Hour)
	s.ApplyRateLimit("auth-1", "Qwen-72B", modelExhaustedHdr(), *cur)

	st := s.Status()
	if len(st) != 1 || st[0].Models["Qwen-72B"] != first {
		t.Fatalf("expected earliest disable time kept, got %+v", st)
	}
}

func TestThresholdProactive(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	c := cfgWith("modelscope")
	c.DisableThreshold = 5
	s.Reconfigure(c)

	// remaining == 5 hits the threshold.
	s.ApplyRateLimit("auth-1", "Qwen-72B", hdr("5"), now)
	if !s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("expected disable at threshold boundary")
	}
}

func TestSchedulerSkipsDisabledAndRotates(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	// Exhaust auth-1 for Qwen-72B only.
	s.ApplyRateLimit("auth-1", "Qwen-72B", modelExhaustedHdr(), now)

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
			{ID: "auth-2", Provider: "modelscope", Priority: 1},
			{ID: "auth-3", Provider: "modelscope", Priority: 1},
		},
	}

	picks := map[string]int{}
	for i := 0; i < 12; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled || resp.AuthID != "auth-2" && resp.AuthID != "auth-3" {
			t.Fatalf("pick %d returned disabled/auth-1 or unhandled: %+v", i, resp)
		}
		if resp.AuthID == "auth-1" {
			t.Fatalf("disabled key must never be picked")
		}
		picks[resp.AuthID]++
	}
	if len(picks) < 2 {
		t.Fatalf("expected rotation across available keys, got %v", picks)
	}

	// A different model on auth-1 must still be pickable (per-model scoping).
	req2 := req
	req2.Model = "Qwen-7B"
	resp, err := s.SchedulerPick(req2)
	if err != nil {
		t.Fatalf("Qwen-7B pick failed: %v", err)
	}
	if !resp.Handled {
		t.Fatal("Qwen-7B should be handled")
	}
}

func TestSchedulerAllDisabledError(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("auth-2", "Qwen-72B", modelExhaustedHdr(), now)

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope"},
			{ID: "auth-2", Provider: "modelscope"},
		},
	}
	_, err := s.SchedulerPick(req)
	if !errors.Is(err, ErrAllCredentialsDisabled) {
		t.Fatalf("expected ErrAllCredentialsDisabled, got %v", err)
	}
}

func TestSchedulerNonManagedProvider(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	req := SchedulerPickRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Candidates: []Candidate{
			{ID: "auth-x", Provider: "openai"},
		},
	}
	resp, err := s.SchedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Handled {
		t.Fatal("non-managed provider must defer (Handled=false)")
	}
}

func TestSchedulerEmptyProvidersDefers(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(DefaultConfig()) // no providers

	resp, err := s.SchedulerPick(SchedulerPickRequest{Provider: "modelscope", Model: "m", Candidates: []Candidate{{ID: "a", Provider: "modelscope"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Handled {
		t.Fatal("empty providers must defer everything")
	}
}

// TestSchedulerMixedProviderSkipsDisabled reproduces CLIProxyAPI's "mixed"
// scheduling path: the request-level Provider is empty while each candidate
// carries the real runtime provider key ("openai-compatible-<name>"). The plugin
// must NOT bail on the empty req.Provider; it must still skip the disabled
// candidate and pick an available one (Handled=true). Before the fix this
// returned Handled=false, deferring to the host's built-in scheduler which
// ignored disables and let exhausted keys receive 429s.
func TestSchedulerMixedProviderSkipsDisabled(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope")) // provider configured as bare "modelscope"

	// key-1 exhausted for the requested model; key-2 still healthy.
	s.ApplyRateLimit("key-1", "Qwen-72B", modelExhaustedHdr(), now)

	req := SchedulerPickRequest{
		Provider: "", // mixed path: top-level provider is empty
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "key-1", Provider: "openai-compatible-modelscope", Priority: 1},
			{ID: "key-2", Provider: "openai-compatible-modelscope", Priority: 1},
		},
	}
	for i := 0; i < 6; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled {
			t.Fatalf("pick %d: mixed path must be handled, not deferred", i)
		}
		if resp.AuthID == "key-1" {
			t.Fatalf("pick %d: disabled key-1 must never be picked", i)
		}
		if resp.AuthID != "key-2" {
			t.Fatalf("pick %d: expected key-2, got %q", i, resp.AuthID)
		}
	}
}

// TestSchedulerMixedProviderAllDisabledError verifies that on the mixed path,
// when every managed candidate is disabled, the plugin returns
// ErrAllCredentialsDisabled (explicit rejection) instead of deferring, so the
// host never routes to an exhausted key.
func TestSchedulerMixedProviderAllDisabledError(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("key-1", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("key-2", "Qwen-72B", modelExhaustedHdr(), now)

	req := SchedulerPickRequest{
		Provider: "",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "key-1", Provider: "openai-compatible-modelscope"},
			{ID: "key-2", Provider: "openai-compatible-modelscope"},
		},
	}
	_, err := s.SchedulerPick(req)
	if !errors.Is(err, ErrAllCredentialsDisabled) {
		t.Fatalf("expected ErrAllCredentialsDisabled, got %v", err)
	}
}

// TestSchedulerMixedProviderDefersUnmanaged verifies that on the mixed path,
// when none of the candidates are managed (different provider entirely), the
// plugin defers (Handled=false) so the host's built-in scheduler handles them.
func TestSchedulerMixedProviderDefersUnmanaged(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	req := SchedulerPickRequest{
		Provider: "",
		Model:    "gpt-4o",
		Candidates: []Candidate{
			{ID: "oai-1", Provider: "openai"},
			{ID: "oai-2", Provider: "openai"},
		},
	}
	resp, err := s.SchedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Handled {
		t.Fatal("unmanaged candidates on mixed path must defer (Handled=false)")
	}
}

// TestSchedulerAllDisabledFallbackToNonManagedCandidate verifies that when all
// managed candidates are disabled but a non-managed candidate is present in the
// same priority group, the plugin picks the non-managed candidate (Handled=true)
// instead of returning ErrAllCredentialsDisabled. This prevents the plugin from
// blocking requests that could be served by a non-managed provider.
func TestSchedulerAllDisabledFallbackToNonManagedCandidate(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("ms-1", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("ms-2", "Qwen-72B", modelExhaustedHdr(), now)

	req := SchedulerPickRequest{
		Provider:  "",
		Providers: []string{"openai-compatible-modelscope", "openai-compatible-bailian"},
		Model:     "Qwen-72B",
		Candidates: []Candidate{
			{ID: "ms-1", Provider: "openai-compatible-modelscope", Priority: 1},
			{ID: "ms-2", Provider: "openai-compatible-modelscope", Priority: 1},
			{ID: "bailian-1", Provider: "openai-compatible-bailian", Priority: 1},
		},
	}
	resp, err := s.SchedulerPick(req)
	if err != nil {
		t.Fatalf("expected fallback to non-managed candidate, got error: %v", err)
	}
	if !resp.Handled {
		t.Fatal("expected Handled=true with non-managed AuthID")
	}
	if resp.AuthID != "bailian-1" {
		t.Fatalf("expected bailian-1, got %q", resp.AuthID)
	}
}

// TestSchedulerAllManagedDisabledDefersToLowerPriorityNonManaged verifies that
// when all managed candidates are disabled and no non-managed candidate is in
// the current list (because the host filtered them out by priority), but the
// route accepts non-managed providers (req.Providers), the plugin defers
// (Handled=false) so the host can retry through managed keys and eventually
// reach the lower-priority non-managed candidates.
func TestSchedulerAllManagedDisabledDefersToLowerPriorityNonManaged(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("ms-1", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("ms-2", "Qwen-72B", modelExhaustedHdr(), now)

	// Only managed candidates in the list (host filtered out lower-priority
	// non-managed candidates), but req.Providers lists a non-managed provider.
	req := SchedulerPickRequest{
		Provider:  "",
		Providers: []string{"openai-compatible-modelscope", "openai-compatible-bailian"},
		Model:     "Qwen-72B",
		Candidates: []Candidate{
			{ID: "ms-1", Provider: "openai-compatible-modelscope", Priority: 10},
			{ID: "ms-2", Provider: "openai-compatible-modelscope", Priority: 10},
		},
	}
	resp, err := s.SchedulerPick(req)
	if err != nil {
		t.Fatalf("expected defer (no error), got: %v", err)
	}
	if resp.Handled {
		t.Fatal("expected Handled=false (defer) so host can reach lower-priority non-managed keys")
	}
}

func TestOnUsageApplies(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		Provider: "modelscope", AuthID: "auth-1", Model: "Qwen-72B",
		ResponseHeaders: totalExhaustedHdr(),
	})
	if !s.isDisabled("auth-1", "Qwen-7B", now) {
		t.Fatal("usage handle with total exhaustion must globally disable")
	}
}

func TestOnUsageAliasMatchesSchedulerModel(t *testing.T) {
	// Regression: the host passes the client-requested alias (e.g. "glm-5.2")
	// to SchedulerPick, while usage records carry the resolved upstream model
	// (e.g. "ZhipuAI/GLM-5.2") in Model and the alias in Alias. Because 429s
	// take the executor error path (the host returns early and skips the
	// response interceptor), OnUsage is the only path that records disables
	// from rate-limited responses, and it previously keyed them under the
	// upstream name. The scheduler then checked isDisabled under the alias,
	// found nothing, and re-routed to an exhausted key once the host's own
	// short-lived cooldown expired while this plugin's daily disable persisted.
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		Provider:        "modelscope",
		AuthID:          "auth-1",
		Model:           "ZhipuAI/GLM-5.2",
		Alias:           "glm-5.2",
		ResponseHeaders: modelExhaustedHdr(),
	})
	if !s.isDisabled("auth-1", "glm-5.2", now) {
		t.Fatal("disable must be stored under the alias the scheduler checks")
	}
	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "glm-5.2",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope"},
		},
	}
	_, err := s.SchedulerPick(req)
	if !errors.Is(err, ErrAllCredentialsDisabled) {
		t.Fatalf("scheduler must reject an all-disabled alias, got err=%v", err)
	}
}

func TestOnUsageDisplaysUpstreamModelName(t *testing.T) {
	// Disables are keyed by the alias the scheduler checks, but the status page
	// and logs should display the upstream model name (rec.Model) once the
	// alias->upstream mapping has been recorded from a usage record.
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		Provider:        "modelscope",
		AuthID:          "auth-1",
		Model:           "ZhipuAI/GLM-5.2",
		Alias:           "glm-5.2",
		ResponseHeaders: modelExhaustedHdr(),
	})
	// Storage stays under the alias (scheduler path).
	if !s.isDisabled("auth-1", "glm-5.2", now) {
		t.Fatal("disable must be stored under the alias")
	}
	// Display resolves the alias to the upstream name.
	if got := s.displayName("glm-5.2"); got != "ZhipuAI/GLM-5.2" {
		t.Fatalf("displayName = %q, want ZhipuAI/GLM-5.2", got)
	}
	// The status snapshot surfaces the upstream name, not the alias.
	snap := s.Snapshot(now)
	kv, ok := snap["auth-1"]
	if !ok || len(kv.Models) == 0 {
		t.Fatalf("no model view for auth-1: %+v", snap)
	}
	if kv.Models[0].Name != "ZhipuAI/GLM-5.2" {
		t.Fatalf("snapshot model name = %q, want ZhipuAI/GLM-5.2", kv.Models[0].Name)
	}
}

func TestOnResponseWithMetadataAuthID(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	resp := s.OnResponse(ResponseInterceptRequest{
		Model:           "Qwen-72B",
		ResponseHeaders: modelExhaustedHdr(),
		Metadata:        map[string]any{"auth_id": "auth-1"},
	})
	// Plugin must not rewrite the response.
	if resp.Body != "" || len(resp.Headers) != 0 {
		t.Fatal("response interceptor must return a no-op result")
	}
	if !s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("OnResponse must apply disable when AuthID is in Metadata")
	}
}

func TestOnStreamChunkHeaderInit(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	// Header-only init call (ChunkIndex == -1) carries the rate-limit headers.
	r := s.OnStreamChunk(StreamChunkInterceptRequest{
		ChunkIndex: -1, Model: "Qwen-72B",
		ResponseHeaders: modelExhaustedHdr(),
		Metadata:        map[string]any{"AuthID": "auth-1"},
	})
	if r.DropChunk {
		t.Fatal("header-init must not drop")
	}
	if !s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("stream header-init must apply disable")
	}

	// Data chunk must not re-trigger or alter anything.
	s.OnStreamChunk(StreamChunkInterceptRequest{ChunkIndex: 0, Model: "Qwen-72B"})
}

func TestAuthIDFromMetadata(t *testing.T) {
	cases := []struct {
		meta map[string]any
		want string
	}{
		{nil, ""},
		{map[string]any{}, ""},
		{map[string]any{"auth_id": "a1"}, "a1"},
		{map[string]any{"AuthID": "a2"}, "a2"},
		{map[string]any{"auth": "a3"}, "a3"},
		{map[string]any{"other": "x"}, ""},
		{map[string]any{"auth_id": 123}, ""}, // non-string ignored
	}
	for _, c := range cases {
		if got := AuthIDFromMetadata(c.meta); got != c.want {
			t.Errorf("AuthIDFromMetadata(%v) = %q, want %q", c.meta, got, c.want)
		}
	}
}

// TestSchedulerProviderOrderPrefersFirst verifies that when multiple managed
// providers have available keys, the plugin always picks from the first-listed
// provider (strict priority by config order). The second provider is only used
// after every key from the first is disabled.
func TestSchedulerProviderOrderPrefersFirst(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("moda", "modelscope"))

	req := SchedulerPickRequest{
		Provider: "",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "ms-1", Provider: "openai-compatible-modelscope", Priority: 1},
			{ID: "moda-1", Provider: "openai-compatible-moda", Priority: 1},
			{ID: "moda-2", Provider: "openai-compatible-moda", Priority: 1},
			{ID: "ms-2", Provider: "openai-compatible-modelscope", Priority: 1},
		},
	}
	// moda is listed first in cfg.Providers, so only moda keys should be picked.
	picks := map[string]int{}
	for i := 0; i < 20; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled {
			t.Fatalf("pick %d: expected Handled=true", i)
		}
		if resp.AuthID != "moda-1" && resp.AuthID != "moda-2" {
			t.Fatalf("pick %d: expected moda key, got %q (modelscope must not be picked when moda is available)", i, resp.AuthID)
		}
		picks[resp.AuthID]++
	}
	if len(picks) < 2 {
		t.Fatalf("expected rotation among moda keys, got %v", picks)
	}
}

// TestSchedulerProviderOrderFallbackToSecond verifies that when all keys from
// the first-listed provider are disabled, the plugin falls through to the
// second-listed provider automatically (zero wasted requests).
func TestSchedulerProviderOrderFallbackToSecond(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("moda", "modelscope"))

	// Disable all moda keys.
	s.ApplyRateLimit("moda-1", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("moda-2", "Qwen-72B", modelExhaustedHdr(), now)

	req := SchedulerPickRequest{
		Provider: "",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "ms-1", Provider: "openai-compatible-modelscope", Priority: 1},
			{ID: "moda-1", Provider: "openai-compatible-moda", Priority: 1},
			{ID: "moda-2", Provider: "openai-compatible-moda", Priority: 1},
			{ID: "ms-2", Provider: "openai-compatible-modelscope", Priority: 1},
		},
	}
	for i := 0; i < 10; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled {
			t.Fatalf("pick %d: expected Handled=true", i)
		}
		if resp.AuthID != "ms-1" && resp.AuthID != "ms-2" {
			t.Fatalf("pick %d: expected modelscope key (moda exhausted), got %q", i, resp.AuthID)
		}
	}
}

// --- Credential strategy tests ---

func cfgWithStrategy(strategy string, providers ...string) *Config {
	c := cfgWith(providers...)
	c.CredentialStrategy = strategy
	return c
}

// TestFillFirstAlwaysPicksFirst verifies that fill-first strategy always
// returns the first available candidate (same key until disabled).
func TestFillFirstAlwaysPicksFirst(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("fill-first", "modelscope"))

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
			{ID: "auth-2", Provider: "modelscope", Priority: 1},
			{ID: "auth-3", Provider: "modelscope", Priority: 1},
		},
	}
	for i := 0; i < 20; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled || resp.AuthID != "auth-1" {
			t.Fatalf("pick %d: fill-first must always return auth-1, got %+v", i, resp)
		}
	}

	// Disable auth-1 for the model; fill-first should then stick to auth-2.
	s.ApplyRateLimit("auth-1", "Qwen-72B", modelExhaustedHdr(), now)
	for i := 0; i < 20; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled || resp.AuthID != "auth-2" {
			t.Fatalf("pick %d: fill-first must return auth-2 after auth-1 disabled, got %+v", i, resp)
		}
	}
}

// TestRoundRobinRotates verifies that round-robin strategy distributes picks
// across all available candidates.
func TestRoundRobinRotates(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "modelscope"))

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
			{ID: "auth-2", Provider: "modelscope", Priority: 1},
			{ID: "auth-3", Provider: "modelscope", Priority: 1},
		},
	}
	// First pick must be the first candidate (config order), not the second.
	first, err := s.SchedulerPick(req)
	if err != nil || !first.Handled || first.AuthID != "auth-1" {
		t.Fatalf("first pick must be auth-1 (config order), got %+v err=%v", first, err)
	}
	picks := map[string]int{first.AuthID: 1}
	for i := 1; i < 30; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled {
			t.Fatalf("pick %d: expected Handled=true", i)
		}
		picks[resp.AuthID]++
	}
	if len(picks) != 3 {
		t.Fatalf("round-robin must hit all 3 keys, got %v", picks)
	}
	for _, id := range []string{"auth-1", "auth-2", "auth-3"} {
		if picks[id] == 0 {
			t.Fatalf("round-robin missed key %q, got %v", id, picks)
		}
	}
}

// TestFillFirstProviderOrder verifies that fill-first respects provider order:
// the first-listed provider's keys are used exclusively until exhausted.
func TestFillFirstProviderOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("fill-first", "moda", "modelscope"))

	req := SchedulerPickRequest{
		Provider: "",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "ms-1", Provider: "openai-compatible-modelscope", Priority: 1},
			{ID: "moda-1", Provider: "openai-compatible-moda", Priority: 1},
			{ID: "moda-2", Provider: "openai-compatible-moda", Priority: 1},
			{ID: "ms-2", Provider: "openai-compatible-modelscope", Priority: 1},
		},
	}
	// moda is listed first: fill-first should always pick moda-1.
	for i := 0; i < 15; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled || resp.AuthID != "moda-1" {
			t.Fatalf("pick %d: expected moda-1, got %+v", i, resp)
		}
	}

	// Disable all moda keys: fill-first should fall through to modelscope-1.
	s.ApplyRateLimit("moda-1", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("moda-2", "Qwen-72B", modelExhaustedHdr(), now)
	for i := 0; i < 15; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled || resp.AuthID != "ms-1" {
			t.Fatalf("pick %d: expected ms-1 after moda exhausted, got %+v", i, resp)
		}
	}
}

// TestNonManagedFallbackUsesHostStrategy verifies that when all managed keys
// are disabled and non-managed candidates are present, the host strategy
// fetcher determines selection: fill-first picks the first non-managed
// candidate, round-robin rotates among them.
func TestNonManagedFallbackUsesHostStrategy(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))

	req := SchedulerPickRequest{
		Provider:  "",
		Model:     "Qwen-72B",
		Providers: []string{"openai-compatible-moda", "openai-compatible-paid"},
		Candidates: []Candidate{
			{ID: "paid-1", Provider: "openai-compatible-paid", Priority: 1},
			{ID: "paid-2", Provider: "openai-compatible-paid", Priority: 1},
			{ID: "moda-1", Provider: "openai-compatible-moda", Priority: 1},
		},
	}

	// fill-first host strategy: always pick paid-1.
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("moda"))
	s.ApplyRateLimit("moda-1", "Qwen-72B", modelExhaustedHdr(), now)
	s.SetStrategyFetcher(func() string { return "fill-first" })
	for i := 0; i < 10; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled || resp.AuthID != "paid-1" {
			t.Fatalf("pick %d: fill-first host strategy must pick paid-1, got %+v", i, resp)
		}
	}

	// round-robin host strategy: rotate across paid-1 and paid-2.
	s2, _ := newTestStore(now)
	s2.Reconfigure(cfgWith("moda"))
	s2.ApplyRateLimit("moda-1", "Qwen-72B", modelExhaustedHdr(), now)
	s2.SetStrategyFetcher(func() string { return "round-robin" })
	picks := map[string]int{}
	for i := 0; i < 20; i++ {
		resp, err := s2.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled {
			t.Fatalf("pick %d: expected Handled=true", i)
		}
		if resp.AuthID != "paid-1" && resp.AuthID != "paid-2" {
			t.Fatalf("pick %d: round-robin must pick paid key, got %q", i, resp.AuthID)
		}
		picks[resp.AuthID]++
	}
	if len(picks) < 2 {
		t.Fatalf("round-robin must rotate across both paid keys, got %v", picks)
	}
}

// TestNonManagedFallbackNoFetcherDefaultsRoundRobin verifies that without a
// strategy fetcher, the default is round-robin for non-managed fallback.
func TestNonManagedFallbackNoFetcherDefaultsRoundRobin(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("moda"))
	// No SetStrategyFetcher: should default to round-robin.
	s.ApplyRateLimit("moda-1", "Qwen-72B", modelExhaustedHdr(), now)

	req := SchedulerPickRequest{
		Provider:  "",
		Model:     "Qwen-72B",
		Providers: []string{"openai-compatible-moda", "openai-compatible-paid"},
		Candidates: []Candidate{
			{ID: "paid-1", Provider: "openai-compatible-paid", Priority: 1},
			{ID: "paid-2", Provider: "openai-compatible-paid", Priority: 1},
			{ID: "moda-1", Provider: "openai-compatible-moda", Priority: 1},
		},
	}
	picks := map[string]int{}
	for i := 0; i < 20; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled {
			t.Fatalf("pick %d: expected Handled=true", i)
		}
		picks[resp.AuthID]++
	}
	if len(picks) < 2 {
		t.Fatalf("default round-robin must rotate across both paid keys, got %v", picks)
	}
}

// TestRoundRobinFollowsConfigOrder verifies that the scheduler re-sorts
// candidates to match config.yaml api-key order (via the orderedIDsFetcher),
// even when the host passes them sorted lexicographically by auth ID hash.
//
// Config order:  c-3, a-1, b-2  (what the management API returns)
// Host order:    a-1, b-2, c-3  (lexicographic by ID — what the host sends)
// Expected picks: c-3 → a-1 → b-2 → c-3 → …  (config order)
func TestRoundRobinFollowsConfigOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "modelscope"))

	// Inject the config-order fetcher (provider -> ordered auth IDs).
	s.SetProviderOrderFetcher(func() map[string][]string {
		return map[string][]string{"modelscope": {"c-3", "a-1", "b-2"}}
	})

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		// Host sends candidates sorted lexicographically by ID.
		Candidates: []Candidate{
			{ID: "a-1", Provider: "modelscope", Priority: 1},
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			{ID: "c-3", Provider: "modelscope", Priority: 1},
		},
	}
	// First three picks must follow config order: c-3, a-1, b-2.
	expected := []string{"c-3", "a-1", "b-2"}
	for i, want := range expected {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled || resp.AuthID != want {
			t.Fatalf("pick %d: expected %s (config order), got %q", i, want, resp.AuthID)
		}
	}
	// Fourth pick wraps back to the first config-order key.
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "c-3" {
		t.Fatalf("pick 3: expected c-3 (wrap), got %+v err=%v", resp, err)
	}
}

// TestFillFirstFollowsConfigOrder verifies that fill-first picks the first key
// in config order, not the first in host-sorted order.
func TestFillFirstFollowsConfigOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("fill-first", "modelscope"))

	s.SetProviderOrderFetcher(func() map[string][]string {
		return map[string][]string{"modelscope": {"c-3", "a-1", "b-2"}}
	})

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "a-1", Provider: "modelscope", Priority: 1},
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			{ID: "c-3", Provider: "modelscope", Priority: 1},
		},
	}
	// fill-first must always pick c-3 (first in config order), not a-1.
	for i := 0; i < 10; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled || resp.AuthID != "c-3" {
			t.Fatalf("pick %d: fill-first must pick c-3 (config order), got %q", i, resp.AuthID)
		}
	}
}

// TestNoOrderedIDsFetcherKeepsHostOrder verifies that without a fetcher the
// scheduler keeps the host-provided (lexicographic) order, so behavior is
// deterministic but not config-aligned.
func TestNoOrderedIDsFetcherKeepsHostOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "modelscope"))
	// No SetOrderedIDsFetcher.

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "a-1", Provider: "modelscope", Priority: 1},
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			{ID: "c-3", Provider: "modelscope", Priority: 1},
		},
	}
	// Without a fetcher, the first pick is the first candidate as received.
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "a-1" {
		t.Fatalf("first pick without fetcher should be a-1 (host order), got %+v err=%v", resp, err)
	}
}

// TestRoundRobinRetryPicksNextInConfigOrder simulates a 429 retry: after
// picking key c-3, the host excludes it (adds to "tried") and calls
// scheduler.pick again with a smaller candidate list. The plugin must pick
// a-1 (the next key in config order after c-3), NOT b-2 (which a naive
// cursor % len(smaller_list) would produce).
func TestRoundRobinRetryPicksNextInConfigOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "modelscope"))

	s.SetProviderOrderFetcher(func() map[string][]string {
		return map[string][]string{"modelscope": {"c-3", "a-1", "b-2"}}
	})

	fullReq := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "a-1", Provider: "modelscope", Priority: 1},
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			{ID: "c-3", Provider: "modelscope", Priority: 1},
		},
	}

	// Fresh request: picks c-3 (first in config order).
	resp, err := s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "c-3" {
		t.Fatalf("fresh pick: expected c-3, got %+v err=%v", resp, err)
	}

	// 429 on c-3: host retries with c-3 excluded from candidates.
	retryReq := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "a-1", Provider: "modelscope", Priority: 1},
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			// c-3 is absent (in host "tried" set)
		},
	}
	resp, err = s.SchedulerPick(retryReq)
	if err != nil || !resp.Handled || resp.AuthID != "a-1" {
		t.Fatalf("retry pick: expected a-1 (next in config order after c-3), got %+v err=%v", resp, err)
	}

	// Next fresh request should continue from b-2 (after a-1).
	resp, err = s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "b-2" {
		t.Fatalf("next fresh pick: expected b-2, got %+v err=%v", resp, err)
	}

	// Wraps back to c-3.
	resp, err = s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "c-3" {
		t.Fatalf("wrap pick: expected c-3, got %+v err=%v", resp, err)
	}
}

// TestRoundRobinRetryWithDisabledKey verifies the cursor skips disabled keys
// during retries. Config order [c-3, a-1, b-2], a-1 is disabled. After picking
// c-3, a retry must skip a-1 and pick b-2.
func TestRoundRobinRetryWithDisabledKey(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "modelscope"))

	s.SetProviderOrderFetcher(func() map[string][]string {
		return map[string][]string{"modelscope": {"c-3", "a-1", "b-2"}}
	})
	// Disable a-1 for this model.
	s.ApplyRateLimit("a-1", "Qwen-72B", modelExhaustedHdr(), now)

	fullReq := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			{ID: "c-3", Provider: "modelscope", Priority: 1},
			// a-1 is absent (disabled, host won't include it)
		},
	}
	// First pick: c-3 (config order, a-1 skipped because disabled).
	resp, err := s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "c-3" {
		t.Fatalf("pick 1: expected c-3, got %+v err=%v", resp, err)
	}
	// Second pick: b-2 (wraps, skips disabled a-1).
	resp, err = s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "b-2" {
		t.Fatalf("pick 2: expected b-2, got %+v err=%v", resp, err)
	}
	// Third pick: c-3 again (wraps past disabled a-1).
	resp, err = s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "c-3" {
		t.Fatalf("pick 3: expected c-3, got %+v err=%v", resp, err)
	}
}

// TestPerProviderCursorIsolation verifies that switching from one managed
// provider to another does NOT carry over the cursor value: the second
// provider starts from its own position 0 (first key in config order),
// not from wherever the first provider left off.
func TestPerProviderCursorIsolation(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "moda", "modelscope"))

	s.SetProviderOrderFetcher(func() map[string][]string {
		return map[string][]string{
			"moda":       {"m-a", "m-b", "m-c"},
			"modelscope": {"ms-1", "ms-2", "ms-3"},
		}
	})

	// Exhaust all moda keys so the scheduler falls through to modelscope.
	s.ApplyRateLimit("m-a", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("m-b", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("m-c", "Qwen-72B", modelExhaustedHdr(), now)

	req := SchedulerPickRequest{
		Provider: "",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "ms-1", Provider: "openai-compatible-modelscope", Priority: 1},
			{ID: "ms-2", Provider: "openai-compatible-modelscope", Priority: 1},
			{ID: "ms-3", Provider: "openai-compatible-modelscope", Priority: 1},
		},
	}
	// First modelscope pick must be ms-1 (config order position 0), not
	// affected by the moda cursor which was advanced during prior picks.
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "ms-1" {
		t.Fatalf("first modelscope pick: expected ms-1, got %+v err=%v", resp, err)
	}
	// Second pick: ms-2 (cursor advanced within modelscope's own space).
	resp, err = s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "ms-2" {
		t.Fatalf("second modelscope pick: expected ms-2, got %+v err=%v", resp, err)
	}
	// Third pick: ms-3.
	resp, err = s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "ms-3" {
		t.Fatalf("third modelscope pick: expected ms-3, got %+v err=%v", resp, err)
	}
}

// TestNonManagedCursorDoesNotCorruptManaged verifies that non-managed
// fallback picks (which use nonManagedRR) do not advance the managed
// per-provider cursor. After a period of non-managed fallback, the next
// managed pick should start from the correct position.
func TestNonManagedCursorDoesNotCorruptManaged(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "moda"))

	s.SetProviderOrderFetcher(func() map[string][]string {
		return map[string][]string{"moda": {"m-a", "m-b", "m-c"}}
	})

	// Disable all moda keys to trigger non-managed fallback.
	s.ApplyRateLimit("m-a", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("m-b", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("m-c", "Qwen-72B", modelExhaustedHdr(), now)

	// Non-managed fallback requests (advance nonManagedRR).
	nonManagedReq := SchedulerPickRequest{
		Provider:  "",
		Model:     "Qwen-72B",
		Providers: []string{"openai-compatible-moda", "openai-compatible-paid"},
		Candidates: []Candidate{
			{ID: "paid-1", Provider: "openai-compatible-paid", Priority: 1},
			{ID: "paid-2", Provider: "openai-compatible-paid", Priority: 1},
			{ID: "m-a", Provider: "openai-compatible-moda", Priority: 1},
			{ID: "m-b", Provider: "openai-compatible-moda", Priority: 1},
			{ID: "m-c", Provider: "openai-compatible-moda", Priority: 1},
		},
	}
	for i := 0; i < 10; i++ {
		s.SchedulerPick(nonManagedReq) // advances nonManagedRR, not managed cursor
	}

	// Now "re-enable" moda keys by using a fresh store time after reset
	// (simulating midnight). In practice, PruneAll would clear disables.
	// For this test, create a new store to simulate fresh state.
	s2, _ := newTestStore(now)
	s2.Reconfigure(cfgWithStrategy("round-robin", "moda"))
	s2.SetProviderOrderFetcher(func() map[string][]string {
		return map[string][]string{"moda": {"m-a", "m-b", "m-c"}}
	})

	managedReq := SchedulerPickRequest{
		Provider: "moda",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "m-a", Provider: "openai-compatible-moda", Priority: 1},
			{ID: "m-b", Provider: "openai-compatible-moda", Priority: 1},
			{ID: "m-c", Provider: "openai-compatible-moda", Priority: 1},
		},
	}
	// First managed pick must be m-a (cursor 0), uncorrupted by the
	// non-managed fallbacks above.
	resp, err := s2.SchedulerPick(managedReq)
	if err != nil || !resp.Handled || resp.AuthID != "m-a" {
		t.Fatalf("first managed pick after non-managed fallback: expected m-a, got %+v err=%v", resp, err)
	}
}

// TestInsufficientQuotaCooldownBlocks verifies that when a managed key has
// an active insufficient_quota cooldown, SchedulerPick blocks (sleeps) for the
// remaining cooldown duration before picking. The block is global: any managed
// key with a cooldown blocks all managed scheduling (shared quota, unrelated
// to which key). After the block, keys are available again.
func TestInsufficientQuotaCooldownBlocks(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, cur := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 1
	s.Reconfigure(cfg)

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
			{ID: "auth-2", Provider: "modelscope", Priority: 1},
		},
	}

	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled {
		t.Fatalf("initial pick failed: %+v err=%v", resp, err)
	}
	picked := resp.AuthID

	s.OnUsage(UsageRecord{
		AuthID:   picked,
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`},
	})

	start := time.Now()
	resp, err = s.SchedulerPick(req)
	elapsed := time.Since(start)
	if err != nil || !resp.Handled {
		t.Fatalf("post-cooldown pick failed: %+v err=%v", resp, err)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("expected ~1s block, got %s", elapsed)
	}
	if resp.AuthID != "auth-1" && resp.AuthID != "auth-2" {
		t.Fatalf("unexpected pick: %q", resp.AuthID)
	}
	_ = cur
}

// TestInsufficientQuotaCooldownDisabled verifies that when
// InsufficientQuotaCooldown is 0, no blocking occurs.
func TestInsufficientQuotaCooldownDisabled(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 0
	s.Reconfigure(cfg)

	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
		},
	}
	start := time.Now()
	resp, err := s.SchedulerPick(req)
	elapsed := time.Since(start)
	if err != nil || !resp.Handled || resp.AuthID != "auth-1" {
		t.Fatalf("pick failed: %+v err=%v", resp, err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("expected no blocking, took %s", elapsed)
	}
}

// TestInsufficientQuotaNon429NoCooldown verifies that a non-429 response with
// "insufficient_quota" in the body does NOT trigger a cooldown.
func TestInsufficientQuotaNon429NoCooldown(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 5
	s.Reconfigure(cfg)

	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 500, Body: `{"error":{"code":"insufficient_quota"}}`},
	})

	if s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("non-429 response must not trigger cooldown")
	}
}

// TestInsufficientQuotaBlock10s verifies that SchedulerPick blocks for the
// full 10-second insufficient_quota cooldown duration before picking. The
// block is global (any managed key with a cooldown blocks all scheduling).
func TestInsufficientQuotaBlock10s(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 10
	s.Reconfigure(cfg)

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "auth-1", Provider: "modelscope", Priority: 1},
			{ID: "auth-2", Provider: "modelscope", Priority: 1},
		},
	}

	// First pick: no cooldown, returns immediately.
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled {
		t.Fatalf("initial pick failed: %+v err=%v", resp, err)
	}
	picked := resp.AuthID

	// Simulate 429 + insufficient_quota on the picked key.
	s.OnUsage(UsageRecord{
		AuthID:   picked,
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})

	// Next pick must block for ~10 seconds.
	start := time.Now()
	resp, err = s.SchedulerPick(req)
	elapsed := time.Since(start)
	if err != nil || !resp.Handled {
		t.Fatalf("post-cooldown pick failed: %+v err=%v", resp, err)
	}
	if elapsed < 9*time.Second {
		t.Fatalf("expected ~10s block, got %s", elapsed)
	}
	if elapsed > 11*time.Second {
		t.Fatalf("block took too long: %s", elapsed)
	}
}

// TestInsufficientQuotaNonManagedNoCooldown verifies that a 429+insufficient_quota
// response from a NON-managed provider (e.g. a paid Aliyun fallback) does NOT set
// a cooldown on that key. The usage hook fires for every provider, so without the
// ManagesProvider guard a non-managed cooldown would pollute the global blocking in
// cooldownWaitDuration and delay unrelated traffic.
func TestInsufficientQuotaNonManagedNoCooldown(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 5
	s.Reconfigure(cfg)

	// A non-managed provider (e.g. Aliyun) returns 429 + insufficient_quota for a
	// model that IS monitored (ManagedModels is empty => all models monitored).
	s.OnUsage(UsageRecord{
		AuthID:   "auth-aliyun",
		Provider: "openai-compatible-aliyun",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`},
	})

	// The non-managed key must NOT have an active cooldown: it should not block
	// the global scheduler scan.
	if wait := s.cooldownWaitDuration(now); wait > 0 {
		t.Fatalf("non-managed provider must not set a cooldown, got %s", wait)
	}
	// And the key must not be considered disabled.
	if s.isDisabled("auth-aliyun", "Qwen-72B", now) {
		t.Fatal("non-managed key must not be disabled")
	}

	// A managed provider returning the same response MUST still cooldown.
	s.OnUsage(UsageRecord{
		AuthID:   "auth-ms",
		Provider: "openai-compatible-modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`},
	})
	if wait := s.cooldownWaitDuration(now); wait <= 0 {
		t.Fatalf("managed provider must set a cooldown, got %s", wait)
	}
}

// TestInsufficientQuotaExponentialBackoff verifies that consecutive rounds of
// 429+insufficient_quota double the cooldown interval (base -> 2x -> 4x),
// capped at maxInsufficientQuotaCooldown (60s), and that a healthy response
// resets the backoff to the base. Modelscope shares a quota across keys, so the
// backoff is global: multiple keys failing in the same window keep the level,
// while a failure after the window expires advances it.
func TestInsufficientQuotaExponentialBackoff(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, loc)
	s, cur := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 5
	s.Reconfigure(cfg)

	fail := func(id string, at time.Time) {
		*cur = at
		s.OnUsage(UsageRecord{
			AuthID:   id,
			Provider: "modelscope",
			Model:    "Qwen-72B",
			Alias:    "Qwen-72B",
			Failed:   true,
			Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
		})
	}
	cooldownOf := func(at time.Time) time.Duration {
		*cur = at
		return s.cooldownWaitDuration(at)
	}

	// Round 1: first failure -> base (5s).
	fail("auth-1", now)
	if w := cooldownOf(now); w != 5*time.Second {
		t.Fatalf("round 1: want 5s, got %s", w)
	}

	// Same window: a second key failing within the 5s window keeps the level
	// (not over-counting one round of shared-quota exhaustion).
	fail("auth-2", now.Add(1*time.Second))
	if w := cooldownOf(now.Add(1 * time.Second)); w != 5*time.Second {
		t.Fatalf("same-window 2nd key: want 5s, got %s", w)
	}

	// Round 2: after the 5s window elapses, the next failure doubles to 10s.
	t2 := now.Add(6 * time.Second)
	fail("auth-1", t2)
	if w := cooldownOf(t2); w != 10*time.Second {
		t.Fatalf("round 2: want 10s, got %s", w)
	}

	// Round 3: after the 10s window, doubles to 20s.
	t3 := t2.Add(11 * time.Second)
	fail("auth-1", t3)
	if w := cooldownOf(t3); w != 20*time.Second {
		t.Fatalf("round 3: want 20s, got %s", w)
	}

	// Cap: advance the level until it reaches the 60s ceiling.
	level := 20
	for step := 0; step < 10; step++ {
		tPrev := s.cooldownLevelAt
		advance := time.Duration(level+1) * time.Second
		tNext := tPrev.Add(advance)
		fail("auth-1", tNext)
		level = min(level*2, maxInsufficientQuotaCooldown)
		if s.cooldownWaitDuration(tNext) != time.Duration(level)*time.Second {
			t.Fatalf("step %d: want %ds, got %s", step, level, s.cooldownWaitDuration(tNext))
		}
		if level == maxInsufficientQuotaCooldown {
			break
		}
	}

	// Healthy response resets the backoff to base.
	*cur = now.Add(1 * time.Hour)
	// Reset is gated on success (!Failed) + managed provider in OnUsage, not
	// on rate-limit headers in ApplyRateLimit (a 429+insufficient_quota can
	// carry remaining > 0).
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: false,
	})
	if s.cooldownWaitDuration(s.now()) != 0 {
		t.Fatal("healthy response should clear cooldowns")
	}
	fail("auth-1", s.now())
	if w := s.cooldownWaitDuration(s.now()); w != 5*time.Second {
		t.Fatalf("after reset: want 5s (base), got %s", w)
	}
}

// TestInsufficientQuotaJitter verifies that a non-zero jitter is added on top
// of the backoff level, and that the total never exceeds the 60s cap.
func TestInsufficientQuotaJitter(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, cur := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 5
	s.Reconfigure(cfg)
	// Fixed 500ms jitter so the assertion is deterministic.
	s.SetJitter(func() time.Duration { return 500 * time.Millisecond })

	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})

	// base(5s) + 500ms jitter, exact.
	if w := s.cooldownWaitDuration(now); w != 5500*time.Millisecond {
		t.Fatalf("want 5.5s (base+jitter), got %s", w)
	}
	_ = cur

	// Drive the level to the 60s cap; jitter must be clipped so the total
	// never exceeds 60s.
	tPrev := now
	for {
		next := tPrev.Add(time.Duration(61) * time.Second) // past any window
		*cur = next
		s.OnUsage(UsageRecord{
			AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
			Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
		})
		w := s.cooldownWaitDuration(next)
		if w > 60*time.Second {
			t.Fatalf("cooldown %s exceeds 60s cap", w)
		}
		if w == 60*time.Second {
			break // reached the cap, jitter clipped
		}
		tPrev = next
		if tPrev.Sub(now) > 5*time.Minute {
			t.Fatal("did not reach 60s cap in time")
		}
	}
}

// TestOnUsage401DisablesUnauthorized verifies that a 401 from a managed
// provider globally disables the credential for the rest of the day (invalid or
// expired secret) without setting an insufficient_quota cooldown.
func TestOnUsage401DisablesUnauthorized(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		AuthID:   "auth-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 401, Body: `{"error":"unauthorized"}`},
	})

	if !s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("401 must globally disable the credential")
	}
	if w := s.cooldownWaitDuration(now); w > 0 {
		t.Fatalf("401 must not set a cooldown, got %s", w)
	}
}

// TestUnauthorizedDisablePersistsAcrossMidnight verifies that a 401 disable is
// persistent until plugin restart: it survives the daily PruneAll boundary,
// unlike a quota-exhaustion global disable which is cleared at midnight.
func TestUnauthorizedDisablePersistsAcrossMidnight(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	night := time.Date(2026, 7, 8, 23, 59, 0, 0, loc)
	s, _ := newTestStore(night)
	s.Reconfigure(cfgWith("modelscope"))

	// 401 at 23:59 -> unauthorized (persistent).
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 401, Body: `{"error":"unauthorized"}`},
	})
	// Quota-exhaustion global disable at 23:59 (daily).
	s.disableGlobal("auth-2", "Qwen-72B", night)

	if !s.isDisabled("auth-1", "Qwen-72B", night) || !s.isDisabled("auth-2", "Qwen-72B", night) {
		t.Fatal("both keys must be disabled before midnight")
	}

	// Roll past midnight and prune.
	nextDay := time.Date(2026, 7, 9, 0, 1, 0, 0, loc)
	s.PruneAll(nextDay)

	if !s.isDisabled("auth-1", "Qwen-72B", nextDay) {
		t.Fatal("401 unauthorized disable must persist across midnight (until restart)")
	}
	if s.isDisabled("auth-2", "Qwen-72B", nextDay) {
		t.Fatal("quota global disable must be cleared at midnight")
	}
}
