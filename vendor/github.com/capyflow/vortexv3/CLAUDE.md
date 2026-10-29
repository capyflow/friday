# CLAUDE.md

本文件为 Claude Code (claude.ai/code) 提供在本仓库中工作时的指导。

## 项目概述

Vortex 是一个基于 Gin 构建的 Go Web 框架，提供统一的 HTTP 和 WebSocket 支持，并使用一致的消息协议 (`VStruct`)。

## 构建与测试命令

```bash
# 运行所有测试
go test ./...

# 以详细模式运行测试
go test -v ./...

# 运行特定测试
go test -run TestName ./...

# 运行特定包的测试
go test -v ./server/ws/

# 整理依赖
go mod tidy

# 构建（本仓库为库，无 main 包）
go build ./...
```

## 架构

### 核心消息协议

所有通信均使用 `VStruct` (`pkg/protocol.go`) 作为统一的消息格式：

```go
type VStruct struct {
    Body any    `json:"body"` // 业务负载
    Head VHead  `json:"head"` // 请求头（带类型化访问器的 map[string]any）
    Path string `json:"path"` // 路由/命令标识符
}
```

`VHead` 提供类型化访问器：`SetUID/GetUID`、`SetSID/GetSID`、`SetEc/GetEc`、`SetEm/GetEm`、`SetTraceId/GetTraceId`、`SetTs/GetTs`。

### WebSocket 架构

- **WsManager** (`server/ws/ws_manager.go`): 管理所有连接，采用双重索引：
  - `conns`: sid -> WsConn（设备级查找）
  - `users`: uid -> sids 集合（用户级查找）
- **WsConn** (`server/ws/ws_conn.go`): 单个连接，包含 `writePump` 和 `readPump` 协程
- **消息路由**: Dispatch 使用 `Path` 字段进行路由：
  - `user.kick` - 关闭指定 uid/sid 的连接
  - `msg.broadcast` - 发送给所有连接
  - 其他路径 - 发送给指定 uid/sid

### HTTP 架构

- **Context** (`server/http/http_context.go`): 包装 gin.Context，提供 `JsonResponse`、`SSEResponse`、`StreamResponse`
- **Router** (`server/http/http_router.go`): 层级路由组，支持中间件链式调用
- **Middleware** (`server/http/http_middleware.go`): 通过 `JwtMiddleWareFunc` 进行 JWT 认证

### 引擎初始化

```go
v := vortex.NewVortexEngine(
    context.Background(),
    vortex.WithPort(8080),
    vortex.WithEnableProtocol([]string{vpkg.HTTP, vpkg.WEBSOCKET}),
    vortex.WithHttpRouterRootGroup(routers),
    vortex.WithJwtOption(&vpkg.JwtOption{SecretKey: "..."}),
)
v.Start()
```

### 关键包

- `pkg/` - 协议常量（HTTP、WEBSOCKET、GRPC）、VStruct/VHead 定义、JWT 选项、会话
- `server/http/` - HTTP 服务器：路由组、上下文包装器、响应码、中间件
- `server/ws/` - WebSocket 服务器：连接管理器和单个连接

## WebSocket 推送 API

通过引擎向客户端推送消息：

```go
// 推送给指定用户的所有设备
v.SendToUser("user-123", &vpkg.VStruct{
    Path: "notify.order",
    Body: data,
})

// 推送给指定用户的特定设备
v.SendToDevice("user-123", "device-456", &vpkg.VStruct{
    Path: "notify.order",
    Body: data,
})

// 广播给所有在线连接
v.Broadcast(&vpkg.VStruct{
    Body: data,
})

// 或直接通过 WsManager 使用
v.WsManager.Dispatch(&vpkg.VStruct{
    Path: "msg.broadcast",
    Body: data,
})
```

## 路由配置

```go
root := vhttp.NewRootGroup("/v1")
root.AddRouter([]string{http.MethodPost}, "/users", handler)
root.AddGroup("/admin").AddRouter(...)

// 路由选项
vhttp.WithSkipJwtVerify()   // 跳过 JWT
vhttp.WithSkipExpireCheck() // 跳过令牌过期检查
vhttp.WithConsoleRouter()   // 控制台/内部路由
vhttp.WithMiddlewares(mw)   // 添加中间件
```

## JWT 令牌查找

令牌按以下顺序从 header、query、cookie 中查找。在 `JwtOption.TokenLookUp` 中的格式：
- `"header:Authorization:Bearer"` - 请求头，去除前缀
- `"query:token"` - 查询参数
- `"cookie:token"` - Cookie 值


## 开发规范

### 注释

- 需要给每一个新写的函数添加注释，注释使用简单的语言描述函数的大概功能。
- 在重要逻辑出添加注释解释逻辑思路。
- 注释要求精炼，简短，表意明确，除函数名和参数外，其他信息必须为中文。

### 日志

- 日志用于后续问题排查，打印日志的要求是
  - 在报错的地方打印日志，在重要逻辑处打印日志，便于后续的调试。
  - 日志打印需要遵循规范，一般按照调用链顺序打印日志，例如: 结构体名|方法名|报错方法名|Error|错误信息|业务参数
  > 日志中如果是错误日志的话，需要在日志中打印 Error,其他类型的日志忽略 Error

### 编码规范

- 函数命名：
  - 使用驼峰命名，函数名追求简单，要求描述清除函数的功能，避免出现函数命名跟内部逻辑不匹配的问题。
- 避免过度抽象，非必要不使用接口。
- 避免过度封装，一段逻辑只有在3处以上复用的时候，才可以封装成一个函数，避免封装过多的 helper。

### 关于测试

- 编写测试用例的时候需要考虑是否有测试的必要，并非每次编写新的函数都需要写测试函数。
- 编写测试函数的时候，尽量使用已经存在的文件，避免创建过多的测试文件，编写测试用例尽可能在原有测试用例的基础上修改，如果不满足的话，在写新的测试用例。
- 编写测试代码时，需要确保功能点已经全部修改完毕，然后进行测试，避免频繁测试影响效率。
- 测试框架统一使用 goconvey

### 开发流程

- 代码修改之后，将修改点同步到changelog文件中，参考changelog文件的格式，将最近的changelog写在文档前方，changlog 中以天为单位，同一天的代码多次修改可以合并成一条 changelog。
  - changelog 的格式参考：
    - 时间：YYYY-MM-DD
      标题：xxx
      修改内容：
      - xxx
      - xxx
  - 编写changelog的时候，并非无脑将变更追加到每日修改中，需要对当天的修改进行合并之后，重新总结出一个当天的changlog。
            
## 关于代码review的关注点

- 代码review关注分页的处理风险(丢页、漏页、分页不稳定)。禁止使用页号、内存数组游标作为分页依据。使用refer_id、refer_msg、offset、limit这些。
- 数据竞争风险、并发锁管理检查
- 接口安全是否内部接口(内网鉴权)、外部接口token校验
- 同步列表不能和分页逻辑不能混用
- 方法、结构体有没有注释
- 内存缓存是否考虑多台机器之间的同步问题
- 系统处理的瓶颈点

## 注意事项

- 不要擅自写删除操作的代码，必须要经过允许。