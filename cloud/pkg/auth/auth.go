// Package auth 实现 cloudcore 管理 API 的 Token 认证中间件（WBS 7.2 RBAC 前置）。
//
// 设计要点：
//   - 开关可配置：EDGEFLOW_CLOUDCORE_AUTH=on 时启用，默认 off（向后兼容，
//     现有未配置令牌的部署/测试行为不变）；
//   - 令牌来源：环境变量 EDGEFLOW_CLOUDCORE_API_TOKEN（单一共享令牌，
//     v0.1 语义等价于"持有令牌即管理员"；后续可演进为多用户 RBAC）；
//   - 校验规则：Authorization: Bearer <token>，constant-time 比较防时序侧信道；
//   - 审计联动：认证通过后把身份写入请求 context（audit.SetIdentity），
//     由审计中间件落盘；认证失败（401）由外层审计中间件记录 operator=anonymous。
//
// 职责边界：本包只做「身份认证」（你是谁），不做「授权」（你能干什么）；
// 当前单令牌模型下认证通过即视为管理员，多角色授权留给后续版本。
package auth

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"edgeflow/cloud/pkg/audit"
)

// 环境变量约定。
const (
	// EnvAuth 是认证开关环境变量名：值为 "on" 时启用 Token 认证。
	EnvAuth = "EDGEFLOW_CLOUDCORE_AUTH"
	// EnvToken 是 API 令牌环境变量名：启用认证时必须设置，否则启动报错（fail-fast）。
	EnvToken = "EDGEFLOW_CLOUDCORE_API_TOKEN"
)

// IdentityToken 是持有有效 API 令牌的调用方在审计日志中的身份标识。
// 单令牌模型下所有认证通过者共用此身份。
const IdentityToken = "token"

// EnabledFromEnv 读取认证开关：EDGEFLOW_CLOUDCORE_AUTH=on 视为启用。
func EnabledFromEnv() bool {
	return os.Getenv(EnvAuth) == "on"
}

// TokenFromEnv 读取 API 令牌（EDGEFLOW_CLOUDCORE_API_TOKEN）。
func TokenFromEnv() string {
	return os.Getenv(EnvToken)
}

// bearerPrefix 是 Authorization 头的 Bearer 方案前缀（大小写不敏感匹配）。
const bearerPrefix = "Bearer "

// Middleware 返回 Token 认证中间件。
//
// 行为：
//   - 未携带 Authorization 头 / 非 Bearer 方案 / 令牌不匹配 → 401 +
//     WWW-Authenticate: Bearer 响应头；
//   - 令牌匹配 → 写入审计身份后放行。
func Middleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !validBearer(r, token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="cloudcore"`)
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			// 认证成功：把身份写入 context，供审计中间件记录 operator
			audit.SetIdentity(r.Context(), IdentityToken)
			next.ServeHTTP(w, r)
		})
	}
}

// validBearer 校验请求是否携带正确的 Bearer 令牌。
// 令牌比较用 constant-time（subtle.ConstantTimeCompare），避免时序侧信道
// 泄露令牌内容（长度差异无法避免，属可接受范围）。
func validBearer(r *http.Request, token string) bool {
	h := r.Header.Get("Authorization")
	if len(h) <= len(bearerPrefix) || !strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return false
	}
	presented := strings.TrimSpace(h[len(bearerPrefix):])
	if presented == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}
