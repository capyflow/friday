#!/usr/bin/env bash
# 构建 Friday Mobile APK（debug 签名，自用分发）。
# 用法：android/build.sh   （产物 android/friday-mobile.apk）
set -euo pipefail
cd "$(dirname "$0")"

echo "==> [1/4] 环境检测"

# JAVA：Android Studio 自带 JBR → brew openjdk → 报错
if [ -z "${JAVA_HOME:-}" ]; then
  for cand in "/Applications/Android Studio.app/Contents/jbr/Contents/Home" \
              "/opt/homebrew/opt/openjdk/libexec/openjdk.jdk/Contents/Home" \
              "$(/usr/libexec/java_home 2>/dev/null)"; do
    if [ -n "$cand" ] && [ -x "$cand/bin/java" ]; then
      export JAVA_HOME="$cand"
      break
    fi
  done
fi
[ -n "${JAVA_HOME:-}" ] && [ -x "$JAVA_HOME/bin/java" ] || {
  echo "未找到 JDK 17+：装 Android Studio、brew install openjdk，或 export JAVA_HOME"; exit 1; }
echo "JAVA_HOME=$JAVA_HOME"

# ANDROID_HOME：环境变量优先，其次默认安装位
export ANDROID_HOME="${ANDROID_HOME:-$HOME/Library/Android/sdk}"
[ -d "$ANDROID_HOME/platforms" ] || { echo "Android SDK 未找到（ANDROID_HOME=$ANDROID_HOME）"; exit 1; }

# local.properties 指向 SDK（gradle 约定，不入库）
if [ ! -f local.properties ]; then
  echo "sdk.dir=$ANDROID_HOME" > local.properties
fi

echo "==> [2/4] gradle"
GRADLE_BIN="$(command -v gradle || true)"
if [ -z "$GRADLE_BIN" ]; then
  # 没有 brew gradle：下载一次到 /tmp（8.9 与 AGP 8.5 匹配）
  GRADLE_HOME="/tmp/gradle-8.9"
  if [ ! -x "$GRADLE_HOME/bin/gradle" ]; then
    echo "下载 gradle-8.9 到 /tmp（仅首次）…"
    curl -fsSL -o /tmp/gradle-8.9-bin.zip https://services.gradle.org/distributions/gradle-8.9-bin.zip
    unzip -qo /tmp/gradle-8.9-bin.zip -d /tmp
  fi
  GRADLE_BIN="$GRADLE_HOME/bin/gradle"
fi
echo "gradle=$($GRADLE_BIN --version | awk '/Gradle:/{print $2}' | head -1 || echo 8.9)"

echo "==> [3/4] assembleDebug（首次联网拉依赖，需几分钟）"
"$GRADLE_BIN" assembleDebug --no-daemon -q

echo "==> [4/4] 产物"
cp app/build/outputs/apk/debug/app-debug.apk friday-mobile.apk
ls -lh friday-mobile.apk
echo "安装到已连手机：adb install -r android/friday-mobile.apk"
