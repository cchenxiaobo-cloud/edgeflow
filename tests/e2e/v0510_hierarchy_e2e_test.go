package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestV0510HierarchyE2E（v0.51.0，spec 0024）：区域协同层六段——
// US-1 三级视图（真节点注册+归属递归聚合）/ US-2 区域节点明细（含子树）/
// US-3 告警汇聚空态结构 / US-4 区域策略 KV / US-5 越权 / US-6 策略幂等。
// 复用 v0490 云端环境（RBAC on + 三凭证）+ 真实 edgecore 注册（replan 验收锚：
// 三级视图演示）。聚合计数矩阵由 hierarchyview 单测深覆盖；e2e 聚焦 API 面、
// RBAC、递归聚合与真节点在线语义。
func TestV0510HierarchyE2E(t *testing.T) {
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
	nodeID := "e2e-v051-1"

	// 真实 edgecore 注册（replan 验收锚「三级视图演示」需要在线节点）
	edge := v0490StartEdgecore(t, root, nodeID, hubPort, filepath.Join(t.TempDir(), "edge-term-audit.jsonl"))
	defer edge.stop()

	// ─── US-1 三级视图：建区域树 → 真节点注册 → 归属 → 递归聚合 ───
	t.Logf("US-1 三级视图：区域树 A→B + 真节点注册 + 归属递归聚合")
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "华东"})
	if code != http.StatusCreated {
		t.Fatalf("US-1 建根 → %d body=%s", code, body)
	}
	var regionA struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(body), &regionA)
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "车间B", "parentId": regionA.ID})
	if code != http.StatusCreated {
		t.Fatalf("US-1 建子 → %d", code)
	}
	var regionB struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(body), &regionB)

	// 轮询直到节点注册（summary.unassignedCount ≥ 1）
	var summary struct {
		UnassignedCount int `json:"unassignedCount"`
		NodeCount       int `json:"nodeCount"`
		OnlineCount     int `json:"onlineCount"`
		Children        []struct {
			ID          string `json:"id"`
			NodeCount   int    `json:"nodeCount"`
			OnlineCount int    `json:"onlineCount"`
			Children    []struct {
				ID        string `json:"id"`
				NodeCount int    `json:"nodeCount"`
			} `json:"children"`
		} `json:"children"`
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/summary", viewToken, nil)
		if code != http.StatusOK {
			t.Fatalf("US-1 summary → %d body=%s", code, body)
		}
		json.Unmarshal([]byte(body), &summary)
		if summary.UnassignedCount >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("US-1 节点 %s 60s 未出现在三级视图: %s", nodeID, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("US-1 节点 %s 已注册（unassigned=%d）", nodeID, summary.UnassignedCount)

	// 归属到子区域 B → 父 A 递归聚合可见
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions/"+regionB.ID+"/nodes", opToken,
		map[string]any{"nodeIds": []string{nodeID}}); code != http.StatusOK {
		t.Fatalf("US-1 归属 → %d body=%s", code, body)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/summary", viewToken, nil)
		if code != http.StatusOK {
			t.Fatalf("US-1 归属后 summary → %d", code)
		}
		var cur struct {
			UnassignedCount int `json:"unassignedCount"` // omitempty：省略时必须重置为 0，不能用外层残留变量
			NodeCount       int `json:"nodeCount"`
			Children        []struct {
				ID        string `json:"id"`
				NodeCount int    `json:"nodeCount"`
			} `json:"children"`
		}
		json.Unmarshal([]byte(body), &cur)
		summary.UnassignedCount = cur.UnassignedCount
		summary.NodeCount = cur.NodeCount
		if cur.UnassignedCount == 0 && len(cur.Children) == 1 && cur.Children[0].NodeCount == 1 {
			json.Unmarshal([]byte(body), &summary) // 完整重解到外层供后续断言
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("US-1 归属未反映到视图: %s", body)
		}
		time.Sleep(300 * time.Millisecond)
	}
	a := summary.Children[0]
	if a.ID != regionA.ID || a.NodeCount != 1 || a.OnlineCount != 1 {
		t.Fatalf("US-1 A 聚合不符: %+v", a)
	}
	if len(a.Children) != 1 || a.Children[0].ID != regionB.ID || a.Children[0].NodeCount != 1 {
		t.Fatalf("US-1 B 递归聚合不符: %+v", a.Children)
	}
	t.Logf("US-1 完成（注册→归属→A 含 B 递归聚合，在线 %d）", a.OnlineCount)

	// ─── US-2 区域节点明细（含子树）───
	t.Logf("US-2 区域节点明细：A 含子树 B 节点；未知区域 404")
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/regions/"+regionA.ID+"/nodes", viewToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-2 明细 → %d", code)
	}
	var rows []struct {
		NodeID string `json:"nodeId"`
		Online bool   `json:"online"`
		Region string `json:"region"`
	}
	json.Unmarshal([]byte(body), &rows)
	if len(rows) != 1 || rows[0].NodeID != nodeID || rows[0].Region != regionB.ID || !rows[0].Online {
		t.Fatalf("US-2 明细不符: %s", body)
	}
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/regions/rg-nope/nodes", viewToken, nil); code != http.StatusNotFound {
		t.Fatalf("US-2 未知区域 → %d（want 404）", code)
	}
	t.Logf("US-2 完成（子树明细 1 行在线）")

	// ─── US-3 告警汇聚（空态结构；计数矩阵由单测覆盖）───
	t.Logf("US-3 告警汇聚：空态结构 + unassigned 桶")
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/alarms", viewToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-3 alarms → %d", code)
	}
	var stats struct {
		Regions    []map[string]any `json:"regions"`
		Unassigned struct {
			Total int `json:"total"`
		} `json:"unassigned"`
	}
	json.Unmarshal([]byte(body), &stats)
	if stats.Regions == nil || stats.Unassigned.Total != 0 {
		t.Fatalf("US-3 空态不符: %s", body)
	}
	t.Logf("US-3 完成（空态结构合法）")

	// ─── US-4 区域策略 KV ───
	t.Logf("US-4 区域策略：upsert → 回读 → null 清除 → 校验拒绝 → 未知 404")
	pol := map[string]any{"logLevel": "warn", "alarmRoute": "ops-a"}
	if code, body = v0490TermDo(t, http.MethodPut, base+"/api/v1/hierarchy/regions/"+regionA.ID+"/policy", opToken, pol); code != http.StatusOK {
		t.Fatalf("US-4 PUT → %d body=%s", code, body)
	}
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/regions/"+regionA.ID+"/policy", viewToken, nil)
	if code != http.StatusOK || !strings.Contains(body, "warn") {
		t.Fatalf("US-4 回读不符: %d %s", code, body)
	}
	// summary policySet 联动
	_, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/summary", viewToken, nil)
	json.Unmarshal([]byte(body), &summary)
	if !strings.Contains(body, `"policySet":true`) {
		t.Fatalf("US-4 summary policySet 不符: %s", body)
	}
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/hierarchy/regions/"+regionA.ID+"/policy", opToken, json.RawMessage("null")); code != http.StatusOK {
		t.Fatalf("US-4 null 清除 → %d（want 200；Go nil 不发体 ≠ JSON null 清除语义）", code)
	}
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/regions/"+regionA.ID+"/policy", viewToken, nil)
	if code != http.StatusOK || !strings.Contains(body, "null") {
		t.Fatalf("US-4 清除后回读不符: %d %s", code, body)
	}
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/hierarchy/regions/"+regionB.ID+"/policy", opToken, []any{1, 2}); code != http.StatusBadRequest {
		t.Fatalf("US-4 非对象 → %d（want 400）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/hierarchy/regions/"+regionB.ID+"/policy", opToken,
		map[string]any{"k": strings.Repeat("x", 4200)}); code != http.StatusBadRequest {
		t.Fatalf("US-4 超限 → %d（want 400）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/hierarchy/regions/rg-nope/policy", opToken, pol); code != http.StatusNotFound {
		t.Fatalf("US-4 未知区域 → %d（want 404）", code)
	}
	t.Logf("US-4 完成（upsert/清除/400×2/404）")

	// ─── US-5 越权 ───
	t.Logf("US-5 越权：viewer 策略写 403 / 匿名 401 / viewer 读 200")
	if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/hierarchy/regions/"+regionA.ID+"/policy", viewToken, pol); code != http.StatusForbidden {
		t.Fatalf("US-5 viewer PUT → %d（want 403）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/summary", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("US-5 匿名 → %d（want 401）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/regions/"+regionA.ID+"/policy", viewToken, nil); code != http.StatusOK {
		t.Fatalf("US-5 viewer 读 → %d（want 200）", code)
	}
	t.Logf("US-5 完成（403/401/200）")

	// ─── US-6 策略幂等 ───
	t.Logf("US-6 策略幂等：同体连写两次 → 回读一致")
	for i := 0; i < 2; i++ {
		if code, _ = v0490TermDo(t, http.MethodPut, base+"/api/v1/hierarchy/regions/"+regionB.ID+"/policy", adminToken, pol); code != http.StatusOK {
			t.Fatalf("US-6 第 %d 次 PUT → %d", i+1, code)
		}
	}
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/regions/"+regionB.ID+"/policy", adminToken, nil)
	if code != http.StatusOK || !strings.Contains(body, "alarmRoute") {
		t.Fatalf("US-6 回读不符: %d %s", code, body)
	}
	t.Logf("US-6 完成（幂等 + admin 写面可用）")
}
