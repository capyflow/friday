// Package service 管理 Friday 内核的 launchd LaunchAgent 常驻服务（macOS）。
//
// 服务化后的所有权模型：内核进程归 launchd（单实例、KeepAlive 崩溃自拉、
// 开机自启），桌面壳与手机都是它的客户端。安装 = 把自身二进制拷到
// <数据目录>/bin/friday + 写 plist + 启动；升级 = 停服务→换二进制→再启动。
// 见 docs/DESKTOP.md「服务模式」。
package service

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Label launchd 服务标识（也是 plist 文件名主体）。
const Label = "com.capyflow.friday.kernel"

// DefaultPort 服务首次安装的默认端口。不取 config admin.port 的默认值 8080——
// 本机开发环境 8080 常被 Docker 等占用（约定 45678，与桌面壳 47831 并存不冲突）。
const DefaultPort = 45678

// PlistPath LaunchAgent plist 落点（当前用户的用户域）。
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("定位用户主目录: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// InstalledBin 服务化内核二进制的固定落点（数据目录内，随 install 自我拷贝升级）。
func InstalledBin(dataDir string) string {
	return filepath.Join(dataDir, "bin", "friday")
}

// Install 安装（或升级）服务：停旧实例 → 拷贝自身二进制 → 写 plist → 启动。
// port<=0 表示沿用已装服务的端口（升级保号），首次安装取 DefaultPort。
// 目标端口已被占用时只落盘不启动（避免 launchd 崩溃循环），由用户排障后
// 手动 `friday service start`。
func Install(dataDir string, port int) error {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("解析数据目录: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位自身二进制: %w", err)
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return fmt.Errorf("解析自身二进制: %w", err)
	}
	if port <= 0 {
		if port = installedPort(); port <= 0 {
			port = DefaultPort
		}
	}

	// 先停旧实例再换二进制：升级语义要求新二进制生效；bootout 也顺带解除
	// KeepAlive 与旧 plist 的绑定（未加载时报错属正常，Stop 内部已吞）。
	_ = Stop()

	if err := copyBinary(self, InstalledBin(dataDir)); err != nil {
		return err
	}
	if err := writePlist(dataDir, port); err != nil {
		return err
	}
	if portBusy(port) {
		log.Printf("[服务] 端口 %d 已被占用（可能是手动运行的开发内核），plist 已写入但未启动；停掉占用进程后执行 `friday service start`", port)
		return nil
	}
	return Start()
}

// Uninstall 停止并移除服务（保留数据目录与二进制——数据不随服务卸载删除）。
func Uninstall() error {
	if err := Stop(); err != nil {
		return err
	}
	plist, err := PlistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 plist: %w", err)
	}
	return nil
}

// Start 加载并启动服务（幂等：已加载先 bootout 再 bootstrap + kickstart）。
// bootout 是异步生效的，紧跟着 bootstrap 可能撞上 "Input/output error"（5），
// 带退避重试几次直到 launchd 状态收敛。
func Start() error {
	plist, err := PlistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plist); err != nil {
		return fmt.Errorf("plist 不存在（先 install）: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		_, _ = launchctlOutput("bootout", guiDomain()+"/"+Label)
		if out, err := launchctlOutput("bootstrap", guiDomain(), plist); err != nil {
			lastErr = fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(out))
			continue
		}
		if out, err := launchctlOutput("kickstart", "-k", guiDomain()+"/"+Label); err != nil {
			lastErr = fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(out))
			continue
		}
		return nil
	}
	return lastErr
}

// Stop 卸载服务实例（launchd 不再拉起；plist 保留）。
func Stop() error {
	out, err := launchctlOutput("bootout", guiDomain()+"/"+Label)
	if err != nil {
		// 未加载是最常见的"失败"，按成功处理。
		if strings.Contains(out, "No such process") || strings.Contains(out, "not loaded") ||
			strings.Contains(out, "Could not find service") || strings.Contains(out, "Bootstrap failed") {
			return nil
		}
		return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// Restart = Stop + Start。
func Restart() error {
	if err := Stop(); err != nil {
		return err
	}
	return Start()
}

// Status 返回 launchctl print 的关键字段（state/pid），服务未加载给提示。
func Status() string {
	out, err := launchctlOutput("print", guiDomain()+"/"+Label)
	if err != nil {
		return "未加载（" + strings.TrimSpace(firstLine(out)) + "）"
	}
	var state, pid string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state =") {
			state = strings.TrimSpace(strings.TrimPrefix(line, "state ="))
		}
		if strings.HasPrefix(line, "pid =") {
			pid = strings.TrimSpace(strings.TrimPrefix(line, "pid ="))
		}
	}
	if pid != "" && pid != "0" {
		return fmt.Sprintf("运行中（state=%s pid=%s）", state, pid)
	}
	return fmt.Sprintf("已加载未运行（state=%s）", state)
}

// ---------- 内部 ----------

func guiDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func launchctlOutput(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return string(out), err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// copyBinary 原子拷贝（tmp + rename），确保可执行位。
func copyBinary(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("创建 bin 目录: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("读自身二进制: %w", err)
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("写临时文件: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("拷贝二进制: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("落盘二进制: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("原子替换二进制: %w", err)
	}
	return nil
}

// writePlist 生成 LaunchAgent plist（原子写）。
func writePlist(dataDir string, port int) error {
	plist, err := PlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录: %w", err)
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>--service</string>
        <string>--data-dir</string>
        <string>%s</string>
        <string>--port</string>
        <string>%d</string>
    </array>
    <!-- logx 在包 init 阶段就 mkdir ./logs，launchd 默认 cwd=/（只读）会 panic，
         必须把工作目录钉在数据目录（与桌面壳 current_dir 同一约定）。 -->
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>5</integer>
    <key>StandardOutPath</key>
    <string>%s/friday-kernel.log</string>
    <key>StandardErrorPath</key>
    <string>%s/friday-kernel.log</string>
</dict>
</plist>
`, Label, InstalledBin(dataDir), dataDir, port, dataDir, dataDir, dataDir)
	tmp := plist + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写 plist: %w", err)
	}
	if err := os.Rename(tmp, plist); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("原子替换 plist: %w", err)
	}
	return nil
}

// installedPort 从已存在的 plist 里解析出当前服务端口（升级保号用）。
func installedPort() int {
	plist, err := PlistPath()
	if err != nil {
		return 0
	}
	b, err := os.ReadFile(plist)
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.Index(s, "<string>--port</string>")
	if i < 0 {
		return 0
	}
	rest := s[i+len("<string>--port</string>"):]
	j := strings.Index(rest, "</string>")
	if j < 0 {
		return 0
	}
	var p int
	if _, err := fmt.Sscanf(strings.TrimSpace(rest[:j]), "%d", &p); err != nil {
		return 0
	}
	return p
}

// portBusy 探测回环端口是否已有监听。
func portBusy(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
