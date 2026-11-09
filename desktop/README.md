# Friday 桌面端（Tauri v2 外壳 + Go sidecar）

> 设计文档与架构决策见 [docs/DESKTOP.md](../docs/DESKTOP.md)；本手册讲怎么跑、怎么打包、怎么扩展。

## 一句话架构

**大脑/业务全部在 Go 内核，OS 集成在这层薄壳**：Tauri 拉起 `friday --desktop` sidecar 子进程，
解析其 stdout 的 `FRIDAY_ADDR=http://127.0.0.1:<port>` 后导航主窗口；两侧经本地 HTTP 与 stdin 通信。

```
desktop/
├── README.md                  # 本手册
├── ui/                        # 壳本地页（index=启动占位，error=内核退出）
├── scripts/
│   ├── build-release.sh       # ★ 打包脚本：一条命令出安装包
│   ├── verify-macos.sh        # macOS 签名 / DMG / 公证校验（失败即停止）
│   └── gen-icon.py            # 图标底图绘制器（纯 stdlib；icns/ico 由 tauri icon 出）
└── src-tauri/
    ├── Cargo.toml / tauri.conf.json / capabilities/
    ├── src/main.rs            # 拉起内核/解析地址/弹窗/退出接线（无业务）
    └── binaries/  resources/  icons/   # 构建产物（gitignore，脚本生成）
```

## 前置依赖

| 工具 | 用途 |
|------|------|
| Go 1.25+ | 编译内核 sidecar |
| Rust（rustup） | 编译外壳（首次拉取 crates 需联网） |
| Node ≥18 / npx | 运行 Tauri CLI（`npx @tauri-apps/cli`，无 npm 工程） |
| Python3 | 生成占位图标（首次） |

平台注意：macOS 透明弹窗已配 `macOSPrivateApi`；Windows 的 WebView2 安装包会自动引导；
Linux 产物为 deb/AppImage（Wayland 下全局快捷键依赖 compositor 支持）。
Rust 版本：`src-tauri/rust-toolchain.toml` 已钉 `stable`（tauri 2.12 要求 rustc ≥1.90），
进入该目录 rustup 自动切换，不影响全局默认工具链。

## 打包（一条命令）

```bash
desktop/scripts/build-release.sh
```

脚本依次完成：生成图标 → `CGO_ENABLED=0 go build` 出 sidecar（版本经
`-ldflags -X main.version=` 注入）→ 拷贝 `config.yaml.example` 到随包资源 →
`npx tauri build` 出安装包 → macOS 校验内核和完整应用签名、DMG 完整性及内置应用一致性 → 列出产物。任何校验失败都不能发布。常用参数：

```bash
desktop/scripts/build-release.sh --debug            # 调试构建
desktop/scripts/build-release.sh --version v0.2.0   # 指定版本（tag 时用）
desktop/scripts/build-release.sh --sidecar-only     # 只出 sidecar，配合 tauri dev
desktop/scripts/build-release.sh --skip-icon        # icons/ 已放正式资源时
desktop/scripts/build-release.sh --bundles dmg      # 覆盖产物类型
```

产物（macOS）：`desktop/src-tauri/target/release/bundle/dmg/Friday_<ver>_<arch>.dmg`；
Windows 为 NSIS `.exe`；Linux 为 `.deb` / `.AppImage`。

## 更新本机版本（一个包，别在两个产物间犹豫）

外壳与内核在同一个 `Friday.app` 里（一体包，2026-10-28 起内核随包分发、不再往数据目录
`bin/` 播种），所以**本机更新只需要一个东西：替换 `/Applications/Friday.app`**。
`bundle/dmg/*.dmg` 是同一个 app 的压缩分发壳——挂载后里面除 `Friday.app` 只多一个
`Applications` 快捷方式，逐文件比对与 `bundle/macos/Friday.app` 无差异。发给别人、放
Release 才用 dmg；本机直接 `ditto` app 目录更快，也不会带上从网络下载产生的
`com.apple.quarantine`。浏览器下载会触发 Gatekeeper；完整 ad-hoc 签名仅保证代码/资源完整性，不代表通过 Apple 公证，首次打开仍可能需要用户放行。

```bash
# 1) 退出运行中的 Friday（托盘菜单「退出 Friday」；只关窗口是驻留，二进制仍被占着）
pgrep -x friday-desktop && osascript -e 'quit app "Friday"' && sleep 2
# 2) 移走旧包（备份在新目录，避免 ditto 合并保留旧隔离属性或残留文件）
FRIDAY_BACKUP_DIR=$(mktemp -d /tmp/friday-backup.XXXXXX)
mv /Applications/Friday.app "$FRIDAY_BACKUP_DIR/Friday.app"
echo "旧包备份：$FRIDAY_BACKUP_DIR/Friday.app"
# 3) 复制到已腾空的目标路径（ditto 保留权限与符号链接）
ditto desktop/src-tauri/target/release/bundle/macos/Friday.app /Applications/Friday.app
# 4) 启动，然后验版本（应等于打包日志里的"版本：…"）
open /Applications/Friday.app
/Applications/Friday.app/Contents/MacOS/friday --version
```

回滚时先将新 app 移到其他位置，再从上面打印的备份路径用 `ditto` 复制回来。只替换 app 不动数据目录
（`~/Library/Application Support/Friday`），要干净重装才按 [docs/DESKTOP.md](../docs/DESKTOP.md) 的清单删数据。

**版本口径（三处数字长得不一样，别拿错地方判断新旧）**：

| 在哪里看 | 是什么 | 能判断装的是哪次构建吗 |
|---|---|---|
| `Contents/MacOS/friday --version` | Go 内核 `main.version`（打包时 ldflags 注入） | ✅ 唯一准绳 |
| 数据目录 `friday-kernel.log` 里 `[启动] Friday <ver>` | 同上，每次内核启动一行 | ✅ 不必命令行，也能看到"上一次真正跑起来的是哪个版本" |
| dmg 文件名 `Friday_<ver>_<arch>.dmg` | 显式 `--version` 时才是那串；缺省构建是 tauri.conf 的 `0.1.0` | ⚠️ 半可信 |
| `Info.plist` 的 `CFBundleShortVersionString` | tauri.conf 的 version | ❌ 只有显式传 `--version` 才跟着变 |

缺省（不传 `--version`）时内核版本是 `desktop-v*` tag 或 `dev-<git 短哈希>`，**工作区有未提交
改动还会补 `-dirty.<构建时刻>`**（例如 `dev-df1f4c0-dirty.2610291340`）：装着改动的包光标上个
提交的哈希，同一哈希连打两个包就分不出装的是哪个，所以把时刻也带进版本串。要一个能对外的版本
号就显式传 `--version`（同时同步 plist 与 dmg 文件名，三处一致，且不加 dirty 后缀）：

```bash
desktop/scripts/build-release.sh --version v0.1.0-ws.1
```

带连字符的 prerelease 形态（`0.1.0-ws.1`）在 macOS 上 plist 结构与挂载校验都正常；要严格 A.B.C
数字段就用 `0.1.1`。

## 本地开发

```bash
desktop/scripts/build-release.sh --sidecar-only     # 1. 编译 sidecar + 资源
cd desktop/src-tauri && npx tauri dev               # 2. 起外壳（改 Rust/前端热重载）
```

- 数据目录：`~/Library/Application Support/Friday`（Win `%APPDATA%\Friday`、Linux `~/.config/Friday`），
  首次启动先在启动页确认数据目录（默认即该路径），确认后才进入建账号页；删除
  `~/Library/Application Support/Friday.launcher.json` 可重跑这一步。确认后外壳把随包的
  `config.yaml.example` 播种为该目录的 `config.yaml`。
- 新安装会进入首次设置页面创建管理员账号；已有有效账号文件保持兼容，删除账号文件不会恢复默认账号。
- **API 密钥**：推荐直接在软件「设置」页填写（写盘 config.yaml，任意 OpenAI 兼容
  服务商通用）；也可用数据目录的 `friday.env`（KEY=VALUE 行式，如
  `FRIDAY_API_KEY=sk-...`，兼容旧名 DEEPSEEK_API_KEY）。未配置密钥不影响打开软件，
  仅对话不可用，界面会提示补配
- 登录：新安装通过首次设置页面创建账号；已有旧版 `admin/admin` 账号保持兼容并可按页面提示改密。弹窗页与主窗口同源共享会话
- 浏览器直访 `http://127.0.0.1:<port>/palette` 也能用弹窗页（无外壳时 Esc 降级为关闭标签）

## 运行时行为速查

- **全局快捷键**：`Alt+Space` 唤起/隐藏 Spotlight 风格弹窗（改 `src-tauri/src/main.rs`
  顶部 `PALETTE_SHORTCUT` 常量一行即可换键）。弹窗是多轮对话面板：Enter 发送、Shift+Enter
  换行、暂停/继续与主窗口同一套；收起只隐藏不销毁，回到面板还能接着看刚才那段；`⌘L` 清本面板显示
- **防多开**：再次启动唤起已有主窗口（single-instance 插件）
- **驻留与退出**：关闭主窗口会隐藏到托盘，内核继续运行；点击托盘「显示 Friday」恢复窗口，点击「退出 Friday」或系统退出菜单才结束进程。内核先死时会显示恢复提示。
- **退出一致性**（详见 docs/DESKTOP.md 第二节）：退出应用后壳向内核 stdin 写 `/quit`，3s 未退 kill 兜底；壳崩溃/被强杀时 stdin EOF 和 ppid 看门狗负责收尾。
- **内核先死**（如配置错误）：主窗口显示错误页并带出内核的最后一句报错；完整内核
  输出持续写在数据目录的 `friday-kernel.log`，排障先看它
- **首次启动没有 LLM 密钥**：不影响打开软件——登录后在「设置」页直接填密钥即可
  （写盘 config.yaml，通用命名，任意 OpenAI 兼容服务商可用）
- **系统通知**：在通知页点击「开启系统通知」才申请权限；未启用不弹系统提示。失败时仍能在通知中心查阅，浏览器页面关闭后需桌面驻留或外部通道接收提醒。
- **隔离测试**：`FRIDAY_DESKTOP_DATA_DIR` 可指定绝对测试目录，避免影响已有账号、配置和任务；不能用它替代正常系统数据目录。

## 发布 / 签名

- CI 按 docs/DESKTOP.md 第六节：push tag `desktop-v*` 触发四平台矩阵
  （`desktop-release.yml` 待接入；本地脚本与 CI 步骤一一对应）
- **macOS 分发**：正式包必须使用 Developer ID Application 签名并完成公证，具体流程见下文。
- **Windows**：未签名会有 SmartScreen 警告（首期接受）
- **自动更新**：后续接 updater 插件（需签名密钥对），见 docs/DESKTOP.md 第五节路径表

### macOS 正确打包流程（2026-11-09 起）

**不要直接将裸 `cargo build` / 未配置签名的 Tauri 产物作为可直接打开的正式包分发。**
0.4.4 旧包只有链接器写入的 Mach-O 临时签名，缺少整个 `.app` 的资源封装，
`codesign --verify --deep --strict` 报 `code has no resources but signature indicates they must be present`。
DMG 校验通过也不能证明内部应用签名有效。浏览器下载带来的隔离属性让这个问题在安装后暴露。

配置现已显式设置 `bundle.macOS.signingIdentity = "-"`，由 Tauri 在封装 DMG 前
为 sidecar 和完整 `.app` 做 ad-hoc 签名；`APPLE_SIGNING_IDENTITY` 可覆盖此默认值。
签名完成后不能再修改二进制、Info.plist、图标或其他资源；需要修改就重新打包。

#### 本地 / 内部测试包（无需开发者证书）

在仓库根目录运行（生成当前机器架构，Apple Silicon 为 aarch64）：

```bash
# 明确选择 ad-hoc；未配置 Apple 凭据的开发机也可省略环境变量。
APPLE_SIGNING_IDENTITY=- desktop/scripts/build-release.sh --version v0.4.4 --skip-icon
```

产物为 `desktop/src-tauri/target/release/bundle/macos/Friday.app` 和
`desktop/src-tauri/target/release/bundle/dmg/Friday_0.4.4_aarch64.dmg`，
DMG 旁同时生成 `.dmg.sha256`。本机按上文备份旧 app 后替换即可。
SHA-256 用于下载后对照可信发布渠道提供的校验值，不能替代代码签名、公证。

**测试包签名有效不等于 Gatekeeper 放行。** 浏览器下载的 ad-hoc 包仍可能被阻止，
先验证来源与校验值，再在「系统设置 → 隐私与安全性」允许该应用打开。
若可信内部测试包仍提示「已损坏」，可仅对该已校验的应用移除下载隔离属性：

```bash
xattr -dr com.apple.quarantine /Applications/Friday.app
open /Applications/Friday.app
```

这只是测试用户的本机放行操作，不是修复签名，也不能作为正式发布流程。
不要关闭系统全局 Gatekeeper；旧包签名失败时应先换成重打包的版本。

#### 正式分发包（Developer ID + Apple 公证）

1. 在钥匙串导入带私钥的 **Developer ID Application** 证书；用
   `security find-identity -v -p codesigning` 确认身份可用。
2. 从安全凭据管理器加载环境变量，不把密码、私钥或证书写入仓库。
3. 执行正式模式，Tauri 负责签名、向 Apple 提交公证并装订票据，脚本执行最终验收。

```bash
export APPLE_SIGNING_IDENTITY='Developer ID Application: 公司名称 (TEAMID)'
# 方案 A：App Store Connect API key（从安全位置加载真实值）
export APPLE_API_ISSUER='<Issuer ID>'
export APPLE_API_KEY='<Key ID>'
export APPLE_API_KEY_PATH='/安全路径/AuthKey_KEYID.p8'
# 或方案 B：APPLE_ID、APPLE_PASSWORD（App 专用密码）、APPLE_TEAM_ID
# 两种方案选一种；APPLE_PASSWORD 不是 Apple ID 登录密码。

desktop/scripts/build-release.sh --version v0.4.4 --skip-icon --macos-release
```

`--macos-release` 要求显式版本、Developer ID 身份及完整公证凭据；缺失则停止。
本机没有可用证书时只能生成测试包，不能将其标为「已公证」。
CI 额外用 `APPLE_CERTIFICATE`（p12 的 base64）和 `APPLE_CERTIFICATE_PASSWORD`
导入证书，仍应使用同一个脚本和正式模式，不能跳过最终验收。

#### 发布前验收与上传

脚本自动执行以下检查，也可独立重跑（测试包去掉 `--notarized`）：

```bash
desktop/scripts/verify-macos.sh \
  desktop/src-tauri/target/release/bundle/macos/Friday.app \
  desktop/src-tauri/target/release/bundle/dmg/Friday_0.4.4_aarch64.dmg \
  --kernel-version v0.4.4 --notarized
```

检查包含 sidecar 签名、整个 app 的深度严格签名校验、实际执行内核 `--version`、
DMG 校验和、只读挂载 DMG 后再次检查内置应用签名并逐文件比对。
正式模式还要求 `xcrun stapler validate` 和 `spctl --assess --type execute` 通过。
失败时脚本非零退出，不能上传；只上传通过验收的 DMG 及其 `.sha256`。
替换已发布同名文件时同时更新校验值，并在发布说明标明重新打包，避免缓存旧包。
最终还应在一台干净的目标 Mac 上通过浏览器下载、拖入 Applications、首次打开并验版本；
本机直接运行不覆盖带隔离属性的真实下载场景。

官方依据：[Tauri macOS 签名与公证](https://v2.tauri.app/distribute/sign/macos/)。

## 已知边界（首期）

- 桌面版服务强制监听 `127.0.0.1`；远程 Web 部署须显式设置 `--host` 或 `admin.host`
- 弹窗透明在部分 Linux 窗管下可能退化为不透明
- 外壳最终兜底是 kill：内核落盘均为原子写（tmp+rename），最多丢最后一次会话快照
