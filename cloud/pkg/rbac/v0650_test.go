// v0650_test.go：导出/导入端点 RBAC 执行层单测（v0.65.0 spec 0038；
// R5 复核 P1-1 处置——路径级 facet：adminOnlyPaths/operatorPlusPaths）。
package rbac

import "testing"

// 三角色 × 三导出/导入路径的 Can() 判定矩阵：
//   - audit/entries/export：admin/operator ✅、viewer ❌（opplus）
//   - forwards/export、forwards/import：仅 admin ✅（admin facet）
func TestV0650ExportImportRBAC(t *testing.T) {
	cases := []struct {
		role         string
		method, path string
		want         bool
	}{
		{RoleAdmin, "GET", "/api/v1/audit/entries/export", true},
		{RoleOperator, "GET", "/api/v1/audit/entries/export", true},
		{RoleViewer, "GET", "/api/v1/audit/entries/export", false},
		{RoleAdmin, "GET", "/api/v1/forwards/export", true},
		{RoleOperator, "GET", "/api/v1/forwards/export", false},
		{RoleViewer, "GET", "/api/v1/forwards/export", false},
		{RoleAdmin, "POST", "/api/v1/forwards/import", true},
		{RoleOperator, "POST", "/api/v1/forwards/import", false},
		{RoleViewer, "POST", "/api/v1/forwards/import", false},
		// 回归锚：既有面不受新 facet 影响。
		{RoleViewer, "GET", "/api/v1/alarms", true},
		{RoleOperator, "DELETE", "/api/v1/forwards/fw-1", false},
		{RoleAdmin, "GET", "/api/v1/roles", true},
		{RoleOperator, "GET", "/api/v1/roles", false},
	}
	for _, c := range cases {
		u := User{ID: "t", Role: c.role}
		if got := Can(u, c.method, c.path); got != c.want {
			t.Errorf("Can(%v, %s, %s) = %v, want %v", c.role, c.method, c.path, got, c.want)
		}
	}
}
