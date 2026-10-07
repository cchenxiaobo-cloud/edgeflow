// Package imageauth 实现私有镜像仓库拉取凭证的云端托管（v0.46.0，spec 0019 US-6，
// 发展规划 G28）。
//
// 设计要点：
//   - 存储：JSON 文件（EDGEFLOW_CLOUDCORE_IMAGEAUTH_FILE，0600）；
//     registry 地址唯一；password 仅存 SHA-256 哈希 + AES-256-GCM 静态加密
//     （密钥 EDGEFLOW_CLOUDCORE_IMAGEAUTH_KEY 派生 SHA-256；未配 key 时仅存
//     哈希并打 Warn——下发不可用，列表语义不变）；
//   - 脱敏硬规则：明文 password 只在 PUT/POST 请求体出现一次；日志/审计/响应
//     一律哈希摘要（≤12 字符）或掩码；
//   - 下发形态：发布注入 imageAuth{registry,username}（authRef 形态），密码
//     不进 config-sync 明文——边缘按 registry 自查本地 env 凭证；
//   - 零第三方依赖：crypto/aes + crypto/cipher 标准库。
package imageauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// 环境变量约定。
const (
	// EnvFile 是凭证文件路径环境变量名。
	EnvFile = "EDGEFLOW_CLOUDCORE_IMAGEAUTH_FILE"
	// EnvKey 是静态加密密钥环境变量名（任意非空字符串；SHA-256 派生 AES-256 key）。
	EnvKey = "EDGEFLOW_CLOUDCORE_IMAGEAUTH_KEY"
)

// Entry 是一条仓库凭证（存储形态）。
type Entry struct {
	Registry string `json:"registry"` // 仓库地址（host[:port]，小写规范化）
	Username string `json:"username"`
	Hash     string `json:"hash"`             // password 的 SHA-256 hex（列表回显前 12 位）
	Secret   string `json:"secret,omitempty"` // AES-GCM(nonce+ciphertext)；未配 key 时空
}

// Store 是凭证表（内存 + 文件写穿）。
type Store struct {
	mu      sync.RWMutex
	path    string
	aead    cipher.AEAD // nil = 未配 key（仅哈希，Warn 下发不可用）
	entries []Entry
}

var registryRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?$`)

// NormalizeRegistry 规范化仓库地址（小写、去协议前缀、去尾斜杠）。
func NormalizeRegistry(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(s, "/")
	return s
}

// ValidRegistry 报告规范化后的地址是否合法（host[:port] 形态）。
func ValidRegistry(s string) bool { return registryRe.MatchString(s) }

// NewKey 从口令派生 AES-256-GCM（SHA-256；口令为空 → nil）。
func NewKey(secret string) (cipher.AEAD, error) {
	if secret == "" {
		return nil, nil
	}
	sum := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Load 从文件加载（不存在 → 空表）。
func Load(path string, aead cipher.AEAD) (*Store, error) {
	s := &Store{path: path, aead: aead}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var es []Entry
	if err := json.Unmarshal(data, &es); err != nil {
		return nil, fmt.Errorf("镜像凭证文件解析失败: %w", err)
	}
	for _, e := range es {
		if e.Registry == "" || e.Username == "" || len(e.Hash) != 64 {
			return nil, fmt.Errorf("镜像凭证条目非法（registry=%q）", e.Registry)
		}
	}
	s.entries = es
	return s, nil
}

// save 写穿（0600 + 原子 rename）。
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Set 新增/覆盖一条凭证（password 明文入参仅此一次；落盘哈希+加密）。
func (s *Store) Set(registry, username, password string) error {
	registry = NormalizeRegistry(registry)
	if !ValidRegistry(registry) {
		return fmt.Errorf("仓库地址非法: %q", registry)
	}
	if username == "" || password == "" {
		return errors.New("username 与 password 不能为空")
	}
	sum := sha256.Sum256([]byte(password))
	e := Entry{Registry: registry, Username: username, Hash: hex.EncodeToString(sum[:])}
	if s.aead != nil {
		nonce := make([]byte, s.aead.NonceSize())
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return err
		}
		e.Secret = hex.EncodeToString(append(nonce, s.aead.Seal(nil, nonce, []byte(password), nil)...))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	replaced := false
	for i := range s.entries {
		if s.entries[i].Registry == registry {
			s.entries[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		s.entries = append(s.entries, e)
	}
	if err := s.save(); err != nil {
		return err
	}
	return nil
}

// Delete 删除一条凭证（不存在 → false）。
func (s *Store) Delete(registry string) bool {
	registry = NormalizeRegistry(registry)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].Registry == registry {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			if err := s.save(); err != nil {
				return false
			}
			return true
		}
	}
	return false
}

// View 是列表/读取的脱敏视图。
type View struct {
	Registry   string `json:"registry"`
	Username   string `json:"username"`
	Password   string `json:"password"`   // 哈希摘要前 12 位（脱敏回显）
	Deployable bool   `json:"deployable"` // secret 在 = 可下发明文给边缘链路
}

// List 返回全部脱敏视图。
func (s *Store) List() []View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]View, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, View{Registry: e.Registry, Username: e.Username,
			Password: HashPrefix(e.Hash), Deployable: e.Secret != ""})
	}
	return out
}

// HashPrefix 返回哈希摘要前 12 位（脱敏口径；不足截全）。
func HashPrefix(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// Lookup 下发链路：按镜像引用的 registry 前缀匹配凭证并解密出明文密码
// （仅发布注入链路调用；未配 key/无 secret → 错误「下发不可用」）。
// imageRef 形态：registry/path/name:tag 或 registry:port/path/name:tag。
func (s *Store) Lookup(imageRef string) (registry, username, password string, err error) {
	reg := registryOf(imageRef)
	if reg == "" {
		return "", "", "", fmt.Errorf("镜像引用无法解析 registry: %q", imageRef)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.Registry != reg {
			continue
		}
		if s.aead == nil || e.Secret == "" {
			return reg, e.Username, "", errors.New("镜像凭证下发不可用（未配置静态加密密钥或条目无密文）")
		}
		raw, derr := hex.DecodeString(e.Secret)
		if derr != nil {
			return "", "", "", derr
		}
		ns := s.aead.NonceSize()
		if len(raw) < ns {
			return "", "", "", errors.New("镜像凭证密文非法")
		}
		pt, derr := s.aead.Open(nil, raw[:ns], raw[ns:], nil)
		if derr != nil {
			return "", "", "", derr
		}
		return reg, e.Username, string(pt), nil
	}
	return "", "", "", os.ErrNotExist
}

// Match 报告镜像引用是否有托管凭证（发布注入判断；不解密）。
func (s *Store) Match(imageRef string) bool {
	reg := registryOf(imageRef)
	if reg == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.Registry == reg {
			return true
		}
	}
	return false
}

// registryOf 从镜像引用提取 registry 段（首个 / 前；含 . 或 : 才视为 registry——
// 官方 Docker Hub 短名 nginx → 空（无 registry 段，用默认仓库不注入））。
func registryOf(imageRef string) string {
	slash := strings.Index(imageRef, "/")
	if slash <= 0 {
		return ""
	}
	head := imageRef[:slash]
	if !strings.ContainsAny(head, ".:") {
		return ""
	}
	return NormalizeRegistry(head)
}

// VerifyPassword 校验一条已存 password（运维验证面；constant-time）。
func (s *Store) VerifyPassword(registry, password string) bool {
	sum := sha256.Sum256([]byte(password))
	want := hex.EncodeToString(sum[:])
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.Registry == NormalizeRegistry(registry) {
			return subtle.ConstantTimeCompare([]byte(want), []byte(e.Hash)) == 1
		}
	}
	return false
}

// EnsureDir 父目录创建（0700）。
func EnsureDir(path string) error { return os.MkdirAll(filepath.Dir(path), 0o700) }
