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

// TestAccountScheduleReconciliation 守住账号页状态必须以核心层当前禁用事实为准，同时余额读取
// 失败不能把已经确认的 exhausted 擅自恢复。
func TestAccountScheduleReconciliation(t *testing.T) {
	originalStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = originalStore })

	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, loc)
	store.SetClock(func() time.Time { return now })
	store.SetLocation(loc)
	store.Reconfigure(&modelscope.Config{Providers: []string{"modelscope"}, Timezone: "Asia/Shanghai"})
	store.ApplyRateLimit("auth-unverified", "model", nil, 403, unverifiedAccountBody, now)
	store.ApplyRateLimit("auth-exhausted", "model", nil, 429, `{"error":{"message":"insufficient balance"}}`, now)

	cases := []struct {
		name      string
		authID    string
		current   string
		available int
		balanceOK bool
		want      string
	}{
		{"余额归零记 exhausted", "auth-normal", "normal", 0, true, "exhausted"},
		{"余额恢复把 exhausted 改回 normal", "auth-normal", "exhausted", 12, true, "normal"},
		{"正常账号保持 normal", "auth-normal", "normal", 12, true, "normal"},
		{"密钥失效不被余额结果改写", "auth-normal", "unauthorized", 12, true, "unauthorized"},
		{"核心未验证状态压过正余额", "auth-unverified", "unverified", 500, true, "unverified"},
		{"核心未验证状态压过零余额", "auth-unverified", "unverified", 0, true, "unverified"},
		{"核心日禁用压过余额恢复", "auth-exhausted", "normal", 500, true, "exhausted"},
		{"余额失败保留已确认 exhausted", "auth-normal", "exhausted", 0, false, "exhausted"},
		{"余额失败清除已过期的 unverified", "auth-normal", "unverified", 0, false, "normal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ""
			if c.balanceOK {
				got = scheduleAfterBalance(c.authID, c.current, c.available, now)
			} else {
				got = scheduleAfterBalanceFailure(c.authID, c.current, now)
			}
			if got != c.want {
				t.Fatalf("schedule = %q, want %q", got, c.want)
			}
		})
	}

	nextDay := now.Add(24 * time.Hour)
	if got := scheduleAfterBalance("auth-unverified", "unverified", 500, nextDay); got != "normal" {
		t.Fatalf("次日未验证状态 = %q, want normal", got)
	}
	if disabled, _ := store.GlobalDisableState("auth-unverified", nextDay); disabled {
		t.Fatal("固定时钟测试夹具的次日核心状态应已过期")
	}
}

func TestStaleAccountsSnapshotClearsExpiredUnverified(t *testing.T) {
	originalStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = originalStore })

	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, loc)
	store.SetClock(func() time.Time { return now })
	store.SetLocation(loc)
	store.Reconfigure(&modelscope.Config{Providers: []string{"modelscope"}, Timezone: "Asia/Shanghai"})
	store.ApplyRateLimit("auth-unverified", "model", nil, 403, unverifiedAccountBody, now)

	accountOf := seedAccountsCache(t, accountSummary{
		authID:    "auth-unverified",
		MaskedKey: "masked-unverified",
		Schedule:  "unverified",
		Balance:   &balanceJSON{Total: 100, Available: 42},
	})

	store.SetClock(func() time.Time { return now.Add(24 * time.Hour) })
	snapshot := getStaleAccountsCache()
	if got := snapshot[0].Schedule; got != "normal" {
		t.Fatalf("expired unverified display = %q, want normal", got)
	}
	if got := accountOf("masked-unverified").Schedule; got != "unverified" {
		t.Fatalf("raw cache should stay immutable until refresh, got %q", got)
	}
}

func TestFetchAccountBalanceOnlyKeepsExhaustedOnMalformedBalance(t *testing.T) {
	originalStore := store
	store = modelscope.NewStore(stderrLogger{})
	t.Cleanup(func() { store = originalStore })
	originalClient := msHTTPClient.Load()
	msHTTPClient.Store(&http.Client{
		Transport: refreshBalanceRoundTrip(func(req *http.Request) (*http.Response, error) {
			body := `{"Success":true,"Data":{"currentUsagePerDay":0}}`
			if strings.Contains(req.URL.Path, "/magicubes/balance") {
				body = `{"Success":true,"Data":"not-a-balance-object"}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		}),
	})
	t.Cleanup(func() { msHTTPClient.Store(originalClient) })

	client := &msClient{}
	account := client.fetchAccountBalanceOnly(
		0,
		"auth-malformed-balance",
		"key-malformed-balance",
		"https://alpha.example",
		"cn",
		&accountSummary{Schedule: "exhausted", Balance: &balanceJSON{Total: 100, Available: 42}},
	)
	if account.Schedule != "exhausted" {
		t.Fatalf("malformed balance response changed schedule to %q, want exhausted", account.Schedule)
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
	return stubRefreshBalanceHTTPWithHook(t, available, total, nil)
}

func stubRefreshBalanceHTTPWithHook(t *testing.T, available, total int, onBalance func()) *probeLog {
	t.Helper()
	probes := &probeLog{}
	original := msHTTPClient.Load()
	msHTTPClient.Store(&http.Client{
		Transport: refreshBalanceRoundTrip(func(req *http.Request) (*http.Response, error) {
			probes.record(req.Header.Get("Cookie"))
			var body string
			switch {
			case strings.Contains(req.URL.Path, "/users/login/info"):
				body = `{"Success":true,"Data":{"Name":"Probe Account","NickName":"Probe","Email":"probe@example.com"}}`
			case strings.Contains(req.URL.Path, "/users/info"):
				body = `{"Success":true,"Data":{"PhoneNo":"13800000000"}}`
			case strings.Contains(req.URL.Path, "/magicubes/balance"):
				if onBalance != nil {
					onBalance()
				}
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
		masked:         map[string]string{authID: maskKey(rawKey)},
		raw:            map[string]string{authID: rawKey},
		maskToRaw:      map[string]string{maskKey(rawKey): rawKey},
		maskToSite:     map[string]string{maskKey(rawKey): siteBase},
		maskToAuthID:   map[string]string{maskKey(rawKey): authID},
		siteBase:       map[string]string{authID: siteBase},
		baseURL:        map[string]string{authID: siteBase + "/v1"},
		routeToName:    routes,
		nameToRoute:    withRouteIndex(routes),
		dedupedAuthIDs: []string{authID},
		providerOrder:  map[string][]string{"alpha": {authID}},
		at:             time.Now(),
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

	cases := []struct {
		name              string
		authID            string
		seeded            string
		available         int
		coreFailureStatus int
		want              string
	}{
		{"核心未验证邮箱不能被余额结果改写", "a-unverified", "unverified", 77, 403, "unverified"},
		{"余额归零记 exhausted", "a-zero", "normal", 0, 0, "exhausted"},
		{"余额恢复把 exhausted 改回 normal", "a-recovered", "exhausted", 77, 0, "normal"},
		{"核心日禁用不能在午夜前被余额恢复", "a-exhausted", "normal", 77, 429, "exhausted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const (
				rawKey   = "key-refresh-balance-probe"
				siteBase = "https://alpha.example"
			)
			setTestRefreshBalanceFixture(t, c.authID, rawKey, siteBase)
			if c.coreFailureStatus == 403 {
				store.ApplyRateLimit(c.authID, "probe-model", nil, 403, unverifiedAccountBody, store.Now())
			}
			if c.coreFailureStatus == 429 {
				store.ApplyRateLimit(c.authID, "probe-model", nil, 429, `{"error":{"message":"insufficient balance"}}`, store.Now())
			}
			maskedKey := maskKey(rawKey)
			accountOf := seedAccountsCache(t, accountSummary{
				authID:    c.authID,
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

func TestRefreshAccountsCacheAppliesCoreDisableToNewSnapshot(t *testing.T) {
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
		authID   = "a-new-cache"
		rawKey   = "key-new-cache"
		siteBase = "https://alpha.example"
	)
	setTestRefreshBalanceFixture(t, authID, rawKey, siteBase)
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, loc)
	store.SetClock(func() time.Time { return now })
	store.SetLocation(loc)
	clearAccountsCache()
	stubRefreshBalanceHTTP(t, 77, 100)

	processUsageRecord(modelscope.UsageRecord{
		Provider: "openai-compatible-alpha",
		AuthID:   authID,
		Model:    "vendor/Probe-Model",
		Alias:    "probe-model",
		Failed:   true,
		Failure:  &modelscope.UsageFailure{StatusCode: 403, Body: unverifiedAccountBody},
	})

	fresh := refreshAccountsCache(getResolved())
	account := accountByMask(t, fresh, maskKey(rawKey))
	if account.Schedule != "unverified" {
		t.Fatalf("new cache schedule = %q, want unverified", account.Schedule)
	}
}

func TestRefreshAccountsCacheMergeKeepsConcurrentDisable(t *testing.T) {
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
		authID   = "a-concurrent"
		rawKey   = "key-concurrent"
		siteBase = "https://alpha.example"
	)
	setTestRefreshBalanceFixture(t, authID, rawKey, siteBase)
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, loc)
	store.SetClock(func() time.Time { return now })
	store.SetLocation(loc)
	maskedKey := maskKey(rawKey)
	accountOf := seedAccountsCache(t, accountSummary{
		authID:    authID,
		MaskedKey: maskedKey,
		Schedule:  "normal",
		Balance:   &balanceJSON{Total: 100, Available: 42},
	})

	balanceStarted := make(chan struct{})
	releaseBalance := make(chan struct{})
	var once sync.Once
	stubRefreshBalanceHTTPWithHook(t, 77, 100, func() {
		once.Do(func() { close(balanceStarted) })
		<-releaseBalance
	})

	done := make(chan []accountSummary, 1)
	go func() {
		done <- refreshAccountsCache(getResolved())
	}()
	<-balanceStarted
	processUsageRecord(modelscope.UsageRecord{
		Provider: "openai-compatible-alpha",
		AuthID:   authID,
		Model:    "vendor/Probe-Model",
		Alias:    "probe-model",
		Failed:   true,
		Failure:  &modelscope.UsageFailure{StatusCode: 403, Body: unverifiedAccountBody},
	})
	close(releaseBalance)

	fresh := <-done
	if account := accountByMask(t, fresh, maskedKey); account.Schedule != "unverified" {
		t.Fatalf("refresh result schedule = %q, want unverified", account.Schedule)
	}
	if account := accountOf(maskedKey); account.Schedule != "unverified" {
		t.Fatalf("cache schedule = %q, want unverified", account.Schedule)
	}
}
