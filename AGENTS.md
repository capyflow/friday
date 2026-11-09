# AGENTS — AI 助手 / 新成员工作须知

## ⚠️ macOS 打包与发布（每次打包前必读）

**任何打包动作之前，必须先读
[desktop/README.md「macOS 正确打包流程」](desktop/README.md#macos-正确打包流程2026-11-09-起)，
不要凭记忆打包。** 核心要求（违者包不能用）：

1. **正式分发包必须使用 Developer ID Application 证书签名 + Apple 公证**。
   构建命令：`desktop/scripts/build-release.sh --version vX.Y.Z --skip-icon --macos-release`。
   缺少 Developer ID 身份或公证凭据时脚本会拒绝执行——这是有意设计，不要绕过。
2. **禁止把 ad-hoc 测试包发布到 GitHub Release**。ad-hoc 签名（`codesign -dv` 显示
   `Signature=adhoc`）只保证完整性，浏览器下载会被 Gatekeeper 拦截；
   `TeamIdentifier=not set` 就是没走正式签名。
3. 打包前先确认证书：`security find-identity -v -p codesigning` 必须列出有效的
   `Developer ID Application: …` 身份。只有过期证书或 0 个身份时只能出本机测试包，
   不得发布、不得标「已公证」。
4. 产物必须通过 `desktop/scripts/verify-macos.sh` 验收（深度签名校验 + DMG 校验 +
   stapler 票据 + spctl），脚本失败即非零退出，**不得上传**。

## 发版速查（细节以 desktop/README.md 为准）

发版 = 打纯 `vX.Y.Z` tag（**不要** `desktop-v` 前缀，tauri.conf 按非 semver 解析会报错）
→ `desktop/scripts/build-release.sh --version vX.Y.Z --skip-icon --macos-release`
→ `android/build.sh`
→ `gh release create vX.Y.Z` 带三个资产：dmg + `.dmg.sha256` + apk。
替换已发布资产用 `gh release upload --clobber`。

## 其他约定

- 提交按主题拆分，当天变更写 `changelog/<yymmdd>.md`（样例见 changelog/261109.md）。
- `.mimosa/` 与 `.workbuddy/` 不入库；仓库根的 `auth-key` 是内核 JWT 密钥，永不入库。
- 本机开发一律 `go run ./cmd/robot --port 45678`（8080 会撞 Docker 容器）。
- 切分支 / 删分支前先 `git worktree list`。
