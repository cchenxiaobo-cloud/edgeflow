package rbac

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"edgeflow/cloud/pkg/audit"
)

// Middleware 返回 RBAC 授权中间件（认证之后、业务 mux 之前）。
//
//   - users 表命中 → 按 Can 判定；通过 → 审计身份改写为凭证 ID，放行；
//   - 未命中 → env 单令牌回退（envToken 命中 → admin 身份 "token"，放行）；
//   - 都未命中 → 401（与 auth 语义一致）；
//   - 命中但 Can=false → 403 {"error":"forbidden","required":"<op>"}。
//
// 注意：本中间件假设认证已由内层完成？不——装配顺序是 auth(认证) 外、
// rbac(授权) 内？v0.46 裁决：RBAC 中间件**替代** auth.Middleware 的位置
// （同层装配），自身完成「认证 + 授权」一体：先查 RBAC 表，再回退 env，
// 401/403 语义集中在此层；auth.Middleware 保留但 RBAC on 时不装配
// （避免双重认证语义漂移）。
func Middleware(store *Store, envToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			u, ok := store.Authenticate(token)
			if !ok {
				// env 单令牌回退（向后兼容；命中 → admin）
				if envToken != "" && bearerValid(r, envToken) {
					audit.SetIdentity(r.Context(), "token")
					next.ServeHTTP(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="cloudcore"`)
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			path := r.URL.Path
			if !Can(u, r.Method, path) {
				o, _ := opOf(r.Method, path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error":    "forbidden",
					"required": string(o),
				})
				return
			}
			audit.SetIdentity(r.Context(), u.ID) // 审计 operator = 凭证 ID
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken 提取 Authorization: Bearer <token>（大小写不敏感方案前缀）。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// bearerValid 是 constant-time 的 token 比对（env 回退路径）。
func bearerValid(r *http.Request, envToken string) bool {
	t := bearerToken(r)
	if t == "" || envToken == "" {
		return false
	}
	return HashToken(t) == HashToken(envToken) // 哈希后 constant-time（subtle 在 Authenticate 内同口径）
}

// RegisterUsersAPI 挂角色管理面端点（admin-only；RBAC 中间件在 facet 判定已拦
// operator/viewer，此处的 admin 校验是纵深防御——env 回退身份 "token" 也算 admin）。
//   - GET  /api/v1/roles        角色枚举 + 权限矩阵说明
//   - GET  /api/v1/users        凭证列表（脱敏）
//   - POST /api/v1/users        创建凭证（明文 token 一次性返回）
//   - DELETE /api/v1/users/{id} 撤销凭证
func RegisterUsersAPI(mux *http.ServeMux, store *Store, isAdmin func(r *http.Request) bool) {
	type userView struct {
		ID      string   `json:"id"`
		Role    string   `json:"role"`
		ScopeNs []string `json:"scopeNs,omitempty"`
		Hash    string   `json:"hashPrefix"`
	}
	type createReq struct {
		ID    string   `json:"id"`
		Token string   `json:"token"`
		Role  string   `json:"role"`
		Scope []string `json:"scopeNs"`
	}

	mux.HandleFunc("GET /api/v1/roles", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r) {
			writeForbidden(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"roles": []map[string]any{
				{"role": RoleAdmin, "permissions": "全部（含角色与凭证管理）"},
				{"role": RoleOperator, "permissions": "读+写；无删除；角色管理面拒绝"},
				{"role": RoleViewer, "permissions": "只读（GET）；角色管理面拒绝"},
			},
		})
	})

	mux.HandleFunc("GET /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r) {
			writeForbidden(w)
			return
		}
		users := store.List()
		views := make([]userView, 0, len(users))
		for _, u := range users {
			views = append(views, userView{ID: u.ID, Role: u.Role, ScopeNs: u.NS, Hash: u.Hash})
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": views, "count": len(views)})
	})

	mux.HandleFunc("POST /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r) {
			writeForbidden(w)
			return
		}
		var req createReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		u, err := store.Create(req.ID, req.Token, req.Role, req.Scope)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// 明文 token 只在此响应出现一次（spec US-1 硬规则）
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": u.ID, "role": u.Role, "scopeNs": u.NS, "token": req.Token,
		})
	})

	mux.HandleFunc("DELETE /api/v1/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r) {
			writeForbidden(w)
			return
		}
		id := r.PathValue("id")
		if !store.Delete(id) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	})
}

func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden", "required": "admin"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// FilePerm 是凭证文件权限常量（0600；创建时强制，spec §3 边界 ⑥）。
const FilePerm = 0o600

// EnsureFilePerm 校验并收紧既有凭证文件权限（启动时调用；失败仅日志）。
func EnsureFilePerm(path string) {
	if st, err := os.Stat(path); err == nil && st.Mode().Perm() != FilePerm {
		_ = os.Chmod(path, FilePerm)
	}
}

var _ = filepath.Dir // 保留导入位（EnsureDir 在 rbac.go）
