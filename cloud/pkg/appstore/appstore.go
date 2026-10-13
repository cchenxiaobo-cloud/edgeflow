// Package appstore 实现云端应用目录与版本目录存储（v0.47.0，spec 0020 US-1，
// 发展规划 G08）。
//
// 设计要点：
//   - 内存 map + RWMutex；可选 JSON 文件持久化（EDGEFLOW_CLOUDCORE_APPSTORE_FILE，
//     0600 + 临时文件原子 rename 写穿——沿用 rbac/imageauth 同族模式）；
//   - 应用：name 唯一（小写规范化，DNS-1123 子集校验）；kind ∈
//     container|script（决定 version 的 image/scriptRef 二选一）；
//   - 版本：app 内 version 字符串唯一（409 语义由 API 层转换）；
//   - 分页：limit/offset 由 API 层做（store 返回全量有序切片）；
//   - 零第三方依赖。
package appstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// EnvFile 是应用目录持久化文件路径环境变量名。
const EnvFile = "EDGEFLOW_CLOUDCORE_APPSTORE_FILE"

// 应用形态白名单。
const (
	KindContainer = "container"
	KindScript    = "script"
)

// ValidKind 报告应用形态是否在白名单内。
func ValidKind(k string) bool { return k == KindContainer || k == KindScript }

// nameRe 限制应用名（DNS-1123 子集：小写字母数字与 -，≤63）。
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidName 报告应用名是否合法。
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Version 是应用的一个版本登记。
type Version struct {
	Version   string            `json:"version"`             // 语义化/任意字符串（非空 ≤64）
	Image     string            `json:"image,omitempty"`     // kind=container 必填
	ScriptRef string            `json:"scriptRef,omitempty"` // kind=script 必填（脚本包引用名）
	Metadata  map[string]string `json:"metadata,omitempty"`
	HasPkg    bool              `json:"hasPkg"` // 应用包是否已上传（US-3；重启后由 pkgdir 对账回填）
}

// App 是一个注册应用（含版本目录）。
type App struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName,omitempty"`
	Description string    `json:"description,omitempty"`
	Kind        string    `json:"kind"`
	CreatedAt   int64     `json:"createdAt"` // Unix 毫秒
	Versions    []Version `json:"versions"`  // 按 version 字典序
}

// ErrExists 应用/版本已存在。
var ErrExists = errors.New("already exists")

// ErrNotFound 应用/版本不存在。
var ErrNotFound = errors.New("not found")

// Store 是应用目录存储（内存 + 可选文件写穿）。
type Store struct {
	mu   sync.RWMutex
	path string
	apps map[string]*App
}

// New 构造空 store（path 为空 = 纯内存）。
func New(path string) *Store { return &Store{path: path, apps: map[string]*App{}} }

// Load 从文件恢复（不存在 → 空表）。
func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var apps []*App
	if err := json.Unmarshal(data, &apps); err != nil {
		return fmt.Errorf("应用目录文件解析失败: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range apps {
		if a.Name == "" || !ValidKind(a.Kind) {
			return fmt.Errorf("应用条目非法（name=%q kind=%q）", a.Name, a.Kind)
		}
		s.apps[a.Name] = a
	}
	return nil
}

// save 写穿（调用方持写锁）。
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	apps := make([]*App, 0, len(s.apps))
	for _, a := range s.apps {
		apps = append(apps, a)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	data, err := json.MarshalIndent(apps, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Create 注册应用（name 唯一；kind 白名单；首个版本可选）。
func (s *Store) Create(name, displayName, description, kind string, nowMs int64, firstVersion *Version) (*App, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !ValidName(name) {
		return nil, fmt.Errorf("应用名非法（DNS-1123 子集）: %q", name)
	}
	if !ValidKind(kind) {
		return nil, fmt.Errorf("应用形态非法: %q（白名单 container|script）", kind)
	}
	if firstVersion != nil {
		if err := validateVersion(kind, firstVersion); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[name]; ok {
		return nil, ErrExists
	}
	a := &App{Name: name, DisplayName: displayName, Description: description,
		Kind: kind, CreatedAt: nowMs}
	if firstVersion != nil {
		a.Versions = append(a.Versions, *firstVersion)
	}
	s.apps[name] = a
	if err := s.save(); err != nil {
		delete(s.apps, name)
		return nil, err
	}
	return a, nil
}

// validateVersion 按 kind 校验版本对象（image/scriptRef 二选一必填）。
func validateVersion(kind string, v *Version) error {
	if v.Version == "" || len(v.Version) > 64 {
		return errors.New("version 非空且 ≤64 字符")
	}
	// 字符集收紧（安全硬规则）：version 会参与包存储路径拼接（appPkgPath），
	// 仅允许 [A-Za-z0-9._-] 且拒绝 ".." 序列——杜绝路径穿越（v0.47 复核修正）
	for _, r := range v.Version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return errors.New("version 仅允许字母/数字/点/下划线/连字符")
	}
	if strings.Contains(v.Version, "..") {
		return errors.New("version 不允许包含 \"..\" 序列")
	}
	switch kind {
	case KindContainer:
		if v.Image == "" {
			return errors.New("container 应用版本必须携带 image")
		}
	case KindScript:
		if v.ScriptRef == "" {
			return errors.New("script 应用版本必须携带 scriptRef")
		}
	default:
		return fmt.Errorf("应用形态非法: %q", kind)
	}
	return nil
}

// Get 取应用（副本语义：返回深拷贝指针，调用方修改不影响存储）。
func (s *Store) Get(name string) (*App, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.apps[strings.ToLower(name)]
	if !ok {
		return nil, false
	}
	cp := *a
	cp.Versions = append([]Version(nil), a.Versions...)
	return &cp, true
}

// List 返回全部应用（按 name 字典序；副本切片）。
func (s *Store) List() []*App {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*App, 0, len(s.apps))
	for _, a := range s.apps {
		cp := *a
		cp.Versions = append([]Version(nil), a.Versions...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Delete 删除应用（不存在 → ErrNotFound）。
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(name)
	if _, ok := s.apps[key]; !ok {
		return ErrNotFound
	}
	delete(s.apps, key)
	return s.save()
}

// AddVersion 登记版本（app 内唯一；kind 决定必填字段）。
func (s *Store) AddVersion(name string, v Version) (*App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[strings.ToLower(name)]
	if !ok {
		return nil, ErrNotFound
	}
	if err := validateVersion(a.Kind, &v); err != nil {
		return nil, err
	}
	for _, existing := range a.Versions {
		if existing.Version == v.Version {
			return nil, ErrExists
		}
	}
	a.Versions = append(a.Versions, v)
	sort.Slice(a.Versions, func(i, j int) bool { return a.Versions[i].Version < a.Versions[j].Version })
	if err := s.save(); err != nil {
		// 回滚（截断新增尾元素；复核留档：同 rbac.Create 回滚模式）
		a.Versions = a.Versions[:len(a.Versions)-1]
		return nil, err
	}
	cp := *a
	cp.Versions = append([]Version(nil), a.Versions...)
	return &cp, nil
}

// GetVersion 取某版本（副本）。
func (s *Store) GetVersion(name, version string) (Version, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.apps[strings.ToLower(name)]
	if !ok {
		return Version{}, false
	}
	for _, v := range a.Versions {
		if v.Version == version {
			return v, true
		}
	}
	return Version{}, false
}

// MarkPkg 回填应用包上传状态（US-3 对账；不存在 app/version → ErrNotFound）。
func (s *Store) MarkPkg(name, version string, has bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[strings.ToLower(name)]
	if !ok {
		return ErrNotFound
	}
	for i := range a.Versions {
		if a.Versions[i].Version == version {
			a.Versions[i].HasPkg = has
			return s.save()
		}
	}
	return ErrNotFound
}

// Count 返回应用数（metrics 注入用）。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.apps)
}

// SanitizeAppName 规范化应用名为下发对象名安全子集（非 [a-z0-9-] 字符替换为 -，
// 与 modelrepo.SanitizeModelName 同族口径）。
func SanitizeAppName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
