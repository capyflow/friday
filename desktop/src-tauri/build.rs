fn main() {
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "kernel_state",
            "enable_notifications",
            "notifications_enabled",
            "pick_data_dir_dialog",
            "apply_data_dir",
            "open_url",
        ]),
    ))
    .expect("无法生成桌面权限清单");
}
