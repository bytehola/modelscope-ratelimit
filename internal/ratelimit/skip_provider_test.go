package ratelimit

import (
	"errors"
	"testing"
	"time"
)

// skip_provider_on_429 模式：托管凭据返回 insufficient_quota 的 429 时，把该 provider 加入按
// provider 维护的跳过集合；后续 SchedulerPick 会过滤掉这一家的候选。集合是 latch 的：非托管失
// 败不会让它失效，于是非托管 key 会被连续试完才回到托管。任意 200 清空。返回 429 的那把托管
// key 不会被日禁用（以便日后重新探测）。
func TestSkipProviderOn429JumpsToNonManaged(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider:   "modelscope",
			Providers:  []string{"modelscope", "aliyun"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}

	// 第 1 次挑选：还没有 429 -> 选托管。
	r1, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if err != nil || r1.AuthID != "m-1" {
		t.Fatalf("1st pick: expected managed m-1, got %+v err=%v", r1, err)
	}

	// 托管 429（insufficient_quota）-> 把 "modelscope" 加入跳过集合。
	// 开启 SkipProviderOn429 时，这把 key 不应被日禁用。
	s.OnUsage(UsageRecord{
		AuthID:          "m-1",
		Provider:        "modelscope",
		Model:           "Qwen-72B",
		Alias:           "Qwen-72B",
		Failed:          true,
		Failure:         &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
		ResponseHeaders: hdr("0"),
	})
	if !s.IsProviderSkipped("modelscope") {
		t.Fatal("managed 429 must add provider to skip set")
	}
	if s.isDisabled("m-1", "Qwen-72B", now) {
		t.Fatal("managed 429 key must NOT be daily-disabled in skip_provider_on_429 mode")
	}

	// 第 2 次挑选（重试）：跳过 modelscope -> 选非托管的 a-1。
	// latch 语义：跳过集合不会被这次挑选消耗掉。
	r2, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if err != nil || r2.AuthID != "a-1" {
		t.Fatalf("2nd pick (skip managed): expected non-managed a-1, got %+v err=%v", r2, err)
	}
	if !s.IsProviderSkipped("modelscope") {
		t.Fatal("skip set must persist after the skip pick (latching, not consumed)")
	}

	// 第 3 次挑选：modelscope 仍在跳过集合里 -> 再次跳过托管，选非托管。这正是与“一次性标
	// 记”的关键差别：不会穿插回托管。
	r3, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if err != nil || r3.AuthID != "a-1" {
		t.Fatalf("3rd pick: expected non-managed a-1 (skip set still armed), got %+v err=%v", r3, err)
	}
}

// skip_provider_on_429 只在 insufficient_quota 的 429 上武装自己。其他 429 类型
// （limit_burst_rate、exceeded-today）不会加入跳过集合，而是走常规的日禁用 + 冷却处理。
func TestSkipProviderOn429Any429Body(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	// insufficient_quota 的 429 -> 加入跳过集合。
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.IsProviderSkipped("modelscope") {
		t.Fatal("insufficient_quota 429 must add provider to skip set")
	}

	// 非 insufficient_quota 的 429（limit_burst_rate）-> 绝不能加入跳过集合。
	s.ClearAllSkippedProviders() // 为下一次检查复位
	s.OnUsage(UsageRecord{
		AuthID:          "m-2",
		Provider:        "modelscope",
		Model:           "Qwen-72B",
		Alias:           "Qwen-72B",
		Failed:          true,
		Failure:         &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"limit_burst_rate"}}`},
		ResponseHeaders: hdr("0"),
	})
	if s.HasSkippedProviders() {
		t.Fatal("limit_burst_rate 429 must NOT add to skip set (only insufficient_quota does)")
	}
	// limit_burst_rate 的 429 不应触发日禁用（只有 429 + "insufficient balance" 才会因为魔粒
	// 耗尽而日禁用）。
	if s.isDisabled("m-2", "Qwen-72B", now) {
		t.Fatal("limit_burst_rate 429 must not daily-disable")
	}
}

// latch 语义：非托管失败不会清空跳过集合，因此非托管 key 会被连续试完才回到托管。
func TestSkipProviderLatchingNonManagedFailures(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider:   "modelscope",
			Providers:  []string{"modelscope", "aliyun"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}

	// 第 1 次挑选：托管。
	r1, _ := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "b-1", Provider: "aliyun", Priority: 1},
		Candidate{ID: "b-2", Provider: "aliyun", Priority: 1},
	))
	if r1.AuthID != "m-1" {
		t.Fatalf("1st pick: expected m-1, got %s", r1.AuthID)
	}

	// 托管 429 -> 加入跳过集合。
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})

	// b-1 已试过（模拟宿主重试）：候选收缩为 m-2、b-2。
	// 跳过集合仍在 -> 继续跳过 modelscope -> 选 b-2。
	r2, _ := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "b-2", Provider: "aliyun", Priority: 1},
	))
	if r2.AuthID != "b-2" {
		t.Fatalf("2nd pick: expected non-managed b-2 (latching skip), got %s", r2.AuthID)
	}
	if !s.IsProviderSkipped("modelscope") {
		t.Fatal("skip set must persist after non-managed pick (latching)")
	}

	// 非托管已全部耗尽：只剩托管 -> 让位给宿主。
	r3, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
	))
	if err != nil || r3.Handled {
		t.Fatalf("3rd pick: expected defer (Handled=false) when only managed left, got %+v err=%v", r3, err)
	}
}

// 任意 200（托管或非托管）都会清空跳过集合，让下一次请求重新探测免费的托管额度。
func TestSkipProviderAny200ClearsFlag(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	// 用一次托管 429 武装跳过集合。
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.HasSkippedProviders() {
		t.Fatal("skip set must be armed after managed 429")
	}

	// 非托管 200 -> 清空跳过集合（优先用免费额度：下一次请求先试托管）。
	s.OnUsage(UsageRecord{
		AuthID:   "a-1",
		Provider: "aliyun",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   false,
	})
	if s.HasSkippedProviders() {
		t.Fatal("any 200 must clear the skip set (non-managed 200 included)")
	}

	// 重新武装，随后托管 200 同样会清空。
	s.SetSkipProvider("modelscope")
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   false,
	})
	if s.HasSkippedProviders() {
		t.Fatal("managed 200 must clear the skip set")
	}
}

// 非托管失败不碰跳过集合（已武装时保持原样）。
func TestSkipProviderNonManagedFailureDoesNotClear(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	// 武装跳过集合。
	s.SetSkipProvider("modelscope")

	// 非托管失败（429）-> 跳过集合必须保持武装。
	s.OnUsage(UsageRecord{
		AuthID:   "a-1",
		Provider: "aliyun",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.IsProviderSkipped("modelscope") {
		t.Fatal("non-managed failure must NOT clear the skip set")
	}
}

// 当所有托管候选都已试过（SchedulerPick 里 managed==0）时清空跳过集合，让下一次请求重新开始、
// 重新探测免费的托管额度。这样可以避免在托管和非托管全部失败时，集合被永久卡住。
func TestSkipProviderFlagClearedWhenAllManagedTried(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider:   "modelscope",
			Providers:  []string{"modelscope", "aliyun"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}

	// 第 1 次挑选：托管 m-1。
	r1, _ := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "b-1", Provider: "aliyun", Priority: 1},
	))
	if r1.AuthID != "m-1" {
		t.Fatalf("1st pick: expected m-1, got %s", r1.AuthID)
	}

	// 托管 429 -> 武装跳过集合。
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.HasSkippedProviders() {
		t.Fatal("skip set must be armed after managed 429")
	}

	// 第 2 次挑选：m-1 已试过 -> 只剩非托管 -> managed==0 -> 清空跳过集合并让位给宿主。
	r2, err := s.SchedulerPick(mkReq(
		Candidate{ID: "b-1", Provider: "aliyun", Priority: 1},
	))
	if err != nil || r2.Handled {
		t.Fatalf("2nd pick: expected defer (Handled=false), got %+v err=%v", r2, err)
	}
	if s.HasSkippedProviders() {
		t.Fatal("skip set must be cleared when all managed candidates are tried (managed==0)")
	}
}

// 当跳过集合已武装、而该 route 又没有任何非托管 provider 时，插件快速失败（返回
// ErrAllCredentialsDisabled），而不是继续去试剩下的托管 key（它们会在同一个资源池上全部 429）。
// 同时清空跳过集合，让下一次请求重新探测托管。
func TestSkipProviderNoNonManagedFailsFast(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider: "modelscope",
			// 该 route 上没有非托管 provider。
			Providers:  []string{"modelscope"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}

	// 第 1 次挑选：跳过集合为空 -> 正常挑托管。
	r1, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-3", Provider: "modelscope", Priority: 1},
	))
	if err != nil || r1.AuthID != "m-1" {
		t.Fatalf("1st pick: expected m-1, got %+v err=%v", r1, err)
	}

	// 托管 429 -> 武装跳过集合。
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.HasSkippedProviders() {
		t.Fatal("skip set must be armed after managed 429")
	}

	// 第 2 次挑选：跳过集合已武装且无非托管 -> 快速失败并清空。
	_, err = s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-3", Provider: "modelscope", Priority: 1},
	))
	if err == nil {
		t.Fatal("2nd pick: expected error (fail fast, no non-managed fallback)")
	}
	if !errors.Is(err, ErrAllCredentialsDisabled) {
		t.Fatalf("2nd pick: expected ErrAllCredentialsDisabled, got %v", err)
	}
	if s.HasSkippedProviders() {
		t.Fatal("skip set must be cleared after fail-fast error so next request probes managed")
	}

	// 第 3 次挑选：跳过集合已清空 -> 重新正常挑托管（探测是否恢复）。
	// 轮询游标已经越过 m-1，所以这次拿到的是 m-2。
	r3, err := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
	))
	if err != nil || r3.AuthID != "m-2" {
		t.Fatalf("3rd pick: expected m-2 (skip set cleared, probe managed), got %+v err=%v", r3, err)
	}
}

// 粒度是 provider：modelscope 返回 429 时，同样托管的 modelscope-my 不会被跳过，它的候选仍然可
// 用。
func TestSkipProviderPerProviderGranularity(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope", "modelscope-my")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider:   "modelscope",
			Providers:  []string{"modelscope", "modelscope-my", "aliyun"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}

	// 第 1 次挑选：没有跳过记录 -> 选 modelscope（配置里的第一家）。
	r1, _ := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "my-1", Provider: "modelscope-my", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if r1.AuthID != "m-1" {
		t.Fatalf("1st pick: expected m-1, got %s", r1.AuthID)
	}

	// modelscope 429 -> 只把 "modelscope" 加入跳过集合（不含 "modelscope-my"）。
	s.OnUsage(UsageRecord{
		AuthID:   "m-1",
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})
	if !s.IsProviderSkipped("modelscope") {
		t.Fatal("modelscope must be in skip set")
	}
	if s.IsProviderSkipped("modelscope-my") {
		t.Fatal("modelscope-my must NOT be in skip set (per-provider granularity)")
	}

	// 第 2 次挑选：跳过 modelscope，而 modelscope-my 未被跳过 -> 选 my-1。
	r2, _ := s.SchedulerPick(mkReq(
		Candidate{ID: "m-2", Provider: "modelscope", Priority: 1},
		Candidate{ID: "my-1", Provider: "modelscope-my", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if r2.AuthID != "my-1" {
		t.Fatalf("2nd pick: expected modelscope-my my-1 (modelscope skipped, my not), got %s", r2.AuthID)
	}

	// modelscope-my 返回 200 -> 清空所有跳过条目 -> modelscope 重新可用。
	s.OnUsage(UsageRecord{
		AuthID:   "my-1",
		Provider: "modelscope-my",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   false,
	})
	if s.HasSkippedProviders() {
		t.Fatal("any 200 must clear all skip entries")
	}
}

// 两家托管 provider 都被跳过、同时存在非托管候选时，插件落到非托管上。
func TestSkipProviderAllManagedSkippedFallsToNonManaged(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope", "modelscope-my")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)

	// 两家 provider 都在跳过集合里。
	s.SetSkipProvider("modelscope")
	s.SetSkipProvider("modelscope-my")

	mkReq := func(cands ...Candidate) SchedulerPickRequest {
		return SchedulerPickRequest{
			Provider:   "modelscope",
			Providers:  []string{"modelscope", "modelscope-my", "aliyun"},
			Model:      "Qwen-72B",
			Candidates: cands,
		}
	}

	// 托管全被跳过 -> availableCount=0 -> 选非托管。
	r, _ := s.SchedulerPick(mkReq(
		Candidate{ID: "m-1", Provider: "modelscope", Priority: 1},
		Candidate{ID: "my-1", Provider: "modelscope-my", Priority: 1},
		Candidate{ID: "a-1", Provider: "aliyun", Priority: 1},
	))
	if r.AuthID != "a-1" {
		t.Fatalf("expected non-managed a-1 (all managed skipped), got %s", r.AuthID)
	}
}
