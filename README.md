# 🛡️ modelscope-ratelimit

> CLIProxyAPI 的 Modelscope 限流插件：限额账号智能禁用、分组凭据智能调度、魔粒余额监控与自动签到。

![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
![CLIProxyAPI](https://img.shields.io/badge/CLIProxyAPI-v7.2.51+-blue)
![License](https://img.shields.io/badge/license-MIT-yellow)
![Build](https://img.shields.io/badge/build-c--shared-black)
![Version](https://img.shields.io/badge/version-2.2.1-green)

## 📖 About

Modelscope 免费额度采用**共享配额**机制——多个 key、多个托管 provider 共用同一天配额。高并发下缺乏上游限流感知，容易陷入「请求 → 429 → 重试 → 再 429」的恶性循环；跑满额度的 key 仍被反复选中，导致死锁报错并浪费请求次数。本插件作为 CLIProxyAPI 原生动态库插件接入宿主调度链路，按 key/model 维度实时管控限流状态。

### ✨ 核心特性

- 🔴 **限额账号智能禁用**：无魔粒禁用该 key，次日 00:00（按配置时区）自动恢复，调度时精准跳过禁用凭据并分发至健康账号。
- 🔁 **分组凭据智能选择**：按 provider 分组，内置 `round-robin`（轮询）/ `fill-first`（填充优先）策略，`providers` 顺序即优先级，前组额度耗尽后才回退后续组。
- ⚡ **skip_provider_on_429 兜底**：开启后，一旦 429 立马使用非 Modelscope 模型重试，避免因限流等待时间过久，推荐打开：
  ```text
  关闭：modelscope 429 → modelscope 429 → modelscope 200
  打开：modelscope 429 → glm 200
  ```
- 🎲 **自动签到与魔粒监控**：每日 00:00 自动调用 `/api/v1/users/login/info` 完成签到领取魔粒，定时拉取 `/openapi/v1/magicubes/balance` 与消费流水，状态页实时展示魔粒余额与今日消耗。
- 📊 **状态页**：管理中心实时展示掩码 key、魔粒余额、今日消耗与可用计数，5s 自动刷新；管理密钥仅服务端使用，不渲染进未鉴权页面。

## 🚀 Getting Started

### 前置依赖

- **Go ≥ 1.26**：`go.mod` 声明 `go 1.26.0`，首次构建需 `GOTOOLCHAIN=auto` 自动拉取工具链。
- **CGO 环境**：`build.sh` 默认 `CGO_ENABLED=1`，需本机 C 编译器（gcc/clang）。
- **交叉编译**：需 [zig](https://ziglang.org/) 0.13+ 作为 C 交叉编译器（Windows、Linux ARM64）。
- **CLIProxyAPI v7.2.51+**：插件经稳定 C ABI 由宿主加载，产物须与宿主进程同架构。
- **国内网络**：`GOPROXY=https://goproxy.cn,direct`（`build.sh` 已内置默认值）。

### 安装方式

#### 方式一：从源码编译安装

```bash
git clone https://github.com/bytehola/modelscope-ratelimit.git
cd modelscope-ratelimit

bash build.sh             # 编译当前平台原生动态库 (.so/.dylib/.dll)
bash build.sh windows      # 交叉编译 Windows/amd64 .dll（需 zig 0.13+）
bash build.sh linux-arm64  # 交叉编译 Linux/arm64 .so（需 zig 0.13+）
bash build.sh all         # 原生 + Windows 均编译
bash build.sh test        # 核心状态机单测（含 -race）
bash build.sh vet         # go vet
```

> ⚠️ 产物文件名必须为 `modelscope-ratelimit.so` / `.dll` / `.dylib`，与配置中的插件 ID `modelscope-ratelimit` 对应，否则宿主无法加载。

将编译产物放入宿主 `plugins/<GOOS>/<GOARCH>/` 目录，在 `config.yaml` 中启用插件（见下方配置）。

#### 方式二：从 GitHub Releases 下载预编译产物安装

前往 [Releases](https://github.com/bytehola/modelscope-ratelimit/releases) 页面，下载对应平台的 `.zip` 包（如 `modelscope-ratelimit-windows-amd64.zip`），解压后得到 `modelscope-ratelimit.dll`（或 `.so` / `.dylib`），放入宿主 `plugins/<GOOS>/<GOARCH>/` 目录，在 `config.yaml` 中启用插件（见下方配置）。

#### 方式三：通过管理中心插件商店安装

在宿主 `config.yaml` 的 `plugins` 段添加 `store-sources`，指向本插件的 `registry.json`：

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/bytehola/modelscope-ratelimit/main/registry.json"
```

内置官方商店始终包含，`store-sources` 是追加自定义源。配置后在管理中心 → 插件商店即可搜索并安装本插件，宿主会自动根据运行平台下载对应架构的产物。

### 配置

将 Modelscope 配置为 `openai-compatibility` 多 key provider，并在 CLIProxyAPI 的 `config.yaml` 中启用插件：

```yaml
openai-compatibility:
  - name: "modelscope"
    disabled: false
    base-url: "https://api-inference.modelscope.cn/v1"
    api-key-entries:
      - api-key: "ms-key-AAAAAAAAAAAA"
      - api-key: "ms-key-BBBBBBBBBBBB"
      - api-key: "ms-key-CCCCCCCCCCCC"
    models:
      - name: "Qwen/Qwen2.5-72B-Instruct"
        alias: "Qwen-72B"
      - name: "Qwen/Qwen2.5-7B-Instruct"
        alias: "Qwen-7B"

plugins:
  enabled: true
  dir: "plugins"
  configs:
    modelscope-ratelimit:
      enabled: true
      priority: 10

      # ---- 必填 ----
      providers: ["modelscope"]
      skip_provider_on_429: false # 推荐打开：429 时使用别的模型兜底
      host_base_url: "http://127.0.0.1:8317"
      management_key: "your-management-secret"

      # ---- 凭据与退避策略 ----
      credential_strategy: "round-robin"  # round-robin | fill-first
      insufficient_quota_cooldown: 0.1

      # proxy_url: "socks5://user:pass@127.0.0.1:1080"
      timezone: "Asia/Shanghai"
      # ms_proxy_url: "http://127.0.0.1:8888"  # 调试用
```

完整字段说明见 `config.example.yaml`。

## 📦 Usage

插件以动态库形式被宿主自动加载，无需独立启动。宿主启动 10 秒后自动拉取全部账号的签到信息与魔粒余额，管理页面即时可用。

管理中心 → **Modelscope 限流** 页面提供以下 API 端点：

```text
?api=accounts         — 全部账号概览（掩码 key、余额、今日消耗、调度状态）
?api=account&key=xxx  — 单账号详情（绑定信息、SDK Token 列表）
?api=transactions&key=xxx — 魔粒消费流水
?api=checkin-all      — 一键全部签到
?api=refresh-balance&key=xxx — 刷新单个账号余额与今日消耗
```

## 📁 Project Structure

```text
modelscope-ratelimit/
├── cmd/plugin/                  # 胶水层：C-ABI 导出、方法路由、配置解析、状态页渲染
│   ├── main.go                  #   插件注册、导出函数、管理 API 路由
│   ├── msclient.go              #   Modelscope HTTP 客户端、账号数据抓取与缓存
│   └── status_page.html         #   管理中心状态页前端（嵌入式 HTML）
├── internal/ratelimit/          # 核心层：纯标准库限流状态机（不引入 SDK / cgo）
│   ├── state.go                 #   限流状态存储、每日重置、魔粒记录、签到记录
│   ├── scheduler.go             #   scheduler.pick 实现：凭据选择与 provider 优先级
│   ├── apply.go                 #   响应处理：429 检测、余额不足判定
│   ├── config.go                #   配置结构与解析
│   ├── proxy.go                 #   代理模式：探测与开关
│   ├── api.go                   #   API 响应结构体与注入闭包类型
│   ├── types.go                 #   插件方法请求/响应载荷镜像类型
│   └── status_html.go           #   状态页 HTML 生成
├── demo/                        # 独立演示程序（模拟宿主调用链路）
├── build.sh                     # 编译脚本（原生 / Windows / Linux ARM64 / 全平台 release）
├── config.example.yaml          # 配置示例
├── registry.json                # 插件商店清单（direct 安装，schema_version 2）
├── go.mod / go.sum              # Go 模块定义
├── LICENSE                      # MIT 协议
└── README.md
```

## 📄 License

本项目采用 MIT 协议开源，版权归 bytehola 所有。
