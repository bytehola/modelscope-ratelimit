package ratelimit

import (
	"errors"
	"fmt"
	"time"
)

// ErrAllCredentialsDisabled 由 SchedulerPick 返回：该模型的候选凭据当天全部被
// 限用，并且没有任何非托管 provider 可以兜底。
var ErrAllCredentialsDisabled = errors.New("all credentials disabled for model (daily rate limit exhausted)")

// ErrPinnedProviderUnavailable 在嵌套执行已经钉定某个托管 provider（见
// PinnedProviderHeader）、而该 provider 已无可用凭据时返回。调用方自己的那一步必须
// 失败，而不是顺位落到别的 provider 上 —— 钉定的意义正在于此；如果返回
// Handled=false，宿主就会替我们选别人的 key。
var ErrPinnedProviderUnavailable = errors.New("pinned provider has no available credential")

// SchedulerPick 实现 scheduler.pick，与 CLIProxyAPI 的多账号轮换配合：已被禁用的凭据
// 会被跳过，请求交给下一个可用账号。魔粒体系下禁用是凭据级的，对该凭据的所有模型生效。
//
// provider 的归属按每个 Candidate 判定，而不是看顶层 req.Provider：在 CLIProxyAPI 的
// "mixed" 调度路径里请求级 Provider 是空的，真正的运行时 key 在每个
// Candidate.Provider 上（例如 "openai-compatible-modelscope"）。
//
// provider 轮换：始终按 Providers 的配置顺序，从第一个还有可用候选的 provider 开始；
// 一家之内的 key 由该家自己的游标轮转（跨请求保持）。skip_provider_on_429 开启时，
// 返回过池级 insufficient_quota 的 provider 会被放进 skip 集，它的候选一直被过滤到
// 下一次 200；关闭时不写 skip 集，改为对该 key 施加退避冷却，冷却结束后仍优先用这一
// 家的下一把 key，本家没有可用 key 才轮到下一家。
//
// 非托管 provider 兜底：当该模型的托管候选全部不可用时，只要还有非托管 provider，
// 插件就不能硬阻塞请求。宿主的 availableAuthsForRouteModel 只传最高 priority 那一
// 组，因此优先级更低的非托管候选可能不在 req.Candidates 里（虽然实际存在）。两种情况
// 分别处理：
//   - 候选列表里能看到非托管凭据（同优先级）：直接从中挑一个优先级最高的
//     （Handled=true），不把请求浪费在已禁用的托管 key 上。
//   - 看不到非托管候选、但 req.Providers 里列出了非托管 provider：让位
//     （Handled=false），由宿主的内置轮换最终触达那些低优先级的非托管 key。
//
// 只有在完全不存在非托管 provider 时，插件才返回 ErrAllCredentialsDisabled，让宿主快
// 速失败，不再把请求浪费在已禁用的托管 key 上。
func (s *Store) SchedulerPick(req SchedulerPickRequest) (SchedulerPickResponse, error) {
	cfg := s.config()
	now := s.now()

	// 来自 Responses 执行器嵌套 chat 调用的请求级钉定。设置后，下面所有阶段只考虑这一
	// 个 provider：不为别的 provider 做冷却阻塞判断，不做非托管兜底，也不跨家轮换。
	pinned := cfg.PinnedProvider(req)
	pinnedIdx := -1
	if pinned != "" {
		pinnedIdx = cfg.ProviderIndex(pinned)
	} else if raw := headerValue(req.Options.Headers, PinnedProviderHeader); raw != "" && !isInternalExecution(req.Options.Metadata) {
		// 要么是客户端猜到了这个头名（应当忽略），要么是宿主不再把嵌套执行标记为内部
		// 来源 —— 后者会把协议降级悄悄放回来。这种情况要大声说出来。
		s.log.Printf("modelscope-ratelimit: ignoring pin provider=%s without internal-source metadata", raw)
	}

	// 预扫描：本次请求里还有托管候选吗？当所有托管 key 都进了宿主的 tried 集合（从候
	// 选里消失）时，请求正在往非托管 provider 兜底，此时全局冷却阻塞不该去拖这次兜底。
	hasManaged := false
	for _, c := range req.Candidates {
		if pinnedIdx >= 0 && cfg.ProviderIndex(c.Provider) != pinnedIdx {
			continue
		}
		if cfg.ManagesProvider(c.Provider) {
			hasManaged = true
			break
		}
	}

	// 全局 insufficient_quota 阻塞：Modelscope 的额度在所有 key 以及所有托管 provider
	// 之间共享（例如 moda + modelscope），所以任意一把托管 key 进入冷却就会阻塞全部托
	// 管调度，避免在已耗尽的共享额度上疯狂打 429。睡醒之后冷却已到期，key 重新可用。
	// 仅当确实还有托管候选要重试时才生效（hasManaged）；已经把托管 key 用尽、正在往非
	// 托管 provider 兜底的请求不会被阻塞。
	// skip_provider_on_429 模式完全绕过这段阻塞：它靠按 provider 的 skip 集（不是逐
	// key 冷却）来决策，所以开启本模式之前遗留的冷却不应该挡住随后进行的 skip 判断。
	// 在这里阻塞会留下一个窗口：flag 被并发的挑选或某次托管 200 消费/清掉，导致 skip
	// 永远不生效。
	if hasManaged && cfg.InsufficientQuotaCooldown > 0 && !cfg.SkipProviderOn429 {
		if wait := s.cooldownWaitDuration(now); wait > 0 {
			// 睡觉之前先记下当前的最长冷却到期时刻。睡醒之后，并发的请求可能已经在另一
			// 把 key 上设置了新的（更长的）冷却。重查时把睡后的最大到期时刻与睡前的比
			// 较：如果往后推了，说明睡眠期间有新的冷却，我们继续阻塞剩余时间。
			// cooldownWaitDuration 返回的是 expiry.Sub(now)，所以第一次睡眠已经消耗掉
			// 的时间不会被重复等待。
			expiryBefore := now.Add(wait)
			s.log.Printf("modelscope-ratelimit: block %s model=%s (insufficient_quota cooldown)",
				wait.Truncate(time.Millisecond), s.displayName(req.Model))
			s.blockEnter()
			time.Sleep(wait)
			s.blockLeave()
			now = s.now()
			if wait2 := s.cooldownWaitDuration(now); wait2 > 0 {
				if expiryAfter := now.Add(wait2); expiryAfter.After(expiryBefore) {
					s.log.Printf("modelscope-ratelimit: block %s model=%s (insufficient_quota cooldown, new during sleep)",
						wait2.Truncate(time.Millisecond), s.displayName(req.Model))
					s.blockEnter()
					time.Sleep(wait2)
					s.blockLeave()
					now = s.now()
				}
			}
		}
	}

	// 把候选分成托管（在管辖范围内）与非托管两组。可用的托管候选按其在 cfg.Providers
	// 里的位置分组，这样插件就能优先选择排在前面的 provider。
	availableByProvider := make(map[int][]Candidate)
	nonManaged := make([]Candidate, 0, len(req.Candidates))
	managed := 0
	availableCount := 0
	seenIDs := make([]string, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		if pinnedIdx >= 0 && cfg.ProviderIndex(c.Provider) != pinnedIdx {
			// 这次嵌套调用是留给被钉定的那个 provider 的；其他托管和非托管 provider 会
			// 在执行器循环里拿到各自的那一步。
			continue
		}
		if !cfg.ManagesProvider(c.Provider) {
			nonManaged = append(nonManaged, c)
			continue
		}
		managed++
		seenIDs = append(seenIDs, c.ID)
		if s.isDisabled(c.ID, req.Model, now) {
			continue
		}
		// skip_provider_on_429：过滤掉最近返回过池级 insufficient_quota 的 provider 的
		// 候选。粒度是 provider：只有真正池级耗尽的那一家被跳过，其他托管 provider 照常可用。
		if cfg.SkipProviderOn429 && s.IsProviderSkipped(extractProviderName(c.Provider)) {
			continue
		}
		idx := cfg.ProviderIndex(c.Provider)
		availableByProvider[idx] = append(availableByProvider[idx], c)
		availableCount++
	}
	s.recordSeen(seenIDs)

	// 钉定的那一步不能悄悄变成别人的步骤：直接失败，让执行器继续走下一个 provider 自己
	// 的那一步（很可能是原生 Responses）。
	if pinnedIdx >= 0 && availableCount == 0 {
		return SchedulerPickResponse{}, fmt.Errorf("%w: provider=%q model=%q", ErrPinnedProviderUnavailable, cfg.Providers[pinnedIdx], s.displayName(req.Model))
	}

	// 没有任何托管候选 => 不在管辖范围内，交回内置调度器。
	if managed == 0 {
		// 该模型的托管候选都已进入宿主的 “tried” 集合或不可用。如果 skip 集里还有 provider（没有 200 来
		// 清空它），就把整个集合清掉，让下一次请求重新开始 —— 托管额度可能已经恢复。这
		// 样能避免集合被卡死：当托管和非托管候选全部失败时，宿主会直接返回错误而不再调
		// 用插件，这里是最后一次清理机会。
		if s.HasSkippedProviders() {
			s.ClearAllSkippedProviders()
			s.SetSkipRoundDone()
		}
		return SchedulerPickResponse{Handled: false}, nil
	}

	disabled := managed - availableCount
	if availableCount == 0 {
		// 托管候选全部被禁用或跳过。在硬阻塞请求之前，先尝试非托管兜底。

		// 情况 1：能看到非托管候选。在最高优先级那一组里用宿主自己的路由策略挑选（策略
		// 通过管理 API 取得），让插件的非托管兜底与宿主语义一致，而不是强加固定顺序。
		if group := highestPriorityGroup(nonManaged); len(group) > 0 {
			picked, _ := s.pickByStrategy(group, s.hostStrategy())
			s.log.Printf("modelscope-ratelimit: all managed disabled, fallback model=%s -> %s",
				s.displayName(req.Model), picked.ID)
			return SchedulerPickResponse{AuthID: picked.ID, Handled: true}, nil
		}

		// 情况 2：看不到非托管候选，但该 route 接受非托管 provider。它们可能优先级更低
		// 被宿主过滤掉了，或者处在宿主的冷却里。让位给宿主，让内置轮换最终触达它们。
		if routeHasNonManagedProvider(req.Providers, cfg) {
			return SchedulerPickResponse{Handled: false}, nil
		}

		// 情况 3：完全不存在非托管 provider，明确报错。若 skip_provider_on_429 生效且有
		// 托管 provider 在 skip 集里，报错之前先清空集合，让下一次请求重新探测托管额度
		// （insufficient_quota 是池级的，速率窗口可能已经恢复）。
		if s.HasSkippedProviders() {
			s.ClearAllSkippedProviders()
			s.SetSkipRoundDone()
			s.log.Printf("modelscope-ratelimit: skip managed on 429, no fallback model=%s", s.displayName(req.Model))
			return SchedulerPickResponse{}, fmt.Errorf("%w: model=%q (insufficient_quota, no non-managed fallback)", ErrAllCredentialsDisabled, s.displayName(req.Model))
		}
		s.log.Printf("modelscope-ratelimit: all disabled model=%s 已限: %d/ 可用: %d / 总共: %d",
			s.displayName(req.Model), disabled, 0, managed)
		return SchedulerPickResponse{}, fmt.Errorf("%w: model=%q", ErrAllCredentialsDisabled, s.displayName(req.Model))
	}

	var picked Candidate
	// provider 轮换：从配置里的第一个 provider 开始，依次找到第一个还有可用候选的
	// provider；skip_provider_on_429 开启时，skip 集已经过滤掉最近池级额度耗尽的那一家。选中
	// 一家之后，由该家自己的 key 游标（NextProviderKey）决定用哪把 key，该游标跨请求
	// 继续轮转。
	start := s.managedProviderStart(len(cfg.Providers))
	for step := 0; step < len(cfg.Providers); step++ {
		i := (start + step) % len(cfg.Providers)
		if candidates, ok := availableByProvider[i]; ok && len(candidates) > 0 {
			ids := make([]string, 0, len(candidates))
			byID := make(map[string]Candidate, len(candidates))
			for _, candidate := range candidates {
				ids = append(ids, candidate.ID)
				byID[candidate.ID] = candidate
			}
			s.recordObservedOrder(cfg.Providers[i], candidates)
			authID := s.NextProviderKey(cfg.Providers[i], ids)
			if selected, ok := byID[authID]; ok {
				picked = selected
				break
			}
			picked = candidates[0]
			break
		}
	}

	// 只在确实有凭据被禁用时才打印限流概况，避免正常流量下的噪声。
	if disabled > 0 {
		rem := s.ModelRemaining(req.Model, now)
		s.log.Printf("modelscope-ratelimit: schedule model=%s 可用魔粒: %d / 已限: %d/ 可用: %d / 总共: %d -> %s",
			s.displayName(req.Model), rem, disabled, availableCount, managed, picked.ID)
	}

	return SchedulerPickResponse{AuthID: picked.ID, Handled: true}, nil
}

// pickByStrategy 按给定策略从非空切片里挑一个非托管候选："fill-first" 永远返回第一
// 个；"round-robin"（默认）用 store 里的非托管游标轮转。该游标与托管的各家 key 游标相
// 互独立，所以非托管轮换绝不会污染托管顺序。仅在切片为空时 ok=false。
func (s *Store) pickByStrategy(candidates []Candidate, strategy string) (Candidate, bool) {
	if len(candidates) == 0 {
		return Candidate{}, false
	}
	if strategy == "fill-first" {
		return candidates[0], true
	}
	// Add(1) 返回的是自增之后的值（第一次调用得到 1），所以减 1 让第一次挑选落在下标 0
	// 上，随后按切片顺序轮转。
	n := s.nonManagedRR.Add(1)
	idx := int((n - 1) % uint64(len(candidates)))
	return candidates[idx], true
}

// highestPriorityGroup 返回 Priority 最大的那一组候选。宿主的
// availableAuthsForRouteModel 本来只传最高优先级那一组，但当多个非托管 provider 处于
// 同一优先级时它们会一起到达 —— 这里再过滤出那一层，让 pickByStrategy 能在其中轮转。
func highestPriorityGroup(candidates []Candidate) []Candidate {
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0].Priority
	for _, c := range candidates[1:] {
		if c.Priority > best {
			best = c.Priority
		}
	}
	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Priority == best {
			out = append(out, c)
		}
	}
	return out
}

// routeHasNonManagedProvider 判断 req.Providers 里是否存在不属于本插件托管集合的
// provider key。
func routeHasNonManagedProvider(routeProviders []string, cfg *Config) bool {
	for _, p := range routeProviders {
		if !cfg.ManagesProvider(p) {
			return true
		}
	}
	return false
}
