package main

// v0.19.0 API 层测试：PATCH failureBudget 白名单扩展 + 发布审计快照 +
// 全局发布查询。复用既有 newContractEnv(t) 真实 mux 环境（同包共享）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"edgeflow/cloud/pkg/modelrepo"
)

// v190seedModel 预置一个模型（直走存储，不经 API 物化依赖）。
func v190seedModel(t *testing.T, st *modelrepo.MemoryModelStore, name string) {
	t.Helper()
	if err := st.CreateModel(nil, &modelrepo.Model{Name: name, Type: "object-detection"}); err != nil && err != modelrepo.ErrModelExists {
		t.Fatalf("seed model: %v", err)
	}
}

// v190do 走真实 httptest server 发 JSON 请求。
func v190do(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	return doJSON(t, method, srv.URL+path, body)
}

func TestV190PatchFailureBudget(t *testing.T) {
	srv, store, _ := newContractEnv(t)
	v190seedModel(t, store, "m190")

	rel := &modelrepo.ModelRelease{ID: "rel190-patch", Model: "m190", Version: "v1",
		BatchSize: 2, FailureBudget: 5}
	if err := store.CreateRelease(nil, rel); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.UpdateReleaseHead(nil, "rel190-patch", func(h *modelrepo.ModelRelease) error {
		h.Status = modelrepo.ReleaseStatusRunning // CreateRelease 归一化 pending 后直改
		return nil
	}); err != nil {
		t.Fatalf("to running: %v", err)
	}
	patchPath := "/api/v1/models/m190/releases/rel190-patch"

	// 1) 单字段：failureBudget=2 → 200 且持久化
	code, body := v190do(t, srv, http.MethodPatch, patchPath, map[string]any{"failureBudget": 2})
	if code != http.StatusOK {
		t.Fatalf("patch = %d %v", code, body)
	}
	got, _ := store.GetRelease(nil, "rel190-patch")
	if got.FailureBudget != 2 {
		t.Fatalf("failureBudget 持久化 = %d, want 2", got.FailureBudget)
	}

	// 2) 与执行参数混合补丁；budget=0 = 运行中关闸
	code, body = v190do(t, srv, http.MethodPatch, patchPath, map[string]any{"batchSize": 3, "failureBudget": 0})
	if code != http.StatusOK {
		t.Fatalf("mixed patch = %d %v", code, body)
	}
	got, _ = store.GetRelease(nil, "rel190-patch")
	if got.BatchSize != 3 || got.FailureBudget != 0 {
		t.Fatalf("mixed = batchSize=%d budget=%d, want 3/0（0=关闸合法值）", got.BatchSize, got.FailureBudget)
	}

	// 3) 响应体含 summaryOf（与 updateRelease 契约一致）
	//    （上面 mixed 已含 summary 字段形状——releaseResponse{ModelRelease, Summary}）
	code, body = v190do(t, srv, http.MethodPatch, patchPath, map[string]any{"failureBudget": 1})
	if code != http.StatusOK {
		t.Fatalf("re-patch = %d", code)
	}
	if _, ok := body["summary"]; !ok {
		t.Fatalf("响应缺 summary: %v", body)
	}

	// 4) 非法值族：负数 / 超上限 / 空补丁 → 400
	for i, tc := range []map[string]any{
		{"failureBudget": -1},
		{"failureBudget": maxFailureBudget + 1},
		{},
	} {
		code, body = v190do(t, srv, http.MethodPatch, patchPath, tc)
		if code != http.StatusBadRequest {
			t.Fatalf("case%d(%v) = %d %v, want 400", i, tc, code, body)
		}
	}

	// 5) 终态 → 409
	if err := store.UpdateReleaseHead(nil, "rel190-patch", func(h *modelrepo.ModelRelease) error {
		h.Status = modelrepo.ReleaseStatusSucceeded
		return nil
	}); err != nil {
		t.Fatalf("to terminal: %v", err)
	}
	code, body = v190do(t, srv, http.MethodPatch, patchPath, map[string]any{"failureBudget": 1})
	if code != http.StatusConflict {
		t.Fatalf("terminal patch = %d %v, want 409", code, body)
	}

	// 6) 不存在的 release → 404
	code, _ = v190do(t, srv, http.MethodPatch, "/api/v1/models/m190/releases/ghost190",
		map[string]any{"failureBudget": 1})
	if code != http.StatusNotFound {
		t.Fatalf("404 patch = %d, want 404", code)
	}
}

func TestV190ReleaseSnapshot(t *testing.T) {
	srv, store, _ := newContractEnv(t)
	v190seedModel(t, store, "m190s")

	rel := &modelrepo.ModelRelease{ID: "rel190-snap", Model: "m190s", Version: "v1"}
	if err := store.CreateRelease(nil, rel); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.UpdateReleaseHead(nil, "rel190-snap", func(h *modelrepo.ModelRelease) error {
		h.Status = modelrepo.ReleaseStatusRunning
		return nil
	}); err != nil {
		t.Fatalf("to running: %v", err)
	}
	mustSetNode190(t, store, "rel190-snap", "edge-a", modelrepo.NodeRelDeployed)
	mustSetNode190(t, store, "rel190-snap", "edge-b", modelrepo.NodeRelFailed)

	// 1) 正常快照：kind/generatedAt/release/summary/nodes 全景
	code, body := v190do(t, srv, http.MethodGet, "/api/v1/models/m190s/releases/rel190-snap/snapshot", nil)
	if code != http.StatusOK {
		t.Fatalf("snapshot = %d %v", code, body)
	}
	if body["kind"] != "ReleaseSnapshot" {
		t.Fatalf("kind = %v", body["kind"])
	}
	if _, ok := body["generatedAt"].(float64); !ok {
		t.Fatalf("generatedAt 缺失: %v", body)
	}
	sum, _ := body["summary"].(map[string]any)
	if sum == nil || sum["total"].(float64) != 2 || sum["deployed"].(float64) != 1 || sum["failed"].(float64) != 1 {
		t.Fatalf("summary 计数异常: %v", sum)
	}
	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	relBody, _ := body["release"].(map[string]any)
	if relBody == nil || relBody["id"] != "rel190-snap" {
		t.Fatalf("release 头异常: %v", relBody)
	}

	// 2) 跨模型引用 → 404（head.Model 钉住防目录穿越式枚举）
	code, _ = v190do(t, srv, http.MethodGet, "/api/v1/models/other-model/releases/rel190-snap/snapshot", nil)
	if code != http.StatusNotFound {
		t.Fatalf("cross-model snapshot = %d, want 404", code)
	}

	// 3) 模型不存在 → 404 先行（C-4 同链序纪律）
	code, _ = v190do(t, srv, http.MethodGet, "/api/v1/models/ghost-m190/releases/rel190-snap/snapshot", nil)
	if code != http.StatusNotFound {
		t.Fatalf("ghost model snapshot = %d, want 404", code)
	}

	// 4) release 不存在 → 404
	code, _ = v190do(t, srv, http.MethodGet, "/api/v1/models/m190s/releases/ghost-snap/snapshot", nil)
	if code != http.StatusNotFound {
		t.Fatalf("ghost release snapshot = %d, want 404", code)
	}
}

func TestV190ListAllReleases(t *testing.T) {
	srv, store, _ := newContractEnv(t)
	// 单模型单发布（四模型各一条）——从源头避开同模型 guard 互斥；
	// tie-break 用跨模型同刻对（r-c@200 与 r-d@200，ID 降序 r-d 在前）。
	seeds := []struct {
		id        string
		model     string
		status    modelrepo.ReleaseStatus
		createdAt int64
	}{
		{"r-a", "mA", modelrepo.ReleaseStatusSucceeded, 100},
		{"r-b", "mB", modelrepo.ReleaseStatusRunning, 300},
		{"r-c", "mC", modelrepo.ReleaseStatusFailed, 200},
		{"r-d", "mD", modelrepo.ReleaseStatusCanceled, 200},
	}
	for _, s := range seeds {
		v190seedModel(t, store, s.model)
		// 存储层 CreateRelease 强制归一化 Status=pending（契约行为）——
		// 种子期用 UpdateReleaseHead 直改终态/route 状态，绕过 guard 判定
		// （判定只看 InFlight；pending 不占 guard）。
		if err := store.CreateRelease(nil, &modelrepo.ModelRelease{ID: s.id, Model: s.model,
			Version: "v1", CreatedAt: s.createdAt}); err != nil {
			t.Fatalf("create %s: %v", s.id, err)
		}
		if s.status != modelrepo.ReleaseStatusPending {
			if err := store.UpdateReleaseHead(nil, s.id, func(h *modelrepo.ModelRelease) error {
				h.Status = s.status
				return nil
			}); err != nil {
				t.Fatalf("status %s → %s: %v", s.id, s.status, err)
			}
		}
	}

	// 1) 全量：CreatedAt 降序 + 同刻 tie-break by ID → r-d(200) 在 r-c(200) 前
	getList := func(query string) (int, map[string]any, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/v1/releases" + query)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		out := map[string]any{}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && resp.StatusCode != http.StatusNoContent {
			t.Fatalf("decode: %v", err)
		}
		return resp.StatusCode, out, resp.Header.Get("X-Total-Count")
	}
	code, body, total := getList("")
	if code != http.StatusOK || total != "4" {
		t.Fatalf("list = %d total=%q", code, total)
	}
	items := body["items"].([]any)
	order := ""
	for _, it := range items {
		order += it.(map[string]any)["id"].(string) + ","
	}
	if want := "r-b,r-d,r-c,r-a,"; order != want {
		t.Fatalf("order = %q, want %q", order, want)
	}

	// 2) status 多值过滤 → X-Total-Count 过滤后口径
	code, body, total = getList("?status=running,failed")
	if code != http.StatusOK || total != "2" {
		t.Fatalf("filter = %d total=%q %v", code, total, body)
	}
	if items := body["items"].([]any); len(items) != 2 {
		t.Fatalf("filter items = %d, want 2", len(items))
	}

	// 3) 非法 status → 400
	if code, _, _ = getList("?status=bogus"); code != http.StatusBadRequest {
		t.Fatalf("bogus status = %d, want 400", code)
	}

	// 4) 分页 offset=1&limit=2 → r-d,r-c
	code, body, _ = getList("?limit=2&offset=1")
	if code != http.StatusOK {
		t.Fatalf("page = %d", code)
	}
	items = body["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != "r-d" || items[1].(map[string]any)["id"] != "r-c" {
		t.Fatalf("page items 异常: %v", body)
	}

	// 5) 参数边界：limit 0/501/非数字、offset<0 → 400
	for _, q := range []string{"limit=0", "limit=501", "limit=abc", "offset=-1"} {
		if code, _, _ = getList("?" + q); code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", q, code)
		}
	}

	// 6) 空库形态：items=[] 非 null，X-Total-Count=0
	srv2, _, _ := newContractEnv(t)
	resp, err := http.Get(srv2.URL + "/api/v1/releases")
	if err != nil {
		t.Fatalf("empty GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	if resp.Header.Get("X-Total-Count") != "0" {
		t.Fatalf("空库 total=%q, want 0", resp.Header.Get("X-Total-Count"))
	}
	if arr, ok := raw["items"].([]any); !ok || arr == nil || len(arr) != 0 {
		t.Fatalf("空库 items 应为 []: %v", raw)
	}
}

// mustSetNode190 写入逐节点结果种子。
func mustSetNode190(t *testing.T, st *modelrepo.MemoryModelStore, relID, nodeID string, stt modelrepo.NodeRelStatus) {
	t.Helper()
	if err := st.SetNodeResult(nil, relID, nodeID, &modelrepo.NodeReleaseResult{Status: stt}); err != nil {
		t.Fatalf("SetNodeResult %s/%s: %v", relID, nodeID, err)
	}
}
