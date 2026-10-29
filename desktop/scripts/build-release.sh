#!/usr/bin/env bash
#
# Friday 桌面端打包脚本 —— 一条命令产出当前系统的安装包（docs/DESKTOP.md）。
#
# 用法：
#   desktop/scripts/build-release.sh                    # 发布构建：release 安装包
#   desktop/scripts/build-release.sh --debug            # 调试构建（含调试符号，产物在 target/debug）
#   desktop/scripts/build-release.sh --version v0.2.0   # 指定版本（注入 Go 内核 main.version；缺省取 desktop-v* tag 或 git 短哈希）
#   desktop/scripts/build-release.sh --sidecar-only     # 只编译 sidecar 与资源（配合 `npx tauri dev` 本地开发）
#   desktop/scripts/build-release.sh --skip-icon        # 不动 icons/（已放置正式设计资源时）
#   desktop/scripts/build-release.sh --bundles dmg      # 覆盖产物类型（默认按系统：mac=dmg / linux=deb,appimage / win=nsis）
#
# 产物位置（macOS 为例）：
#   desktop/src-tauri/target/release/bundle/dmg/Friday_<ver>_<arch>.dmg   # 分发/归档用
#   desktop/src-tauri/target/release/bundle/macos/Friday.app              # 本机更新就换这个
#   （外壳与内核在同一份 .app 里，本机更新不需要 dmg；见 desktop/README.md「更新本机版本」）
#
# 前置依赖：Go 1.25+、Rust（rustup）、Node/npx（跑 Tauri CLI）、Python3（占位图标）。
# Windows 需在 Git Bash / MSYS 下运行，且已装 MSVC 工具链与 WebView2。
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
TAURI_DIR="$REPO_ROOT/desktop/src-tauri"

DEBUG=0 SIDECAR_ONLY=0 SKIP_ICON=0 FORCE_ICON=0 VERSION="" VERSION_SET=0 BUNDLES=""
usage() { grep '^#   ' "$0" | sed 's/^#   //'; exit 0; }
while [ $# -gt 0 ]; do
  case "$1" in
    --debug) DEBUG=1 ;;
    --sidecar-only) SIDECAR_ONLY=1 ;;
    --skip-icon) SKIP_ICON=1 ;;
    --force-icon) FORCE_ICON=1 ;;
    --version) VERSION="${2:?--version 需要参数，如 v0.2.0}"; VERSION_SET=1; shift ;;
    --bundles) BUNDLES="${2:?--bundles 需要参数，如 dmg 或 deb,appimage}"; shift ;;
    -h|--help|help) usage ;;
    *) echo "未知参数: $1（--help 查看用法）" >&2; exit 2 ;;
  esac
  shift
done

step() { printf '\n\033[1;35m==> %s\033[0m\n' "$*"; }
die() { echo "错误: $*" >&2; exit 1; }
trap 'echo "" >&2; echo "打包失败（见上方日志）。首次构建需联网拉取 Rust crates 与 Go 模块。" >&2' ERR

# ---- 0. 前置检查 ----
step "前置检查"
command -v go >/dev/null || die "未安装 Go（https://go.dev/dl/）"
command -v cargo >/dev/null || die "未安装 Rust（https://rustup.rs）"
command -v npx >/dev/null || die "未安装 Node/npx（Tauri CLI 经 npx 运行）"
# Windows 的 Python 官方安装包只提供 python.exe（python3 常是商店假别名——
# 能通过 command -v 但一运行就报错跳转商店），故需实测可执行性后再回退 python。
if command -v python3 >/dev/null 2>&1 && python3 -c "" 2>/dev/null; then PYTHON=python3
elif command -v python >/dev/null 2>&1 && python -c "" 2>/dev/null; then PYTHON=python
else die "未安装 Python3（生成占位图标用）"; fi
TAURI_CLI=(npx -y @tauri-apps/cli@^2)
echo "go $(go version | awk '{print $3}') / rust $(rustc --version | awk '{print $2}') / tauri-cli ${TAURI_CLI[*]}"

# ---- 1. 版本号 ----
# 缺省取 desktop-v* tag 或 git 短哈希；工作区有未提交改动时补 -dirty——否则装着
# 改动的包标着上一个提交的哈希，事后无法区分（本机迭代最常踩的歧义）。
# 显式 --version 时不加后缀（那串就是要发出去的版本），并把同一串同步给 tauri，
# 让内核版本、Info.plist 与 dmg 文件名三处一致。
if [ -z "$VERSION" ]; then
  VERSION="$(git -C "$REPO_ROOT" describe --tags --match 'desktop-v*' --abbrev=7 2>/dev/null \
    || echo "dev-$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo none)")"
  # 只看已跟踪文件的差异：未跟踪的用户数据（acp-threads.json 等）不该算"脏"。
  # 后缀带构建时刻：同一 HEAD 上连打两个脏包，光看 -dirty 分不出装的是哪个。
  if [ -n "$(git -C "$REPO_ROOT" status --porcelain --untracked-files=no 2>/dev/null)" ]; then
    VERSION="$VERSION-dirty.$(date +%y%m%d%H%M)"
  fi
fi
step "版本：${VERSION}（注入 Go 内核 main.version）"

# ---- 2. 目标三元组（Tauri externalBin 命名规则 binaries/friday-<triple>）----
TRIPLE="$(rustc -vV | awk '/^host/ {print $2}')"
EXE=""
case "$TRIPLE" in *windows*) EXE=".exe" ;; esac
step "目标：$TRIPLE"

# ---- 3. 图标（正式设计资源就位后可 --skip-icon）----
if [ "$SKIP_ICON" = 0 ] && { [ "$FORCE_ICON" = 1 ] || [ ! -f "$TAURI_DIR/icons/icon.icns" ] || [ ! -f "$TAURI_DIR/icons/icon.ico" ]; }; then
  step "绘制图标底图（gen-icon.py，纯 stdlib）"
  "$PYTHON" "$SCRIPT_DIR/gen-icon.py" --out "$TAURI_DIR/icons"
  # icns/ico 容器必须用官方工具生成：手写结构 sips 能读但系统图标服务不认，
  # 表现为安装后的 app "没有图标"。
  step "生成规范图标全套（tauri icon）"
  ( cd "$TAURI_DIR" && "${TAURI_CLI[@]}" icon "$TAURI_DIR/icons/app-icon.png" --output "$TAURI_DIR/icons" )
elif [ "$SKIP_ICON" = 1 ]; then
  step "跳过图标生成"
fi
[ -f "$TAURI_DIR/icons/icon.icns" ] || [ -f "$TAURI_DIR/icons/icon.ico" ] \
  || die "$TAURI_DIR/icons 缺少图标文件"

# ---- 4. Go 内核 sidecar：CGO_ENABLED=0 纯 Go 静态编译（约 16MB）----
# 前端先于 Go 编译：web/（Vue3+Vite）构建产物内联为自包含 HTML，
# 落在 internal/admin/webdist/，经 go:embed 打进 sidecar 二进制。
step "构建网页前端（web → internal/admin/webdist）"
( cd "$REPO_ROOT/web"
  [ -d node_modules ] || npm install --no-audit --no-fund
  npm run build )
step "编译 Go 内核 sidecar"
BIN_NAME="friday-$TRIPLE$EXE"
mkdir -p "$TAURI_DIR/binaries"
# 私有模块走 SSH 时需要：与 Docker 构建同一把密钥（见 docs/DESKTOP.md 第六节）。
# 如 go build 报 module 认证错误，先 `ssh-add` 对应私钥并设置 GOPRIVATE=github.com/capyflow/*。
( cd "$REPO_ROOT" && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o "$TAURI_DIR/binaries/$BIN_NAME" ./cmd/robot )
ls -lh "$TAURI_DIR/binaries/$BIN_NAME" | awk '{print "  " $5 "  " $9}'

# ---- 5. 随包资源：首启播种用 config.yaml.example ----
mkdir -p "$TAURI_DIR/resources"
cp "$REPO_ROOT/config.yaml.example" "$TAURI_DIR/resources/config.yaml.example"
echo "资源已就位：resources/config.yaml.example"

if [ "$SIDECAR_ONLY" = 1 ]; then
  step "sidecar 就绪（--sidecar-only）：$TAURI_DIR/binaries/$BIN_NAME"
  echo "本地开发：cd $TAURI_DIR && ${TAURI_CLI[*]} dev"
  exit 0
fi

# ---- 6. Tauri 打包（首次编译 Rust 依赖需数分钟）----
OS="$(uname -s)"
if [ -z "$BUNDLES" ]; then
  case "$OS" in
    Darwin) BUNDLES="app,dmg" ;;
    Linux) BUNDLES="deb,appimage" ;;
    MINGW* | MSYS* | CYGWIN*) BUNDLES="nsis" ;;
    *) die "未知系统 ${OS}，请用 --bundles 指定产物类型" ;;
  esac
fi
step "Tauri 打包（--bundles $BUNDLES$([ "$DEBUG" = 1 ] && echo ' --debug')）"
BUILD_ARGS=(build --bundles "$BUNDLES")
[ "$DEBUG" = 1 ] && BUILD_ARGS+=(--debug)
# --version 显式指定时同步 tauri.conf 的版本（去掉 v 前缀满足 semver）。
if [ "$VERSION_SET" = 1 ]; then
  BUILD_ARGS+=("--config" "{\"version\":\"${VERSION#v}\"}")
fi
( cd "$TAURI_DIR" && "${TAURI_CLI[@]}" "${BUILD_ARGS[@]}" )

# ---- 7. 产物清单 ----
BUNDLE_ROOT="$TAURI_DIR/target/$([ "$DEBUG" = 1 ] && echo debug || echo release)/bundle"
step "打包完成，产物："
find "$BUNDLE_ROOT" -type f \( -name '*.dmg' -o -name '*.app' -o -name '*.exe' -o -name '*.deb' -o -name '*.AppImage' -o -name '*.rpm' \) \
  -exec ls -lh {} \; | awk '{print "  " $5 "  " $9}'
[ -d "$BUNDLE_ROOT" ] || die "未找到产物目录 $BUNDLE_ROOT"
if [ "$OS" = "Darwin" ]; then
  # 本机更新只认一个东西：外壳与内核在同一个 .app 里（一体包），dmg 只是它的压缩外壳。
  # 每次把这段贴出来，避免"装哪个包/怎么确认装上了"再问一遍。
  APP="$BUNDLE_ROOT/macos/Friday.app"
  DMG_FOUND="$(find "$BUNDLE_ROOT/dmg" -maxdepth 1 -name '*.dmg' -print -quit 2>/dev/null || true)"
  step "本机更新（一个包：替换 .app 即可，数据目录不动）"
  echo "  1. 退出运行中的 Friday（托盘菜单「退出 Friday」，不是关窗口）"
  echo "  2. ditto '$APP' /Applications/Friday.app"
  echo "  3. 验版本：'/Applications/Friday.app/Contents/MacOS/friday' --version   # 应输出 ${VERSION}"
  echo "     或看数据目录日志的新行：grep '\\[启动\\] Friday' ~/Library/Application\\ Support/Friday/friday-kernel.log | tail -1"
  echo "     （Info.plist 的版本串只在显式 --version 时才跟着变，别拿它判断新旧）"
  if [ -n "$DMG_FOUND" ]; then
    echo "  分发/归档用 dmg：$DMG_FOUND（内容与上面的 .app 逐文件相同，只多一个 Applications 快捷方式）"
  else
    echo "  本次只出了 .app（--bundles app）：分发时要 dmg 就补一句 --bundles dmg"
  fi
fi
