package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modelscope-ratelimit/internal/ratelimit"
)

// msClient 用 api key 直连 Modelscope API，把它作为 m_session_id cookie 的值。这把 key 本身就
// 是 cookie，永不过期。
type msClient struct {
	httpClient *http.Client
}

var msHTTPClient atomic.Pointer[http.Client]

func init() {
	msHTTPClient.Store(&http.Client{
		Timeout:   15 * time.Second,
		Transport: newMSTransport(nil),
	})
}

// newMSTransport 构造一个带合理连接池参数的 http.Transport。高并发下（20 个 worker 乘 3 个协程
// 等于 60 个并发请求）Go 默认的 MaxIdleConnsPerHost=2 会造成过多的 TCP 建拆，可能把调试代理
// （ms_proxy_url）压垮。每主机最多保留 30 条空闲连接对受信号量限制的 worker 已经够用，也不会因为走代理而把
// 临时端口耗尽。
func newMSTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	return &http.Transport{
		Proxy:               proxy,
		MaxIdleConnsPerHost: 30,
		MaxConnsPerHost:     30,
	}
}

// SetMSProxy 让 Modelscope API 的 HTTP 客户端改走代理。当 config 里设置了 ms_proxy_url 时由
// configure() 调用。
func SetMSProxy(proxyURL string) {
	if proxyURL == "" {
		msHTTPClient.Store(&http.Client{
			Timeout:   15 * time.Second,
			Transport: newMSTransport(nil),
		})
		return
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return
	}
	msHTTPClient.Store(&http.Client{
		Timeout:   15 * time.Second,
		Transport: newMSTransport(http.ProxyURL(u)),
	})
}

// siteBaseFromURL 从 provider 的 base-url 推导 Modelscope 站点基础地址：modelscope.cn 是国内站
// 点，modelscope.ai 是国际站点。
func siteBaseFromURL(baseURL string) string {
	if strings.Contains(baseURL, "modelscope.ai") {
		return "https://www.modelscope.ai"
	}
	return "https://modelscope.cn"
}

// getWithRetry 调用 get()，并在 429（Too Many Requests）和网络错误时重试。重试预算是 5 次，退避
// 带抖动。网络错误（err != nil 且 sc == 0）用比 429 限流更短的退避（0.5-1 秒 对 2-5 秒），因为走
// 代理时的连接故障通常是瞬时的、会很快恢复。
func (c *msClient) getWithRetry(siteBase, path, sessionID string) (int, json.RawMessage, error) {
	for attempt := 0; attempt < 5; attempt++ {
		sc, data, err := c.get(siteBase, path, sessionID)
		// 成功：立刻返回。
		if err == nil && sc != 429 {
			return sc, data, err
		}
		// 退避：429 由服务端强制，等得更久；网络错误等得更短。
		var delay time.Duration
		if sc == 429 {
			delay = time.Duration(2+rand.Intn(4)) * time.Second
		} else {
			delay = time.Duration(500+rand.Intn(500)) * time.Millisecond
		}
		time.Sleep(delay)
	}
	// 最后一次尝试，不再重试
	return c.get(siteBase, path, sessionID)
}

func (c *msClient) get(siteBase, path, sessionID string) (int, json.RawMessage, error) {
	req, err := http.NewRequest("GET", siteBase+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Cookie", "m_session_id="+sessionID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := msHTTPClient.Load().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ar, err := ratelimit.NormalizeAPIResponse(body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, ar.Data, nil
}

// ---- 取数闭包实现（注入给 Store）----

func (c *msClient) loginInfoFetcher(siteBase, sessionID string) ratelimit.LoginInfoFetcher {
	return func(sid string) (*ratelimit.LoginInfo, error) {
		sc, data, err := c.get(siteBase, "/api/v1/users/login/info", sid)
		if err != nil || sc != 200 {
			return nil, fmt.Errorf("login/info: status=%d err=%v", sc, err)
		}
		var li ratelimit.LoginInfo
		if err := json.Unmarshal(data, &li); err != nil {
			return nil, fmt.Errorf("login/info decode: %w", err)
		}
		return &li, nil
	}
}

func (c *msClient) balanceFetcher(siteBase string) ratelimit.BalanceFetcher {
	return func(sid string) (*ratelimit.MagicCubeBalance, error) {
		sc, data, err := c.get(siteBase, "/openapi/v1/magicubes/balance", sid)
		if err != nil || sc != 200 {
			return nil, fmt.Errorf("balance: status=%d err=%v", sc, err)
		}
		var b ratelimit.MagicCubeBalance
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, fmt.Errorf("balance decode: %w", err)
		}
		return &b, nil
	}
}

func (c *msClient) rateLimitFetcher(siteBase string) ratelimit.RateLimitFetcher {
	return func(sid string) (*ratelimit.InferenceRateLimit, error) {
		sc, data, err := c.get(siteBase, "/api/v1/inference/rate-limit", sid)
		if err != nil || sc != 200 {
			return nil, fmt.Errorf("rate-limit: status=%d err=%v", sc, err)
		}
		var rl ratelimit.InferenceRateLimit
		if err := json.Unmarshal(data, &rl); err != nil {
			return nil, fmt.Errorf("rate-limit decode: %w", err)
		}
		return &rl, nil
	}
}

// ---- 管理接口端点（供前端 XHR 调用）----

type accountSummary struct {
	Index         int          `json:"index"`
	MaskedKey     string       `json:"maskedKey"`
	Phone         string       `json:"phone,omitempty"`
	Site          string       `json:"site"`
	Name          string       `json:"name"`
	NickName      string       `json:"nickName"`
	Email         string       `json:"email"`
	HavanaID      string       `json:"havanaId"`
	FromSite      string       `json:"fromSite"`
	IsRealName    bool         `json:"isRealName"`
	CheckedIn     bool         `json:"checkedIn"`
	LastCheckIn   string       `json:"lastCheckIn"`
	Balance       *balanceJSON `json:"balance"`
	UsagePerDay   int          `json:"usagePerDay"`
	TodayConsumed int          `json:"todayConsumed"`
	Schedule      string       `json:"schedule"`
	Error         string       `json:"error,omitempty"`
}

type balanceJSON struct {
	Total     int `json:"total"`
	Available int `json:"available"`
	Frozen    int `json:"frozen"`
}

type detailJSON struct {
	Index         int         `json:"index"`
	Name          string      `json:"name"`
	NickName      string      `json:"nickName"`
	Email         string      `json:"email"`
	Phone         string      `json:"phone"`
	HavanaID      string      `json:"havanaId"`
	FromSite      string      `json:"fromSite"`
	IsRealName    bool        `json:"isRealName"`
	Org           string      `json:"org"`
	School        string      `json:"school"`
	Industry      string      `json:"industry"`
	UsagePerDay   int         `json:"usagePerDay"`
	TodayConsumed int         `json:"todayConsumed"`
	SdkToken      string      `json:"sdkToken"`
	AliyunBound   bool        `json:"aliyunBound"`
	AliyunAID     string      `json:"aliyunAid"`
	AliyunNick    string      `json:"aliyunNick"`
	Site          string      `json:"site"`
	Tokens        []tokenJSON `json:"tokens"`
}

type tokenJSON struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Token   string `json:"token"`
	Valid   bool   `json:"valid"`
	Expires string `json:"expires"`
}

// fetchLoginInfo 调用 login/info（每日签到）和 users/info（手机号）。返回的 accountSummary 只填了
// 资料部分，余额、限流和流水在第 2 阶段单独拉取，以便所有账号先完成签到再读余额。
func (c *msClient) fetchLoginInfo(idx int, sessionID, siteBase, site string) accountSummary {
	a := accountSummary{Index: idx, MaskedKey: maskKey(sessionID), Site: site, Schedule: "normal"}

	// 并发调用 login/info 与 users/info
	var (
		liSc, uiSc   int
		liRaw, uiRaw []byte
		liErr, uiErr error
		wg1          sync.WaitGroup
	)
	wg1.Add(2)
	go func() {
		defer wg1.Done()
		liSc, liRaw, liErr = c.getWithRetry(siteBase, "/api/v1/users/login/info", sessionID)
	}()
	go func() {
		defer wg1.Done()
		uiSc, uiRaw, uiErr = c.getWithRetry(siteBase, "/api/v1/users/info", sessionID)
	}()
	wg1.Wait()

	if liErr == nil && liSc == 200 {
		var li ratelimit.LoginInfo
		if json.Unmarshal(liRaw, &li) == nil {
			a.Name = li.Name
			a.NickName = li.NickName
			a.Email = li.Email
			a.HavanaID = li.HavanaId
			a.FromSite = li.FromSite
			a.IsRealName = li.IsRealName
			a.CheckedIn = true
			a.LastCheckIn = time.Now().Format("2006-01-02 15:04:05")
		}
	} else if liSc == 401 || liSc == 403 {
		a.Schedule = "unauthorized"
		a.Error = fmt.Sprintf("login/info: %d", liSc)
		return a
	}

	// 用 users/info 的结果填手机号
	if uiErr == nil && uiSc == 200 {
		var ui ratelimit.UserInfo
		if json.Unmarshal(uiRaw, &ui) == nil {
			a.Phone = ui.PhoneNo
		}
	}

	return a
}

// fetchBalanceData 并发拉取单个账号的余额、限流和流水。它在所有账号都完成 login/info（签到）之后的
// 第 2 阶段被调用，因此余额能反映签到后的魔粒额度。
func (c *msClient) fetchBalanceData(a *accountSummary, sessionID, siteBase string) {
	var (
		bSc, rlSc, txSc    int
		bRaw, rlRaw, txRaw []byte
		bErr, rlErr, txErr error
		wg                 sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		bSc, bRaw, bErr = c.getWithRetry(siteBase, "/openapi/v1/magicubes/balance", sessionID)
	}()
	go func() {
		defer wg.Done()
		rlSc, rlRaw, rlErr = c.getWithRetry(siteBase, "/api/v1/inference/rate-limit", sessionID)
	}()
	go func() {
		defer wg.Done()
		txSc, txRaw, txErr = c.getWithRetry(siteBase, "/openapi/v1/magicubes/transactions?type=SPEND_CONFIRM&page=1&page_size=50", sessionID)
	}()
	wg.Wait()

	// 处理余额
	if bErr == nil && bSc == 200 {
		var b ratelimit.MagicCubeBalance
		if json.Unmarshal(bRaw, &b) == nil {
			a.Balance = &balanceJSON{Total: b.TotalBalance, Available: b.AvailableBalance, Frozen: b.FrozenAmount}
		}
	}
	// 处理限流
	if rlErr == nil && rlSc == 200 {
		var rl ratelimit.InferenceRateLimit
		if json.Unmarshal(rlRaw, &rl) == nil {
			a.UsagePerDay = rl.CurrentUsagePerDay
		}
	}
	// 处理流水：累加当天的 SPEND_CONFIRM total_amount
	if txErr == nil && txSc == 200 {
		var tx ratelimit.TransactionsResp
		if json.Unmarshal(txRaw, &tx) == nil {
			today := time.Now().Format("2006-01-02")
			for _, r := range tx.Records {
				if r.GmtCreated == today && r.Type == "SPEND_CONFIRM" {
					a.TodayConsumed += r.TotalAmount
				}
			}
		}
	}
	// 收尾：按真实余额设置 schedule 状态
	if a.Balance == nil {
		// 余额接口失败：schedule 保持原样（normal）
	} else if a.Balance.Available <= 0 {
		a.Schedule = "exhausted"
		if a.Balance.Total == 0 && a.TodayConsumed > 0 {
			a.Balance.Total = a.TodayConsumed
		}
	}
}

// fetchTransactions 调用 /openapi/v1/magicubes/transactions。
func (c *msClient) fetchTransactions(siteBase, sessionID string, page, pageSize int) (json.RawMessage, error) {
	path := fmt.Sprintf("/openapi/v1/magicubes/transactions?page=%d&page_size=%d", page, pageSize)
	sc, data, err := c.get(siteBase, path, sessionID)
	if err != nil || sc != 200 {
		return nil, fmt.Errorf("transactions: status=%d", sc)
	}
	return data, nil
}

// 账号缓存
// accounts 端点只读缓存，由午夜签到或手动刷新填充。只有首次加载，或者缓存被清空之后，才会为所有账号
// 触发新的取数。

const accountsCacheTTL = 30 * time.Minute

type accountsCacheEntry struct {
	results        []accountSummary
	dedupedAuthIDs []string
	siteBase       map[string]string
	raw            map[string]string
	at             time.Time
}

var (
	accountsCacheMu   sync.Mutex
	accountsCache     accountsCacheEntry
	refreshInProgress bool // 避免重复的后台刷新
)

// fetchAccountBalanceOnly 只取余额和限流，不查 login/info 也不查流水，用于首次加载之后的缓存刷新。
// 资料（昵称、邮箱等）沿用现有缓存。
func (c *msClient) fetchAccountBalanceOnly(idx int, sessionID, siteBase, site string, existing *accountSummary) accountSummary {
	a := *existing // 保留首次加载时拿到的资料
	a.Index = idx

	// 这里只并发拉取余额和限流
	var (
		bSc, rlSc   int
		bRaw, rlRaw []byte
		bErr, rlErr error
		wg          sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		bSc, bRaw, bErr = c.getWithRetry(siteBase, "/openapi/v1/magicubes/balance", sessionID)
	}()
	go func() {
		defer wg.Done()
		rlSc, rlRaw, rlErr = c.getWithRetry(siteBase, "/api/v1/inference/rate-limit", sessionID)
	}()
	wg.Wait()

	if bErr == nil && bSc == 200 {
		var b ratelimit.MagicCubeBalance
		if json.Unmarshal(bRaw, &b) == nil {
			a.Balance = &balanceJSON{Total: b.TotalBalance, Available: b.AvailableBalance, Frozen: b.FrozenAmount}
			// 余额接口成功：按真实余额设置 schedule，覆盖 markAccountExhausted 留下的过期 exhausted 状态。
			if b.AvailableBalance <= 0 {
				a.Schedule = "exhausted"
			} else if a.Schedule == "exhausted" {
				a.Schedule = "normal" // 已恢复
			}
		}
	} else {
		// 余额接口失败：如果之前的 schedule 是 exhausted（由 markAccountExhausted 设置），不要保留它。此时我
		// 并不了解真实状态，退回 normal，避免卡在一个假的禁用上。
		if a.Schedule == "exhausted" {
			a.Schedule = "normal"
		}
	}
	if rlErr == nil && rlSc == 200 {
		var rl ratelimit.InferenceRateLimit
		if json.Unmarshal(rlRaw, &rl) == nil {
			a.UsagePerDay = rl.CurrentUsagePerDay
		}
	}
	return a
}

// refreshAccountsCache 并发地为所有账号取新数据并更新缓存，返回这批新结果。
func refreshAccountsCache(resolved resolvedEntry) []accountSummary {
	mc := &msClient{}
	// 先看有没有现成缓存（决定这次是刷新还是首次加载）。
	// 在持锁期间深拷贝切片，避免并发的 markAccountExhausted 写入与下面不加锁的遍历发生竞争。
	accountsCacheMu.Lock()
	existing := accountsCache.results
	isRefresh := existing != nil && len(existing) == len(resolved.dedupedAuthIDs)
	// 在持锁期间按 maskedKey 建好索引，避免后面的临界区之外再读同一份底层数组。
	existingByMask := make(map[string]*accountSummary, len(existing))
	if isRefresh {
		for i := range accountsCache.results {
			s := accountsCache.results[i]
			existingByMask[s.MaskedKey] = &s
		}
	}
	accountsCacheMu.Unlock()
	type indexedResult struct {
		idx int
		sum accountSummary
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make([]indexedResult, 0, len(resolved.raw))
		sem = make(chan struct{}, 20)
	)
	idx := 0
	for _, authID := range resolved.dedupedAuthIDs {
		sessionID := resolved.raw[authID]
		sb := resolved.siteBase[authID]
		site := "cn"
		if strings.Contains(sb, "modelscope.ai") {
			site = "ai"
		}
		i := idx
		sid := sessionID
		sbCopy := sb
		siteCopy := site
		mk := maskKey(sid)
		idx++
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if rec := recover(); rec != nil {
					logInfo("recovered panic in account worker: %v", rec)
				}
			}()
			var s accountSummary
			if isRefresh {
				ex, ok := existingByMask[mk]
				if ok {
					s = mc.fetchAccountBalanceOnly(i, sid, sbCopy, siteCopy, ex)
				} else {
					s = mc.fetchLoginInfo(i, sid, sbCopy, siteCopy)
				}
			} else {
				s = mc.fetchLoginInfo(i, sid, sbCopy, siteCopy)
			}
			mu.Lock()
			out = append(out, indexedResult{i, s})
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(out, func(a, b int) bool { return out[a].idx < out[b].idx })
	results := make([]accountSummary, len(out))
	for i, r := range out {
		results[i] = r.sum
	}

	// 第 2 阶段（仅首次加载）：此时所有账号都已完成 login/info（签到），现在为每个账号并发拉取余额、限流
	// 和流水，这样余额能反映签到激活后的魔粒额度。
	if !isRefresh {
		var wg2 sync.WaitGroup
		sem2 := make(chan struct{}, 20)
		for i := range results {
			if results[i].Schedule == "unauthorized" {
				continue
			}
			sid := resolved.maskToRaw[results[i].MaskedKey]
			sbCopy := resolved.maskToSite[results[i].MaskedKey]
			wg2.Add(1)
			sem2 <- struct{}{}
			go func(idx int, sessionID, siteBase string) {
				defer wg2.Done()
				defer func() { <-sem2 }()
				defer func() {
					if rec := recover(); rec != nil {
						logInfo("recovered panic in balance worker: %v", rec)
					}
				}()
				mc.fetchBalanceData(&results[idx], sessionID, siteBase)
			}(i, sid, sbCopy)
		}
		wg2.Wait()
	}

	// 对缺手机号的账号，用明细缓存补齐
	for i := range results {
		if results[i].Phone == "" {
			if d, ok := getCachedDetail(results[i].MaskedKey); ok && d.Phone != "" {
				results[i].Phone = d.Phone
			}
		}
	}
	accountsCacheMu.Lock()
	accountsCache = accountsCacheEntry{results: results, dedupedAuthIDs: resolved.dedupedAuthIDs, siteBase: resolved.siteBase, raw: resolved.raw, at: time.Now()}
	accountsCacheMu.Unlock()
	return results
}

// getAccountsCache 返回缓存的账号概况，为空时返回 nil。
func getAccountsCache() []accountSummary {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	if accountsCache.results == nil || time.Since(accountsCache.at) >= accountsCacheTTL {
		return nil
	}
	return accountsCache.results
}

// clearAccountsCache 让缓存失效（reconfigure 时调用）。
func clearAccountsCache() {
	accountsCacheMu.Lock()
	accountsCache = accountsCacheEntry{}
	accountsCacheMu.Unlock()
}

// markAccountExhausted 把该账号在账号缓存里的余额置 0、schedule 置为 exhausted。模型请求返回
// 429 + insufficient balance 时调用它，让前端不必等下一次刷新就能看到真实的余额状态。
func markAccountExhausted(maskedKey string) {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	if accountsCache.results == nil {
		return
	}
	for i := range accountsCache.results {
		if accountsCache.results[i].MaskedKey == maskedKey {
			accountsCache.results[i].Schedule = "exhausted"
			// 这里替换 Balance 指针而不是原地改字段，避免并发的刷新（它可能拷贝了结构体并握着旧指针）在
			// Available 字段上产生竞争。
			total := 0
			if accountsCache.results[i].Balance != nil {
				total = accountsCache.results[i].Balance.Total
			}
			accountsCache.results[i].Balance = &balanceJSON{Total: total, Available: 0, Frozen: 0}
			break
		}
	}
}

// fetchAccountDetailConcurrent 首次加载时并发调用 users/info、binding/check、tokens/list 和
// login/info，把延迟压到最低（限流数据取自账号缓存）。只刷明细时只调 binding/check 和
// tokens/list：资料（昵称、邮箱、手机号、havanaId、fromSite、isRealName）直接用账号缓存，
// 避免重复请求。
func (c *msClient) fetchAccountDetailConcurrent(idx int, sessionID, siteBase, site string, src *accountSummary) detailJSON {
	d := detailJSON{Index: idx, Site: site}

	// 用账号缓存填充资料字段
	if src != nil {
		d.Name = src.Name
		d.NickName = src.NickName
		d.Email = src.Email
		d.Phone = src.Phone
		d.HavanaID = src.HavanaID
		d.FromSite = src.FromSite
		d.IsRealName = src.IsRealName
		d.UsagePerDay = src.UsagePerDay
		d.TodayConsumed = src.TodayConsumed
	}

	var (
		bcMu, tlMu sync.Mutex
		bc         ratelimit.BindingCheck
		tl         ratelimit.SDKTokenList
		wg         sync.WaitGroup
	)

	// binding/check
	wg.Add(1)
	go func() {
		defer wg.Done()
		if sc, data, err := c.getWithRetry(siteBase, "/api/v1/users/binding/check", sessionID); err == nil && sc == 200 {
			bcMu.Lock()
			json.Unmarshal(data, &bc)
			bcMu.Unlock()
		}
	}()

	// tokens/list
	wg.Add(1)
	go func() {
		defer wg.Done()
		if sc, data, err := c.getWithRetry(siteBase, "/api/v1/users/tokens/list", sessionID); err == nil && sc == 200 {
			tlMu.Lock()
			json.Unmarshal(data, &tl)
			tlMu.Unlock()
		}
	}()

	wg.Wait()

	// 汇总结果
	d.AliyunBound = bc.IsBindingYun
	d.AliyunAID = bc.YunInfo.Aid
	d.AliyunNick = bc.YunInfo.NickName
	for _, t := range tl.SdkTokens {
		d.Tokens = append(d.Tokens, tokenJSON{
			ID: t.Id, Name: t.SdkTokenName, Token: maskToken(t.SdkToken),
			Valid: t.Valid == "1", Expires: t.ExpiresAt,
		})
	}
	d.SdkToken = maskToken(sessionID)
	return d
}

// 明细缓存（按 idx 缓存，不设 TTL）

type detailCacheEntry struct {
	detail detailJSON
	at     time.Time
}

var (
	detailCacheMu sync.Mutex
	detailCache   = make(map[string]detailCacheEntry)
)

const detailCacheTTL = 0 // 不设 TTL：缓存在插件生命周期内有效，checkin-all 或 reconfigure 时清空

func getCachedDetail(mk string) (detailJSON, bool) {
	detailCacheMu.Lock()
	defer detailCacheMu.Unlock()
	if e, ok := detailCache[mk]; ok {
		return e.detail, true
	}
	return detailJSON{}, false
}

func setCachedDetail(mk string, d detailJSON) {
	detailCacheMu.Lock()
	detailCache[mk] = detailCacheEntry{detail: d, at: time.Now()}
	detailCacheMu.Unlock()
}

func clearDetailCache() {
	detailCacheMu.Lock()
	detailCache = make(map[string]detailCacheEntry)
	detailCacheMu.Unlock()
}

func clearAllCaches() {
	clearAccountsCache()
	clearDetailCache()
}

// shouldBackgroundRefresh 判断该不该启动一次后台刷新；已有刷新在跑时返回 false。
func shouldBackgroundRefresh() bool {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	if refreshInProgress {
		return false
	}
	if accountsCache.at.IsZero() || time.Since(accountsCache.at) < accountsCacheTTL {
		return false
	}
	refreshInProgress = true
	return true
}

// forceBackgroundRefresh 忽略缓存 TTL，在没有刷新在跑时启动一次；返回是否新启动了刷新。
func forceBackgroundRefresh() bool {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	if refreshInProgress {
		return false
	}
	refreshInProgress = true
	return true
}

// 清除 running 标记。
func markRefreshDone() {
	accountsCacheMu.Lock()
	refreshInProgress = false
	accountsCacheMu.Unlock()
}

// 忽略 TTL 返回缓存数据，供重新校验期间继续使用。
func getStaleAccountsCache() []accountSummary {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	return accountsCache.results
}

func getAccountsCacheTime() string {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	if accountsCache.at.IsZero() {
		return ""
	}
	return accountsCache.at.Format("15:04:05")
}
