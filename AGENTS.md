# AGENTS — AI 助手 / 新成员工作须知

## ⚠️ macOS 打包与发布（每次打包前必读）

**任何打包动作之前，必须先读
[desktop/README.md「macOS 正确打包流程」](desktop/README.md#macos-正确打包流程2026-11-09-起)，
不要凭记忆打包。** 核心要求（违者包不能用）：

1. **只能用脚本打包，不能手搓产物**：日常发版 =
   `desktop/scripts/build-release.sh --version vX.Y.Z --skip-icon`（ad-hoc 完整签名，
   Tauri 封 DMG 前签 sidecar 与整个 .app）。裸 `cargo build` / 未封装签名的产物
   缺资源签名，装上会报「已损坏」，禁止分发。
2. **产物必须过验收**：脚本末尾自动跑 `desktop/scripts/verify-macos.sh`
   （深度签名校验 + DMG 校验 + 内置 app 逐文件比对），失败即非零退出，**不得上传**。
3. **发布说明必须写明签名状态**：当前 GitHub Release 分发的是 ad-hoc 包（非 Apple
   公证），下载后首次打开需在系统设置放行或
   `xattr -dr com.apple.quarantine /Applications/Friday.app`，发布说明要带这句。
4. **正式分发（Developer ID + Apple 公证）是升级路径不是默认路径**：只有当本机
   `security find-identity -v -p codesigning` 列出有效 Developer ID 证书且公证凭据
   （App Store Connect API key 或 Apple ID 应用专用密码）就绪时，才用
   `--macos-release`；凭据缺失时脚本会拒绝执行，这是有意设计，不要绕过、
   也不要把缺证书当成打包失败。
5. 替换已发布资产用 `gh release upload --clobber`，同时更新 `.dmg.sha256`，
   发布说明标明「重新打包」。

## 发版速查（细节以 desktop/README.md 为准）

发版 = 打纯 `vX.Y.Z` tag（**不要** `desktop-v` 前缀，tauri.conf 按非 semver 解析会报错）
→ `desktop/scripts/build-release.sh --version vX.Y.Z --skip-icon`
→ `android/build.sh`
→ `gh release create vX.Y.Z` 带三个资产：dmg + `.dmg.sha256` + apk。

## 其他约定

- 提交按主题拆分，当天变更写 `changelog/<yymmdd>.md`（样例见 changelog/261109.md）。
- `.mimosa/` 与 `.workbuddy/` 不入库；仓库根的 `auth-key` 是内核 JWT 密钥，永不入库。
- 本机开发一律 `go run ./cmd/robot --port 45678`（8080 会撞 Docker 容器）。
- 切分支 / 删分支前先 `git worktree list`。
