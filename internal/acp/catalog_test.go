package acp

import (
	"context"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/config"
)

func TestCatalogEntriesValid(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range catalog {
		if e.ID == "" || e.Check == "" || e.InstallCmd == "" {
			t.Fatalf("目录条目字段不全: %+v", e)
		}
		if seen[e.ID] {
			t.Fatalf("目录 ID 重复: %s", e.ID)
		}
		seen[e.ID] = true
		if e.Template.Name == "" || e.Template.Command == "" {
			t.Fatalf("条目 %s 的启动模板不全", e.ID)
		}
	}
	if len(catalog) < 3 {
		t.Fatalf("目录至少应有 3 个条目，实际 %d", len(catalog))
	}
}

func TestCatalogViewAndInstallUnknown(t *testing.T) {
	m := NewManager(func() config.AcpConfig { return config.AcpConfig{} }, nil, t.TempDir()+"/t.json")
	defer m.Close()
	views := m.Catalog()
	if len(views) != len(catalog) {
		t.Fatal("目录视图数量不符")
	}
	if _, err := m.Install(context.Background(), "no-such-agent"); err == nil {
		t.Fatal("未知条目应报错")
	} else if !strings.Contains(err.Error(), "目录中没有") {
		t.Fatalf("错误信息不符: %v", err)
	}
}
