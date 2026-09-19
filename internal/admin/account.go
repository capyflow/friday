package admin

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// 默认管理员账号：admin/admin（可通过启动配置或环境变量预置替换）。
const (
	DefaultUsername = "admin"
	DefaultPassword = "admin"
	MinPasswordLen  = 6
	MaxUsernameLen  = 32
)

// Account 是管理员账号（密码只存 bcrypt 哈希）。
// 账号来源：启动时由配置文件或环境变量预置（见 config.ResolveAdminCredentials），
// 未预置时使用默认账号 admin/admin。旧账号文件中的 mustChange 字段加载时被忽略。
type Account struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// AccountStore 是管理员账号仓库：JSON 落盘，重启后账号不变。
type AccountStore struct {
	mu   sync.Mutex
	path string // 为空表示不持久化（测试用）
	acc  Account
}

// NewAccountStore 创建账号仓库；文件不存在时初始化默认账号 admin/admin。
func NewAccountStore(path string) *AccountStore {
	s := &AccountStore{path: path}
	if s.load() {
		return s
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(DefaultPassword), bcrypt.DefaultCost)
	if err != nil {
		panic("admin: 初始化默认账号失败: " + err.Error())
	}
	s.acc = Account{
		Username:     DefaultUsername,
		PasswordHash: string(hash),
		UpdatedAt:    time.Now(),
	}
	s.saveLocked()
	return s
}

// load 从文件加载账号；文件不存在返回 false（走默认初始化），损坏时同样回退默认并告警。
func (s *AccountStore) load() bool {
	if s.path == "" {
		return false
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return false
	}
	var acc Account
	if err := json.Unmarshal(b, &acc); err != nil || acc.Username == "" || acc.PasswordHash == "" {
		log.Printf("[admin] 账号文件损坏，已回退默认账号 admin/admin: %v", err)
		return false
	}
	s.acc = acc
	return true
}

// saveLocked 原子写回（调用方须持有 mu）。
func (s *AccountStore) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.MarshalIndent(s.acc, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("[admin] 账号持久化失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[admin] 账号持久化失败: %v", err)
	}
}

// Login 校验用户名与密码；成功返回账号信息（不含密码哈希）。
func (s *AccountStore) Login(username, password string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.acc = Account{
		Username:     username,
		PasswordHash: string(hash),
		UpdatedAt:    time.Now(),
	}
	s.saveLocked()
	return nil
}

// Current 返回当前账号信息（不含密码哈希）。
func (s *AccountStore) Current() Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Account{Username: s.acc.Username}
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
