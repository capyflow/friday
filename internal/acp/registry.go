// registry.go 对接官方 ACP agent 注册表（M-3，与 Zed 同源同 schema）：
// https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json
//
// 拉取 → 本地缓存（TTL 1h）→ 与内置精选目录按 id 合并（内置优先：它们的探测
// 与安装命令更贴合无 TTY 服务器场景）。上游上架新 agent 时 Friday 无需发版：
// 打开目录即见，npx 条目走 npm -g 安装，binary 条目按平台下载 + sha256 校验 +
// 解压到 <数据目录>/acp/agents/<id>/<version>/。拉取失败回退过期缓存，
// 连缓存都没有则只展示内置目录——离线时目录页照常可用。
package acp

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/capyflow/friday/internal/config"
)

// RegistryURL 官方注册表地址（与 Zed 的 agent_registry_store 同源）。
const RegistryURL = "https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json"

const (
	// registryCacheTTL 缓存有效期；过期后下一次打开/刷新目录时重新拉取。
	registryCacheTTL = time.Hour
	// registryFetchTimeout 单次拉取上限。
	registryFetchTimeout = 10 * time.Second
	// maxBinaryBytes binary 分发的下载上限（agent CLI 普遍 <100MB，留余量）。
	maxBinaryBytes = 256 << 20
	// maxExtractBytes 解包后的总字节上限（压缩包膨胀防护）。
	maxExtractBytes = 1 << 30
)

// ---- 官方 wire schema（字段名与 Zed RegistryIndex 逐一对齐，不做改动）----

type registryIndex struct {
	Version string          `json:"version"`
	Agents  []registryEntry `json:"agents"`
}

type registryEntry struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	Version      string               `json:"version"`
	Description  string               `json:"description"`
	Repository   string               `json:"repository"`
	Website      string               `json:"website"`
	LicenseURL   string               `json:"license_url"`
	Icon         string               `json:"icon"`
	Distribution registryDistribution `json:"distribution"`
}

type registryDistribution struct {
	Binary map[string]registryBinaryTarget `json:"binary"`
	Npx    *registryNpxDistribution        `json:"npx"`
}

type registryBinaryTarget struct {
	Archive string            `json:"archive"`
	Cmd     string            `json:"cmd"`
	Args    []string          `json:"args"`
	SHA256  string            `json:"sha256"`
	Env     map[string]string `json:"env"`
}

type registryNpxDistribution struct {
	Package string            `json:"package"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// registryURLFn 可被测试替换（指向 httptest 服务）。
var registryURLFn = func() string { return RegistryURL }

// registryHTTP 可被测试替换（离线测试用秒败 transport）。
var registryHTTP = &http.Client{Timeout: registryFetchTimeout}

// platformKey 返回官方 binary 分发的平台键（darwin-aarch64 形态）。
func platformKey() string {
	osName := "unknown"
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		osName = runtime.GOOS
	}
	arch := "x86_64"
	switch runtime.GOARCH {
	case "arm64":
		arch = "aarch64"
	case "amd64":
		arch = "x86_64"
	}
	return osName + "-" + arch
}

// registryCacheFile 是本地缓存形态：官方文件本身没有拉取时间，包一层。
type registryCacheFile struct {
	FetchedAt time.Time     `json:"fetchedAt"`
	Index     registryIndex `json:"index"`
}

// fetchRegistryIndex 拉取并解析官方目录。
func fetchRegistryIndex(ctx context.Context) (registryIndex, error) {
	var idx registryIndex
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURLFn(), nil)
	if err != nil {
		return idx, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := registryHTTP.Do(req)
	if err != nil {
		return idx, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return idx, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return idx, err
	}
	if err := json.Unmarshal(body, &idx); err != nil {
		return idx, fmt.Errorf("解析 registry.json 失败: %w", err)
	}
	return idx, nil
}

// registryIndex 取官方条目（Manager 方法：缓存路径与开关随实例走，便于测试）。
// force=true 忽略 TTL 强制拉取；任何失败都回退过期缓存，缓存也没有则返回 nil。
func (m *Manager) registryIndex(ctx context.Context, force bool) []registryEntry {
	if m.registryOff {
		return nil
	}
	m.fetchMu.Lock()
	defer m.fetchMu.Unlock()
	cached := m.readRegistryCache()
	if !force && cached != nil && time.Since(cached.FetchedAt) < registryCacheTTL {
		return cached.Index.Agents
	}
	ictx, cancel := context.WithTimeout(ctx, registryFetchTimeout)
	defer cancel()
	idx, err := fetchRegistryIndex(ictx)
	if err != nil {
		if cached != nil {
			log.Printf("[智能体] 官方目录拉取失败，使用 %s 的缓存: %v",
				cached.FetchedAt.Format("01-02 15:04"), err)
			return cached.Index.Agents
		}
		log.Printf("[智能体] 官方目录不可用（仅显示内置精选）: %v", err)
		return nil
	}
	m.writeRegistryCache(idx)
	return idx.Agents
}

func (m *Manager) readRegistryCache() *registryCacheFile {
	data, err := os.ReadFile(m.registryPath)
	if err != nil {
		return nil
	}
	var f registryCacheFile
	if json.Unmarshal(data, &f) != nil || len(f.Index.Agents) == 0 {
		return nil
	}
	return &f
}

func (m *Manager) writeRegistryCache(idx registryIndex) {
	if err := os.MkdirAll(filepath.Dir(m.registryPath), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(registryCacheFile{FetchedAt: time.Now(), Index: idx})
	if err != nil {
		return
	}
	tmp := m.registryPath + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, m.registryPath)
	}
}

// ---- 官方条目 → Friday 目录卡片 ----

// toCatalogEntry 转成目录卡片。返回 supported=false 表示 binary 分发不含当前
// 平台（卡片照常展示但禁用安装）。Template.Command 的三种形态：
//   - npx 条目：启动器 "npx"（安装/注册时经 lookPathFn 锚定绝对路径）；
//   - binary 条目：预期安装路径（真实路径以安装结果为准）；
//   - 其余（无分发）：不产生卡片。
func (e registryEntry) toCatalogEntry(registryBase string) (CatalogEntry, bool) {
	ce := CatalogEntry{
		ID:          e.ID,
		Name:        e.Name,
		Description: e.Description,
		DocURL:      firstNonEmpty(e.Repository, e.Website),
	}
	ce.Template = config.AcpAgentConfig{Name: e.ID, Version: e.Version}
	switch {
	case e.Distribution.Npx != nil:
		n := e.Distribution.Npx
		ce.InstallCmd = "npm install -g " + n.Package
		ce.Template.Command = "npx"
		ce.Template.Args = append([]string{"-y", n.Package}, n.Args...)
		ce.Template.Env = copyEnv(n.Env)
		return ce, true
	case len(e.Distribution.Binary) > 0:
		if registryBase == "" {
			return ce, false
		}
		rel, err := safeRel(e.Distribution.binCmd())
		if err != nil {
			return ce, false
		}
		target, ok := e.Distribution.Binary[platformKey()]
		if ok {
			ce.Template.Args = target.Args
			ce.Template.Env = copyEnv(target.Env)
		}
		ce.Template.Command = filepath.Join(registryBase, "agents", safeComponent(e.ID),
			safeComponent(e.Version), filepath.FromSlash(rel))
		return ce, ok
	default:
		return ce, false
	}
}

// binCmd 返回 binary 分发在当前平台的 cmd；无当前平台分发时返回任意一个键的
// cmd（仅用于推导预期路径，supported=false 时安装会被拒）。
func (d registryDistribution) binCmd() string {
	if t, ok := d.Binary[platformKey()]; ok {
		return t.Cmd
	}
	for _, t := range d.Binary {
		return t.Cmd
	}
	return ""
}

func copyEnv(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- binary 分发：下载 + sha256 校验 + 解压 ----

// installBinaryDistribution 下载当前平台的 binary 分发并解压，返回可执行文件
// 路径与输出摘要。目录为 <base>/agents/<id>/<version>/，重装同版本先清空。
func installBinaryDistribution(ctx context.Context, registryBase string, re registryEntry) (string, string, error) {
	target, ok := re.Distribution.Binary[platformKey()]
	if !ok {
		return "", "", fmt.Errorf("agent %q 在本平台（%s）没有分发包", re.Name, platformKey())
	}
	if registryBase == "" {
		return "", "", fmt.Errorf("agent 安装目录不可用（无法解析 HOME）")
	}
	dir := filepath.Join(registryBase, "agents", safeComponent(re.ID), safeComponent(re.Version))

	out := ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.Archive, nil)
	if err != nil {
		return "", out, err
	}
	resp, err := registryHTTP.Do(req)
	if err != nil {
		return "", out, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", out, fmt.Errorf("下载失败：HTTP %d（%s）", resp.StatusCode, target.Archive)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBinaryBytes+1))
	if err != nil {
		return "", out, fmt.Errorf("下载失败: %w", err)
	}
	if len(data) > maxBinaryBytes {
		return "", out, fmt.Errorf("分发超过 %dMB 上限，拒绝写入", maxBinaryBytes>>20)
	}
	if target.SHA256 != "" {
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != strings.ToLower(target.SHA256) {
			return "", out, fmt.Errorf("sha256 校验不匹配（预期 %.12s…，实际 %.12s…）——分发可能被篡改或已更新",
				target.SHA256, got)
		}
	} else {
		out += "（分发未提供 sha256，跳过校验）\n"
	}

	if err := os.RemoveAll(dir); err != nil {
		return "", out, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", out, err
	}
	bin, err := extractArchive(target.Archive, data, dir, target.Cmd)
	if err != nil {
		return "", out, fmt.Errorf("解压失败: %w", err)
	}
	return bin, out, nil
}

// safeComponent 把任意字符串清成单层安全目录名（保留点号——版本号形态）。
func safeComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if out := b.String(); out != "" && out != "." && out != ".." {
		return out
	}
	return "latest"
}

// safeRel 把官方 cmd（如 "./amp-acp"）清成相对路径，拒绝越界。
func safeRel(cmd string) (string, error) {
	rel := path.Clean(strings.TrimPrefix(strings.TrimSpace(cmd), "./"))
	if rel == "." || rel == "" || path.IsAbs(rel) || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("非法的分发命令路径 %q", cmd)
	}
	return rel, nil
}

// extractArchive 按下载地址后缀解包：.tar.gz/.tgz → tar，.zip → zip，其余按
// 裸二进制落盘。返回 cmd 指向的可执行文件路径。常规文件一律 0o755（自包含
// agent 目录，二进制必须可执行）；拒绝符号/硬链接条目；解包总量受
// maxExtractBytes 约束。
func extractArchive(archiveURL string, data []byte, dir, cmd string) (string, error) {
	rel, err := safeRel(cmd)
	if err != nil {
		return "", err
	}
	lower := strings.ToLower(archiveURL)
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(data, dir, rel)
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(data, dir, rel)
	default:
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, data, 0o755); err != nil {
			return "", err
		}
		return p, nil
	}
}

// writeEntry 落一个常规文件（带总量守卫与路径越界检查），返回是否跳过。
func writeEntry(dir, name string, r io.Reader, sizeHint int64, total *int64) (bool, error) {
	clean := path.Clean(name)
	if clean == "." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return true, nil // 可疑条目：跳过不落盘
	}
	if sizeHint > 0 {
		*total += sizeHint
	} else {
		*total++ // 未知大小按最小值计，靠拷贝上限兜底
	}
	if *total > maxExtractBytes {
		return false, fmt.Errorf("解包总量超过 %dMB 上限", maxExtractBytes>>20)
	}
	dst := filepath.Join(dir, filepath.FromSlash(clean))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if sizeHint > 0 {
		_, err = io.CopyN(f, r, sizeHint)
	} else {
		_, err = io.Copy(f, io.LimitReader(r, maxExtractBytes))
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func extractTarGz(data []byte, dir, rel string) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			clean := path.Clean(hdr.Name)
			if clean != "." && !path.IsAbs(clean) && !strings.HasPrefix(clean, "../") {
				if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(clean)), 0o755); err != nil {
					return "", err
				}
			}
		case tar.TypeReg:
			if skip, err := writeEntry(dir, hdr.Name, tr, hdr.Size, &total); err != nil || skip {
				if err != nil {
					return "", err
				}
			}
		case tar.TypeSymlink, tar.TypeLink:
			return "", fmt.Errorf("分发包包含链接条目 %q，拒绝解包", hdr.Name)
		}
	}
	return finishBinary(dir, rel)
}

func extractZip(data []byte, dir, rel string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var total int64
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		if zf.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("分发包包含链接条目 %q，拒绝解包", zf.Name)
		}
		rc, err := zf.Open()
		if err != nil {
			return "", err
		}
		skip, err := writeEntry(dir, zf.Name, rc, int64(zf.UncompressedSize64), &total)
		rc.Close()
		if err != nil {
			return "", err
		}
		_ = skip
	}
	return finishBinary(dir, rel)
}

// finishBinary 校验 cmd 指向的文件存在并确保可执行。
func finishBinary(dir, rel string) (string, error) {
	bin := filepath.Join(dir, filepath.FromSlash(rel))
	info, err := os.Stat(bin)
	if err != nil {
		return "", fmt.Errorf("分发包里没有 cmd 指向的 %q", rel)
	}
	if info.IsDir() {
		return "", fmt.Errorf("cmd %q 指向的是目录而非可执行文件", rel)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return "", err
	}
	return bin, nil
}
