package ratelimit

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// rotationSim is a lightweight test harness that drives SchedulerPick + OnUsage
// in sequence and records the resulting provider+key picks as a table.
type rotationSim struct {
	s         *Store
	providers []string
	keys      map[string][]string // provider -> key IDs
	seen      map[string]bool     // keys tried (excluded from next candidate list)
	t         *testing.T
}

func newRotationSim(t *testing.T, providers ...string) *rotationSim {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, loc)
	s, _ := newTestStore(now)
	cfg := cfgWith(providers...)
	cfg.InsufficientQuotaCooldown = 0 // no blocking; focus on rotation
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
	r.seen[id] = true // host removes tried key from next candidate list
}

func (r *rotationSim) usage200(id, provider string) {
	r.s.OnUsage(UsageRecord{
		AuthID:   id,
		Provider: provider,
		Model:    "Qwen-72B",
		Alias:    "Qwen-72B",
	})
	r.seen = map[string]bool{} // new request: all keys available again
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
	r.seen[id] = true // key is disabled and removed from candidate list
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

// TestRotationSim_UserScenario reproduces the exact two-cycle sequence the
// user specified and logs it as a readable table.
func TestRotationSim_UserScenario(t *testing.T) {
	sim := newRotationSim(t, "modelscope", "modelscope-ai", "modelscope-my")

	var rows []string
	rows = append(rows, "步骤 | 选中 | 结果")
	rows = append(rows, "-----|------|------")

	// === 第一轮 ===
	// 1: ms#1 429
	id := sim.pick()
	rows = append(rows, fmt.Sprintf("1 | %s | 429 insufficient_quota", shortName(id)))
	sim.usage429(id, providerOf(id))
	// 2: ai#1 429
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("2 | %s | 429 insufficient_quota", shortName(id)))
	sim.usage429(id, providerOf(id))
	// 3: my#1 429
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("3 | %s | 429 insufficient_quota", shortName(id)))
	sim.usage429(id, providerOf(id))
	// 4: ms#2 200
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("4 | %s | 200", shortName(id)))
	sim.usage200(id, providerOf(id))

	rows = append(rows, "--- 第一轮结束 ---")

	// === 第二轮 ===
	// 1: ms#3 429
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("1 | %s | 429 insufficient_quota", shortName(id)))
	sim.usage429(id, providerOf(id))
	// 2: ai#2 429
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("2 | %s | 429 insufficient_quota", shortName(id)))
	sim.usage429(id, providerOf(id))
	// 3: my#2 429
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("3 | %s | 429 insufficient_quota", shortName(id)))
	sim.usage429(id, providerOf(id))
	// 4: ms#4 429
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("4 | %s | 429 insufficient_quota", shortName(id)))
	sim.usage429(id, providerOf(id))
	// 5: ai#3 200
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("5 | %s | 200", shortName(id)))
	sim.usage200(id, providerOf(id))

	for _, row := range rows {
		t.Log(row)
	}

	// Verify the exact sequence
	want := []string{"modelscope-k1", "modelscope-ai-k1", "modelscope-my-k1", "modelscope-k2", "modelscope-k3", "modelscope-ai-k2", "modelscope-my-k2", "modelscope-k4", "modelscope-ai-k3"}
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

// TestRotationSim_AllExhausted simulates all 12 keys returning 429 until none
// are available. Shows the full round-robin exhaustion sequence.
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

// TestRotationSim_MixedSuccess simulates a mix of 200 and 429 responses across
// providers and keys, showing how a 200 keeps the provider while advancing its
// key, and a 429 rotates to the next provider.
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
		{true},  // ms#3 429 -> rotate to ai
		{true},  // ai#1 429 -> rotate to my
		{false}, // my#1 200 -> reset to ms
		{false}, // ms#4 200
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

	// Verify: ms#1, ms#2, ms#3, ai#1, my#1, ms#4 (the my#1 200 resets to ms)
	want := []string{"modelscope-k1", "modelscope-k2", "modelscope-k3", "modelscope-ai-k1", "modelscope-my-k1", "modelscope-k4"}
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

// TestRotationSim_BalanceExhausted shows how an "insufficient balance" 429
// permanently disables a key, so it's skipped in all subsequent picks while
// other keys continue to rotate.
func TestRotationSim_BalanceExhausted(t *testing.T) {
	sim := newRotationSim(t, "modelscope", "modelscope-ai")

	var rows []string
	rows = append(rows, "步骤 | 选中 | 结果")
	rows = append(rows, "-----|------|------")

	// 1: ms#1 429 insufficient_balance -> daily disabled
	id := sim.pick()
	rows = append(rows, fmt.Sprintf("1 | %s | 429 insufficient_balance (禁用)", shortName(id)))
	sim.usageBalanceExhausted(id, providerOf(id))

	// 2: ms#2 429 insufficient_quota -> rotate to ai
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("2 | %s | 429 insufficient_quota", shortName(id)))
	sim.usage429(id, providerOf(id))

	// 3: ai#1 200 (cursor at ai) -> reset to ms
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("3 | %s | 200 (cursor at ai -> reset)", shortName(id)))
	sim.usage200(id, providerOf(id))

	// 4: new request, ms#1 still disabled -> ms#3 200 (reset to first provider)
	id = sim.pick()
	rows = append(rows, fmt.Sprintf("4 | %s | 200 (ms#1 仍禁用)", shortName(id)))
	sim.usage200(id, providerOf(id))

	for _, row := range rows {
		t.Log(row)
	}

	// Verify key sequence
	sim2 := newRotationSim(t, "modelscope", "modelscope-ai")
	want := []string{"modelscope-k1", "modelscope-k2", "modelscope-ai-k1", "modelscope-k3"}
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
