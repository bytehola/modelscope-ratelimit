package ratelimit

import (
	"testing"
	"time"
)

// insufficientQuotaBody is a 429 body containing "insufficient_quota" so
// isInsufficientQuota detects it.
const insufficientQuotaBody = `{"error":{"code":"insufficient_quota"}}`

// TestProviderKeyRotationNoSkip verifies the simultaneous provider+key
// rotation when skip_provider_on_429 is OFF:
//
//	Providers: modelscope, modelscope-ai, modelscope-my (4 keys each).
//
// First response cycle:
//  1. modelscope key1 -> 429
//  2. modelscope-ai  key1 -> 429
//  3. modelscope-my  key1 -> 429
//  4. modelscope key2 -> 200
//
// Second response cycle (next request, same global provider cursor reused):
//  1. modelscope key3 -> 429
//  2. modelscope-ai  key2 -> 429
//  3. modelscope-my  key2 -> 429
//  4. modelscope key4 -> 429
//  5. modelscope-ai  key3 -> 200
func TestProviderKeyRotationNoSkip(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, loc)
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope", "modelscope-ai", "modelscope-my")
	cfg.InsufficientQuotaCooldown = 0 // no blocking sleeps; advance is independent
	cfg.SkipProviderOn429 = false
	s.Reconfigure(cfg)

	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider:   "modelscope",
			Providers:  []string{"modelscope", "modelscope-ai", "modelscope-my"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}
	allCands := func(exclude ...string) []Candidate {
		seen := map[string]bool{}
		for _, id := range exclude {
			seen[id] = true
		}
		base := []struct {
			p, id string
		}{
			{"modelscope", "m-1"}, {"modelscope", "m-2"}, {"modelscope", "m-3"}, {"modelscope", "m-4"},
			{"modelscope-ai", "a-1"}, {"modelscope-ai", "a-2"}, {"modelscope-ai", "a-3"}, {"modelscope-ai", "a-4"},
			{"modelscope-my", "y-1"}, {"modelscope-my", "y-2"}, {"modelscope-my", "y-3"}, {"modelscope-my", "y-4"},
		}
		out := make([]Candidate, 0, len(base))
		for _, e := range base {
			if seen[e.id] {
				continue
			}
			out = append(out, Candidate{ID: e.id, Provider: e.p, Priority: 1})
		}
		return out
	}
	usage429 := func(authID, provider string) {
		s.OnUsage(UsageRecord{
			AuthID:   authID,
			Provider: provider,
			Model:    "Qwen-72B",
			Alias:    "Qwen-72B",
			Failed:   true,
			Failure:  &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody},
		})
	}
	usage200 := func(authID, provider string) {
		s.OnUsage(UsageRecord{
			AuthID:   authID,
			Provider: provider,
			Model:    "Qwen-72B",
			Alias:    "Qwen-72B",
		})
	}

	type step struct {
		wantID    string
		exclude   []string // candidate IDs excluded by host so far
		failed    bool
		fprovider string
	}
	steps := []step{
		// First response cycle.
		{wantID: "m-1", exclude: nil, failed: true, fprovider: "modelscope"},
		{wantID: "a-1", exclude: []string{"m-1"}, failed: true, fprovider: "modelscope-ai"},
		{wantID: "y-1", exclude: []string{"m-1", "a-1"}, failed: true, fprovider: "modelscope-my"},
		{wantID: "m-2", exclude: []string{"m-1", "a-1", "y-1"}, failed: false, fprovider: "modelscope"},
		// Second response cycle.
		{wantID: "m-3", exclude: nil, failed: true, fprovider: "modelscope"},
		{wantID: "a-2", exclude: []string{"m-3"}, failed: true, fprovider: "modelscope-ai"},
		{wantID: "y-2", exclude: []string{"m-3", "a-2"}, failed: true, fprovider: "modelscope-my"},
		{wantID: "m-4", exclude: []string{"m-3", "a-2", "y-2"}, failed: true, fprovider: "modelscope"},
		{wantID: "a-3", exclude: []string{"m-3", "a-2", "y-2", "m-4"}, failed: false, fprovider: "modelscope-ai"},
	}
	for i, st := range steps {
		req := mkReq(allCands(st.exclude...)...)
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("step %d: SchedulerPick err=%v", i+1, err)
		}
		if resp.AuthID != st.wantID {
			t.Fatalf("step %d: want %s, got %s", i+1, st.wantID, resp.AuthID)
		}
		if st.failed {
			usage429(resp.AuthID, st.fprovider)
		} else {
			usage200(resp.AuthID, st.fprovider)
		}
	}
}

// TestProviderRotationResetsOnSuccess verifies that any 200 resets the
// within-request cross-provider cursor to 0, so the next request always
// starts from the first managed provider; only per-provider key cursors keep
// rotating across requests.
func TestProviderRotationResetsOnSuccess(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, loc)
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope", "modelscope-ai", "modelscope-my")
	cfg.InsufficientQuotaCooldown = 0
	cfg.SkipProviderOn429 = false
	s.Reconfigure(cfg)

	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider:   "modelscope",
			Providers:  []string{"modelscope", "modelscope-ai", "modelscope-my"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}

	r1, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "modelscope-ai", Priority: 1},
		Candidate{ID: "a-2", Provider: "modelscope-ai", Priority: 1},
	))
	if err != nil || r1.AuthID != "m-1" {
		t.Fatalf("1st pick: want m-1, got %+v err=%v", r1, err)
	}
	// Request 1: ms#1 429 -> cursor advances to ai.
	s.OnUsage(UsageRecord{
		AuthID: "m-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody},
	})

	r2, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "modelscope-ai", Priority: 1},
		Candidate{ID: "a-2", Provider: "modelscope-ai", Priority: 1},
	))
	if err != nil || r2.AuthID != "a-1" {
		t.Fatalf("2nd pick: want a-1 (cursor advanced to ai), got %+v err=%v", r2, err)
	}

	// ai#1 200 -> cursor resets to 0; next request starts at ms (not ai).
	s.OnUsage(UsageRecord{AuthID: "a-1", Provider: "modelscope-ai", Model: "Qwen-72B", Alias: "Qwen-72B"})
	// Request 2: cursor reset -> ms#2 (ms key cursor advanced past m-1).
	r3, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "modelscope-ai", Priority: 1},
	))
	if err != nil || r3.AuthID != "m-2" {
		t.Fatalf("3rd pick after 200: want m-2 (reset to first provider), got %+v err=%v", r3, err)
	}
}

// TestProviderKeyRotationResetsOnSuccess_UserExample reproduces the documented
// per-request rotation: each request starts from the first provider, a 429
// advances to the next provider within the same request, and any 200 resets
// so the next request starts from the first provider again. Only keys keep
// rotating across requests.
//
//	第一轮：ms#1 429 -> ai#1 429 -> my#1 200
//	第二轮：ms#2 429 -> ai#2 200
//	第三轮：ms#3 200
func TestProviderKeyRotationResetsOnSuccess_UserExample(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, loc)
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope", "modelscope-ai", "modelscope-my")
	cfg.InsufficientQuotaCooldown = 0
	cfg.SkipProviderOn429 = false
	s.Reconfigure(cfg)

	base := []struct {
		p, id string
	}{
		{"modelscope", "m-1"}, {"modelscope", "m-2"}, {"modelscope", "m-3"},
		{"modelscope-ai", "a-1"}, {"modelscope-ai", "a-2"}, {"modelscope-ai", "a-3"},
		{"modelscope-my", "y-1"}, {"modelscope-my", "y-2"}, {"modelscope-my", "y-3"},
	}
	allCands := func(exclude ...string) []Candidate {
		seen := map[string]bool{}
		for _, id := range exclude {
			seen[id] = true
		}
		out := make([]Candidate, 0, len(base))
		for _, e := range base {
			if !seen[e.id] {
				out = append(out, Candidate{ID: e.id, Provider: e.p, Priority: 1})
			}
		}
		return out
	}
	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider:   "modelscope",
			Providers:  []string{"modelscope", "modelscope-ai", "modelscope-my"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}
	usage429 := func(authID, provider string) {
		s.OnUsage(UsageRecord{
			AuthID: authID, Provider: provider, Model: "Qwen-72B", Alias: "Qwen-72B",
			Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody},
		})
	}
	usage200 := func(authID, provider string) {
		s.OnUsage(UsageRecord{
			AuthID: authID, Provider: provider, Model: "Qwen-72B", Alias: "Qwen-72B",
		})
	}
	pick := func(exclude ...string) string {
		resp, err := s.SchedulerPick(mkReq(allCands(exclude...)...))
		if err != nil {
			t.Fatalf("SchedulerPick err=%v", err)
		}
		return resp.AuthID
	}

	// 第一轮
	if id := pick(); id != "m-1" {
		t.Fatalf("r1#1: want m-1, got %s", id)
	}
	usage429("m-1", "modelscope")
	if id := pick("m-1"); id != "a-1" {
		t.Fatalf("r1#2: want a-1, got %s", id)
	}
	usage429("a-1", "modelscope-ai")
	if id := pick("m-1", "a-1"); id != "y-1" {
		t.Fatalf("r1#3: want y-1, got %s", id)
	}
	usage200("y-1", "modelscope-my")

	// 第二轮
	if id := pick(); id != "m-2" {
		t.Fatalf("r2#1: want m-2, got %s", id)
	}
	usage429("m-2", "modelscope")
	if id := pick("m-2"); id != "a-2" {
		t.Fatalf("r2#2: want a-2, got %s", id)
	}
	usage200("a-2", "modelscope-ai")

	// 第三轮
	if id := pick(); id != "m-3" {
		t.Fatalf("r3#1: want m-3, got %s", id)
	}
	usage200("m-3", "modelscope")
}
