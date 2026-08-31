package ratelimit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// recordLogger captures core log output so the pin diagnostics are assertable.
type recordLogger struct{ lines []string }

func (l *recordLogger) Printf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// pinnedReq builds a scheduler.pick payload like the one the host forwards for
// the plugin's own nested chat call: the marker header plus the internal-source
// metadata. Without the metadata the request looks client-originated.
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

	// Unpinned: configured order wins, so provider 1 is chosen.
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

// TestSchedulerPickPinnedUnavailableFails guards the leak the pin exists to
// close: when the pinned provider cannot serve, the pick must fail instead of
// deferring, because deferring hands the nested call to another provider's
// credential and that provider loses its native Responses attempt.
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

// TestSchedulerPickIgnoresClientSuppliedPin keeps a caller from dictating which
// account serves it: the marker only counts for internally originated picks.
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

// TestSchedulerPickAnnouncesDroppedPin protects the live diagnosis: the pin only
// works because the host flags nested executions as internally originated, so a
// header that arrives without that marker must be reported rather than silently
// ignored (a silent drop would quietly re-enable the protocol downgrade).
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
