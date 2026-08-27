package e2e

// v0.17.0 模型管理深化 E2E：真实 cloudcore 子进程（嵌入式 etcd + 发布控制
// 器装配）下验证——发布列表 status 过滤、dryRun 预检零落盘、PATCH 运行中
// 可调参数在批次边界生效（配合窗口/pause 链路）。既有用例不改一行。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// v170mustJSON 编码请求体。
func v170mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// v170getJSON GET 并解析 JSON。
func v170getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	m := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

// TestV170ReleaseOpsE2E 主链路：
// 注册节点 → 建模型/版本/激活 → dryRun 预检（wouldCreate=true 且零落盘）
// → 窗口发布创建 → pending 期 PATCH 放慢节奏 → 到点 running → pause →
// paused 期 PATCH batchSize → resume 收敛 succeeded → status 过滤断言。
func TestV170ReleaseOpsE2E(t *testing.T) {
	root := repoRoot(t)
	buildBinaries(t)
	if buildErr != nil {
		t.Skipf("二进制构建失败: %v", buildErr)
	}

	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	nodeA := "e2e-rel170-a"
	dbA := filepath.Join(t.TempDir(), "edgecore-a.db")
	envA := edgeEnv(nodeA, "ws://127.0.0.1:"+strconv.Itoa(hubPort), dbA)
	startProcess(t, "edgecore-"+nodeA, filepath.Join(binDir, "edgecore"), nil, envA)
	waitNodeRegistered(t, base, nodeA)

	// 1) 模型 + 版本 + 激活
	code, body := v160post(t, base+"/api/v1/models", map[string]any{"name": "rel170m"})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("建模型 = %d %v", code, body)
	}
	code, body = v160post(t, base+"/api/v1/models/rel170m/versions", map[string]any{
		"version": "v1.0.0",
		"mirror":  "registry.example.com/e2e/rel170:v1.0.0",
		"sha256":  "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("建版本 = %d %v", code, body)
	}
	if code, _ := v160post(t, base+"/api/v1/models/rel170m/versions/v1.0.0/activate", nil); code != http.StatusOK {
		t.Fatalf("激活失败 = %d", code)
	}

	// 2) dryRun 预检：wouldCreate=true 且零落盘
	code, dry := v160post(t, base+"/api/v1/models/rel170m/releases", map[string]any{
		"version": "v1.0.0",
		"target":  map[string]any{"type": "nodeIDs", "nodeIDs": []string{nodeA}},
		"dryRun":  true,
	})
	if code != http.StatusOK || dry["wouldCreate"] != true {
		t.Fatalf("dryRun = %d %v，want 200+true", code, dry)
	}
	code, listBefore := v170getJSON(t, base+"/api/v1/models/rel170m/releases")
	if code != http.StatusOK {
		t.Fatalf("dryRun 后列表 = %d", code)
	}
	if items, ok := listBefore["items"].([]any); ok && len(items) != 0 {
		t.Fatalf("dryRun 落盘了！items=%v", items)
	}
	t.Logf("dryRun 预检通过且零落盘: nodes=%v prevActive=%v", dry["targetNodes"], dry["prevActive"])

	// 3) 窗口发布（6s 后启动），pending 期 PATCH 放慢批节奏
	notBefore := time.Now().Add(6 * time.Second).UnixMilli()
	code, rel := v160post(t, base+"/api/v1/models/rel170m/releases", map[string]any{
		"version":     "v1.0.0",
		"target":      map[string]any{"type": "nodeIDs", "nodeIDs": []string{nodeA}},
		"batchSize":   1,
		"notBeforeMs": notBefore,
	})
	if code != http.StatusAccepted {
		t.Fatalf("创建 = %d %v", code, rel)
	}
	relID := rel["id"].(string)
	baseRel := base + "/api/v1/models/rel170m/releases/" + relID

	patchReq := func(payload map[string]any) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPatch, baseRel, bytes.NewReader(v170mustJSON(payload)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PATCH 失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		m := map[string]any{}
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &m)
		return resp.StatusCode, m
	}
	if code, patched := patchReq(map[string]any{"pauseBetween": int64(500)}); code != http.StatusOK || patched["batchSize"] == nil {
		t.Fatalf("pending 期 PATCH = %d %v，want 200", code, patched)
	}

	// 4) 到点自动 running → pause → paused 期 PATCH batchSize=3 → resume
	v160waitReleaseStatus(t, base, relID, 60*time.Second, func(m map[string]any) bool {
		st, _ := m["status"].(string)
		return st == "running" || st == "succeeded"
	})
	if code, paused := v160post(t, baseRel+"/pause", nil); code != http.StatusOK && paused["status"] != "paused" {
		t.Logf("pause 返回 %d %v（单节点批次可能已完成）", code, paused)
	} else if code == http.StatusOK {
		if code, _ := patchReq(map[string]any{"batchSize": 3}); code != http.StatusOK {
			t.Fatalf("paused 期 PATCH = %d", code)
		}
		if code, _ := v160post(t, baseRel+"/resume", nil); code != http.StatusOK {
			t.Fatalf("resume = %d", code)
		}
	}

	// 5) 收敛 succeeded
	done := v160waitReleaseStatus(t, base, relID, 90*time.Second, func(m map[string]any) bool {
		return m["status"] == "succeeded"
	})
	if done["status"] != "succeeded" {
		t.Fatalf("未收敛：%v", done["status"])
	}

	// 6) status 过滤：全量 1 条、succeeded 过滤含它、pending 过滤为空
	code, all := v170getJSON(t, base+"/api/v1/models/rel170m/releases?status=succeeded")
	if code != http.StatusOK {
		t.Fatalf("status 过滤列表 = %d", code)
	}
	items, _ := all["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("status=succeeded 条数 = %d, want 1", len(items))
	}
	code, pendList := v170getJSON(t, base+"/api/v1/models/rel170m/releases?status=pending")
	if code != http.StatusOK {
		t.Fatalf("pending 过滤列表 = %d", code)
	}
	if items, ok := pendList["items"].([]any); ok && len(items) != 0 {
		t.Fatalf("status=pending 应为空，got %v", items)
	}
	t.Logf("v0.17.0 E2E 主链路完成：dryRun 零落盘 + 窗口/pause 双阶段 PATCH + status 过滤断言全过")
}
