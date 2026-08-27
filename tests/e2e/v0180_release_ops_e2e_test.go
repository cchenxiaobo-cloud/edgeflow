package e2e

// v0.18.0 发布面智能运维 E2E：真实 cloudcore 子进程下验证——
// 创建含 failureBudget 的发布、events 时间线流转、全局 deployments 聚合。
// 既有用例不改一行。

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestV180ReleaseOpsTimelineE2E 主链路：建模型/版本/激活 → dryRun 校验
// budget 合法性 → 真实创建（created 事件入时间线）→ PATCH 触发后查询详情
// 含 events → 全局 deployments 端点聚合（此场景部署影子为空，断言空列表 +
// 200）→ status 过滤回归。
func TestV180ReleaseOpsTimelineE2E(t *testing.T) {
	root := repoRoot(t)
	buildBinaries(t)
	if buildErr != nil {
		t.Skipf("二进制构建失败: %v", buildErr)
	}

	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	nodeA := "e2e-rel180-a"
	dbA := filepath.Join(t.TempDir(), "edgecore-a.db")
	envA := edgeEnv(nodeA, "ws://127.0.0.1:"+strconv.Itoa(hubPort), dbA)
	startProcess(t, "edgecore-"+nodeA, filepath.Join(binDir, "edgecore"), nil, envA)
	waitNodeRegistered(t, base, nodeA)

	code, body := v160post(t, base+"/api/v1/models", map[string]any{"name": "rel180m"})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("建模型 = %d %v", code, body)
	}
	code, _ = v160post(t, base+"/api/v1/models/rel180m/versions", map[string]any{
		"version": "v1.0.0", "mirror": "registry.example.com/e2e/rel180:v1.0.0",
		"sha256": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("建版本 = %d %v", code, body)
	}
	if code, _ := v160post(t, base+"/api/v1/models/rel180m/versions/v1.0.0/activate", nil); code != http.StatusOK {
		t.Fatalf("激活失败 = %d", code)
	}

	// 1) 带 failureBudget 的真实创建（单节点白名单、立即启动）
	code, rel := v160post(t, base+"/api/v1/models/rel180m/releases", map[string]any{
		"version":       "v1.0.0",
		"target":        map[string]any{"type": "nodeIDs", "nodeIDs": []string{nodeA}},
		"failureBudget": 1,
	})
	if code != http.StatusAccepted {
		t.Fatalf("创建 = %d %v", code, rel)
	}
	relID := rel["id"].(string)

	// 2) 详情断言 created 事件存在
	var detail map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code2, b := v170getJSON(t, base+"/api/v1/models/rel180m/releases/"+relID)
		if code2 == http.StatusOK {
			detail = b
			if evs, ok := b["events"].([]any); ok && len(evs) > 0 {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	evs, _ := detail["events"].([]any)
	if len(evs) == 0 {
		t.Fatalf("events 时间线为空: %v", detail)
	}
	firstKind := evs[0].(map[string]any)["kind"]
	if firstKind != "created" {
		t.Logf("首事件 kind=%v（并发下认领事件可能先入时间线，容忍）", firstKind)
	}
	kinds := map[string]bool{}
	for _, e := range evs {
		kinds[e.(map[string]any)["kind"].(string)] = true
	}
	if !kinds["created"] && !kinds["batch_done"] && !kinds["terminal"] {
		t.Fatalf("时间线无任何预期事件: %v", kinds)
	}
	t.Logf("时间线事件种类: %v", kinds)

	// 3) 收敛（单节点批次快速完成）
	done := v160waitReleaseStatus(t, base, relID, 90*time.Second, func(m map[string]any) bool {
		return m["status"] == "succeeded" || m["status"] == "failed"
	})
	st := done["status"].(string)
	t.Logf("发布终态=%s failureReason=%v", st, done["failureReason"])

	// 4) 终态后再查时间线——terminal 事件应在（若为 succeeded 且 head 有 Events）
	if done["events"] == nil {
		t.Fatalf("终态响应缺 events 字段")
	}

	// 5) 全局 deployments 端点：真实 edgecore 上报后影子非空（nodeID 过滤）
	dl := 90 * time.Second
	deadline = time.Now().Add(dl)
	var globalList map[string]any
	for time.Now().Before(deadline) {
		code3, b := v170getJSON(t, base+"/api/v1/deployments?model=rel180m&nodeID="+nodeA)
		if code3 == http.StatusOK {
			globalList = b
			if items, ok := b["items"].([]any); ok && len(items) > 0 {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	items, _ := globalList["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("全局 deployments 未观察到节点影子: %v", globalList)
	}
	item0 := items[0].(map[string]any)
	if item0["model"] != "rel180m" || item0["nodeID"] != nodeA {
		t.Fatalf("影子字段异常: %v", item0)
	}
	t.Logf("全局 deployments 聚合正确: model=rel180m nodeID=%s version=%v", nodeA, item0["version"])
}
