package rbac

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"edgeflow/cloud/pkg/audit"
)

// Middleware 返回 RBAC 中间件（认证 + 授权一体，替代 auth.Middleware 装配位；
// RBAC on 时不装配 auth.Middleware，避免双重认证语义漂移）。
//
// 判定顺序与语义边界（复核 P1-2 收敛）：
//  1. RBAC 凭证表命中 → Can 判定；通过 → 审计身份 = 凭证 ID，放行；
//  2. 表未命中 → env 单令牌回退（命中 → admin 身份 "token"，放行）；
//  3. 都未命中 → 401 + WWW-Authenticate（未认证，与 auth 语义一致）；
//  4. 命中但 Can=false → 403 {"error":"forbidden","required":"<op>"}
//     （已认证但越权；无 WWW-Authenticate——401/403 分界：401=你是谁未知，
//     403=已知身份不被允许；RBAC off 时管理面恒 403 属「无认证环境主动
//     拒绝管理操作」，契约面稳定、不泄露内部状态）。
//     401/403 完整分界说明见 docs/API-SPEC.md §1.2 与 docs/SECURITY-GUIDE.md。
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
// 两侧先各算 SHA-256（定长 32 字节），再 subtle.ConstantTimeCompare——
// 显式常时比较，不依赖「hex 定长 + string ==」的隐式事实（复核 P1-1）。
func bearerValid(r *http.Request, envToken string) bool {
	t := bearerToken(r)
	if t == "" || envToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashToken(t)), []byte(HashToken(envToken))) == 1
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
