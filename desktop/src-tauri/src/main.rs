// Friday 桌面外壳（docs/DESKTOP.md）：拉起 Go 内核 sidecar、解析 FRIDAY_ADDR
// 并导航主窗口、全局快捷键唤起 /palette 弹窗、退出时向内核 stdin 写 /quit。
// 壳保持无业务：大脑/能力全部在 Go 内核，两侧经本地 HTTP 与 stdin 通信。

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::collections::BTreeSet;
use std::io::Write;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use std::time::Duration;

use tauri::menu::{Menu, MenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{Emitter, Manager, RunEvent, WebviewUrl, WebviewWindowBuilder, WindowEvent};
use tauri_plugin_dialog::DialogExt;
use tauri_plugin_global_shortcut::{GlobalShortcutExt, Shortcut, ShortcutState};
#[cfg(not(target_os = "macos"))]
use tauri_plugin_notification::{NotificationExt, PermissionState};
use tauri_plugin_shell::process::{CommandChild, CommandEvent};
use tauri_plugin_shell::ShellExt;

/// 全局快捷键（Spotlight 风格弹窗），想换键改这一行（格式见 tauri-plugin-global-shortcut）。
const PALETTE_SHORTCUT: &str = "Alt+Space";
/// 退出时等内核优雅退出的时长，超时 kill 兜底。
const QUIT_GRACE: Duration = Duration::from_secs(3);
/// 内核数据目录名（系统配置目录下）。
const DATA_DIR_NAME: &str = "Friday";
/// 数据目录指针文件（系统配置目录下，记录首跑选定的数据目录；删除即重跑首导向）。
const LAUNCHER_POINTER: &str = "Friday.launcher.json";

/// 内核进程状态：服务地址、子进程句柄、退出标记、数据目录、最后一条 stderr 行。
struct Kernel {
    addr: Mutex<Option<String>>,
    child: Mutex<Option<CommandChild>>,
    exited: AtomicBool,
    quitting: AtomicBool,
    data_dir: PathBuf,
    last_stderr: Mutex<String>,
    dead_reason: Mutex<Option<String>>,
    palette_shortcut: Shortcut,
}

#[derive(serde::Deserialize)]
struct NativeNotice {
    id: String,
    text: String,
}

#[derive(Default, Clone, serde::Deserialize, serde::Serialize)]
struct NativeHistory {
    enabled: bool,
    seen: BTreeSet<String>,
}

struct NativeNotifications {
    history: Mutex<NativeHistory>,
    path: PathBuf,
    load_failed: bool,
}

impl NativeNotifications {
    fn load(path: PathBuf) -> Self {
        let (history, load_failed) = match std::fs::read(&path) {
            Ok(data) => match serde_json::from_slice(&data) {
                Ok(history) => (history, false),
                Err(_) => (NativeHistory::default(), true),
            },
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
                (NativeHistory::default(), false)
            }
            Err(_) => (NativeHistory::default(), true),
        };
        Self {
            history: Mutex::new(history),
            path,
            load_failed,
        }
    }

    fn save(&self, history: &NativeHistory) -> Result<(), String> {
        if self.load_failed {
            return Err("系统通知记录不可用，请检查数据目录".into());
        }
        let data = serde_json::to_vec(history).map_err(|_| "无法编码系统通知记录")?;
        static NEXT_WRITE: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let tmp = self.path.with_extension(format!(
            "json.tmp-{}-{}",
            std::process::id(),
            NEXT_WRITE.fetch_add(1, Ordering::SeqCst)
        ));
        let mut options = std::fs::OpenOptions::new();
        options.create_new(true).write(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options
            .open(&tmp)
            .map_err(|_| "无法创建系统通知记录临时文件")?;
        let result = (|| -> std::io::Result<()> {
            file.write_all(&data)?;
            file.sync_all()?;
            drop(file);
            std::fs::rename(&tmp, &self.path)
        })();
        if result.is_err() {
            let _ = std::fs::remove_file(&tmp);
        }
        result.map_err(|_| "无法保存系统通知记录，请检查数据目录".into())
    }

    fn enable(&self) -> Result<(), String> {
        let mut history = self.history.lock().unwrap();
        let mut next = history.clone();
        next.enabled = true;
        self.save(&next)?;
        *history = next;
        Ok(())
    }

    fn enabled<F>(&self, check_permission: F) -> Result<bool, String>
    where
        F: FnOnce() -> Result<bool, String>,
    {
        if self.load_failed || !self.history.lock().unwrap().enabled {
            return Ok(false);
        }
        check_permission()
    }

    // 必须同时有显式开启和现有系统权限；失败不标 seen，系统接受后才记录。
    fn dispatch<F>(&self, notice: &NativeNotice, granted: bool, send: F) -> Result<bool, String>
    where
        F: FnOnce(&NativeNotice) -> Result<(), String>,
    {
        let mut history = self.history.lock().unwrap();
        if self.load_failed || !history.enabled || !granted || history.seen.contains(&notice.id) {
            return Ok(false);
        }
        send(notice)?;
        history.seen.insert(notice.id.clone());
        self.save(&history)?;
        Ok(true)
    }
}

fn parse_native_notice(line: &str) -> Option<NativeNotice> {
    let raw = line.strip_prefix("FRIDAY_NOTICE=")?;
    if raw.len() > 64 * 1024 {
        return None;
    }
    let notice: NativeNotice = serde_json::from_str(raw).ok()?;
    if notice.id.trim().is_empty() || notice.id.len() > 256 || notice.text.trim().is_empty() {
        return None;
    }
    Some(notice)
}

fn trusted_notification_origin(label: &str, current: &str, kernel: Option<&str>) -> bool {
    if label != "main" {
        return false;
    }
    let (Ok(current), Some(kernel)) = (tauri::Url::parse(current), kernel) else {
        return false;
    };
    let Ok(kernel) = tauri::Url::parse(kernel) else {
        return false;
    };
    current.scheme() == "http"
        && kernel.scheme() == "http"
        && current.host_str() == Some("127.0.0.1")
        && kernel.host_str() == Some("127.0.0.1")
        && current.username().is_empty()
        && current.password().is_none()
        && current.origin() == kernel.origin()
}

#[tauri::command]
async fn enable_notifications(
    app: tauri::AppHandle,
    window: tauri::WebviewWindow,
) -> Result<bool, String> {
    let addr = app.state::<Kernel>().addr.lock().unwrap().clone();
    let current = window.url().map_err(|_| "无法确认当前窗口来源")?;
    if !trusted_notification_origin(window.label(), current.as_str(), addr.as_deref()) {
        return Err("当前窗口不能启用系统通知".into());
    }
    let cloned = app.clone();
    tauri::async_runtime::spawn_blocking(move || {
        if !request_native_permission(&cloned)? {
            return Ok(false);
        }
        cloned.state::<NativeNotifications>().enable()?;
        Ok(true)
    })
    .await
    .map_err(|_| "系统通知授权未完成".to_string())?
}

#[tauri::command]
async fn notifications_enabled(
    app: tauri::AppHandle,
    window: tauri::WebviewWindow,
) -> Result<bool, String> {
    let addr = app.state::<Kernel>().addr.lock().unwrap().clone();
    let current = window.url().map_err(|_| "无法确认当前窗口来源")?;
    if !trusted_notification_origin(window.label(), current.as_str(), addr.as_deref()) {
        return Err("当前窗口不能查询系统通知权限".into());
    }
    let cloned = app.clone();
    tauri::async_runtime::spawn_blocking(move || {
        cloned
            .state::<NativeNotifications>()
            .enabled(|| native_permission_granted(&cloned))
    })
    .await
    .map_err(|_| "无法查询系统通知权限".to_string())?
}

/// 内核状态快照：占位页主动查询用（防"就绪事件早于页面监听注册"的竞态）。
#[derive(serde::Serialize)]
struct KernelState {
    addr: Option<String>,
    dead: bool,
    reason: Option<String>,
}

#[tauri::command]
fn kernel_state(state: tauri::State<Kernel>) -> KernelState {
    KernelState {
        addr: state.addr.lock().unwrap().clone(),
        dead: state.exited.load(Ordering::SeqCst),
        reason: state.dead_reason.lock().unwrap().clone(),
    }
}

// ---------- 数据目录解析与内核定位 ----------

#[derive(serde::Deserialize, serde::Serialize)]
struct LauncherPointer {
    #[serde(rename = "dataDir")]
    data_dir: String,
}

fn read_pointer(path: &std::path::Path) -> Option<PathBuf> {
    let raw = std::fs::read(path).ok()?;
    let pointer: LauncherPointer = serde_json::from_slice(&raw).ok()?;
    let dir = PathBuf::from(pointer.data_dir);
    if !dir.is_absolute() {
        return None;
    }
    Some(dir)
}

fn write_pointer(path: &std::path::Path, dir: &std::path::Path) -> Result<(), String> {
    let data = serde_json::to_vec(&LauncherPointer {
        data_dir: dir.to_string_lossy().to_string(),
    })
    .map_err(|_| "无法编码数据目录指针".to_string())?;
    std::fs::write(path, data).map_err(|e| format!("无法写入数据目录指针：{e}"))
}

/// 网盘同步目录特征：文件锁与半透明占位文件会拖挂数据目录，选中时须确认。
fn looks_like_sync_drive(path: &std::path::Path) -> bool {
    const MARKERS: [&str; 5] = [
        "Mobile Documents",
        "com~apple~CloudDocs",
        "OneDrive",
        "Dropbox",
        "Google Drive",
    ];
    let text = path.to_string_lossy();
    MARKERS.iter().any(|m| text.contains(m))
}

/// 已有 Friday 数据的特征文件（首装种子与「接管已有数据」的判断依据）。
fn looks_like_friday_data(dir: &std::path::Path) -> bool {
    dir.join("config.yaml").is_file()
        || dir.join("friday-kernel.log").is_file()
        || dir.join("bin").exists()
}

/// 把用户在目录选择框里选中的路径收敛成实际数据目录：
/// 不存在/空目录 → 原样；已有 Friday 数据 → 原样（接管）；其它非空 → 收进其下 Friday 子目录。
fn effective_target(picked: &std::path::Path) -> PathBuf {
    let is_empty = std::fs::read_dir(picked).map(|mut entries| entries.next().is_none());
    match is_empty {
        Ok(true) | Err(_) => picked.to_path_buf(),
        Ok(false) => {
            if looks_like_friday_data(picked) {
                picked.to_path_buf()
            } else {
                picked.join(DATA_DIR_NAME)
            }
        }
    }
}

fn ensure_writable(dir: &std::path::Path) -> Result<(), String> {
    std::fs::create_dir_all(dir).map_err(|e| format!("无法创建目录 {}：{e}", dir.display()))?;
    let probe = dir.join(".friday-write-probe");
    std::fs::write(&probe, b"ok").map_err(|e| format!("目录不可写 {}：{e}", dir.display()))?;
    let _ = std::fs::remove_file(&probe);
    Ok(())
}

fn pick_data_dir(
    app: &tauri::AppHandle,
    default_dir: &std::path::Path,
) -> Option<std::path::PathBuf> {
    app.dialog()
        .file()
        .set_title("选择 Friday 数据目录")
        .set_directory(default_dir)
        .blocking_pick_folder()
        .and_then(|picked| picked.into_path().ok())
}

fn confirm_sync_drive(app: &tauri::AppHandle, dir: &std::path::Path) -> bool {
    app.dialog()
        .message(format!(
            "{} 位于网盘同步目录，可能出现文件锁与性能问题。\n仍要使用这个位置吗？",
            dir.display()
        ))
        .title("同步盘提醒")
        .buttons(tauri_plugin_dialog::MessageDialogButtons::OkCancelCustom(
            "仍要使用".into(),
            "换个位置".into(),
        ))
        .blocking_show()
}

/// 数据目录解析：开发覆盖（FRIDAY_DESKTOP_DATA_DIR，不落指针）→ 指针文件 →
/// 首跑目录选择（默认系统配置目录/Friday；取消即认可默认并落指针，此后不再询问）。
fn resolve_data_dir(app: &tauri::AppHandle) -> Result<PathBuf, String> {
    if let Some(value) = std::env::var_os("FRIDAY_DESKTOP_DATA_DIR").filter(|v| !v.is_empty()) {
        let path = PathBuf::from(value);
        if !path.is_absolute() {
            return Err("FRIDAY_DESKTOP_DATA_DIR 必须为绝对路径".into());
        }
        return Ok(path);
    }
    let cfg = app
        .path()
        .config_dir()
        .map_err(|_| "无法定位系统配置目录".to_string())?;
    let default_dir = cfg.join(DATA_DIR_NAME);
    let pointer = cfg.join(LAUNCHER_POINTER);
    if let Some(dir) = read_pointer(&pointer) {
        // 指针失效（目录被移动/删除）→ 回退默认路径，不重跑首导向，避免数据惊吓。
        if dir.exists() {
            return Ok(dir);
        }
        eprintln!(
            "[外壳] 指针指向的数据目录不存在，回退默认：{}",
            default_dir.display()
        );
        return Ok(default_dir);
    }
    loop {
        let Some(picked) = pick_data_dir(app, &default_dir) else {
            // 取消 = 认可默认位置，落指针防止每次启动都弹框。
            ensure_writable(&default_dir)?;
            write_pointer(&pointer, &default_dir)?;
            return Ok(default_dir);
        };
        let target = effective_target(&picked);
        if looks_like_sync_drive(&target) && !confirm_sync_drive(app, &target) {
            continue; // 拒绝同步盘 → 重新选
        }
        ensure_writable(&target)?;
        write_pointer(&pointer, &target)?;
        return Ok(target);
    }
}

/// 内核二进制的运行路径：数据目录/bin/friday（更新通道未来的替换目标；
/// 包内那份只是首装种子）。改这里要同步 docs/DESKTOP.md。
fn kernel_path(data_dir: &std::path::Path) -> PathBuf {
    let name = if cfg!(windows) { "friday.exe" } else { "friday" };
    data_dir.join("bin").join(name)
}

/// 首装种子：把随包分发的内核拷到数据目录 bin/（已存在则不动——版本归更新通道管）。
fn seed_kernel(data_dir: &std::path::Path) {
    let dst = kernel_path(data_dir);
    if dst.exists() {
        return;
    }
    std::fs::create_dir_all(dst.parent().expect("内核路径必有父目录"))
        .expect("创建内核目录失败");
    let src = std::env::current_exe()
        .expect("无法定位外壳可执行文件")
        .parent()
        .map(|dir| {
            dir.join(if cfg!(windows) { "friday.exe" } else { "friday" })
        })
        .expect("无法定位随包内核");
    match std::fs::copy(&src, &dst) {
        Ok(_) => {
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                let _ = std::fs::set_permissions(&dst, std::fs::Permissions::from_mode(0o755));
            }
            eprintln!("[外壳] 已播种内核：{} ← {}", dst.display(), src.display());
        }
        Err(err) => panic!("播种内核失败（{}）：{err}", src.display()),
    }
}

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            // 防多开：再次启动 → 唤起已有主窗口。
            if let Some(win) = app.get_webview_window("main") {
                let _ = win.show();
                let _ = win.set_focus();
            }
        }))
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_notification::init())
        .plugin(
            tauri_plugin_global_shortcut::Builder::new()
                .with_handler(|app, shortcut, event| {
                    if event.state() != ShortcutState::Pressed {
                        return;
                    }
                    if app.state::<Kernel>().palette_shortcut == *shortcut {
                        toggle_palette(app);
                    }
                })
                .build(),
        )
        .setup(|app| {
            let palette_shortcut: Shortcut = PALETTE_SHORTCUT
                .parse()
                .unwrap_or_else(|_| panic!("PALETTE_SHORTCUT 非法: {PALETTE_SHORTCUT}"));
            // 数据目录：开发覆盖（env）→ 指针文件 → 首跑目录选择（见 resolve_data_dir）。
            // 注意不要用 app_config_dir()：v2 会再拼一层 identifier，产生 com.capyflow.friday/Friday。
            let data_dir = resolve_data_dir(app.handle())
                .map_err(|e| -> Box<dyn std::error::Error> { e.into() })?;
            std::fs::create_dir_all(&data_dir).expect("创建数据目录失败");
            app.manage(NativeNotifications::load(
                data_dir.join("desktop-notifications.json"),
            ));
            app.manage(Kernel {
                addr: Mutex::new(None),
                child: Mutex::new(None),
                exited: AtomicBool::new(false),
                quitting: AtomicBool::new(false),
                data_dir,
                last_stderr: Mutex::new(String::new()),
                dead_reason: Mutex::new(None),
                palette_shortcut,
            });
            setup_tray(app)?;
            if let Some(win) = app.get_webview_window("main") {
                let cloned = win.clone();
                let handle = app.handle().clone();
                win.on_window_event(move |event| {
                    if let WindowEvent::CloseRequested { api, .. } = event {
                        if !handle.state::<Kernel>().quitting.load(Ordering::SeqCst) {
                            api.prevent_close();
                            let _ = cloned.hide();
                        }
                    }
                });
            }
            app.global_shortcut().register(palette_shortcut)?;
            spawn_kernel(app.handle().clone());
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            kernel_state,
            enable_notifications,
            notifications_enabled
        ])
        .build(tauri::generate_context!())
        .expect("构建 Tauri 应用失败")
        .run(|app, event| {
            match event {
                RunEvent::ExitRequested { code, api, .. } if code.is_none() => {
                    // 系统菜单退出（Cmd-Q 等）仍正常终止内核。
                    app.state::<Kernel>().quitting.store(true, Ordering::SeqCst);
                    quit_kernel(app);
                    let _ = api;
                }
                RunEvent::Exit => {
                    app.state::<Kernel>().quitting.store(true, Ordering::SeqCst);
                    quit_kernel(app);
                }
                _ => {}
            }
        });
}

fn show_main(app: &tauri::AppHandle) {
    if let Some(win) = app.get_webview_window("main") {
        let _ = win.show();
        let _ = win.unminimize();
        let _ = win.set_focus();
    }
}

fn setup_tray(app: &tauri::App) -> tauri::Result<()> {
    let show = MenuItem::with_id(app, "show", "显示 Friday", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "退出 Friday", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&show, &quit])?;
    let icon = app.default_window_icon().cloned().unwrap_or_else(|| {
        // 默认图标缺失仍提供可见入口，不依赖额外图片资源。
        tauri::image::Image::new_owned(vec![109, 107, 242, 255].repeat(32 * 32), 32, 32)
    });
    TrayIconBuilder::with_id("friday")
        .icon(icon)
        .tooltip("Friday 个人管家")
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id.as_ref() {
            "show" => show_main(app),
            "quit" => {
                app.state::<Kernel>().quitting.store(true, Ordering::SeqCst);
                app.exit(0);
            }
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                show_main(tray.app_handle());
            }
        })
        .build(app)?;
    Ok(())
}

/// 拉起 Go 内核并在后台线程解析其 stdout。内核运行于数据目录 bin/ 下的
/// 种子拷贝（首装时从包内复制），更新通道未来只替换这一份。
fn spawn_kernel(app: tauri::AppHandle) {
    let data_dir = app.state::<Kernel>().data_dir.clone();
    eprintln!("[外壳] 数据目录：{}", data_dir.display());
    seed_config(&app, &data_dir);
    seed_kernel(&data_dir);
    let envs = read_env_file(&data_dir.join("friday.env"));

    let data_arg = data_dir.to_string_lossy().to_string();
    let (mut rx, child) = app
        .shell()
        .command(kernel_path(&data_dir).to_string_lossy().to_string())
        .args(["--desktop", "--data-dir", data_arg.as_str(), "--port", "0"])
        // cwd 必须落在数据目录：allspark-go/logx 在包 init 阶段（早于 Go 侧 chdir）
        // 就 mkdir ./logs，launchd 拉起的进程 cwd 是 /（只读）会直接 panic。
        .current_dir(&data_dir)
        .envs(envs)
        .spawn()
        .expect("拉起 Friday 内核失败");
    app.state::<Kernel>().child.lock().unwrap().replace(child);

    tauri::async_runtime::spawn(async move {
        while let Some(event) = rx.recv().await {
            match event {
                CommandEvent::Stdout(line) => {
                    let line = String::from_utf8_lossy(&line).trim_end().to_string();
                    if line.starts_with("FRIDAY_NOTICE=") {
                        if let Some(notice) = parse_native_notice(&line) {
                            let handle = app.clone();
                            tauri::async_runtime::spawn_blocking(move || {
                                let notifications = handle.state::<NativeNotifications>();
                                let granted = notifications
                                    .enabled(|| native_permission_granted(&handle))
                                    .unwrap_or(false);
                                match notifications.dispatch(&notice, granted, |notice| {
                                    show_native_notice(&handle, notice)
                                }) {
                                    Ok(true) => {}
                                    Ok(false) => eprintln!(
                                        "[外壳] 系统通知未启用或已处理，详情保留在通知中心"
                                    ),
                                    Err(_) => {
                                        eprintln!("[外壳] 系统通知未完成，请查看应用通知中心")
                                    }
                                }
                            });
                        } else {
                            eprintln!("[外壳] 忽略无效的系统通知消息");
                        }
                        continue;
                    }
                    kernel_log(&app, &line);
                    if let Some(addr) = line.trim().strip_prefix("FRIDAY_ADDR=") {
                        kernel_ready(&app, addr.trim().to_string());
                    }
                }
                CommandEvent::Stderr(line) => {
                    let line = String::from_utf8_lossy(&line).trim_end().to_string();
                    kernel_log(&app, &line);
                    if !line.is_empty() {
                        // 留末行：内核异常退出时作为死因展示到错误页。
                        *app.state::<Kernel>().last_stderr.lock().unwrap() = line;
                    }
                }
                CommandEvent::Error(err) => {
                    kernel_log(&app, &format!("[外壳] sidecar 错误：{err}"));
                    kernel_dead(&app, err);
                }
                CommandEvent::Terminated(_) => {
                    let reason = app.state::<Kernel>().last_stderr.lock().unwrap().clone();
                    kernel_dead(&app, reason);
                }
                _ => {}
            }
        }
    });
}

/// 把内核输出追加到数据目录的 friday-kernel.log（用户排障入口），同步打到终端。
fn kernel_log(app: &tauri::AppHandle, line: &str) {
    eprintln!("[内核] {line}");
    let path = app.state::<Kernel>().data_dir.join("friday-kernel.log");
    if let Ok(mut f) = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
    {
        let _ = std::io::Write::write_all(&mut f, format!("{line}\n").as_bytes());
    }
}

/// 内核服务就绪：记录地址，主窗口从占位页导航过去。
fn kernel_ready(app: &tauri::AppHandle, addr: String) {
    *app.state::<Kernel>().addr.lock().unwrap() = Some(addr.clone());
    eprintln!("[外壳] 内核就绪：{addr}");
    let _ = app.emit("server-ready", addr);
}

/// 内核先死（如配置错误）：主窗口显示错误页并带上死因（自动重启策略后续再加）。
fn mark_kernel_dead(kernel: &Kernel, reason: &str) -> bool {
    *kernel.addr.lock().unwrap() = None;
    kernel.exited.store(true, Ordering::SeqCst);
    if kernel.quitting.load(Ordering::SeqCst) {
        return false;
    }
    *kernel.dead_reason.lock().unwrap() = Some(reason.to_string());
    true
}

fn kernel_dead(app: &tauri::AppHandle, reason: String) {
    let kernel = app.state::<Kernel>();
    if !mark_kernel_dead(&kernel, &reason) {
        return;
    }
    if let Some(win) = app.get_webview_window("palette") {
        let _ = win.hide();
    }
    let _ = app.emit("kernel-dead", reason.clone());
    if let Some(win) = app.get_webview_window("main") {
        // tauri 前端资源的本地 origin：macOS/Linux 为 tauri://localhost，Windows 为 http://tauri.localhost。
        let origin = if cfg!(windows) {
            "http://tauri.localhost"
        } else {
            "tauri://localhost"
        };
        let dir = app.state::<Kernel>().data_dir.display().to_string();
        let _ = win.eval(&format!(
            "location.replace('{origin}/error.html?reason={}&dir={}')",
            percent_encode(&reason),
            percent_encode(&dir),
        ));
    }
}

/// query 值用的最小百分号编码（RFC 3986 unreserved 之外全转义）。
fn percent_encode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

/// 全局快捷键：创建/切换 /palette 弹窗（无边框 + 透明 + 置顶 + 不进任务栏）。
fn toggle_palette(app: &tauri::AppHandle) {
    let addr = app.state::<Kernel>().addr.lock().unwrap().clone();
    let Some(addr) = addr else {
        // 内核未就绪：把主窗口拉到前台，让用户看到启动进度。
        if let Some(win) = app.get_webview_window("main") {
            let _ = win.show();
            let _ = win.set_focus();
        }
        return;
    };
    match app.get_webview_window("palette") {
        Some(win) => {
            if win.is_visible().unwrap_or(false) {
                let _ = win.hide();
            } else {
                let _ = win.show();
                let _ = win.set_focus();
            }
        }
        None => {
            let url: tauri::Url = format!("{addr}/palette").parse().expect("内核地址非法");
            let win = WebviewWindowBuilder::new(app, "palette", WebviewUrl::External(url))
                .title("Friday")
                .inner_size(680.0, 420.0)
                .decorations(false)
                .transparent(true)
                .always_on_top(true)
                .skip_taskbar(true)
                .resizable(false)
                .build()
                .expect("创建弹窗失败");
            // 失焦即隐藏（仅隐藏不销毁，保住会话与登录态）；Esc 隐藏由页面侧处理。
            let w = win.clone();
            win.on_window_event(move |event| {
                if let WindowEvent::Focused(false) = event {
                    let _ = w.hide();
                }
            });
        }
    }
}

/// 正常退出（确定性）：向内核 stdin 写 /quit，等 QUIT_GRACE 后 kill 兜底。
/// 壳崩溃/被强杀时无需此函数——内核读 stdin 管道 EOF 自行优雅退出。
fn quit_kernel(app: &tauri::AppHandle) {
    let kernel = app.state::<Kernel>();
    let Some(mut child) = kernel.child.lock().unwrap().take() else {
        return;
    };
    let _ = child.write(b"/quit\n");
    for _ in 0..(QUIT_GRACE.as_millis() / 100) {
        if kernel.exited.load(Ordering::SeqCst) {
            return;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    eprintln!(
        "[外壳] 内核未在 {}s 内退出，kill 兜底",
        QUIT_GRACE.as_secs()
    );
    let _ = child.kill();
}

/// 首次运行：把随包分发的 config.yaml.example 播种为数据目录的 config.yaml
/// （对齐 docker entrypoint 模式；已存在则不动，保留用户配置）。
fn seed_config(app: &tauri::AppHandle, data_dir: &PathBuf) {
    let dst = data_dir.join("config.yaml");
    if dst.exists() {
        return;
    }
    let resource = app
        .path()
        .resource_dir()
        .expect("无法定位资源目录")
        .join("resources/config.yaml.example");
    match std::fs::copy(&resource, &dst) {
        Ok(_) => eprintln!(
            "[外壳] 已播种配置：{} ← {}",
            dst.display(),
            resource.display()
        ),
        Err(err) => panic!("播种 config.yaml 失败（{}）：{err}", resource.display()),
    }
}

/// 读数据目录的 friday.env（KEY=VALUE，# 注释），作为内核子进程环境——
/// 解决 Finder/Dock 启动拿不到 shell 环境变量的问题（如 DEEPSEEK_API_KEY）。
fn read_env_file(path: &PathBuf) -> Vec<(String, String)> {
    let Ok(text) = std::fs::read_to_string(path) else {
        return Vec::new();
    };
    text.lines()
        .filter_map(|line| {
            let line = line.trim();
            if line.is_empty() || line.starts_with('#') {
                return None;
            }
            let (k, v) = line.split_once('=')?;
            Some((k.trim().to_string(), v.trim().to_string()))
        })
        .collect()
}

#[cfg(not(target_os = "macos"))]
fn request_native_permission(app: &tauri::AppHandle) -> Result<bool, String> {
    app.notification()
        .request_permission()
        .map(|state| state == PermissionState::Granted)
        .map_err(|_| "系统通知暂不可用，请使用通知中心".into())
}

#[cfg(not(target_os = "macos"))]
fn native_permission_granted(app: &tauri::AppHandle) -> Result<bool, String> {
    app.notification()
        .permission_state()
        .map(|state| state == PermissionState::Granted)
        .map_err(|_| "无法查询系统通知权限".into())
}

#[cfg(not(target_os = "macos"))]
fn show_native_notice(app: &tauri::AppHandle, notice: &NativeNotice) -> Result<(), String> {
    let mut notification = notify_rust::Notification::new();
    notification.summary("Friday 提醒").body(&notice.text);
    #[cfg(windows)]
    notification.app_id(&app.config().identifier);
    let _ = app;
    notification
        .show()
        .map(|_| ())
        .map_err(|_| "系统未接受通知".into())
}

#[cfg(target_os = "macos")]
mod mac_notifications {
    use super::*;
    use block2::RcBlock;
    use objc2::runtime::Bool;
    use objc2_foundation::{NSError, NSString};
    use objc2_user_notifications::{
        UNAuthorizationOptions, UNAuthorizationStatus, UNMutableNotificationContent,
        UNNotificationRequest, UNNotificationSettings, UNUserNotificationCenter,
    };
    use std::ptr::NonNull;
    use std::sync::mpsc;

    pub fn granted() -> Result<bool, String> {
        let (tx, rx) = mpsc::channel();
        let callback = RcBlock::new(move |settings: NonNull<UNNotificationSettings>| {
            // 系统保证回调指针在回调期间有效，仅复制枚举值。
            let status = unsafe { settings.as_ref() }.authorizationStatus();
            let _ = tx.send(
                status == UNAuthorizationStatus::Authorized
                    || status == UNAuthorizationStatus::Provisional,
            );
        });
        UNUserNotificationCenter::currentNotificationCenter()
            .getNotificationSettingsWithCompletionHandler(&callback);
        rx.recv_timeout(Duration::from_secs(5))
            .map_err(|_| "无法查询系统通知权限".into())
    }

    pub fn request() -> Result<bool, String> {
        let (tx, rx) = mpsc::channel();
        let callback = RcBlock::new(move |granted: Bool, error: *mut NSError| {
            let _ = tx.send(granted.as_bool() && error.is_null());
        });
        UNUserNotificationCenter::currentNotificationCenter()
            .requestAuthorizationWithOptions_completionHandler(
                UNAuthorizationOptions::Alert | UNAuthorizationOptions::Sound,
                &callback,
            );
        rx.recv_timeout(Duration::from_secs(60))
            .map_err(|_| "系统授权未完成，请稍后重试".into())
    }

    pub fn show(notice: &NativeNotice) -> Result<(), String> {
        let content = UNMutableNotificationContent::new();
        content.setTitle(&NSString::from_str("Friday 提醒"));
        content.setBody(&NSString::from_str(&notice.text));
        let request = UNNotificationRequest::requestWithIdentifier_content_trigger(
            &NSString::from_str(&notice.id),
            &content,
            None,
        );
        let (tx, rx) = mpsc::channel();
        let callback = RcBlock::new(move |error: *mut NSError| {
            let _ = tx.send(error.is_null());
        });
        UNUserNotificationCenter::currentNotificationCenter()
            .addNotificationRequest_withCompletionHandler(&request, Some(&callback));
        match rx.recv_timeout(Duration::from_secs(5)) {
            Ok(true) => Ok(()),
            _ => Err("系统未接受通知".into()),
        }
    }
}

#[cfg(target_os = "macos")]
fn request_native_permission(_app: &tauri::AppHandle) -> Result<bool, String> {
    mac_notifications::request()
}
#[cfg(target_os = "macos")]
fn native_permission_granted(_app: &tauri::AppHandle) -> Result<bool, String> {
    mac_notifications::granted()
}
#[cfg(target_os = "macos")]
fn show_native_notice(_app: &tauri::AppHandle, notice: &NativeNotice) -> Result<(), String> {
    mac_notifications::show(notice)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;

    fn fixture_dir(tag: &str) -> PathBuf {
        static NEXT: AtomicUsize = AtomicUsize::new(0);
        let path = std::env::temp_dir().join(format!(
            "friday-data-dir-test-{tag}-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::SeqCst)
        ));
        std::fs::create_dir_all(&path).unwrap();
        path
    }

    #[test]
    fn sync_drive_markers_are_detected_anywhere_in_path() {
        assert!(looks_like_sync_drive(std::path::Path::new(
            "/Users/a/Library/Mobile Documents/com~apple~CloudDocs/Friday"
        )));
        assert!(looks_like_sync_drive(std::path::Path::new(
            r"C:\Users\a\OneDrive\Friday"
        )));
        assert!(looks_like_sync_drive(std::path::Path::new(
            "/Users/a/Dropbox/Friday"
        )));
        assert!(!looks_like_sync_drive(std::path::Path::new(
            "/Users/a/Library/Application Support/Friday"
        )));
        assert!(!looks_like_sync_drive(std::path::Path::new("/tmp/friday")));
    }

    #[test]
    fn picked_dir_resolution_uses_marker_and_subdir_rules() {
        // 不存在的目录 → 原样（将创建）
        let missing = std::env::temp_dir().join("friday-nope-never-exists");
        assert_eq!(effective_target(&missing), missing);
        // 空目录 → 原样
        let empty = fixture_dir("empty");
        assert_eq!(effective_target(&empty), empty);
        // 已有 Friday 数据 → 原样接管
        let existing = fixture_dir("existing");
        std::fs::write(existing.join("config.yaml"), b"# friday").unwrap();
        assert_eq!(effective_target(&existing), existing);
        // 非空无关目录 → 收进其下 Friday 子目录
        let foreign = fixture_dir("foreign");
        std::fs::write(foreign.join("unrelated.txt"), b"x").unwrap();
        assert_eq!(effective_target(&foreign), foreign.join("Friday"));
        for dir in [empty, existing, foreign] {
            std::fs::remove_dir_all(dir).unwrap();
        }
    }

    #[test]
    fn pointer_roundtrip_rejects_relative_and_corrupt_payloads() {
        let dir = fixture_dir("pointer");
        let path = dir.join("Friday.launcher.json");
        assert!(read_pointer(&path).is_none()); // 不存在
        write_pointer(&path, std::path::Path::new("/tmp/friday-data")).unwrap();
        assert_eq!(
            read_pointer(&path),
            Some(PathBuf::from("/tmp/friday-data"))
        );
        std::fs::write(&path, b"{damaged").unwrap();
        assert!(read_pointer(&path).is_none());
        // 相对路径视为无效指针
        std::fs::write(&path, br#"{"dataDir":"relative/path"}"#).unwrap();
        assert!(read_pointer(&path).is_none());
        std::fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn kernel_path_lives_in_data_dir_bin() {
        let path = kernel_path(std::path::Path::new("/data"));
        assert_eq!(path.parent(), Some(std::path::Path::new("/data/bin")));
        assert!(path
            .file_name()
            .is_some_and(|n| n.to_string_lossy().starts_with("friday")));
    }

    #[test]
    fn writable_probe_passes_on_real_dir_and_fails_when_path_is_a_file() {
        let dir = fixture_dir("writable");
        assert!(ensure_writable(&dir).is_ok());
        // 路径中间被一个普通文件占住：create_dir_all 失败（如把文件路径当目录选入）
        std::fs::write(dir.join("occupied"), b"x").unwrap();
        let blocked = dir.join("occupied").join("deeper");
        assert!(ensure_writable(&blocked).is_err());
        std::fs::remove_dir_all(dir).unwrap();
    }

    fn fixture_path() -> PathBuf {
        static NEXT: AtomicUsize = AtomicUsize::new(0);
        let path = std::env::temp_dir().join(format!(
            "friday-native-test-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::SeqCst)
        ));
        std::fs::create_dir_all(&path).unwrap();
        path.join("notifications.json")
    }

    #[test]
    fn notification_command_only_accepts_current_kernel_main_origin() {
        let addr = Some("http://127.0.0.1:18080");
        assert!(trusted_notification_origin(
            "main",
            "http://127.0.0.1:18080/",
            addr
        ));
        for (label, url) in [
            ("palette", "http://127.0.0.1:18080"),
            ("main", "http://127.0.0.1:18081"),
            ("main", "http://localhost:18080"),
            ("main", "https://remote.example"),
            ("main", "tauri://localhost"),
        ] {
            assert!(!trusted_notification_origin(label, url, addr));
        }
        assert!(!trusted_notification_origin(
            "main",
            "http://127.0.0.1:18080",
            None
        ));
    }

    #[test]
    fn notice_parser_rejects_empty_and_malformed_messages() {
        assert!(
            parse_native_notice("FRIDAY_NOTICE={\"id\":\"notice-1\",\"text\":\"提醒\"}").is_some()
        );
        for line in [
            "FRIDAY_NOTICE=invalid",
            "FRIDAY_NOTICE={\"id\":\"\",\"text\":\"提醒\"}",
            "FRIDAY_NOTICE={\"id\":\"id\",\"text\":\" \"}",
            "unrelated log",
        ] {
            assert!(parse_native_notice(line).is_none());
        }
    }

    #[test]
    fn dispatch_requires_opt_in_and_permission_and_persists_only_accepted_ids() {
        let path = fixture_path();
        let state = NativeNotifications::load(path.clone());
        let notice = NativeNotice {
            id: "notice-1".into(),
            text: "private reminder".into(),
        };
        assert!(!state
            .dispatch(&notice, true, |_| panic!("must not dispatch before opt in"))
            .unwrap());
        state.enable().unwrap();
        assert!(!state
            .dispatch(&notice, false, |_| panic!(
                "must not request permission implicitly"
            ))
            .unwrap());
        assert!(state
            .dispatch(&notice, true, |_| Err("simulated OS rejection".into()))
            .is_err());
        assert!(!state.history.lock().unwrap().seen.contains(&notice.id));
        assert!(state.dispatch(&notice, true, |_| Ok(())).unwrap());
        assert!(!state
            .dispatch(&notice, true, |_| panic!("must not repeat"))
            .unwrap());
        let restarted = NativeNotifications::load(path.clone());
        assert!(!restarted
            .dispatch(&notice, true, |_| panic!("must not repeat after restart"))
            .unwrap());
        assert!(!std::fs::read_to_string(&path)
            .unwrap()
            .contains("private reminder"));
        std::fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn damaged_history_disables_native_dispatch_without_overwriting_file() {
        let path = fixture_path();
        std::fs::write(&path, b"damaged").unwrap();
        let state = NativeNotifications::load(path.clone());
        assert!(state.enable().is_err());
        let notice = NativeNotice {
            id: "notice".into(),
            text: "private".into(),
        };
        assert!(!state
            .dispatch(&notice, true, |_| panic!(
                "damaged history must fail closed"
            ))
            .unwrap());
        assert_eq!(std::fs::read(&path).unwrap(), b"damaged");
        std::fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn opt_in_save_failure_does_not_enable_delivery() {
        let path = fixture_path();
        let unavailable = path
            .parent()
            .unwrap()
            .join("missing")
            .join("notifications.json");
        let state = NativeNotifications::load(unavailable);
        assert!(state.enable().is_err());
        assert!(!state.history.lock().unwrap().enabled);
        let notice = NativeNotice {
            id: "notice".into(),
            text: "private".into(),
        };
        assert!(!state
            .dispatch(&notice, true, |_| panic!(
                "failed opt in must not enable dispatch"
            ))
            .unwrap());
        std::fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn concurrent_duplicate_messages_dispatch_once() {
        let path = fixture_path();
        let state = std::sync::Arc::new(NativeNotifications::load(path.clone()));
        state.enable().unwrap();
        let sent = std::sync::Arc::new(AtomicUsize::new(0));
        let mut workers = Vec::new();
        for _ in 0..8 {
            let state = state.clone();
            let sent = sent.clone();
            workers.push(std::thread::spawn(move || {
                let notice = NativeNotice {
                    id: "same-notice".into(),
                    text: "private".into(),
                };
                state
                    .dispatch(&notice, true, |_| {
                        sent.fetch_add(1, Ordering::SeqCst);
                        Ok(())
                    })
                    .unwrap();
            }));
        }
        for worker in workers {
            worker.join().unwrap();
        }
        assert_eq!(sent.load(Ordering::SeqCst), 1);
        std::fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn notification_status_is_read_only_and_never_prompts() {
        let path = fixture_path();
        let state = NativeNotifications::load(path.clone());
        assert!(!state
            .enabled(|| panic!("not opted in: no system permission request or query"))
            .unwrap());
        state.enable().unwrap();
        let original = std::fs::read(&path).unwrap();
        assert!(state.enabled(|| Ok(true)).unwrap());
        assert!(!state.enabled(|| Ok(false)).unwrap());
        assert_eq!(std::fs::read(&path).unwrap(), original);
        let reopened = NativeNotifications::load(path.clone());
        assert!(reopened.enabled(|| Ok(true)).unwrap());
        std::fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn exited_kernel_drops_address_before_recovery_and_does_not_show_error_on_quit() {
        let kernel = Kernel {
            addr: Mutex::new(Some("http://127.0.0.1:18080".into())),
            child: Mutex::new(None),
            exited: AtomicBool::new(false),
            quitting: AtomicBool::new(false),
            data_dir: PathBuf::new(),
            last_stderr: Mutex::new(String::new()),
            dead_reason: Mutex::new(None),
            palette_shortcut: PALETTE_SHORTCUT.parse().unwrap(),
        };
        assert!(mark_kernel_dead(&kernel, "simulated kernel failure"));
        assert!(kernel.addr.lock().unwrap().is_none());
        assert!(kernel.exited.load(Ordering::SeqCst));
        assert_eq!(
            kernel.dead_reason.lock().unwrap().as_deref(),
            Some("simulated kernel failure")
        );
        kernel.quitting.store(true, Ordering::SeqCst);
        assert!(!mark_kernel_dead(&kernel, "normal shutdown"));
    }
}
