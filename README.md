# 🛡️ modelscope-ratelimit

> CLIProxyAPI 的 Modelscope 限流插件：限额账号智能禁用、分组凭据智能调度、指数退避避流重试。

![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
![CLIProxyAPI](https://img.shields.io/badge/CLIProxyAPI-v7.2.51+-blue)
![License](https://img.shields.io/badge/license-MIT-yellow)
![Build](https://img.shields.io/badge/build-c--shared-black)

## 📖 About

Modelscope 免费额度采用**共享配额**机制——多个 key、多个托管 provider 共用同一天配额。高并发下缺乏上游限流感知，容易陷入「请求 → 429 → 重试 → 再 429」的恶性循环；跑满额度的 key 仍被反复选中，导致死锁报错并浪费请求次数。本插件作为 CLIProxyAPI 原生动态库插件接入宿主调度链路，按 key/model 维度实时管控限流状态。

### ✨ 核心特性

- 🔴 **限额账号智能禁用**：实时解析响应头剩余次数，单模型耗尽仅禁该模型、总额度耗尽全局禁用该 key，次日 00:00（按配置时区）自动恢复，调度时精准跳过禁用凭据并分发至健康账号。
- 🔁 **分组凭据智能选择**：按 provider 分组，内置 `round-robin`（轮询）/ `fill-first`（填充优先）策略，`providers` 顺序即优先级，前组额度耗尽后才回退后续组。
- ⏳ **指数退避避流重试**：连续 429（`insufficient_quota`）时全局阻塞，退避按 ×2 递增（10→20→40→60s 封顶）并叠加随机抖动防惊群，成功响应自动重置。
- 🌐 **代理模式**：配置 `proxy_url` 后，托管 provider 收到 429 时探测代理可达性并经管理 API 开启全局上游代理，成功后自动关闭并还原宿主原代理。
- 📊 **状态页**：管理端实时展示掩码 key、模型剩余进度条与可用计数，5s 自动刷新；管理密钥仅服务端使用，不渲染进未鉴权页面。

## 🧰 Tech Stack

| 类别 | 技术 / 依赖 |
| --- | --- |
| 语言 | Go 1.26（`go.mod` 声明 `go 1.26.0`） |
| 构建模式 | CGO `c-shared`（导出 C ABI 动态库 `.so` / `.dylib` / `.dll`） |
| 宿主 SDK | `github.com/router-for-me/CLIProxyAPI/v7 v7.2.51`（`sdk/pluginabi` 方法常量路由） |
| 依赖库 | `gopkg.in/yaml.v3 v3.0.1`（配置解析） |
| 核心层约束 | `internal/ratelimit/` 纯标准库，不引入宿主 SDK / cgo，可独立 `go test -race` |
| 管理调用 | 插件自带 `net/http` 直连宿主管理 API（不经 cgo 回调） |
| 文档站 | VitePress（仓库根 `docs/` 提供插件开发规范） |

## 🚀 Getting Started

### 前置依赖

- **Go ≥ 1.26**：`go.mod` 声明 `go 1.26.0`，首次构建需 `GOTOOLCHAIN=auto` 自动拉取工具链。
- **CGO 环境**：`build.sh` 默认 `CGO_ENABLED=1`，需本机 C 编译器（gcc/clang）。
- **交叉编译 Windows**：需 [zig](https://ziglang.org/) 0.13+ 作为 C 交叉编译器。
- **CLIProxyAPI v7.2.51+**：插件经稳定 C ABI 由宿主加载，产物须与宿主进程同架构。
- **国内网络**：`GOPROXY=https://goproxy.cn,direct`（`build.sh` 已内置默认值）。

### 安装与配置

```bash
git clone <仓库地址>
cd modelscope-ratelimit

bash build.sh             # 编译当前平台原生动态库 (.so/.dylib/.dll)
bash build.sh windows     # 交叉编译 Windows/amd64 .dll（需 zig）
bash build.sh all         # 原生 + Windows 均编译
bash build.sh test        # 核心状态机单测（含 -race）
bash build.sh vet         # go vet ./...
```

> ⚠️ 产物文件名必须为 `modelscope-ratelimit.so` / `.dll`，需与配置中的插件 ID `modelscope-ratelimit` 对应，否则宿主无法加载。

将 Modelscope 配置为 `openai-compatibility` 多 key provider，并在 CLIProxyAPI 的 `config.yaml` 中启用插件：

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
      providers: ["modelscope"]              # 监控的供应商名称，必填，顺序=优先级
      host_base_url: "http://127.0.0.1:8317" # CPA 管理 API 地址（选填）
      management_key: "your-management-secret" # CPA 管理密钥（必填，仅服务端使用）
      credential_strategy: "round-robin"     # round-robin | fill-first
      insufficient_quota_cooldown: 10         # 退避基准秒数，封顶 60s
      timezone: "Asia/Shanghai"              # 每日 00:00 重置时区
      disable_threshold: 0                   # 剩余次数 ≤ 此值即当日禁用，默认 0（耗尽）
      # proxy_url: "socks5://user:pass@127.0.0.1:1080"  # 429 时自动代理（选填）
      # managed_models: ["Qwen-72B"]          # 仅监控这些别名模型，留空=全部
      # skip_provider_on_429: false           # 429 时一次性跳至非托管 provider
```

## 📦 Usage

插件入口为 [cmd/plugin/main.go](cmd/plugin/main.go)，导出 `cliproxyPluginCall` / `cliproxyPluginFree` / `cliproxyPluginShutdown` 三个 C ABI 函数，由宿主按声明的能力调用对应方法。注册时声明 `scheduler`、`response_interceptor`、`response_stream_interceptor`、`usage_plugin`、`management_api` 五项能力。

不经脚本直接单测核心状态机：

```bash
GOTOOLCHAIN=auto go test -race -count=1 ./internal/ratelimit/
```

请求处理链路：

```text
请求 → scheduler.pick（跳过禁用 key/model，按优先级选健康凭据）
     → 上游请求
     → response / stream 拦截
        ├─ 有限流头 → 解析剩余次数，耗尽则禁用
        └─ 429 insufficient_quota → 全局阻塞退避（×2 递增，封顶 60s）
     → 00:00 自动重置禁用状态
```

- **优先级**：`providers` 顺序即优先级，前组额度耗尽后才用后续组。
- **作用域**：单模型耗尽只禁该模型，总额度耗尽全局禁用，非托管 provider 不受影响。
- **退避**：同 provider 共享配额，冷却无关 key，全局递增避免二次限流。

## 🗂️ Project Structure

```text
modelscope-ratelimit/
├── cmd/plugin/                    # C-ABI 胶水层：方法分发、配置解析、panic 恢复、管理 API 调用与状态页渲染
│   ├── main.go                    # 唯一导出入口，映射插件方法到核心层 helper
│   └── stableid_test.go           # 稳定 ID 单测
├── internal/ratelimit/            # 限流状态机核心（纯标准库，可独立 -race 单测）
│   ├── apply.go                   # 限流头解析 + 冷却钩子
│   ├── config.go                  # 配置结构 + ManagesProvider 匹配
│   ├── doc.go                     # 包文档与业务规则说明
│   ├── proxy.go                   # 代理模式：探测、开关、还原宿主代理
│   ├── scheduler.go               # 全局阻塞 + 优先级调度
│   ├── state.go                   # 并发状态：禁用 / 冷却 / 退避 / 快照
│   ├── status_html.go             # 状态页 HTML 渲染
│   ├── types.go                   # JSON 线格式（镜像文档载荷）
│   └── *_test.go                  # 核心层单测（含 -race）
├── build.sh                       # 编译 / 测试 / vet 脚本
├── config.example.yaml            # 配置示例
├── go.mod / go.sum                # 模块声明与依赖校验
└── LICENSE                        # MIT 许可证
```

## 📄 License

本项目采用 [MIT](LICENSE) 协议开源，版权归 © 2026 bytehola 所有。
