// v0460_rbac_api.go —— v0.46.0（spec 0019）云端装配：RBAC 开关/凭证表装配、
// 镜像凭证（imageauth）装配与 image-auths 管理 API、users/roles 管理面挂载。
//
// 装配口径（默认零行为）：
//   - EDGEFLOW_CLOUDCORE_RBAC != on → RBAC 中间件不装配，auth 单令牌语义
//     与 v0.45 逐字节一致（authEnabled 路径保留）；
//   - EDGEFLOW_CLOUDCORE_IMAGEAUTH_FILE 未设置 → imageAuthStore = nil，
//     零行为（不挂 API、不注入查表）。
package main

import (
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"edgeflow/cloud/pkg/auth"
	"edgeflow/cloud/pkg/imageauth"
	"edgeflow/cloud/pkg/rbac"
	"edgeflow/pkg/log"
)

// imageAuthStore 是全局镜像凭证表（装配层赋值；nil = 未启用）。
// 仅在 main.go 装配与 v0460 API 挂载处使用。
var imageAuthStore *imageauth.Store

// rbacUsers 是全局 RBAC 凭证表（装配层赋值；nil = RBAC off）。
var rbacUsers *rbac.Store

// rbacEnabledFromEnv 读取 RBAC 开关（EDGEFLOW_CLOUDCORE_RBAC=on）。
func rbacEnabledFromEnv() bool { return os.Getenv(rbac.EnvRBAC) == "on" }

// assembleRBAC 装配 RBAC 凭证表（RBAC on 时调用；文件必须配置且可加载，
// 否则 fail-fast 返回错误——认证语义收紧的开关不允许静默降级）。
func assembleRBAC() (*rbac.Store, error) {
	path := os.Getenv(rbac.EnvUsersFile)
	if path == "" {
		return nil, fmt.Errorf("%s=on 但 %s 未设置（RBAC 开启必须配置凭证文件）", rbac.EnvRBAC, rbac.EnvUsersFile)
	}
	if err := rbac.EnsureDir(path); err != nil {
		return nil, fmt.Errorf("RBAC 凭证目录创建失败: %w", err)
	}
	store, err := rbac.LoadUsers(path)
	if err != nil {
		return nil, fmt.Errorf("RBAC 凭证表加载失败: %w", err)
	}
	rbac.EnsureFilePerm(path)
	n := len(store.List())
	log.Infof("[rbac] 已启用（凭证 %d 条，文件 %s）；env 单令牌作为 admin 回退保留", n, path)
	if n == 0 {
		log.Warnf("[rbac] 凭证表为空：所有请求将回退 env 单令牌（如已配置）或被拒绝")
	}
	return store, nil
}

// assembleImageAuth 装配镜像凭证表（未配置文件路径 → nil 零行为）。
func assembleImageAuth() *imageauth.Store {
	path := os.Getenv(imageauth.EnvFile)
	if path == "" {
		return nil
	}
	if err := imageauth.EnsureDir(path); err != nil {
		log.Errorf("[imageauth] 凭证目录创建失败: %v", err)
		return nil
	}
	key := os.Getenv(imageauth.EnvKey)
	aead, err := imageauth.NewKey(key)
	if err != nil {
		log.Errorf("[imageauth] 静态加密密钥派生失败: %v", err)
		return nil
	}
	if key == "" {
		log.Warnf("[imageauth] 未配置 %s：凭证仅存哈希，下发链路不可用（列表/校验可用）", imageauth.EnvKey)
	}
	store, err := LoadImageAuth(path, aead)
	if err != nil {
		log.Errorf("[imageauth] 凭证表加载失败: %v", err)
		return nil
	}
	log.Infof("[imageauth] 已启用（凭证 %d 条，文件 %s，静态加密=%v）", len(store.List()), path, key != "")
	return store
}

// LoadImageAuth 是 imageauth.Load 的包内别名（便于测试注入）。
func LoadImageAuth(path string, aead cipher.AEAD) (*imageauth.Store, error) {
	return imageauth.Load(path, aead)
}

// registerV0460APIs 挂 v0.46.0 管理面 API（RBAC users/roles + image-auths）。
// adminOnly 判定由 RBAC 中间件的 facet 拦截承担（roles/users 面 operator/viewer
// 已 403）；RBAC off 时按「认证开启 → env 令牌=admin」语义判定，未开启认证时
// 这些端点默认拒绝（管理面不裸奔——比存量读端点更保守，spec §2 US-4）。
func registerV0460APIs(mux *http.ServeMux) {
	adminOf := func(r *http.Request) bool {
		if rbacUsers != nil {
			// RBAC on：facet 拦截在上游已完成；能到这里的只可能是 admin
			// （operator/viewer 被 Middleware 403）。env 回退身份 "token" 同 admin。
			return true
		}
		// RBAC off：env 单令牌语义（EDGEFLOW_CLOUDCORE_AUTH=on 时 Middleware
		// 已验证 token 到达此处）；认证未启用 → 管理面拒绝（403），
		// 避免无认证环境开放凭证操作。
		return auth.EnabledFromEnv()
	}
	if rbacUsers != nil {
		rbac.RegisterUsersAPI(mux, rbacUsers, adminOf)
	} else {
		// RBAC off：端点存在但恒 403（契约面稳定；不泄露内部状态）。
		// 显式逐条注册（契约源级扫描可解析）。
		mux.HandleFunc("GET /api/v1/roles", func(w http.ResponseWriter, r *http.Request) {
			writeV0460Forbidden(w)
		})
		mux.HandleFunc("GET /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
			writeV0460Forbidden(w)
		})
		mux.HandleFunc("POST /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
			writeV0460Forbidden(w)
		})
		mux.HandleFunc("DELETE /api/v1/users/{id}", func(w http.ResponseWriter, r *http.Request) {
			writeV0460Forbidden(w)
		})
	}
	registerImageAuthAPI(mux, adminOf)
}

// registerImageAuthAPI 挂镜像凭证管理 API（admin-only）：
//
//	PUT    /api/v1/image-auths/{registry}  设置/覆盖（body: {username,password}）
//	GET    /api/v1/image-auths             列表（脱敏）
//	DELETE /api/v1/image-auths/{registry}  删除
func registerImageAuthAPI(mux *http.ServeMux, adminOf func(*http.Request) bool) {
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if !adminOf(r) {
			writeV0460Forbidden(w)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/v1/image-auths", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) || imageAuthStore == nil {
			if imageAuthStore == nil {
				writeV0460JSON(w, http.StatusOK, map[string]any{"items": []any{}, "count": 0, "enabled": false})
				return
			}
			return
		}
		items := imageAuthStore.List()
		writeV0460JSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items), "enabled": true})
	})
	mux.HandleFunc("PUT /api/v1/image-auths/{registry}", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		if imageAuthStore == nil {
			writeV0460JSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "image-auth 未启用（需配置 EDGEFLOW_CLOUDCORE_IMAGEAUTH_FILE）"})
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeV0460JSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		registry := r.PathValue("registry")
		if err := imageAuthStore.Set(registry, body.Username, body.Password); err != nil {
			writeV0460JSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// 响应不含明文 password（spec US-6 硬规则）
		writeV0460JSON(w, http.StatusOK, map[string]any{
			"registry": imageauth.NormalizeRegistry(registry),
			"username": body.Username,
			"status":   "stored",
		})
	})
	mux.HandleFunc("DELETE /api/v1/image-auths/{registry}", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		if imageAuthStore == nil {
			writeV0460JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "image-auth 未启用"})
			return
		}
		if !imageAuthStore.Delete(r.PathValue("registry")) {
			writeV0460JSON(w, http.StatusNotFound, map[string]string{"error": "registry not found"})
			return
		}
		writeV0460JSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("registry")})
	})
}

func writeV0460Forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden", "required": "admin"})
}

func writeV0460JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// v0460EnvRaw 是 os.Getenv 的包内别名（测试可注入的缝；当前直通）。
func v0460EnvRaw(k string) string { return os.Getenv(k) }
