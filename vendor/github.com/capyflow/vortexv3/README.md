# Vortex v3

基于 [Gin](https://github.com/gin-gonic/gin) 的服务框架，统一支持 **HTTP** 和 **WebSocket** 两种通信协议。

## 目录

- [快速开始](#快速开始)
- [HTTP](#http)
  - [路由注册](#路由注册)
  - [路由组与中间件](#路由组与中间件)
  - [JWT 鉴权](#jwt-鉴权)
  - [响应](#响应)
- [WebSocket](#websocket)
  - [建立连接](#建立连接)
  - [消息路由](#消息路由)
  - [发送消息](#发送消息)
  - [Session 管理](#session-管理)
- [配置选项](#配置选项)
- [消息协议](#消息协议)

---

## 快速开始

```go
package main

import (
    "context"
    "net/http"

    "github.com/capyflow/vortexv3"
    vpkg "github.com/capyflow/vortexv3/pkg"
    vhttp "github.com/capyflow/vortexv3/server/http"
)

func main() {
    // 1. 定义路由
    root := vhttp.NewRootGroup("/v1")
    root.AddRouter([]string{http.MethodPost}, "/hello", func(ctx *vhttp.Context) error {
        return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{
            "message": "hello vortex",
        })
    })

    // 2. 创建引擎
    engine := vortex.NewVortexEngine(
        context.Background(),
        vortex.WithPort(8080),
        vortex.WithHttpRouterRootGroup(root),
    )

    // 3. 启动
    engine.Start()
    select {} // 阻塞
}
```

---

## HTTP

### 路由注册

通过 `VortexHttpRouterGroup` 注册路由，handler 签名为 `func(*vhttp.Context) error`：

```go
root := vhttp.NewRootGroup("/v1")

// 带 JWT 鉴权的路由
root.AddRouter([]string{http.MethodPost}, "/user/info", handleUserInfo)

// 跳过 JWT 验证
root.AddRouter([]string{http.MethodPost}, "/login", handleLogin,
    vhttp.WithSkipJwtVerify(),
)

// 跳过 token 过期检查
root.AddRouter([]string{http.MethodGet}, "/refresh", handleRefresh,
    vhttp.WithSkipExpireCheck(),
)

// 静态资源路由（跳过 JWT）
root.AddStaticRouter("/static/*filepath", handleStatic)
```

### 路由组与中间件

```go
root := vhttp.NewRootGroup("/v1")

// 路由组级别的中间件会应用到组内所有路由
api := root.AddGroup("/api", authMiddleware, rateLimitMiddleware)
api.AddRouter([]string{http.MethodPost}, "/order", handleOrder)

// 路由级中间件，只影响当前路由
api.AddRouter([]string{http.MethodGet}, "/public", handlePublic,
    vhttp.WithSkipJwtVerify(),
    vhttp.WithMiddlewares(logMiddleware),
)
```

### JWT 鉴权

框架内置 JWT 中间件，Token 支持三种查找方式（按优先级）：

1. `header:Authorization:Bearer` — HTTP 标准头
2. `query:token` — URL 查询参数
3. `query:access_token` — URL 查询参数（浏览器 WebSocket 使用）

```go
engine := vortex.NewVortexEngine(ctx,
    vortex.WithJwtOption(&vpkg.JwtOption{
        SecretKey:         "your-secret-key",
        Expire:            7200,
        TokenLookUp:       []string{"header:Authorization:Bearer", "query:token"},
        SkipExpiredVerfiy: false,
    }),
)
```

JWT payload 应包含 `uid`、`sid`、`exp` 字段，解析后注入为 `VSession`：

```go
type VSession struct {
    UID string `json:"uid"` // 用户ID
    SID string `json:"sid"` // 设备/会话ID
    Exp int64  `json:"exp"` // 过期时间
    IP  string `json:"ip"`  // 客户端IP（框架自动填充）
}
```

Handler 中获取 Session：

```go
func handle(ctx *vhttp.Context) error {
    session := ctx.GetVSession()
    // session.UID, session.SID, session.IP ...
}
```

### 响应

统一响应格式为 `VStruct{Head, Body, Path}`，框架自动填充 `ec`（状态码）、`em`（消息）、`trace_id`、`ts`。

```go
func handle(ctx *vhttp.Context) error {
    // JSON 响应
    return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
        "user": "data",
    })

    // 错误响应
    return ctx.JsonResponse(vhttp.Codes.Unauthorized, vpkg.VMsgResponse{
        "error": "invalid token",
    })
}
```

内置响应码：

| 常量 | HTTP 状态码 | 说明 |
|------|------------|------|
| `Codes.Success` | 200 | 成功 |
| `Codes.BadRequest` | 400 | 错误请求 |
| `Codes.Unauthorized` | 401 | 未授权 |
| `Codes.PermissionDeny` | 403 | 权限拒绝 |
| `Codes.InternalError` | 500 | 内部错误 |

---

## WebSocket

### 建立连接

在 `WithEnableProtocol` 中启用 `WEBSOCKET` 协议，框架自动注册 `/v1/websocket` 升级路由：

```go
engine := vortex.NewVortexEngine(ctx,
    vortex.WithPort(8080),
    vortex.WithEnableProtocol([]string{vpkg.HTTP, vpkg.WEBSOCKET}),
    vortex.WithJwtOption(jwtOpt),
    vortex.WithHttpRouterRootGroup(root),
)
```

客户端连接：

```
ws://host:port/v1/websocket?token=<jwt_token>
```

连接建立后，JWT 中的 `uid`/`sid` 自动绑定为设备标识。无 JWT 时默认 `uid="guest"`。

### 消息路由

客户端消息通过 `WsRouter` 分发，分三层优先级：

```
客户端消息 → 框架内置路由（如 ping）→ 用户业务路由 → 兜底处理
```

```go
wsRouter := vws.NewWsRouter()

// 注册业务消息路由
wsRouter.On("chat.message", func(conn *vws.WsConn, msg *vpkg.VStruct) error {
    // 处理聊天消息
    body := msg.Body.(map[string]any)
    conn.Reply(&vpkg.VStruct{
        Path: "chat.ack",
        Body: "received",
    })
    return nil
})

// 兜底处理（处理未注册的 path）
wsRouter.SetDefaultHandler(func(conn *vws.WsConn, msg *vpkg.VStruct) error {
    log.Printf("未知消息: %s from %s", msg.Path, conn.Uid())
    return nil
})

engine := vortex.NewVortexEngine(ctx,
    vortex.WithWsRouter(wsRouter),
)
```

**框架内置路由**：客户端发送 `{"path":"ping"}` 自动回复 `{"path":"pong","head":{"ts":...}}`，无需用户处理。

### 发送消息

每个 handler 都能通过 `*WsConn` 直接发送消息：

```go
func(conn *vws.WsConn, msg *vpkg.VStruct) error {
    // 回复当前连接
    conn.Reply(&vpkg.VStruct{Path: "ack", Body: "ok"})

    // 推送给指定用户的所有设备
    conn.SendToUser("user-123", &vpkg.VStruct{
        Path: "notify",
        Body: "you have a new message",
    })

    // 推送给指定用户的特定设备
    conn.SendToDevice("user-123", "device-abc", &vpkg.VStruct{
        Path: "notify",
        Body: "targeted message",
    })

    // 广播给所有在线连接
    conn.Broadcast(&vpkg.VStruct{
        Path: "system.notice",
        Body: "server maintenance in 5 minutes",
    })

    return nil
}
```

也可以通过 `VortexEngine` 实例在任何地方推送（如 HTTP handler、定时任务）：

```go
engine.SendToUser("user-123", &vpkg.VStruct{Path: "order.paid", Body: orderData})
engine.Broadcast(&vpkg.VStruct{Path: "announcement", Body: "..."})
```

### Session 管理

框架提供 `SessionManager` 接口，由业务服务端实现，用于管理用户在线状态：

```go
type SessionManager interface {
    OnConnect(session *vpkg.VSession) error
    OnDisconnect(session *vpkg.VSession) error
    GetUserSessions(uid string) ([]*vpkg.VSession, error)
    GetSession(sid string) (*vpkg.VSession, error)
}
```

**示例：基于 Redis 的 Session 管理**

```go
type RedisSessionManager struct {
    rdb *redis.Client
}

func (m *RedisSessionManager) OnConnect(s *vpkg.VSession) error {
    data, _ := json.Marshal(s)
    return m.rdb.HSet(ctx, "ws:online:"+s.UID, s.SID, data).Err()
}

func (m *RedisSessionManager) OnDisconnect(s *vpkg.VSession) error {
    return m.rdb.HDel(ctx, "ws:online:"+s.UID, s.SID).Err()
}

func (m *RedisSessionManager) GetUserSessions(uid string) ([]*vpkg.VSession, error) {
    result, err := m.rdb.HGetAll(ctx, "ws:online:"+uid).Result()
    if err != nil {
        return nil, err
    }
    sessions := make([]*vpkg.VSession, 0, len(result))
    for _, raw := range result {
        var s vpkg.VSession
        json.Unmarshal([]byte(raw), &s)
        sessions = append(sessions, &s)
    }
    return sessions, nil
}

func (m *RedisSessionManager) GetSession(sid string) (*vpkg.VSession, error) {
    // 根据 sid 查询 session
}

// 注入框架
engine := vortex.NewVortexEngine(ctx,
    vortex.WithSessionManager(&RedisSessionManager{rdb: rdb}),
)
```

框架内置的查询方法（基于内存索引，不依赖 `SessionManager`）：

```go
engine.WsManager.IsUserOnline("user-123")  // 是否在线
engine.WsManager.Count()                    // 连接数（设备数）
engine.WsManager.UserCount()               // 在线用户数
engine.WsManager.KickUser("user-123", "")  // 踢用户所有设备
engine.WsManager.KickUser("user-123", "sid") // 踢指定设备
```

---

## 配置选项

| 选项 | 说明 |
|------|------|
| `WithPort(8080)` | 监听端口，默认 8080 |
| `WithGinMode(gin.ReleaseMode)` | Gin 运行模式 |
| `WithEnableProtocol([]string{vpkg.HTTP, vpkg.WEBSOCKET})` | 启用协议，默认仅 HTTP |
| `WithHttpRouterRootGroup(root)` | HTTP 路由根组 |
| `WithJwtOption(&vpkg.JwtOption{...})` | JWT 配置 |
| `WithWsRouter(router)` | WS 消息路由器 |
| `WithSessionManager(mgr)` | Session 管理器 |
| `WithWsSendMessage(handler)` | 推送成功回调 |
| `WithI18n(map)` | 国际化文案映射 |
| `WithStaticDir(dir)` | 静态文件目录 |
| `WithHttpServerOptions(opts...)` | Gin Server 选项 |

---

## 消息协议

HTTP 和 WebSocket 统一使用 `VStruct` 作为消息体：

```json
{
    "path": "order.create",
    "head": {
        "ec": 200,
        "em": "success",
        "trace_id": "uuid",
        "ts": 1234567890,
        "uid": "user-123",
        "sid": "device-abc"
    },
    "body": { "order_id": 123 }
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `path` | string | 请求路径 / 消息路由 |
| `head.ec` | int | 业务响应码 |
| `head.em` | string | 业务响应消息 |
| `head.trace_id` | string | 全链路追踪 ID（框架自动生成） |
| `head.ts` | int64 | 时间戳 |
| `head.uid` | string | 用户 ID（推送目标） |
| `head.sid` | string | 设备 ID（推送目标，为空则推送该用户所有设备） |
| `body` | any | 业务数据 |

`VHead` 本质是 `map[string]any`，可通过 `Set(key, value)` / `Get(key)` 扩展自定义字段。

---

## 项目结构

```
vortexv3/
├── vortex.go                    # 引擎：创建、路由挂载、启动
├── voretx_ws.go                 # 引擎级 WS 推送 API
├── options.go                   # 选项模式配置
├── pkg/
│   ├── protocol.go              # VStruct / VHead / 协议常量
│   ├── session.go               # VSession 会话结构
│   ├── jwt.go                   # JWT 选项与 Token 查找
│   └── request.go               # TraceId 生成
└── server/
    ├── http/
    │   ├── http_router.go       # HTTP 路由组与路由定义
    │   ├── http_context.go      # 请求上下文（包装 gin.Context）
    │   ├── http_middleware.go    # JWT 中间件
    │   └── http_code.go         # HTTP 响应码
    └── ws/
        ├── ws_manager.go        # WS 连接管理器 + SessionManager 接口
        ├── ws_conn.go           # 单条 WS 连接封装
        ├── ws_router.go         # 客户端消息路由器
        └── ws_sender.go         # MessageSender 接口
```
