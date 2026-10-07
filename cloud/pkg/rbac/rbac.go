// Package rbac 实现 cloudcore 管理 API 的角色权限控制（v0.46.0，spec 0019 US-1/US-2/US-3，
// 发展规划 G27）。
//
// 设计要点：
//   - 角色三档：admin（全权）/ operator（读写除角色管理面）/ viewer（只读）；
//   - 凭证：id + token SHA-256 哈希 + role + scope.ns（可选命名空间范围，
//     空 = 不限）；持久化 JSON 文件（权限 0600），Token 明文只在创建响应出现一次；
//   - 授权判定：authz.Can(role, op, ns)（纯函数，表驱动可测）；
//   - 默认 off 零变化：EDGEFLOW_CLOUDCORE_RBAC != on 时中间件不装配，
//     auth 单令牌语义与 v0.45 逐字节一致；
//   - env 回退：RBAC 凭证表未命中时回退 env 单令牌（命中 → admin），
//     向后兼容既有部署；
//   - 职责边界：本包做「授权」（你能干什么）；「认证」（你是谁）仍在 cloud/pkg/auth。
package rbac

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 环境变量约定。
const (
	// EnvRBAC 是 RBAC 开关环境变量名：值为 "on" 时启用（且必须配置凭证文件）。
	EnvRBAC = "EDGEFLOW_CLOUDCORE_RBAC"
	// EnvUsersFile 是凭证文件路径环境变量名。
	EnvUsersFile = "EDGEFLOW_CLOUDCORE_RBAC_FILE"
)

// 角色枚举（低基数，白名单校验）。
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// ValidRole 报告角色是否在白名单内。
func ValidRole(r string) bool {
	switch r {
	case RoleAdmin, RoleOperator, RoleViewer:
		return true
	}
	return false
}

// op 是权限点操作（按 HTTP 方法 + 面判定，低基数）。
type op string

const (
	opRead   op = "read"   // GET
	opWrite  op = "write"  // POST/PUT
	opDelete op = "delete" // DELETE
)

// facetRoles 是角色管理面路径前缀（最高敏感，仅 admin；operator 删除也拒绝）。
var facetRoles = []string{"/api/v1/roles", "/api/v1/users"}

// User 是一条 API 凭证（存储形态：Token 仅存哈希）。
type User struct {
	ID   string   `json:"id"`                // 凭证 ID（审计 operator 记录此值）
	Hash string   `json:"hash"`              // token 的 SHA-256 hex
	Role string   `json:"role"`              // admin|operator|viewer
	NS   []string `json:"scopeNs,omitempty"` // 命名空间范围；空 = 不限
}

// Store 是凭证表（内存 + 文件写穿；Token 撤销即时生效）。
type Store struct {
	mu    sync.RWMutex
	path  string
	users []User // id 唯一；加载时校验
}

// HashToken 计算 token 的 SHA-256 hex（存储与比对统一口径）。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// LoadUsers 从 JSON 文件加载凭证表（文件不存在 → 空表 + nil 错误——
// 允许先开 RBAC 再建凭证？不：空表时仅 env 回退可用，启动 Warn 提示）。
func LoadUsers(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var users []User
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, fmt.Errorf("凭证文件解析失败: %w", err)
	}
	ids := map[string]bool{}
	for _, u := range users {
		if u.ID == "" || !ValidRole(u.Role) || len(u.Hash) != 64 {
			return nil, fmt.Errorf("凭证条目非法（id=%q role=%q hashLen=%d）", u.ID, u.Role, len(u.Hash))
		}
		if ids[u.ID] {
			return nil, fmt.Errorf("凭证 ID 重复: %q", u.ID)
		}
		ids[u.ID] = true
	}
	s.users = users
	return s, nil
}

// save 写穿凭证文件（0600；写入临时文件后 rename 原子替换）。
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.users, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Create 新增凭证（token 明文入参，落盘前转哈希；返回含明文的视图供一次性响应）。
// ID 重复 / 角色非法 → 错误。
func (s *Store) Create(id, token, role string, ns []string) (*User, error) {
	if id == "" || token == "" {
		return nil, errors.New("id 与 token 不能为空")
	}
	if !ValidRole(role) {
		return nil, fmt.Errorf("角色非法: %q（白名单 admin|operator|viewer）", role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.ID == id {
			return nil, fmt.Errorf("凭证 ID 已存在: %q", id)
		}
	}
	u := User{ID: id, Hash: HashToken(token), Role: role, NS: append([]string(nil), ns...)}
	s.users = append(s.users, u)
	if err := s.save(); err != nil {
		s.users = s.users[:len(s.users)-1] // 回滚内存
		return nil, err
	}
	return &u, nil
}

// Delete 撤销凭证（即时生效；不存在 → false）。
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, u := range s.users {
		if u.ID == id {
			s.users = append(s.users[:i], s.users[i+1:]...)
			if err := s.save(); err != nil {
				// 写穿失败：回滚内存保持一致（调用方记日志）
				s.users = append(s.users[:i], append([]User{u}, s.users[i:]...)...)
				return false
			}
			return true
		}
	}
	return false
}

// List 返回凭证视图（脱敏：hash 截断前 12 字符；ID/角色/范围完整）。
func (s *Store) List() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, len(s.users))
	for i, u := range s.users {
		u.Hash = truncateHash(u.Hash)
		u.NS = append([]string(nil), u.NS...)
		out[i] = u
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func truncateHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// Authenticate 按明文 token 查凭证（constant-time 哈希比对）。
// 命中 → (User, true)；未命中 → (zero, false)。
func (s *Store) Authenticate(token string) (User, bool) {
	if token == "" {
		return User{}, false
	}
	h := HashToken(token)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if subtle.ConstantTimeCompare([]byte(h), []byte(u.Hash)) == 1 {
			return u, true
		}
	}
	return User{}, false
}

// opOf 把方法 + 路径映射为权限点（facet 判定：roles/users 面特殊标记 roles）。
func opOf(method, path string) (o op, facet string) {
	switch method {
	case "GET":
		o = opRead
	case "POST", "PUT":
		o = opWrite
	case "DELETE":
		o = opDelete
	default:
		o = opWrite
	}
	for _, p := range facetRoles {
		if path == p || strings.HasPrefix(path, p+"/") {
			return o, "roles"
		}
	}
	return o, ""
}

// nsOf 提取路径中的命名空间段（/api/v1/namespaces/{ns}/... 与
// /api/v1/{res}/namespaces/{ns}/... 两形态；无 → 空）。
func nsOf(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, seg := range segs {
		if seg == "namespaces" && i+1 < len(segs) {
			return segs[i+1]
		}
	}
	return ""
}

// Can 判定角色对 (op, facet, ns) 的权限（纯函数——表驱动测试锚点）。
//   - admin：全部允许；
//   - operator：read/write 允许；delete 仅非 roles 面；roles 面一律拒绝；
//   - viewer：仅 read；roles 面 read 允许（角色枚举可见）其余拒绝；
//   - scope.ns 非空且 ns 非空且不在范围 → false（403）。
func Can(u User, method, path string) bool {
	o, facet := opOf(method, path)
	switch u.Role {
	case RoleAdmin:
		// admin 全权（仍受 scope.ns 约束？admin scope 视为空——语义见 spec §2 US-2）
		if facet == "roles" {
			return true
		}
	case RoleOperator:
		if facet == "roles" {
			return false // 角色管理面 operator 一律拒绝
		}
		if o == opDelete {
			return false // v0.46 语义：operator 无删除（保守起步）
		}
	case RoleViewer:
		if o != opRead || facet == "roles" {
			return false
		}
	default:
		return false
	}
	// 命名空间范围约束（admin 也受约束——scope 显式即生效，仅空 = 不限；
	// admin 默认创建时不带 scope，需限定时显式配置）
	if len(u.NS) > 0 {
		if ns := nsOf(path); ns != "" {
			ok := false
			for _, allowed := range u.NS {
				if allowed == ns {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		}
	}
	return true
}

// EnsureDir 是凭证文件父目录创建辅助（0700）。
func EnsureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}
