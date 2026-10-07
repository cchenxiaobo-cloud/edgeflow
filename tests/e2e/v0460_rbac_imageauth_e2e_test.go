package e2e

// v0.46.0 e2e（spec 0019）：RBAC 角色矩阵 + 镜像凭证管理 + 默认零行为。
//
// 场景（独立 cloudcore 实例，无边缘——本版聚焦云端管理面）：
//   - A 默认零行为：RBAC off + 无凭证文件 → roles/users/image-auths 恒 403，
//     既有端点语义不变（无认证时无 Bearer 直接 200/404）；
//   - B RBAC on：创建 admin/operator/viewer 凭证 →
//       admin 全权（users CRUD + 业务写 + DELETE）；
//       operator 业务写 200、DELETE 403、users 面 403；
//       viewer 业务读 200、业务写 403；
//       坏 token 401；撤销 operator 后原 token 立即 401；
//   - C 镜像凭证：PUT 设置 → 列表脱敏（password ≤12 且 ≠ 明文）→
//       RBAC off 实例 503 未启用语义。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// v0460Sha256 计算凭证哈希（与 rbac.HashToken 同口径，e2e 预写引导文件用）。
func v0460Sha256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// v0460Do 带可选 Bearer 的请求辅助（返回状态码 + 响应体）。
func v0460Do(t *testing.T, method, url, token string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestV0460RBACImageAuthE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)
	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	// ── A. 默认零行为实例（RBAC off，无凭证文件）──
	cloud := v0400StartCloudcoreOnPorts(t, root, httpPort, hubPort)
	_ = cloud

	// 管理面恒 403（不泄露内部状态）
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/v1/roles"}, {"GET", "/api/v1/users"},
		{"POST", "/api/v1/users"}, {"DELETE", "/api/v1/users/x"},
	} {
		if code, body := v0460Do(t, c.m, base+c.p, "", nil); code != 403 {
			t.Fatalf("RBAC off %s %s = %d, want 403（body=%s）", c.m, c.p, code, body)
		}
	}
	// image-auths 列表：未启用 → 503（SET）路径之外，列表本身返回 200 空或 403
	// （无认证 → adminOf=false → 403）。契约语义：403 优先。
	if code, _ := v0460Do(t, "GET", base+"/api/v1/image-auths", "", nil); code != 403 {
		t.Fatalf("image-auths 无认证 = %d, want 403", code)
	}
	// 既有端点零变化：无认证 GET nodes 200（认证未启用）
	if code, _ := v0460Do(t, "GET", base+"/api/v1/nodes", "", nil); code != 200 {
		t.Fatalf("RBAC off GET nodes = %d, want 200（零行为被破坏）", code)
	}
	t.Logf("A 默认零行为 OK：管理面 403、既有端点语义不变")

	// ── B. RBAC on 实例（新端口新实例）──
	usersFile := filepath.Join(t.TempDir(), "users.json")
	imageAuthFile := filepath.Join(t.TempDir(), "imageauth.json")
	preUsers := `[{"id":"boot-admin","hash":"` + v0460Sha256("admin-pass-1") + `","role":"admin"},{"id":"op-1","hash":"` + v0460Sha256("op-pass-1") + `","role":"operator"},{"id":"view-1","hash":"` + v0460Sha256("view-pass-1") + `","role":"viewer"}]`
	if err := os.WriteFile(usersFile, []byte(preUsers), 0o600); err != nil {
		t.Fatalf("预写凭证文件: %v", err)
	}
	httpPort2, hubPort2 := reservePort(t), reservePort(t)
	base2 := fmt.Sprintf("http://127.0.0.1:%d", httpPort2)
	env := append(v0400CloudEnv(httpPort2, hubPort2),
		"EDGEFLOW_CLOUDCORE_RBAC=on",
		"EDGEFLOW_CLOUDCORE_RBAC_FILE="+usersFile,
		"EDGEFLOW_CLOUDCORE_IMAGEAUTH_FILE="+imageAuthFile,
		"EDGEFLOW_CLOUDCORE_IMAGEAUTH_KEY=test-key-material",
	)
	cloud2 := startProcess(t, "cloudcore-v046", filepath.Join(binDir, "cloudcore"), nil, env)
	_ = cloud2
	waitHTTP(t, 15*time.Second, base2+"/healthz", nil)

	// admin（env 回退语义先不设 env token——首个 admin 凭证如何来？
	// B 段裁决：RBAC on 且凭证表空 → env 单令牌回退；表空 + env 未设 →
	// 管理面 401（无引导漏洞）。e2e 用预写引导文件（启动前落盘）。
	// 实例2 已用同一预写内容启动；实例3 再单独预写：
	httpPort3, hubPort3 := reservePort(t), reservePort(t)
	base3 := fmt.Sprintf("http://127.0.0.1:%d", httpPort3)
	usersFile3 := filepath.Join(t.TempDir(), "users3.json")
	if err := os.WriteFile(usersFile3, []byte(preUsers), 0o600); err != nil {
		t.Fatalf("预写凭证文件3: %v", err)
	}
	env3 := append(v0400CloudEnv(httpPort3, hubPort3),
		"EDGEFLOW_CLOUDCORE_RBAC=on",
		"EDGEFLOW_CLOUDCORE_RBAC_FILE="+usersFile3,
	)
	cloud3 := startProcess(t, "cloudcore-v046b", filepath.Join(binDir, "cloudcore"), nil, env3)
	_ = cloud3
	waitHTTP(t, 15*time.Second, base3+"/healthz", nil)
	base = base3 // 后续断言用 RBAC on 实例

	// admin：全权
	if code, body := v0460Do(t, "GET", base+"/api/v1/roles", "admin-pass-1", nil); code != 200 || !bytes.Contains([]byte(body), []byte("operator")) {
		t.Fatalf("admin GET roles = %d body=%s", code, body)
	}
	// admin 创建 viewer 凭证（响应含明文 token 一次）
	code, body := v0460Do(t, "POST", base+"/api/v1/users", "admin-pass-1", map[string]any{
		"id": "v-2", "token": "v2-pass", "role": "viewer", "scopeNs": []string{"ns-x"},
	})
	if code != 201 || !bytes.Contains([]byte(body), []byte("v2-pass")) {
		t.Fatalf("admin POST users = %d body=%s", code, body)
	}
	// operator：业务写 200、DELETE 403、users 面 403
	if code, _ := v0460Do(t, "POST", base+"/api/v1/models", "op-pass-1", map[string]any{
		"name": "m-rbac", "type": "anomaly-detection",
	}); code != 200 {
		t.Fatalf("operator POST models = %d, want 200", code)
	}
	if code, _ := v0460Do(t, "DELETE", base+"/api/v1/models/m-rbac", "op-pass-1", nil); code != 403 {
		t.Fatalf("operator DELETE = %d, want 403", code)
	}
	if code, _ := v0460Do(t, "GET", base+"/api/v1/users", "op-pass-1", nil); code != 403 {
		t.Fatalf("operator GET users = %d, want 403", code)
	}
	// viewer：读 200、写 403
	if code, _ := v0460Do(t, "GET", base+"/api/v1/models", "view-pass-1", nil); code != 200 {
		t.Fatalf("viewer GET models = %d, want 200", code)
	}
	if code, _ := v0460Do(t, "POST", base+"/api/v1/models", "view-pass-1", map[string]any{
		"name": "m-v", "type": "anomaly-detection",
	}); code != 403 {
		t.Fatalf("viewer POST = %d, want 403", code)
	}
	// 坏 token 401
	if code, _ := v0460Do(t, "GET", base+"/api/v1/nodes", "wrong-pass", nil); code != 401 {
		t.Fatalf("坏 token = %d, want 401", code)
	}
	// 撤销 + 即时生效
	if code, _ := v0460Do(t, "DELETE", base+"/api/v1/users/v-2", "admin-pass-1", nil); code != 200 {
		t.Fatalf("admin DELETE users/v-2 = %d, want 200", code)
	}
	if code, _ := v0460Do(t, "GET", base+"/api/v1/models", "v2-pass", nil); code != 401 {
		t.Fatalf("撤销后原 token = %d, want 401", code)
	}
	// 凭证列表脱敏（hashPrefix ≤12）
	code, body = v0460Do(t, "GET", base+"/api/v1/users", "admin-pass-1", nil)
	if code != 200 || bytes.Contains([]byte(body), []byte("admin-pass-1")) {
		t.Fatalf("users 列表泄露明文: %d %s", code, body)
	}
	t.Logf("B RBAC 角色矩阵 OK：admin 全权 / operator 无删 / viewer 只读 / 401/403 语义 / 撤销即时生效")

	// ── C. 镜像凭证 503 语义（RBAC on 但未配 IMAGEAUTH_FILE，实例3）──
	if code, _ := v0460Do(t, "PUT", base+"/api/v1/image-auths/priv.registry.io:5000", "admin-pass-1", map[string]any{
		"username": "ops", "password": "pull-secret-9",
	}); code != 503 {
		t.Fatalf("未启用 image-auth PUT = %d, want 503", code)
	}
	if code, body := v0460Do(t, "GET", base+"/api/v1/image-auths", "admin-pass-1", nil); code != 200 || !bytes.Contains([]byte(body), []byte(`"enabled":false`)) {
		t.Fatalf("未启用 image-auths 列表 = %d %s, want 200 enabled=false", code, body)
	}

	// ── D. 镜像凭证全链（实例2：RBAC on + IMAGEAUTH_FILE + KEY 已配）──
	// 注意实例2 的凭证文件已在启动前预写（usersFile 同一预写内容）
	if code, _ := v0460Do(t, "PUT", base2+"/api/v1/image-auths/priv.registry.io:5000", "admin-pass-1", map[string]any{
		"username": "ops", "password": "pull-secret-9",
	}); code != 200 {
		t.Fatalf("PUT image-auths = %d", code)
	}
	code, body = v0460Do(t, "GET", base2+"/api/v1/image-auths", "admin-pass-1", nil)
	if code != 200 {
		t.Fatalf("GET image-auths = %d", code)
	}
	if bytes.Contains([]byte(body), []byte("pull-secret-9")) {
		t.Fatalf("image-auths 列表泄露明文: %s", body)
	}
	if !bytes.Contains([]byte(body), []byte(`"deployable":true`)) {
		t.Fatalf("配置了 key 的条目应 deployable: %s", body)
	}
	// 落盘文件无明文
	if iaData, err := os.ReadFile(imageAuthFile); err != nil || bytes.Contains(iaData, []byte("pull-secret-9")) {
		t.Fatalf("凭证文件泄露明文或读取失败: err=%v data=%s", err, iaData)
	}
	// 删除
	if code, _ := v0460Do(t, "DELETE", base2+"/api/v1/image-auths/priv.registry.io:5000", "admin-pass-1", nil); code != 200 {
		t.Fatalf("DELETE image-auths = %d, want 200", code)
	}
	if code, _ := v0460Do(t, "DELETE", base2+"/api/v1/image-auths/priv.registry.io:5000", "admin-pass-1", nil); code != 404 {
		t.Fatalf("重复 DELETE = %d, want 404", code)
	}
	t.Logf("C/D 镜像凭证 OK：未启用 503、设置/列表脱敏/deployable、落盘无明文、删除 200/404")
}
