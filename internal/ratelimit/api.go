package ratelimit

import (
	"encoding/json"
	"fmt"
)

// APIResponse is the normalized response from Modelscope APIs. Both response
// formats are supported:
//   - /api/v1/*     → {"Code":200,"Data":{...},"Success":true}  (CamelCase)
//   - /openapi/v1/* → {"success":true,"data":{...}}             (snake_case)
// The Data field holds the inner payload as raw JSON for the caller to
// unmarshal into the appropriate struct.
type APIResponse struct {
	Success bool
	Code    int // 0 for openapi format
	Data    json.RawMessage
}

// NormalizeAPIResponse parses a Modelscope API response body and returns the
// inner Data payload regardless of whether the endpoint uses the CamelCase
// (/api/v1/*) or snake_case (/openapi/v1/*) envelope. It is a pure function
// with no HTTP dependency so it can be unit-tested in isolation.
func NormalizeAPIResponse(raw []byte) (*APIResponse, error) {
	// Single unmarshal: json.Unmarshal is case-insensitive for struct field
	// matching, so a single anonymous struct captures both CamelCase (/api/v1/*)
	// and snake_case (/openapi/v1/*) envelopes, including mixed formats where
	// "Code" is CamelCase but "data" is lowercase.
	var env struct {
		Code    int             `json:"Code"`
		Success bool            `json:"Success"`
		Data    json.RawMessage `json:"Data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("unparseable API response (first 200 bytes: %s)", truncate(raw, 200))
	}
	// If "Data" (CamelCase) was matched, use it directly.
	if len(env.Data) > 0 {
		return &APIResponse{Success: env.Success || env.Code == 200, Code: env.Code, Data: env.Data}, nil
	}
	// CamelCase "Data" was empty — try lowercase "data" (mixed envelope).
	// json.Unmarshal matched it case-insensitively above, so env.Data
	// already contains it. If env.Data is still empty, the response has
	// no Data field at all (e.g. error responses).
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

// ---- Response structs (field names and json tags mirror the real API) ----

// LoginInfo mirrors GET /api/v1/users/login/info -> Data.
// Calling this endpoint also activates the daily magic cube sign-in.
type LoginInfo struct {
	Name       string `json:"Name"`
	NickName   string `json:"NickName"`
	Email      string `json:"Email"`
	HavanaId   string `json:"HavanaId"`
	FromSite   string `json:"FromSite"`
	IsRealName bool   `json:"is_real_name"`
}

// MagicCubeBalance mirrors GET /openapi/v1/magicubes/balance -> data.
// Only three fields are returned by the real API.
type MagicCubeBalance struct {
	TotalBalance     int `json:"total_balance"`
	AvailableBalance int `json:"available_balance"`
	FrozenAmount     int `json:"frozen_amount"`
}

// TransactionRecord mirrors a single record from
// GET /openapi/v1/magicubes/transactions -> data.records[].
// Fields expire_days/expire_at/days_remaining are present only on EARN records.
// model_tier is present only on SPEND_CONFIRM records.
type TransactionRecord struct {
	GmtCreated    string `json:"gmt_created"`
	Type          string `json:"type"` // EARN / SPEND_CONFIRM / EXPIRE
	RuleKey       string `json:"rule_key"`
	CategoryKey   string `json:"category_key"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	Category      string `json:"category"`
	ModelTier     string `json:"model_tier"`     // SPEND_CONFIRM only
	TotalAmount   int    `json:"total_amount"`
	Count         int    `json:"count"`
	ExpireDays    int    `json:"expire_days"`    // EARN only
	ExpireAt      string `json:"expire_at"`      // EARN only
	DaysRemaining int    `json:"days_remaining"` // EARN only
}

// TransactionsResp mirrors GET /openapi/v1/magicubes/transactions -> data.
type TransactionsResp struct {
	Page     int                  `json:"page"`
	PageSize int                  `json:"page_size"`
	Total    int                  `json:"total"`
	Records  []TransactionRecord `json:"records"`
}

// SDKToken mirrors GET /api/v1/users/tokens/list -> Data.SdkTokens[].
type SDKToken struct {
	Id           int    `json:"Id"`
	SdkToken     string `json:"SdkToken"`
	SdkTokenName string `json:"SdkTokenName"`
	Valid        string `json:"Valid"` // "1" = valid, "0" = invalid
	ExpiresAt    string `json:"ExpiresAt"`
	GmtCreated   string `json:"GmtCreated"`
	Username     string `json:"Username"`
}

// SDKTokenList mirrors GET /api/v1/users/tokens/list -> Data.
type SDKTokenList struct {
	SdkTokens  []SDKToken `json:"SdkTokens"`
	TotalCount int        `json:"TotalCount"`
}

// UserInfo mirrors GET /api/v1/users/info -> Data (mixed case fields).
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

// BindingCheck mirrors GET /api/v1/users/binding/check -> Data.
type BindingCheck struct {
	IsBindingYun bool `json:"IsBindingYun"`
	YunInfo      struct {
		Aid              string `json:"aid"`
		LoginName        string `json:"login_name"`
		NickName         string `json:"nick_name"`
		RealNameVerified bool   `json:"real_name_verified"`
	} `json:"YunInfo"`
}

// InferenceRateLimit mirrors GET /api/v1/inference/rate-limit -> Data.
type InferenceRateLimit struct {
	CurrentUsagePerDay int `json:"currentUsagePerDay"`
}

// ---- Fetcher closure types ----
// The core layer holds function pointers; the glue layer provides
// implementations that make real HTTP calls (net/http direct, cookie auth).
// Session = API key itself (m_session_id cookie value), never expires.

type LoginInfoFetcher func(sessionID string) (*LoginInfo, error)
type BalanceFetcher func(sessionID string) (*MagicCubeBalance, error)
type TransactionsFetcher func(sessionID string, page, pageSize int) (*TransactionsResp, error)
type TokenListFetcher func(sessionID string) (*SDKTokenList, error)
type RateLimitFetcher func(sessionID string) (*InferenceRateLimit, error)

// MidnightSignInFunc is called by the core layer at the daily boundary
// (PruneAll -> dayChanged). The glue layer implementation iterates all known
// auth IDs, calls LoginInfoFetcher for each, then refreshes balance and
// rate-limit, updating core state via recordCheckIn/recordMagicCube/recordUsagePerDay.
type MidnightSignInFunc func()

// ---- Store setter methods for injected closures ----

func (s *Store) SetLoginInfoFetcher(f LoginInfoFetcher) {
	if f == nil {
		s.loginInfoFetcher.Store(nil)
		return
	}
	s.loginInfoFetcher.Store(&f)
}

func (s *Store) SetBalanceFetcher(f BalanceFetcher) {
	if f == nil {
		s.balanceFetcher.Store(nil)
		return
	}
	s.balanceFetcher.Store(&f)
}

func (s *Store) SetRateLimitFetcher(f RateLimitFetcher) {
	if f == nil {
		s.rateLimitFetcher.Store(nil)
		return
	}
	s.rateLimitFetcher.Store(&f)
}

func (s *Store) SetMidnightSignInFunc(f MidnightSignInFunc) {
	if f == nil {
		s.midnightSignInFunc.Store(nil)
		return
	}
	s.midnightSignInFunc.Store(&f)
}
