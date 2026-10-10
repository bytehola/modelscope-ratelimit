package ratelimit

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Logger 是最小的日志出口。SDK 胶水层注入基于 host.log 的实现，测试里注入空实现或记录器。
type Logger interface {
	Printf(format string, args ...any)
}

// NoopLogger 丢弃一切日志。
type NoopLogger struct{}

func (NoopLogger) Printf(string, ...any) {}

// keyState 保存单个凭据（auth ID）的禁用状态。
//
// 禁用范围永远是“配置时区里当天剩余时间”。记录的时间点是观察到额度耗尽的时刻。

type keyState struct {
	mu                 sync.RWMutex
	global             *time.Time // 全局（不区分模型）禁用时刻，nil 表示没有
	globalReason       string     // 空串/ReasonUnverified 是当日禁用（午夜恢复）；"unauthorized" 是 401（直到重启）
	cooldown           *time.Time // 短期全局冷却到期时刻（insufficient_quota），nil 表示没有
	successCount       int        // 当日成功请求数（午夜重置）
	magicCubeAvailable int        // available_balance，来自 /openapi/v1/magicubes/balance
	magicCubeTotal     int        // 余额总额
	magicCubeFrozen    int        // 冻结金额
	usagePerDay        int        // currentUsagePerDay，来自 /api/v1/inference/rate-limit
	checkedIn          bool       // 当日是否已签到（/api/v1/users/login/info）
	lastCheckIn        time.Time  // 执行签到的时刻
}

// Store 是所有插件钩子共用的、并发安全的限流状态。
type Store struct {
	cfg atomic.Pointer[Config]
	loc atomic.Pointer[time.Location]

	// managedCursors 保存按 provider 的轮询游标。每个游标索引的是该 provider 完整的、按配置
	// 顺序排列的 key 列表，这样切换 provider（例如某一家耗尽后换下一家）时是从新一家列表里的
	// 正确位置开始，而不是沿用上一家留下的任意值。
	managedCursorsMu sync.Mutex
	managedCursors   map[string]uint64
	// managedProviderCursor 是跨 provider 的游标。当前实现里没有任何路径会推进它（历史上曾由
	// skip OFF 的 429 处理逻辑推进），它只会在任意 200 时被复位为 0，因此调度始终从
	// cfg.Providers 的第一个 provider 开始；真正跨请求轮转的是各家自己的 key 游标（见
	// managedCursors）。
	managedProviderCursor uint64
	// managedOrder 按托管 provider 记住见过的最大的一份“宿主顺序候选 ID 列表”。当
	// providerFullOrder 不可用时（fetcher 未注入或管理接口不可达），pickManaged 用这份快照作
	// 为稳定的“完整”列表，这样 429 重试（候选组会变小）仍然按固定顺序向后扫描并落在下一把 key
	// 上，而不是用被上一次挑选推进过的游标去索引一个变短的列表。每次收到新的候选组都会尝试扩
	// 展它；reconfigure 时重置。
	managedOrderMu sync.Mutex
	managedOrder   map[string][]string

	// nonManagedRR 是非托管兜底候选（情况 1）的轮询游标。它与托管游标分开，所以非托管轮换永
	// 远不会破坏托管 provider 的顺序。
	nonManagedRR atomic.Uint64

	mu   sync.RWMutex
	keys map[string]*keyState

	// seen 记录调度器为托管 provider 见过的每一个凭据 ID，这样即使还没有任何禁用发生，也能报
	// 出“key 总数”。
	seenMu sync.RWMutex
	seen   map[string]struct{}

	// aliasUpstream 把客户端请求的模型别名（也是存取禁用状态和调度器检查时使用的键）映射到解
	// 析后的上游模型名，于是日志和状态页可以显示上游名，而存储键仍保持别名。数据来自用量记
	// 录，它同时携带 Alias 和 Model。
	aliasMu       sync.RWMutex
	aliasUpstream map[string]string

	// modelUsage 为状态页统计按模型的当日用量。
	modelUsageMu sync.RWMutex
	modelUsage   map[string]int // 模型名 -> 请求计数（午夜重置）

	// strategyFetcher 返回宿主内置的路由策略（"round-robin" 或 "fill-first"），由胶水层通过
	// 管理 API 注入。为 nil 或返回空字符串时，hostStrategy 退回 "round-robin"，保证托管挑选不
	// 会因为等待策略结果而阻塞。
	strategyFetcher atomic.Pointer[func() string]

	// providerOrderFetcher 返回 provider 名称到“按 config.yaml 中 api-key 顺序排列的完整
	// auth ID 列表”的映射（包含当前可能被禁用或已试过的 key）。调度器把轮询游标索引到这份
	// 完整列表上并跳过不存在的项，因此 429 重试和禁用 key 都不会打乱轮换顺序。
	providerOrderFetcher atomic.Pointer[func() map[string][]string]
	// Modelscope API 取数函数（由胶水层注入，直接用 net/http，走 cookie 鉴权）
	loginInfoFetcher   atomic.Pointer[LoginInfoFetcher]
	balanceFetcher     atomic.Pointer[BalanceFetcher]
	rateLimitFetcher   atomic.Pointer[RateLimitFetcher]
	midnightSignInFunc atomic.Pointer[MidnightSignInFunc]

	// backoffMu 保护全局的 insufficient_quota 指数退避档位。Modelscope 的额度在所有 key 之间
	// 共享，所以退避是全局的（不是逐 key）：每一轮连续的额度失败（判据 isInsufficientQuota：
	// Chat 的 429+insufficient_quota，或 Responses 以 200 回送的速率拒绝）让冷却时长翻倍，
	// 封顶 maxInsufficientQuotaCooldown。
	backoffMu       sync.Mutex
	cooldownLevel   float64   // 当前退避间隔（秒）；0 表示空闲
	cooldownLevelAt time.Time // 档位最近一次推进的时间
	// cooldownExpiresAt 是当前这一轮冷却真正到期的时刻（等于档位时长，无抖动）。它既是“同一
	// 轮”的判定锚点，也是这一轮里每把 key 的冷却到期时刻，所以同轮所有 key 在同一瞬间到期
	// （不漂移）。早于该时刻的失败算同一个共享额度轮次（保持不翻倍）；晚于该时刻的失败算新一
	// 轮（翻倍）。
	cooldownExpiresAt time.Time

	// cooldownCount 与 cooldownWaitNanos 为状态页统计当日 insufficient_quota 情况：冷却被触
	// 发的次数，以及在 SchedulerPick 里累计的阻塞时长。两者都在跨日时（PruneAll）重置，与退
	// 避档位一致。
	cooldownStatsMu   sync.Mutex
	cooldownCount     int64
	cooldownWaitNanos int64 // 有 goroutine 处于阻塞期间的墙钟纳秒数
	cooldownStatsDay  time.Time
	successCount      int64     // 当日托管成功请求数（状态页）
	rateLimitCount    int64     // 当日来自托管 provider 的 429 响应数
	blockActive       int       // 当前正处于冷却睡眠中的 goroutine 数量
	blockStart        time.Time // 本轮阻塞的起点（blockActive 由 0 变 1 时）

	// skipProviders 是 skip_provider_on_429 模式下按 provider 的 latch 跳过集合。OnUsage 在托管
	// 凭据返回**池级** insufficient_quota 时把该 provider 名加入集合（Chat 的 429 JSON 与
	// Responses 的 200 + SSE response.failed 都算，判据见 IsPoolLevelQuotaExhausted；Responses 的
	// key 级 rate_limit_exceeded 不在此列）。SchedulerPick 会过滤掉集合内 provider 的
	// 候选。粒度是 provider：只有真正池级耗尽的那一家被跳过，其他托管 provider 仍
	// 可用。清空时机：任意 200（让下一次请求重新探测免费的托管额度），或托管候选已全部试过
	// （managed==0）。
	skipMu        sync.RWMutex
	skipProviders map[string]struct{}

	// skipRoundDone 记录 skip ON 模式是否已经走完一整轮 provider 轮换（每家都被跳过一次）。
	// 目前它只是状态标记：没有任何读取方（SkipRoundDone() 无调用点），因此不会真的改变行为，
	// SetSkipProvider 依旧按 skip 集工作。保留它是为了将来“整轮跳过后转为 key 穷尽模式”的扩展
	// 点。任意 200 会清除它。
	skipRoundDone atomic.Bool

	// responsesRateLimited 记录产生过 Responses API response.failed/rate_limit_exceeded 事件
	// 的 key。由于这类失败以 HTTP 200 的流返回，随后的用量钩子否则会把它当成健康响应，从而清
	// 掉刚设置的退避/跳过状态。
	responsesRateLimitMu sync.Mutex
	responsesRateLimited map[string]time.Time
	// responsesRateLimitApplied 避免重复记账：执行器已经调用过 ApplyResponsesRateLimit，宿主
	// 随后还会对同一个响应跑响应/流拦截器。
	responsesRateLimitAppliedMu sync.Mutex
	responsesRateLimitApplied   map[string]time.Time

	clock func() time.Time // 可在测试中注入，默认是 time.Now

	stopMu  sync.Mutex
	started bool
	stop    chan struct{}
	log     Logger
}

// NewStore 创建一个空的 store。在调用 Reconfigure 之前，时区默认为 UTC。
func NewStore(log Logger) *Store {
	if log == nil {
		log = NoopLogger{}
	}
	utc := time.UTC
	s := &Store{
		keys:                      make(map[string]*keyState),
		seen:                      make(map[string]struct{}),
		aliasUpstream:             make(map[string]string),
		modelUsage:                make(map[string]int),
		managedCursors:            make(map[string]uint64),
		managedOrder:              make(map[string][]string),
		responsesRateLimited:      make(map[string]time.Time),
		responsesRateLimitApplied: make(map[string]time.Time),
		clock:                     time.Now,
		stop:                      make(chan struct{}),
		log:                       log,
	}
	s.loc.Store(utc)
	s.cfg.Store(DefaultConfig())
	return s
}

// SetClock 覆盖取时函数（“现在”的时刻），仅供测试使用。
func (s *Store) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// SetLocation 覆盖计算日界所用的时区。测试使用固定时区，避免依赖操作系统的 tz 数据库。
func (s *Store) SetLocation(loc *time.Location) {
	if loc != nil {
		s.loc.Store(loc)
	}
}

// SetStrategyFetcher 注入一个返回宿主内置路由策略的回调。胶水层通过管理 API
// （GET /v0/management/routing/strategy）获取并做短缓存。从未注入或获取失败时，
// hostStrategy 回退为 "round-robin"。
func (s *Store) SetStrategyFetcher(f func() string) {
	if f == nil {
		s.strategyFetcher.Store(nil)
		return
	}
	fp := f
	s.strategyFetcher.Store(&fp)
}

// hostStrategy 返回宿主内置的凭据选择策略，取不到时返回 "round-robin"。非托管兜底候选使用
// 它，从而让插件复刻宿主自身的轮换语义，而不是强加一个固定顺序。
func (s *Store) hostStrategy() string {
	if fp := s.strategyFetcher.Load(); fp != nil && *fp != nil {
		if v := (*fp)(); v == "fill-first" {
			return "fill-first"
		}
	}
	return "round-robin"
}

// managedStrategy 返回配置里针对托管 provider 的选择策略，取值限定为 "round-robin"（默认）
// 或 "fill-first"。
func (s *Store) managedStrategy() string {
	switch s.config().CredentialStrategy {
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	default:
		return "round-robin"
	}
}

// SetProviderOrderFetcher 注入一个返回“provider 名称 -> 完整的配置顺序 auth ID 列表”的回
// 调。胶水层通过管理 API 获取，缓存策略与解析 key 时相同。未注入时，pickManaged 退化为在宿主
// 顺序的候选上做简单轮询。
func (s *Store) SetProviderOrderFetcher(f func() map[string][]string) {
	if f == nil {
		s.providerOrderFetcher.Store(nil)
		return
	}
	fp := f
	s.providerOrderFetcher.Store(&fp)
}

// providerFullOrder 返回某个 provider 完整的、按配置顺序排列的 auth ID 列表（包含当前被禁用
// 或已试过的 key）；fetcher 未注入或该 provider 未知时返回 nil。
func (s *Store) providerFullOrder(providerCfgName string) []string {
	fp := s.providerOrderFetcher.Load()
	if fp == nil || *fp == nil {
		return nil
	}
	m := (*fp)()
	if m == nil {
		return nil
	}
	return m[strings.ToLower(strings.TrimSpace(providerCfgName))]
}

// NextProviderKey 按该 provider 自己的游标（轮询）或 fill-first 策略返回下一把 auth ID，并推
// 进游标，使下一次调用返回再下一把 key。当 fetcher 提供的完整顺序可用时，复用 pickManaged 那
// 套“顺序扫描 + 跳过不存在项”的逻辑；否则直接在 ids 切片上取模。
func (s *Store) NextProviderKey(provider string, ids []string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || len(ids) == 0 {
		return ""
	}
	full := s.providerFullOrder(provider)
	if len(full) == 0 {
		full = s.observedOrder(provider)
	}
	if len(full) == 0 {
		full = ids
	}
	present := make(map[string]bool, len(ids))
	for _, id := range ids {
		present[id] = true
	}
	if s.managedStrategy() == "fill-first" {
		for _, id := range full {
			if present[id] {
				return id
			}
		}
		return ids[0]
	}
	s.managedCursorsMu.Lock()
	defer s.managedCursorsMu.Unlock()
	cursor := s.managedCursors[provider]
	start := int(cursor % uint64(len(full)))
	for step := 0; step < len(full); step++ {
		pos := (start + step) % len(full)
		if present[full[pos]] {
			s.managedCursors[provider] = uint64(pos + 1)
			return full[pos]
		}
	}
	return ids[0]
}

// recordObservedOrder 用一份候选组扩展按 provider 的观测顺序快照。首次请求会带上宿主能路由的全
// 部 key（按宿主顺序）；429 重试带来的则是它的真子集（已试过的那把 key 不在里面）。只有当新组
// 不小于已记忆的列表时才扩展它，从而保住完整顺序、让兜底扫描可以跳过缺失项。可并发调用，也可在
// managedCursorsMu 之外调用（它自带一把锁）。
func (s *Store) recordObservedOrder(provider string, group []Candidate) {
	if len(group) == 0 {
		return
	}
	key := strings.ToLower(strings.TrimSpace(provider))
	s.managedOrderMu.Lock()
	defer s.managedOrderMu.Unlock()
	cur := s.managedOrder[key]
	if len(group) < len(cur) {
		// 重试用的子集：保留更完整的那份记忆列表，让扫描始终有一个稳定的超集可索引。
		return
	}
	// 规模相等或更大的候选组：在保持原有顺序的前提下重建，再把新见到的 ID 追加到末尾
	// （覆盖插件运行期间新增 key 的情况）。
	seen := make(map[string]bool, len(cur))
	out := make([]string, 0, len(group))
	for _, id := range cur {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, c := range group {
		if !seen[c.ID] {
			seen[c.ID] = true
			out = append(out, c.ID)
		}
	}
	s.managedOrder[key] = out
}

// observedOrder 返回按 provider 记录的观测顺序快照的副本；还没有记录过时返回 nil。只读，
// 可在 managedCursorsMu 下安全调用。
func (s *Store) observedOrder(provider string) []string {
	key := strings.ToLower(strings.TrimSpace(provider))
	s.managedOrderMu.Lock()
	defer s.managedOrderMu.Unlock()
	src := s.managedOrder[key]
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// pickManaged 从一个非空的托管 provider 候选组里挑一个，即使在 429 重试和存在禁用 key 的情
// 况下也保持 config.yaml 里 api-key 的顺序。
//
// 轮询游标索引的是*完整*的配置顺序列表（包含可能被禁用、或已进宿主 tried 集合的 key）。每次
// 调用都从游标位置向后扫描，跳过当前候选组里不存在的项，落在下一把可用 key 上。这样一次 429
// 重试（候选列表会变短）仍然按配置顺序选下一把 key，而不是被 cursor % len(变短的列表) 随机
// 定位。
//
// fill-first 永远按配置顺序选第一把可用 key。
//
// 当 fetcher 提供的完整顺序不可用时，插件退化为按 provider 维护的“观测顺序”快照（由历次候选
// 组累积而成），重试仍然扫描这个稳定超集并跳过缺失的 key。只有第一次调用（既没有 fetcher，也
// 还没有快照）才会退化成在当前候选组上做简单轮询。
func (s *Store) pickManaged(group []Candidate, providerCfgName string) (Candidate, bool) {
	if len(group) == 0 {
		return Candidate{}, false
	}
	strategy := s.managedStrategy()

	// 在获取游标互斥锁*之前*扩展观测顺序快照，这样短暂的 managedCursorsMu 临界区永远不会去等
	// 这次 map 写入。当 fetcher 不可用（未注入或管理接口暂时失败）时，这份快照就是兜底的“完整”
	// 列表。
	s.recordObservedOrder(providerCfgName, group)

	if strategy == "fill-first" {
		// 永远按配置顺序选第一把可用 key。
		full := s.providerFullOrder(providerCfgName)
		if len(full) == 0 {
			full = s.observedOrder(providerCfgName)
		}
		if len(full) > 0 {
			present := make(map[string]bool, len(group))
			byID := make(map[string]Candidate, len(group))
			for _, c := range group {
				present[c.ID] = true
				byID[c.ID] = c
			}
			for _, id := range full {
				if present[id] {
					return byID[id], true
				}
			}
		}
		return group[0], true
	}

	// 在获取游标互斥锁*之前*取完整的配置顺序 key 列表：providerOrderFetcher 是外部注入的回
	// 调，核心层无法约束它的耗时（胶水层当前的实现读缓存并异步刷新，不阻塞；但持锁调用外部回
	// 调本身就是风险，一旦它同步访问管理接口，所有 provider 的全部托管挑选都会被一起卡住）。完
	// 整列表在本次调用期间不会变化，因此在锁外计算是安全的。
	full := s.providerFullOrder(providerCfgName)

	// 按 provider 的游标由 managedCursorsMu 保护，使“读取-扫描-写回”这一段是原子的。临界区很
	// 短（一次扫描 + 一次 map 写入），并且绝不做 I/O。
	s.managedCursorsMu.Lock()
	defer s.managedCursorsMu.Unlock()

	cursor := s.managedCursors[providerCfgName]

	if len(full) == 0 {
		// 没有 fetcher 提供的顺序：使用由历次候选组累积出的观测顺序快照。它是当前候选组（可能
		// 因重试而变小）的超集，所以下面“扫描并跳过缺失项”的逻辑仍然会落在记忆顺序里的下一把
		// key 上，而不是用旧游标去索引一个变短的列表。
		full = s.observedOrder(providerCfgName)
	}

	if len(full) > 0 {
		// 完整顺序路径：在按配置顺序排列的完整列表（含已禁用/已试过的 key）里从游标位置向后扫
		// 描，跳过不存在的项，这样重试和禁用 key 都不会打乱轮换。
		present := make(map[string]bool, len(group))
		byID := make(map[string]Candidate, len(group))
		for _, c := range group {
			present[c.ID] = true
			byID[c.ID] = c
		}
		start := int(cursor % uint64(len(full)))
		found := -1
		for step := 0; step < len(full); step++ {
			pos := (start + step) % len(full)
			if present[full[pos]] {
				found = pos
				break
			}
		}
		if found < 0 {
			return group[0], true
		}
		s.managedCursors[providerCfgName] = uint64(found + 1)
		return byID[full[found]], true
	}

	// 第一次调用且既没有 fetcher 也没有观测快照：在当前这份宿主顺序的候选组上，用该 provider
	// 自己的游标做简单轮询（与其他 provider 互不影响）。在快照建立之前，这个分支每个 provider
	// 只会走一次。
	n := cursor + 1
	s.managedCursors[providerCfgName] = n
	idx := int((n - 1) % uint64(len(group)))
	return group[idx], true
}

// managedProviderStart 返回下一次调度挑选的起始 provider 下标（对 managedProviderCursor 取
// 模）。由于当前没有任何路径推进那个游标，返回值恒为 0，也就是严格按配置里的第一个 provider
// 开始；各家的 key 游标彼此独立。
func (s *Store) managedProviderStart(providerCount int) int {
	if providerCount <= 0 {
		return 0
	}
	s.managedCursorsMu.Lock()
	defer s.managedCursorsMu.Unlock()
	return int(s.managedProviderCursor % uint64(providerCount))
}

// advanceManagedProvider 把跨 provider 游标推进到下一家。当前没有任何调用方：provider 轮换已
// 经改为“本家 key 逐个试完再换下一家”，不再需要在请求内切换起点。保留它是为了让
// resetManagedProviderCursor 与 managedProviderStart 的语义仍然成对。
func (s *Store) advanceManagedProvider() {
	s.managedCursorsMu.Lock()
	s.managedProviderCursor++
	s.managedCursorsMu.Unlock()
}

// resetManagedProviderCursor 把跨 provider 游标复位到第一家。OnUsage 在任意 200（托管或非托
// 管）时调用它，保证下一次请求总是从第一个托管 provider 重新开始；跨请求继续轮转的只有各家的
// key 游标。
func (s *Store) resetManagedProviderCursor() {
	s.managedCursorsMu.Lock()
	s.managedProviderCursor = 0
	s.managedCursorsMu.Unlock()
}

// Now 返回当前时钟取值（胶水层渲染状态页时使用）。
func (s *Store) Now() time.Time { return s.now() }

func (s *Store) location() *time.Location {
	if l := s.loc.Load(); l != nil {
		return l
	}
	return time.UTC
}

func (s *Store) config() *Config {
	if c := s.cfg.Load(); c != nil {
		return c
	}
	return DefaultConfig()
}

func (s *Store) now() time.Time {
	return s.clock()
}

// Reconfigure 应用一份新配置（由 plugin.reconfigure 和首次 plugin.register 调用）。它会解析
// 时区，系统 tz 数据库不可用时退回 UTC+8（中国标准时间）。
func (s *Store) Reconfigure(cfg *Config) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	s.cfg.Store(cfg)
	s.loc.Store(resolveLocation(cfg.Timezone, s.log))
	// 重置轮换游标：provider 集合可能已经变了，旧配置留下的游标值没有意义。
	s.managedCursorsMu.Lock()
	s.managedCursors = make(map[string]uint64)
	s.managedProviderCursor = 0
	s.managedCursorsMu.Unlock()
	s.nonManagedRR.Store(0)
	// 观测顺序快照也要重置：provider 集合变化或 api-key 重新排序后，记忆中的宿主顺序列表已经过
	// 期。
	s.managedOrderMu.Lock()
	s.managedOrder = make(map[string][]string)
	s.managedOrderMu.Unlock()
	// 重置指数退避档位，让改动后的 insufficient_quota_cooldown 基准立即生效，而不是接着旧的档
	// 位继续。
	s.backoffMu.Lock()
	s.cooldownLevel = 0
	s.cooldownLevelAt = time.Time{}
	s.cooldownExpiresAt = time.Time{}
	s.backoffMu.Unlock()
	// 清掉上一份配置留下的逐 key 短期冷却：insufficient_quota_cooldown 基准变了（或切换了跳过
	// 模式）之后它们已经过期。日级全局禁用保留（依然有效）。
	s.clearAllCooldowns()
	// 清空按 provider 的 skip 集：旧配置留下的条目会让下一次挑选无缘无故地跳过托管 provider。
	s.ClearAllSkippedProviders()
	s.responsesRateLimitMu.Lock()
	s.responsesRateLimited = make(map[string]time.Time)
	s.responsesRateLimitMu.Unlock()
	s.responsesRateLimitAppliedMu.Lock()
	s.responsesRateLimitApplied = make(map[string]time.Time)
	s.responsesRateLimitAppliedMu.Unlock()
}

const responsesRateLimitWindow = time.Minute

// markResponsesRateLimited 给某把 key 打上标记（存续到下一次用量回调），避免以 200 返回的
// Responses 失败被误当成健康响应。
func (s *Store) markResponsesRateLimited(authID string, now time.Time) {
	if authID == "" {
		return
	}
	s.responsesRateLimitMu.Lock()
	s.responsesRateLimited[authID] = now.Add(responsesRateLimitWindow)
	s.responsesRateLimitMu.Unlock()
}

// markResponsesRateLimitApplied 记录“该 key 已由 ApplyResponsesRateLimit 登记过状态”，这样宿主
// 驱动的响应/流拦截器不会在同一次响应上重复更新冷却/跳过状态。
func (s *Store) markResponsesRateLimitApplied(authID string, now time.Time) {
	if authID == "" {
		return
	}
	s.responsesRateLimitAppliedMu.Lock()
	s.responsesRateLimitApplied[authID] = now.Add(responsesRateLimitWindow)
	s.responsesRateLimitAppliedMu.Unlock()
}

// consumeResponsesRateLimitApplied 消费掉这个“已登记”标记。
func (s *Store) consumeResponsesRateLimitApplied(authID string, now time.Time) bool {
	s.responsesRateLimitAppliedMu.Lock()
	defer s.responsesRateLimitAppliedMu.Unlock()
	expiry, ok := s.responsesRateLimitApplied[authID]
	if !ok {
		return false
	}
	delete(s.responsesRateLimitApplied, authID)
	return !expiry.IsZero() && now.Before(expiry)
}

// consumeResponsesRateLimited 为一次用量回调消费掉该标记（读取并清除）。
func (s *Store) consumeResponsesRateLimited(authID string, now time.Time) bool {
	s.responsesRateLimitMu.Lock()
	defer s.responsesRateLimitMu.Unlock()
	expiry, ok := s.responsesRateLimited[authID]
	if !ok {
		return false
	}
	delete(s.responsesRateLimited, authID)
	return !expiry.IsZero() && now.Before(expiry)
}

// Start 启动后台 goroutine，定期清理已过期的禁用状态，这样即使完全没有请求进来，状态也会在
// 00:00 复位。可重复调用。
func (s *Store) Start() {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if s.started {
		return
	}
	s.stop = make(chan struct{})
	s.started = true
	go s.cleaner()
}

// Shutdown 停止后台清理协程。可重复调用。
func (s *Store) Shutdown() {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if !s.started {
		return
	}
	s.started = false
	close(s.stop)
}

// cleaner 周期性清理掉已经跨过自然日的禁用状态。
func (s *Store) cleaner() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.PruneAll(now)
		}
	}
}

// resolveLocation 加载 IANA 时区，失败时退回 UTC+8。
func resolveLocation(name string, log Logger) *time.Location {
	if name == "" {
		name = "Asia/Shanghai"
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	log.Printf("modelscope-ratelimit: timezone %q unavailable, falling back to UTC+8", name)
	return time.FixedZone("CST", 8*3600)
}

// sameDay 判断 t1 与 t2 在时区 loc 下是否属于同一个自然日。
func sameDay(t1, t2 time.Time, loc *time.Location) bool {
	y1, m1, d1 := t1.In(loc).Date()
	y2, m2, d2 := t2.In(loc).Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// active 判断“在 at 时刻观察到的禁用”在 now 时刻是否仍然有效：必须与 now 属于同一自然日，且
// 不能来自未来。
func active(at, now time.Time, loc *time.Location) bool {
	return sameDay(at, now, loc) && !now.Before(at)
}

// getKey 返回 authID 对应的 keyState，不会创建它（不存在时返回 nil）。
func (s *Store) getKey(authID string) *keyState {
	s.mu.RLock()
	ks := s.keys[authID]
	s.mu.RUnlock()
	return ks
}

// getOrCreateKey 返回 authID 对应的 keyState，必要时创建。
func (s *Store) getOrCreateKey(authID string) *keyState {
	s.mu.RLock()
	ks, ok := s.keys[authID]
	s.mu.RUnlock()
	if ok {
		return ks
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 拿到写锁之后再检查一次，以处理并发创建的竞态。
	if ks, ok = s.keys[authID]; ok {
		return ks
	}
	ks = &keyState{}
	s.keys[authID] = ks
	return ks
}

// deleteKey 删除一个已经为空的 keyState 条目。
func (s *Store) deleteKey(authID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ks, ok := s.keys[authID]; ok {
		ks.mu.RLock()
		empty := ks.global == nil && ks.cooldown == nil
		ks.mu.RUnlock()
		if empty {
			delete(s.keys, authID)
		}
	}
}

// isDisabled 判断 authID 当前是否处于全局禁用状态。已经跨日的旧日禁用记录不再算数，除非原因
// 是 "unauthorized"；后台清理协程会移除过期的日级记录。
func (s *Store) IsKeyDisabled(authID, model string, now time.Time) bool {
	return s.isDisabled(authID, model, now)
}

func (s *Store) isDisabled(authID, model string, now time.Time) bool {
	ks := s.getKey(authID)
	if ks == nil {
		return false
	}
	loc := s.location()
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	// 这里故意不检查 insufficient_quota 冷却：Modelscope 的额度在所有 key 和所有 provider
	// 之间共享，所以冷却是全局的而不是逐 key 的，改由 SchedulerPick 在挑选之前按最长的有效
	// 冷却统一阻塞一次；阻塞结束后冷却已到期，key 重新可用。日级全局禁用仍然在这里检查。
	if ks.global != nil && (ks.globalReason == "unauthorized" || active(*ks.global, now, loc)) {
		return true
	}
	return false
}

// recordSeen 登记调度器见过的凭据 ID，以便能报出 key 总数。
func (s *Store) recordSeen(ids []string) {
	if len(ids) == 0 {
		return
	}
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	for _, id := range ids {
		if id != "" {
			s.seen[id] = struct{}{}
		}
	}
}

// recordAlias 记录“该 alias 解析到这个上游模型名”，这样日志和状态页可以显示上游名，而禁用状态
// 依旧以调度器检查的那个别名作为键。可并发调用；任一侧为空或两侧相同时是空操作。
func (s *Store) recordAlias(alias, upstream string) {
	alias = strings.TrimSpace(alias)
	upstream = strings.TrimSpace(upstream)
	if alias == "" || upstream == "" || alias == upstream {
		return
	}
	s.aliasMu.Lock()
	s.aliasUpstream[alias] = upstream
	s.aliasMu.Unlock()
}

// recordModelUsage 累加当日的按模型请求计数。
func (s *Store) recordModelUsage(model string) {
	if model == "" {
		return
	}
	s.modelUsageMu.Lock()
	s.modelUsage[model]++
	s.modelUsageMu.Unlock()
}

// ModelUsageStats 返回当日的按模型请求计数。
func (s *Store) ModelUsageStats() map[string]int {
	s.modelUsageMu.RLock()
	defer s.modelUsageMu.RUnlock()
	out := make(map[string]int, len(s.modelUsage))
	for k, v := range s.modelUsage {
		out[k] = v
	}
	return out
}

// displayName 在已知映射时返回别名对应的上游模型名，否则返回别名本身。只用于展示（日志、状态
// 页）；底层的禁用存储键不受影响。
func (s *Store) displayName(model string) string {
	if s == nil {
		return model
	}
	s.aliasMu.RLock()
	upstream, ok := s.aliasUpstream[model]
	s.aliasMu.RUnlock()
	if ok && upstream != "" {
		return upstream
	}
	return model
}

// seenIDs 返回所有已观察到的凭据 ID 的快照。
func (s *Store) seenIDs() []string {
	s.seenMu.RLock()
	ids := make([]string, 0, len(s.seen))
	for id := range s.seen {
		ids = append(ids, id)
	}
	s.seenMu.RUnlock()
	return ids
}

// Overview 返回 now 时刻的整体 key 数量：观察到的总数、处于有效全局禁用的 key 数（与调度器的
// isDisabled 视角一致），以及可用数（总数 - 禁用数）。短期冷却由 Snapshot 单独汇报，因此这里的
// “可用数”永远和调度器实际会选到的结果一致。
func (s *Store) Overview(now time.Time) (total, disabled, available int) {
	ids := s.seenIDs()
	total = len(ids)
	for _, id := range ids {
		if s.isDisabled(id, "", now) {
			disabled++
		}
	}
	available = total - disabled
	return
}

// Counts 返回给定模型下已观察到的 key 数量。全局禁用对所有模型生效，所以模型名只影响日志和汇总
// 文案。
func (s *Store) Counts(model string, now time.Time) (disabled, available, total int) {
	ids := s.seenIDs()
	total = len(ids)
	for _, id := range ids {
		if s.isDisabled(id, model, now) {
			disabled++
		}
	}
	available = total - disabled
	return
}

// ModelRemaining 返回所有仍有效（未禁用）key 上可用魔粒余额的总和。调度日志用它报告“还有多少
// 余额在可用 key 上”。
func (s *Store) ModelRemaining(model string, now time.Time) int {
	ids := s.seenIDs()
	loc := s.location()
	total := 0
	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.RLock()
		skip := ks.global != nil && active(*ks.global, now, loc)
		if !skip {
			total += ks.magicCubeAvailable
		}
		ks.mu.RUnlock()
	}
	return total
}

// ReasonUnverified 标记 403 + "账号未验证邮箱"造成的禁用。它与魔粒耗尽一样是日禁用（午夜恢复），
// 只比默认原因多一个展示标签，让状态页能写明具体原因而不是笼统的"已禁用"。
const ReasonUnverified = "unverified"

// disableGlobal 是当日全局禁用的入口（不带原因标签），魔粒耗尽这类日额度条件走它。
func (s *Store) disableGlobal(authID, model string, now time.Time) {
	s.disableGlobalWithReason(authID, model, "", now)
}

// disableGlobalWithReason 把 authID 标记为当天剩余时间内全局禁用，并保留最早的观察时刻。model
// 参数只是为了兼容调用点和调试上下文；全局禁用对该凭据的所有请求模型都生效。
//
// reason 只影响状态展示：唯一带跨日语义的取值是 "unauthorized"（401，直到插件重启），其余取值
// （含 ReasonUnverified）都按当日禁用处理，午夜由 PruneAll 清除。同一个自然日内重复命中时保留
// 最早的时刻与最初的原因，避免后来的失败把禁用时间一路推后。
func (s *Store) disableGlobalWithReason(authID, model, reason string, now time.Time) {
	ks := s.getOrCreateKey(authID)
	loc := s.location()
	ks.mu.Lock()
	if ks.global != nil && active(*ks.global, now, loc) {
		ks.mu.Unlock()
		return // 当天已禁用；保留最早的时间
	}
	t := now
	ks.global = &t
	ks.globalReason = reason
	ks.mu.Unlock()
	if reason == "" {
		s.log.Printf("modelscope-ratelimit: key %s globally disabled", authID)
		return
	}
	s.log.Printf("modelscope-ratelimit: key %s globally disabled (%s)", authID, reason)
}

// DisableGlobal 对外暴露的当天全局禁用入口。胶水层用它把一次 insufficient balance 禁用传播到
// 跨 provider 共享同一原始 api key 的所有 auth ID 上，这样核心层无需知道原始 key，同时账号级的
// 禁用状态仍然只有一份。
func (s *Store) DisableGlobal(authID, model string, now time.Time) {
	s.disableGlobal(authID, model, now)
}

// DisableGlobalWithReason 与 DisableGlobal 相同，只是额外带上一个展示用的原因标签，供胶水层把
// 账号级禁用广播给共享同一原始 api key 的其它 auth ID 时保持同一个原因（见 ReasonUnverified）。
func (s *Store) DisableGlobalWithReason(authID, model, reason string, now time.Time) {
	s.disableGlobalWithReason(authID, model, reason, now)
}

// disableUnauthorized 把 authID 全局禁用直到插件重启（午夜不恢复），原因是该 api key 返回了
// 401（密钥无效或已过期）。状态页通过 GlobalReason 显示“密钥失效”。
func (s *Store) disableUnauthorized(authID string, now time.Time) {
	ks := s.getOrCreateKey(authID)
	loc := s.location()
	ks.mu.Lock()
	if ks.global != nil && (ks.globalReason == "unauthorized" || active(*ks.global, now, loc)) {
		ks.mu.Unlock()
		return // 已经禁用；保留最早的时间
	}
	t := now
	ks.global = &t
	ks.globalReason = "unauthorized"
	ks.mu.Unlock()
	s.log.Printf("modelscope-ratelimit: key %s disabled (unauthorized: secret may be invalid)", authID)
}

// maxInsufficientQuotaCooldown 是单次 insufficient_quota 冷却时长的上限（秒）。指数退避每轮翻
// 倍但不会超过该值，因此 SchedulerPick 单次的阻塞时间也不会超过它。
const maxInsufficientQuotaCooldown = 60

// setCooldown 用指数退避把某把 key 在所有模型上临时禁用。用于托管 key 返回 429 且 body 是
// "insufficient_quota"（例如阿里 Model Studio 的额度耗尽）的场景。
//
// 由于 Modelscope 的额度在所有 key 之间共享，退避档位是全局的：每多一轮连续的 429 就让间隔翻
// 倍（基准 -> 2x -> 4x -> ...），封顶 maxInsufficientQuotaCooldown。“一轮”的判定依据是上一轮冷
// 却窗口是否已经过期：同一个窗口内多把 key 一起失败（共享额度一起耗尽）保持当前档位，避免把一
// 轮算成多轮；窗口过期之后再失败才推进到下一档（翻倍）。
//
// 出现健康响应（额度恢复，见 ApplyRateLimit）以及跨日（PruneAll）时，档位重置回基准值。
func (s *Store) setCooldown(authID string, base float64, now time.Time) {
	if authID == "" || base <= 0 {
		return
	}
	s.backoffMu.Lock()
	level := s.cooldownLevel
	advanced := false
	switch {
	case level <= 0:
		// 退避复位之后的第一次失败。
		level = base
		advanced = true
	case now.Before(s.cooldownExpiresAt):
		// 仍在当前这一轮的冷却窗口内：同一份额度下多把 key 一起失败算一轮。既要保持档位不
		// 变，也要保持这一轮的锚点（cooldownExpiresAt）不变，避免把一轮数成多轮、或把窗口整
		// 体往后推。下面设置的逐 key 冷却同样锚定到这个到期时刻，因此同一轮里所有 key 在同一
		// 瞬间解封（不漂移、不过度升级）。
	default:
		// 上一轮窗口已过期 —— 是新一轮失败，翻倍并封顶。
		level = min(level*2, maxInsufficientQuotaCooldown)
		advanced = true
	}
	if advanced {
		s.cooldownLevel = level
		s.cooldownLevelAt = now
		expiry := time.Duration(level * float64(time.Second))
		if expiry > maxInsufficientQuotaCooldown*time.Second {
			expiry = maxInsufficientQuotaCooldown * time.Second
		}
		s.cooldownExpiresAt = now.Add(expiry)
	}
	// dur 只是用于日志输出的退避时长。
	dur := time.Duration(level * float64(time.Second))
	// 把逐 key 冷却锚定到“这一轮的到期时刻”，这样同一轮内所有 key 一起解封。这是对之前漂移问题
	// 的修复：同轮的新 key 过去会被设成 now+level（更晚的时刻），把实际阻塞推到了锚点之后，进而
	// 造成过度升级。
	until := s.cooldownExpiresAt
	s.backoffMu.Unlock()
	ks := s.getOrCreateKey(authID)
	ks.mu.Lock()
	ks.cooldown = &until
	ks.mu.Unlock()
	s.cooldownStatsMu.Lock()
	s.cooldownCount++
	s.cooldownStatsMu.Unlock()
	s.log.Printf("modelscope-ratelimit: key %s cooldown %s (insufficient_quota)", authID, dur.Truncate(time.Millisecond))
}

// resetCooldownBackoff 清除全局指数退避档位。观察到健康的托管响应时调用，让下一次
// insufficient_quota 失败重新从配置的基准值开始。
func (s *Store) resetCooldownBackoff() {
	s.backoffMu.Lock()
	s.cooldownLevel = 0
	s.cooldownLevelAt = time.Time{}
	s.cooldownExpiresAt = time.Time{}
	s.backoffMu.Unlock()
}

// clearAllCooldowns 清空所有逐 key 的短期冷却。由 Reconfigure 调用，避免 insufficient_quota
// _cooldown 基准变化或切换跳过模式后，Snapshot 视图里还带着过期冷却。日级全局禁用是有意保留
// 的（到午夜或重启之前仍然有效）。
func (s *Store) clearAllCooldowns() {
	s.mu.RLock()
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.Lock()
		ks.cooldown = nil
		ks.mu.Unlock()
	}
}

// CooldownStats 返回当日 insufficient_quota 冷却触发次数，以及在 SchedulerPick 里累计的阻塞时
// 长。
func (s *Store) CooldownStats() (count int, wait time.Duration) {
	s.cooldownStatsMu.Lock()
	c, w := s.cooldownCount, s.cooldownWaitNanos
	s.cooldownStatsMu.Unlock()
	return int(c), time.Duration(w)
}

// blockEnter 标记一次冷却睡眠开始。活跃计数从 0 变 1 时记录墙钟起点；并发的睡眠（计数大于 1）
// 不会移动起点 —— 统计的是所有阻塞区间的并集，而不是各自时长的累加。
func (s *Store) blockEnter() {
	s.cooldownStatsMu.Lock()
	if s.blockActive == 0 {
		s.blockStart = time.Now()
	}
	s.blockActive++
	s.cooldownStatsMu.Unlock()
}

// blockLeave 标记一次冷却睡眠结束。活跃计数归零时，把自 blockStart 起经过的墙钟时间累加进去。
// 这样得到的是所有并发阻塞区间的并集，避免把每个 goroutine 的睡眠时长各自累加而虚高。
func (s *Store) blockLeave() {
	s.cooldownStatsMu.Lock()
	s.blockActive--
	if s.blockActive <= 0 {
		s.blockActive = 0
		if !s.blockStart.IsZero() {
			s.cooldownWaitNanos += int64(time.Since(s.blockStart))
			s.blockStart = time.Time{}
		}
	}
	s.cooldownStatsMu.Unlock()
}

// recordSuccess 累加全局和单 key 的当日成功计数。由 OnUsage 在未失败的托管响应上调用。
func (s *Store) recordSuccess(authID string) {
	s.cooldownStatsMu.Lock()
	s.successCount++
	s.cooldownStatsMu.Unlock()
	ks := s.getOrCreateKey(authID)
	ks.mu.Lock()
	ks.successCount++
	ks.mu.Unlock()
}

// SuccessStats 返回当日托管 provider 请求的成功数。
func (s *Store) SuccessStats() int {
	s.cooldownStatsMu.Lock()
	n := s.successCount
	s.cooldownStatsMu.Unlock()
	return int(n)
}

// recordRateLimit 累加托管 provider 的当日 429 计数。
func (s *Store) recordRateLimit() {
	s.cooldownStatsMu.Lock()
	s.rateLimitCount++
	s.cooldownStatsMu.Unlock()
}

// RateLimitCount 返回当日来自托管 provider 的 429 响应数。
func (s *Store) RateLimitCount() int {
	s.cooldownStatsMu.Lock()
	n := s.rateLimitCount
	s.cooldownStatsMu.Unlock()
	return int(n)
}

// SetSkipProvider 把一个托管 provider 加入按 provider 维护的跳过集合（skip_provider_on_429 开
// 启时，OnUsage 在托管 insufficient_quota 429 之后调用）。该 provider 会一直留在集合里，直到被
// 任意 200 清空，或者托管候选全部试过。
func (s *Store) SetSkipProvider(provider string) {
	if provider == "" {
		return
	}
	s.skipMu.Lock()
	defer s.skipMu.Unlock()
	if s.skipProviders == nil {
		s.skipProviders = make(map[string]struct{})
	}
	s.skipProviders[provider] = struct{}{}
}

// IsProviderSkipped 判断给定 provider 是否在跳过集合里。只读：不会移除条目。SchedulerPick 对每
// 个托管候选都会查它，用来过滤掉最近返回过 429+insufficient_quota 的 provider。
func (s *Store) IsProviderSkipped(provider string) bool {
	if provider == "" {
		return false
	}
	s.skipMu.RLock()
	defer s.skipMu.RUnlock()
	_, ok := s.skipProviders[provider]
	return ok
}

// HasSkippedProviders 判断跳过集合是否非空。
func (s *Store) HasSkippedProviders() bool {
	s.skipMu.RLock()
	defer s.skipMu.RUnlock()
	return len(s.skipProviders) > 0
}

// ClearAllSkippedProviders 清空按 provider 的跳过集合。触发点：OnUsage 收到任意 200（让下一次
// 请求重新探测免费的托管额度），或 SchedulerPick 发现托管候选已全部试过。
func (s *Store) ClearAllSkippedProviders() {
	s.skipMu.Lock()
	defer s.skipMu.Unlock()
	s.skipProviders = make(map[string]struct{})
}

// SetSkipRoundDone 标记 skip ON 已经走完一整轮 provider 轮换。
func (s *Store) SetSkipRoundDone() {
	s.skipRoundDone.Store(true)
}

// SkipRoundDone 返回 skip ON 是否已走完一整轮。当前没有调用方，仅作状态观测。
func (s *Store) SkipRoundDone() bool {
	return s.skipRoundDone.Load()
}

// ClearSkipRoundDone 清除该标记（任意 200 时调用）。
func (s *Store) ClearSkipRoundDone() {
	s.skipRoundDone.Store(false)
}

// cooldownWaitDuration 返回 store 中*所有* key 里剩余最长的 insufficient_quota 冷却，没有有效
// 冷却时返回 0。之所以检查所有 key（而不只是 req.Candidates）：处于冷却中的 key 已经进了宿主的
// "tried" 重试集合，因此根本不会出现在宿主传给 SchedulerPick 的候选列表里，只查候选会完全漏掉。
// 冷却只会由 ApplyInsufficientQuotaCooldown 设置（仅限托管 key），所以扫描整个 store 是安全的。
func (s *Store) cooldownWaitDuration(now time.Time) time.Duration {
	var maxWait time.Duration
	s.mu.RLock()
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.RLock()
		if ks.cooldown != nil && now.Before(*ks.cooldown) {
			if w := ks.cooldown.Sub(now); w > maxWait {
				maxWait = w
			}
		}
		ks.mu.RUnlock()
	}
	// 防御性封顶：单次冷却本身就不超过 maxInsufficientQuotaCooldown，但时钟漂移或写了未来时间
	// 的冷却也绝不允许阻塞超过这个上限。
	if maxWait > maxInsufficientQuotaCooldown*time.Second {
		maxWait = maxInsufficientQuotaCooldown * time.Second
	}
	return maxWait
}

// DisableStatus 是单个 key 禁用状态的快照，用于诊断展示。
type DisableStatus struct {
	AuthID string     `json:"auth_id"`
	Global *time.Time `json:"global_disabled_at,omitempty"`
}

// Status 返回所有存在有效禁用的 key 的快照。
func (s *Store) Status() []DisableStatus {
	now := s.now()
	loc := s.location()
	s.mu.RLock()
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	var out []DisableStatus
	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.RLock()
		st := DisableStatus{AuthID: id}
		// 401（unauthorized）的全局禁用会跨过午夜继续生效（globalReason == "unauthorized"）；
		// 日额度禁用只在其所属的自然日内有效。这里的判定与 isDisabled、Snapshot 保持一致，
		// 因此 Status() 不会和调度器的视角出现分歧。
		if ks.global != nil && (ks.globalReason == "unauthorized" || active(*ks.global, now, loc)) {
			t := *ks.global
			st.Global = &t
		}
		ks.mu.RUnlock()
		if st.Global != nil {
			out = append(out, st)
		}
	}
	return out
}

// PruneAll 清理所有 key 上已经跨日的禁用状态。由后台清理协程调用，也可以按需调用。
func (s *Store) PruneAll(now time.Time) {
	loc := s.location()

	// 在遍历 key 之前先判断是否已跨日，这样每把 key 的当日计数（successCount）可以在同一趟里
	// 一起复位。
	s.cooldownStatsMu.Lock()
	dayChanged := !s.cooldownStatsDay.IsZero() && !sameDay(s.cooldownStatsDay, now, loc)
	s.cooldownStatsDay = now
	if dayChanged {
		s.cooldownCount = 0
		s.cooldownWaitNanos = 0
		s.successCount = 0
		s.rateLimitCount = 0
		s.blockActive = 0
		s.blockStart = time.Time{}
	}
	s.cooldownStatsMu.Unlock()

	s.mu.RLock()
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.Lock()
		if dayChanged {
			ks.successCount = 0
		}
		if ks.cooldown != nil && !now.Before(*ks.cooldown) {
			ks.cooldown = nil
		}
		if ks.global != nil && ks.globalReason != "unauthorized" && !active(*ks.global, now, loc) {
			ks.global = nil
			ks.globalReason = ""
		}
		empty := ks.global == nil && ks.cooldown == nil
		ks.mu.Unlock()
		if empty {
			s.deleteKey(id)
		}
	}

	// 跨日时清空签到状态，让插件通过 /api/v1/users/login/info 给每个账号重新签到（这个调用同时
	// 也会激活当日的魔粒额度）。
	if dayChanged {
		for _, id := range ids {
			ks := s.getKey(id)
			if ks == nil {
				continue
			}
			ks.mu.Lock()
			ks.checkedIn = false
			ks.lastCheckIn = time.Time{}
			ks.magicCubeAvailable = 0
			ks.magicCubeTotal = 0
			ks.magicCubeFrozen = 0
			ks.usagePerDay = 0
			ks.mu.Unlock()
		}
		// 午夜重置按模型的用量计数
		s.modelUsageMu.Lock()
		s.modelUsage = make(map[string]int)
		s.modelUsageMu.Unlock()
		if fp := s.midnightSignInFunc.Load(); fp != nil && *fp != nil {
			(*fp)()
		}
	}

	// 跨日时重置全局 insufficient_quota 退避档位：上游的日额度在午夜重置，新的一天应从配置的基
	// 准值重新开始退避。
	s.backoffMu.Lock()
	if !s.cooldownLevelAt.IsZero() && !sameDay(s.cooldownLevelAt, now, loc) {
		s.cooldownLevel = 0
		s.cooldownLevelAt = time.Time{}
		s.cooldownExpiresAt = time.Time{}
	}
	s.backoffMu.Unlock()

	// 午夜跨日时重置当日的 insufficient_quota 统计。
	s.cooldownStatsMu.Lock()
	if !s.cooldownStatsDay.IsZero() && !sameDay(s.cooldownStatsDay, now, loc) {
		s.cooldownCount = 0
		s.cooldownWaitNanos = 0
	}
	s.cooldownStatsDay = now
	s.cooldownStatsMu.Unlock()
}

// KeyView 是单把凭据状态的可渲染快照。
type KeyView struct {
	AuthID             string
	GlobalDisabled     bool
	GlobalSince        time.Time
	GlobalReason       string
	Cooldown           bool
	CooldownUntil      time.Time
	SuccessCount       int
	MagicCubeAvailable int
	MagicCubeTotal     int
	MagicCubeFrozen    int
	UsagePerDay        int
	CheckedIn          bool
	LastCheckIn        time.Time
}

// Snapshot 返回 now 时刻所有已观察凭据状态的可渲染视图。已经跨日的禁用（除 unauthorized 外）不
// 计入。
func (s *Store) Snapshot(now time.Time) map[string]KeyView {
	loc := s.location()
	s.mu.RLock()
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	out := make(map[string]KeyView, len(ids))
	for _, id := range ids {
		ks := s.getKey(id)
		if ks == nil {
			continue
		}
		ks.mu.RLock()
		kv := KeyView{AuthID: id}
		if ks.cooldown != nil && now.Before(*ks.cooldown) {
			kv.Cooldown = true
			kv.CooldownUntil = *ks.cooldown
		}
		if ks.global != nil && (ks.globalReason == "unauthorized" || active(*ks.global, now, loc)) {
			kv.GlobalDisabled = true
			kv.GlobalSince = *ks.global
			kv.GlobalReason = ks.globalReason
		}
		kv.SuccessCount = ks.successCount
		kv.MagicCubeAvailable = ks.magicCubeAvailable
		kv.MagicCubeTotal = ks.magicCubeTotal
		kv.MagicCubeFrozen = ks.magicCubeFrozen
		kv.UsagePerDay = ks.usagePerDay
		kv.CheckedIn = ks.checkedIn
		kv.LastCheckIn = ks.lastCheckIn
		ks.mu.RUnlock()
		out[id] = kv
	}
	return out
}

// recordMagicCube 保存某把凭据的魔粒余额。
func (s *Store) RecordMagicCube(authID string, available, total, frozen int, now time.Time) {
	ks := s.getOrCreateKey(authID)
	ks.mu.Lock()
	ks.magicCubeAvailable = available
	ks.magicCubeTotal = total
	ks.magicCubeFrozen = frozen
	ks.mu.Unlock()
}

// recordCheckIn 标记某把凭据当日已签到。
func (s *Store) RecordCheckIn(authID string, now time.Time) {
	ks := s.getOrCreateKey(authID)
	ks.mu.Lock()
	ks.checkedIn = true
	ks.lastCheckIn = now
	ks.mu.Unlock()
}

// recordUsagePerDay 保存平台侧报告的当日用量计数。
func (s *Store) RecordUsagePerDay(authID string, usage int) {
	ks := s.getOrCreateKey(authID)
	ks.mu.Lock()
	ks.usagePerDay = usage
	ks.mu.Unlock()
}
