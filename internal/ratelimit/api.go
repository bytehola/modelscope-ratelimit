package ratelimit

import (
	"encoding/json"
	"fmt"
)

// APIResponse 是 Modelscope API 的归一化响应。两种响应格式都支持：
//   - /api/v1/*     → {"Code":200,"Data":{...},"Success":true}  （大驼峰）
//   - /openapi/v1/* → {"success":true,"data":{...}}             （小写下划线）
//
// Data 字段以内层原始 JSON 的形式保留，由调用方反序列化成对应结构体。
type APIResponse struct {
	Success bool
	Code    int // openapi 格式下为 0
	Data    json.RawMessage
}

// NormalizeAPIResponse 解析 Modelscope API 的响应体，返回内层的 Data 载荷，无论该端点
// 用的是大驼峰信封（/api/v1/*）还是小写下划线信封（/openapi/v1/*）。它是纯函数、不依赖
// HTTP，因此可以脱离宿主单独做单测。
func NormalizeAPIResponse(raw []byte) (*APIResponse, error) {
	// 一次反序列化搞定：json.Unmarshal 对结构体字段匹配是大小写不敏感的，所以一个匿名
	// 结构体就能同时吃下大驼峰（/api/v1/*）和小写下划线（/openapi/v1/*）两种信封，包括
	// "Code" 是大驼峰而 "data" 是小写这种混合写法。
	var env struct {
		Code    int             `json:"Code"`
		Success bool            `json:"Success"`
		Data    json.RawMessage `json:"Data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("unparseable API response (first 200 bytes: %s)", truncate(raw, 200))
	}
	// 如果匹配到了大驼峰 "Data"，直接使用。
	if len(env.Data) > 0 {
		return &APIResponse{Success: env.Success || env.Code == 200, Code: env.Code, Data: env.Data}, nil
	}
	// 大驼峰 "Data" 为空 —— 试试小写 "data"（混合信封）。上面 json.Unmarshal 已经按大
	// 小写不敏感匹配过，env.Data 里就是它。如果 env.Data 仍为空，说明响应根本没有 Data
	// 字段（例如错误响应）。
	if env.Code != 0 || env.Success {
		return &APIResponse{Success: env.Success || env.Code == 200, Code: env.Code, Data: env.Data}, nil
	}
	return nil, fmt.Errorf("unparseable API response (first 200 bytes: %s)", truncate(raw, 200))
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}

// ---- 响应结构体（字段名与 json tag 与真实 API 一致）----

// LoginInfo 对应 GET /api/v1/users/login/info 的 Data。
// 调用该端点同时会触发当日魔粒签到。
type LoginInfo struct {
	Name       string `json:"Name"`
	NickName   string `json:"NickName"`
	Email      string `json:"Email"`
	HavanaId   string `json:"HavanaId"`
	FromSite   string `json:"FromSite"`
	IsRealName bool   `json:"is_real_name"`
}

// MagicCubeBalance 对应 GET /openapi/v1/magicubes/balance 的 data。
// 真实 API 只返回这三个字段。
type MagicCubeBalance struct {
	TotalBalance     int `json:"total_balance"`
	AvailableBalance int `json:"available_balance"`
	FrozenAmount     int `json:"frozen_amount"`
}

// TransactionRecord 对应 GET /openapi/v1/magicubes/transactions 的
// data.records[] 中的单条记录。expire_days/expire_at/days_remaining 只在 EARN 记录里出
// 现；model_tier 只在 SPEND_CONFIRM 记录里出现。
type TransactionRecord struct {
	GmtCreated    string `json:"gmt_created"`
	Type          string `json:"type"` // 取值 EARN / SPEND_CONFIRM / EXPIRE
	RuleKey       string `json:"rule_key"`
	CategoryKey   string `json:"category_key"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	Category      string `json:"category"`
	ModelTier     string `json:"model_tier"` // 仅 SPEND_CONFIRM 有
	TotalAmount   int    `json:"total_amount"`
	Count         int    `json:"count"`
	ExpireDays    int    `json:"expire_days"`    // 仅 EARN 有
	ExpireAt      string `json:"expire_at"`      // 仅 EARN 有
	DaysRemaining int    `json:"days_remaining"` // 仅 EARN 有
}

// TransactionsResp 对应 GET /openapi/v1/magicubes/transactions 的 data。
type TransactionsResp struct {
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
	Total    int                 `json:"total"`
	Records  []TransactionRecord `json:"records"`
}

// SDKToken 对应 GET /api/v1/users/tokens/list 的 Data.SdkTokens[]。
type SDKToken struct {
	Id           int    `json:"Id"`
	SdkToken     string `json:"SdkToken"`
	SdkTokenName string `json:"SdkTokenName"`
	Valid        string `json:"Valid"` // "1" 有效，"0" 无效
	ExpiresAt    string `json:"ExpiresAt"`
	GmtCreated   string `json:"GmtCreated"`
	Username     string `json:"Username"`
}

// SDKTokenList 对应 GET /api/v1/users/tokens/list 的 Data。
type SDKTokenList struct {
	SdkTokens  []SDKToken `json:"SdkTokens"`
	TotalCount int        `json:"TotalCount"`
}

// UserInfo 对应 GET /api/v1/users/info 的 Data（字段大小写混用）。
type UserInfo struct {
	UserName     string `json:"user_name"`
	UserNickname string `json:"user_nickname"`
	Email        string `json:"email"`
	PhoneNo      string `json:"phone_no"`
	IsRealName   bool   `json:"is_real_name"`
	Organization string `json:"Organization"`
	School       string `json:"School"`
	Industry     string `json:"industry"`
	UserId       string `json:"user_id"`
}

// BindingCheck 对应 GET /api/v1/users/binding/check 的 Data。
type BindingCheck struct {
	IsBindingYun bool `json:"IsBindingYun"`
	YunInfo      struct {
		Aid              string `json:"aid"`
		LoginName        string `json:"login_name"`
		NickName         string `json:"nick_name"`
		RealNameVerified bool   `json:"real_name_verified"`
	} `json:"YunInfo"`
}

// InferenceRateLimit 对应 GET /api/v1/inference/rate-limit 的 Data。
type InferenceRateLimit struct {
	CurrentUsagePerDay int `json:"currentUsagePerDay"`
}

// ---- 取数闭包类型 ----
// 核心层只持有函数指针，具体实现由胶水层提供，负责真正发起 HTTP 调用（直接用
// net/http，走 cookie 鉴权）。session 就是 api key 本身（m_session_id cookie 的值），
// 永不过期。
//
// 下面五个类型里，目前真正被胶水层注入并读取的只有 MidnightSignInFunc（见 Store 上的
// SetMidnightSignInFunc）；LoginInfoFetcher / BalanceFetcher / RateLimitFetcher 的 setter
// 没有任何调用方，TransactionsFetcher / TokenListFetcher 连 setter 都没有 —— 状态页与签到
// 走的是 cmd/plugin/msclient.go 里的那套客户端方法。保留它们只是为了让核心层能脱机单测这
// 些载荷形状，删除前请先确认没有新的注入点。

type LoginInfoFetcher func(sessionID string) (*LoginInfo, error)
type BalanceFetcher func(sessionID string) (*MagicCubeBalance, error)
type TransactionsFetcher func(sessionID string, page, pageSize int) (*TransactionsResp, error)
type TokenListFetcher func(sessionID string) (*SDKTokenList, error)
type RateLimitFetcher func(sessionID string) (*InferenceRateLimit, error)

// MidnightSignInFunc 由核心层在跨过日期边界时（PruneAll -> dayChanged）调用。胶水层的
// 实现会遍历所有已解析的 auth ID，逐个用 msClient 调 /api/v1/users/login/info（激活当日魔粒）
// 并拉取余额与限流，再通过 RecordCheckIn / RecordMagicCube / RecordUsagePerDay 写回核心
// 状态 —— 它不经过上面那几个 Fetcher。
type MidnightSignInFunc func()

// ---- Store 上用于注入闭包的 setter ----

// SetLoginInfoFetcher 注入一个按 session id 读取用户资料（昵称、邮箱、Havana ID 等）的
// 函数。当前没有调用方，也没有读取方（见文件头的说明）。
func (s *Store) SetLoginInfoFetcher(f LoginInfoFetcher) {
	if f == nil {
		s.loginInfoFetcher.Store(nil)
		return
	}
	s.loginInfoFetcher.Store(&f)
}

// SetBalanceFetcher 注入一个按 session id（m_session_id）读取当前魔粒余额的函数。当前
// 没有调用方，也没有读取方（见文件头的说明）。
func (s *Store) SetBalanceFetcher(f BalanceFetcher) {
	if f == nil {
		s.balanceFetcher.Store(nil)
		return
	}
	s.balanceFetcher.Store(&f)
}

// SetRateLimitFetcher 注入一个按 session id 读取当日用量计数的函数。当前没有调用方，也
// 没有读取方（见文件头的说明）。
func (s *Store) SetRateLimitFetcher(f RateLimitFetcher) {
	if f == nil {
		s.rateLimitFetcher.Store(nil)
		return
	}
	s.rateLimitFetcher.Store(&f)
}

// SetMidnightSignInFunc 注入跨日回调，由 PruneAll 在日期变化时调用。胶水层的实现会给
// 每个账号签到并刷新余额 + 用量。
func (s *Store) SetMidnightSignInFunc(f MidnightSignInFunc) {
	if f == nil {
		s.midnightSignInFunc.Store(nil)
		return
	}
	s.midnightSignInFunc.Store(&f)
}
