package ratelimit

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// rotationSim 是一个轻量测试台：按顺序驱动 SchedulerPick + OnUsage，并把每次得到的
// provider+key 选择记录成一张表。
type rotationSim struct {
	s         *Store
	providers []string
	keys      map[string][]string // provider -> key ID 列表
	seen      map[string]bool     // 已试过的 key（下一次候选列表里会被排除）
	t         *testing.T
}

func newRotationSim(t *testing.T, providers ...string) *rotationSim {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, loc)
	s, _ := newTestStore(now)
	cfg := cfgWith(providers...)
	cfg.InsufficientQuotaCooldown = 0 // 不产生阻塞，只关注轮换
	cfg.SkipProviderOn429 = false
	s.Reconfigure(cfg)

	keys := make(map[string][]string)
	for _, p := range providers {
		ids := make([]string, 4)
		for i := 0; i < 4; i++ {
			ids[i] = fmt.Sprintf("%s-k%d", p, i+1)
		}
		keys[p] = ids
	}
	return &rotationSim{s: s, providers: providers, keys: keys, seen: map[string]bool{}, t: t}
}

func (r *rotationSim) candidates() []Candidate {
	var out []Candidate
	for _, p := range r.providers {
		for _, id := range r.keys[p] {
			if r.seen[id] {
				continue
			}
			out = append(out, Candidate{ID: id, Provider: p, Priority: 1})
		}
	}
	return out
}

func (r *rotationSim) pick() string {
	req := SchedulerPickRequest{
		Provider:   r.providers[0],
		Providers:  r.providers,
		Model:      "Qwen-72B",
		Candidates: r.candidates(),
	}
	resp, err := r.s.SchedulerPick(req)
	if err != nil {
		return "ERROR:" + err.Error()
	}
	return resp.AuthID
}

func (r *rotationSim) usage429(id, provider string) {
	r.s.OnUsage(UsageRecord{
		AuthID:   id,
		Provider: provider,
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: insufficientQuotaBody},
	})
	r.seen[id] = true // 宿主会把已试过的 key 从下一次候选列表里移除
}

func (r *rotationSim) usage200(id, provider string) {
	r.s.OnUsage(UsageRecord{
		AuthID:   id,
		Provider: provider,
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
	})
	r.seen = map[string]bool{} // 新请求：所有 key 重新可用
}

func (r *rotationSim) usageBalanceExhausted(id, provider string) {
	r.s.OnUsage(UsageRecord{
		AuthID:   id,
		Provider: provider,
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
		Failed:   true,
		Failure:  &UsageFailure{StatusCode: 429, Body: insufficientBalanceBody},
	})
	r.seen[id] = true // 该 key 被禁用，并从候选列表里移除
}

func (r *rotationSim) newRequest() {
	r.seen = map[string]bool{}
}

func providerOf(id string) string {
	if idx := strings.LastIndex(id, "-k"); idx > 0 {
		return id[:idx]
	}
	return id
}

func shortName(id string) string {
	parts := strings.SplitN(id, "-k", 2)
	if len(parts) == 2 {
		return fmt.Sprintf("%s#%s", parts[0], parts[1])
	}
	return id
}

// TestRotationSim_UserScenario 复现约定的那两轮完整序列，并打印成可读的表格。
func TestRotationSim_UserScenario(t *testing.T) {
	sim := newRotationSim(t, "modelscope", "modelscope-ai", "modelscope-my")

	var rows []string
	rows = append(rows, "步骤 | 选中 | 结果")
	rows = append(rows, "-----|------|------")

	// === 第一轮 ===
	// skip OFF：先把 ms 家的 key 试完，再轮到 ai 家、my 家
	id := sim.pick()
	rows = append(rows, fmt.Sprintf("1 | %s | 429", shortName(id)))
	sim.usage429(id, providerOf(id))
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("2 | %s | 429", shortName(id)))
	sim.usage429(id, providerOf(id))
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("3 | %s | 429", shortName(id)))
	sim.usage429(id, providerOf(id))
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("4 | %s | 200", shortName(id)))
	sim.usage200(id, providerOf(id))

	rows = append(rows, "--- 第一轮结束 ---")

	id = sim.pick()
	rows = append(rows, fmt.Sprintf("1 | %s | 429", shortName(id)))
	sim.usage429(id, providerOf(id))
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("2 | %s | 429", shortName(id)))
	sim.usage429(id, providerOf(id))
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("3 | %s | 429", shortName(id)))
	sim.usage429(id, providerOf(id))
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("4 | %s | 429", shortName(id)))
	sim.usage429(id, providerOf(id))
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("5 | %s | 200", shortName(id)))
	sim.usage200(id, providerOf(id))

	for _, row := range rows {
		t.Log(row)
	}

	// 校验精确序列：每家都要先把自己的 key 试完才换下一家
	want := []string{"modelscope-k1", "modelscope-k2", "modelscope-k3", "modelscope-k4", "modelscope-k1", "modelscope-k2", "modelscope-k3", "modelscope-k4", "modelscope-ai-k1"}
	sim2 := newRotationSim(t, "modelscope", "modelscope-ai", "modelscope-my")
	var got []string
	for i := 0; i < len(want); i++ {
		id := sim2.pick()
		got = append(got, id)
		if i == 3 || i == len(want)-1 {
			sim2.usage200(id, providerOf(id))
		} else {
			sim2.usage429(id, providerOf(id))
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: want %s, got %s", i+1, want[i], got[i])
		}
	}
}

// TestRotationSim_AllExhausted 模拟 12 把 key 全部返回 429 直到无可用凭据，展示完整的轮询耗尽
// 序列。
func TestRotationSim_AllExhausted(t *testing.T) {
	sim := newRotationSim(t, "modelscope", "modelscope-ai", "modelscope-my")

	var rows []string
	rows = append(rows, "步骤 | 选中 | 结果")
	rows = append(rows, "-----|------|------")

	step := 1
	for {
		id := sim.pick()
		if id == "" || strings.HasPrefix(id, "ERROR") {
			rows = append(rows, fmt.Sprintf("%d | (全部耗尽) | 无可用 key", step))
			break
		}
		rows = append(rows, fmt.Sprintf("%d | %s | 429", step, shortName(id)))
		sim.usage429(id, providerOf(id))
		step++
	}

	for _, row := range rows {
		t.Log(row)
	}
}

// TestRotationSim_MixedSuccess 在各家各 key 上混合 200 与 429，展示：200 会留在当前这家并推进
// 它的 key 游标，而某家全部 key 都不可用时才会换下一家。
func TestRotationSim_MixedSuccess(t *testing.T) {
	sim := newRotationSim(t, "modelscope", "modelscope-ai", "modelscope-my")

	var rows []string
	rows = append(rows, "步骤 | 选中 | 结果")
	rows = append(rows, "-----|------|------")

	steps := []struct {
		failed bool
	}{
		{false}, // ms#1 200
		{false}, // ms#2 200
		{true},  // ms 第 3 把 429 -> 仍留在 ms 家
		{true},  // ms 第 4 把 429 -> ms 家已耗尽 -> 换到 ai 家
		{false}, // ai 第 1 把 200 -> 下一次请求回到 ms 家
		{false}, // ms 第 1 把 200（key 游标继续往后）
	}

	for i, st := range steps {
		id := sim.pick()
		result := "200"
		if st.failed {
			result = "429"
		}
		rows = append(rows, fmt.Sprintf("%d | %s | %s", i+1, shortName(id), result))
		if st.failed {
			sim.usage429(id, providerOf(id))
		} else {
			sim.usage200(id, providerOf(id))
		}
	}

	for _, row := range rows {
		t.Log(row)
	}

	// 校验：skip OFF 会先耗尽一家：ms#1、ms#2、ms#3、ms#4，然后才是 ai#1
	want := []string{"modelscope-k1", "modelscope-k2", "modelscope-k3", "modelscope-k4", "modelscope-k1", "modelscope-k2"}
	sim2 := newRotationSim(t, "modelscope", "modelscope-ai", "modelscope-my")
	for i, st := range steps {
		id := sim2.pick()
		if id != want[i] {
			t.Errorf("step %d: want %s, got %s", i+1, want[i], id)
		}
		if st.failed {
			sim2.usage429(id, providerOf(id))
		} else {
			sim2.usage200(id, providerOf(id))
		}
	}
}

// TestRotationSim_BalanceExhausted 展示 "insufficient balance" 的 429 会把该 key 当天永久禁
// 用，因此后续挑选都会跳过它，其余 key 继续轮换。
func TestRotationSim_BalanceExhausted(t *testing.T) {
	sim := newRotationSim(t, "modelscope", "modelscope-ai")

	var rows []string
	rows = append(rows, "步骤 | 选中 | 结果")
	rows = append(rows, "-----|------|------")

	// 1: ms#1 因 insufficient_balance 返回 429 -> 当天禁用
	id := sim.pick()
	rows = append(rows, fmt.Sprintf("1 | %s | 429 insufficient_balance (禁用)", shortName(id)))
	sim.usageBalanceExhausted(id, providerOf(id))

	// 2: ms#2 因 insufficient_quota 返回 429 -> 仍留在 ms 家
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("2 | %s | 429 insufficient_quota", shortName(id)))
	sim.usage429(id, providerOf(id))

	// 3: ms#3 返回 200 -> 复位
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("3 | %s | 200 (reset)", shortName(id)))
	sim.usage200(id, providerOf(id))

	// 4: 新请求，ms#1 仍在禁用中 -> 拿到 ms#3 并返回 200（回到第一家）
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("4 | %s | 200 (ms#1 仍禁用)", shortName(id)))
	sim.usage200(id, providerOf(id))

	for _, row := range rows {
		t.Log(row)
	}

	// 校验 key 序列
	sim2 := newRotationSim(t, "modelscope", "modelscope-ai")
	want := []string{"modelscope-k1", "modelscope-k2", "modelscope-k3", "modelscope-k4"}
	for i, w := range want {
		id := sim2.pick()
		if id != w {
			t.Errorf("step %d: want %s, got %s", i+1, w, id)
		}
		switch i {
		case 0:
			sim2.usageBalanceExhausted(id, providerOf(id))
		case 1:
			sim2.usage429(id, providerOf(id))
		case 2, 3:
			sim2.usage200(id, providerOf(id))
		}
	}
}
