#!/usr/bin/env bash
#
# modelscope-ratelimit 编译脚本
#
# 用法:
#   ./build.sh              # 编译当前平台原生动态库 (Linux->.so / macOS->.dylib)
#   ./build.sh windows      # 交叉编译 Windows/amd64 .dll (需 zig 0.13+)
#   ./build.sh linux-arm64  # 交叉编译 Linux/arm64 .so (需 zig 0.13+)
#   ./build.sh darwin-amd64 # 交叉编译 macOS/amd64 .dylib (需 zig 0.13+)
#   ./build.sh darwin-arm64 # 交叉编译 macOS/arm64 .dylib (需 zig 0.13+)
#   ./build.sh all          # 原生 + Windows 都编译
#   ./build.sh release      # 全部 5 平台编译 + 打包 .zip + 计算 SHA256
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
RELEASE_DIR="$SCRIPT_DIR/release"

log() { printf '>> %s\n' "$*"; }

# mkzip <zip_path> <file_to_add> — 用 Python zipfile 打包 (兼容无 zip 命令的环境)
mkzip() {
  python3 -c "
import zipfile, sys
z, f = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(z, 'w', zipfile.ZIP_DEFLATED) as zf:
    zf.write(f, arcname=__import__('os').path.basename(f))
" "$1" "$2"
}

update_build_time() {
  local html="$SCRIPT_DIR/cmd/plugin/status_page.html"
  local now=$(TZ=Asia/Shanghai date "+%Y-%m-%d %H:%M:%S")
  if [ -f "$html" ]; then
    sed -i "s|构建于.*CST|构建于 ${now} CST|g" "$html"
    log "构建时间已更新: ${now} CST"
  fi
}

build_native() {
  local ext out
  case "$(go env GOOS)" in
    linux)   ext="so"    ;;
    windows) ext="dll"   ;;
    darwin)  ext="dylib" ;;
    *)       ext="so"    ;;
  esac
  out="${BUILD_OUT:-$OUT_DIR/$NAME.$ext}"
  log "编译原生动态库 ($(go env GOOS)/$(go env GOARCH)) -> $out"
  update_build_time
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
  local zig out="${BUILD_OUT:-$OUT_DIR/$NAME.dll}"
  zig="$(resolve_zig)" || {
    printf '错误: 未找到 zig C 交叉编译器。请安装 zig (0.13+), 或设置 ZIG=/path/to/zig\n' >&2
    printf '       查找顺序: $ZIG -> PATH(zig) -> /opt/zig/zig -> /tmp/zig/zig\n' >&2
    exit 1
  }
  log "交叉编译 Windows/amd64 (zig=$zig) -> $out"
  update_build_time
  GOOS=windows GOARCH=amd64 \
    CC="$zig cc -target x86_64-windows-gnu" \
    go build -buildmode=c-shared -trimpath -o "$out" "$PKG"
  log "完成: $out"
}

# 通用 zig 交叉编译: 参数 GOOS, GOARCH, zig-target, ext
build_cross() {
  local goos="$1" goarch="$2" zig_target="$3" ext="$4"
  local zig out="${BUILD_OUT:-$OUT_DIR/$NAME.$ext}"
  zig="$(resolve_zig)" || {
    printf '错误: 未找到 zig C 交叉编译器。请安装 zig (0.13+), 或设置 ZIG=/path/to/zig\n' >&2
    exit 1
  }
  log "交叉编译 $goos/$goarch (zig=$zig) -> $out"
  update_build_time
  GOOS="$goos" GOARCH="$goarch" \
    CC="$zig cc -target $zig_target" \
    go build -buildmode=c-shared -trimpath -o "$out" "$PKG" || return 1
  log "完成: $out"
}

build_linux_arm64()  { build_cross linux arm64 aarch64-linux-gnu so; }
build_darwin_amd64() { build_cross darwin amd64 x86_64-macos dylib; }
build_darwin_arm64() { build_cross darwin arm64 aarch64-macos dylib; }

# build_and_package: 编译单个平台并立即打包 (避免 .so 产物互相覆盖)
build_and_package() {
  local goos="$1" goarch="$2" zig_target="$3" ext="$4"
  local lib_name="$NAME.$ext"
  local zip_name="$NAME-$goos-$goarch.zip"
  local out="$OUT_DIR/$lib_name"
  # 编译到固定名, 打包后删除, 避免文件名冲突
  # 失败时返回非零, 调用方用 || 容错跳过
  BUILD_OUT="$out" \
    build_cross "$goos" "$goarch" "$zig_target" "$ext" || return 1
  (cd "$OUT_DIR" && mkzip "$RELEASE_DIR/$zip_name" "$lib_name")
  rm -f "$out"
  printf '  %s  sha256: %s\n' "$zip_name" "$(sha256sum "$RELEASE_DIR/$zip_name" | awk '{print $1}')"
}


run_vet()  { log "go vet ./..."; go vet ./...; }
run_test() { log "go test -race ./internal/ratelimit/"; go test -race -timeout 120s ./internal/ratelimit/; }

target="${1:-native}"
case "$target" in
  native|"") build_native ;;
  windows|win) build_windows ;;
  linux-arm64) build_linux_arm64 ;;
  darwin-amd64) build_darwin_amd64 ;;
  darwin-arm64) build_darwin_arm64 ;;
  all) build_native; build_windows ;;
  release)
    rm -rf "$RELEASE_DIR"
    mkdir -p "$RELEASE_DIR"
    update_build_time
    echo ""
    log "========== 编译 + 打包 release =========="
    # linux/amd64 (原生编译)
    BUILD_OUT="$OUT_DIR/$NAME.so" build_native
    (cd "$OUT_DIR" && mkzip "$RELEASE_DIR/$NAME-linux-amd64.zip" "$NAME.so")
    rm -f "$OUT_DIR/$NAME.so"
    printf '  %s  sha256: %s\n' "$NAME-linux-amd64.zip" "$(sha256sum "$RELEASE_DIR/$NAME-linux-amd64.zip" | awk '{print $1}')"
    # 交叉编译平台
    build_and_package windows  amd64 x86_64-windows-gnu dll || log "跳过 windows/amd64"
    build_and_package linux    arm64 aarch64-linux-gnu  so  || log "跳过 linux/arm64"
    build_and_package darwin   amd64 x86_64-macos       dylib || log "跳过 darwin/amd64 (需 macOS SDK)"
    build_and_package darwin   arm64 aarch64-macos      dylib || log "跳过 darwin/arm64 (需 macOS SDK)"
    echo ""
    log "所有 release 产物: $RELEASE_DIR/"
    ls -la "$RELEASE_DIR/"
    ;;
  vet)  run_vet ;;
  test) run_test ;;
  *) printf '用法: %s [native|windows|linux-arm64|darwin-amd64|darwin-arm64|all|release|vet|test]\n' "$0" >&2; exit 2 ;;
esac
