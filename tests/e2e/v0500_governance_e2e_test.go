package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestV0500GovernanceE2E（v0.50.0，spec 0023）：区域层与标签治理六段——
// US-1 区域树 / US-2 节点归属 / US-3 打标 / US-4 组合查询 / US-5 越权 / US-6
// 防环与删除约束。纯云端管理面（不起 edgecore；归属为静态管理数据，spec
// 裁决 4 不校验节点存在性）。复用 v0490 云端环境（RBAC on + 三凭证）。
func TestV0500GovernanceE2E(t *testing.T) {
	buildBinaries(t)
	root := t.TempDir()
	httpPort, hubPort := reservePort(t), reservePort(t)
	usersFile := filepath.Join(t.TempDir(), "users.json")
	preUsers := `[{"id":"boot-admin","hash":"` + v0460Sha256("admin-pass-1") + `","role":"admin"},` +
		`{"id":"op-1","hash":"` + v0460Sha256("op-pass-1") + `","role":"operator"},` +
		`{"id":"view-1","hash":"` + v0460Sha256("view-pass-1") + `","role":"viewer"}]`
	if err := os.WriteFile(usersFile, []byte(preUsers), 0o600); err != nil {
		t.Fatal(err)
	}
	p := v0490StartCloudcore(t, root, httpPort, hubPort, usersFile, filepath.Join(t.TempDir(), "term-audit.jsonl"))
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)

	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"
	const adminToken = "admin-pass-1"

	// ─── US-1 区域树 ───
	t.Logf("US-1 区域树：创建根/子 → 树形 → 更新移动 → 删除")
	t0 := time.Now()
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "华东", "description": "大区"})
	if code != http.StatusCreated {
		t.Fatalf("US-1 创建根 → %d body=%s", code, body)
	}
	var region struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	json.Unmarshal([]byte(body), &region)
	if region.ID == "" {
		t.Fatalf("US-1 根区域无 ID: %s", body)
	}
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "车间A", "parentId": region.ID})
	if code != http.StatusCreated {
		t.Fatalf("US-1 创建子 → %d body=%s", code, body)
	}
	var child struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(body), &child)

	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/regions", viewToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-1 树查询 → %d", code)
	}
	var tree struct {
		Regions []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Children []struct {
				ID string `json:"id"`
			} `json:"children"`
			NodeCount int `json:"nodeCount"`
		} `json:"regions"`
		Total int `json:"total"`
	}
	json.Unmarshal([]byte(body), &tree)
	if tree.Total != 1 || len(tree.Regions) != 1 || tree.Regions[0].ID != region.ID || len(tree.Regions[0].Children) != 1 {
		t.Fatalf("US-1 树形不符: %s", body)
	}
	// 更新：改名 + 移动车间A 到根，再移回（覆盖 parentId 指针语义）
	rootParent := ""
	code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/regions/"+child.ID, opToken,
		map[string]any{"name": "车间A2", "parentId": rootParent})
	if code != http.StatusOK {
		t.Fatalf("US-1 移到根 → %d", code)
	}
	code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/regions/"+child.ID, opToken,
		map[string]any{"parentId": region.ID})
	if code != http.StatusOK {
		t.Fatalf("US-1 移回 → %d", code)
	}
	// 删除空子区域 → 200；重复删 → 404
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/regions/"+child.ID, adminToken, nil); code != http.StatusOK {
		t.Fatalf("US-1 删除空区 → %d", code)
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/regions/"+child.ID, adminToken, nil); code != http.StatusNotFound {
		t.Fatalf("US-1 重复删除 → %d（want 404）", code)
	}
	t.Logf("US-1 完成（%dms）", time.Since(t0).Milliseconds())

	// ─── US-2 节点归属 ───
	t.Logf("US-2 节点归属：批量 assign → skipped → 幂等换区")
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "车间B"})
	json.Unmarshal([]byte(body), &region) // 复用结构体：车间B
	if code != http.StatusCreated {
		t.Fatalf("US-2 创建车间B → %d", code)
	}
	shopB := region.ID
	_, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "车间C"})
	json.Unmarshal([]byte(body), &region)
	shopC := region.ID

	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions/"+shopB+"/nodes", opToken,
		map[string]any{"nodeIds": []string{"e2e-v050-1", "", "e2e-v050-2"}})
	if code != http.StatusOK {
		t.Fatalf("US-2 归属 → %d body=%s", code, body)
	}
	var assignRes struct {
		Assigned int `json:"assigned"`
		Skipped  int `json:"skipped"`
	}
	json.Unmarshal([]byte(body), &assignRes)
	if assignRes.Assigned != 2 || assignRes.Skipped != 1 {
		t.Fatalf("US-2 归属计数不符: %s", body)
	}
	// 幂等换区：e2e-v050-1 从 B 迁到 C
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions/"+shopC+"/nodes", opToken,
		map[string]any{"nodeIds": []string{"e2e-v050-1"}})
	json.Unmarshal([]byte(body), &assignRes)
	if code != http.StatusOK || assignRes.Assigned != 1 {
		t.Fatalf("US-2 换区不符: %d %s", code, body)
	}
	// B 只剩 e2e-v050-2
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/governance/nodes?region="+shopB, adminToken, nil)
	var govView struct {
		Nodes []struct {
			NodeID string `json:"nodeId"`
		} `json:"nodes"`
		Total int `json:"total"`
	}
	json.Unmarshal([]byte(body), &govView)
	if code != http.StatusOK || govView.Total != 1 || govView.Nodes[0].NodeID != "e2e-v050-2" {
		t.Fatalf("US-2 换区后 B 视图不符: %d %s", code, body)
	}
	t.Logf("US-2 完成（换区迁移已断言）")

	// ─── US-3 打标 ───
	t.Logf("US-3 打标：upsert → 幂等 → null 删标")
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/tags", opToken,
		map[string]any{"anchorType": "node", "anchorId": "e2e-v050-2", "tags": map[string]any{"line": "A1", "floor": "3"}})
	if code != http.StatusOK {
		t.Fatalf("US-3 打标 → %d body=%s", code, body)
	}
	var tagRes struct {
		Changed int `json:"changed"`
	}
	json.Unmarshal([]byte(body), &tagRes)
	if tagRes.Changed != 2 {
		t.Fatalf("US-3 首打应 changed=2: %s", body)
	}
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/tags", opToken,
		map[string]any{"anchorType": "node", "anchorId": "e2e-v050-2", "tags": map[string]any{"line": "A1"}})
	json.Unmarshal([]byte(body), &tagRes)
	if code != http.StatusOK || tagRes.Changed != 0 {
		t.Fatalf("US-3 幂等重打应 changed=0: %d %s", code, body)
	}
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/tags", opToken,
		map[string]any{"anchorType": "node", "anchorId": "e2e-v050-2", "tags": map[string]any{"floor": nil}})
	json.Unmarshal([]byte(body), &tagRes)
	if code != http.StatusOK || tagRes.Changed != 1 {
		t.Fatalf("US-3 删标应 changed=1: %d %s", code, body)
	}
	// 校验拒绝（坏 key）
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/tags", opToken,
		map[string]any{"anchorType": "node", "anchorId": "x", "tags": map[string]any{"bad key": "v"}}); code != http.StatusBadRequest {
		t.Fatalf("US-3 坏 key → %d（want 400）", code)
	}
	t.Logf("US-3 完成（upsert/幂等/删标/校验）")

	// ─── US-4 组合查询 ───
	t.Logf("US-4 组合查询：region 过滤 → tag 过滤 → AND 交集")
	// e2e-v050-2 在 B 区、line=A1；e2e-v050-1 在 C 区、无标签
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/governance/nodes?tag=line:A1", adminToken, nil)
	json.Unmarshal([]byte(body), &govView)
	if code != http.StatusOK || govView.Total != 1 || govView.Nodes[0].NodeID != "e2e-v050-2" {
		t.Fatalf("US-4 tag 过滤不符: %d %s", code, body)
	}
	// AND：B 区 × line=A1 → 命中；B 区 × line=ZZ → 空
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/governance/nodes?region="+shopB+"&tag=line:A1", adminToken, nil)
	json.Unmarshal([]byte(body), &govView)
	if code != http.StatusOK || govView.Total != 1 {
		t.Fatalf("US-4 AND 命中不符: %d %s", code, body)
	}
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/governance/nodes?region="+shopB+"&tag=line:ZZ", adminToken, nil)
	json.Unmarshal([]byte(body), &govView)
	if code != http.StatusOK || govView.Total != 0 {
		t.Fatalf("US-4 AND 排除不符: %d %s", code, body)
	}
	// 全量视图（无过滤）≥2
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/governance/nodes", adminToken, nil)
	json.Unmarshal([]byte(body), &govView)
	if code != http.StatusOK || govView.Total < 2 {
		t.Fatalf("US-4 全量视图不符: %d %s", code, body)
	}
	// 不存在区域 → 404
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/governance/nodes?region=rg-nothing", adminToken, nil); code != http.StatusNotFound {
		t.Fatalf("US-4 未知区域 → %d（want 404）", code)
	}
	t.Logf("US-4 完成（tag/AND/全量/404）")

	// ─── US-5 越权 ───
	t.Logf("US-5 越权：viewer 写 403 ×2 / 匿名 401 / viewer 读 200")
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", viewToken,
		map[string]any{"name": "x"}); code != http.StatusForbidden {
		t.Fatalf("US-5 viewer 建区 → %d（want 403）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/tags", viewToken,
		map[string]any{"anchorType": "node", "anchorId": "x", "tags": map[string]any{"k": "v"}}); code != http.StatusForbidden {
		t.Fatalf("US-5 viewer 打标 → %d（want 403）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/regions", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("US-5 匿名读 → %d（want 401）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/regions/rg-x", viewToken, nil); code != http.StatusForbidden {
		t.Fatalf("US-5 viewer 删区 → %d（want 403）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/regions/rg-x", opToken, nil); code != http.StatusForbidden {
		t.Fatalf("US-5 operator 删区 → %d（want 403，v0.46 operator 无 delete）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/regions", viewToken, nil); code != http.StatusOK {
		t.Fatalf("US-5 viewer 读 → %d（want 200）", code)
	}
	t.Logf("US-5 完成（403×2/401/200）")

	// ─── US-6 防环与删除约束 ───
	t.Logf("US-6 防环：A→B→C 移动成环 409；删除约束 409×2 → 迁出后可删")
	_, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken, map[string]any{"name": "环A"})
	json.Unmarshal([]byte(body), &region)
	ra := region.ID
	_, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "环B", "parentId": ra})
	json.Unmarshal([]byte(body), &region)
	rb := region.ID
	_, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "环C", "parentId": rb})
	json.Unmarshal([]byte(body), &region)
	rc := region.ID
	// A 移到 C 下（C 在 A 子树内）→ 409
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/regions/"+ra, opToken,
		map[string]any{"parentId": rc}); code != http.StatusConflict {
		t.Fatalf("US-6 成环 → %d（want 409）", code)
	}
	// 自环
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/regions/"+ra, opToken,
		map[string]any{"parentId": ra}); code != http.StatusConflict {
		t.Fatalf("US-6 自环 → %d（want 409）", code)
	}
	// 合法移动：C 移到根（后续删除约束可独立归因）
	rootParent = ""
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/regions/"+rc, opToken,
		map[string]any{"parentId": rootParent}); code != http.StatusOK {
		t.Fatalf("US-6 C 移到根 → %d", code)
	}
	// 删除约束①：A 有子 B → 409
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/regions/"+ra, adminToken, nil); code != http.StatusConflict {
		t.Fatalf("US-6 有子删除 → %d（want 409）", code)
	}
	// 删除约束②：B 有节点（无子）→ 409
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions/"+rb+"/nodes", opToken,
		map[string]any{"nodeIds": []string{"e2e-v050-3"}}); code != http.StatusOK {
		t.Fatalf("US-6 归属失败: %d %s", code, body)
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/regions/"+rb, adminToken, nil); code != http.StatusConflict {
		t.Fatalf("US-6 有节点删除 → %d（want 409）", code)
	}
	// 迁出后 B 可删；A 随之无子也可删
	if code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions/"+rc+"/nodes", opToken,
		map[string]any{"nodeIds": []string{"e2e-v050-3"}}); code != http.StatusOK {
		t.Fatalf("US-6 迁出失败")
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/regions/"+rb, adminToken, nil); code != http.StatusOK {
		t.Fatalf("US-6 迁出后删除 → %d", code)
	}
	if code, _ = v0490TermDo(t, http.MethodDelete, base+"/api/v1/regions/"+ra, adminToken, nil); code != http.StatusOK {
		t.Fatalf("US-6 级联后删根 → %d", code)
	}
	t.Logf("US-6 完成（409×3 + 200×3）")
}
