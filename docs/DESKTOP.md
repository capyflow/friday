# Friday 桌面端方案（Tauri v2 + Go sidecar）

> 状态：已评审通过，外壳工程与打包脚本已落地（2026-09-28）；CI workflow（第六节）待接入
> 关联：[desktop/README.md](../desktop/README.md)（操作手册）、[ROADMAP.md](ROADMAP.md)

## 一、总体架构

现状已具备条件：单 Go 二进制 + `go:embed` 内嵌 UI + 本地 HTTP 服务，语音 cgo 链路已归档（`CGO_ENABLED=0` 纯 Go 交叉编译已实测通过，单平台约 16MB 静态二进制）。

桌面形态 = **Tauri v2 外壳（Rust，系统 WebView）+ Go 内核 sidecar 子进程**。分工原则：**大脑/业务留在 Go，OS 集成（窗口、快捷键、托盘、自启、自动更新）在壳**。

启动链路：

1. Tauri 启动 → 拉起 sidecar：`friday --desktop --data-dir <系统应用数据目录>/Friday --port 0`
2. 首次运行：外壳把随包分发的 `config.yaml.example` 播种为数据目录的 `config.yaml`（对齐 docker entrypoint 模式）；API 密钥经数据目录可选的 `friday.env`（KEY=VALUE）注入子进程环境——解决 Finder/Dock 启动拿不到 shell 环境变量的问题
3. Go 内核 chdir 到数据目录 → 随机空闲端口起服务 → stdout 打印 `FRIDAY_ADDR=http://127.0.0.1:<port>`
4. 外壳逐行解析 stdout，拿到地址后通知主窗口导航（此前显示启动占位页）
5. 退出链路见「生命周期一致性」

## 二、生命周期一致性（三层保证）

sidecar 机制本身不提供父子进程生命周期绑定（Tauri 不自动杀子进程），需显式接线：

1. **正常退出（确定性）**：主窗口关闭 → Tauri `RunEvent::Exit` → 外壳向子进程 stdin 写一行 `/quit`，3 秒未退则 `kill()` 兜底。Go 侧桌面模式监听 stdin：`/quit` 或 EOF 均触发现有 `stop()` 优雅退出链（任务引擎/会话/MCP defer Close 全部执行）
2. **壳崩溃/被强杀（OS 级，零代码）**：子进程 stdin 是管道，写端握在外壳手里；进程死亡 → OS 关闭句柄 → Go 读到 EOF → 优雅退出。经典 sidecar 生命周期模式（VS Code language server 同款），跨平台通用
3. **兜底看门狗（加固）**：Go 内核每 30s 检查 `os.Getppid()==1`（Unix 孤儿进程被过继给 launchd/init）即优雅退出，防 stdin 监听本身失效的极端情况；Windows 无进程过继机制该检查恒不触发（退出靠 stdin EOF），Windows 终极手段 Job Object 首期不引入

反向一致：内核先死（如配置错误）→ 外壳收到 sidecar `Terminated` 事件 → 窗口显示错误页（后续可加自动重启策略）。

已知边界：外壳最终兜底是 kill，Go 侧落盘均为原子写（tmp+rename），最多丢最后一次会话快照。

## 三、Go 侧改造（改动很小）

`cmd/robot/main.go`：

- `--desktop` / `--data-dir` / `--port`（0=随机空闲端口）/ `--version` 参数
- 桌面模式 chdir 数据目录（默认 `os.UserConfigDir()/Friday`：mac `~/Library/Application Support/Friday`、Win `%APPDATA%\Friday`、Linux `~/.config/Friday`），全部相对路径数据文件自动落位
- 桌面模式无终端 REPL；stdin 循环：读到 `/quit` 或 EOF → 优雅退出；ppid 看门狗兜底
- 服务就绪（轮询 `GET /` 200）后 stdout 打印 `FRIDAY_ADDR=` 标记；桌面模式强制 admin 开启
- `version` 变量供 `-ldflags -X main.version=` 注入

`internal/admin`：新增 `GET /palette` 弹窗页（`go:embed` + 公有静态路由，模式同 `handleIndex`）——Spotlight 风格输入框，同源调 `/api/chat`（SSE 流式渲染），未登录提示先去主窗口登录。

已知边界（首期不修）：vortexv3 绑定 `0.0.0.0`，桌面版服务对局域网可见（后续给 vortex 加 host 选项或依赖系统防火墙）。

## 四、桌面外壳工程 `desktop/`

```
desktop/
├── README.md                  # 本地开发 / 发布 / 扩展文档
├── ui/index.html              # 启动占位页（监听 server-ready → 跳转内核地址）
├── scripts/gen-icon.py        # 占位图标生成器（纯 stdlib 画 PNG，无需设计资源）
└── src-tauri/
    ├── Cargo.toml             # tauri 2 + shell / global-shortcut / single-instance 插件
    ├── tauri.conf.json        # externalBin(sidecar) + resources(config.yaml.example)
    ├── capabilities/default.json
    ├── src/main.rs            # 拉起内核/解析地址/弹窗/退出接线（约 150 行，无业务）
    ├── binaries/  resources/  icons/   # CI/本地构建产物（gitignore）
```

`main.rs` 职责：

- `tauri-plugin-single-instance` 防多开（再次启动 → 唤起已有主窗口）
- sidecar 拉起内核，后台线程解析 `FRIDAY_ADDR=` → emit 事件；监听 `Terminated` → 错误页
- 退出接线：`RunEvent::Exit` → stdin 写 `/quit` → 3s 后 kill 兜底
- `tauri-plugin-global-shortcut` 注册全局快捷键（默认 **Alt+Space**，常量一行可改）→ 创建/切换弹窗：无边框+透明+置顶+不进任务栏，加载内核 `/palette`；失焦/Esc 隐藏（仅隐藏不销毁）
- 零 JS 构建链：无 npm 工程，`withGlobalTauri` 用全局 API

弹窗页放 **Go 侧**是刻意设计：同源共享登录会话（无 CORS/鉴权烦恼）、浏览器访问 `:port/palette` 也能用、壳保持极薄。

## 五、后续按系统扩展不同功能的路径

分层原则：**OS 集成 = 壳（Tauri 插件/Rust），业务能力 = 内核（Go agent 工具）**，两侧经本地 HTTP / stdin 通信。

| 需求 | 实现方式 | 难度 |
|------|----------|------|
| 全局快捷键/弹窗 | 本方案第一个样例 | ✅ |
| 系统托盘菜单 | tauri `tray-icon` feature | 低 |
| 系统通知 | notification 插件（或沿用钉钉/飞书渠道） | 低 |
| 开机自启 | autostart 插件 | 低 |
| 自动更新 | updater 插件（需签名密钥对） | 中 |
| 剪贴板/文件拖拽 | clipboard-manager 插件 / 拖放事件 → 内核文件工具 | 低 |
| 深度原生 API（菜单栏定制、媒体控制等） | 壳层 Rust 直调系统 API；或封装为 Go 侧新 Tool 给 agent | 按需 |

平台差异：

- **macOS**：透明窗需 `macOSPrivateApi`（已配）；分发需 Apple Developer（$99/年）签名+公证，否则首次打开要右键绕过 Gatekeeper
- **Windows**：WebView2 自动引导安装；SmartScreen 警告需代码签名证书（首期未签名）
- **Linux**：X11 全局快捷键正常，Wayland 依赖 compositor 协议支持；产物 deb/AppImage

## 六、CI/发布：`.github/workflows/desktop-release.yml`

- 触发：push tag `desktop-v*` + `workflow_dispatch`（手动出测试产物）
- 矩阵：macos-14（arm64）/ macos-13（x64）/ ubuntu / windows → 每个 runner：`gen-icon.py` + `npx tauri icon` 生成图标 → `CGO_ENABLED=0 go build -ldflags "-X main.version=<tag>"` 出 `binaries/friday-<triple>` → rust-toolchain + tauri-action 出安装包
- 产物：`.dmg`（双架构）、`.exe`（NSIS）、`.deb`/`.AppImage`，tag 时自动创建 GitHub Release
- 私有模块：`webfactory/ssh-agent` + secrets SSH 私钥（`DEPLOY_SSH_KEY`，与 Docker 构建同一把）+ `GOPRIVATE`
- 签名 secrets 预留注释位（APPLE_CERTIFICATE… / WINDOWS_CERTIFICATE…），补上即出签名包
