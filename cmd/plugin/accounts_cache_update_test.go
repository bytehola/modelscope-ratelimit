package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	modelscope "modelscope-ratelimit/internal/ratelimit"
)

// TestNextSchedule 守住"由余额推导调度状态"这一条规则，它是余额刷新与 refresh-balance 共用的
// 唯一实现。未验证邮箱与密钥失效都不由余额决定，必须原样保留。
func TestNextSchedule(t *testing.T) {
	cases := []struct {
		name      string
		current   string
		available int
		want      string
	}{
		{"未验证邮箱在余额归零时保留", "unverified", 0, "unverified"},
		{"未验证邮箱在余额充足时保留", "unverified", 500, "unverified"},
		{"余额归零记 exhausted", "normal", 0, "exhausted"},
		{"余额恢复把 exhausted 改回 normal", "exhausted", 12, "normal"},
		{"正常账号保持 normal", "normal", 12, "normal"},
		{"密钥失效不被余额结果改写", "unauthorized", 12, "unauthorized"},
		{"密钥失效在余额归零时也不被改写", "unauthorized", 0, "unauthorized"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nextSchedule(c.current, c.available); got != c.want {
				t.Fatalf("nextSchedule(%q, %d) = %q, want %q", c.current, c.available, got, c.want)
			}
		})
	}
}

// TestUpdateAccountByMaskKeepsHandedOutSnapshotImmutable 守住 copy-on-write：状态页拿到
// accountsCache.results 之后在锁外 json.Marshal，所以交出去的切片不能再被标记写入改动。
func TestUpdateAccountByMaskKeepsHandedOutSnapshotImmutable(t *testing.T) {
	accountOf := seedAccountsCache(t, accountSummary{
		MaskedKey: "probe-masked-key",
		Schedule:  "normal",
		Balance:   &balanceJSON{Total: 100, Available: 42},
	})

	snapshot := getStaleAccountsCache()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot length = %d, want 1", len(snapshot))
	}

	markAccountUnverified("probe-masked-key")
	markAccountExhausted("probe-masked-key")

	if snapshot[0].Schedule != "normal" {
		t.Fatalf("已交出的快照被就地改写: schedule = %q, want %q", snapshot[0].Schedule, "normal")
	}
	if snapshot[0].Balance == nil || snapshot[0].Balance.Available != 42 {
		t.Fatalf("已交出的快照余额被就地改写: %+v, want available 42", snapshot[0].Balance)
	}

	state := accountOf("probe-masked-key")
	if state.Schedule != "exhausted" {
		t.Fatalf("缓存里的 schedule = %q, want %q", state.Schedule, "exhausted")
	}
	if state.Balance == nil || state.Balance.Available != 0 {
		t.Fatalf("缓存里的余额 = %+v, want available 0", state.Balance)
	}
}

// TestUpdateAccountByMaskDeepCopiesBalance 守住助手的另一半承诺：mutate 里原地改 Balance 字段
// 时，已交出的快照同样不能被影响。调用方只有记得替换指针才安全的话，这个契约太脆。
func TestUpdateAccountByMaskDeepCopiesBalance(t *testing.T) {
	accountOf := seedAccountsCache(t, accountSummary{
		MaskedKey: "probe-masked-key",
		Schedule:  "normal",
		Balance:   &balanceJSON{Total: 100, Available: 42},
	})

	snapshot := getStaleAccountsCache()
	updateAccountByMask("probe-masked-key", func(account *accountSummary) {
		account.Balance.Available = 0
		account.Schedule = "exhausted"
	})

	if snapshot[0].Balance.Available != 42 {
		t.Fatalf("原地改 Balance 影响了已交出的快照: %+v", snapshot[0].Balance)
	}
	if state := accountOf("probe-masked-key"); state.Balance.Available != 0 {
		t.Fatalf("更新没有落到缓存上: %+v", state.Balance)
	}
}

// refreshBalanceRoundTrip 让测试用固定响应替代真实出站请求。
type refreshBalanceRoundTrip func(*http.Request) (*http.Response, error)

func (roundTrip refreshBalanceRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	return roundTrip(req)
}

// probeLog 记录打桩客户端收到的探测请求，供测试断言哪些账号真的被打了。账号缓存刷新与
// refresh-balance 都是并发发请求，所以写方要自带锁。
type probeLog struct {
	mu      sync.Mutex
	cookies []string
}

func (l *probeLog) record(cookie string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cookies = append(l.cookies, cookie)
}

// snapshot 返回已记录 Cookie 的副本，调用方读的时候不会再与写方竞争。
func (l *probeLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.cookies))
	copy(out, l.cookies)
	return out
}

// stubRefreshBalanceHTTP 让 msClient 的余额与流水两次 GET 返回固定数据，并返回它收到的探测请求。
// 探测请求的 Cookie 里带着被探测的原始 session id，调用方据此断言哪些账号真的被打了。
func stubRefreshBalanceHTTP(t *testing.T, available, total int) *probeLog {
	t.Helper()
	probes := &probeLog{}
	original := msHTTPClient.Load()
	msHTTPClient.Store(&http.Client{
		Transport: refreshBalanceRoundTrip(func(req *http.Request) (*http.Response, error) {
			probes.record(req.Header.Get("Cookie"))
			var body string
			switch {
			case strings.Contains(req.URL.Path, "/magicubes/balance"):
				body = fmt.Sprintf(`{"Success":true,"Data":{"total_balance":%d,"available_balance":%d,"frozen_amount":0}}`, total, available)
			case strings.Contains(req.URL.Path, "/magicubes/transactions"):
				body = `{"Success":true,"Data":{"page":1,"page_size":50,"total":0,"records":[]}}`
			case strings.Contains(req.URL.Path, "/inference/rate-limit"):
				body = `{"Success":true,"Data":{"currentUsagePerDay":0}}`
			default:
				return nil, fmt.Errorf("测试桩收到未预期的请求: %s", req.URL.Path)
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		}),
	})
	t.Cleanup(func() { msHTTPClient.Store(original) })
	return probes
}

// setTestRefreshBalanceFixture 准备 refresh-balance 需要的配置与解析视图（掩码 key 到原始
// session id 的映射）。
func setTestRefreshBalanceFixture(t *testing.T, authID, rawKey, siteBase string) {
	t.Helper()
	routes := map[string]map[string]string{"alpha": {"probe-model": "vendor/Probe-Model"}}
	resolvedMu.Lock()
	resolvedCache = resolvedEntry{
		masked:        map[string]string{authID: maskKey(rawKey)},
		raw:           map[string]string{authID: rawKey},
		maskToRaw:     map[string]string{maskKey(rawKey): rawKey},
		maskToSite:    map[string]string{maskKey(rawKey): siteBase},
		siteBase:      map[string]string{authID: siteBase},
		baseURL:       map[string]string{authID: siteBase + "/v1"},
		routeToName:   routes,
		nameToRoute:   withRouteIndex(routes),
		providerOrder: map[string][]string{"alpha": {authID}},
		at:            time.Now(),
	}
	resolvedMu.Unlock()
	pluginCfg = &modelscope.Config{
		Providers:     []string{"alpha"},
		HostBaseURL:   "http://127.0.0.1:1",
		ManagementKey: "test-management-key",
	}
	store.Reconfigure(pluginCfg)
}

// TestRefreshBalanceKeepsUnverifiedSchedule 覆盖账号详情的"刷新余额"按钮所走的管理接口：它按
// 余额推导 schedule 时必须与余额刷新共用同一份规则，未验证邮箱不能被盖成 normal，否则点一次
// 刷新，徽标就会回到"正常调度"而该 key 当天仍被禁用。
func TestRefreshBalanceKeepsUnverifiedSchedule(t *testing.T) {
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
		authID   = "a1"
		rawKey   = "key-refresh-balance-probe"
		siteBase = "https://alpha.example"
	)
	setTestRefreshBalanceFixture(t, authID, rawKey, siteBase)
	maskedKey := maskKey(rawKey)

	cases := []struct {
		name      string
		seeded    string
		available int
		want      string
	}{
		{"未验证邮箱不能被余额结果改写", "unverified", 77, "unverified"},
		{"余额归零记 exhausted", "normal", 0, "exhausted"},
		{"余额恢复把 exhausted 改回 normal", "exhausted", 77, "normal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			accountOf := seedAccountsCache(t, accountSummary{
				MaskedKey: maskedKey,
				Schedule:  c.seeded,
				Balance:   &balanceJSON{Total: 100, Available: 42},
			})
			stubRefreshBalanceHTTP(t, c.available, 100)

			resp := handleStatusAPI(managementRequest{
				Method: "GET",
				Query:  map[string][]string{"key": {maskedKey}},
			}, "refresh-balance")
			if resp.StatusCode != 200 {
				t.Fatalf("refresh-balance status = %d, body = %s", resp.StatusCode, string(resp.Body))
			}

			state := accountOf(maskedKey)
			if state.Schedule != c.want {
				t.Fatalf("schedule = %q, want %q", state.Schedule, c.want)
			}
			if state.Balance == nil || state.Balance.Available != c.available {
				t.Fatalf("balance = %+v, want available %d", state.Balance, c.available)
			}
		})
	}
}
