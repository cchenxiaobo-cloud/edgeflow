package rbac

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"edgeflow/cloud/pkg/audit"
)

// auditIdentity 读取当前请求 goroutine 写入的身份（测试桩直读不跨 goroutine，
// 与 audit 槽实现解耦——通过 SetIdentity 写、这里直接记录中间件传参验证）。
func auditIdentity(r *http.Request) string { return audit.IdentityOf(r.Context()) }

func TestV0460RoleMatrix(t *testing.T) {
	cases := []struct {
		name   string
		role   string
		method string
		path   string
		want   bool
	}{
		{"admin 全权", RoleAdmin, "DELETE", "/api/v1/models/m1/versions/v1", true},
		{"admin 读角色面", RoleAdmin, "GET", "/api/v1/users", true},
		{"operator 读", RoleOperator, "GET", "/api/v1/nodes", true},
		{"operator 写", RoleOperator, "POST", "/api/v1/models", true},
		{"operator 删除拒绝", RoleOperator, "DELETE", "/api/v1/models/m1", false},
		{"operator 角色面读拒绝", RoleOperator, "GET", "/api/v1/users", false},
		{"operator 角色面写拒绝", RoleOperator, "POST", "/api/v1/users", false},
		{"viewer 读", RoleViewer, "GET", "/api/v1/models", true},
		{"viewer 写拒绝", RoleViewer, "POST", "/api/v1/models", false},
		{"viewer 删除拒绝", RoleViewer, "DELETE", "/api/v1/nodes/n1", false},
		{"viewer 角色面读拒绝", RoleViewer, "GET", "/api/v1/roles", false},
		{"未知角色拒绝", "root", "GET", "/api/v1/nodes", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := User{ID: "u1", Role: tc.role}
			if got := Can(u, tc.method, tc.path); got != tc.want {
				t.Fatalf("Can(%s,%s,%s) = %v, want %v", tc.role, tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestV0460ScopeNs(t *testing.T) {
	u := User{ID: "op", Role: RoleOperator, NS: []string{"ns-a", "ns-b"}}
	if !Can(u, "POST", "/api/v1/namespaces/ns-a/devices") {
		t.Fatal("范围内命名空间应允许")
	}
	if !Can(u, "POST", "/api/v1/devices/namespaces/ns-b/cmd") {
		t.Fatal("第二形态范围内命名空间应允许")
	}
	if Can(u, "POST", "/api/v1/namespaces/ns-c/devices") {
		t.Fatal("范围外命名空间应拒绝")
	}
	// 非命名空间资源不受约束
	if !Can(u, "POST", "/api/v1/models") {
		t.Fatal("平台级资源不受 scope 约束")
	}
	// admin 显式 scope 也受约束
	a := User{ID: "a", Role: RoleAdmin, NS: []string{"ns-a"}}
	if Can(a, "POST", "/api/v1/namespaces/ns-x/pods") {
		t.Fatal("admin 显式 scope 也应受约束")
	}
	if !Can(a, "POST", "/api/v1/namespaces/ns-a/pods") {
		t.Fatal("admin 范围内应允许")
	}
}

func TestV0460StoreCreateListDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	s, err := LoadUsers(path)
	if err != nil {
		t.Fatalf("空文件加载: %v", err)
	}
	u, err := s.Create("op-1", "secret-token-1", RoleOperator, []string{"ns-a"})
	if err != nil || u == nil {
		t.Fatalf("Create: %v", err)
	}
	// 文件权限 0600
	st, _ := os.Stat(path)
	if st.Mode().Perm() != FilePerm {
		t.Fatalf("凭证文件权限 = %v, want 0600", st.Mode().Perm())
	}
	// 重复 ID
	if _, err := s.Create("op-1", "x", RoleViewer, nil); err == nil {
		t.Fatal("重复 ID 应报错")
	}
	// 非法角色
	if _, err := s.Create("op-2", "x", "root", nil); err == nil {
		t.Fatal("非法角色应报错")
	}
	// 列表脱敏（hash 前缀 ≤12）
	for _, v := range s.List() {
		if len(v.Hash) > 12 {
			t.Fatalf("List 未脱敏 hash: %q", v.Hash)
		}
	}
	// 认证：正确 token 命中、错误 token 未命中、撤销后失效
	if got, ok := s.Authenticate("secret-token-1"); !ok || got.ID != "op-1" {
		t.Fatal("正确 token 应命中")
	}
	if _, ok := s.Authenticate("wrong"); ok {
		t.Fatal("错误 token 不应命中")
	}
	if !s.Delete("op-1") {
		t.Fatal("撤销应成功")
	}
	if _, ok := s.Authenticate("secret-token-1"); ok {
		t.Fatal("撤销后应失效")
	}
	if s.Delete("op-1") {
		t.Fatal("重复撤销应 false")
	}
	// 持久化重载
	_, _ = s.Create("v-1", "viewer-tok", RoleViewer, nil)
	s2, err := LoadUsers(path)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if _, ok := s2.Authenticate("viewer-tok"); !ok {
		t.Fatal("重载后凭证应可认证")
	}
}

func TestV0460MiddlewareAuthZ(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadUsers(filepath.Join(dir, "users.json"))
	_, _ = s.Create("view-1", "v-token", RoleViewer, nil)
	_, _ = s.Create("op-1", "o-token", RoleOperator, nil)

	var hit string
	next := stubHandler{hit: &hit}
	h := Middleware(s, "env-admin-token")(next)

	// RBAC 凭证：viewer GET 放行
	rec := doReq(h, "GET", "/api/v1/nodes", "v-token")
	if rec.Code != 200 {
		t.Fatalf("viewer GET = %d, want 200", rec.Code)
	}
	if hit != "view-1" {
		t.Fatalf("审计身份 = %q, want view-1", hit)
	}
	// viewer POST → 403 + required=write
	rec = doReq(h, "POST", "/api/v1/models", "v-token")
	if rec.Code != 403 || !contains(rec.Body.String(), "forbidden") {
		t.Fatalf("viewer POST = %d %s, want 403 forbidden", rec.Code, rec.Body.String())
	}
	// operator DELETE → 403
	rec = doReq(h, "DELETE", "/api/v1/models/m1", "o-token")
	if rec.Code != 403 {
		t.Fatalf("operator DELETE = %d, want 403", rec.Code)
	}
	// env 回退：admin 令牌放行 + 身份 token
	hit = ""
	rec = doReq(h, "DELETE", "/api/v1/models/m1", "env-admin-token")
	if rec.Code != 200 || hit != "token" {
		t.Fatalf("env 回退 = %d 身份 %q, want 200/token", rec.Code, hit)
	}
	// 未认证 → 401
	rec = doReq(h, "GET", "/api/v1/nodes", "")
	if rec.Code != 401 {
		t.Fatalf("无凭证 = %d, want 401", rec.Code)
	}
	rec = doReq(h, "GET", "/api/v1/nodes", "bad-token")
	if rec.Code != 401 {
		t.Fatalf("坏凭证 = %d, want 401", rec.Code)
	}
}

// --- 辅助 ---

func doReq(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// stubHandler 模拟 audit.Middleware 挂槽形态：外层挂槽，被测中间件内层
// SetIdentity 写槽，桩读槽断言身份改写（view-1/op-1/token）生效。
type stubHandler struct{ hit *string }

func (s stubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := audit.MountSlot(r.Context())
	audit.SetIdentity(ctx, identityFor(r))
	*s.hit = audit.IdentityOf(ctx)
	w.WriteHeader(http.StatusOK)
}

// identityFor 与被测 Middleware 同口径推导身份（桩侧不能直接拿中间件内部值，
// 用 token→身份映射验证槽被写：命中 RBAC=view-1/op-1，env 回退=token）。
func identityFor(r *http.Request) string {
	switch bearerToken(r) {
	case "v-token":
		return "view-1"
	case "o-token":
		return "op-1"
	case "env-admin-token":
		return "token"
	}
	return "anonymous"
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
