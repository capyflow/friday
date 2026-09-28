// Friday 桌面外壳（docs/DESKTOP.md）：拉起 Go 内核 sidecar、解析 FRIDAY_ADDR
// 并导航主窗口、全局快捷键唤起 /palette 弹窗、退出时向内核 stdin 写 /quit。
// 壳保持无业务：大脑/能力全部在 Go 内核，两侧经本地 HTTP 与 stdin 通信。

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use std::time::Duration;

use tauri::{Emitter, Manager, RunEvent, WebviewUrl, WebviewWindowBuilder, WindowEvent};
use tauri_plugin_global_shortcut::{GlobalShortcutExt, Shortcut, ShortcutState};
use tauri_plugin_shell::process::{CommandChild, CommandEvent};
use tauri_plugin_shell::ShellExt;

/// 全局快捷键（Spotlight 风格弹窗），想换键改这一行（格式见 tauri-plugin-global-shortcut）。
const PALETTE_SHORTCUT: &str = "Alt+Space";
/// 退出时等内核优雅退出的时长，超时 kill 兜底。
const QUIT_GRACE: Duration = Duration::from_secs(3);
/// 内核数据目录名（系统配置目录下）。
const DATA_DIR_NAME: &str = "Friday";

/// 内核进程状态：服务地址、子进程句柄、退出标记、数据目录、最后一条 stderr 行。
struct Kernel {
    addr: Mutex<Option<String>>,
    child: Mutex<Option<CommandChild>>,
    exited: AtomicBool,
    data_dir: PathBuf,
    last_stderr: Mutex<String>,
    dead_reason: Mutex<Option<String>>,
    palette_shortcut: Shortcut,
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
            // 数据目录 = 系统配置目录/Friday（与内核 --desktop 缺省、docs/DESKTOP.md 一致）。
            // 注意不要用 app_config_dir()：v2 会再拼一层 identifier，产生 com.capyflow.friday/Friday。
            let data_dir = app
                .path()
                .config_dir()
                .expect("无法定位系统配置目录")
                .join(DATA_DIR_NAME);
            std::fs::create_dir_all(&data_dir).expect("创建数据目录失败");
            app.manage(Kernel {
                addr: Mutex::new(None),
                child: Mutex::new(None),
                exited: AtomicBool::new(false),
                data_dir,
                last_stderr: Mutex::new(String::new()),
                dead_reason: Mutex::new(None),
                palette_shortcut,
            });
            app.global_shortcut().register(palette_shortcut)?;
            spawn_kernel(app.handle().clone());
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![kernel_state])
        .build(tauri::generate_context!())
        .expect("构建 Tauri 应用失败")
        .run(|app, event| {
            if let RunEvent::Exit = event {
                quit_kernel(app);
            }
        });
}

/// 拉起 Go 内核 sidecar 并在后台线程解析其 stdout。
fn spawn_kernel(app: tauri::AppHandle) {
    let data_dir = app.state::<Kernel>().data_dir.clone();
    eprintln!("[外壳] 数据目录：{}", data_dir.display());
    seed_config(&app, &data_dir);
    let envs = read_env_file(&data_dir.join("friday.env"));

    let data_arg = data_dir.to_string_lossy().to_string();
    let (mut rx, child) = app
        .shell()
        .sidecar("friday")
        .expect("定位 sidecar 失败（binaries/friday-<triple> 未随包分发）")
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
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(path) {
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
fn kernel_dead(app: &tauri::AppHandle, reason: String) {
    let kernel = app.state::<Kernel>();
    kernel.exited.store(true, Ordering::SeqCst);
    *kernel.dead_reason.lock().unwrap() = Some(reason.clone());
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
            let url: tauri::Url = format!("{addr}/palette")
                .parse()
                .expect("内核地址非法");
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
    eprintln!("[外壳] 内核未在 {}s 内退出，kill 兜底", QUIT_GRACE.as_secs());
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
        Ok(_) => eprintln!("[外壳] 已播种配置：{} ← {}", dst.display(), resource.display()),
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
