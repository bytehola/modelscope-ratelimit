package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static/index.html
var indexHTML []byte

// ---- config / keys ----

type keyConfig struct {
	BaseURL  string
	SiteBase string
	Site     string // "cn" or "ai"
	Keys     []string
}

func loadKeys(path string) (*keyConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var baseURL string
	var keys []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "base-url:") {
			baseURL = strings.TrimSpace(strings.TrimPrefix(line, "base-url:"))
		} else if i := strings.Index(line, "api-key:"); i >= 0 {
			raw := strings.Trim(strings.TrimSpace(line[i+len("api-key:"):]), "\"'")
			if raw != "" {
				keys = append(keys, raw)
			}
		}
	}
	if baseURL == "" || len(keys) == 0 {
		return nil, fmt.Errorf("keys.txt: missing base-url or api-key")
	}
	siteBase := "https://modelscope.cn"
	site := "cn"
	if strings.Contains(baseURL, "modelscope.ai") {
		siteBase = "https://www.modelscope.ai"
		site = "ai"
	}
	return &keyConfig{BaseURL: baseURL, SiteBase: siteBase, Site: site, Keys: keys}, nil
}

func maskKey(k string) string {
	if len(k) <= 12 {
		return k
	}
	return k[len(k)-12:]
}

// ---- Modelscope API client (uses token as m_session_id cookie) ----

var httpClient = &http.Client{Timeout: 15 * time.Second}

// apiGet calls a Modelscope API and normalizes the dual response format.
// /api/v1/*  → {"Code":200,"Data":{...},"Success":true}  (CamelCase)
// /openapi/* → {"success":true,"data":{...}}              (snake_case)
// Returns HTTP status, the inner Data payload (raw JSON), and error.
func apiGet(siteBase, path, token string) (httpStatus int, data json.RawMessage, err error) {
	req, err := http.NewRequest("GET", siteBase+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Cookie", "m_session_id="+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// Try CamelCase
	var camel struct {
		Code    int             `json:"Code"`
		Data    json.RawMessage `json:"Data"`
		Success bool            `json:"Success"`
	}
	if json.Unmarshal(body, &camel) == nil && (camel.Code != 0 || camel.Success) {
		return resp.StatusCode, camel.Data, nil
	}
	// Try snake_case
	var snake struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &snake) == nil {
		return resp.StatusCode, snake.Data, nil
	}
	return resp.StatusCode, nil, fmt.Errorf("unparseable response (status=%d)", resp.StatusCode)
}

// ---- response types (what the frontend sees) ----

type accountResp struct {
	Index       int     `json:"index"`
	MaskedKey   string  `json:"maskedKey"`
	Site        string  `json:"site"`
	Name        string  `json:"name"`
	NickName    string  `json:"nickName"`
	Email       string  `json:"email"`
	Phone       string  `json:"phone"`
	HavanaID    string  `json:"havanaId"`
	FromSite    string  `json:"fromSite"`
	IsRealName  bool    `json:"isRealName"`
	CheckedIn   bool    `json:"checkedIn"`
	LastCheckIn string  `json:"lastCheckIn"`
	Balance     *balanceData `json:"balance"`
	UsagePerDay int     `json:"usagePerDay"`
	Schedule    string  `json:"schedule"`
	Error       string  `json:"error,omitempty"`
}

type balanceData struct {
	Total     int    `json:"total"`
	Available int    `json:"available"`
	Frozen    int    `json:"frozen"`
	Level     string `json:"level"`
	ExpiryAt  string `json:"expiryAt"`
	ExpiryAmt int    `json:"expiryAmt"`
}

type detailResp struct {
	Index        int         `json:"index"`
	Name         string      `json:"name"`
	NickName     string      `json:"nickName"`
	Email        string      `json:"email"`
	Phone        string     `json:"phone"`
	HavanaID     string      `json:"havanaId"`
	FromSite     string     `json:"fromSite"`
	IsRealName   bool        `json:"isRealName"`
	Org          string      `json:"org"`
	School       string      `json:"school"`
	Industry     string      `json:"industry"`
	Level        string      `json:"level"`
	ExpiryAt     string      `json:"expiryAt"`
	ExpiryAmt    int         `json:"expiryAmt"`
	UsagePerDay  int         `json:"usagePerDay"`
	SdkToken     string      `json:"sdkToken"`
	AliyunBound  bool        `json:"aliyunBound"`
	AliyunAID    string      `json:"aliyunAid"`
	AliyunNick   string      `json:"aliyunNick"`
	Site         string      `json:"site"`
	Tokens       []tokenInfo  `json:"tokens"`
	Error        string      `json:"error,omitempty"`
}

type tokenInfo struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Token   string `json:"token"`
	Valid   bool   `json:"valid"`
	Expires string `json:"expires"`
}

// ---- server ----

type server struct {
	cfg      *keyConfig
	mu       sync.Mutex
}

func (s *server) fetchAccountSummary(idx int) accountResp {
	token := s.cfg.Keys[idx]
	sb := s.cfg.SiteBase
	a := accountResp{
		Index:     idx,
		MaskedKey: maskKey(token),
		Site:      s.cfg.Site,
		Schedule:  "normal",
	}

	// 1. login/info (also performs daily sign-in)
	if sc, data, err := apiGet(sb, "/api/v1/users/login/info", token); err == nil && sc == 200 {
		var info struct {
			Name        string `json:"Name"`
			NickName    string `json:"NickName"`
			Email       string `json:"Email"`
			HavanaID    string `json:"HavanaId"`
			FromSite    string `json:"FromSite"`
			IsRealName  bool   `json:"is_real_name"`
		}
		if json.Unmarshal(data, &info) == nil {
			a.Name = info.Name
			a.NickName = info.NickName
			a.Email = info.Email
			a.HavanaID = info.HavanaID
			a.FromSite = info.FromSite
			a.IsRealName = info.IsRealName
			a.CheckedIn = true
			a.LastCheckIn = time.Now().Format("2006-01-02 15:04:05")
		}
	} else if sc == 401 || sc == 403 {
		a.Schedule = "unauthorized"
		a.Error = fmt.Sprintf("login/info returned %d", sc)
		return a
	}

	// 2. balance
	if sc, data, err := apiGet(sb, "/openapi/v1/magicubes/balance", token); err == nil && sc == 200 {
		var b struct {
			Total     int    `json:"total_balance"`
			Available int    `json:"available_balance"`
			Frozen    int    `json:"frozen_amount"`
		}
		if json.Unmarshal(data, &b) == nil {
			a.Balance = &balanceData{
				Total: b.Total, Available: b.Available, Frozen: b.Frozen,
			}
			if b.Available <= 0 {
				a.Schedule = "exhausted"
			}
		}
	}

	// 3. inference rate-limit
	if sc, data, err := apiGet(sb, "/api/v1/inference/rate-limit", token); err == nil && sc == 200 {
		var rl struct {
			CurrentUsagePerDay int `json:"currentUsagePerDay"`
		}
		if json.Unmarshal(data, &rl) == nil {
			a.UsagePerDay = rl.CurrentUsagePerDay
		}
	}

	return a
}

func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	var results []accountResp
	for i := range s.cfg.Keys {
		results = append(results, s.fetchAccountSummary(i))
	}
	writeJSON(w, results)
}

func (s *server) handleAccountDetail(w http.ResponseWriter, r *http.Request) {
	idx := extractIdx(r)
	if idx < 0 || idx >= len(s.cfg.Keys) {
		writeJSONError(w, "invalid idx")
		return
	}
	token := s.cfg.Keys[idx]
	sb := s.cfg.SiteBase
	d := detailResp{Index: idx, Site: s.cfg.Site}

	// users/info
	if sc, data, err := apiGet(sb, "/api/v1/users/info", token); err == nil && sc == 200 {
		var info struct {
			UserName    string `json:"user_name"`
			NickName    string `json:"user_nickname"`
			Email       string `json:"email"`
			Phone       string `json:"phone_no"`
			IsRealName  bool   `json:"is_real_name"`
			Org         string `json:"Organization"`
			School      string `json:"School"`
			Industry    string `json:"industry"`
		}
		if json.Unmarshal(data, &info) == nil {
			d.Name = info.UserName
			d.NickName = info.NickName
			d.Email = info.Email
			d.Phone = info.Phone
			d.IsRealName = info.IsRealName
			d.Org = info.Org
			d.School = info.School
			d.Industry = info.Industry
		}
	} else if sc == 401 {
		d.Error = "密钥失效 (401)"
	}

	// login/info (for HavanaId, FromSite)
	if sc, data, err := apiGet(sb, "/api/v1/users/login/info", token); err == nil && sc == 200 {
		var li struct {
			HavanaID string `json:"HavanaId"`
			FromSite string `json:"FromSite"`
		}
		if json.Unmarshal(data, &li) == nil {
			d.HavanaID = li.HavanaID
			d.FromSite = li.FromSite
		}
	}

	// binding/check
	if sc, data, err := apiGet(sb, "/api/v1/users/binding/check", token); err == nil && sc == 200 {
		var bc struct {
			IsBound bool `json:"IsBindingYun"`
			Yun     struct {
				AID      string `json:"aid"`
				NickName string `json:"nick_name"`
			} `json:"YunInfo"`
		}
		if json.Unmarshal(data, &bc) == nil {
			d.AliyunBound = bc.IsBound
			d.AliyunAID = bc.Yun.AID
			d.AliyunNick = bc.Yun.NickName
		}
	}

	// balance (for level, expiry)
	if sc, data, err := apiGet(sb, "/openapi/v1/magicubes/balance", token); err == nil && sc == 200 {
		var b struct {
			Level           string `json:"user_level"`
			ExpiryAt        string `json:"nearest_expiry_at"`
			ExpiryAmt       int    `json:"nearest_expiry_amount"`
		}
		if json.Unmarshal(data, &b) == nil {
			d.Level = b.Level
			d.ExpiryAt = b.ExpiryAt
			d.ExpiryAmt = b.ExpiryAmt
		}
	}

	// rate-limit
	if sc, data, err := apiGet(sb, "/api/v1/inference/rate-limit", token); err == nil && sc == 200 {
		var rl struct {
			Usage int `json:"currentUsagePerDay"`
		}
		if json.Unmarshal(data, &rl) == nil {
			d.UsagePerDay = rl.Usage
		}
	}

	// tokens/list
	if sc, data, err := apiGet(sb, "/api/v1/users/tokens/list", token); err == nil && sc == 200 {
		var tl struct {
			Tokens []struct {
				ID        int    `json:"Id"`
				Name      string `json:"SdkTokenName"`
				Token     string `json:"SdkToken"`
				Valid     string `json:"Valid"`
				Expires   string `json:"ExpiresAt"`
			} `json:"SdkTokens"`
		}
		if json.Unmarshal(data, &tl) == nil {
			for _, t := range tl.Tokens {
				d.Tokens = append(d.Tokens, tokenInfo{
					ID: t.ID, Name: t.Name, Token: t.Token,
					Valid: t.Valid == "1", Expires: t.Expires,
				})
			}
		}
	}

	d.SdkToken = token
	writeJSON(w, d)
}

func (s *server) handleTransactions(w http.ResponseWriter, r *http.Request) {
	idx := extractIdx(r)
	if idx < 0 || idx >= len(s.cfg.Keys) {
		writeJSONError(w, "invalid idx")
		return
	}
	token := s.cfg.Keys[idx]
	sb := s.cfg.SiteBase
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}
	pageSize := r.URL.Query().Get("page_size")
	if pageSize == "" {
		pageSize = "50"
	}
	sc, data, err := apiGet(sb, "/openapi/v1/magicubes/transactions?page="+page+"&page_size="+pageSize, token)
	if err != nil || sc != 200 {
		writeJSON(w, map[string]any{"error": fmt.Sprintf("transactions fetch failed (status=%d)", sc), "records": []any{}})
		return
	}
	// Pass through the raw data (already normalized)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(data)
}

func (s *server) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	var results []map[string]any
	for i, token := range s.cfg.Keys {
		sc, _, err := apiGet(s.cfg.SiteBase, "/api/v1/users/login/info", token)
		ok := err == nil && sc == 200
		results = append(results, map[string]any{
			"index": i, "success": ok, "status": sc,
		})
	}
	writeJSON(w, results)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// ---- helpers ----

func extractIdx(r *http.Request) int {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// /api/account/{idx}/... → parts = ["api", "account", "{idx}", ...]
	if len(parts) >= 3 {
		idx, err := strconv.Atoi(parts[2])
		if err == nil {
			return idx
		}
	}
	return -1
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(400)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func main() {
	keysFile := flag.String("keys", "../keys.txt", "path to keys.txt")
	addr := flag.String("addr", ":8318", "listen address")
	flag.Parse()

	cfg, err := loadKeys(*keysFile)
	if err != nil {
		log.Fatalf("loadKeys: %v", err)
	}
	log.Printf("loaded %d keys, baseURL=%s, siteBase=%s (%s)", len(cfg.Keys), cfg.BaseURL, cfg.SiteBase, cfg.Site)

	srv := &server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/accounts", srv.handleAccounts)
	mux.HandleFunc("/api/account/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/transactions") {
			srv.handleTransactions(w, r)
		} else {
			srv.handleAccountDetail(w, r)
		}
	})
	mux.HandleFunc("/api/checkin-all", srv.handleCheckinAll)

	log.Printf("demo server: http://127.0.0.1%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
