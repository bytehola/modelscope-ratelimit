package ratelimit

import (
	"encoding/json"
	"strings"
	"time"
)

// ApplyRateLimit 检查 Modelscope API 的响应并记录对应的禁用状态。它是响应拦截器、
// 流拦截器（仅响应头阶段）和用量钩子共用的唯一判定入口。
//
//   - 429 + "insufficient balance" => 全局禁用（魔粒已耗尽）。
func (s *Store) ApplyRateLimit(authID, model string, headers map[string][]string, statusCode int, body string, now time.Time) {
	if authID == "" {
		// 没有 auth id 就无法把禁用归因到具体凭据；用量钩子（一定带 AuthID）会补上。
		return
	}
	// 429 + "insufficient balance"（账号余额耗尽）走日禁用：这不是速率窗口，在充值之前
	// 这把 key 确实不可用。放在解析限流响应头之前判断，因为余额不足的响应可能不带限流
	// 头。该判断不受 skip_provider_on_429 影响：余额耗尽不是可恢复的速率窗口，跳过逻辑
	// 不允许绕过日禁用。
	if IsInsufficientBalance(statusCode, body) {
		s.recordSeen([]string{authID})
		s.disableGlobal(authID, model, now)
		return
	}
}

// IsResponsesRateLimitExceeded 判断一个 Responses API 事件是不是速率/额度被拒（即
// response.failed + rate_limit_exceeded）。匹配的是带引号的 JSON 取值 token
// （"response.failed" / "rate_limit_exceeded"），因此能容忍冒号后的空格和 SSE 的 "data:"
// 分帧。
//
// 它只说明“这一次请求被拒”，不说明整家共享额度耗尽：实测中 Modelscope 在单把 key 魔粒耗尽或
// 撞上该 key 的速率窗口时都会回这个事件（同一个账号条件在 Chat 侧表现为 429 + "insufficient
// balance"），换同一家的下一把 key 通常立刻能服务。因此它只允许触发逐 key 处理；要摘掉整个
// provider 必须另外命中 IsPoolLevelQuotaExhausted。
func IsResponsesRateLimitExceeded(body string) bool {
	if body == "" {
		return false
	}
	return strings.Contains(body, `"response.failed"`) &&
		strings.Contains(body, `"rate_limit_exceeded"`)
}

// IsResponsesFailed 判断一个 Responses API 事件是否是任意一种 response.failed（不限于
// rate_limit_exceeded）。Responses 协议会把错误以 HTTP 200 的 SSE + response.failed 事
// 件送回来，所以执行器必须识别出所有这类情况并换下一把 key/下一个 provider 重试，而不
// 是把 200 当成真正成功。与 IsResponsesRateLimitExceeded 一样，这里匹配带引号的
// token，避免因为 JSON 空格或 SSE 分帧而漏判。
func IsResponsesFailed(body string) bool {
	if body == "" {
		return false
	}
	return strings.Contains(body, `"response.failed"`)
}

// responsesErrorProbe 只读取顶层 JSON 文档中足够的信息，用来把上游错误体与合法的
// Responses 载荷区分开。只看顶层成员，所以工具调用或消息内容里嵌套的
// "code"/"message" 字段不会造成误判。
type responsesErrorProbe struct {
	Object  *string          `json:"object"`
	Error   *json.RawMessage `json:"error"`
	Code    *string          `json:"code"`
	Message *string          `json:"message"`
}

// IsResponsesErrorPayload 判断一个以 HTTP 200 返回的响应体实际上是不是错误文档而不是
// Response 资源。Modelscope 会把部分拒绝（参数非法、模型不支持）以 200 + 裸
// {"code":...,"message":...} 返回；而 Responses 协议本身在 completed 的对象里会保留
// "error":null。所以无论是对 "error" 做子串匹配还是只看状态码都不足以下结论，调用方借
// 此避免把上游失败当成成功响应交给客户端。
func IsResponsesErrorPayload(body string) bool {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "{") {
		return false
	}
	// 解码前的廉价预筛：真实响应体很大，只要通篇没有提到 error 或 code，就不可能是错误
	// 文档。
	if !strings.Contains(body, `"error"`) && !strings.Contains(body, `"code"`) {
		return false
	}
	var probe responsesErrorProbe
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return false
	}
	if hasJSONErrorValue(probe.Error) {
		return true
	}
	if probe.Object != nil && strings.EqualFold(strings.TrimSpace(*probe.Object), "response") {
		// error 为 null/空的 Response 资源是真正的成功。
		return false
	}
	return probe.Code != nil && strings.TrimSpace(*probe.Code) != "" &&
		probe.Message != nil && strings.TrimSpace(*probe.Message) != ""
}

// hasJSONErrorValue 判断 "error" 成员是否真的带了内容。null、空对象/空数组、空字符串
// 都表示“没有错误”。
func hasJSONErrorValue(raw *json.RawMessage) bool {
	if raw == nil {
		return false
	}
	switch strings.TrimSpace(string(*raw)) {
	case "", "null", "{}", "[]", `""`:
		return false
	default:
		return true
	}
}

// ResponsesFirstDataEvent 返回到“第一个带 data 字段的完整 SSE 事件”结束处的字节。在字
// 段本身和事件分发边界都到达之前返回 false；atEOF 为真时例外（SSE 允许最后一个事件由
// 流结束来触发）。
func ResponsesFirstDataEvent(body string, atEOF bool) (string, bool) {
	dataStart := firstSSEDataFieldIndex(body)
	if dataStart < 0 {
		return "", false
	}

	for i := dataStart; i < len(body); i++ {
		if body[i] != '\n' {
			continue
		}
		if i+1 < len(body) && body[i+1] == '\n' {
			return body[:i+1], true
		}
		if i+2 < len(body) && body[i+1] == '\r' && body[i+2] == '\n' {
			return body[:i+1], true
		}
	}

	if atEOF {
		return body, true
	}
	return "", false
}

func firstSSEDataFieldIndex(body string) int {
	for start := 0; start < len(body); {
		relEnd := strings.IndexByte(body[start:], '\n')
		end := len(body)
		hasNewline := relEnd >= 0
		if hasNewline {
			end = start + relEnd
		}

		line := strings.TrimSuffix(body[start:end], "\r")
		if strings.HasPrefix(line, "data:") {
			return start
		}
		if !hasNewline {
			break
		}
		start = end + 1
	}
	return -1
}

// IsPoolLevelQuotaExhausted 判断响应体是否代表“整家共享额度耗尽”：只有带引号的
// "insufficient_quota" token 才算，Chat 的 429 JSON 与 Responses 的 200 SSE 事件都用它表
// 达。这是 skip_provider_on_429 唯一允许把整个 provider 放进 skip 集的依据；
// response.failed + rate_limit_exceeded 不在其列，那只代表这一把 key。
//
// 判定用子串匹配（带引号，避免命中普通文案），因为这条判断在每次失败响应的热路径上。
func IsPoolLevelQuotaExhausted(body string) bool {
	return strings.Contains(body, `"insufficient_quota"`)
}

// isInsufficientQuota 判断一次失败响应是否值得为该 key 施加 insufficient_quota 冷却：既包
// 括池级的 429 + "insufficient_quota"，也包括 Responses API 以 200 流事件回送的速率拒绝。
// 冷却只影响这一把 key（外加全局退避档位），所以这里可以放宽；要摘掉整个 provider 必须再用
// IsPoolLevelQuotaExhausted 判一次。
func isInsufficientQuota(statusCode int, body string) bool {
	if IsResponsesRateLimitExceeded(body) {
		return true
	}
	return statusCode == 429 && IsPoolLevelQuotaExhausted(body)
}

// IsInsufficientBalance 判断响应是否是 body 里带 "insufficient balance" 字样的 429
// （账号余额耗尽）。与 insufficient_quota（池级速率窗口）不同，余额耗尽意味着在充值之
// 前这把 key 真的不可用，因此不论 skip_provider_on_429 如何都会触发日禁用。判定用对
// "insufficient balance"（中间是空格而非下划线）的大小写不敏感子串匹配，以覆盖各种
// JSON 写法。
func IsInsufficientBalance(statusCode int, body string) bool {
	if statusCode != 429 {
		return false
	}
	return strings.Contains(strings.ToLower(body), "insufficient balance")
}

// ApplyInsufficientQuotaCooldown 检查 429 + insufficient_quota 响应，命中后给该 key 设
// 一个短期冷却，让调度器在配置的时间窗内选别的 key。与日禁用不同，冷却在 X 秒后自动
// 到期，key 不必等到午夜就重新可用。
func (s *Store) ApplyInsufficientQuotaCooldown(authID, model string, statusCode int, body string, now time.Time) {
	if authID == "" {
		return
	}
	cfg := s.config()
	if cfg.InsufficientQuotaCooldown <= 0 {
		return
	}
	if !isInsufficientQuota(statusCode, body) {
		return
	}
	s.recordSeen([]string{authID})
	s.setCooldown(authID, cfg.InsufficientQuotaCooldown, now)
}

// OnResponse 处理 response.intercept_after：解析响应头并更新状态，返回一个不改动任何
// 东西的拦截结果（本插件只观察，绝不改写 Modelscope 的响应）。
func (s *Store) OnResponse(req ResponseInterceptRequest) ResponseInterceptResponse {
	authID := AuthIDFromMetadata(req.Metadata)
	s.ApplyRateLimit(authID, req.Model, req.ResponseHeaders, req.StatusCode, req.Body, s.now())
	if IsResponsesRateLimitExceeded(req.Body) {
		if s.consumeResponsesRateLimitApplied(authID, s.now()) {
			return ResponseInterceptResponse{}
		}
		s.ApplyResponsesRateLimit(
			authID,
			ProviderFromMetadata(req.Metadata),
			req.RequestedModel,
			req.Body,
			s.now(),
		)
	}
	return ResponseInterceptResponse{}
}

// OnStreamChunk 处理 response.intercept_stream_chunk。只有仅带头信息的初始化调用
// （ChunkIndex == -1）才带限流响应头，数据块原样放行。
func (s *Store) OnStreamChunk(req StreamChunkInterceptRequest) StreamChunkInterceptResponse {
	if req.ChunkIndex == -1 {
		authID := AuthIDFromMetadata(req.Metadata)
		s.ApplyRateLimit(authID, req.Model, req.ResponseHeaders, 0, "", s.now())
		return StreamChunkInterceptResponse{}
	}
	if IsResponsesRateLimitExceeded(req.Body) {
		authID := AuthIDFromMetadata(req.Metadata)
		if s.consumeResponsesRateLimitApplied(authID, s.now()) {
			return StreamChunkInterceptResponse{}
		}
		s.ApplyResponsesRateLimit(
			authID,
			ProviderFromMetadata(req.Metadata),
			req.RequestedModel,
			req.Body,
			s.now(),
		)
	}
	return StreamChunkInterceptResponse{}
}

// ApplyResponsesRateLimit 处理如今以 HTTP 200 + response.failed 形态出现的 Responses API
// 额度/速率失败。
//
// 只有 body 里带池级 "insufficient_quota" 时，skip_provider_on_429 才有资格把整家摘掉。其余
// 情况（最常见的是该 key 自己的速率窗口/魔粒耗尽）一律按逐 key 冷却处理，本家剩下的 key 继续
// 轮换：一次 key 级失败就换 provider 会在第一家还剩大量可用凭据时，把请求交给另一家的另一个上
// 游模型。
func (s *Store) ApplyResponsesRateLimit(authID, provider, model, body string, now time.Time) {
	if authID == "" {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	cfg := s.config()
	s.recordSeen([]string{authID})
	s.recordRateLimit()
	if cfg.SkipProviderOn429 && IsPoolLevelQuotaExhausted(body) {
		// 池级额度耗尽：整家进 skip 集，latch 到任意一次 200 为止。
		if provider != "" {
			s.SetSkipProvider(extractProviderName(provider))
		}
	} else {
		// key 级速率窗口（或跳过模式关闭）：只冷却这一把 key，本家剩下的 key 继续轮换。
		s.ApplyInsufficientQuotaCooldown(authID, model, 200, body, now)
	}
	// 两个标记无条件落账：这类失败以 HTTP 200 送达，否则随后的用量钩子会把它当成健康响应，
	// 把刚设好的 skip 与退避清掉。
	s.markResponsesRateLimited(authID, now)
	s.markResponsesRateLimitApplied(authID, now)
}

// OnUsage 处理 usage.handle。UsageRecord 同时带有 AuthID、Model 和响应头，因此这里是
// 权威的关联入口（流式与非流式都覆盖）。
//
// 模型名对齐：CLIProxyAPI 的调度器（SchedulerPick）收到的是*客户端请求的别名*（例如
// "glm-5.2"），不是解析后的上游模型名；而用量记录里 rec.Model 是解析后的上游模型（例
// 如 "ZhipuAI/GLM-5.2"），rec.Alias 才是请求的别名。由于 429 走的是执行器的错误路径
// （宿主提前返回，不会调用响应拦截器），本钩子是记录限流响应对应禁用的唯一路径，而它
// 过去按上游名做键。调度器随后用别名去查 isDisabled，什么都查不到 —— 一旦宿主自己的短
// 冷却过期而本插件的日禁用仍在，就会把请求重新派给已耗尽的 key，继续吃上游 429。现在
// 禁用按调度器检查的那个名字（别名）做键，isDisabled(id, alias) 就能命中；没有配置别名
// 时（别名 == 上游名）回退到 rec.Model。
func (s *Store) OnUsage(rec UsageRecord) {
	// 先记录 别名 -> 上游名 的映射，这样下面通过 ApplyRateLimit 打出的禁用日志能显示上
	// 游名。
	s.recordAlias(rec.Alias, rec.Model)
	model := strings.TrimSpace(rec.Alias)
	if model == "" {
		model = rec.Model
	}
	now := s.now()
	// 提取状态码和 body，让 ApplyRateLimit 能识别 "insufficient balance"（魔粒耗尽），
	// 并把该 key 当天全局禁用。
	var statusCode int
	var body string
	if rec.Failed && rec.Failure != nil {
		statusCode = rec.Failure.StatusCode
		body = rec.Failure.Body
	}
	s.ApplyRateLimit(rec.AuthID, model, rec.ResponseHeaders, statusCode, body, now)
	cfg := s.config()
	// 托管 provider 返回成功（非失败）响应，说明共享的上游额度已恢复：重置指数退避，让
	// 下一次 insufficient_quota 失败重新从配置的基准值开始。只有托管 provider 才触发重
	// 置 —— 非托管兜底（例如阿里）返回的 200 不应该重置一个无关的、已耗尽的托管 provider
	// 的退避。这件事不放在 ApplyRateLimit 里做，因为 429+insufficient_quota 的响应可能带
	// 着 remaining > 0 的限流头（那是单请求速率限制，不是计费额度耗尽），否则每次失败都
	// 会错误地重置退避。
	if !rec.Failed && cfg.ManagesProvider(rec.Provider) {
		// Responses API 的额度失败以 HTTP 200 到达，用量钩子否则会把它误读成健康响应，
		// 把 applyResponsesRateLimit 刚设置的退避/跳过状态清掉。
		if s.consumeResponsesRateLimited(rec.AuthID, now) {
			return
		}
		s.resetCooldownBackoff()
		s.recordSuccess(rec.AuthID)
		s.recordModelUsage(s.displayName(model))
	}
	// 任意 200（无论托管还是非托管）都清空按 provider 的 skip 集，让下一次请求重新去试
	// 免费的托管额度。这就是“优先用免费额度”策略：每个新请求先试托管，只有
	// 429+insufficient_quota 之后才跳到非托管兜底。同时把跨家游标复位到 0（当前实现里
	// 没有任何路径会推进它，因此调度恒定从第一个 provider 开始）；真正跨请求轮转的只有
	// 各家的 key 游标。
	if !rec.Failed {
		s.ClearAllSkippedProviders()
		s.ClearSkipRoundDone()
		s.resetManagedProviderCursor()
	}

	// 429 走的是执行器的错误路径（宿主提前返回，不会调用响应拦截器），所以 OnUsage 是
	// 识别 429 的权威路径。用量钩子对*每一个* provider 都会触发（宿主的用量管理器把每条
	// 记录无条件分发给所有插件），因此同样返回 429+insufficient_quota 的非托管兜底
	// provider（例如阿里百炼）绝不能被施加冷却。用 ManagesProvider 做守卫，只影响被监控
	// 的 provider。
	if rec.Failed && rec.Failure != nil {
		if cfg.ManagesProvider(rec.Provider) {
			// 记录托管 provider 的 429（状态页的当日计数）。
			if rec.Failure.StatusCode == 429 {
				s.recordRateLimit()
			}
			// 401 Unauthorized：api key 无效或已过期。把该凭据全局禁用直到插件重启（午夜
			// 不恢复），这样调度器会跳过它。状态页显示“密钥失效”。
			if rec.Failure.StatusCode == 401 {
				s.recordSeen([]string{rec.AuthID})
				s.disableUnauthorized(rec.AuthID, now)
				return
			}
			// skip_provider_on_429 模式：托管 provider 返回 insufficient_quota 的 429 时，
			// 把它放进按 provider 的 skip 集，后续 SchedulerPick 就会过滤掉它的候选。只跳过
			// 真正 429 的那一家，其他托管 provider 仍然可用。 latch 语义：条目会跨非托管失败
			// 保留，由任意 200（OnUsage）或“托管候选已全部试过”来清空。其他类型的 429
			// （exceeded-today、limit_burst_rate）走常规的日禁用 + 冷却处理。
			//
			// 判据是池级的 IsPoolLevelQuotaExhausted，不是宽口径的 isInsufficientQuota：后
			// 者会把 Responses 的 key 级 rate_limit_exceeded（以 HTTP 200 回送，因此也会走到
			// 这条用量记录）算进来，那只会把整家误摘掉。
			if cfg.SkipProviderOn429 && IsPoolLevelQuotaExhausted(rec.Failure.Body) {
				s.recordSeen([]string{rec.AuthID})
				providerName := extractProviderName(rec.Provider)
				s.SetSkipProvider(providerName)
				s.log.Printf("modelscope-ratelimit: managed 429, skip provider %s model=%s", providerName, s.displayName(model))
			}
			// 冷却兜底：仅在跳过模式关闭时执行（该模式下 429 由 skip 集处理，不用冷却）。
			// SchedulerPick 里的冷却阻塞段在跳过模式开启时同样会被绕过。
			if !cfg.SkipProviderOn429 {
				s.ApplyInsufficientQuotaCooldown(rec.AuthID, model, rec.Failure.StatusCode, rec.Failure.Body, now)
			}
		}
	}
}

// authIDKeys 是把一次响应/流拦截调用关联回具体凭据时，按优先级依次尝试的 metadata
// 键。宿主的上下文快照（Metadata）按只读处理；这些键都不存在时，仍然可以由用量钩子完
// 成关联。
var authIDKeys = []string{
	"auth_id", "AuthID", "authID", "authId", "auth",
	"selected_auth_id", "pinned_auth_id",
}

// AuthIDFromMetadata 以防御方式从响应/流拦截请求携带的宿主上下文快照里取出凭据标识。
func AuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	for _, k := range authIDKeys {
		if v, ok := meta[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// providerIDKeys 是把流或响应拦截调用关联回 provider key 时依次尝试的 metadata 键，供
// skip_provider_on_429 使用。
var providerIDKeys = []string{
	"provider", "Provider", "provider_key", "auth_provider", "AuthProvider", "auth_provider_key",
}

// ProviderFromMetadata 以防御方式从响应/流拦截请求携带的宿主上下文快照里取出
// provider key。
func ProviderFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	for _, k := range providerIDKeys {
		if v, ok := meta[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
