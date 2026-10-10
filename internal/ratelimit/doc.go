// Package ratelimit 实现 CLIProxyAPI 的 Modelscope 账号级魔粒限流控制。
//
// 本包刻意不引用任何 CLIProxyAPI SDK，这样状态机、每日 00:00 重置、轮询调度和
// 响应判定都能脱离宿主独立做单测。cmd/plugin 里的薄胶水层把这些 helper 接到文档
// 约定的插件钩子上（scheduler.pick、response.intercept_after、
// response.intercept_stream_chunk、usage.handle）。
//
// 业务规则：
//
//   - 429 + insufficient balance（该账号魔粒耗尽）：这把 key 当天全局禁用。
//   - 403 + "账号未完成邮箱验证"（文案里带 email verification / verified accounts）：验证之前魔粒
//     确实不可用，所以与魔粒耗尽同路，这把 key 当天全局禁用、状态页写明"未验证邮箱"；在本请求内
//     继续换下一把 key，因为池里别的账号能正常服务。
//   - 429 + insufficient_quota（整家共享的额度耗尽）：skip_provider_on_429 开启
//     时整个 provider 进 latch 的 skip 集，下一次挑选直接落到下一个 provider 或
//     非托管兜底；关闭时按 insufficient_quota_cooldown 给该 key 设指数退避（基准
//     秒数，连续失败翻倍，封顶 60 秒）。
//   - Responses API 以 HTTP 200 + SSE response.failed 回送的 rate_limit_exceeded：只算
//     这一把 key 的速率/额度拒绝，按 insufficient_quota_cooldown 冷却后在本家内换下一把
//     key；只有事件 body 里同时带 insufficient_quota 才算池级，才允许 skip_provider_on_429
//     把整家摘掉。
//   - Responses API 以 HTTP 200 回送的"客户端内容被上游审查拦下"（DataInspectionFailed，
//     上游会把 error.code 降级成 server_error，所以判据只能落在 code 与文案上）：同一个请求体
//     在任何一把 key、任何一家上都会撞上同一条规则，轮换只是把同一个非法请求重复投递。因此既不
//     记凭据失败也不轮换，执行器直接把 400 报错给客户端，与 Chat 侧上游回 400 的表现一致。上游
//     写明拦的是模型输出时不算在内——换一把 key 重新生成有可能过审，仍按常规轮换。
//   - 405 Method Not Allowed：请求体里含有被上游拦截的字符,本家所有 key 都会以同样的方式被拒。
//     判据不看 body，与池级 insufficient_quota 共用同一个分支：skip_provider_on_429 开启时整家
//     进 skip 集，关闭时继续逐 key 轮换。
//   - 503 + model_not_ready（上游模型还在加载，文案是"模型尚未就绪：8卡加载中"）：与 405 同类的
//     provider 级可用性拒绝，本家所有 key 都会同样被拒，逐 key 轮换只是白撞。判据必须同时看状态码
//     与 body——状态码写死 503（502/504 多出在网关链路，摘家会误伤好凭据），body 里要有带引号的
//     "model_not_ready"。与 405、池级 insufficient_quota 共用同一个分支：skip_provider_on_429
//     开启时整家进 skip 集，关闭时继续逐 key 轮换。skip 集由任意 200 清空，正好让下一次请求再探一次。
//   - 401 Unauthorized：该 key 全局禁用直到插件重启（午夜不恢复）。
//   - 到配置时区的 00:00，日禁用状态清空，key 重新可调度。
//   - 调度时跳过已禁用 key，把请求交给下一个可用账号，而不是直接失败。
//   - providers 的填写顺序就是轮换顺序：请求先从第一个 provider 开始，在本 provider
//     内逐 key 尝试（key 级失败只淘汰当前 key），本家没有可用 key 才轮到下一个
//     provider；任意 200 让下一次请求重新从第一个 provider 开始。
package ratelimit
