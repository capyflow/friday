# Friday 桌面端方案（Tauri v2 + Go sidecar）

> 状态：已评审通过，外壳工程与打包脚本已落地（2026-09-28）；托盘驻留、系统通知和 CI 检查已接入，正式跨平台发布仍待验收。
> 关联：[desktop/README.md](../desktop/README.md)（操作手册）、[ROADMAP.md](ROADMAP.md)

## 一、总体架构

现状已具备条件：单 Go 二进制 + `go:embed` 内嵌 UI + 本地 HTTP 服务，语音 cgo 链路已归档（`CGO_ENABLED=0` 纯 Go 交叉编译已实测通过，单平台约 16MB 静态二进制）。

桌面形态 = **Tauri v2 外壳（Rust，系统 WebView）+ Go 内核 sidecar 子进程**。分工原则：**大脑/业务留在 Go，OS 集成（窗口、快捷键、托盘、自启、自动更新）在壳**。

启动链路：

1. Tauri 启动 → 解析数据目录（见「数据目录与内核二进制」）→ 拉起内核：`<包内>/friday --desktop --data-dir <数据目录> --port 47831`（固定端口保登录态；被占用时退回 `--port 0`）
2. 首次运行：外壳把随包分发的 `config.yaml.example` 播种为数据目录的 `config.yaml`（对齐 docker entrypoint 模式）；API 密钥经数据目录可选的 `friday.env`（KEY=VALUE）注入子进程环境——解决 Finder/Dock 启动拿不到 shell 环境变量的问题
3. Go 内核 chdir 到数据目录 → 在给定端口起服务（固定端口被占用时由外壳传 0，改用随机空闲端口） → stdout 打印 `FRIDAY_ADDR=http://127.0.0.1:<port>`
4. 外壳逐行解析 stdout，拿到地址后通知主窗口导航（此前显示启动占位页）
5. 退出链路见「生命周期一致性」

## 一·五、数据目录与内核二进制（一体包）

**数据目录解析顺序**（`resolve_data_dir`）：

1. `FRIDAY_DESKTOP_DATA_DIR` 环境变量（开发/测试覆盖，须绝对路径，不落指针）
2. 指针文件 `系统配置目录/Friday.launcher.json`（`{"dataDir": "…"}`）；指针指向的目录已不存在 → 回退默认路径并重跑向导
3. 都没有 → **首装向导模式**：内核先按默认路径拉起，主窗口进入应用内向导页 `/wizard.html`（`needs_dir_setup=true` 时占位页自动导航）——第一步创建管理员账号（已有账号则跳过），第二步确认数据目录。目录规则：空目录/不存在 → 原样使用；已有 Friday 数据（config.yaml 等）→ 原样接管；其它非空目录 → 收进其下 `Friday/` 子目录。网盘同步目录（iCloud/OneDrive/Dropbox/Google Drive）需二次确认。确认后：写指针 → 迁移本次向导产生的文件（账号/配置；目标已有数据则跳过=接管）→ 内核以新目录重启 → 跳登录页。删除指针文件即可重跑向导

**内核一体包**（2026-10-28 起，替代早先的「外置内核」方案）：内核即随包分发的 sidecar，落在外壳可执行文件旁边，**卸载应用即随之删除，数据目录只存数据**——目录里没有二进制，换数据目录/换设备迁移的都是纯数据，Windows 卸载器也能顺手带走内核。内核更新随应用更新走（后续 updater 插件整包替换）；启动时尽力清理旧版外置布局遗留的 `<数据目录>/bin/friday`。壳-内核契约（`--desktop/--data-dir/--port <port>`、stdout 的 `FRIDAY_ADDR`、stdin 的 `/quit`）不变。曾选「外置内核」（数据目录/bin/friday，包内做首装种子）是为内核自更新通道留地基——通道一直未建，外置反而让每次迭代要多同步一份二进制，遂回归一体包。

已知取舍：Windows 上数据目录在 `%APPDATA%`（Roaming），漫游同步只带数据不带二进制；企业部署需求出现时再拆 `%LocalAppData%`。

## 一·六、登录态持久化（三层）

1. **JWT（服务端）**：HS256、7 天 TTL，载荷带用户名与密码版本（改密即全量失效）；签名密钥落盘 `<数据目录>/auth-key`，内核重启后旧 token 仍可验签
2. **固定端口（origin）**：外壳固定传 `--port 47831`，WebView origin 恒定，localStorage（`friday.authToken`，主窗口与弹窗共享）天然跨重启保留；端口被占时退回 `--port 0` 随机端口
3. **原生冷备（兜底）**：外壳把 token 原子写入 `<数据目录>/desktop-auth-token`（Unix 0600），原生命令 `auth_get_token/auth_set_token/auth_clear_token` 仅在 origin 与内核一致的主窗口/弹窗内可用；前端 `auth.js` 启动 `hydrateToken()` 回填（native 为准，本地多出的推给 native），`setToken` 写穿同步。**端口回退随机或 WebView 数据被清时，登录态由这层兜住**。浏览器直访无 `__TAURI__`，自动降级为纯 localStorage

## 二、生命周期一致性（三层保证）

sidecar 机制本身不提供父子进程生命周期绑定（Tauri 不自动杀子进程），需显式接线：

1. **窗口关闭（驻留）**：主窗口关闭只隐藏窗口并保留内核；托盘菜单的「退出 Friday」或系统退出事件才进入进程退出链。退出时 Tauri 向子进程 stdin 写一行 `/quit`，3 秒未退则 `kill()` 兜底。Go 侧桌面模式监听 stdin：`/quit` 或 EOF 均触发现有 `stop()` 优雅退出链（任务引擎/会话/MCP defer Close 全部执行）
2. **壳崩溃/被强杀（OS 级，零代码）**：子进程 stdin 是管道，写端握在外壳手里；进程死亡 → OS 关闭句柄 → Go 读到 EOF → 优雅退出。经典 sidecar 生命周期模式（VS Code language server 同款），跨平台通用
3. **兜底看门狗（加固）**：Go 内核每 30s 检查 `os.Getppid()==1`（Unix 孤儿进程被过继给 launchd/init）即优雅退出，防 stdin 监听本身失效的极端情况；Windows 无进程过继机制该检查恒不触发（退出靠 stdin EOF），Windows 终极手段 Job Object 首期不引入

反向一致：内核先死（如配置错误）→ 外壳收到 sidecar `Terminated` 事件 → 窗口显示错误页（后续可加自动重启策略）。

已知边界：外壳最终兜底是 kill，Go 侧落盘均为原子写（tmp+rename），最多丢最后一次会话快照。

## 三、Go 侧改造（改动很小）

`cmd/robot/main.go`：

- `--desktop` / `--data-dir` / `--host` / `--port`（0=随机空闲端口）/ `--version` 参数；桌面模式强制本机监听
- 桌面模式 chdir 数据目录（默认 `os.UserConfigDir()/Friday`：mac `~/Library/Application Support/Friday`、Win `%APPDATA%\Friday`、Linux `~/.config/Friday`），全部相对路径数据文件自动落位
- 桌面模式无终端 REPL；stdin 循环：读到 `/quit` 或 EOF → 优雅退出；ppid 看门狗兜底
- 服务就绪（轮询 `GET /` 200）后 stdout 打印 `FRIDAY_ADDR=` 标记；桌面模式强制 admin 开启
- `version` 变量供 `-ldflags -X main.version=` 注入

`internal/admin`：新增 `GET /palette` 弹窗页（`go:embed` + 公有静态路由，模式同 `handleIndex`）——Spotlight 风格输入框，同源调 `/api/chat`（SSE 流式渲染），未登录提示先去主窗口登录。

桌面版强制监听 `127.0.0.1`；Web 远程部署须显式设置 host。首次账号通过本机设置页面创建，远程部署使用启动凭据预置，旧账号保持兼容。

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
- 托盘「显示 Friday」/「退出 Friday」，关闭主窗口隐藏并继续运行；系统退出 → stdin 写 `/quit` → 3s 后 kill 兜底
- 解析 `FRIDAY_NOTICE=`，显式授权后交给系统通知；准确内核来源的主窗口才有权限执行通知开启命令
- `tauri-plugin-global-shortcut` 注册全局快捷键（默认 **Alt+Space**，常量一行可改）→ 创建/切换弹窗：无边框+透明+置顶+不进任务栏，加载内核 `/palette`；失焦/Esc 隐藏（仅隐藏不销毁）
- 零 JS 构建链：无 npm 工程，`withGlobalTauri` 用全局 API

弹窗页放 **Go 侧**是刻意设计：同源共享登录会话（无 CORS/鉴权烦恼）、浏览器访问 `:port/palette` 也能用、壳保持极薄。

## 五、后续按系统扩展不同功能的路径

分层原则：**OS 集成 = 壳（Tauri 插件/Rust），业务能力 = 内核（Go agent 工具）**，两侧经本地 HTTP / stdin 通信。

| 需求 | 实现方式 | 难度 |
|------|----------|------|
| 全局快捷键/弹窗 | 本方案第一个样例 | ✅ |
| 系统托盘菜单 | tauri `tray-icon` feature | 已接入 |
| 系统通知 | 显式授权、系统状态检查与通知中心兜底 | 已接入，跨平台系统联调待验收 |
| 外部链接（MCP 授权 / 正文外链） | 外壳命令 `open_url` 交给系统浏览器（WebView 不允许 `window.open`）；只放行 http/https | 已接入 |
| 开机自启 | autostart 插件 | 低 |
| 自动更新 | updater 插件（需签名密钥对） | 中 |
| 剪贴板/文件拖拽 | clipboard-manager 插件 / 拖放事件 → 内核文件工具 | 低 |
| 深度原生 API（菜单栏定制、媒体控制等） | 壳层 Rust 直调系统 API；或封装为 Go 侧新 Tool 给 agent | 按需 |

平台差异：

- **macOS**：透明窗需 `macOSPrivateApi`（已配）；分发需 Apple Developer（$99/年）签名+公证，否则首次打开要右键绕过 Gatekeeper
- **Windows**：WebView2 自动引导安装；SmartScreen 警告需代码签名证书（首期未签名）
- **Linux**：X11 全局快捷键正常，Wayland 依赖 compositor 协议支持；产物 deb/AppImage

## 六、CI/发布：`.github/workflows/desktop-release.yml`

以下为正式发布计划，`desktop-release.yml` 尚未创建。本轮已有 `.github/workflows/check.yml` 覆盖 Go、前端、嵌入产物一致性及本地假模型集成检查；本地 macOS 调试包可构建，签名/公证及 Windows/Linux 安装验收尚待发布环境。

- 触发：push tag `desktop-v*` + `workflow_dispatch`（手动出测试产物）
- 矩阵：macos-14（arm64）/ macos-13（x64）/ ubuntu / windows → 每个 runner：`gen-icon.py` + `npx tauri icon` 生成图标 → `CGO_ENABLED=0 go build -ldflags "-X main.version=<tag>"` 出 `binaries/friday-<triple>` → rust-toolchain + tauri-action 出安装包
- 产物：`.dmg`（双架构）、`.exe`（NSIS）、`.deb`/`.AppImage`，tag 时自动创建 GitHub Release
- 私有模块：`webfactory/ssh-agent` + secrets SSH 私钥（`DEPLOY_SSH_KEY`，与 Docker 构建同一把）+ `GOPRIVATE`
- 签名 secrets 预留注释位（APPLE_CERTIFICATE… / WINDOWS_CERTIFICATE…），补上即出签名包
