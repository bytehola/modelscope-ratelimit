package ratelimit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// recordLogger 捕获核心层的日志输出，这样钉定相关的诊断信息可以被断言。
type recordLogger struct{ lines []string }

func (l *recordLogger) Printf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// pinnedReq 构造一个 scheduler.pick 载荷，和宿主为插件自己的嵌套 chat 调用转发过来的那份一
// 致：既有标记请求头，也有内部来源的 metadata。缺了 metadata 时，这个请求看起来就像客户端直接
// 发来的。
func pinnedReq(pinned string, internal bool, cands ...Candidate) SchedulerPickRequest {
	req := SchedulerPickRequest{
		Provider:   "mixed",
		Providers:  []string{"openai-compatible-modelscope-ai-deepseek2", "openai-compatible-modelscope-my"},
		Model:      "glm-5.2",
		Candidates: cands,
		Options: SchedulerOptions{
			Headers: map[string][]string{PinnedProviderHeader: {pinned}},
		},
	}
	if internal {
		req.Options.Metadata = map[string]any{internalExecutionMetadataKey: internalExecutionSource}
	}
	return req
}

func TestSchedulerPickHonoursPinnedProvider(t *testing.T) {
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope-ai-deepseek2", "modelscope-my"))

	cands := []Candidate{
		{ID: "auth-glm", Provider: "openai-compatible-modelscope-ai-deepseek2", Priority: 4},
		{ID: "auth-ds", Provider: "openai-compatible-modelscope-my", Priority: 4},
	}

	// 未钉定时按配置顺序优先，因此选中 provider 1。
	plain := SchedulerPickRequest{Provider: "mixed", Providers: []string{"mixed"}, Model: "glm-5.2", Candidates: cands}
	first, err := s.SchedulerPick(plain)
	if err != nil {
		t.Fatalf("unpinned pick: %v", err)
	}
	if first.AuthID != "auth-glm" {
		t.Fatalf("unpinned pick = %q, want the first configured provider", first.AuthID)
	}

	got, err := s.SchedulerPick(pinnedReq("modelscope-my", true, cands...))
	if err != nil {
		t.Fatalf("pinned pick: %v", err)
	}
	if got.AuthID != "auth-ds" || !got.Handled {
		t.Fatalf("pinned pick = %+v, want auth-ds handled", got)
	}
}

// TestSchedulerPickPinnedUnavailableFails 守住引入钉定就是为了堵住的那个漏洞：被钉定的 provider
// 无法服务时，挑选必须失败而不能让位；一旦让位，嵌套调用就会被交给别的 provider 的凭据，那家就
// 失去了属于它自己的原生 Responses 尝试。
func TestSchedulerPickPinnedUnavailableFails(t *testing.T) {
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope-ai-deepseek2", "modelscope-my")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)
	s.DisableGlobal("auth-glm", "glm-5.2", now)

	_, err := s.SchedulerPick(pinnedReq("modelscope-ai-deepseek2", true,
		Candidate{ID: "auth-glm", Provider: "openai-compatible-modelscope-ai-deepseek2", Priority: 4},
		Candidate{ID: "auth-ds", Provider: "openai-compatible-modelscope-my", Priority: 4},
	))
	if !errors.Is(err, ErrPinnedProviderUnavailable) {
		t.Fatalf("err = %v, want ErrPinnedProviderUnavailable", err)
	}
}

func TestSchedulerPickPinnedSkipsProviderAfter429(t *testing.T) {
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	cfg := cfgWith("modelscope-ai-deepseek2", "modelscope-my")
	cfg.SkipProviderOn429 = true
	s.Reconfigure(cfg)
	s.SetSkipProvider("modelscope-ai-deepseek2")

	_, err := s.SchedulerPick(pinnedReq("modelscope-ai-deepseek2", true,
		Candidate{ID: "auth-glm", Provider: "openai-compatible-modelscope-ai-deepseek2", Priority: 4},
	))
	if !errors.Is(err, ErrPinnedProviderUnavailable) {
		t.Fatalf("err = %v, want ErrPinnedProviderUnavailable for a skipped provider", err)
	}
}

// TestSchedulerPickIgnoresClientSuppliedPin 防止调用方指定由哪个账号服务它：这个标记只在内部来
// 源的挑选里才生效。
func TestSchedulerPickIgnoresClientSuppliedPin(t *testing.T) {
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope-ai-deepseek2", "modelscope-my"))

	cands := []Candidate{
		{ID: "auth-glm", Provider: "openai-compatible-modelscope-ai-deepseek2", Priority: 4},
		{ID: "auth-ds", Provider: "openai-compatible-modelscope-my", Priority: 4},
	}
	got, err := s.SchedulerPick(pinnedReq("modelscope-my", false, cands...))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got.AuthID != "auth-glm" {
		t.Fatalf("client-supplied pin was honoured: %+v", got)
	}
}

func TestSchedulerPickIgnoresPinForUnmanagedProvider(t *testing.T) {
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope-ai-deepseek2", "modelscope-my"))

	cands := []Candidate{
		{ID: "auth-glm", Provider: "openai-compatible-modelscope-ai-deepseek2", Priority: 4},
		{ID: "auth-ds", Provider: "openai-compatible-modelscope-my", Priority: 4},
	}
	got, err := s.SchedulerPick(pinnedReq("some-other-provider", true, cands...))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got.AuthID != "auth-glm" {
		t.Fatalf("unknown pinned provider changed the pick: %+v", got)
	}
}

func TestPinnedProviderReadsHeaderTolerantly(t *testing.T) {
	c := cfgWith("modelscope-my")
	req := SchedulerPickRequest{
		Options: SchedulerOptions{
			Headers:  map[string][]string{"x-MODELSCOPE-ratelimit-PROVIDER": {"  ModelsCopE-My  "}},
			Metadata: map[string]any{internalExecutionMetadataKey: internalExecutionSource},
		},
	}
	if got := c.PinnedProvider(req); got != "modelscope-my" {
		t.Fatalf("PinnedProvider = %q, want modelscope-my", got)
	}
}

// TestSchedulerPickAnnouncesDroppedPin 保护线上的可诊断性：钉定之所以生效，是因为宿主会把嵌套
// 执行标记为内部来源；因此只带请求头却没有该标记时，必须打日志说出来而不能悄悄忽略（静默丢弃等
// 于把协议降级悄悄放回来）。
func TestSchedulerPickAnnouncesDroppedPin(t *testing.T) {
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	rec := &recordLogger{}
	s := NewStore(rec)
	s.SetClock(func() time.Time { return now })
	s.Reconfigure(cfgWith("modelscope-ai-deepseek2", "modelscope-my"))

	_, err := s.SchedulerPick(pinnedReq("modelscope-my", false,
		Candidate{ID: "auth-glm", Provider: "openai-compatible-modelscope-ai-deepseek2", Priority: 4},
	))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	found := false
	for _, line := range rec.lines {
		if strings.Contains(line, "ignoring pin provider=modelscope-my") {
			found = true
		}
	}
	if !found {
		t.Fatalf("dropped pin was not reported, lines = %v", rec.lines)
	}
}
