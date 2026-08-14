package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// do 用中间件包装一个返回 200 的 handler 并发起请求，返回响应。
func do(t *testing.T, mw func(http.Handler) http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)
	return rec
}

// TestMiddlewareNoToken 验证未携带 Authorization 头时返回 401（并带 WWW-Authenticate）。
func TestMiddlewareNoToken(t *testing.T) {
	mw := Middleware("secret-token")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	rec := do(t, mw, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌状态码 = %d，期望 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("401 响应应携带 WWW-Authenticate 响应头")
	}
}

// TestMiddlewareWrongToken 验证令牌错误 / 非 Bearer 方案 / 空令牌时均 401。
func TestMiddlewareWrongToken(t *testing.T) {
	mw := Middleware("secret-token")
	cases := []struct {
		name string
		auth string
	}{
		{name: "令牌错误", auth: "Bearer wrong"},
		{name: "非 Bearer 方案", auth: "Basic c2VjcmV0"},
		{name: "空令牌", auth: "Bearer "},
		{name: "前缀大小写不符", auth: "bearerX secret-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
			req.Header.Set("Authorization", tc.auth)
			if rec := do(t, mw, req); rec.Code != http.StatusUnauthorized {
				t.Errorf("状态码 = %d，期望 401", rec.Code)
			}
		})
	}
}

// TestMiddlewareValidToken 验证正确令牌放行（含 scheme 大小写不敏感）。
func TestMiddlewareValidToken(t *testing.T) {
	mw := Middleware("secret-token")
	for _, auth := range []string{"Bearer secret-token", "bearer secret-token", "BEARER secret-token"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
		req.Header.Set("Authorization", auth)
		if rec := do(t, mw, req); rec.Code != http.StatusOK {
			t.Errorf("Authorization=%q 状态码 = %d，期望 200", auth, rec.Code)
		}
	}
}

// TestMiddlewareTrailingSpace 验证 Bearer 后多余空白容忍（TrimSpace）。
func TestMiddlewareTrailingSpace(t *testing.T) {
	mw := Middleware("secret-token")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer   secret-token  ")
	if rec := do(t, mw, req); rec.Code != http.StatusOK {
		t.Errorf("带空白令牌状态码 = %d，期望 200", rec.Code)
	}
}

// TestEnvParsing 验证环境变量读取：默认 off、=on 启用、令牌透传。
func TestEnvParsing(t *testing.T) {
	t.Setenv(EnvAuth, "")
	t.Setenv(EnvToken, "")
	if EnabledFromEnv() {
		t.Error("未设置 EDGEFLOW_CLOUDCORE_AUTH 时应默认关闭（向后兼容）")
	}
	t.Setenv(EnvAuth, "on")
	t.Setenv(EnvToken, "t0ken")
	if !EnabledFromEnv() {
		t.Error("EDGEFLOW_CLOUDCORE_AUTH=on 时应启用")
	}
	if got := TokenFromEnv(); got != "t0ken" {
		t.Errorf("TokenFromEnv = %q，期望 t0ken", got)
	}
}
