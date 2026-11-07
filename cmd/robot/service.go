// service 子命令：管理 Friday 内核的 launchd 常驻服务（仅 macOS）。
// `friday service install --data-dir <dir> [--port N]` 由桌面外壳在首启时调用，
// 也可以手动执行做排障；详见 internal/service 与 docs/DESKTOP.md「服务模式」。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/capyflow/friday/internal/service"
)

func runServiceCommand(args []string) {
	if runtime.GOOS != "darwin" {
		log.Fatalf("service 子命令仅支持 macOS（launchd）；其他平台用 systemd/服务管理器自行托管")
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: friday service <install|uninstall|status|start|stop|restart> [--data-dir <dir>] [--port <n>]")
		os.Exit(2)
	}
	action := args[0]
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "数据目录（默认系统配置目录下的 Friday）")
	port := fs.Int("port", 0, "服务监听端口（0 = 沿用已装服务的端口，首次安装默认 45678）")
	if err := fs.Parse(args[1:]); err != nil {
		log.Fatalf("解析参数失败: %v", err)
	}
	dir := *dataDir
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			log.Fatalf("无法定位系统配置目录: %v", err)
		}
		dir = base + "/Friday"
	}
	run := func(err error, what string) {
		if err != nil {
			log.Fatalf("%s失败: %v", what, err)
		}
	}
	switch action {
	case "install":
		run(service.Install(dir, *port), "安装服务")
		log.Printf("[服务] 已安装并启动（数据目录 %s）", dir)
	case "uninstall":
		run(service.Uninstall(), "卸载服务")
		log.Print("[服务] 已卸载（数据目录与二进制保留）")
	case "start":
		run(service.Start(), "启动服务")
		log.Print("[服务] 已启动")
	case "stop":
		run(service.Stop(), "停止服务")
		log.Print("[服务] 已停止")
	case "restart":
		run(service.Restart(), "重启服务")
		log.Print("[服务] 已重启")
	case "status":
		fmt.Println(service.Status())
	default:
		fmt.Fprintln(os.Stderr, "用法: friday service <install|uninstall|status|start|stop|restart> [--data-dir <dir>] [--port <n>]")
		os.Exit(2)
	}
}
