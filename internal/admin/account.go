package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// 默认账号常量仅用于已有部署兼容；新部署必须完成首次设置或预置凭据。
const (
	DefaultUsername = "admin"
	DefaultPassword = "admin"
	MinPasswordLen  = 6
	MaxUsernameLen  = 32
)

// Account 是管理员账号（密码只存 bcrypt 哈希）。
// 账号来源：启动时由配置文件或环境变量预置（见 config.ResolveAdminCredentials），
// 未预置时进入首次设置。旧账号文件中的 mustChange 字段加载时被忽略。
type Account struct {
	Username      string    `json:"username"`
	PasswordHash  string    `json:"passwordHash"`
	UpdatedAt     time.Time `json:"updatedAt"`
	SetupRequired bool      `json:"setupRequired,omitempty"`
}

// AccountStore 是管理员账号仓库：JSON 落盘，重启后账号不变。
type AccountStore struct {
	mu      sync.Mutex
	path    string // 为空表示不持久化（测试用）
	acc     Account
	initErr error
}

// NewAccountStore 创建账号仓库；文件不存在时保存未初始化状态，不创建默认凭据。
// 调用方须检查 Err；加载或初始化失败时仓库拒绝登录和更新，并保留原文件。
func NewAccountStore(path string) *AccountStore {
	s := &AccountStore{path: path}
	loaded, err := s.load()
	if err != nil {
		s.initErr = err
		return s
	}
	if loaded {
		return s
	}
	acc := Account{
		SetupRequired: true,
		UpdatedAt:     time.Now(),
	}
	if err := s.saveLocked(acc); err != nil {
		s.initErr = err
		return s
	}
	s.acc = acc
	return s
}

// Err 返回加载或初始化错误；恢复原文件后需重新创建仓库。
func (s *AccountStore) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initErr
}

// load 从文件加载账号；只有路径确实不存在才允许默认初始化。
func (s *AccountStore) load() (bool, error) {
	if s.path == "" {
		return false, nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// ReadFile 对悬空符号链接也返回不存在，不能将它当作首次启动。
			if _, statErr := os.Lstat(s.path); errors.Is(statErr, os.ErrNotExist) {
				return false, nil
			}
		}
		return false, fmt.Errorf("读取账号文件 %q 失败: %w", s.path, err)
	}
	var acc Account
	if err := json.Unmarshal(b, &acc); err != nil {
		return false, fmt.Errorf("解析账号文件 %q 失败: %w", s.path, err)
	}
	if acc.SetupRequired {
		if acc.Username != "" || acc.PasswordHash != "" {
			return false, fmt.Errorf("账号文件 %q 的首次设置状态无效", s.path)
		}
		s.acc = acc
		return true, nil
	}
	if strings.TrimSpace(acc.Username) == "" || utf8.RuneCountInString(acc.Username) > MaxUsernameLen {
		return false, fmt.Errorf("账号文件 %q 的用户名无效", s.path)
	}
	if _, err := bcrypt.Cost([]byte(acc.PasswordHash)); err != nil {
		return false, fmt.Errorf("账号文件 %q 的密码哈希无效: %w", s.path, err)
	}
	// Cost 只解析哈希头；实际校验还可发现损坏的盐编码。
	if err := bcrypt.CompareHashAndPassword([]byte(acc.PasswordHash), nil); err != nil && !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, fmt.Errorf("账号文件 %q 的密码哈希无效: %w", s.path, err)
	}
	s.acc = acc
	return true, nil
}

// saveLocked 原子写回（调用方须持有 mu）。
func (s *AccountStore) saveLocked(acc Account) error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(acc, "", "  ")
	if err != nil {
		return fmt.Errorf("编码账号失败: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建账号临时文件失败: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("写入账号临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭账号临时文件失败: %w", err)
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return fmt.Errorf("替换账号文件失败: %w", err)
	}
	return nil
}

// Login 校验用户名与密码；成功返回账号信息（不含密码哈希）。
func (s *AccountStore) Login(username, password string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initErr != nil || s.acc.SetupRequired {
		return Account{}, false
	}
	if subtleMismatch(s.acc.Username, username) {
		return Account{}, false
	}
	if bcrypt.CompareHashAndPassword([]byte(s.acc.PasswordHash), []byte(password)) != nil {
		return Account{}, false
	}
	return Account{Username: s.acc.Username}, true
}

// Update 用给定的用户名与密码覆盖账号（启动预置凭据走此入口，见 main）。
// 规则：用户名非空且 ≤32 字符；密码 ≥6 位。
func (s *AccountStore) Update(username, password string) error {
	return s.update(username, password, false)
}

var ErrAlreadyInitialized = errors.New("管理员账号已设置，请使用登录")

// Initialize 仅允许未初始化的账号仓库设置一次；检查和落盘处于同一互斥锁。
func (s *AccountStore) Initialize(username, password string) error {
	return s.update(username, password, true)
}

func (s *AccountStore) update(username, password string, initialOnly bool) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("用户名不能为空")
	}
	if utf8.RuneCountInString(username) > MaxUsernameLen {
		return fmt.Errorf("用户名最长 %d 个字符", MaxUsernameLen)
	}
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return fmt.Errorf("密码至少 %d 位", MinPasswordLen)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("加密密码失败: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initErr != nil {
		return fmt.Errorf("账号仓库不可用: %w", s.initErr)
	}
	if initialOnly && !s.acc.SetupRequired {
		return ErrAlreadyInitialized
	}
	acc := Account{
		Username:     username,
		PasswordHash: string(hash),
		UpdatedAt:    time.Now(),
	}
	if err := s.saveLocked(acc); err != nil {
		return err
	}
	s.acc = acc
	return nil
}

// Current 返回当前账号信息（不含密码哈希）。
func (s *AccountStore) Current() Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Account{Username: s.acc.Username, SetupRequired: s.acc.SetupRequired}
}

// subtleMismatch 常量时间比较用户名，避免用户名枚举计时侧信道。
func subtleMismatch(a, b string) bool {
	if len(a) != len(b) {
		return true
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v != 0
}
