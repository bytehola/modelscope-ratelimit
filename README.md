# 🛡️ modelscope-ratelimit

> CLIProxyAPI 的 Modelscope 限流插件：限额账号禁用、凭据轮换、魔粒监控与自动签到。

![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
![CLIProxyAPI](https://img.shields.io/badge/CLIProxyAPI-v7.2.51+-blue)
![License](https://img.shields.io/badge/license-MIT-yellow)
![Build](https://img.shields.io/badge/build-c--shared-black)
![Version](https://img.shields.io/badge/version-2.4.5-green)

## 核心特性

- 🔴 **限额账号禁用**：余额耗尽禁用该 key，次日 00:00（按配置时区）自动恢复，调度跳过禁用凭据。
- 🔁 **凭据轮换**：支持 `round-robin` / `fill-first`。每次请求从第一个 provider 开始，**先在本家内逐个 key 轮换**（`insufficient balance` 这类 key 级 429 只禁用当前 key，下一把 key 同属这家），本家没有可用凭据了才切下一个 provider；任意 200 后下一请求回到第一个 provider。
- ⚡ **skip_provider_on_429 兜底**：只对**池级** `insufficient_quota` 生效（Chat 的 429 JSON、Responses 的 200 + SSE `response.failed` 都算）——开启后该 provider 整家立即摘掉（切下一家或直接非托管模型兜底），不必逐 key 撞墙，推荐打开；key 级失败不受该开关影响，仍然在本家内换 key。
- 🛡 **Responses 速率拒绝按 key 处理**：Responses 用 200 + SSE `response.failed`（`rate_limit_exceeded`）表达的拒绝只说明这一把 key 被限，不禁用 key、也不摘 provider，下一步继续本家换 key；为防止池级速率风暴把上百把 key 全撞一遍，同一次请求内同一家连续 3 次这类失败后才改试下一家，且该判定只在本次请求内有效，不写入跨请求 skip 集。
- 🧪 **use_responses 透传**：填上游模型 `name`（不是 alias）命中即透传，`"*"`/`"true"` 表示全部模型，`"false"` 或匹配不到模型的字符串表示关闭；多模型可写成一行字符串 `["model-a", "model-b"]`（也支持 `model-a, model-b` 与 YAML 数组），通过上游 /v1/responses 透传并复用现有限流/key 轮换；未列入的托管 provider 按 `providers` 顺序交由宿主以 Chat 协议兜底；上游用 HTTP 200 返回错误文档（`{"code","message"}` / 非空 `error`）时按失败处理并继续轮换，不会当成成功返回；默认关闭。
- 📜 **日志接入宿主**：插件日志经 `host.log` 写入 CPA 自己的日志文件，`GET /v0/management/logs` 与管理中心可直接检索；明细调度为 debug 级，需打开 CPA 调试模式。
- 🎲 **自动签到 + 魔粒监控**：每日 00:00 签到领取魔粒，定时拉取余额与消费流水。
- 📊 **状态页**：管理中心查看掩码 key、余额、消耗，5 秒刷新；管理密钥不渲染。

## 快速开始

依赖：Go ≥ 1.26、CGO + C 编译器、CLIProxyAPI v7.2.51+；跨平台交叉编译需 [zig](https://ziglang.org/) 0.13+。

```bash
git clone https://github.com/bytehola/modelscope-ratelimit.git
cd modelscope-ratelimit
bash build.sh            # 原生动态库
bash build.sh windows    # 交叉编译 Windows/amd64 .dll
bash build.sh all        # 原生 + Windows
bash build.sh test       # 核心单测（含 -race）
```

产物名必须为 `modelscope-ratelimit.so/.dll/.dylib`，放入宿主 `plugins/<GOOS>/<GOARCH>/`。

也可从 [Releases](https://github.com/bytehola/modelscope-ratelimit/releases) 下载预编译包，或经插件商店安装：

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/bytehola/modelscope-ratelimit/main/registry.json"
```

## 配置

在 CLIProxyAPI `config.yaml` 中启用：

```yaml
openai-compatibility:
  - name: "modelscope"
    base-url: "https://api-inference.modelscope.cn/v1"
    api-key-entries:
      - api-key: "ms-key-AAAAAAAAAAAA"
      - api-key: "ms-key-BBBBBBBBBBBB"
    models:
      - name: "Qwen/Qwen2.5-72B-Instruct"
        alias: "Qwen-72B"

plugins:
  enabled: true
  dir: "plugins"
  configs:
    modelscope-ratelimit:
      enabled: true
      priority: 10
      providers: ["modelscope"]
      skip_provider_on_429: false # 推荐 true
      use_responses: "*" # "*"或"true"=全部模型；"false"=关闭；单个上游模型名=仅该模型；多模型写一行字符串 ["model-a", "model-b"]
      host_base_url: "http://127.0.0.1:8317"
      management_key: "your-management-secret"
      credential_strategy: "round-robin" # round-robin | fill-first
      insufficient_quota_cooldown: 0.1 # 设为 0 跳过冷却
      timezone: "Asia/Shanghai"
```

完整字段见 [`config.example.yaml`](./config.example.yaml)。

## Usage

宿主启动 10 秒后自动拉取账号签到、魔粒余额和消耗数据。管理中心入口：**Modelscope 限流**。

```text
?api=accounts                    # 账号总览
?api=account&key=xxx             # 单账号详情
?api=transactions&key=xxx        # 消费流水
?api=checkin-all                 # 一键签到
?api=refresh-balance&key=xxx     # 刷新余额与消耗
```

### 日志

插件运行在自己的 Go runtime 里，早期版本的 `log.Printf` 只写到进程 stderr（即控制台窗口），
`logging-to-file: true` 时不会进入 `main.log`，管理中心的日志页也搜不到。现在统一走宿主的
`host.log` 回调：

- `info` 级：禁用、冷却、provider 跳过、上游失败、200-错误响应体等运维事件，始终可见。
- `debug` 级：每次 Responses 请求的候选顺序与实际命中 provider（`responses attempt/served`），
  需要打开 CPA 调试模式（管理中心 → 调试模式，或 `debug: true`）。
- 配置生效会打印一行 `configured v<版本> providers=... use_responses=...`。宿主在挂接日志文件之前
  就加载插件，所以这一行推迟到首个运行期调用（或启动 10 秒后的后台拉取）才写，空闲服务也会记一次；
  同一份配置重复 register/reconfigure 只打印一次，用来确认线上加载的是哪个构建。

检索示例：`GET /v0/management/logs?limit=500` 后过滤 `modelscope-ratelimit:`。
host.log 不可用（或宿主未就绪）时自动回退到进程 stderr，日志不会丢。

### Responses 协议怎么选

`/v1/responses` 由插件执行器接管后，按 `providers` 顺序逐个 provider 决定协议：

1. 该 provider 命中 `use_responses` → 插件直连它的上游 `/v1/responses`（原生透传）。
2. 未命中 → 交宿主按 Chat 协议兜底（宿主会为自己的嵌套执行重跑 scheduler.pick）。

顺序优先于协议：只要排在前面的 provider 能用 chat 答出来，就不会走到后面 provider 的原生透传。
同一个 provider 内的凭据按 `credential_strategy` 逐个尝试，一家把可用 key 试完才换下一家；
只有池级 `insufficient_quota`（`skip_provider_on_429` 开启时）会把整家立刻摘掉，key 级禁用不会。
兜底步骤会带上内部标记头 `X-Modelscope-Ratelimit-Provider` 把这次嵌套执行钉在该 provider 上：
没有它，该 provider 在嵌套执行里 429 后宿主会直接用后面 provider 的凭据应答，导致本该走原生
`/v1/responses` 的模型退化成 chat。钉住需要宿主标记的内部来源元数据，外部请求自带该头无效。

## 结构

```text
modelscope-ratelimit/
├── cmd/plugin/          # C-ABI 胶水层、配置解析、状态页
├── internal/ratelimit/  # 纯标准库限流状态机
├── demo/                # 模拟宿主调用链路
├── build.sh             # 编译/测试脚本
├── config.example.yaml  # 完整配置示例
└── registry.json        # 插件商店清单
```

## License

MIT
