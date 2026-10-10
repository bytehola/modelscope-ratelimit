package main

import (
	"strings"
	"testing"
	"time"

	modelscope "modelscope-ratelimit/internal/ratelimit"
)

// TestUnauthorizedMaskedKeys 守住"密钥失效集合"的来源：只有 unauthorized 进集合，日禁用的
// exhausted / unverified 不进 —— 前者的余额探测正是"已恢复"的判定手段，必须继续打。
func TestUnauthorizedMaskedKeys(t *testing.T) {
	seedAccountsCache(t,
		accountSummary{MaskedKey: "masked-live", Schedule: "normal"},
		accountSummary{MaskedKey: "masked-dead", Schedule: "unauthorized"},
		accountSummary{MaskedKey: "masked-empty", Schedule: "exhausted"},
		accountSummary{MaskedKey: "masked-unverified", Schedule: "unverified"},
	)

	dead := unauthorizedMaskedKeys()
	if len(dead) != 1 {
		t.Fatalf("unauthorizedMaskedKeys() = %v, want only the unauthorized account", dead)
	}
	if _, ok := dead["masked-dead"]; !ok {
		t.Fatalf("unauthorizedMaskedKeys() = %v, want masked-dead", dead)
	}
}

// TestRefreshAccountsCacheSkipsUnauthorizedAccounts 守住判死的凭据不再被后台探测：每 30 分钟的
// 缓存刷新不该再打密钥失效账号的余额与限流，直接把现有快照沿用下去。要重探只能走全量取数
// （reconfigure，或管理中心"全部签到"触发的 clearAllCaches + 全量刷新）。
func TestRefreshAccountsCacheSkipsUnauthorizedAccounts(t *testing.T) {
	originalStore := store
	store = modelscope.NewStore(stderrLogger{})
	originalCfg := pluginCfg
	originalResolved := getResolved()
	t.Cleanup(func() {
		store = originalStore
		pluginCfg = originalCfg
		resolvedMu.Lock()
		resolvedCache = originalResolved
		resolvedMu.Unlock()
	})

	const (
		liveAuthID  = "a1"
		liveRawKey  = "ms-key-live-AAAA"
		deadAuthID  = "a2"
		deadRawKey  = "ms-key-dead-BBBB"
		alphaSite   = "https://alpha.example"
		deadBalance = 42
		liveBalance = 77
	)
	routes := map[string]map[string]string{"alpha": {"probe-model": "vendor/Probe-Model"}}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		masked:         map[string]string{liveAuthID: maskKey(liveRawKey), deadAuthID: maskKey(deadRawKey)},
		raw:            map[string]string{liveAuthID: liveRawKey, deadAuthID: deadRawKey},
		siteBase:       map[string]string{liveAuthID: alphaSite, deadAuthID: alphaSite},
		baseURL:        map[string]string{liveAuthID: alphaSite + "/v1", deadAuthID: alphaSite + "/v1"},
		routeToName:    routes,
		nameToRoute:    withRouteIndex(routes),
		providerOrder:  map[string][]string{"alpha": {liveAuthID, deadAuthID}},
		dedupedAuthIDs: []string{liveAuthID, deadAuthID},
		at:             time.Now(),
	}
	resolvedMu.Unlock()
	pluginCfg = &modelscope.Config{
		Providers:     []string{"alpha"},
		HostBaseURL:   "http://127.0.0.1:1",
		ManagementKey: "test-management-key",
	}
	store.Reconfigure(pluginCfg)

	probes := stubRefreshBalanceHTTP(t, liveBalance, 100)
	seedAccountsCache(t,
		accountSummary{MaskedKey: maskKey(liveRawKey), Schedule: "normal", Balance: &balanceJSON{Total: 100, Available: 1}},
		accountSummary{MaskedKey: maskKey(deadRawKey), Schedule: "unauthorized", Balance: &balanceJSON{Total: 100, Available: deadBalance}},
	)

	fresh := refreshAccountsCache(getResolved())

	seen := probes.snapshot()
	for _, cookie := range seen {
		if strings.Contains(cookie, deadRawKey) {
			t.Fatalf("密钥失效的账号不该再被后台探测，实际请求: %s", cookie)
		}
	}
	if len(seen) == 0 {
		t.Fatal("仍然可用的账号必须照常刷新余额")
	}
	for _, cookie := range seen {
		if !strings.Contains(cookie, liveRawKey) {
			t.Fatalf("只有可用账号该被探测，实际请求: %s", cookie)
		}
	}

	live := accountByMask(t, fresh, maskKey(liveRawKey))
	if live.Schedule != "normal" || live.Balance == nil || live.Balance.Available != liveBalance {
		t.Fatalf("可用账号应拿到新余额: %+v", live)
	}
	dead := accountByMask(t, fresh, maskKey(deadRawKey))
	if dead.Schedule != "unauthorized" {
		t.Fatalf("密钥失效状态被改动: %q, want %q", dead.Schedule, "unauthorized")
	}
	if dead.Balance == nil || dead.Balance.Available != deadBalance {
		t.Fatalf("密钥失效账号应沿用现有快照: %+v", dead.Balance)
	}
}

func accountByMask(t *testing.T, accounts []accountSummary, maskedKey string) accountSummary {
	t.Helper()
	for _, account := range accounts {
		if account.MaskedKey == maskedKey {
			return account
		}
	}
	t.Fatalf("account %s not found in refreshed accounts cache", maskedKey)
	return accountSummary{}
}
