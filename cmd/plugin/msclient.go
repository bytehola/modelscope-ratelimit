package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"strings"
	"time"

	"modelscope-ratelimit/internal/ratelimit"
)

// msClient makes direct HTTP calls to the Modelscope API using the API key
// as the m_session_id cookie value. The key IS the cookie — it never expires.
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

// newMSTransport builds an http.Transport with sensible connection-pool
// settings. Under high concurrency (20 workers × 3 goroutines = 60 requests),
// Go's default MaxIdleConnsPerHost=2 causes excessive TCP churn that can
// overwhelm a debug proxy (ms_proxy_url). 30 concurrent connections per host
// is enough for the semaphore-limited workers without exhausting ephemeral
// ports through a proxy.
func newMSTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	return &http.Transport{
		Proxy:               proxy,
		MaxIdleConnsPerHost: 30,
		MaxConnsPerHost:     30,
	}
}

// SetMSProxy reconfigures the Modelscope API HTTP client to use a proxy.
// Called from configure() when ms_proxy_url is set in config.
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
		Timeout: 15 * time.Second,
		Transport: newMSTransport(http.ProxyURL(u)),
	})
}

// siteBaseFromURL derives the Modelscope site base URL from the provider's
// base-url: modelscope.cn → domestic, modelscope.ai → international.
func siteBaseFromURL(baseURL string) string {
	if strings.Contains(baseURL, "modelscope.ai") {
		return "https://www.modelscope.ai"
	}
	return "https://modelscope.cn"
}

// getWithRetry calls get() and retries on 429 (Too Many Requests) and on
// network errors. The retry budget is 5 attempts with jittered backoff.
// Network errors (err != nil, sc == 0) use a shorter backoff (0.5-1s) than
// 429 rate-limit responses (2-5s), because connection failures through a
// proxy tend to be transient and resolve quickly.
func (c *msClient) getWithRetry(siteBase, path, sessionID string) (int, json.RawMessage, error) {
	for attempt := 0; attempt < 5; attempt++ {
		sc, data, err := c.get(siteBase, path, sessionID)
		// Success: return immediately.
		if err == nil && sc != 429 {
			return sc, data, err
		}
		// Backoff: longer for 429 (server-enforced), shorter for network errors.
		var delay time.Duration
		if sc == 429 {
			delay = time.Duration(2+rand.Intn(4)) * time.Second
		} else {
			delay = time.Duration(500+rand.Intn(500)) * time.Millisecond
		}
		time.Sleep(delay)
	}
	// Final attempt without retry
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

// ---- Fetcher closure implementations (injected into Store) ----

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

// ---- Management API endpoints (served to frontend XHR) ----

type accountSummary struct {
	Index       int     `json:"index"`
	MaskedKey   string  `json:"maskedKey"`
	Phone       string  `json:"phone,omitempty"`
	Site        string  `json:"site"`
	Name        string  `json:"name"`
	NickName    string  `json:"nickName"`
	Email       string  `json:"email"`
	HavanaID    string  `json:"havanaId"`
	FromSite    string  `json:"fromSite"`
	IsRealName  bool    `json:"isRealName"`
	CheckedIn   bool    `json:"checkedIn"`
	LastCheckIn string  `json:"lastCheckIn"`
	Balance     *balanceJSON `json:"balance"`
	UsagePerDay int     `json:"usagePerDay"`
	TodayConsumed int     `json:"todayConsumed"`
	Schedule    string  `json:"schedule"`
	Error       string  `json:"error,omitempty"`
}

type balanceJSON struct {
	Total     int `json:"total"`
	Available int `json:"available"`
	Frozen    int `json:"frozen"`
}

type detailJSON struct {
	Index       int        `json:"index"`
	Name        string     `json:"name"`
	NickName    string     `json:"nickName"`
	Email       string     `json:"email"`
	Phone       string     `json:"phone"`
	HavanaID    string     `json:"havanaId"`
	FromSite    string     `json:"fromSite"`
	IsRealName  bool       `json:"isRealName"`
	Org         string     `json:"org"`
	School      string     `json:"school"`
	Industry    string     `json:"industry"`
	UsagePerDay int        `json:"usagePerDay"`
	TodayConsumed int     `json:"todayConsumed"`
	SdkToken    string     `json:"sdkToken"`
	AliyunBound bool       `json:"aliyunBound"`
	AliyunAID   string     `json:"aliyunAid"`
	AliyunNick  string     `json:"aliyunNick"`
	Site        string     `json:"site"`
	Tokens      []tokenJSON `json:"tokens"`
}

type tokenJSON struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Token   string `json:"token"`
	Valid   bool   `json:"valid"`
	Expires string `json:"expires"`
}

// fetchLoginInfo calls login/info (daily sign-in) + users/info (phone).
// Returns a partially populated accountSummary with profile data only.
// Balance/rate-limit/transactions are fetched separately in phase 2
// so that sign-in completes for ALL accounts before any balance read.
func (c *msClient) fetchLoginInfo(idx int, sessionID, siteBase, site string) accountSummary {
	a := accountSummary{Index: idx, MaskedKey: maskKey(sessionID), Site: site, Schedule: "normal"}

	// login/info + users/info — concurrent
	var (
		liSc, uiSc     int
		liRaw, uiRaw   []byte
		liErr, uiErr   error
		wg1            sync.WaitGroup
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

	// Populate phone from users/info
	if uiErr == nil && uiSc == 200 {
		var ui ratelimit.UserInfo
		if json.Unmarshal(uiRaw, &ui) == nil {
			a.Phone = ui.PhoneNo
		}
	}

	return a
}
 
// fetchBalanceData fetches balance + rate-limit + transactions concurrently
// for a single account. Called in phase 2 after ALL accounts have completed
// login/info (sign-in) so balance reflects the updated magic cube quota.
func (c *msClient) fetchBalanceData(a *accountSummary, sessionID, siteBase string) {
	var (
		bSc, rlSc, txSc int
		bRaw, rlRaw, txRaw []byte
		bErr, rlErr, txErr error
		wg sync.WaitGroup
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
 
	// Process balance
	if bErr == nil && bSc == 200 {
		var b ratelimit.MagicCubeBalance
		if json.Unmarshal(bRaw, &b) == nil {
			a.Balance = &balanceJSON{Total: b.TotalBalance, Available: b.AvailableBalance, Frozen: b.FrozenAmount}
		}
	}
	// Process rate-limit
	if rlErr == nil && rlSc == 200 {
		var rl ratelimit.InferenceRateLimit
		if json.Unmarshal(rlRaw, &rl) == nil {
			a.UsagePerDay = rl.CurrentUsagePerDay
		}
	}
	// Process transactions: sum today's SPEND_CONFIRM total_amount
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
	// Post-processing: set schedule based on actual balance
	if a.Balance == nil {
		// Balance API failed: keep schedule as-is (normal)
	} else if a.Balance.Available <= 0 {
		a.Schedule = "exhausted"
		if a.Balance.Total == 0 && a.TodayConsumed > 0 {
			a.Balance.Total = a.TodayConsumed
		}
	}
}

// fetchTransactions calls /openapi/v1/magicubes/transactions.
func (c *msClient) fetchTransactions(siteBase, sessionID string, page, pageSize int) (json.RawMessage, error) {
	path := fmt.Sprintf("/openapi/v1/magicubes/transactions?page=%d&page_size=%d", page, pageSize)
	sc, data, err := c.get(siteBase, path, sessionID)
	if err != nil || sc != 200 {
		return nil, fmt.Errorf("transactions: status=%d", sc)
	}
	return data, nil
}

// ---- Accounts cache ----
// The accounts endpoint reads from cache (populated by midnight sign-in or
// explicit "一键刷新"). Only the first load (or after cache clear) triggers
// fresh API calls for all accounts.

const accountsCacheTTL = 30 * time.Minute

type accountsCacheEntry struct {
	results        []accountSummary
	orderedAuthIDs []string
	siteBase       map[string]string
	raw            map[string]string
	at             time.Time
}

var (
	accountsCacheMu     sync.Mutex
	accountsCache       accountsCacheEntry
	refreshInProgress   bool // prevents duplicate background refresh
)
// fetchAccountBalanceOnly fetches only balance + rate-limit (no login/info, no transactions).
// Used for cache refresh after first load. Profile data (name, email, etc.) is preserved
// from the existing cache.
func (c *msClient) fetchAccountBalanceOnly(idx int, sessionID, siteBase, site string, existing *accountSummary) accountSummary {
	a := *existing // preserve profile data from first load
	a.Index = idx

	// Only fetch balance + rate-limit concurrently
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
			// Balance API succeeded: set schedule based on actual balance.
			// This overrides any stale "exhausted" from markAccountExhausted.
			if b.AvailableBalance <= 0 {
				a.Schedule = "exhausted"
			} else if a.Schedule == "exhausted" {
				a.Schedule = "normal" // recovered
			}
		}
	} else {
		// Balance API failed: if previous schedule was "exhausted" (from
		// markAccountExhausted), don't keep it — we don't know the real
		// state, so fall back to "normal" to avoid a stuck false disable.
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

// refreshAccountsCache fetches fresh data for all accounts concurrently
// and updates the cache. Returns the fresh results.
func refreshAccountsCache(resolved *resolvedEntry) []accountSummary {
	mc := &msClient{}
	// Check if we have existing cached data (refresh vs first load)
	// Deep-copy the slice under the lock so concurrent markAccountExhausted
	// writes can't race with the lock-free traversal below.
	accountsCacheMu.Lock()
	existing := accountsCache.results
	isRefresh := existing != nil && len(existing) == len(resolved.orderedAuthIDs)
	// Build a lookup by maskedKey under the lock so the same underlying
	// array is not read outside the critical section.
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
	for _, authID := range resolved.orderedAuthIDs {
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
					log.Printf("modelscope-ratelimit: recovered panic in account worker: %v", rec)
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

	// Phase 2 (first load only): ALL accounts have completed login/info (sign-in).
	// Now fetch balance + rate-limit + transactions concurrently for each account.
	// This ensures balance reflects the magic cubes activated by the sign-in.
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
						log.Printf("modelscope-ratelimit: recovered panic in balance worker: %v", rec)
					}
				}()
				mc.fetchBalanceData(&results[idx], sessionID, siteBase)
			}(i, sid, sbCopy)
		}
		wg2.Wait()
	}
 
	// Enrich phone from detail cache for accounts missing it
	for i := range results {
		if results[i].Phone == "" {
			if d, ok := getCachedDetail(results[i].MaskedKey); ok && d.Phone != "" {
				results[i].Phone = d.Phone
			}
		}
	}
	accountsCacheMu.Lock()
	accountsCache = accountsCacheEntry{results: results, orderedAuthIDs: resolved.orderedAuthIDs, siteBase: resolved.siteBase, raw: resolved.raw, at: time.Now()}
	accountsCacheMu.Unlock()
	return results
}

// getAccountsCache returns cached account summaries, or nil if empty.
func getAccountsCache() []accountSummary {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	if accountsCache.results == nil || time.Since(accountsCache.at) >= accountsCacheTTL {
		return nil
	}
	return accountsCache.results
}

// clearAccountsCache invalidates the cache (called on reconfigure).
func clearAccountsCache() {
	accountsCacheMu.Lock()
	accountsCache = accountsCacheEntry{}
	accountsCacheMu.Unlock()
}

// markAccountExhausted sets the account's balance to 0 and schedule to
// "exhausted" in the accounts cache. Called when a model request returns
// 429 + "insufficient balance" so the frontend immediately reflects the
// real balance state without waiting for the next API refresh.
func markAccountExhausted(maskedKey string) {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	if accountsCache.results == nil {
		return
	}
	for i := range accountsCache.results {
		if accountsCache.results[i].MaskedKey == maskedKey {
			accountsCache.results[i].Schedule = "exhausted"
			if accountsCache.results[i].Balance != nil {
				accountsCache.results[i].Balance.Available = 0
			} else {
				accountsCache.results[i].Balance = &balanceJSON{Total: 0, Available: 0, Frozen: 0}
			}
			break
		}
	}
}

// fetchAccountDetailConcurrent calls users/info + binding/check + tokens/list
// + login/info concurrently for minimal latency (rate-limit from accounts cache).
// fetchAccountDetailConcurrent calls binding/check + tokens/list only.
// Profile data (name, email, phone, havanaId, fromSite, isRealName) is
// taken from the accounts cache to avoid redundant API requests.
func (c *msClient) fetchAccountDetailConcurrent(idx int, sessionID, siteBase, site string, src *accountSummary) detailJSON {
	d := detailJSON{Index: idx, Site: site}

	// Populate profile fields from accounts cache
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
		bc  ratelimit.BindingCheck
		tl  ratelimit.SDKTokenList
		wg  sync.WaitGroup
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

	// Assemble results
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

// ---- Detail cache (per idx, 60s TTL) ----

type detailCacheEntry struct {
	detail detailJSON
	at     time.Time
}

var (
	detailCacheMu sync.Mutex
	detailCache   = make(map[string]detailCacheEntry)
)

const detailCacheTTL = 0 // no TTL: cache for plugin lifetime, cleared on checkin-all/reconfigure

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

// ---- Transactions cache (per "idx:page:pageSize", 60s TTL) ----

type txCacheEntry struct {
	data json.RawMessage
	at   time.Time
}

var (
	txCacheMu sync.Mutex
	txCache   = make(map[string]txCacheEntry)
)

const txCacheTTL = 60 * time.Second

func getCachedTx(key string) (json.RawMessage, bool) {
	txCacheMu.Lock()
	defer txCacheMu.Unlock()
	if e, ok := txCache[key]; ok && time.Since(e.at) < txCacheTTL {
		return e.data, true
	}
	// Evict expired entry to prevent unbounded growth.
	delete(txCache, key)
	return nil, false
}

func setCachedTx(key string, data json.RawMessage) {
	txCacheMu.Lock()
	txCache[key] = txCacheEntry{data: data, at: time.Now()}
	txCacheMu.Unlock()
}

func clearTxCache() {
	txCacheMu.Lock()
	txCache = make(map[string]txCacheEntry)
	txCacheMu.Unlock()
}

func clearAllCaches() {
	clearAccountsCache()
	clearDetailCache()
	clearTxCache()
}

// shouldBackgroundRefresh returns true if a background refresh should be started.
// Returns false if a refresh is already in progress.
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
 
// forceBackgroundRefresh starts a background refresh if one is not already
// running, regardless of cache TTL. Returns true if a new refresh was started.
func forceBackgroundRefresh() bool {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	if refreshInProgress {
		return false
	}
	refreshInProgress = true
	return true
}

// markRefreshDone clears the in-progress flag.
func markRefreshDone() {
	accountsCacheMu.Lock()
	refreshInProgress = false
	accountsCacheMu.Unlock()
}

// getStaleAccountsCache returns cached data regardless of TTL (for stale-while-revalidate).
func getStaleAccountsCache() []accountSummary {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	return accountsCache.results
}

// isAccountsCacheExpired reports whether the cache is older than accountsCacheTTL.
func isAccountsCacheExpired() bool {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	return !accountsCache.at.IsZero() && time.Since(accountsCache.at) >= accountsCacheTTL
}

// getAccountsCacheTime returns the timestamp when the cache was populated.
// getAccountsCacheSnapshot returns the orderedAuthIDs + raw + siteBase snapshot
// from when the accounts cache was built, ensuring idx consistency.
func getAccountsCacheSnapshot() ([]string, map[string]string, map[string]string) {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	if accountsCache.orderedAuthIDs == nil {
		return nil, nil, nil
	}
	return accountsCache.orderedAuthIDs, accountsCache.raw, accountsCache.siteBase
}

func getAccountsCacheTime() string {
	accountsCacheMu.Lock()
	defer accountsCacheMu.Unlock()
	if accountsCache.at.IsZero() {
		return ""
	}
	return accountsCache.at.Format("15:04:05")
}
