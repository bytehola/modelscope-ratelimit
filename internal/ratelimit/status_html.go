package ratelimit

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// nextMidnight 返回时区 loc 下严格晚于 now 的下一个 00:00。
func nextMidnight(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	y, m, d := n.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, loc)
}

type GuidanceReason int

const (
	// GuidanceMissing：providers 或 management_key 为空时展示。
	GuidanceMissing GuidanceReason = iota
	// GuidanceResolutionFailed：服务端解析凭据失败时展示（management_key 错误、
	// host_base_url 不可达等）。
	GuidanceResolutionFailed
	// GuidanceProviderNotFound：配置里的 provider 一个都匹配不到上游
	// openai-compatibility 条目时展示。
	GuidanceProviderNotFound
)

// RenderGuidanceHTML 渲染配置引导页。它始终把运维指引到管理中心的“插件管理 → 编辑配置”，而不
// 是让人手改 config.yaml。detail 用于附带一条额外提示（解析错误，或用逗号连接的未匹配 provider
// 列表）。页面每 15 秒自动刷新，所以配置改好、插件重载之后（reconfigure 会清理解析缓存）就会自
// 动切到状态页。
func RenderGuidanceHTML(cfg *Config, reason GuidanceReason, detail string) string {
	hostBase := "http://127.0.0.1:8317"
	if cfg != nil && cfg.HostBaseURL != "" {
		hostBase = cfg.HostBaseURL
	}
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta http-equiv="refresh" content="15">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<title>Modelscope 限流 — 配置引导</title><style>`)
	b.WriteString(`*{box-sizing:border-box;margin:0;padding:0}`)
	b.WriteString(`body{font-family:-apple-system,BlinkMacSystemFont,"SF Pro Display","SF Pro Text","Helvetica Neue",Helvetica,Arial,"PingFang SC",sans-serif;background:#fbfbfd;color:#1d1d1f;-webkit-font-smoothing:antialiased;padding:28px 16px 56px}`)
	b.WriteString(`.wrap{max-width:780px;margin:0 auto}`)
	b.WriteString(`.title{font-size:32px;font-weight:700;letter-spacing:-.02em;margin-bottom:6px}`)
	b.WriteString(`.sub{font-size:15px;color:#86868b;line-height:1.6;margin-bottom:24px}`)
	b.WriteString(`.sub b{color:#1d1d1f;font-weight:600}`)
	b.WriteString(`.section{font-size:19px;font-weight:600;letter-spacing:-.01em;margin:28px 0 12px}`)
	b.WriteString(`.notice{background:#fff;border-radius:14px;padding:20px 22px;box-shadow:0 1px 3px rgba(0,0,0,.04),0 6px 16px rgba(0,0,0,.03);margin-bottom:14px}`)
	b.WriteString(`.notice .nh{font-size:17px;font-weight:600;margin-bottom:8px}`)
	b.WriteString(`.notice .nh.err{color:#bf0711}.notice .nh.warn{color:#b25000}`)
	b.WriteString(`.notice .nd{font-size:14px;color:#424245;line-height:1.6}`)
	b.WriteString(`code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12.5px;background:#f5f5f7;padding:2px 6px;border-radius:6px;color:#424245}`)
	b.WriteString(`.detail{display:block;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12.5px;background:#f5f5f7;padding:10px 12px;border-radius:10px;margin-top:10px;color:#6e6e73;word-break:break-all}`)
	b.WriteString(`.steprow{font-size:14px;color:#424245;line-height:1.7;margin-bottom:6px}`)
	b.WriteString(`.step{display:inline-flex;align-items:center;gap:5px;background:#f0f0f3;color:#1d1d1f;font-size:13px;font-weight:600;padding:4px 12px;border-radius:980px;white-space:nowrap}`)
	b.WriteString(`.cards{display:flex;flex-direction:column;gap:10px}`)
	b.WriteString(`.fcard{background:#fff;border-radius:14px;padding:16px 18px;box-shadow:0 1px 3px rgba(0,0,0,.04),0 6px 16px rgba(0,0,0,.03)}`)
	b.WriteString(`.fcard .ftop{display:flex;align-items:center;gap:8px;margin-bottom:6px;flex-wrap:wrap}`)
	b.WriteString(`.fcard .fname{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:14px;font-weight:600;color:#1d1d1f;background:none;padding:0}`)
	b.WriteString(`.badge{display:inline-block;font-size:11px;font-weight:600;padding:2px 9px;border-radius:980px}`)
	b.WriteString(`.b-req{background:#fdecec;color:#bf0711}.b-opt{background:#f0f0f3;color:#86868b}`)
	b.WriteString(`.fcard .fdesc{font-size:13.5px;color:#86868b;line-height:1.6}`)
	b.WriteString(`.footer{font-size:13px;color:#86868b;line-height:1.6;margin-top:28px}`)
	b.WriteString(`.footer code{font-size:12px}`)
	b.WriteString(`</style></head><body><div class="wrap">`)

	b.WriteString(`<div class="title">Modelscope 限流</div>`)
	b.WriteString(`<div class="sub">配置引导 · 在管理中心保存配置后自动重载</div>`)

	// 提示卡片：说明本次为什么展示这个引导页。
	switch reason {
	case GuidanceResolutionFailed:
		b.WriteString(`<div class="notice"><div class="nh err">服务端解析失败</div>`)
		b.WriteString(`<div class="nd"><code>management_key</code> 可能有误，或 <code>host_base_url</code> 不可达。</div>`)
		if detail != "" {
			fmt.Fprintf(&b, `<span class="detail">%s</span>`, html.EscapeString(detail))
		}
		b.WriteString(`</div>`)
	case GuidanceProviderNotFound:
		b.WriteString(`<div class="notice"><div class="nh err">providers 设置错误</div>`)
		fmt.Fprintf(&b, `<div class="nd">未找到与 <code>%s</code> 匹配的 openai 提供商。</div>`, html.EscapeString(detail))
		b.WriteString(`</div>`)
	default: // GuidanceMissing（缺少必填配置）
		b.WriteString(`<div class="notice"><div class="nh warn">尚未配置</div>`)
		b.WriteString(`<div class="nd">尚未配置 <code>providers</code> 或 <code>management_key</code>，状态页暂不可用。</div>`)
		b.WriteString(`</div>`)
	}

	b.WriteString(`<div class="steprow">前往 <span class="step">插件管理 → 编辑配置</span> 填写以下配置项，保存即重载。</div>`)

	b.WriteString(`<div class="section">配置项</div>`)
	b.WriteString(`<div class="cards">`)
	b.WriteString(`<div class="fcard"><div class="ftop"><span class="fname">providers</span><span class="badge b-req">必填</span></div><div class="fdesc">提供商名称，须与 config.yaml 中 openai-compatibility 的 <code>name</code> 一致，如 <code>modelscope</code>。可填多个。</div></div>`)
	b.WriteString(`<div class="fcard"><div class="ftop"><span class="fname">management_key</span><span class="badge b-req">必填 · 敏感</span></div><div class="fdesc">CPA 管理密钥。</div></div>`)
	b.WriteString(`<div class="fcard"><div class="ftop"><span class="fname">host_base_url</span><span class="badge b-opt">选填</span></div><div class="fdesc">CPA 链接地址，默认 <code>http://127.0.0.1:8317</code>。第三方面板端口错位时填实际 API 端口。</div></div>`)
	b.WriteString(`</div>`)

	fmt.Fprintf(&b, `<div class="footer">当前 host_base_url：<code>%s</code> · 每 15 秒自动刷新，配置生效后自动转入状态页。</div>`, html.EscapeString(hostBase))
	b.WriteString(`</div></body></html>`)
	return b.String()
}
