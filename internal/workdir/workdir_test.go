package workdir

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithFrom(t *testing.T) {
	ctx := context.Background()
	if got := From(ctx); got != "" {
		t.Fatalf("没挂过就该是空串: %q", got)
	}
	if got := From(nil); got != "" {
		t.Fatalf("nil ctx 不该 panic: %q", got)
	}
	with := With(ctx, "/tmp/proj")
	if From(with) != "/tmp/proj" {
		t.Fatalf("取值不对: %q", From(with))
	}
	// 空串（含只有空白）等于不挂：调用方要能靠"没指定"回落到默认目录
	if changed := With(ctx, "   "); From(changed) != "" {
		t.Fatalf("空白目录不该挂进 ctx: %q", From(changed))
	}
	if From(ctx) != "" {
		t.Fatalf("With 不该改动原 ctx: %q", From(ctx))
	}
}

func TestNormalize(t *testing.T) {
	dir := t.TempDir()

	if got, err := Normalize(""); err != nil || got != "" {
		t.Fatalf("留空就是不指定: %q %v", got, err)
	}
	if got, err := Normalize("  " + dir + "/  "); err != nil || got != dir {
		t.Fatalf("应去掉首尾空白与尾斜杠: %q %v (期望 %q)", got, err, dir)
	}

	home, err := os.UserHomeDir()
	if err == nil {
		// 主目录下那个项目多半不存在，但报错里必须已经是展开后的绝对路径——
		// 这既验了 ~ 的展开，也验了"必须存在"这条判据。
		want := filepath.Join(home, "friday-workdir-不存在的项目")
		if _, err := Normalize("~/friday-workdir-不存在的项目"); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("~ 应展开成主目录下的绝对路径: %v（期望含 %q）", err, want)
		}
	}

	// 相对路径按进程工作目录解析成绝对路径
	if wd, err := os.Getwd(); err == nil {
		if got, err := Normalize("."); err != nil || got != filepath.Clean(wd) {
			t.Fatalf("相对路径应解析成绝对路径: %q %v（期望 %q）", got, err, wd)
		}
	}

	missing := filepath.Join(dir, "没这个目录")
	if _, err := Normalize(missing); err == nil || !strings.Contains(err.Error(), "重选") {
		t.Fatalf("目录不存在应报错并给下一步: %v", err)
	}

	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Normalize(file); err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("指到文件应拒绝: %v", err)
	}
}
