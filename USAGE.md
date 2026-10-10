# Friday 使用指南

## 本地服务

首次参照 `config.yaml.example` 创建并编辑 `config.yaml`，配置模型接口地址和模型名。
密钥可通过环境变量注入，也可在运行后的「设置」页填写。

```bash
export FRIDAY_API_KEY=sk-...
go run ./cmd/robot --config config.yaml
```

默认网页入口为 `http://127.0.0.1:8080/`；端口占用时使用 `--port 18080`。
`--host` 可覆盖配置中的 `admin.host`，默认仅监听本机；桌面模式始终使用本机地址。

管理员凭据由 `FRIDAY_ADMIN_USERNAME` / `FRIDAY_ADMIN_PASSWORD` 或
`admin.username/password` 预置，两项须一起提供。首次不存在账号文件时保留默认
`admin/admin` 兼容行为；损坏或不可读取的账号文件会停止启动并保留原路径。

## 任务、通知与恢复

- 可通过模型对话或后台任务接口创建单次、多次、循环和智能任务。
- 到期任务先保存到 `notifications.json`，成功后才推进任务计划；网页启用时可查询通知记录。
- `GET /api/notifications` 获取最新记录，`POST /api/notifications/:id/retry` 重试失败通知；两者需要登录 Cookie。
- 无网页或真实输出通道时，通知会明确记录失败。远程通道重试最多 5 次，保留已确认成功通道的去重状态。
- 任务、待办、会话及通知分别落盘；保存失败返回错误。损坏文件不会被空数据覆盖，恢复有效文件后重启。
- 模型和人设配置写回成功后才热生效；失败继续使用旧配置。清除上下文会等待同会话在途请求结束。

通知采用至少一次语义；远程已接收但确认尚未落盘时，恢复可能重复投递。
完整配置、恢复流程与边界见 [稳定性说明](docs/STABILITY.md)。

## 后端验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

## 网页开发与交互

```bash
cd web
npm ci
npm run dev
npm run check
npm run build
```

开发服务默认在 `http://localhost:5173/`，将 `/api` 代理到本机8080的 Go 服务。
构建将主页面与快捷面板分别内联到 `internal/admin/webdist/`；修改前端后须重新编译 Go。

- 聊天支持普通、计划和目标意图；计划/目标模式当前传递文字提示。
- 请求失败保留文本、模式和图片草稿；发送后图片预览保留至消息清除或页面卸载。
- 审批成功后才移除卡片，失败可重试；退出登录清理旧审批状态。
- 「通知」页展示提醒正文、投递状态和失败信息，失败通知可重新排队。
- 深色输入框内部背景透明，由外层提供玻璃背景和聚焦效果。
