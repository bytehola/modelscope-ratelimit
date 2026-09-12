package ratelimit

import (
	"fmt"
	"strings"
)

// Config 是插件配置，由宿主提供的 plugins.configs.<pluginID> 这段 YAML 解析而来
// （宿主自己消费 enabled 和 priority 两个字段，其余原样传给插件）。
type Config struct {
	// Providers 是本插件托管的 provider 名称集合（openai-compatibility 条目里的 name）。
	// 调度器只拦截 Provider 落在该集合内的请求，其余一律交回宿主内置调度（Handled=false）。
	// 想让调度生效，这一项必填。
	//
	// 填写顺序就是跨 provider 的轮换顺序：请求从第一个 provider 开始，把这一家的凭据逐个
	// 试下去，所以 key 级失败（insufficient balance 的 429 只禁用一把 key）会在本家内换下
	// 一把 key；只有这一家已经没有可用凭据，或者 skip_provider_on_429 因池级
	// insufficient_quota 把它摘掉，才会轮到下一家。任意 200 会让下一次请求重新从第一家开
	// 始；各家的 key 游标彼此独立，并且跨请求继续轮转。
	Providers []string `json:"providers" yaml:"providers"`

	// SkipProviderOn429 开启“跳过整家”模式：托管凭据返回**池级** insufficient_quota（Chat 的
	// 429 JSON，或 Responses 以 200 + SSE response.failed 回送的同一种 code），或本家服务不了
	// 当前请求（405 端点/协议不被接受、503 + model_not_ready 模型还在加载，见
	// IsProviderAvailabilityRejection）时，该 provider 进入 latch 的 skip 集，下一次挑选直接落到
	// 下一个 provider 或非托管凭据，两个 Responses 执行器循环也会丢掉这一家剩下的 key 步骤。
	// 任意 200（无论托管还是非托管）清空 skip 集，下一次请求重新探测——对“模型还在加载”这类
	// 暂时状态，这正好等于“成功一次之后再探一次”。该开关不会缩短每家的 key 步骤表：key 级失败
	// （含 Responses 的 rate_limit_exceeded）一定先在本家内轮换。关闭时（默认）
	// insufficient_quota 改为给该 key 施加逐 key 退避冷却，本家的扫描继续用下一把 key。
	SkipProviderOn429 bool `json:"skip_provider_on_429" yaml:"skip_provider_on_429"`

	// Timezone 是每日 00:00 重置边界所用的 IANA 时区，默认 "Asia/Shanghai"。
	Timezone string `json:"timezone" yaml:"timezone"`

	// CredentialStrategy 控制插件在一家内的可用 key 之间怎么选："round-robin"（默认）摊开
	// 负载；"fill-first" 一直用第一把 key 直到它被禁用。只对托管 provider 生效。
	CredentialStrategy string `json:"credential_strategy" yaml:"credential_strategy"`

	// InsufficientQuotaCooldown 是 429+insufficient_quota 之后指数退避冷却的基准秒数。由于
	// Modelscope 的额度在所有托管 key 之间共享，这个冷却是全局的：SchedulerPick 会阻塞所有
	// 托管挑选直到当前冷却到期。连续失败按 ×2 递增，封顶 60 秒，无抖动；同一轮里所有 key
	// 的冷却在同一时刻到期（不漂移、不重复升级）。任意 200 复位。设为 0 可完全关闭冷却
	// （429 立即轮换到下一个 key 或下一家，不阻塞）。支持小数秒（例如 0.1），默认 0.1。
	InsufficientQuotaCooldown float64 `json:"insufficient_quota_cooldown" yaml:"insufficient_quota_cooldown"`

	// MsProxyURL 非空时，所有 Modelscope API 调用都改走这个代理，用于调试和抓包（例如
	// "http://127.0.0.1:8888"）。
	// TODO：调试结束后移除。
	MsProxyURL string `json:"ms_proxy_url" yaml:"ms_proxy_url"`

	// HostBaseURL 是 CLIProxyAPI 管理 API 的绝对地址（例如 "http://127.0.0.1:8317"），用于
	// 服务端解析凭据。它与 ManagementKey 同时配置时，插件在服务端（直接用 net/http，不走
	// cgo 回调）解析掩码 api-key 与真实总数，并把结果烘进状态页（不由浏览器发请求，页面里
	// 也不含密钥）。
	HostBaseURL string `json:"host_base_url" yaml:"host_base_url"`

	// ManagementKey 是 CLIProxyAPI 管理密钥，取自 config.yaml 的插件配置段。只有在设置了
	// HostBaseURL 时才在服务端使用（走 net/http），绝不会渲染进无需鉴权的资源页。把它写进
	// config.yaml 是运维自己的选择，请相应限制文件权限。
	ManagementKey string `json:"management_key" yaml:"management_key"`

	// UseResponses 控制实验性的 Responses API 透传执行器。取值可以是上游模型名、"*"，或者
	// 一组上游模型名。"*" 表示所有 Responses 请求都启用透传，否则只有命中的模型才走上游
	// /v1/responses。管理面板把这一项渲染成单个文本框，所以下面这些写法都会以普通字符串送
	// 达，插件全部接受：
	//
	//   "*" 或 true              全部模型
	//   "false" 或 false         关闭
	//   "model-a"                只开一个上游模型
	//   ["model-a","model-b"]    一行内写多个上游模型
	//   "model-a, model-b"       同上，不带方括号
	//
	// 真正的 YAML 序列同样支持。匹配不到任何已知模型的取值，等价于不启用执行器。
	UseResponses []string `json:"use_responses" yaml:"use_responses"`
}

// DefaultConfig 返回插件默认配置：宿主管理地址、重置时区、凭据策略和退避基准。
func DefaultConfig() *Config {
	return &Config{
		HostBaseURL:               "http://127.0.0.1:8317",
		Timezone:                  "Asia/Shanghai",
		CredentialStrategy:        "round-robin",
		InsufficientQuotaCooldown: 0.1,
	}
}

// openaiCompatProviderPrefix 是宿主给 openai-compatibility provider 名称加的前缀，用来
// 拼出运行时的 provider key（见 CLIProxyAPI util.OpenAICompatibleProviderKey）：
// "modelscope" 变成 "openai-compatible-modelscope"。
const openaiCompatProviderPrefix = "openai-compatible-"

// extractProviderName 从 provider key 或 auth ID 上剥掉已知前缀，还原运维在配置里写的
// 那个裸名称：
//
//   - "openai-compatible-<name>"（宿主调度器用的短横运行时 key）
//   - "openai-compatibility:<name>:<hash>"（冒号形式的凭据类型前缀 / auth ID）
//   - "<name>"（裸配置名，原样传入）
//
// 剥完之后调用方拿它和 Config.Providers 做精确比较（忽略大小写）。这样不会有歧义：
// "modelscope" 和 "modelscope-my" 永远不会互相命中；而早先按 "-" 分词的做法会把
// "modelscope-my" 拆出 "modelscope" 这个 token，从而误判。
func extractProviderName(p string) string {
	if strings.HasPrefix(p, openaiCompatProviderPrefix) {
		return p[len(openaiCompatProviderPrefix):]
	}
	if strings.HasPrefix(p, "openai-compatibility:") {
		rest := p[len("openai-compatibility:"):]
		if idx := strings.IndexByte(rest, ':'); idx >= 0 {
			return rest[:idx]
		}
		return rest
	}
	return p
}

// ProviderIndex 返回与给定 provider key 或 auth ID 匹配的配置项下标（从 0 开始），没有
// 任何匹配时返回 -1。宿主生成的 key 只有两种固定形态（调度候选用
// "openai-compatible-<name>"，auth ID 用 "openai-compatibility:<name>:<hash>"），所以这
// 里剥掉前缀后按裸名称精确比较（忽略大小写）。它取代了旧的按 token 匹配，因为 "-" 是
// token 分隔符，旧写法无法区分 "modelscope" 和 "modelscope-my"。
func (c *Config) ProviderIndex(provider string) int {
	if len(c.Providers) == 0 {
		return -1
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return -1
	}
	p = extractProviderName(p)
	for idx, want := range c.Providers {
		if strings.ToLower(strings.TrimSpace(want)) == p {
			return idx
		}
	}
	return -1
}

// ManagesProvider 判断给定 provider key 是否在本插件的管辖范围内。Providers 为空表示什
// 么都不托管。
func (c *Config) ManagesProvider(provider string) bool {
	return c.ProviderIndex(provider) >= 0
}

// UsesResponses 判断请求的模型是否启用 Responses API 透传。含 "*" 项时全部启用，否则按
// 忽略大小写、忽略首尾空白的精确匹配。
func (c *Config) UsesResponses(model string) bool {
	if c == nil || len(c.UseResponses) == 0 {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, want := range c.UseResponses {
		want = strings.ToLower(strings.TrimSpace(want))
		if want == "*" || (model != "" && want == model) {
			return true
		}
	}
	return false
}

// ConfigFromMap 从通用 map 构建 Config（胶水层把宿主的 YAML 解析成 map 后交给这里）。
// 缺失或零值字段回退到 DefaultConfig，因此只写一部分配置时跨版本依然有效。
func ConfigFromMap(raw map[string]any) (*Config, error) {
	cfg := DefaultConfig()
	if raw == nil {
		return cfg, nil
	}
	if v, ok := raw["providers"]; ok {
		cfg.Providers = toStringSlice(v)
	}
	if v, ok := raw["skip_provider_on_429"].(bool); ok {
		cfg.SkipProviderOn429 = v
	}
	if v, ok := raw["host_base_url"].(string); ok && strings.TrimSpace(v) != "" {
		cfg.HostBaseURL = strings.TrimSpace(v)
	}
	if v, ok := raw["management_key"].(string); ok {
		cfg.ManagementKey = strings.TrimSpace(v)
	}
	if v, ok := raw["timezone"].(string); ok && v != "" {
		cfg.Timezone = v
	}
	if v, ok := raw["credential_strategy"].(string); ok {
		cfg.CredentialStrategy = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := raw["insufficient_quota_cooldown"]; ok {
		if n, err := toFloat64(v); err == nil {
			cfg.InsufficientQuotaCooldown = n
		} else {
			return nil, fmt.Errorf("invalid insufficient_quota_cooldown: %w", err)
		}
	}
	if v, ok := raw["ms_proxy_url"].(string); ok {
		cfg.MsProxyURL = strings.TrimSpace(v)
	}
	if v, ok := raw["use_responses"]; ok {
		cfg.UseResponses = useResponsesFromValue(v)
	}
	return cfg, nil
}

func useResponsesFromValue(v any) []string {
	if enabled, ok := v.(bool); ok {
		if enabled {
			return []string{"*"}
		}
		return nil
	}
	values := toStringSlice(v)
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		for _, model := range splitUseResponses(value) {
			// 匹配本身忽略大小写，所以这里把只有大小写差异的重复项折叠掉，
			// 让生效列表（以及启动横幅）更易读。
			key := strings.ToLower(model)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, model)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitUseResponses 把一个配置标量展开成上游模型名列表。
//
// use_responses 向宿主声明为字符串型配置字段，所以管理面板每次都会把它写成单个 YAML
// 标量：运维在文本框里输入 ["model-a", "model-b"] 或 true，插件收到的就是这段字面文本
// 而不是序列。在这里做拆分，既能正常用面板，也让真正的 YAML 序列（每个元素一行）走同
// 一条路径。
//
// 接受的形态："*" 以及 true/TRUE 表示全部模型；"false" 表示关闭；带方括号或不带方括号
// 的逗号分隔列表按项展开并剥掉外层引号；其他内容原样保留，只有正好等于某个上游模型名
// 时才会生效。
func splitUseResponses(raw string) []string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	switch strings.ToLower(value) {
	case "true":
		return []string{"*"}
	case "false":
		return nil
	}
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(strings.Trim(strings.TrimSpace(field), "\"'"))
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

// toStringSlice 把 YAML/JSON 的标量或序列强制转换成 []string。
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return append([]string(nil), t...)
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	default:
		return nil
	}
}

// toFloat64 把常见的 YAML/JSON 数值表示转换成 float64。
func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	case string:
		var x float64
		_, err := fmt.Sscanf(n, "%f", &x)
		return x, err
	default:
		return 0, fmt.Errorf("unsupported numeric type %T", v)
	}
}
