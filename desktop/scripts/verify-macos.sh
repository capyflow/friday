#!/usr/bin/env bash
# 用法：verify-macos.sh /path/Friday.app [/path/Friday.dmg] [--kernel-version v0.4.4] [--notarized]
# 校验失败即非零退出；不修改签名或隔离属性，也不启动用户数据目录中的应用。
set -euo pipefail

APP="${1:?需要 Friday.app 路径}"
shift
DMG="" NOTARIZED=0 MOUNT_DIR="" MOUNTED=0 EXPECTED_VERSION=""
while [ $# -gt 0 ]; do
  case "$1" in
    --notarized) NOTARIZED=1 ;;
    --kernel-version) EXPECTED_VERSION="${2:?--kernel-version 需要参数}"; shift ;;
    *) [ -z "$DMG" ] || { echo "多余参数：$1" >&2; exit 2; }; DMG="$1" ;;
  esac
  shift
done

cleanup() {
  if [ "$MOUNTED" = 1 ]; then
    hdiutil detach "$MOUNT_DIR" -quiet || return 1
  fi
  [ -z "$MOUNT_DIR" ] || rmdir "$MOUNT_DIR"
}
trap 'verify_status=$?; cleanup || verify_status=1; exit "$verify_status"' EXIT

verify_app() {
  local app="$1"
  codesign --verify --strict --verbose=2 "$app/Contents/MacOS/friday"
  codesign --verify --deep --strict --verbose=2 "$app"
  if [ "$NOTARIZED" = 1 ]; then
    xcrun stapler validate "$app"
    spctl --assess --type execute --verbose=2 "$app"
  fi
}

verify_app "$APP"
VERSION=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$APP/Contents/Info.plist")
KERNEL_VERSION=$("$APP/Contents/MacOS/friday" --version)
if [ -n "$EXPECTED_VERSION" ] && [ "$KERNEL_VERSION" != "$EXPECTED_VERSION" ]; then
  echo "内核版本不符合预期：$KERNEL_VERSION / $EXPECTED_VERSION" >&2
  exit 1
fi
echo "应用签名校验通过：${APP}（应用版本：${VERSION}；内核：${KERNEL_VERSION}）"

if [ -n "$DMG" ]; then
  hdiutil verify "$DMG"
  MOUNT_DIR=$(mktemp -d "${TMPDIR:-/tmp}/friday-verify.XXXXXX")
  hdiutil attach "$DMG" -readonly -nobrowse -mountpoint "$MOUNT_DIR" -quiet
  MOUNTED=1
  verify_app "$MOUNT_DIR/Friday.app"
  # 包括资源、plist 与签名封装，防止签名后又修改 app 或封入旧包。
  diff -qr "$APP" "$MOUNT_DIR/Friday.app"
  hdiutil detach "$MOUNT_DIR" -quiet
  MOUNTED=0
  rmdir "$MOUNT_DIR"
  MOUNT_DIR=""
  ( cd "$(dirname "$DMG")" && shasum -a 256 "$(basename "$DMG")" > "$(basename "$DMG").sha256" )
  echo "DMG 完整性、内置应用签名及内容一致性校验通过：$DMG"
fi
