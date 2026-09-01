package ratelimit

import (
	"testing"
	"time"
)

// insufficientQuotaBody 是一个带 "insufficient_quota" 的 429 body，便于 isInsufficientQuota 识别。
const insufficientQuotaBody = `{"error":{"code":"insufficient_quota"}}`

// TestProviderKeyRotationNoSkip 验证 skip_provider_on_429 关闭时的轮换顺序：先把排在前面那家
// 的 key 逐个试完，才轮到下一家。
//
// 配置顺序：modelscope、modelscope-ai、modelscope-my，每家 4 把 key。
//
// 第一个请求：
//  1. modelscope 第 1 把 -> 429
//  2. modelscope 第 2 把 -> 429
//  3. modelscope 第 3 把 -> 429
//  4. modelscope 第 4 把 -> 200
//
// 第二个请求（任意 200 之后重新从第一家开始，该家的 key 游标继续往后走）：
//  1. modelscope 第 1 把 -> 429
//  2. modelscope 第 2 把 -> 429
//  3. modelscope 第 3 把 -> 429
//  4. modelscope 第 4 把 -> 429
//  5. modelscope-ai 第 1 把 -> 200（modelscope 已无可用 key，才换到第二家）
func TestProviderKeyRotationNoSkip(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, loc)
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope", "modelscope-ai", "modelscope-my")
	cfg.InsufficientQuotaCooldown = 0 // 不产生阻塞睡眠，只观察轮换
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
		exclude   []string // 到目前为止宿主已排除的候选 ID
		failed    bool
		fprovider string
	}
	steps := []step{
		// skip OFF：先把 m 家 4 把 key 试完，再轮到 a 家、y 家
		{wantID: "m-1", exclude: nil, failed: true, fprovider: "modelscope"},
		{wantID: "m-2", exclude: []string{"m-1"}, failed: true, fprovider: "modelscope"},
		{wantID: "m-3", exclude: []string{"m-1", "m-2"}, failed: true, fprovider: "modelscope"},
		{wantID: "m-4", exclude: []string{"m-1", "m-2", "m-3"}, failed: false, fprovider: "modelscope"},
		// 200 复位；下一个请求重新从 m 家开始（key 游标继续往后走）
		{wantID: "m-1", exclude: nil, failed: true, fprovider: "modelscope"},
		{wantID: "m-2", exclude: []string{"m-1"}, failed: true, fprovider: "modelscope"},
		{wantID: "m-3", exclude: []string{"m-1", "m-2"}, failed: true, fprovider: "modelscope"},
		{wantID: "m-4", exclude: []string{"m-1", "m-2", "m-3"}, failed: true, fprovider: "modelscope"},
		// m 家已无可用 key -> 换到 a 家
		{wantID: "a-1", exclude: []string{"m-1", "m-2", "m-3", "m-4"}, failed: false, fprovider: "modelscope-ai"},
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

// TestProviderRotationResetsOnSuccess 验证任意 200 都会把跨 provider 游标复位，于是下一次请求
// 总是从第一个托管 provider 重新开始；跨请求继续轮转的只有各家的 key 游标。
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
	// 请求 1 的第一次挑选拿到 ms#1，随后它返回 429。
	s.OnUsage(UsageRecord{
		AuthID: "m-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody},
	})

	r2, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "modelscope-ai", Priority: 1},
		Candidate{ID: "a-2", Provider: "modelscope-ai", Priority: 1},
	))
	if err != nil || r2.AuthID != "m-2" {
		t.Fatalf("2nd pick: want m-2 (exhaust m keys first), got %+v err=%v", r2, err)
	}

	// 某次 200 -> 游标复位；下一次请求重新从 ms 家开始（而不是留在 ai 家）。
	s.OnUsage(UsageRecord{AuthID: "a-1", Provider: "modelscope-ai", Model: "Qwen-72B", Alias: "Qwen-72B"})
	// 请求 2：游标已复位 -> 重新从 ms 家开始挑（ms 的 key 游标此时正好回绕）。
	r3, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "modelscope-ai", Priority: 1},
	))
	if err != nil || r3.AuthID != "m-1" {
		t.Fatalf("3rd pick after 200: want m-1 (key cursor wrapped), got %+v err=%v", r3, err)
	}
}

// TestProviderKeyRotationResetsOnSuccess_UserExample 复现文档里描述的每次请求的轮换形态：请求
// 从第一个 provider 开始，429 时先把本家剩下的 key 逐个试完，本家没有可用 key 才换下一家；任意
// 200 之后，下一次请求重新从第一个 provider 开始。跨请求继续轮转的只有各家的 key 游标。
//
//	第一轮：m#1 429 -> m#2 429 -> m#3 429（m 家已无可用 key）-> ai#1 429 -> ai#2 200
//	第二轮：m#1 429 -> m#2 429 -> m#3 429 -> ai#3 200（ai 家的 key 游标继续往后）
//	第三轮：m#1 200
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

	// 第一轮：先耗尽 modelscope 的 key
	if id := pick(); id != "m-1" {
		t.Fatalf("r1#1: want m-1, got %s", id)
	}
	usage429("m-1", "modelscope")
	if id := pick("m-1"); id != "m-2" {
		t.Fatalf("r1#2: want m-2, got %s", id)
	}
	usage429("m-2", "modelscope")
	if id := pick("m-1", "m-2"); id != "m-3" {
		t.Fatalf("r1#3: want m-3, got %s", id)
	}
	usage429("m-3", "modelscope")
	if id := pick("m-1", "m-2", "m-3"); id != "a-1" {
		t.Fatalf("r1#4: want a-1 (m exhausted), got %s", id)
	}
	usage429("a-1", "modelscope-ai")
	if id := pick("m-1", "m-2", "m-3", "a-1"); id != "a-2" {
		t.Fatalf("r1#5: want a-2, got %s", id)
	}
	usage200("a-2", "modelscope-ai")

	// 第二轮：200 之后重新从 modelscope 开始
	if id := pick(); id != "m-1" {
		t.Fatalf("r2#1: want m-1, got %s", id)
	}
	usage429("m-1", "modelscope")
	if id := pick("m-1"); id != "m-2" {
		t.Fatalf("r2#2: want m-2, got %s", id)
	}
	usage429("m-2", "modelscope")
	if id := pick("m-1", "m-2"); id != "m-3" {
		t.Fatalf("r2#3: want m-3, got %s", id)
	}
	usage429("m-3", "modelscope")
	if id := pick("m-1", "m-2", "m-3"); id != "a-3" {
		t.Fatalf("r2#4: want a-3 (m exhausted, ai cursor advanced from a-1), got %s", id)
	}
	usage200("a-2", "modelscope-ai")

	// 第三轮：再次从第一家开始
	if id := pick(); id != "m-1" {
		t.Fatalf("r3#1: want m-1, got %s", id)
	}
	usage200("m-1", "modelscope")
}
