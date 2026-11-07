package admin

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureCertsCreatesCA 生成 CA 与叶子证书，且叶子的 SAN 覆盖本机回环地址。
func TestEnsureCertsCreatesCA(t *testing.T) {
	dir := t.TempDir()
	leafCert, leafKey, caPEM, err := EnsureCerts(dir)
	if err != nil {
		t.Fatalf("EnsureCerts: %v", err)
	}
	for _, p := range []string{leafCert, leafKey, filepath.Join(dir, caCertName)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s 应存在: %v", p, err)
		}
	}
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil {
		t.Fatalf("CA PEM 解析失败")
	}
	if _, err := x509.ParseCertificate(caBlock.Bytes); err != nil {
		t.Fatalf("CA 证书解析: %v", err)
	}
	// 叶子证书能配对加载（tls.LoadX509KeyPair 同语义）。
	if _, err := TLSConfig(leafCert, leafKey); err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	// 叶子 SAN 含 127.0.0.1 与 localhost（desiredSANs 恒有项）。
	pemBytes, _ := os.ReadFile(leafCert)
	block, _ := pem.Decode(pemBytes)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("叶子解析: %v", err)
	}
	hasLoopback := false
	for _, ip := range cert.IPAddresses {
		if ip.String() == "127.0.0.1" {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Fatalf("叶子证书应含 127.0.0.1 SAN，实际 %v", cert.IPAddresses)
	}
	if len(cert.DNSNames) == 0 {
		t.Fatalf("叶子证书应含 DNS SAN（localhost/主机名）")
	}
}

// TestEnsureCertsIdempotent 二次调用不重签（证书字节不变），私钥权限 0600。
func TestEnsureCertsIdempotent(t *testing.T) {
	dir := t.TempDir()
	c1, _, _, err := EnsureCerts(dir)
	if err != nil {
		t.Fatalf("首次: %v", err)
	}
	first, _ := os.ReadFile(c1)
	c2, _, _, err := EnsureCerts(dir)
	if err != nil {
		t.Fatalf("二次: %v", err)
	}
	second, _ := os.ReadFile(c2)
	if string(first) != string(second) {
		t.Fatalf("SAN 未变化时不应重签")
	}
	info, err := os.Stat(filepath.Join(dir, leafKeyName))
	if err != nil {
		t.Fatalf("叶子私钥应存在: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("私钥权限 = %v，应为 0600", info.Mode().Perm())
	}
}

// TestCAKeyPairConsistent 叶子证书的签发者是数据目录里的 CA。
func TestCAKeyPairConsistent(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := EnsureCerts(dir); err != nil {
		t.Fatalf("EnsureCerts: %v", err)
	}
	caPEM, _ := os.ReadFile(filepath.Join(dir, caCertName))
	leafPEM, _ := os.ReadFile(filepath.Join(dir, leafCertName))
	caBlock, _ := pem.Decode(caPEM)
	leafBlock, _ := pem.Decode(leafPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("CA 解析: %v", err)
	}
	leafCert, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatalf("叶子解析: %v", err)
	}
	if err := leafCert.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("叶子应由本目录 CA 签发: %v", err)
	}
	if !caCert.IsCA {
		t.Fatalf("CA 证书应带 IsCA")
	}
	if caCert.PublicKeyAlgorithm != x509.ECDSA {
		t.Fatalf("CA 应为 ECDSA（体积与性能友好）")
	}
}
