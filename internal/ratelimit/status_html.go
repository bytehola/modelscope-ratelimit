package ratelimit

import (
	"fmt"
	"html"
	"sort"
	"strings"
	"time"
)

// nextMidnight returns the next 00:00 in loc strictly after now.
func nextMidnight(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	y, m, d := n.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, loc)
}

// anyModelDisabled reports whether any of the model views is disabled.
func anyModelDisabled(ms []ModelView) bool {
	for _, m := range ms {
		if m.Disabled {
			return true
		}
	}
	return false
}

// barColorClass returns the progress fill / number color class for a
// remaining/limit pair: g = plenty (>50%), a = low (1-50%), r = exhausted or
// disabled (0). When no limit is known, any remaining > 0 is treated as green.
func barColorClass(remaining, limit int, hasLim, disabled bool) string {
	if disabled || remaining <= 0 {
		return "r"
	}
	if !hasLim || limit <= 0 {
		return "g"
	}
	if remaining*100/limit > 50 {
		return "g"
	}
	return "a"
}

// barWidthPct returns the fill width (0-100) for a remaining/limit pair. With
// no known limit, a positive remaining fills fully (indicates "has quota").
func barWidthPct(remaining, limit int, hasLim bool) int {
	if !hasLim || limit <= 0 {
		if remaining > 0 {
			return 100
		}
		return 0
	}
	w := remaining * 100 / limit
	if w > 100 {
		w = 100
	}
	if w < 0 {
		w = 0
	}
	return w
}

// modelRemClass returns the binary color class for a model's remaining in the
// 模型剩余 column: "g" when it still has quota (>0 and not disabled), "r" when
// exhausted or disabled, "" when no remaining data has been observed (name
// stays muted gray, no bar).
func modelRemClass(mv ModelView, globallyDisabled bool) string {
	if !mv.HasRem {
		return ""
	}
	if mv.Disabled || globallyDisabled || mv.Remaining <= 0 {
		return "r"
	}
	return "g"
}

// classSuffix turns a color class into a CSS class-list suffix (" g" / " r" / "").
func classSuffix(cls string) string {
	if cls == "" {
		return ""
	}
	return " " + cls
}

// formatDuration renders a duration in a compact human-readable form for the
// status page: "350ms", "12.3s", "2m13s".
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	return fmt.Sprintf("%dm%ds", m, s)
}

// providerOf returns the configured provider name for an auth ID, or "" when
// none matches or the config is nil. It tries ProviderIndex (token matching,
// same logic the scheduler uses) first, then falls back to parsing the
// "openai-compatibility:<provider>:<hash>" format.
func providerOf(cfg *Config, id string) string {
	if cfg == nil {
		return ""
	}
	if idx := cfg.ProviderIndex(id); idx >= 0 {
		return cfg.Providers[idx]
	}
	parts := strings.SplitN(id, ":", 3)
	if len(parts) >= 2 && parts[0] == "openai-compatibility" {
		return parts[1]
	}
	return ""
}

// modelAvailable returns the effective available request count for a model view
// (0 when the key is globally disabled or the model is disabled), used to order
// model bars from most to least available.
func modelAvailable(mv ModelView, globallyDisabled bool) int {
	if globallyDisabled || mv.Disabled {
		return 0
	}
	return mv.Remaining
}

// mergeModelView folds mv into cur for two views that share the same (already
// display-resolved) model name. The most recent observation wins, so a fresh
// (recovered) snapshot is not hidden behind a stale disable recorded under
// another alias/name. Ties (same instant) keep the first-seen view.
func mergeModelView(cur, mv *ModelView) {
	if !mv.ObservedAt.IsZero() && (cur.ObservedAt.IsZero() || mv.ObservedAt.After(cur.ObservedAt)) {
		cur.Remaining = mv.Remaining
		cur.HasRem = mv.HasRem
		cur.Limit = mv.Limit
		cur.HasLim = mv.HasLim
		cur.Disabled = mv.Disabled
		cur.Since = mv.Since
		cur.ObservedAt = mv.ObservedAt
	}
}

// dedupModelsByName merges model views that share the same (display-resolved)
// Name, so a model recorded under several alias/upstream storage keys appears
// as a single row. Order follows first occurrence (callers re-sort).
func dedupModelsByName(in []ModelView) []ModelView {
	if len(in) <= 1 {
		return in
	}
	byName := make(map[string]*ModelView, len(in))
	order := make([]string, 0, len(in))
	for i := range in {
		mv := in[i]
		if cur, ok := byName[mv.Name]; ok {
			mergeModelView(cur, &mv)
			continue
		}
		cp := mv
		byName[mv.Name] = &cp
		order = append(order, mv.Name)
	}
	out := make([]ModelView, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out
}

// RenderStatusHTMLFull renders the Apple-style status page: a header summary,
// per-model availability cards, and a table of every resolved (masked) key with
// its disabled status and last-seen remaining counts. The "estimated recovery"
// column is omitted — all disables recover at the next 00:00 reset (shown in the
// header). Only non-sensitive data is rendered: keys are masked (last
// 12); the full key never reaches this unauthenticated resource page.
func (s *Store) RenderStatusHTMLFull(now time.Time, cfg *Config, masked map[string]string, realTotal int) string {
	loc := s.location()
	snap := s.Snapshot(now)
	// Merge each key's model views by display name so the same model recorded
	// under several alias/upstream storage keys appears as a single row (and is
	// not double-counted in the per-model summary).
	for id, kv := range snap {
		kv.Models = dedupModelsByName(kv.Models)
		snap[id] = kv
	}
	reset := nextMidnight(now, loc)

	total := realTotal
	if total <= 0 {
		total = len(masked)
	}
	disabled := 0
	for _, kv := range snap {
		if kv.GlobalDisabled || anyModelDisabled(kv.Models) {
			disabled++
		}
	}
	available := total - disabled
	if available < 0 {
		available = 0
	}

	// Per-model availability summary across active (non-disabled) keys.
	type modelSum struct {
		available int
		active    int
		disabled  int
		hasRem    bool
	}
	models := map[string]*modelSum{}
	for _, kv := range snap {
		for _, mv := range kv.Models {
			ms := models[mv.Name]
			if ms == nil {
				ms = &modelSum{}
				models[mv.Name] = ms
			}
			if kv.GlobalDisabled || mv.Disabled {
				ms.disabled++
			} else if mv.HasRem {
				ms.available += mv.Remaining
				ms.active++
				ms.hasRem = true
			} else {
				ms.active++
			}
		}
	}
	modelNames := make([]string, 0, len(models))
	for m := range models {
		modelNames = append(modelNames, m)
	}
	sort.Strings(modelNames)

	authIDs := make([]string, 0, len(masked))
	for id := range masked {
		authIDs = append(authIDs, id)
	}
	// Key 明细排序：先按模型剩余次数从高到低，再按状态（正常 → 模型禁用 →
	// 全局禁用），最后按 auth ID 稳定排序。剩余取该 key 仍可用（未禁用）模型
	// 的剩余之和；全局禁用的 key 剩余记 0，排到最末。未被观测过的正常 key 无
	// 剩余数据，记 0，但状态为正常，故排在禁用 key 之前。
	type keySort struct{ rem, rank int }
	keyOrder := make(map[string]keySort, len(authIDs))
	for _, id := range authIDs {
		var ks keySort
		if kv, ok := snap[id]; ok {
			if kv.GlobalDisabled {
				ks.rank = 2
			} else if anyModelDisabled(kv.Models) {
				ks.rank = 1
			}
			if ks.rank < 2 {
				for _, mv := range kv.Models {
					if !mv.Disabled && mv.HasRem {
						ks.rem += mv.Remaining
					}
				}
			}
		}
		keyOrder[id] = ks
	}
	sort.Slice(authIDs, func(i, j int) bool {
		a, b := keyOrder[authIDs[i]], keyOrder[authIDs[j]]
		if a.rem != b.rem {
			return a.rem > b.rem
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return authIDs[i] < authIDs[j]
	})

	providerLabel := "—"
	if cfg != nil && len(cfg.Providers) > 0 {
		providerLabel = strings.Join(cfg.Providers, ", ")
	}
	showProvider := cfg != nil && len(cfg.Providers) > 1

	// Build authoritative authID → provider name reverse map from the
	// providerOrderFetcher (same management-API resolution that built the
	// masked map). This avoids token-matching ambiguities when a provider
	// name contains separators (- space . etc). Falls back to providerOf
	// (token matching) when the fetcher is unavailable.
	provOfKey := make(map[string]string)
	if showProvider {
		if fp := s.providerOrderFetcher.Load(); fp != nil && *fp != nil {
			if m := (*fp)(); m != nil {
				for _, p := range cfg.Providers {
					pLower := strings.ToLower(strings.TrimSpace(p))
					for _, aid := range m[pLower] {
						provOfKey[aid] = p
					}
					if pLower != p {
						for _, aid := range m[p] {
							if _, ok := provOfKey[aid]; !ok {
								provOfKey[aid] = p
							}
						}
					}
				}
			}
		}
	}

	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta http-equiv="refresh" content="5">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<title>Modelscope 限流状态</title><style>`)
	b.WriteString(`*{box-sizing:border-box;margin:0;padding:0}`)
	b.WriteString(`body{font-family:-apple-system,BlinkMacSystemFont,"SF Pro Display","SF Pro Text","Helvetica Neue",Helvetica,Arial,"PingFang SC",sans-serif;background:#fbfbfd;color:#1d1d1f;-webkit-font-smoothing:antialiased;padding:28px 16px 56px}`)
	b.WriteString(`.wrap{max-width:980px;margin:0 auto}`)
	b.WriteString(`.title{font-size:32px;font-weight:700;letter-spacing:-.02em;margin-bottom:6px}`)
	b.WriteString(`.sub{font-size:14px;color:#86868b;line-height:1.6;margin-bottom:22px}`)
	b.WriteString(`.sub b{color:#1d1d1f;font-weight:600}`)
	b.WriteString(`.section{font-size:19px;font-weight:600;letter-spacing:-.01em;margin:26px 0 12px}`)
	b.WriteString(`.cards{display:flex;gap:12px;flex-wrap:wrap}`)
	b.WriteString(`.card{background:#fff;border-radius:14px;padding:16px 18px;box-shadow:0 1px 3px rgba(0,0,0,.04),0 6px 16px rgba(0,0,0,.03);min-width:150px;flex:1}`)
	b.WriteString(`.card .k{font-size:12px;color:#86868b;margin-bottom:6px}`)
	b.WriteString(`.card .v{font-size:24px;font-weight:600;letter-spacing:-.01em}`)
	b.WriteString(`.v.green{color:#1a7f37}.v.red{color:#bf0711}`)
	b.WriteString(`.mcard{background:#fff;border-radius:12px;padding:14px 16px;box-shadow:0 1px 3px rgba(0,0,0,.04);min-width:170px;flex:1}`)
	b.WriteString(`.mcard .mn{font-size:12px;color:#86868b;margin-bottom:6px;word-break:break-all}`)
	b.WriteString(`.mcard .ma{font-size:22px;font-weight:600;color:#1a7f37;letter-spacing:-.01em}.mcard .ma-r{color:#bf0711}`)
	b.WriteString(`.mcard .mk{font-size:12px;color:#86868b;margin-top:4px}`)
	b.WriteString(`table{width:100%;border-collapse:collapse;background:#fff;border-radius:14px;overflow:hidden;box-shadow:0 1px 3px rgba(0,0,0,.04),0 6px 16px rgba(0,0,0,.03)}`)
	b.WriteString(`th{text-align:left;font-size:11px;font-weight:600;color:#86868b;text-transform:uppercase;letter-spacing:.05em;padding:13px 16px;border-bottom:1px solid #e8e8ed;background:#fafafa}`)
	b.WriteString(`td{padding:13px 16px;border-bottom:1px solid #f0f0f3;font-size:13.5px;vertical-align:middle}`)
	b.WriteString(`tr:last-child td{border-bottom:none}`)
	b.WriteString(`tbody tr:nth-child(even) td{background:#f5f5f7}`)
	b.WriteString(`.key{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12.5px;color:#424245;word-break:break-all}`)
	b.WriteString(`.prov{display:block;font-size:10.5px;font-weight:600;color:#86868b;background:#f0f0f3;border-radius:980px;padding:2px 8px;margin-top:4px;width:fit-content}`)
	b.WriteString(`.badge{display:inline-block;font-size:11.5px;font-weight:600;padding:3px 10px;border-radius:980px;white-space:nowrap}`)
	b.WriteString(`.b-ok{background:#e8f5e9;color:#1a7f37}.b-glob{background:#fdecec;color:#bf0711}.b-mod{background:#fff4e0;color:#b25000}`)
	b.WriteString(`.when{display:block;font-size:11px;color:#86868b;margin-top:3px}`)
	b.WriteString(`.mbars{display:flex;flex-direction:column;gap:8px;min-width:200px}`)
	b.WriteString(`.mb-h{display:flex;justify-content:space-between;align-items:baseline;gap:8px;margin-bottom:4px}`)
	b.WriteString(`.mb-n{font-size:11.5px;color:#86868b;word-break:break-all}`)
	b.WriteString(`.mb-n.g{color:#1a7f37}.mb-n.r{color:#bf0711}`)
	b.WriteString(`.bv{font-size:12.5px;font-weight:600;font-variant-numeric:tabular-nums;color:#1d1d1f;white-space:nowrap}`)
	b.WriteString(`.bv.r{color:#bf0711}.bv.a{color:#b25000}.bv.g{color:#1a7f37}`)
	b.WriteString(`.track{height:6px;background:#f0f0f3;border-radius:980px;overflow:hidden;min-width:80px}`)
	b.WriteString(`.fill{display:block;height:100%;border-radius:980px}`)
	b.WriteString(`.fill.g{background:#1a7f37}.fill.a{background:#b25000}.fill.r{background:#bf0711}`)
	b.WriteString(`.tb{display:flex;flex-direction:column;gap:5px;min-width:120px}`)
	b.WriteString(`.dash{color:#86868b}`)
	b.WriteString(`</style></head><body><div class="wrap">`)

	b.WriteString(`<div class="title">Modelscope 限流</div>`)
	fmt.Fprintf(&b, `<div class="sub">供应商 <b>%s</b> · 总共 <b>%d</b> · 已禁用 <b>%d</b> · 可用 <b>%d</b> · 下次重置 <b>%s</b>（每 5 秒自动刷新）</div>`,
		html.EscapeString(providerLabel), total, disabled, available, reset.In(loc).Format("2006-01-02 15:04:05 MST"))

	cdCount, cdWait := s.CooldownStats()
	if cdCount > 0 || cdWait > 0 {
		b.WriteString(`<div class="cards" style="margin-bottom:22px">`)
		fmt.Fprintf(&b, `<div class="card"><div class="k">退避触发</div><div class="v">%d 次</div></div>`, cdCount)
		fmt.Fprintf(&b, `<div class="card"><div class="k">累计等待</div><div class="v">%s</div></div>`, formatDuration(cdWait))
		b.WriteString(`</div>`)
	}

	if len(modelNames) > 0 {
		b.WriteString(`<div class="section">按模型可用次数汇总</div><div class="cards">`)
		for _, m := range modelNames {
			ms := models[m]
			availStr := fmt.Sprintf("%d", ms.available)
			maClass := "ma"
			if ms.available <= 0 {
				maClass = "ma ma-r"
			}
			fmt.Fprintf(&b, `<div class="mcard"><div class="mn">%s</div><div class="%s">%s</div><div class="mk">%d 个活跃 key · %d 个已禁用</div></div>`,
				html.EscapeString(m), maClass, availStr, ms.active, ms.disabled)
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(`<div class="section">Key 明细</div>`)
	b.WriteString(`<table><thead><tr><th>Key</th><th>状态</th><th>模型剩余</th><th>总剩余</th></tr></thead><tbody>`)
	if len(authIDs) == 0 {
		b.WriteString(`<tr><td colspan="4" class="dash">暂无已解析的 key。</td></tr>`)
	}
	for _, id := range authIDs {
		mk := masked[id]
		if mk == "" {
			mk = id
		}
		kv, ok := snap[id]
		b.WriteString(`<tr><td class="key" title="`)
		b.WriteString(html.EscapeString(id))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(mk))
		if showProvider {
			pn := provOfKey[id]
			if pn == "" {
				pn = providerOf(cfg, id)
			}
			if pn != "" {
				fmt.Fprintf(&b, `<span class="prov">%s</span>`, html.EscapeString(pn))
			}
		}
		b.WriteString(`</td>`)
		// 状态：仅在确有禁用时才标记。原先只要 key 有观测数据就落入
		// "模型禁用" 分支，导致仅剩次数被观测但未禁用的活跃 key 被误标。
		switch {
		case ok && kv.GlobalDisabled:
			fmt.Fprintf(&b, `<td><span class="badge b-glob">全部禁用</span><span class="when">禁用于 %s</span></td>`, kv.GlobalSince.In(loc).Format("15:04:05"))
		case ok && anyModelDisabled(kv.Models):
			sortedModels := append([]ModelView(nil), kv.Models...)
			sort.Slice(sortedModels, func(i, j int) bool {
				ei, ej := modelAvailable(sortedModels[i], kv.GlobalDisabled), modelAvailable(sortedModels[j], kv.GlobalDisabled)
				if ei != ej {
					return ei > ej
				}
				return sortedModels[i].Name < sortedModels[j].Name
			})
			var dis strings.Builder
			dis.WriteString(`<span class="badge b-mod">模型禁用</span>`)
			for _, mv := range sortedModels {
				if mv.Disabled {
					fmt.Fprintf(&dis, `<span class="when">%s</span>`, html.EscapeString(mv.Name))
				}
			}
			fmt.Fprintf(&b, `<td>%s</td>`, dis.String())
		default:
			b.WriteString(`<td><span class="badge b-ok">全部正常</span></td>`)
		}
		// 模型剩余（每模型一条进度条）。额度为 0 时模型名/数值/条为红色，
		// 否则为绿色（按剩余比例填充宽度）；无观测数据时仅显示灰名 + —。
		// 模型剩余（每模型一条进度条，按可用次数从高到低排序）。
		if ok && len(kv.Models) > 0 {
			sortedModels := append([]ModelView(nil), kv.Models...)
			sort.Slice(sortedModels, func(i, j int) bool {
				ei, ej := modelAvailable(sortedModels[i], kv.GlobalDisabled), modelAvailable(sortedModels[j], kv.GlobalDisabled)
				if ei != ej {
					return ei > ej
				}
				return sortedModels[i].Name < sortedModels[j].Name
			})
			b.WriteString(`<td><div class="mbars">`)
			for _, mv := range sortedModels {
				cls := modelRemClass(mv, kv.GlobalDisabled)
				c := classSuffix(cls)
				num := "—"
				w := 0
				if mv.HasRem {
					if mv.HasLim {
						num = fmt.Sprintf("%d / %d", mv.Remaining, mv.Limit)
					} else {
						num = fmt.Sprintf("%d", mv.Remaining)
					}
					w = barWidthPct(mv.Remaining, mv.Limit, mv.HasLim)
				}
				fmt.Fprintf(&b, `<div class="mb-h"><span class="mb-n%s">%s</span><span class="bv%s">%s</span></div>`,
					c, html.EscapeString(mv.Name), c, num)
				if mv.HasRem {
					fmt.Fprintf(&b, `<div class="track"><i class="fill%s" style="width:%d%%"></i></div>`, c, w)
				}
			}
			b.WriteString(`</div></td>`)
		} else {
			b.WriteString(`<td class="dash">—</td>`)
		}
		// 总剩余（key 总额度：数值 + 进度条）。
		switch {
		case ok && kv.HasTotalRem:
			cls := barColorClass(kv.TotalRemaining, kv.TotalLimit, kv.HasTotalLim, kv.GlobalDisabled)
			num := fmt.Sprintf("%d", kv.TotalRemaining)
			if kv.HasTotalLim {
				num = fmt.Sprintf("%d / %d", kv.TotalRemaining, kv.TotalLimit)
			}
			w := barWidthPct(kv.TotalRemaining, kv.TotalLimit, kv.HasTotalLim)
			fmt.Fprintf(&b, `<td><div class="tb"><span class="bv %s">%s</span><div class="track"><i class="fill %s" style="width:%d%%"></i></div></div></td>`, cls, num, cls, w)
		default:
			b.WriteString(`<td class="dash">—</td>`)
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table>`)
	b.WriteString(`</div></body></html>`)
	return b.String()
}

// GuidanceReason explains why the guidance page is shown.
type GuidanceReason int

const (
	// GuidanceMissing is shown when providers or management_key is empty.
	GuidanceMissing GuidanceReason = iota
	// GuidanceResolutionFailed is shown when server-side resolution failed
	// (wrong management_key, unreachable host_base_url, etc.).
	GuidanceResolutionFailed
	// GuidanceProviderNotFound is shown when none of the configured providers
	// matches an openai-compatibility entry upstream.
	GuidanceProviderNotFound
)

// RenderGuidanceHTML renders the configuration guide. It always directs the
// operator to the management center's "插件管理 → 编辑配置" instead of
// hand-editing config.yaml. detail carries an extra hint (the resolution
// error, or the comma-joined unmatched provider list). The page auto-refreshes
// every 15s so it transitions to the status page once the config is fixed and
// the plugin is reloaded (reconfigure clears the resolution cache).
// RenderGuidanceHTML renders the Apple-style configuration guide. It always
// directs the operator to the management center's "插件管理 → 编辑配置" instead
// of hand-editing config.yaml. detail carries an extra hint (the resolution
// error, or the comma-joined unmatched provider list). The page auto-refreshes
// every 15s so it transitions to the status page once the config is fixed and
// the plugin is reloaded (reconfigure clears the resolution cache).
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

	// Notice card: the specific reason this guide is shown.
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
	default: // GuidanceMissing
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
