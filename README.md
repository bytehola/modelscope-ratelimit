# 🛡️ modelscope-ratelimit

> CLIProxyAPI 的 Modelscope 限流插件：限额账号禁用、凭据轮换、魔粒监控与自动签到。

![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
![CLIProxyAPI](https://img.shields.io/badge/CLIProxyAPI-v7.2.51+-blue)
![License](https://img.shields.io/badge/license-MIT-yellow)
![Build](https://img.shields.io/badge/build-c--shared-black)
![Version](https://img.shields.io/badge/version-2.3.1-green)

## 核心特性

- 🔴 **限额账号禁用**：余额耗尽禁用该 key，次日 00:00（按配置时区）自动恢复，调度跳过禁用凭据。
- 🔁 **凭据轮换**：支持 `round-robin` / `fill-first`。每次请求从第一个 provider 开始，429 切下一个 provider，200 后下一请求回到第一个 provider；key 在各 provider 内跨请求轮换。
- ⚡ **skip_provider_on_429 兜底**：开启后 429 立即用非托管模型重试，避免等待，推荐打开。
- 🧪 **use_responses 透传**：开启后将 OpenAI Responses API 请求透传到上游 /v1/responses，并复用现有限流/key 轮换；默认关闭。
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
      use_responses: false # 开启后 Responses API 请求透传到上游 /v1/responses
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
