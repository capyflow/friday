package ansi

import "fmt"

// 红色字体
func ColorRed(msg string) string {
	return fmt.Sprintf("\033[31m%s\033[0m", msg)
}

// 加粗红色字体
func ColorBlodRed(msg string) string {
	return fmt.Sprintf("\033[1;31m%s\033[0m", msg)
}

// 绿色字体
func ColorGreen(msg string) string {
	return fmt.Sprintf("\033[32m%s\033[0m", msg)
}

// 加粗绿色字体
func ColorBoldGreen(msg string) string {
	return fmt.Sprintf("\033[1;32m%s\033[0m", msg)
}

// 黄色字体
func ColorYellow(msg string) string {
	return fmt.Sprintf("\033[33m%s\033[0m", msg)
}

// 加粗黄色字体
func ColorBoldYellow(msg string) string {
	return fmt.Sprintf("\033[1;33m%s\033[0m", msg)
}

// 蓝色字体
func ColorBlue(msg string) string {
	return fmt.Sprintf("\033[34m%s\033[0m", msg)
}

// 加粗蓝色字体
func ColorBoldBlue(msg string) string {
	return fmt.Sprintf("\033[1;34m%s\033[0m", msg)
}

// 紫色字体
func ColorMagenta(msg string) string {
	return fmt.Sprintf("\033[35m%s\033[0m", msg)
}

// 加粗紫色字体
func ColorBoldMagenta(msg string) string {
	return fmt.Sprintf("\033[1;35m%s\033[0m", msg)
}

// 青色字体
func ColorCyan(msg string) string {
	return fmt.Sprintf("\033[36m%s\033[0m", msg)
}

// 加粗青色字体
func ColorBoldCyan(msg string) string {
	return fmt.Sprintf("\033[1;36m%s\033[0m", msg)
}

// 白色字体
func ColorWhite(msg string) string {
	return fmt.Sprintf("\033[37m%s\033[0m", msg)
}

// 加粗白色字体
func ColorBoldWhite(msg string) string {
	return fmt.Sprintf("\033[1;37m%s\033[0m", msg)
}

// 黑色字体
func ColorBlack(msg string) string {
	return fmt.Sprintf("\033[30m%s\033[0m", msg)
}

// 加粗黑色字体
func ColorBoldBlack(msg string) string {
	return fmt.Sprintf("\033[1;30m%s\033[0m", msg)
}

// 红色背景
func BgRed(msg string) string {
	return fmt.Sprintf("\033[41m%s\033[0m", msg)
}

// 绿色背景
func BgGreen(msg string) string {
	return fmt.Sprintf("\033[42m%s\033[0m", msg)
}

// 黄色背景
func BgYellow(msg string) string {
	return fmt.Sprintf("\033[43m%s\033[0m", msg)
}

// 蓝色背景
func BgBlue(msg string) string {
	return fmt.Sprintf("\033[44m%s\033[0m", msg)
}

// 紫色背景
func BgMagenta(msg string) string {
	return fmt.Sprintf("\033[45m%s\033[0m", msg)
}

// 青色背景
func BgCyan(msg string) string {
	return fmt.Sprintf("\033[46m%s\033[0m", msg)
}

// 白色背景
func BgWhite(msg string) string {
	return fmt.Sprintf("\033[47m%s\033[0m", msg)
}

// 黑色背景
func BgBlack(msg string) string {
	return fmt.Sprintf("\033[40m%s\033[0m", msg)
}

// 下划线
func Underline(msg string) string {
	return fmt.Sprintf("\033[4m%s\033[0m", msg)
}

// 闪烁
func Blink(msg string) string {
	return fmt.Sprintf("\033[5m%s\033[0m", msg)
}

// 反色显示
func Reverse(msg string) string {
	return fmt.Sprintf("\033[7m%s\033[0m", msg)
}

// 删除线
func Strikethrough(msg string) string {
	return fmt.Sprintf("\033[9m%s\033[0m", msg)
}

// 暗淡
func Dim(msg string) string {
	return fmt.Sprintf("\033[2m%s\033[0m", msg)
}

// 加粗
func Bold(msg string) string {
	return fmt.Sprintf("\033[1m%s\033[0m", msg)
}

// 斜体
func Italic(msg string) string {
	return fmt.Sprintf("\033[3m%s\033[0m", msg)
}

// 高亮红色（亮红色）
func ColorBrightRed(msg string) string {
	return fmt.Sprintf("\033[91m%s\033[0m", msg)
}

// 高亮绿色（亮绿色）
func ColorBrightGreen(msg string) string {
	return fmt.Sprintf("\033[92m%s\033[0m", msg)
}

// 高亮黄色（亮黄色）
func ColorBrightYellow(msg string) string {
	return fmt.Sprintf("\033[93m%s\033[0m", msg)
}

// 高亮蓝色（亮蓝色）
func ColorBrightBlue(msg string) string {
	return fmt.Sprintf("\033[94m%s\033[0m", msg)
}

// 高亮紫色（亮紫色）
func ColorBrightMagenta(msg string) string {
	return fmt.Sprintf("\033[95m%s\033[0m", msg)
}

// 高亮青色（亮青色）
func ColorBrightCyan(msg string) string {
	return fmt.Sprintf("\033[96m%s\033[0m", msg)
}

// 高亮白色（亮白色）
func ColorBrightWhite(msg string) string {
	return fmt.Sprintf("\033[97m%s\033[0m", msg)
}

// 高亮黑色（亮黑色/灰色）
func ColorBrightBlack(msg string) string {
	return fmt.Sprintf("\033[90m%s\033[0m", msg)
}
