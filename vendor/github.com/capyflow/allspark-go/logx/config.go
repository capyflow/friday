package logx

// LogConfig 日志配置
type LogConfig struct {
	Level         LogLevel // 日志级别
	LogDir        string   // 日志目录
	LogFileName   string   // 日志文件名
	MaxFileSize   int64    // 最大文件大小（字节）
	EnableConsole bool     // 是否输出到控制台
	EnableFile    bool     // 是否输出到文件
	TimeFormat    string   // 时间格式
}

type ConfigOption func(*LogConfig)

// WithLevel 设置日志级别
func WithLevel(level LogLevel) ConfigOption {
	return func(c *LogConfig) {
		c.Level = level
	}
}

// WithLogDir 配置日志目录
func WithLogDir(logDir string) ConfigOption {
	return func(c *LogConfig) {
		c.LogDir = logDir
	}
}

// WithLogFileName 配置日志文件名
func WithLogFileName(logFileName string) ConfigOption {
	return func(c *LogConfig) {
		c.LogFileName = logFileName
	}
}

// WithMaxFileSize 配置日志文件最大大小
func WithMaxFileSize(maxFileSize int64) ConfigOption {
	return func(c *LogConfig) {
		c.MaxFileSize = maxFileSize
	}
}

// WithEnableConsole 配置是否输出到控制台
func WithEnableConsole(enableConsole bool) ConfigOption {
	return func(c *LogConfig) {
		c.EnableConsole = enableConsole
	}
}

// WithEnableFile 配置是否输出到文件
func WithEnableFile(enableFile bool) ConfigOption {
	return func(c *LogConfig) {
		c.EnableFile = enableFile
	}
}

// WithTimeFormat 配置时间格式
func WithTimeFormat(timeFormat string) ConfigOption {
	return func(c *LogConfig) {
		c.TimeFormat = timeFormat
	}
}
