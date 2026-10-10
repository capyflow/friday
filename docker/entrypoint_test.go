package docker

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 运行实际入口逻辑，用模拟 robot 捕获参数，不启动服务。
func TestEntrypointUsesMatchingAdminAddress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Docker 入口脚本需要 POSIX sh")
	}
	entrypoint, err := os.ReadFile("entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../config.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, host, port string
		existing         bool
	}{
		{name: "fresh_defaults"},
		{name: "existing_config", existing: true},
		{name: "custom_address", existing: true, host: "0.0.0.0", port: "9090"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "stored config.yaml")
			examplePath := filepath.Join(dir, "config.yaml.example")
			argsPath := filepath.Join(dir, "robot-args")
			robotPath := filepath.Join(dir, "robot")
			scriptPath := filepath.Join(dir, "entrypoint.sh")
			if err := os.WriteFile(examplePath, example, 0o600); err != nil {
				t.Fatal(err)
			}
			previousConfig := []byte("admin:\n  host: 127.0.0.1\n  port: 8090\n")
			if tc.existing {
				if err := os.WriteFile(configPath, previousConfig, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(robotPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ROBOT_ARGS_FILE\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			// 只替换镜像内的固定文件路径，保留所有入口参数和播种逻辑。
			script := strings.ReplaceAll(string(entrypoint), "/usr/local/bin/robot", robotPath)
			script = strings.ReplaceAll(script, "/usr/share/friday/config.yaml.example", examplePath)
			if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", scriptPath)
			for _, env := range os.Environ() {
				key, _, _ := strings.Cut(env, "=")
				switch key {
				case "CONFIG_PATH", "ADMIN_HOST", "ADMIN_PORT", "ROBOT_ARGS_FILE":
					continue
				}
				cmd.Env = append(cmd.Env, env)
			}
			cmd.Env = append(cmd.Env, "CONFIG_PATH="+configPath, "ROBOT_ARGS_FILE="+argsPath,
				"ADMIN_HOST="+tc.host, "ADMIN_PORT="+tc.port)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("入口执行失败: %v\n%s", err, output)
			}
			host, port := tc.host, tc.port
			if host == "" {
				host = "0.0.0.0"
			}
			if port == "" {
				port = "8080"
			}
			args, err := os.ReadFile(argsPath)
			want := "-config\n" + configPath + "\n-host\n" + host + "\n-port\n" + port + "\n"
			if err != nil || string(args) != want {
				t.Fatalf("robot 参数与容器地址不一致，got=%q want=%q err=%v", args, want, err)
			}
			config, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			wantConfig := example
			if tc.existing {
				wantConfig = previousConfig
			}
			if !bytes.Equal(config, wantConfig) {
				t.Fatal("首次应播种示例配置，升级应保留已有配置")
			}
		})
	}
}

func TestEntrypointRejectsInvalidPortBeforeStarting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Docker 入口脚本需要 POSIX sh")
	}
	for _, port := range []string{"0", "-1", "http", "65536", "999999999999999999999"} {
		t.Run(port, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			cmd := exec.Command("sh", "entrypoint.sh")
			cmd.Env = append(os.Environ(), "ADMIN_PORT="+port, "CONFIG_PATH="+configPath)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "ADMIN_PORT 必须是") {
				t.Fatalf("无效端口应直接拒绝，err=%v output=%s", err, output)
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatalf("无效端口不能播种或运行服务，err=%v", err)
			}
		})
	}
}
