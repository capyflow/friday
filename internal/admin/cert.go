// TLS 局域网门的证书管理：数据目录 tls/ 下自签 CA + 叶子证书。
//
// 信任模型（mkcert 同款）：CA 只在本机生成一次（私钥不出数据目录），手机端
// 首次连接时下载 /friday-ca.crt 安装并信任，之后 https 直连不再有告警，且
// PWA 的 secure context 要求（加主屏/剪贴板等）全部满足。叶子证书按 SAN 集
// 变化重签——网卡 IP 变了（换 Wi-Fi/插拔网线）自动跟进，无需人工干预。
package admin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	caCertName   = "ca.crt"
	caKeyName    = "ca.key"
	leafCertName = "leaf.crt"
	leafKeyName  = "leaf.key"
	// leafTTL 叶子证书有效期。公共信任链 Apple 上限 398 天，私有 CA 不受限，
	// 但取 825 天对齐 Safari 宽松口径，到期随 SAN 变化或手动删除重签。
	leafTTL = 825 * 24 * time.Hour
	caTTL   = 10 * 365 * 24 * time.Hour
)

// EnsureCerts 确保 dir 下有 CA 与叶子证书，返回 (leafCert, leafKey, caPEM, err)。
// 叶子证书的 SAN 与当前机器不符（IP/主机名变化）时自动重签。
func EnsureCerts(dir string) (string, string, []byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", nil, fmt.Errorf("创建 tls 目录: %w", err)
	}
	caCertPath := filepath.Join(dir, caCertName)
	caKeyPath := filepath.Join(dir, caKeyName)
	leafCertPath := filepath.Join(dir, leafCertName)
	leafKeyPath := filepath.Join(dir, leafKeyName)

	caCert, caKey, err := loadOrCreateCA(caCertPath, caKeyPath)
	if err != nil {
		return "", "", nil, err
	}
	caPEM := certPEMBytes(caCert)

	dns, ips := desiredSANs()
	if ok, _ := leafUpToDate(leafCertPath, dns, ips); ok {
		return leafCertPath, leafKeyPath, caPEM, nil
	}
	if err := writeLeaf(leafCertPath, leafKeyPath, caCert, caKey, dns, ips); err != nil {
		return "", "", nil, err
	}
	return leafCertPath, leafKeyPath, caPEM, nil
}

// TLSConfig 组装 HTTPS 监听用的 tls.Config（叶子证书由 EnsureCerts 落盘）。
func TLSConfig(certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("加载 TLS 证书: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// LANIPv4 供手机直连的局域网 IPv4（配对链接用）；没有返回空串。
// 启发式要避开三类干扰源：lo0 上 VPN 工具挂的 /32 别名、utun 隧道地址、
// bridge/docker 虚拟网段——物理网卡（en*）永远优先。
func LANIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var fallback string
	for _, iff := range ifaces {
		if iff.Flags&net.FlagUp == 0 || iff.Flags&net.FlagRunning == 0 || iff.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		name := iff.Name
		if strings.HasPrefix(name, "lo") || strings.HasPrefix(name, "utun") ||
			strings.HasPrefix(name, "bridge") || strings.HasPrefix(name, "awdl") ||
			strings.HasPrefix(name, "llw") || strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "veth") {
			continue
		}
		addrs, err := iff.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if v4 := ipn.IP.To4(); v4 != nil {
					if strings.HasPrefix(name, "en") {
						return v4.String()
					}
					if fallback == "" {
						fallback = v4.String()
					}
				}
			}
		}
	}
	return fallback
}

// loadOrCreateCA 读取现有 CA（证书+私钥成对），缺任一侧就重新生成一对。
func loadOrCreateCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if certPEM, err := os.ReadFile(certPath); err == nil {
		if keyPEM, err := os.ReadFile(keyPath); err == nil {
			cert, key, err := parsePair(certPEM, keyPEM)
			if err == nil {
				return cert, key, nil
			}
			// 文件损坏（截断/编码错）：删掉重生成，不让 TLS 门永久起不来。
			_ = os.Remove(certPath)
			_ = os.Remove(keyPath)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("生成 CA 密钥: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "Friday Local CA", Organization: []string{"capyflow"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caTTL),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("签发 CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("解析 CA: %w", err)
	}
	if err := writeKeyPair(certPath, keyPath, der, key); err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// leafUpToDate 判断现有叶子证书是否仍覆盖全部期望 SAN（多一个 IP 无妨，少一个才重签）。
func leafUpToDate(certPath string, dns []string, ips []net.IP) (bool, error) {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return false, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return false, fmt.Errorf("证书 PEM 解析失败")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, err
	}
	if time.Now().After(cert.NotAfter) || time.Now().Before(cert.NotBefore) {
		return false, nil
	}
	haveDNS := map[string]bool{}
	for _, d := range cert.DNSNames {
		haveDNS[d] = true
	}
	for _, d := range dns {
		if !haveDNS[d] {
			return false, nil
		}
	}
	haveIP := map[string]bool{}
	for _, ip := range cert.IPAddresses {
		haveIP[ip.String()] = true
	}
	for _, ip := range ips {
		if !haveIP[ip.String()] {
			return false, nil
		}
	}
	return true, nil
}

func writeLeaf(certPath, keyPath string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns []string, ips []net.IP) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("生成叶子密钥: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "Friday Kernel", Organization: []string{"capyflow"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafTTL),
		DNSNames:     dns,
		IPAddresses:  ips,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("签发叶子证书: %w", err)
	}
	return writeKeyPair(certPath, keyPath, der, key)
}

func writeKeyPair(certPath, keyPath string, certDER []byte, key *ecdsa.PrivateKey) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("编码私钥: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("写证书: %w", err)
	}
	// 私钥只归本用户：0600，多一位都不给。
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("写私钥: %w", err)
	}
	return nil
}

func parsePair(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("证书 PEM 解析失败")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("私钥 PEM 解析失败")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func certPEMBytes(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// desiredSANs 期望的主机名与 IP 集：localhost、本机主机名、回环与全部非回环 IPv4。
func desiredSANs() ([]string, []net.IP) {
	dns := []string{"localhost"}
	if h, err := os.Hostname(); err == nil && h != "" {
		dns = append(dns, h)
	}
	sort.Strings(dns)
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			if v4 := ipn.IP.To4(); v4 != nil {
				ips = append(ips, v4)
			}
		}
	}
	return dns, ips
}

func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

// trimSANs 便于测试比较（排序去重）。
func trimSANs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
