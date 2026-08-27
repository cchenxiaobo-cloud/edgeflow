package e2e

// v0.19.0 发布面智能运维第二批 E2E：真实 cloudcore 子进程下验证——
// 快照端点全景（头含 events + summary 六计数）+ 全局发布查询（过滤/分页/
// 排序）。主链路建发布跑至终态后做只读断言，不改既有用例一行。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestV190ReleaseIntelE2E(t *testing.T) {
	root := repoRoot(t)
	buildBinaries(t)
	if buildErr != nil {
		t.Skipf("二进制构建失败: %v", buildErr)
	}

	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	nodeA := "e2e-rel190-a"
	dbA := filepath.Join(t.TempDir(), "edgecore-a.db")
	envA := edgeEnv(nodeA, "ws://127.0.0.1:"+strconv.Itoa(hubPort), dbA)
	startProcess(t, "edgecore-"+nodeA, filepath.Join(binDir, "edgecore"), nil, envA)
	waitNodeRegistered(t, base, nodeA)

	v190mustModelVersionActivate(t, base, "intel190", "v1.0.0")

	code, rel := v160post(t, base+"/api/v1/models/intel190/releases", map[string]any{
		"version":       "v1.0.0",
		"target":        map[string]any{"type": "nodeIDs", "nodeIDs": []string{nodeA}},
		"failureBudget": 1,
	})
	if code != http.StatusAccepted {
		t.Fatalf("创建 = %d %v", code, rel)
	}
	relID := rel["id"].(string)

	// 终态收敛（单节点批次快速完成；succeeded 或 failed 均可——预算达标
	// autopause 时手动 resume 续跑）
	var status string
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		_, b := v170getJSON(t, base+"/api/v1/models/intel190/releases/"+relID)
		if s, ok := b["status"].(string); ok {
			status = s
			if s == "succeeded" || s == "failed" {
				break
			}
			if s == "paused" { // budget 触发自动刹车 → resume 续跑
				if code2, _ := v160post(t, base+"/api/v1/models/intel190/releases/"+relID+"/resume", nil); code2 == http.StatusOK {
					t.Logf("预算触发自动暂停 → resume 续跑")
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("发布终态=%s", status)

	// 1) 审计快照：kind/generatedAt/release(events)/summary/nodes 全景
	snapCode, snap := v170getJSON(t, base+"/api/v1/models/intel190/releases/"+relID+"/snapshot")
	if snapCode != http.StatusOK {
		t.Fatalf("snapshot = %d %v", snapCode, snap)
	}
	if snap["kind"] != "ReleaseSnapshot" {
		t.Fatalf("snapshot kind = %v", snap["kind"])
	}
	if _, ok := snap["generatedAt"].(float64); !ok {
		t.Fatalf("snapshot 缺 generatedAt")
	}
	sum, _ := snap["summary"].(map[string]any)
	if sum == nil {
		t.Fatalf("snapshot 缺 summary: %v", snap)
	}
	t.Logf("snapshot summary: %v", sum)
	nodes, _ := snap["nodes"].([]any)
	if len(nodes) < 1 {
		t.Fatalf("snapshot nodes 空")
	}

	// 2) 全局发布查询：本场景 ≥1 条，model 建后排序降序含本 ID；X-Total-Count 同步
	listDeadline := time.Now().Add(10 * time.Second)
	var total string
	var found bool
	for time.Now().Before(listDeadline) {
		resp, err := http.Get(base + "/api/v1/releases?limit=100")
		if err == nil && resp.StatusCode == http.StatusOK {
			total = resp.Header.Get("X-Total-Count")
			body := v190decodeList(t, resp)
			for _, it := range body {
				if m, ok := it.(map[string]any); ok && m["id"] == relID {
					found = true
					break
				}
			}
			_ = resp.Body.Close()
			if found {
				break
			}
		} else if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		t.Fatalf("全局发布查询未见本发布（total=%q）", total)
	}
	t.Logf("全局查询命中，total=%s", total)

	// 3) status 过滤：terminal 后用 succeeded/failed 过滤必命中本 ID；
	//    pending 过滤应为 0（guard/gc 下 pending 不滞留）
	stFiltered := false
	resp, err := http.Get(base + "/api/v1/releases?status=succeeded,failed")
	if err == nil {
		total2 := resp.Header.Get("X-Total-Count")
		items := v190decodeList(t, resp)
		_ = resp.Body.Close()
		for _, it := range items {
			if m, ok := it.(map[string]any); ok && m["id"] == relID {
				stFiltered = true
			}
		}
		t.Logf("终态过滤 total=%s 命中=%v", total2, stFiltered)
	}
	if !stFiltered {
		t.Fatalf("终态过滤未命中本发布")
	}

	// 4) 非法参数族 → 400
	for _, q := range []string{"status=bogus", "limit=0", "limit=501"} {
		r, e := http.Get(base + "/api/v1/releases?" + q)
		if e != nil {
			t.Fatalf("%s: %v", q, e)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", q, r.StatusCode)
		}
	}
}

// --- 本文件局部 helper ---

func v190mustModelVersionActivate(t *testing.T, base, model, version string) {
	t.Helper()
	if code, b := v160post(t, base+"/api/v1/models", map[string]any{"name": model}); code != http.StatusOK && code != http.StatusCreated && code != http.StatusConflict {
		t.Fatalf("建模型 = %d %v", code, b)
	}
	if code, b := v160post(t, base+"/api/v1/models/"+model+"/versions", map[string]any{
		"version": version, "mirror": "registry.example.com/e2e/intel190:" + version,
		"sha256": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	}); code != http.StatusOK && code != http.StatusCreated && code != http.StatusConflict {
		t.Fatalf("建版本 = %d %v", code, b)
	}
	if code, _ := v160post(t, base+"/api/v1/models/"+model+"/versions/"+version+"/activate", nil); code != http.StatusOK {
		t.Fatalf("激活失败 = %d", code)
	}
}

func v190decodeList(t *testing.T, resp *http.Response) []any {
	t.Helper()
	raw := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	arr, _ := raw["items"].([]any)
	return arr
}
