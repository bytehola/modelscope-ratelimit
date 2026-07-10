package ratelimit

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLogger) Printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingLogger) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

func newRecordingStore(now time.Time) (*Store, *recordingLogger, *time.Time) {
	loc := time.FixedZone("CST", 8*3600)
	log := &recordingLogger{}
	s := NewStore(log)
	s.SetLocation(loc)
	cur := now
	s.SetClock(func() time.Time { return cur })
	s.Reconfigure(cfgWith("modelscope"))
	return s, log, &cur
}

func TestOverviewCountsHTMLAndLogs(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, log, _ := newRecordingStore(now)

	req := SchedulerPickRequest{Provider: "modelscope", Model: "Qwen-72B", Candidates: []Candidate{
		{ID: "k1", Provider: "modelscope"},
		{ID: "k2", Provider: "modelscope"},
		{ID: "k3", Provider: "modelscope"},
	}}
	// Prime "seen" with 3 keys (no disables yet -> no schedule log).
	for i := 0; i < 3; i++ {
		if _, err := s.SchedulerPick(req); err != nil {
			t.Fatal(err)
		}
	}
	if len(log.Lines()) != 0 {
		t.Fatalf("expected no logs during prime, got %v", log.Lines())
	}

	// k1 global, k2 model-only.
	s.ApplyRateLimit("k1", "Qwen-72B", totalExhaustedHdr(), now)
	s.ApplyRateLimit("k2", "Qwen-72B", modelExhaustedHdr(), now)

	total, disabled, available := s.Overview(now)
	if total != 3 || disabled != 2 || available != 1 {
		t.Fatalf("overview = total %d disabled %d avail %d", total, disabled, available)
	}

	d, a, tot := s.Counts("Qwen-72B", now)
	if tot != 3 || d != 2 || a != 1 {
		t.Fatalf("counts = tot %d dis %d avail %d", tot, d, a)
	}
	if d2, _, _ := s.Counts("Qwen-7B", now); d2 != 1 {
		t.Fatalf("Qwen-7B disabled = %d, want 1 (global only)", d2)
	}

	// Disable logs are concise: they carry no count suffix.
	joined := strings.Join(log.Lines(), "\n")
	for _, want := range []string{"model Qwen-72B disabled", "globally disabled"} {
		if !strings.Contains(joined, want) {
			t.Errorf("disable log missing %q:\n%s", want, joined)
		}
	}
	for _, notWant := range []string{"剩余请求次数:", "已限: 1", "已限: 2"} {
		if strings.Contains(joined, notWant) {
			t.Errorf("disable log should not contain %q:\n%s", notWant, joined)
		}
	}

	// A pick with disables present emits a schedule summary with the counts.
	if _, err := s.SchedulerPick(req); err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(log.Lines(), "\n")
	for _, want := range []string{"schedule model=Qwen-72B", "已限: 2", "可用: 1", "总共: 3", "剩余请求次数:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("schedule log missing %q:\n%s", want, joined)
		}
	}

	masked := map[string]string{"k1": "\u2026AAAAAAAA", "k2": "\u2026BBBBBBBB", "k3": "\u2026CCCCCCCC"}
	html := s.RenderStatusHTMLFull(now, s.config(), masked, 3)
	for _, want := range []string{
		"Modelscope 限流状态",
		`供应商 <b>modelscope</b>`,
		`总共 <b>3</b>`,
		`已禁用 <b>2</b>`,
		`可用 <b>1</b>`,
		`title="k1"`,
		`title="k2"`,
		`title="k3"`,
		"\u2026AAAAAAAA", "\u2026BBBBBBBB", "\u2026CCCCCCCC",
		"全部禁用", "模型禁用", "全部正常", "下次重置",
		"按模型可用次数汇总",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q", want)
		}
	}
}

func TestRenderStatusHTMLFull(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _, _ := newRecordingStore(now)

	// Two disabled keys among the resolved set.
	s.ApplyRateLimit("k1", "Qwen-72B", totalExhaustedHdr(), now)
	s.ApplyRateLimit("k2", "Qwen-72B", modelExhaustedHdr(), now)

	// Server-side resolution supplies masked keys and a real total of 10.
	masked := map[string]string{"k1": "\u2026AAAAAAAA", "k2": "\u2026BBBBBBBB"}
	html := s.RenderStatusHTMLFull(now, s.config(), masked, 10)

	for _, want := range []string{
		`总共 <b>10</b>`,
		`已禁用 <b>2</b>`,
		`可用 <b>8</b>`,
		"\u2026AAAAAAAA", "\u2026BBBBBBBB",
		`title="k1"`, `title="k2"`,
		"全部禁用", "模型禁用",
		"按模型可用次数汇总",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q", want)
		}
	}
	// Server-side page must not ship any browser key-resolver JS or input.
	for _, unwanted := range []string{`<script>`, `id="rl-key"`, `id="rl-provs"`} {
		if strings.Contains(html, unwanted) {
			t.Errorf("html must not contain %q (no browser JS)", unwanted)
		}
	}
}

// modelRemHdr builds a rate-limit header set with the given model and total
// remaining values (limits omitted). remaining > 0 does not trigger a disable,
// so it can prime an active key's observed quota before another key is disabled.
func modelRemHdr(modelRem, totalRem int) map[string][]string {
	return map[string][]string{
		"Modelscope-Ratelimit-Model-Requests-Remaining": {strconv.Itoa(modelRem)},
		"Modelscope-Ratelimit-Requests-Remaining":       {strconv.Itoa(totalRem)},
	}
}

// TestScheduleLogShowsModelRemaining verifies the schedule log reports the sum
// of remaining requests for the model across still-active keys (剩余请求次数)
// when a pick is made with some keys disabled.
func TestScheduleLogShowsModelRemaining(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, log, _ := newRecordingStore(now)

	// Prime seen with 3 keys via the scheduler (no remaining data yet).
	req := SchedulerPickRequest{Provider: "modelscope", Model: "Qwen-72B", Candidates: []Candidate{
		{ID: "k1", Provider: "modelscope"},
		{ID: "k2", Provider: "modelscope"},
		{ID: "k3", Provider: "modelscope"},
	}}
	for i := 0; i < 3; i++ {
		if _, err := s.SchedulerPick(req); err != nil {
			t.Fatal(err)
		}
	}

	// k2 still has 100 model requests left (active, recorded, not disabled).
	s.ApplyRateLimit("k2", "Qwen-72B", modelRemHdr(100, 500), now)
	// k3 still has 40 model requests left.
	s.ApplyRateLimit("k3", "Qwen-72B", modelRemHdr(40, 500), now)
	// k1 exhausts the model quota -> disabled. The disable log is now concise.
	s.ApplyRateLimit("k1", "Qwen-72B", modelExhaustedHdr(), now)
	log.Lines() // drain disables

	// A pick with k1 disabled emits a schedule summary reporting the remaining
	// quota on the active keys: 100 + 40 = 140.
	if _, err := s.SchedulerPick(req); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(log.Lines(), "\n")
	for _, want := range []string{"剩余请求次数: 140", "已限: 1", "可用: 2", "总共: 3", "schedule model=Qwen-72B"} {
		if !strings.Contains(joined, want) {
			t.Errorf("schedule log missing %q:\n%s", want, joined)
		}
	}
}

// TestDedupModelsByNameMergesAliasAndUpstream verifies that a model recorded
// under both an alias and its upstream storage key (which the usage hook maps to
// the same display name) collapses into a single row, and is not double-counted
// in the per-model summary.
func TestDedupModelsByNameMergesAliasAndUpstream(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	// Same model, recorded under alias "hy3" and upstream "Tencent-Hunyuan/Hy3".
	s.OnUsage(UsageRecord{
		Provider: "modelscope", AuthID: "k1",
		Model: "Tencent-Hunyuan/Hy3", Alias: "hy3",
		ResponseHeaders: modelExhaustedHdr(),
	})
	s.ApplyRateLimit("k1", "Tencent-Hunyuan/Hy3", modelExhaustedHdr(), now)

	snap := s.Snapshot(now)
	kv := snap["k1"]
	if len(kv.Models) != 2 {
		t.Fatalf("pre-dedup model count = %d, want 2", len(kv.Models))
	}
	merged := dedupModelsByName(kv.Models)
	if len(merged) != 1 {
		t.Fatalf("post-dedup model count = %d, want 1: %+v", len(merged), merged)
	}
	if merged[0].Name != "Tencent-Hunyuan/Hy3" || !merged[0].Disabled || merged[0].Remaining != 0 {
		t.Fatalf("merged = %+v", merged[0])
	}

	// The status page must not double-count the disabled key in the summary.
	html := s.RenderStatusHTMLFull(now, s.config(), map[string]string{"k1": "\u2026AAAAAAAA"}, 1)
	if strings.Contains(html, "2 个已禁用") {
		t.Errorf("summary double-counted disabled key:\n%s", html)
	}
	if !strings.Contains(html, "1 个已禁用") {
		t.Errorf("summary should show 1 disabled key:\n%s", html)
	}
}

// TestDedupModelsByNamePrefersLatestObservation verifies that a stale disable
// recorded under one name does not hide a fresh (recovered) snapshot recorded
// later under another name that resolves to the same display name.
func TestDedupModelsByNamePrefersLatestObservation(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, cur := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	// Earlier: model exhausted under alias "hy3" (maps to MiniMax/MiniMax-M3).
	earlier := now.Add(-time.Hour)
	*cur = earlier
	s.OnUsage(UsageRecord{
		Provider: "modelscope", AuthID: "k1",
		Model: "MiniMax/MiniMax-M3", Alias: "hy3",
		ResponseHeaders: modelExhaustedHdr(),
	})

	// Later: same display name, fresh quota observed under the upstream name.
	*cur = now
	s.ApplyRateLimit("k1", "MiniMax/MiniMax-M3", modelRemHdr(100, 500), now)

	snap := s.Snapshot(now)
	merged := dedupModelsByName(snap["k1"].Models)
	if len(merged) != 1 {
		t.Fatalf("merged count = %d, want 1: %+v", len(merged), merged)
	}
	if merged[0].Disabled {
		t.Errorf("merged should not be disabled (fresh snapshot wins): %+v", merged[0])
	}
	if merged[0].Remaining != 100 {
		t.Errorf("merged remaining = %d, want 100: %+v", merged[0].Remaining, merged[0])
	}
	if merged[0].Name != "MiniMax/MiniMax-M3" {
		t.Errorf("merged name = %q", merged[0].Name)
	}
}

// TestKeyDetailStatusAndColumnOrder verifies that an active key with observed
// remaining data is shown as 正常 (not mislabelled 模型禁用), and that the
// 模型剩余 column precedes 总剩余.
func TestKeyDetailStatusAndColumnOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _, _ := newRecordingStore(now)

	// k1: active, observed remaining (100 model / 500 total) — must be 正常.
	s.ApplyRateLimit("k1", "Qwen-72B", modelRemHdr(100, 500), now)
	// k2: model-exhausted — 模型禁用.
	s.ApplyRateLimit("k2", "Qwen-72B", modelExhaustedHdr(), now)
	// k3: total-exhausted — 全局禁用.
	s.ApplyRateLimit("k3", "Qwen-72B", totalExhaustedHdr(), now)

	masked := map[string]string{"k1": "k1tail0000001", "k2": "k2tail0000002", "k3": "k3tail0000003"}
	html := s.RenderStatusHTMLFull(now, s.config(), masked, 3)

	// Column order: 模型剩余 before 总剩余.
	hdr := `<th>模型剩余</th><th>总剩余</th>`
	if !strings.Contains(html, hdr) {
		t.Errorf("header order wrong, missing %q", hdr)
	}
	// k1 active with data must be 正常, NOT 模型禁用.
	if !strings.Contains(html, `title="k1"`) {
		t.Fatal("k1 row missing")
	}
	k1Start := strings.Index(html, `title="k1"`)
	k1End := strings.Index(html[k1Start:], `</tr>`) + k1Start
	k1Row := html[k1Start:k1End]
	if !strings.Contains(k1Row, `b-ok">全部正常`) {
		t.Errorf("k1 should be 正常 (active with data):\n%s", k1Row)
	}
	if strings.Contains(k1Row, "模型禁用") {
		t.Errorf("k1 must not be 模型禁用:\n%s", k1Row)
	}
	// k1 model bar is active: a green fill is present and no red (disabled) fill.
	if !strings.Contains(k1Row, `fill g`) || strings.Contains(k1Row, `fill r`) {
		t.Errorf("k1 model bar should be active (green, no red):\n%s", k1Row)
	}
	if !strings.Contains(k1Row, `mb-n g`) {
		t.Errorf("k1 model name should be green (quota > 0):\n%s", k1Row)
	}
	if !strings.Contains(k1Row, `class="track"`) {
		t.Errorf("k1 should render a progress bar track:\n%s", k1Row)
	}
	// Within k1's row, 模型剩余 (model name) precedes 总剩余 (total value).
	iChip := strings.Index(k1Row, "Qwen-72B")
	iTotal := strings.Index(k1Row, "500")
	if iChip < 0 || iTotal < 0 || iChip > iTotal {
		t.Errorf("model bar should precede total remaining in row:\n%s", k1Row)
	}
	// k2 model-disabled.
	k2Start := strings.Index(html, `title="k2"`)
	k2Row := html[k2Start : strings.Index(html[k2Start:], `</tr>`)+k2Start]
	if !strings.Contains(k2Row, `b-mod">模型禁用`) {
		t.Errorf("k2 should be 模型禁用:\n%s", k2Row)
	}
	// k3 globally disabled; its bars must render red (disabled).
	k3Start := strings.Index(html, `title="k3"`)
	k3Row := html[k3Start : strings.Index(html[k3Start:], `</tr>`)+k3Start]
	if !strings.Contains(k3Row, `b-glob">全部禁用`) {
		t.Errorf("k3 should be 全局禁用:\n%s", k3Row)
	}
	if !strings.Contains(k3Row, `fill r`) {
		t.Errorf("k3 disabled bars should be red:\n%s", k3Row)
	}
	if !strings.Contains(k3Row, `mb-n r`) {
		t.Errorf("k3 model name should be red (quota 0):\n%s", k3Row)
	}
}

// TestRenderGuidanceHTML verifies the Apple-style guidance page renders the
// notice, step pill and config field cards for each reason, and that the
// action directs to 插件管理 → 编辑配置 (not hand-editing config.yaml).
func TestRenderGuidanceHTML(t *testing.T) {
	cases := []struct {
		name   string
		reason GuidanceReason
		detail string
		wants  []string
	}{
		{"missing", GuidanceMissing, "", []string{
			`class="title"`, `配置引导`, `尚未配置`, `插件管理 → 编辑配置`,
			`providers`, `必填`, `management_key`, `host_base_url`, `选填`,
			`class="fcard"`, `class="step"`,
		}},
		{"resolution", GuidanceResolutionFailed, "boom: HTTP 401", []string{
			`服务端解析失败`, `boom: HTTP 401`, `class="detail"`,
		}},
		{"provider", GuidanceProviderNotFound, "modelscope, foo", []string{
			`providers 设置错误`, `modelscope, foo`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := cfgWith("modelscope")
			html := RenderGuidanceHTML(cfg, c.reason, c.detail)
			for _, w := range c.wants {
				if !strings.Contains(html, w) {
					t.Errorf("missing %q", w)
				}
			}
			// No legacy GitHub-style classes remain.
			for _, bad := range []string{`class="hint"`, `class="warn"`, `class="field"`, `class="muted"`, `<h2>`} {
				if strings.Contains(html, bad) {
					t.Errorf("legacy markup %q still present", bad)
				}
			}
		})
	}
}

// TestKeyDetailSortOrder verifies the Key 明细 ordering: by model remaining
// high->low, then status normal -> model-disabled -> global-disabled.
func TestKeyDetailSortOrder(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	s, _, _ := newRecordingStore(now)

	// Prime seen with 5 keys.
	req := SchedulerPickRequest{Provider: "modelscope", Model: "Qwen-72B", Candidates: []Candidate{
		{ID: "k1", Provider: "modelscope"}, {ID: "k2", Provider: "modelscope"},
		{ID: "k3", Provider: "modelscope"}, {ID: "k4", Provider: "modelscope"},
		{ID: "k5", Provider: "modelscope"},
	}}
	for i := 0; i < 5; i++ {
		if _, err := s.SchedulerPick(req); err != nil {
			t.Fatal(err)
		}
	}
	// k1 normal rem 100, k2 normal rem 50, k3 unobserved (no data),
	// k4 model-disabled, k5 global-disabled.
	s.ApplyRateLimit("k1", "Qwen-72B", modelRemHdr(100, 500), now)
	s.ApplyRateLimit("k2", "Qwen-72B", modelRemHdr(50, 500), now)
	s.ApplyRateLimit("k4", "Qwen-72B", modelExhaustedHdr(), now)
	s.ApplyRateLimit("k5", "Qwen-72B", totalExhaustedHdr(), now)

	masked := map[string]string{"k1": "k1tail", "k2": "k2tail", "k3": "k3tail", "k4": "k4tail", "k5": "k5tail"}
	html := s.RenderStatusHTMLFull(now, s.config(), masked, 5)

	// Expected row order by position of title="kX".
	want := []string{"k1", "k2", "k3", "k4", "k5"}
	pos := map[string]int{}
	for _, k := range want {
		idx := strings.Index(html, `title="`+k+`"`)
		if idx < 0 {
			t.Fatalf("key %q not found in html", k)
		}
		pos[k] = idx
	}
	for i := 1; i < len(want); i++ {
		if pos[want[i-1]] >= pos[want[i]] {
			t.Errorf("order wrong: %s(%d) should precede %s(%d)",
				want[i-1], pos[want[i-1]], want[i], pos[want[i]])
		}
	}
}
