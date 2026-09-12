# 🛡️ modelscope-ratelimit

> CLIProxyAPI 的 Modelscope 限流插件：限额账号禁用、凭据轮换、魔粒监控与自动签到。

![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
![CLIProxyAPI](https://img.shields.io/badge/CLIProxyAPI-v7.2.51+-blue)
![License](https://img.shields.io/badge/license-MIT-yellow)
![Build](https://img.shields.io/badge/build-c--shared-black)
![Version](https://img.shields.io/badge/version-2.4.9-green)

## 核心特性
- 🎲 **自动签到 + 魔粒监控**：每日 00:00 签到领取魔粒，定时拉取余额与消费流水。
- 📊 **状态页**：管理中心查看余额、消耗，5 秒刷新。
- 🔴 **限额账号禁用**：余额耗尽禁用该 key，次日 00:00（按配置时区）自动恢复，调度跳过禁用凭据。
- 🔁 **凭据轮换**：支持 `round-robin` / `fill-first`。
- 🧪 **use_responses 透传**：填上游模型 `name` 命中即透传 Respnses API。
- 📜 **日志接入宿主**：插件日志经 `host.log` 写入 CPA 自己的日志文件，`GET /v0/management/logs` 与管理中心可直接检索；明细调度为 debug 级，需打开 CPA 调试模式。

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

```

## License

MIT
