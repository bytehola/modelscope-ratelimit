package ratelimit

import "strings"

// PinnedProviderHeader 是插件内部使用的标记头。Responses 执行器在为某个 provider
// 做 chat 兜底、嵌套调用 host.model.execute 时带上它，值是本次唯一允许应答的托管
// provider；scheduler.pick 再从 Options.Headers 里把它读回来。
//
// 之所以需要它：嵌套调用路由的是一个*共享*别名，如果被选中的 provider 在这次调用
// 里返回 429，宿主会自己换到另一个托管凭据应答，于是本该走原生 Responses 的
// provider 被悄悄降级成 chat，它自己的原生步骤也就再没机会执行。宿主的 compat
// 执行器只把凭据里声明过的 header: 属性发往上游（util.ApplyCustomHeadersFromAttrs），
// 所以这个标记头不会离开本进程。
const PinnedProviderHeader = "X-Modelscope-Ratelimit-Provider"

// internalExecutionMetadataKey 与 internalExecutionSource 对应宿主
// CLIProxyAPI sdk/api/handlers/model_execution.go 里的
// modelExecutionMetadataSourceKey / modelExecutionInternalSource，用来标记这个请求
// 来自插件自己的 host.model.execute 回调，而不是外部客户端。
const (
	internalExecutionMetadataKey = "source"
	internalExecutionSource      = "plugin_host_model_callback"
)

// PinnedProvider 返回本次挑选必须由哪个托管 provider 应答；不受限时返回 ""。
//
// 只有被宿主标记为内部来源的请求才认这个钉定：否则任何客户端都能在普通请求里带上
// 这个头，指定由哪个账号来服务。
func (c *Config) PinnedProvider(req SchedulerPickRequest) string {
	if c == nil || len(c.Providers) == 0 {
		return ""
	}
	if !isInternalExecution(req.Options.Metadata) {
		return ""
	}
	name := headerValue(req.Options.Headers, PinnedProviderHeader)
	if name == "" {
		return ""
	}
	if c.ProviderIndex(name) < 0 {
		// 不是我们托管的 provider：完全忽略这个标记头。
		return ""
	}
	return name
}

// isInternalExecution 判断宿主是否已把该请求标记为插件的嵌套模型执行。
func isInternalExecution(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[internalExecutionMetadataKey]
	if !ok {
		return false
	}
	value, okString := raw.(string)
	return okString && value == internalExecutionSource
}

// headerValue 以大小写不敏感的方式查一个请求头，与载荷解析其余部分的宽松查找
// 保持一致。
func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.ToLower(strings.TrimSpace(value)); trimmed != "" {
				return trimmed
			}
		}
		return ""
	}
	return ""
}
