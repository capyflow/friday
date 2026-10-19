# LogX - Go 日志框架

一个功能强大、易于使用的 Go 语言日志框架，支持多级别日志、文件轮转、彩色输出等特性。

## 特性

- 🎯 **多级别日志**：支持 DEBUG、INFO、WARN、ERROR、FATAL 五个日志级别
- 📁 **文件轮转**：当日志文件达到指定大小时自动归档
- 🎨 **彩色输出**：终端输出支持不同级别的颜色区分
- 🔄 **异步写入**：使用 goroutine 和 channel 实现异步日志写入
- 🛡️ **线程安全**：支持并发环境下的安全使用
- ⚙️ **灵活配置**：支持自定义日志目录、文件名、时间格式等
- 📤 **双重输出**：可同时或单独输出到控制台和文件
- 🌐 **全局日志器**：提供便捷的全局函数调用

## 安装

```bash
go get github.com/capyflow/allspark-go/logx
```

## 快速开始

### 使用全局日志器

```go
package main

import "github.com/capyflow/allspark-go/logx"

func main() {
    // 直接使用全局函数
    logx.Debug("这是一条调试信息")
    logx.Info("这是一条信息日志")
    logx.Warn("这是一条警告日志")
    logx.Error("这是一条错误日志")
    
    // 格式化输出
    logx.Infof("用户 %s 登录成功，ID: %d", "张三", 123)
    logx.Errorf("连接数据库失败: %v", err)
}
```

### 创建自定义日志器

```go
package main

import (
    "github.com/capyflow/allspark-go/logx"
)

func main() {
    // 创建自定义配置
    config := &logx.LogConfig{
        Level:         logx.INFO,
        LogDir:        "./logs",
        LogFileName:   "app.log",
        MaxFileSize:   5 * 1024 * 1024, // 5MB
        EnableConsole: true,
        EnableFile:    true,
        TimeFormat:    "2006-01-02 15:04:05.000",
    }
    
    // 创建日志器
    logger, err := logx.NewLogger(config)
    if err != nil {
        panic(err)
    }
    defer logger.Close()
    
    // 使用自定义日志器
    logger.Info("应用启动成功")
    logger.Errorf("处理请求失败: %v", err)
}
```

## API 参考

### 日志级别

```go
const (
    DEBUG LogLevel = iota  // 调试信息
    INFO                   // 一般信息
    WARN                   // 警告信息
    ERROR                  // 错误信息
    FATAL                  // 致命错误（会导致程序退出）
)
```

### 配置选项

```go
type LogConfig struct {
    Level         LogLevel // 日志级别
    LogDir        string   // 日志目录
    LogFileName   string   // 日志文件名
    MaxFileSize   int64    // 最大文件大小（字节）
    EnableConsole bool     // 是否输出到控制台
    EnableFile    bool     // 是否输出到文件
    TimeFormat    string   // 时间格式
}
```

### 全局函数

#### 基本日志输出

```go
// 不同级别的日志输出
logx.Debug(msg string)
logx.Info(msg string)
logx.Warn(msg string)
logx.Error(msg string)
logx.Fatal(msg string)  // 输出后程序退出

// 格式化输出
logx.Debugf(format string, args ...interface{})
logx.Infof(format string, args ...interface{})
logx.Warnf(format string, args ...interface{})
logx.Errorf(format string, args ...interface{})
logx.Fatalf(format string, args ...interface{})  // 输出后程序退出
```

#### 全局设置

```go
// 设置全局日志级别
logx.SetLevel(logx.WARN)

// 设置全局输出目标
logx.SetOutput(console bool, file bool)

// 获取/设置默认日志器
logger := logx.GetDefaultLogger()
logx.SetDefaultLogger(customLogger)
```

### Logger 实例方法

```go
// 创建日志器
logger, err := logx.NewLogger(config)

// 日志输出方法（与全局函数相同）
logger.Debug(msg)
logger.Info(msg)
logger.Warn(msg)
logger.Error(msg)
logger.Fatal(msg)

// 格式化输出
logger.Debugf(format, args...)
logger.Infof(format, args...)
// ... 其他级别

// 设置方法
logger.SetLevel(logx.INFO)
logger.SetOutput(true, false)  // 只输出到控制台

// 获取信息
size := logger.GetCurrentFileSize()

// 关闭日志器
logger.Close()
```

## 高级用法

### 文件轮转

当日志文件大小达到 `MaxFileSize` 时，会自动进行文件轮转：

```
logs/
├── app.log              # 当前日志文件
├── app.log.1640995200   # 归档文件（时间戳）
└── app.log.1640995300   # 归档文件（时间戳）
```

### 日志级别过滤

```go
// 设置日志级别为 WARN，只有 WARN、ERROR、FATAL 级别的日志会被输出
logx.SetLevel(logx.WARN)

logx.Debug("不会输出")
logx.Info("不会输出")
logx.Warn("会输出")
logx.Error("会输出")
```

### 输出控制

```go
// 只输出到控制台
logx.SetOutput(true, false)

// 只输出到文件
logx.SetOutput(false, true)

// 同时输出到控制台和文件
logx.SetOutput(true, true)
```

### 自定义时间格式

```go
config := &logx.LogConfig{
    TimeFormat: "15:04:05.000",  // 只显示时分秒毫秒
    // 或者
    TimeFormat: "2006/01/02 15:04:05",  // 自定义日期格式
}
```

## 配置示例

### 开发环境配置

```go
devConfig := &logx.LogConfig{
    Level:         logx.DEBUG,
    LogDir:        "./logs",
    LogFileName:   "dev.log",
    MaxFileSize:   10 * 1024 * 1024, // 10MB
    EnableConsole: true,
    EnableFile:    true,
    TimeFormat:    "2006-01-02 15:04:05.000",
}
```

### 生产环境配置

```go
prodConfig := &logx.LogConfig{
    Level:         logx.INFO,
    LogDir:        "/var/log/myapp",
    LogFileName:   "app.log",
    MaxFileSize:   100 * 1024 * 1024, // 100MB
    EnableConsole: false,
    EnableFile:    true,
    TimeFormat:    "2006-01-02 15:04:05",
}
```

### 调试配置（只输出到控制台）

```go
debugConfig := &logx.LogConfig{
    Level:         logx.DEBUG,
    EnableConsole: true,
    EnableFile:    false,
    TimeFormat:    "15:04:05.000",
}
```

## 输出格式

日志输出格式为：`[级别] 时间戳 消息内容`

```
[INFO] 2023-12-07 15:30:45 应用启动成功
[WARN] 2023-12-07 15:30:46 配置文件不存在，使用默认配置
[ERROR] 2023-12-07 15:30:47 连接数据库失败: connection refused
```

终端输出支持颜色：
- 🔵 DEBUG：蓝色
- 🟢 INFO：绿色
- 🟡 WARN：黄色
- 🔴 ERROR：红色
- 🟣 FATAL：紫色

## 注意事项

1. **Fatal 级别**：使用 `Fatal` 或 `Fatalf` 会在输出日志后调用 `os.Exit(1)` 退出程序
2. **文件权限**：确保程序对日志目录有写权限
3. **并发安全**：所有方法都是线程安全的，可以在多个 goroutine 中安全使用
4. **资源释放**：使用完自定义日志器后记得调用 `Close()` 方法释放资源
5. **异步写入**：日志写入是异步的，程序退出前确保所有日志都已写入

## 依赖

- `github.com/capyflow/allspark-go/ansi` - 用于终端颜色输出

## 许可证

MIT License