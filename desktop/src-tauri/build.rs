fn main() {
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "kernel_state",
            "enable_notifications",
            "notifications_enabled",
        ]),
    ))
    .expect("无法生成桌面权限清单");
}
