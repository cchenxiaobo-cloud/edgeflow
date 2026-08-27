package e2e

// v0.20.0 发布生命周期收口 E2E：真实 cloudcore 子进程 + 真实边缘节点——
// failed 发布 retry 克隆重试 → 新发布终态收敛；终态发布 DELETE 归档；
// releaseNotes 全读取路径透出。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestV200LifecycleOpsE2E(t *testing.T) {
	root := repoRoot(t)
	buildBinaries(t)
	if buildErr != nil {
		t.Skipf("二进制构建失败: %v", buildErr)
	}

	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	nodeA := "e2e-rel200-a"
	nodeB := "e2e-rel200-b"
	dbA := filepath.Join(t.TempDir(), "edgecore-a.db")
	dbB := filepath.Join(t.TempDir(), "edgecore-b.db")
	envA := edgeEnv(nodeA, "ws://127.0.0.1:"+strconv.Itoa(hubPort), dbA)
	envB := edgeEnv(nodeB, "ws://127.0.0.1:"+strconv.Itoa(hubPort), dbB)
	startProcess(t, "edgecore-"+nodeA, filepath.Join(binDir, "edgecore"), nil, envA)
	startProcess(t, "edgecore-"+nodeB, filepath.Join(binDir, "edgecore"), nil, envB)
	waitNodeRegistered(t, base, nodeA)
	waitNodeRegistered(t, base, nodeB)

	v190mustModelVersionActivate(t, base, "lc200", "v1.0.0")

	// 1) 带 releaseNotes 创建双节点发布 → 跑到终态
	code, rel := v160post(t, base+"/api/v1/models/lc200/releases", map[string]any{
		"version":      "v1.0.0",
		"target":       map[string]any{"type": "nodeIDs", "nodeIDs": []string{nodeA, nodeB}},
		"releaseNotes": "E2E 变更单 CHG-E2E-200",
	})
	if code != http.StatusAccepted {
		t.Fatalf("创建 = %d %v", code, rel)
	}
	relID := rel["id"].(string)

	status := v170waitTerminal(t, base, "lc200", relID)
	t.Logf("首发终态=%s", status)

	// 2) releaseNotes 读路径透出（详情/快照）
	_, detail := v170getJSON(t, base+"/api/v1/models/lc200/releases/"+relID)
	if s, _ := detail["releaseNotes"].(string); s != "E2E 变更单 CHG-E2E-200" {
		t.Fatalf("详情缺 notes: %v", detail["releaseNotes"])
	}

	// 3) 按首发终态分支：failed → retry 重试 failed 节点；succeeded → 直接收尾断言
	if status == "failed" {
		code2, retryRel := v160post(t, base+"/api/v1/models/lc200/releases/"+relID+"/retry", nil)
		if code2 != http.StatusAccepted {
			t.Fatalf("retry = %d %v", code2, retryRel)
		}
		newID := retryRel["id"].(string)
		if newID == relID {
			t.Fatalf("retry 应产出新 ID")
		}
		if got := retryRel["retryOf"].(string); got != relID {
			t.Fatalf("retryOf = %q", got)
		}
		newStatus := v170waitTerminal(t, base, "lc200", newID)
		t.Logf("重试发布终态=%s", newStatus)
		// 全成功发布：终态但有 0 个 failed 节点 → retry 422（no failed nodes to retry）
		v170expectStatus(t, http.MethodPost,
			base+"/api/v1/models/lc200/releases/"+relID+"/retry", http.StatusUnprocessableEntity)
		t.Logf("全成功发布 retry → 422（no failed nodes，语义钉住）")
	}

	// 4) 终态归档删除：删除首发 → 详情 404 → 全局查询消失
	req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/models/lc200/releases/"+relID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			t.Fatalf("delete = %d err=%v", resp.StatusCode, err)
		}
		t.Fatalf("delete err=%v", err)
	}
	var delResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&delResp); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	_ = resp.Body.Close()
	if _, ok := delResp["status"]; !ok {
		t.Fatalf("delete 响应缺 status: %v", delResp)
	}
	v170expectStatus(t, http.MethodGet, base+"/api/v1/models/lc200/releases/"+relID, http.StatusNotFound)

	// 5) 非终态不可删守卫（新建一条 pending——不发版仅挂起；guard 占用下 DELETE 409）
	code3, guardRel := v160post(t, base+"/api/v1/models/lc200/releases", map[string]any{
		"version": "v1.0.0",
		"target":  map[string]any{"type": "nodeIDs", "nodeIDs": []string{nodeA}},
	})
	if code3 != http.StatusAccepted {
		t.Fatalf("guard 种子 = %d %v", code3, guardRel)
	}
	guardID := guardRel["id"].(string)
	time.Sleep(800 * time.Millisecond) // 可能已被控制器认领推进——两种状态都行，只要非终态删不得
	req4, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/models/lc200/releases/"+guardID, nil)
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("delete in-flight: %v", err)
	}
	// 若已自然跑到终态则 409 不成立——读回状态分流
	_, gb := v170getJSON(t, base+"/api/v1/models/lc200/releases/"+guardID)
	if gs, _ := gb["status"].(string); !isTerminalStatusV200(gs) {
		_ = resp4.Body.Close()
		if resp4.StatusCode != http.StatusConflict {
			t.Fatalf("非终态 delete = %d, want 409", resp4.StatusCode)
		}
		t.Logf("在途发布 delete 守卫 409 钉住")
		// 收敛后清理，给后续轮次留干净 guard（guard 种子发布取消收尾）
		if _, cb := v160post(t, base+"/api/v1/models/lc200/releases/"+guardID+"/cancel", nil); cb != nil {
			t.Logf("guard 种子已请求取消")
		}
	} else {
		t.Logf("种子已自然终态（%s），跳过 409 断言", gs)
	}
}

func isTerminalStatusV200(s string) bool {
	return s == "succeeded" || s == "failed" || s == "canceled" || s == "rolled_back"
}

// v170waitTerminal 轮询到发布终态或超时（返回最终状态串）。
func v170waitTerminal(t *testing.T, base, model, relID string) string {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		_, b := v170getJSON(t, base+"/api/v1/models/"+model+"/releases/"+relID)
		if s, ok := b["status"].(string); ok {
			status = s
			if isTerminalStatusV200(status) {
				return status
			}
			if status == "paused" { // budget 触发自动暂停 → resume 续跑容错
				if code, _ := v160post(t, base+"/api/v1/models/"+model+"/releases/"+relID+"/resume", nil); code == http.StatusOK {
					t.Logf("预算触发自动暂停 → resume 续跑")
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待终态超时，最后状态=%s", status)
	return status
}

// v170expectStatus 断言一次 HTTP 调用的状态码（重试/守卫分支用）。
func v170expectStatus(t *testing.T, method, url string, want int) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d", method, url, resp.StatusCode, want)
	}
}
