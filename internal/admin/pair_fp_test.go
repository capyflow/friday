package admin

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFingerprintPEM 指纹：64 位 hex、同一证书两次一致、文件缺失返回空串。
func TestFingerprintPEM(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := EnsureCerts(dir); err != nil {
		t.Fatalf("EnsureCerts: %v", err)
	}
	fp := FingerprintPEM(filepath.Join(dir, leafCertName))
	if len(fp) != 64 {
		t.Fatalf("指纹长度 = %d，应为 64 hex", len(fp))
	}
	for _, c := range fp {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("指纹含非小写 hex 字符: %q", fp)
		}
	}
	if again := FingerprintPEM(filepath.Join(dir, leafCertName)); again != fp {
		t.Fatalf("同一证书两次指纹应一致: %s vs %s", fp, again)
	}
	if got := FingerprintPEM(filepath.Join(dir, "nope.crt")); got != "" {
		t.Fatalf("缺失文件应返回空串，实际 %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.crt"), []byte("not a pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FingerprintPEM(filepath.Join(dir, "junk.crt")); got != "" {
		t.Fatalf("非 PEM 文件应返回空串，实际 %q", got)
	}
}
