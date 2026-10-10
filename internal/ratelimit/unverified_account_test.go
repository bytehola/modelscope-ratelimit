package ratelimit

import (
	"testing"
	"time"
)

// unverifiedAccountBody 是线上实测的 403 文案：账号没完成邮箱验证时，魔粒推理接口直接拒绝服务。
const unverifiedAccountBody = `{"error":{"message":"For account security, please complete your profile via email verification. ModelScope Magicubes is only available to verified accounts. Please refer to https://modelscope.cn/my/settings/account","request_id":"dadfecb2-6079-4a49-a1b8-922232b6084c"}}`

func TestIsUnverifiedAccount(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		want       bool
	}{
		{"线上实测文案", 403, unverifiedAccountBody, true},
		{"大小写不敏感", 403, `{"error":{"message":"Please complete your profile via EMAIL VERIFICATION."}}`, true},
		{"只认 403", 429, unverifiedAccountBody, false},
		{"401 归密钥失效", 401, unverifiedAccountBody, false},
		{"403 但没有特征串", 403, `{"error":{"message":"permission denied"}}`, false},
		{"空 body 不误判", 403, "", false},
		{"魔粒耗尽文案不误判", 403, insufficientBalanceBody, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsUnverifiedAccount(c.statusCode, c.body); got != c.want {
				t.Fatalf("IsUnverifiedAccount(%d, %q) = %v, want %v", c.statusCode, c.body, got, c.want)
			}
		})
	}
}

// TestApplyRateLimitUnverifiedAccountDisablesForDay 守住与魔粒耗尽一致的路径：403 + 未验证邮箱
// 让该凭据当日全局禁用，状态页能看到原因，午夜由 PruneAll 恢复。
func TestApplyRateLimitUnverifiedAccountDisablesForDay(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, loc)
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 403, unverifiedAccountBody, now)

	// 全局禁用会挡住该 key 上的所有模型，且不牵连别的凭据。
	for _, model := range []string{"Qwen-72B", "DeepSeek-V3"} {
		if !s.isDisabled("auth-1", model, now) {
			t.Fatalf("expected %s disabled by unverified account", model)
		}
	}
	if s.isDisabled("auth-2", "Qwen-72B", now) {
		t.Fatal("global disable must be scoped to the owning key")
	}
	view := s.Snapshot(now)["auth-1"]
	if !view.GlobalDisabled || view.GlobalReason != ReasonUnverified {
		t.Fatalf("snapshot = %+v, want a global disable with reason %q", view, ReasonUnverified)
	}

	// 与魔粒耗尽一样属于日禁用：跨过午夜就重新可调度，而不是扛到插件重启。
	nextDay := time.Date(2026, 9, 22, 0, 5, 0, 0, loc)
	s.PruneAll(nextDay)
	if s.isDisabled("auth-1", "Qwen-72B", nextDay) {
		t.Fatal("unverified account disable must be cleared at midnight")
	}
}

// TestApplyRateLimitUnverifiedKeepsEarliestTimestamp 同日重复命中保留最早的时间与最初的原因，
// 避免后到的失败把禁用时间一路推后。
func TestApplyRateLimitUnverifiedKeepsEarliestTimestamp(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, loc)
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 403, unverifiedAccountBody, now)
	later := now.Add(2 * time.Hour)
	// 后到的魔粒耗尽不该覆盖已经记录的原因与时间。
	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 429, insufficientBalanceBody, later)

	view := s.Snapshot(later)["auth-1"]
	if view.GlobalReason != ReasonUnverified {
		t.Fatalf("global reason = %q, want %q", view.GlobalReason, ReasonUnverified)
	}
	if !view.GlobalSince.Equal(now) {
		t.Fatalf("global since = %s, want the earliest hit %s", view.GlobalSince, now)
	}
}

// TestApplyRateLimitUnverifiedIgnoresOtherFailures 确认新判据不会误伤：状态码不对、或 403 但没有
// 特征串时，不允许禁用任何凭据。
func TestApplyRateLimitUnverifiedIgnoresOtherFailures(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, loc)
	s, _ := newTestStore(now)
	s.Reconfigure(cfgWith("modelscope"))

	s.ApplyRateLimit("auth-1", "Qwen-72B", nil, 403, `{"error":{"message":"permission denied"}}`, now)
	s.ApplyRateLimit("auth-2", "Qwen-72B", nil, 429, unverifiedAccountBody, now)

	if s.isDisabled("auth-1", "Qwen-72B", now) {
		t.Fatal("a 403 without the verification wording must not disable the key")
	}
	if s.isDisabled("auth-2", "Qwen-72B", now) {
		t.Fatal("only 403 marks an unverified account")
	}
}
