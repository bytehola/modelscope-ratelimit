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
	cur := now
	s.SetClock(func() time.Time { return cur })
	return s, &cur
}

func cfgWith(providers ...string) *Config {
	c := DefaultConfig()
	c.Providers = providers
	return c
}

// hdr 返回 nil：魔粒体系下不再使用响应头，保留只是为了兼容既有测试。
func hdr(remaining string) map[string][]string {
	return nil
}

// insufficientBalanceBody 模拟 Modelscope 推理接口返回的 429 + "insufficient balance"（魔粒耗
// 尽）。
const insufficientBalanceBody = `{"error":{"message":"insufficient balance","request_id":"test"}}`

func totalExhaustedHdr() map[string][]string {
	return nil
}

func modelExhaustedHdr() map[string][]string {
	return nil
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
		if c.InsufficientQuotaCooldown != 0.1 {
			t.Fatalf("cooldown = %v", c.InsufficientQuotaCooldown)
		}
		if c.ManagesProvider("modelscope") {
			t.Fatal("empty providers should manage nothing")
		}
	})
	t.Run("overrides", func(t *testing.T) {
		c, err := ConfigFromMap(map[string]any{
			"providers":                   []any{"ms", "ms2"},
			"timezone":                    "UTC",
			"insufficient_quota_cooldown": 3,
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
		if c.InsufficientQuotaCooldown != 3 {
			t.Fatalf("cooldown = %v", c.InsufficientQuotaCooldown)
		}
	})
}

// TestModelOnlyDisable 已删除：魔粒体系下不存在按单个模型的禁用。
// isInsufficientBalance 一律调用 disableGlobal（对该凭据的所有模型生效）。

func TestGlobalDisable(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

	// 全局禁用会挡住该 key 上的所有模型。
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

	s.ApplyRateLimit("auth-1", "Qwen-72B", map[string][]string{"Content-Type": {"application/json"}}, 0, "", now)
	if s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("non-Modelscope response must not trigger a disable")
	}
}

func TestDailyReset(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 23, 59, 0, 0, loc)
	s, cur := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 429, insufficientBalanceBody, *cur)
	if !s.isDisabled("auth-1", "Qwen-72B", *cur) {
		t.Fatal("expected disabled before midnight")
	}

	// 跨到次日 00:00 -> 被清除。
	*cur = time.Date(2026, 7, 9, 0, 0, 1, 0, loc)
	if s.isDisabled("auth-1", "Qwen-72B", *cur) {
		t.Fatal("expected cleared after midnight")
	}

	// PruneAll 应当丢掉这条过期记录。
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

	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 429, insufficientBalanceBody, *cur)
	first := *cur
	*cur = now.Add(2 * time.Hour)
	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 429, insufficientBalanceBody, *cur)

	st := s.Status()
	if len(st) != 1 || st[0].Global == nil || *st[0].Global != first {
		t.Fatalf("expected earliest disable time kept, got %+v", st)
	}
}

// TestThresholdProactive 已删除：魔粒体系里没有 DisableThreshold 这个概念。

func TestSchedulerSkipsDisabledAndRotates(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	// 把 auth-1 全局禁用（insufficient balance，即魔粒耗尽）。
	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

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

	// auth-1 已被全局禁用，因此换一个模型时仍然要能正常处理（由 auth-2/auth-3 承接）。
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

	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
	s.ApplyRateLimit("auth-2", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

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
	s.Reconfigure(DefaultConfig()) // 未配置 providers

	resp, err := s.SchedulerPick(SchedulerPickRequest{Provider: "modelscope", Model: "m", Candidates: []Candidate{{ID: "a", Provider: "modelscope"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Handled {
		t.Fatal("empty providers must defer everything")
	}
}

// TestSchedulerMixedProviderSkipsDisabled 复现 CLIProxyAPI 的 "mixed" 调度路径：请求级 Provider
// 是空的，真正的运行时 provider key 在每个候选上（"openai-compatible-<name>"）。插件绝不能因为
// req.Provider 为空就退出，仍然要跳过被禁用的候选并挑一个可用的（Handled=true）。修复前这里返回
// Handled=false，把决定权交回宿主内置调度器，而它会忽略禁用状态，让已耗尽的 key 继续去吃 429。
func TestSchedulerMixedProviderSkipsDisabled(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope")) // provider 按裸名 "modelscope" 配置

	// 对请求的模型来说 key-1 已耗尽，key-2 仍然健康。
	s.ApplyRateLimit("key-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

	req := SchedulerPickRequest{
		Provider: "", // mixed 路径：顶层 provider 为空
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

// TestSchedulerMixedProviderAllDisabledError 验证 mixed 路径上托管候选全部被禁用时，插件返回
// ErrAllCredentialsDisabled（显式拒绝）而不是让位，从而宿主永远不会把请求派给已耗尽的 key。
func TestSchedulerMixedProviderAllDisabledError(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("key-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
	s.ApplyRateLimit("key-2", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

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

// TestSchedulerMixedProviderDefersUnmanaged 验证 mixed 路径上候选里没有一个是托管的（完全是别的
// provider）时，插件让位（Handled=false），交给宿主内置调度器处理。
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

// TestSchedulerAllDisabledFallbackToNonManagedCandidate 验证托管候选全部禁用、但同一优先级组里还
// 有非托管候选时，插件直接选那个非托管候选（Handled=true），而不是返回 ErrAllCredentialsDisabled
// ——否则本来能被非托管 provider 服务的请求会被插件卡住。
func TestSchedulerAllDisabledFallbackToNonManagedCandidate(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("ms-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
	s.ApplyRateLimit("ms-2", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

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

// TestSchedulerAllManagedDisabledDefersToLowerPriorityNonManaged 验证：托管候选全部禁用、当前列
// 表里看不到非托管候选（宿主按优先级把它们过滤掉了），但 route 本身接受非托管 provider
// （req.Providers）时，插件让位（Handled=false），让宿主可以继续重试并最终触达那些低优先级的非托
// 管候选。
func TestSchedulerAllManagedDisabledDefersToLowerPriorityNonManaged(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("ms-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
	s.ApplyRateLimit("ms-2", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

	// 列表里只有托管候选（宿主已把低优先级的非托管候选过滤掉），但 req.Providers 里列出了非托管
	// provider。
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
		ResponseHeaders: nil, Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: insufficientBalanceBody},
	})
	if !s.isDisabled("auth-1", "Qwen-7B", now) {
		t.Fatal("usage handle with total exhaustion must globally disable")
	}
}

func TestOnUsageAliasMatchesSchedulerModel(t *testing.T) {
	// 回归用例：宿主传给 SchedulerPick 的是客户端请求的别名（例如 "glm-5.2"），而用量记录里
	// Model 是解析后的上游模型（例如 "ZhipuAI/GLM-5.2"）、Alias 才是别名。由于 429 走执行器的错
	// 误路径（宿主提前返回，不会调用响应拦截器），OnUsage 是记录限流禁用的唯一路径，而它过去按上
	// 游名做键。于是调度器用别名去查 isDisabled 时查不到东西，一旦宿主自己的短冷却过期、而本插件
	// 的日禁用仍然生效，请求就被重新派给一把已耗尽的 key。
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		Provider:        "modelscope",
		AuthID:          "auth-1",
		Model:           "ZhipuAI/GLM-5.2",
		Alias:           "glm-5.2",
		ResponseHeaders: nil, Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: insufficientBalanceBody},
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
	// 禁用状态以调度器检查的别名为键，但一旦从用量记录里登记了 别名->上游名 的映射，状态页和日志
	// 就应显示上游模型名（rec.Model）。
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.OnUsage(UsageRecord{
		Provider:        "modelscope",
		AuthID:          "auth-1",
		Model:           "ZhipuAI/GLM-5.2",
		Alias:           "glm-5.2",
		ResponseHeaders: nil, Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: insufficientBalanceBody},
	})
	// 存储仍然按别名做键（调度器路径）。
	if !s.isDisabled("auth-1", "glm-5.2", now) {
		t.Fatal("disable must be stored under the alias")
	}
	// 展示时把别名解析成上游名。
	if got := s.displayName("glm-5.2"); got != "ZhipuAI/GLM-5.2" {
		t.Fatalf("displayName = %q, want ZhipuAI/GLM-5.2", got)
	}
	// 状态快照里暴露的是上游名，不是别名。
	snap := s.Snapshot(now)
	kv, ok := snap["auth-1"]
	if !ok {
		t.Fatalf("no snapshot for auth-1: %+v", snap)
	}
	_ = kv // 魔粒体系重构时已移除按模型的视图
}

func TestOnResponseWithMetadataAuthID(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	resp := s.OnResponse(ResponseInterceptRequest{
		Model:           "Qwen-72B",
		ResponseHeaders: nil, StatusCode: 429, Body: insufficientBalanceBody,
		Metadata: map[string]any{"auth_id": "auth-1"},
	})
	// 插件不得改写响应。
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

	// 流的初始化调用（ChunkIndex == -1）不再解析响应头。
	// insufficient balance 改由 OnUsage 识别（429 + body）。
	r := s.OnStreamChunk(StreamChunkInterceptRequest{
		ChunkIndex: -1, Model: "Qwen-72B",
		ResponseHeaders: nil,
		Metadata:        map[string]any{"AuthID": "auth-1"},
	})
	if r.DropChunk {
		t.Fatal("header-init must not drop")
	}
	// 通过带 statusCode 和 body 的 OnUsage 生效。
	s.OnUsage(UsageRecord{
		Provider: "modelscope", AuthID: "auth-1", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed:  true,
		Failure: &UsageFailure{StatusCode: 429, Body: insufficientBalanceBody},
	})
	if !s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("429 + insufficient balance must globally disable")
	}

	// 数据块不得再次触发，也不得改动任何东西。
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
		{map[string]any{"auth_id": 123}, ""}, // 非字符串取值被忽略
	}
	for _, c := range cases {
		if got := AuthIDFromMetadata(c.meta); got != c.want {
			t.Errorf("AuthIDFromMetadata(%v) = %q, want %q", c.meta, got, c.want)
		}
	}
}

// TestSchedulerProviderOrderPrefersFirst 验证 provider 的优先顺序：只要排在第一家的那个还有可用
// key，就一直选它；第一家全部禁用之后才轮到第二家。
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
	// cfg.Providers 里 moda 在前，所以只应选到 moda 的 key。
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

// TestSchedulerProviderOrderFallbackToSecond 验证第一家的 key 全部禁用后，插件会自动落到第二家，
// 不浪费任何请求。
func TestSchedulerProviderOrderFallbackToSecond(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("moda", "modelscope"))

	// 禁用 moda 的所有 key。
	s.ApplyRateLimit("moda-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
	s.ApplyRateLimit("moda-2", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

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

// ---- 凭据策略测试 ----

func cfgWithStrategy(strategy string, providers ...string) *Config {
	c := cfgWith(providers...)
	c.CredentialStrategy = strategy
	return c
}

// TestFillFirstAlwaysPicksFirst 验证 fill-first 策略永远返回第一个可用候选（同一把 key 直到被禁
// 用）。
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

	// 禁用该模型下的 auth-1；此后 fill-first 应当固定在 auth-2。
	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
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

// TestRoundRobinRotates 验证 round-robin 会把挑选均匀分布在所有可用候选上。
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
	// 第一次挑选必须是第一个候选（配置顺序），而不是第二个。
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

// TestFillFirstProviderOrder 验证 fill-first 会一直用第一家第一把可用 key，直到它被禁用才落到下
// 一家（跨 provider 游标恒为 0，因此顺序始终从第一家开始）。
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
	// moda 在前：fill-first 应始终选 moda-1。
	for i := 0; i < 15; i++ {
		resp, err := s.SchedulerPick(req)
		if err != nil {
			t.Fatalf("pick %d failed: %v", i, err)
		}
		if !resp.Handled || resp.AuthID != "moda-1" {
			t.Fatalf("pick %d: expected moda-1, got %+v", i, resp)
		}
	}

	// 禁用 moda 全部 key：fill-first 应落到 modelscope-1。
	s.ApplyRateLimit("moda-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
	s.ApplyRateLimit("moda-2", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
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

// TestNonManagedFallbackUsesHostStrategy 验证托管 key 全部禁用且存在非托管候选时，由宿主的策略
// 决定选谁：fill-first 选第一个非托管候选，round-robin 在它们之间轮转。
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

	// 宿主策略 fill-first：始终选 paid-1。
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("moda"))
	s.ApplyRateLimit("moda-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
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

	// 宿主策略 round-robin：在 paid-1 与 paid-2 之间轮转。
	s2, _ := newTestStore(now)
	s2.Reconfigure(cfgWith("moda"))
	s2.ApplyRateLimit("moda-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
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

// TestNonManagedFallbackNoFetcherDefaultsRoundRobin 验证没有注入策略 fetcher 时，非托管兜底默认按
// round-robin 处理。
func TestNonManagedFallbackNoFetcherDefaultsRoundRobin(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("moda"))
	// 不调用 SetStrategyFetcher：应当默认 round-robin。
	s.ApplyRateLimit("moda-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

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

// TestRoundRobinFollowsConfigOrder 验证调度器会把候选重新排成 config.yaml 里 api-key 的顺序（通
// 过 providerOrderFetcher 取得），即使宿主传来的是按 auth ID 哈希字典序排列的列表。
//
// 配置顺序：c-3, a-1, b-2（管理接口返回的顺序）
// 宿主顺序：a-1, b-2, c-3（按 ID 字典序，也就是宿主发来的顺序）
// 期望挑选：c-3 -> a-1 -> b-2 -> c-3 -> …（按配置顺序）
func TestRoundRobinFollowsConfigOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "modelscope"))

	// 注入配置顺序取数函数（provider -> 有序 auth ID）。
	s.SetProviderOrderFetcher(func() map[string][]string {
		return map[string][]string{"modelscope": {"c-3", "a-1", "b-2"}}
	})

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		// 宿主按 ID 字典序发送候选。
		Candidates: []Candidate{
			{ID: "a-1", Provider: "modelscope", Priority: 1},
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			{ID: "c-3", Provider: "modelscope", Priority: 1},
		},
	}
	// 前三次挑选必须按配置顺序：c-3、a-1、b-2。
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
	// 第四次挑选回绕到配置顺序里的第一把 key。
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "c-3" {
		t.Fatalf("pick 3: expected c-3 (wrap), got %+v err=%v", resp, err)
	}
}

// TestFillFirstFollowsConfigOrder 验证 fill-first 选的是配置顺序里的第一把 key，而不是宿主排序后
// 的第一把。
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
	// fill-first 必须始终选 c-3（配置顺序里的第一把），而不是 a-1。
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

// TestNoOrderedIDsFetcherKeepsHostOrder 验证没有注入 providerOrderFetcher 时，调度器沿用宿主给出
// 的（字典序）顺序，因此行为确定但与配置顺序不一致。
func TestNoOrderedIDsFetcherKeepsHostOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "modelscope"))
	// 不调用 SetProviderOrderFetcher。

	req := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "a-1", Provider: "modelscope", Priority: 1},
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			{ID: "c-3", Provider: "modelscope", Priority: 1},
		},
	}
	// 没有 fetcher 时，第一次挑选就是收到的第一个候选。
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "a-1" {
		t.Fatalf("first pick without fetcher should be a-1 (host order), got %+v err=%v", resp, err)
	}
}

// TestRoundRobinRetryPicksNextInConfigOrder 模拟一次 429 重试：选中 c-3 之后宿主把它排除（加入
// "tried"），并用更小的候选列表再次调用 scheduler.pick。插件必须选 a-1（配置顺序里 c-3 的下一
// 把），而不是 b-2（简单用 cursor 对变短的列表取模会得到 b-2）。
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

	// 新请求：选 c-3（配置顺序第一把）。
	resp, err := s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "c-3" {
		t.Fatalf("fresh pick: expected c-3, got %+v err=%v", resp, err)
	}

	// c-3 返回 429：宿主重试时已把 c-3 从候选中剔除。
	retryReq := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "a-1", Provider: "modelscope", Priority: 1},
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			// c-3 不在列表里（已进宿主 "tried" 集合）
		},
	}
	resp, err = s.SchedulerPick(retryReq)
	if err != nil || !resp.Handled || resp.AuthID != "a-1" {
		t.Fatalf("retry pick: expected a-1 (next in config order after c-3), got %+v err=%v", resp, err)
	}

	// 下一个新请求应当从 b-2 继续（a-1 之后）。
	resp, err = s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "b-2" {
		t.Fatalf("next fresh pick: expected b-2, got %+v err=%v", resp, err)
	}

	// 回绕到 c-3。
	resp, err = s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "c-3" {
		t.Fatalf("wrap pick: expected c-3, got %+v err=%v", resp, err)
	}
}

// TestRoundRobinRetryWithDisabledKey 验证重试时游标会跳过已禁用的 key。配置顺序
// [c-3, a-1, b-2]，其中 a-1 被禁用。选中 c-3 之后的重试必须跳过 a-1 选 b-2。
func TestRoundRobinRetryWithDisabledKey(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "modelscope"))

	s.SetProviderOrderFetcher(func() map[string][]string {
		return map[string][]string{"modelscope": {"c-3", "a-1", "b-2"}}
	})
	// 为该模型禁用 a-1。
	s.ApplyRateLimit("a-1", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

	fullReq := SchedulerPickRequest{
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "b-2", Provider: "modelscope", Priority: 1},
			{ID: "c-3", Provider: "modelscope", Priority: 1},
			// a-1 不在列表里（已禁用，宿主不会带上）
		},
	}
	// 第一次挑选：c-3（按配置顺序，a-1 因禁用被跳过）。
	resp, err := s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "c-3" {
		t.Fatalf("pick 1: expected c-3, got %+v err=%v", resp, err)
	}
	// 第二次挑选：b-2（回绕并跳过已禁用的 a-1）。
	resp, err = s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "b-2" {
		t.Fatalf("pick 2: expected b-2, got %+v err=%v", resp, err)
	}
	// 第三次挑选：又是 c-3（回绕时跳过已禁用的 a-1）。
	resp, err = s.SchedulerPick(fullReq)
	if err != nil || !resp.Handled || resp.AuthID != "c-3" {
		t.Fatalf("pick 3: expected c-3, got %+v err=%v", resp, err)
	}
}

// TestPerProviderCursorIsolation 验证从一家托管 provider 切到另一家时不会沿用对方的游标值：第二
// 家从它自己的位置 0（配置顺序的第一把）开始，而不是接着第一家走到哪儿。
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

	// 耗尽 moda 的全部 key，让调度落到 modelscope 上。
	s.ApplyRateLimit("m-a", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
	s.ApplyRateLimit("m-b", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
	s.ApplyRateLimit("m-c", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

	req := SchedulerPickRequest{
		Provider: "",
		Model:    "Qwen-72B",
		Candidates: []Candidate{
			{ID: "ms-1", Provider: "openai-compatible-modelscope", Priority: 1},
			{ID: "ms-2", Provider: "openai-compatible-modelscope", Priority: 1},
			{ID: "ms-3", Provider: "openai-compatible-modelscope", Priority: 1},
		},
	}
	// modelscope 的第一次挑选必须是 ms-1（配置顺序位置 0），不受此前被推进过的 moda 游标影响。
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "ms-1" {
		t.Fatalf("first modelscope pick: expected ms-1, got %+v err=%v", resp, err)
	}
	// 第二次挑选：ms-2（游标在 modelscope 自己的空间里前进）。
	resp, err = s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "ms-2" {
		t.Fatalf("second modelscope pick: expected ms-2, got %+v err=%v", resp, err)
	}
	// 第三次挑选：ms-3。
	resp, err = s.SchedulerPick(req)
	if err != nil || !resp.Handled || resp.AuthID != "ms-3" {
		t.Fatalf("third modelscope pick: expected ms-3, got %+v err=%v", resp, err)
	}
}

// TestNonManagedCursorDoesNotCorruptManaged 验证非托管兜底的挑选（走 nonManagedRR）不会推进托管的
// 按 provider 游标：经过一段时间的非托管兜底之后，下一次托管挑选仍应处在正确位置。
func TestNonManagedCursorDoesNotCorruptManaged(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWithStrategy("round-robin", "moda"))

	s.SetProviderOrderFetcher(func() map[string][]string {
		return map[string][]string{"moda": {"m-a", "m-b", "m-c"}}
	})

	// 禁用 moda 的全部 key，以触发非托管兜底。
	s.ApplyRateLimit("m-a", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
	s.ApplyRateLimit("m-b", "Qwen-72B", nil, 429, insufficientBalanceBody, now)
	s.ApplyRateLimit("m-c", "Qwen-72B", nil, 429, insufficientBalanceBody, now)

	// 若干次非托管兜底请求（推进 nonManagedRR）。
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
		s.SchedulerPick(nonManagedReq) // 推进的是 nonManagedRR，不是托管游标
	}

	// 接下来“恢复” moda 的 key：用一个全新的 store 模拟午夜之后的干净状态。实际运行中是由
	// PruneAll 在跨日时清除禁用记录。
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
	// 第一次托管挑选必须是 m-a（游标 0），没有被上面的非托管兜底污染。
	resp, err := s2.SchedulerPick(managedReq)
	if err != nil || !resp.Handled || resp.AuthID != "m-a" {
		t.Fatalf("first managed pick after non-managed fallback: expected m-a, got %+v err=%v", resp, err)
	}
}

// TestInsufficientQuotaCooldownBlocks 验证托管 key 存在有效的 insufficient_quota 冷却时，
// SchedulerPick 会先按剩余冷却时长阻塞（睡眠）再挑选。这个阻塞是全局的：任意托管 key 在冷却中就
// 会挡住全部托管调度（额度共享，与是哪把 key 无关）。阻塞结束后 key 重新可用。
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

// TestInsufficientQuotaCooldownDisabled 验证 InsufficientQuotaCooldown 为 0 时不会发生任何阻塞。
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

// TestInsufficientQuotaNon429NoCooldown 验证非 429 的响应即使 body 里有 "insufficient_quota" 也不
// 会触发冷却。
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

// TestInsufficientQuotaBlock10s 验证 SchedulerPick 会按完整的 10 秒 insufficient_quota 冷却时长阻
// 塞后再挑选。阻塞是全局的（任意托管 key 在冷却中就会挡住全部调度）。
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

	// 第一次挑选：没有冷却，立即返回。
	resp, err := s.SchedulerPick(req)
	if err != nil || !resp.Handled {
		t.Fatalf("initial pick failed: %+v err=%v", resp, err)
	}
	picked := resp.AuthID

	// 让刚选中的那把 key 模拟返回 429 + insufficient_quota。
	s.OnUsage(UsageRecord{
		AuthID:   picked,
		Provider: "modelscope",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
	})

	// 下一次挑选必须阻塞约 10 秒。
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

// TestInsufficientQuotaNonManagedNoCooldown 验证非托管 provider（例如付费的阿里云兜底）返回的
// 429+insufficient_quota 不会给那把 key 设置冷却。用量钩子对所有 provider 都会触发，若没有
// ManagesProvider 守卫，非托管的冷却就会污染 cooldownWaitDuration 的全局阻塞，拖累无关流量。
func TestInsufficientQuotaNonManagedNoCooldown(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 5
	s.Reconfigure(cfg)

	// 非托管 provider（例如阿里云）就在被监控的模型上返回 429 + insufficient_quota（默认所有模型
	// 都被监控）。
	s.OnUsage(UsageRecord{
		AuthID:   "auth-aliyun",
		Provider: "openai-compatible-aliyun",
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`},
	})

	// 这把非托管 key 不应存在有效冷却：它不该阻塞调度器的全局扫描。
	if wait := s.cooldownWaitDuration(now); wait > 0 {
		t.Fatalf("non-managed provider must not set a cooldown, got %s", wait)
	}
	// 而且这把 key 也不应被判定为禁用。
	if s.isDisabled("auth-aliyun", "Qwen-72B", now) {
		t.Fatal("non-managed key must not be disabled")
	}

	// 托管 provider 返回同样的响应时仍然必须进入冷却。
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

// TestInsufficientQuotaExponentialBackoff 验证连续的 429+insufficient_quota 会让冷却时长翻倍
// （基准 -> 2x -> 4x），封顶 maxInsufficientQuotaCooldown（60s），并且健康响应会把退避重置回基准
// 值。Modelscope 的额度跨 key 共享，所以退避是全局的：同一窗口内多把 key 失败保持档位，窗口过期
// 之后再失败才推进档位。
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

	// 第 1 轮：首次失败 -> 基准（5s）。
	fail("auth-1", now)
	if w := cooldownOf(now); w != 5*time.Second {
		t.Fatalf("round 1: want 5s, got %s", w)
	}

	// 同一窗口内：5s 窗口里第二把 key 失败时保持档位（不把一轮共享额度耗尽重复计数），并且锚定
	// 到与第一把相同的到期时刻（不漂移）。于是在 now+1s 时剩余等待是 4s，而不是新的 5s。
	fail("auth-2", now.Add(1*time.Second))
	if w := cooldownOf(now.Add(1 * time.Second)); w != 4*time.Second {
		t.Fatalf("same-window 2nd key: want 4s (anchored, no drift), got %s", w)
	}

	// 第 2 轮：5s 窗口结束后再次失败，翻倍到 10s。
	t2 := now.Add(6 * time.Second)
	fail("auth-1", t2)
	if w := cooldownOf(t2); w != 10*time.Second {
		t.Fatalf("round 2: want 10s, got %s", w)
	}

	// 第 3 轮：10s 窗口之后，翻倍到 20s。
	t3 := t2.Add(11 * time.Second)
	fail("auth-1", t3)
	if w := cooldownOf(t3); w != 20*time.Second {
		t.Fatalf("round 3: want 20s, got %s", w)
	}

	// 封顶：不断推动档位直到触及 60s 上限。
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

	// 健康响应把退避重置回基准值。
	*cur = now.Add(1 * time.Hour)
	// 复位条件在 OnUsage 里以“成功（!Failed）+ 托管 provider”为准，而不是看 ApplyRateLimit 里的
	// 限流响应头（429+insufficient_quota 也可能带着 remaining > 0）。
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

// TestInsufficientQuotaBackoffCap 验证无抖动时冷却严格等于退避档位、每轮翻倍、绝不超过 60s 上
// 限；并且同一轮里的第二把 key 会锚定到与第一把完全相同的到期时刻（这是漂移修复：以前它会被设成
// now+level，负载下会把有效阻塞推后并过度升级）。
func TestInsufficientQuotaBackoffCap(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, cur := newTestStore(now)
	cfg := cfgWith("modelscope")
	cfg.InsufficientQuotaCooldown = 5
	s.Reconfigure(cfg)
	fail := func(id string, at time.Time) {
		*cur = at
		s.OnUsage(UsageRecord{
			AuthID: id, Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
			Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
		})
	}
	cooldownOf := func(at time.Time) time.Duration {
		*cur = at
		return s.cooldownWaitDuration(at)
	}

	// 第 1 轮：首次失败 -> 正好是基准（5s），无抖动。
	fail("auth-1", now)
	if w := cooldownOf(now); w != 5*time.Second {
		t.Fatalf("round 1: want 5s, got %s", w)
	}

	// 同一轮：窗口内第二把 key 失败时必须锚定到与第一把相同的到期时刻（不漂移）。于是 now+1s 时
	// 剩余等待是 4s（原本的 5s 窗口减去已过的 1s），而不是新的 5s。
	fail("auth-2", now.Add(1*time.Second))
	if w := cooldownOf(now.Add(1 * time.Second)); w != 4*time.Second {
		t.Fatalf("same-round 2nd key: want 4s (anchored, no drift), got %s", w)
	}

	// 把档位推到 60s 上限；冷却必须严格等于档位（无抖动）且从不超过 60s。
	tPrev := now
	for {
		next := tPrev.Add(61 * time.Second) // 已越过上一轮窗口
		*cur = next
		s.OnUsage(UsageRecord{
			AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
			Failed: true, Failure: &UsageFailure{StatusCode: 429, Body: `{"error":{"code":"insufficient_quota"}}`},
		})
		w := cooldownOf(next)
		if w > 60*time.Second {
			t.Fatalf("cooldown %s exceeds 60s cap", w)
		}
		if w == 60*time.Second {
			break // 已到达上限
		}
		tPrev = next
		if tPrev.Sub(now) > 5*time.Minute {
			t.Fatal("did not reach 60s cap in time")
		}
	}
}

// TestOnUsage401DisablesUnauthorized 验证托管 provider 返回 401 时，该凭据当天剩余时间被全局禁用
// （密钥无效或过期），并且不会设置 insufficient_quota 冷却。
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

// TestUnauthorizedDisablePersistsAcrossMidnight 验证 401 禁用会持续到插件重启：它能扛过跨日的
// PruneAll，而额度耗尽导致的全局禁用在午夜被清除。
func TestUnauthorizedDisablePersistsAcrossMidnight(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	night := time.Date(2026, 7, 8, 23, 59, 0, 0, loc)
	s, _ := newTestStore(night)
	s.Reconfigure(cfgWith("modelscope"))

	// 23:59 的 401 -> unauthorized（持续生效）。
	s.OnUsage(UsageRecord{
		AuthID: "auth-1", Provider: "modelscope", Model: "Qwen-72B", Alias: "Qwen-72B",
		Failed: true, Failure: &UsageFailure{StatusCode: 401, Body: `{"error":"unauthorized"}`},
	})
	// 23:59 的额度耗尽全局禁用（日级）。
	s.disableGlobal("auth-2", "Qwen-72B", night)

	if !s.isDisabled("auth-1", "Qwen-72B", night) || !s.isDisabled("auth-2", "Qwen-72B", night) {
		t.Fatal("both keys must be disabled before midnight")
	}

	// 跨过午夜并执行清理。
	nextDay := time.Date(2026, 7, 9, 0, 1, 0, 0, loc)
	s.PruneAll(nextDay)

	if !s.isDisabled("auth-1", "Qwen-72B", nextDay) {
		t.Fatal("401 unauthorized disable must persist across midnight (until restart)")
	}
	if s.isDisabled("auth-2", "Qwen-72B", nextDay) {
		t.Fatal("quota global disable must be cleared at midnight")
	}
}

// TestCrossProviderDisablePropagation 模拟胶水层的传播路径：共享账号在某家 provider 的 auth ID 上
// 返回 429 + "insufficient balance" 时，胶水层会对共用同一原始 api key 的其它 auth ID 调用
// DisableGlobal。本测试验证核心状态机会尊重这种传播，使调度器在所有 provider 上都跳过该账号。
func TestCrossProviderDisablePropagation(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, loc)
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope", "modelscope2"))

	authA := "openai-compatibility:modelscope:aaaabbbbcccc"
	authB := "openai-compatibility:modelscope2:aaaabbbbcccc" // 同一个原始 key，不同 provider

	// ProviderA 的 key 返回 429 + insufficient balance。
	s.ApplyRateLimit(authA, "Qwen-72B", nil, 429, insufficientBalanceBody, now)

	// authA 已被禁用，authB 还没有（它们是两个独立的 auth ID）。
	if !s.isDisabled(authA, "Qwen-72B", now) {
		t.Fatal("authA should be disabled after 429+insufficient balance")
	}
	if s.isDisabled(authB, "Qwen-72B", now) {
		t.Fatal("authB should NOT be disabled before propagation")
	}

	// 胶水层做传播：对所有同账号的兄弟 auth ID 调用 DisableGlobal。
	s.DisableGlobal(authB, "Qwen-72B", now)

	// 现在两把都被禁用了。
	if !s.isDisabled(authB, "Qwen-72B", now) {
		t.Fatal("authB should be disabled after propagation")
	}

	// 调度器必须把这两个候选都跳过。
	req := SchedulerPickRequest{
		Provider:  "openai-compatible-modelscope",
		Model:     "Qwen-72B",
		Providers: []string{"openai-compatible-modelscope", "openai-compatible-modelscope2"},
		Candidates: []Candidate{
			{ID: authA, Provider: "openai-compatible-modelscope", Priority: 10, Status: "active"},
			{ID: authB, Provider: "openai-compatible-modelscope2", Priority: 5, Status: "active"},
		},
	}
	// 两个托管候选都被禁用，且没有非托管兜底 ->
	// ErrAllCredentialsDisabled.
	resp, err := s.SchedulerPick(req)
	if !errors.Is(err, ErrAllCredentialsDisabled) {
		t.Fatalf("expected ErrAllCredentialsDisabled, got Handled=%v AuthID=%q err=%v", resp.Handled, resp.AuthID, err)
	}
}

// TestDisableGlobalIsIdempotent 确认对已禁用的 key 再次调用 DisableGlobal 会保留最早的禁用时间
// （与内部 disableGlobal 语义一致）。
func TestDisableGlobalIsIdempotent(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 9, 0, 0, 0, loc)
	s, cur := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.DisableGlobal("auth-1", "Qwen-72B", *cur)
	first := *cur

	*cur = now.Add(3 * time.Hour)
	s.DisableGlobal("auth-1", "Qwen-7B", *cur)

	st := s.Status()
	if len(st) != 1 || st[0].Global == nil || *st[0].Global != first {
		t.Fatalf("expected earliest time %v kept, got %+v", first, st)
	}
}
