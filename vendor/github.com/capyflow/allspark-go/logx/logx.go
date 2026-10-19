package logx

import (
	"fmt"
	"github.com/capyflow/allspark-go/ansi"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var config = &LogConfig{}

// LogLevel 日志级别
type LogLevel int

const (
	DEBUG LogLevel = iota
	INFO
	WARN
	ERROR
	FATAL
)

// String 返回日志级别的字符串表示
func (l LogLevel) String() string {
	switch l {
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO"
	case WARN:
		return "WARN"
	case ERROR:
		return "ERROR"
	case FATAL:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// DefaultConfig 默认配置
func defaultConfig() *LogConfig {
	return &LogConfig{
		Level:         DEBUG,
		LogDir:        "./logs",
		LogFileName:   "stdout.log",
		MaxFileSize:   10 * 1024 * 1024, // 10MB
		EnableConsole: true,
		EnableFile:    true,
		TimeFormat:    "2006-01-02 15:04:05",
	}
}

// Logger 日志器
type Logger struct {
	config      *LogConfig
	file        *os.File
	fileMu      sync.Mutex
	currentSize int64
	msg         chan []byte // 异步接收消息打印
}

// NewLogger 创建新的日志器
func NewLogger(config *LogConfig) (*Logger, error) {
	if config == nil {
		config = defaultConfig()
	}

	logger := &Logger{
		config: config,
		msg:    make(chan []byte, 128),
	}

	if config.EnableFile {
		if err := logger.initLogFile(); err != nil {
			return nil, err
		}
	}

	go logger.runWriteWork()

	return logger, nil
}

func (l *Logger) runWriteWork() {
	for msg := range l.msg {
		// 输出到控制台
		mseeage := string(msg) + "\n"
		if l.config.EnableConsole {
			fmt.Print(mseeage)
		}

		// 输出到文件
		if l.config.EnableFile && l.file != nil {
			l.fileMu.Lock()
			n, err := l.file.WriteString(mseeage)
			if err == nil {
				l.currentSize += int64(n)
				l.file.Sync() // 强制刷新到磁盘
			}
			l.fileMu.Unlock()

			// 检查是否需要轮转
			if l.currentSize >= l.config.MaxFileSize {
				if err := l.rotateFile(); err != nil {
					fmt.Printf("日志文件轮转失败: %v\n", err)
				}
			}
		}
	}
}

// initLogFile 初始化日志文件
func (l *Logger) initLogFile() error {
	// 创建日志目录
	if err := os.MkdirAll(l.config.LogDir, 0755); err != nil {
		return fmt.Errorf("创建日志目录失败: %v", err)
	}

	logPath := filepath.Join(l.config.LogDir, l.config.LogFileName)

	// 检查文件是否存在，获取当前大小
	if info, err := os.Stat(logPath); err == nil {
		l.currentSize = info.Size()
	}

	// 打开或创建日志文件
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %v", err)
	}

	l.file = file
	return nil
}

// rotateFile 轮转日志文件
func (l *Logger) rotateFile() error {
	l.fileMu.Lock()
	defer l.fileMu.Unlock()

	if l.file != nil {
		l.file.Close()
	}

	// 生成归档文件名
	now := time.Now().Unix()
	logPath := filepath.Join(l.config.LogDir, l.config.LogFileName)
	archivePath := fmt.Sprintf("%s.%d", logPath, now)

	err := os.Rename(logPath, archivePath)
	if nil != err {
		return fmt.Errorf("归档日志文件失败: %v", err)
	}

	// 重新初始化日志文件
	l.currentSize = 0
	return l.initLogFile()
}

// writeLog 写入日志
func (l *Logger) writeLog(level LogLevel, msg string) {
	if level < l.config.Level {
		return
	}

	// 格式化日志消息
	timestamp := time.Now().Format(l.config.TimeFormat)
	logMsg := fmt.Sprintf("[%s] %s %s",
		level.String(), timestamp, msg)

	//加上终端的颜色信息
	switch level {
	case DEBUG:
		logMsg = ansi.ColorBoldBlue(logMsg)
	case INFO:
		logMsg = ansi.ColorBoldGreen(logMsg)
	case WARN:
		logMsg = ansi.ColorBoldYellow(logMsg)
	case ERROR:
		logMsg = ansi.ColorBlodRed(logMsg)
	case FATAL:
		logMsg = ansi.ColorBoldMagenta(logMsg)
	}
	l.msg <- []byte(logMsg)
}

// Debug 输出调试日志
func (l *Logger) Debug(msg string) {
	l.writeLog(DEBUG, msg)
}

// Debugf 格式化输出调试日志
func (l *Logger) Debugf(format string, args ...interface{}) {
	l.writeLog(DEBUG, fmt.Sprintf(format, args...))
}

// Info 输出信息日志
func (l *Logger) Info(msg string) {
	l.writeLog(INFO, msg)
}

// Infof 格式化输出信息日志
func (l *Logger) Infof(format string, args ...interface{}) {
	l.writeLog(INFO, fmt.Sprintf(format, args...))
}

// Warn 输出警告日志
func (l *Logger) Warn(msg string) {
	l.writeLog(WARN, msg)
}

// Warnf 格式化输出警告日志
func (l *Logger) Warnf(format string, args ...interface{}) {
	l.writeLog(WARN, fmt.Sprintf(format, args...))
}

// Error 输出错误日志
func (l *Logger) Error(msg string) {
	l.writeLog(ERROR, msg)
}

// Errorf 格式化输出错误日志
func (l *Logger) Errorf(format string, args ...interface{}) {
	l.writeLog(ERROR, fmt.Sprintf(format, args...))
}

// Fatal 输出致命错误日志并退出程序
func (l *Logger) Fatal(msg string) {
	l.writeLog(FATAL, msg)
	os.Exit(1)
}

// Fatalf 格式化输出致命错误日志并退出程序
func (l *Logger) Fatalf(format string, args ...interface{}) {
	l.writeLog(FATAL, fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Close 关闭日志器
func (l *Logger) Close() error {
	close(l.msg)
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// SetLevel 设置日志级别
func (l *Logger) SetLevel(level LogLevel) {
	l.config.Level = level
}

// SetOutput 设置输出目标
func (l *Logger) SetOutput(console, file bool) {
	l.config.EnableConsole = console
	l.config.EnableFile = file
}

// GetCurrentFileSize 获取当前日志文件大小
func (l *Logger) GetCurrentFileSize() int64 {
	return l.currentSize
}

// 全局默认日志器
var logger *Logger
var once sync.Once

// init 初始化默认日志器
func init() {
	once.Do(func() {
		var err error
		config = defaultConfig()
		logger, err = NewLogger(config)
		if err != nil {
			panic(fmt.Sprintf("初始化默认日志器失败: %v", err))
		}
	})
}

// 全局函数，使用默认日志器

// Debug 输出调试日志
func Debug(msg string) {
	logger.Debug(msg)
}

// Debugf 格式化输出调试日志
func Debugf(format string, args ...interface{}) {
	logger.Debugf(format, args...)
}

// Info 输出信息日志
func Info(msg string) {
	logger.Info(msg)
}

// Infof 格式化输出信息日志
func Infof(format string, args ...interface{}) {
	logger.Infof(format, args...)
}

// Warn 输出警告日志
func Warn(msg string) {
	logger.Warn(msg)
}

// Warnf 格式化输出警告日志
func Warnf(format string, args ...interface{}) {
	logger.Warnf(format, args...)
}

// Error 输出错误日志
func Error(msg string) {
	logger.Error(msg)
}

// Errorf 格式化输出错误日志
func Errorf(format string, args ...interface{}) {
	logger.Errorf(format, args...)
}

// Fatal 输出致命错误日志并退出程序
func Fatal(msg string) {
	logger.Fatal(msg)
}

// Fatalf 格式化输出致命错误日志并退出程序
func Fatalf(format string, args ...interface{}) {
	logger.Fatalf(format, args...)
}

// ResetLogger 重置日志器
func ResetLogger() {
	if logger != nil {
		_ = logger.Close()
	}
	var err error
	logger, err = NewLogger(config)
	if err != nil {
		panic(fmt.Sprintf("初始化默认日志器失败: %v", err))
	}
}
