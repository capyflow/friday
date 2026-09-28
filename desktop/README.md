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
`npx tauri build` 出安装包 → 列出产物。常用参数：

```bash
desktop/scripts/build-release.sh --debug            # 调试构建
desktop/scripts/build-release.sh --version v0.2.0   # 指定版本（tag 时用）
desktop/scripts/build-release.sh --sidecar-only     # 只出 sidecar，配合 tauri dev
desktop/scripts/build-release.sh --skip-icon        # icons/ 已放正式资源时
desktop/scripts/build-release.sh --bundles dmg      # 覆盖产物类型
```

产物（macOS）：`desktop/src-tauri/target/release/bundle/dmg/Friday_<ver>_<arch>.dmg`；
Windows 为 NSIS `.exe`；Linux 为 `.deb` / `.AppImage`。

## 本地开发

```bash
desktop/scripts/build-release.sh --sidecar-only     # 1. 编译 sidecar + 资源
cd desktop/src-tauri && npx tauri dev               # 2. 起外壳（改 Rust/前端热重载）
```

- 数据目录：`~/Library/Application Support/Friday`（Win `%APPDATA%\Friday`、Linux `~/.config/Friday`），
  首次启动外壳会把随包的 `config.yaml.example` 播种为该目录的 `config.yaml`
- **API 密钥**：推荐直接在软件「设置」页填写（写盘 config.yaml，任意 OpenAI 兼容
  服务商通用）；也可用数据目录的 `friday.env`（KEY=VALUE 行式，如
  `FRIDAY_API_KEY=sk-...`，兼容旧名 DEEPSEEK_API_KEY）。未配置密钥不影响打开软件，
  仅对话不可用，界面会提示补配
- 登录：默认 `admin/admin`（首次登录强制改密）；弹窗页与主窗口同源共享会话
- 浏览器直访 `http://127.0.0.1:<port>/palette` 也能用弹窗页（无外壳时 Esc 降级为关闭标签）

## 运行时行为速查

- **全局快捷键**：`Alt+Space` 唤起/隐藏 Spotlight 风格弹窗（改 `src-tauri/src/main.rs`
  顶部 `PALETTE_SHORTCUT` 常量一行即可换键）
- **防多开**：再次启动唤起已有主窗口（single-instance 插件）
- **退出一致性**（三层保证，详见 docs/DESKTOP.md 第二节）：
  1. 关窗 → 壳向内核 stdin 写 `/quit`，3s 未退 kill 兜底
  2. 壳崩溃/被强杀 → 内核读到 stdin EOF 自动优雅退出（零代码，OS 管道行为）
  3. 内核 ppid=1 看门狗（30s）兜底孤儿进程（Windows 无进程过继，恒不触发）
- **内核先死**（如配置错误）：主窗口显示错误页并带出内核的最后一句报错；完整内核
  输出持续写在数据目录的 `friday-kernel.log`，排障先看它
- **首次启动没有 LLM 密钥**：不影响打开软件——登录后在「设置」页直接填密钥即可
  （写盘 config.yaml，通用命名，任意 OpenAI 兼容服务商可用）

## 发布 / 签名

- CI 按 docs/DESKTOP.md 第六节：push tag `desktop-v*` 触发四平台矩阵
  （`desktop-release.yml` 待接入；本地脚本与 CI 步骤一一对应）
- **macOS 分发**：需 Apple Developer（$99/年）签名+公证，否则首次打开要
  右键 → 打开 绕过 Gatekeeper；签名 secrets 预留位见 workflow 注释
- **Windows**：未签名会有 SmartScreen 警告（首期接受）
- **自动更新**：后续接 updater 插件（需签名密钥对），见 docs/DESKTOP.md 第五节路径表

## 已知边界（首期）

- 内核 vortexv3 绑定 `0.0.0.0`：桌面版服务对局域网可见（后续给 vortex 加 host 选项或依赖系统防火墙）
- 弹窗透明在部分 Linux 窗管下可能退化为不透明
- 外壳最终兜底是 kill：内核落盘均为原子写（tmp+rename），最多丢最后一次会话快照
