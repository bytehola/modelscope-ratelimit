#!/usr/bin/env bash
#
# modelscope-ratelimit 编译脚本
#
# 用法:
#   ./build.sh              # 编译当前平台原生动态库 (Linux->.so / macOS->.dylib / Windows->.dll)
#   ./build.sh windows      # 交叉编译 Windows/amd64 .dll (需 zig 作为 C 交叉编译器)
#   ./build.sh all          # 原生 + Windows 都编译
#   ./build.sh vet          # 仅 go vet ./...
#   ./build.sh test         # 仅核心状态机单测 (含 -race)
#
# 可在调用前覆盖的环境变量:
#   GOPROXY / GOCACHE / GOPATH / GOFLAGS / CGO_ENABLED / GOTOOLCHAIN / ZIG
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# ---- Go 工具链环境 (本机 go 不在默认 PATH 时回退 /tmp/go/bin) ----
if ! command -v go >/dev/null 2>&1; then
  export PATH="/tmp/go/bin:${PATH}"
fi
: "${GOCACHE:=/tmp/gocache}"
: "${GOPATH:=/tmp/gopath}"
: "${GOPROXY:=https://goproxy.cn,direct}"   # 国内 proxy.golang.org 被墙, 用 goproxy.cn
: "${GOFLAGS:=-mod=mod}"
: "${CGO_ENABLED:=1}"
: "${GOTOOLCHAIN:=auto}"                     # go.mod 要求 go 1.26, 首次构建自动下载 toolchain
export PATH GOCACHE GOPATH GOPROXY GOFLAGS CGO_ENABLED GOTOOLCHAIN

PKG="./cmd/plugin"
OUT_DIR="$SCRIPT_DIR/cmd/plugin"
NAME="modelscope-ratelimit"

log() { printf '>> %s\n' "$*"; }

build_native() {
  local ext
  case "$(go env GOOS)" in
    linux)   ext="so"    ;;
    windows) ext="dll"   ;;
    darwin)  ext="dylib" ;;
    *)       ext="so"    ;;
  esac
  local out="$OUT_DIR/$NAME.$ext"
  log "编译原生动态库 ($(go env GOOS)/$(go env GOARCH)) -> $out"
  go build -buildmode=c-shared -trimpath -o "$out" "$PKG"
  log "完成: $out"
}

# 解析 zig C 交叉编译器路径: $ZIG -> PATH(command -v zig) -> /opt/zig/zig -> /tmp/zig/zig
resolve_zig() {
  local cand
  for cand in "${ZIG:-}" "$(command -v zig 2>/dev/null)" /opt/zig/zig /tmp/zig/zig; do
    [ -n "$cand" ] && [ -x "$cand" ] && { printf '%s' "$cand"; return 0; }
  done
  return 1
}

build_windows() {
  local zig out="$OUT_DIR/$NAME.dll"
  zig="$(resolve_zig)" || {
    printf '错误: 未找到 zig C 交叉编译器。请安装 zig (0.13+), 或设置 ZIG=/path/to/zig\n' >&2
    printf '       查找顺序: $ZIG -> PATH(zig) -> /opt/zig/zig -> /tmp/zig/zig\n' >&2
    exit 1
  }
  log "交叉编译 Windows/amd64 (zig=$zig) -> $out"
  GOOS=windows GOARCH=amd64 \
    CC="$zig cc -target x86_64-windows-gnu" \
    go build -buildmode=c-shared -trimpath -o "$out" "$PKG"
  log "完成: $out"
}

run_vet()  { log "go vet ./..."; go vet ./...; }
run_test() { log "go test -race ./internal/ratelimit/"; go test -race -timeout 120s ./internal/ratelimit/; }

target="${1:-native}"
case "$target" in
  native|"") build_native ;;
  windows|win) build_windows ;;
  all) build_native; build_windows ;;
  vet)  run_vet ;;
  test) run_test ;;
  *) printf '用法: %s [native|windows|all|vet|test]\n' "$0" >&2; exit 2 ;;
esac
