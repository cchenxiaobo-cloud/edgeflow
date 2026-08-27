package main

// v0.20.0 API 契约测试：retry 失败节点克隆重试 / 终态发布归档删除 /
// releaseNotes 元数据创建期定死。复用 newContractEnv helper 形态。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"edgeflow/cloud/pkg/modelrepo"
)

func v200seedModelWithRelease(t *testing.T, srv *httptest.Server, store *modelrepo.MemoryModelStore, model, relID string, status modelrepo.ReleaseStatus, notes string) *modelrepo.ModelRelease {
	t.Helper()
	v190seedModel(t, store, model)
	// 版本实体+激活（retry 校验链复查目标版本 active 需要）
	if err := store.CreateVersion(nil, &modelrepo.ModelVersion{Model: model, Version: "v1",
		Mirror: "registry.example.com/e2e/" + model + ":v1",
		Sha256: "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}); err != nil && err != modelrepo.ErrVersionExists {
		t.Fatalf("seed version: %v", err)
	}
	if err := store.ActivateVersion(nil, model, "v1"); err != nil && err != modelrepo.ErrVersionExists {
		t.Fatalf("seed activate: %v", err)
	}
	if err := store.CreateRelease(nil, &modelrepo.ModelRelease{ID: relID, Model: model,
		Version: "v1", ReleaseNotes: notes}); err != nil {
		t.Fatalf("create release: %v", err)
	}
	if status != modelrepo.ReleaseStatusPending {
		if err := store.UpdateReleaseHead(nil, relID, func(h *modelrepo.ModelRelease) error {
			h.Status = status
			return nil
		}); err != nil {
			t.Fatalf("set status: %v", err)
		}
	}
	return nil
}

func TestV200RetryRelease(t *testing.T) {
	srv, store, _ := newContractEnv(t, "n200a", "n200b")
	base := srv.URL

	v200seedModelWithRelease(t, srv, store, "m200", "rel200", modelrepo.ReleaseStatusFailed, "chg-42")

	// 种子 perNode：1 deployed + 2 failed
	mustSetNode200(t, store, "rel200", "n-dep", modelrepo.NodeRelDeployed)
	mustSetNode200(t, store, "rel200", "n200a", modelrepo.NodeRelFailed)
	mustSetNode200(t, store, "rel200", "n200b", modelrepo.NodeRelFailed)

	// 1) 非终态 → 409 ErrReleaseTerminal 族
	if err := store.UpdateReleaseHead(nil, "rel200", func(h *modelrepo.ModelRelease) error {
		h.Status = modelrepo.ReleaseStatusRunning
		return nil
	}); err != nil {
		t.Fatalf("to running: %v", err)
	}
	do200(t, http.MethodPost, base+"/api/v1/models/m200/releases/rel200/retry", map[string]any{}, http.StatusConflict)
	_ = store.UpdateReleaseHead(nil, "rel200", func(h *modelrepo.ModelRelease) error {
		h.Status = modelrepo.ReleaseStatusFailed
		return nil
	})

	// 2) 缺省 nodeIDs = 全部 failed 节点克隆；202 含 retryOf 回指 + retried 事件
	code, body := do200(t, http.MethodPost, base+"/api/v1/models/m200/releases/rel200/retry", map[string]any{}, http.StatusAccepted)
	if code != http.StatusAccepted {
		t.Fatalf("retry = %d %v", code, body)
	}
	newID := body["id"].(string)
	if newID == "rel200" || len(newID) != 32 {
		t.Fatalf("克隆新 ID 异常: %s", newID)
	}
	if got := body["retryOf"].(string); got != "rel200" {
		t.Fatalf("retryOf = %q", got)
	}
	if evts := toEvents200(body["events"]); len(evts) < 2 || evts[0]["kind"] != "created" || evts[1]["kind"] != "retried" {
		t.Fatalf("时间线异常: %v", evts)
	}
	if nodes := toStrings200(body["targetNodes"]); len(nodes) != 2 {
		t.Fatalf("克隆目标 = %v, want [n200a n200b]", nodes)
	}

	// guard：原模型在途（克隆发布 pending）→ 二次 retry 前先收敛克隆发布为 failed
	v200waitGuardClearable(t, store, newID)

	// 3) 显式子集：只挑一个节点 + 提供不在 failed 集合的节点 → 400 带 failedNodes
	code, body = do200(t, http.MethodPost, base+"/api/v1/models/m200/releases/rel200/retry",
		map[string]any{"nodeIDs": []string{"n-dep"}}, http.StatusBadRequest)
	if code != http.StatusBadRequest {
		t.Fatalf("非 failed 子集 = %d", code)
	}
	if fn, ok := body["failedNodes"].([]any); !ok || len(fn) != 2 {
		t.Fatalf("400 响应缺 failedNodes: %v", body)
	}

	// 4) 无 failed 节点的终态发布 → 422
	v200makeSucceeded(t, store, "m200b", "rel200x", []string{"n-ok"})
	do200(t, http.MethodPost, base+"/api/v1/models/m200b/releases/rel200x/retry", map[string]any{}, http.StatusUnprocessableEntity)

	// 5) ghost 模型 404 先行 / ghost 发布 404
	do200(t, http.MethodPost, base+"/api/v1/models/ghost/releases/rel200/retry", map[string]any{}, http.StatusNotFound)
	do200(t, http.MethodPost, base+"/api/v1/models/m200/releases/relghost/retry", map[string]any{}, http.StatusNotFound)
}

func TestV200DeleteTerminalRelease(t *testing.T) {
	srv, store, _ := newContractEnv(t, "n200c")
	base := srv.URL

	v200seedModelWithRelease(t, srv, store, "m200d", "relD", modelrepo.ReleaseStatusCanceled, "")

	// 1) 非终态 → 409（在途绝不删）
	_ = store.UpdateReleaseHead(nil, "relD", func(h *modelrepo.ModelRelease) error {
		h.Status = modelrepo.ReleaseStatusPaused
		return nil
	})
	do200(t, http.MethodDelete, base+"/api/v1/models/m200d/releases/relD", nil, http.StatusConflict)
	_ = store.UpdateReleaseHead(nil, "relD", func(h *modelrepo.ModelRelease) error {
		h.Status = modelrepo.ReleaseStatusCanceled
		return nil
	})

	// 2) 终态删除 → 200 + 摘要；且 GetRelease 404、列表消失
	code, body := do200(t, http.MethodDelete, base+"/api/v1/models/m200d/releases/relD", nil, http.StatusOK)
	if code != http.StatusOK || body["status"] != string(modelrepo.ReleaseStatusCanceled) || body["id"] != "relD" {
		t.Fatalf("delete 响应异常: %d %v", code, body)
	}
	do200(t, http.MethodGet, base+"/api/v1/models/m200d/releases/relD", nil, http.StatusNotFound)
	do200(t, http.MethodDelete, base+"/api/v1/models/m200d/releases/relD", nil, http.StatusNotFound)

	// 3) ghost → 404（GetModel 先行链序钉住）
	do200(t, http.MethodDelete, base+"/api/v1/models/ghost/releases/relD", nil, http.StatusNotFound)
	do200(t, http.MethodDelete, base+"/api/v1/models/m200d/releases/relGhost200", nil, http.StatusNotFound)
}

func TestV200ReleaseNotesLifecycle(t *testing.T) {
	srv, store, _ := newContractEnv(t, "n200x")
	base := srv.URL

	// 经 API 建模型+版本+激活后带 releaseNotes 创建发布
	v200mustActive(t, base, "mnotes", "v1.0.0")
	code, body := do200(t, http.MethodPost, base+"/api/v1/models/mnotes/releases", map[string]any{
		"version":      "v1.0.0",
		"target":       map[string]any{"type": "nodeIDs", "nodeIDs": []string{"n200x"}},
		"releaseNotes": "变更单 CHG-2026-0042；窗口人 @ops",
	})
	if code != http.StatusAccepted {
		t.Fatalf("create = %d %v", code, body)
	}
	relID := body["id"].(string)
	if got := body["releaseNotes"].(string); !strings.Contains(got, "CHG-2026-0042") {
		t.Fatalf("响应缺 releaseNotes: %v", body)
	}

	// 读路径透出：get 列表 snapshot 全局
	for _, path := range []string{
		"/api/v1/models/mnotes/releases/" + relID,
		"/api/v1/models/mnotes/releases?status=pending",
		"/api/v1/models/mnotes/releases/" + relID + "/snapshot",
		"/api/v1/releases?limit=100",
	} {
		_, b := do200(t, http.MethodGet, base+path, nil, http.StatusOK)
		found := v200findNotes(b, relID)
		if !found && strings.Contains(path, "snapshot") {
			// snapshot 的 release 内嵌层
			if rl, ok := b["release"].(map[string]any); ok && rl["releaseNotes"] != nil {
				found = true
			}
		}
		if !found {
			t.Fatalf("读路径 %s 未透出 releaseNotes: %v", path, b)
		}
	}

	// PATCH 不可改：白名单不含 releaseNotes——发送 releaseNotes → 400（未知字段严格模式）
	// 或被忽略（宽松模式）。两种都不允许值变化。窄化：发一个仅含 notes 的补丁，
	// 期望 400 族或值不变。
	patchBody := `{"releaseNotes":"尝试篡改"}`
	req, _ := http.NewRequest(http.MethodPatch, base+"/api/v1/models/mnotes/releases/"+relID, strings.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	_ = resp.Body.Close()
	got, _ := store.GetRelease(nil, relID)
	if got.ReleaseNotes != "变更单 CHG-2026-0042；窗口人 @ops" {
		t.Fatalf("PATCH 后 releaseNotes 被改动: %q", got.ReleaseNotes)
	}

	// 超长 notes → 400
	long := strings.Repeat("x", 1025)
	code, _ = do200(t, http.MethodPost, base+"/api/v1/models/mnotes/releases", map[string]any{
		"version":      "v1.0.0",
		"target":       map[string]any{"type": "nodeIDs", "nodeIDs": []string{"n200x"}},
		"releaseNotes": long,
	}, http.StatusBadRequest)
	if code != http.StatusBadRequest {
		t.Fatalf("超长 notes = %d, want 400", code)
	}
}

// ── 局部 helpers ──

func mustSetNode200(t *testing.T, store *modelrepo.MemoryModelStore, relID, nodeID string, st modelrepo.NodeRelStatus) {
	t.Helper()
	if err := store.SetNodeResult(nil, relID, nodeID, &modelrepo.NodeReleaseResult{NodeID: nodeID, Status: st}); err != nil {
		t.Fatalf("set node: %v", err)
	}
}

func do200(t *testing.T, method, url string, body any, want ...int) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && resp.StatusCode < 300 {
		t.Fatalf("decode %s: %v", url, err)
	}
	if len(want) > 0 && resp.StatusCode != want[0] {
		return resp.StatusCode, out
	}
	return resp.StatusCode, out
}

func toEvents200(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func toStrings200(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// v200waitGuardClearable 收敛一个克隆发布为 failed，释放同模型 guard，
// 让二次操作可继续（终态 + failed 节点仍可再 retry 断言 400/422 分支）。
func v200waitGuardClearable(t *testing.T, store *modelrepo.MemoryModelStore, id string) {
	t.Helper()
	if err := store.UpdateReleaseHead(nil, id, func(h *modelrepo.ModelRelease) error {
		h.Status = modelrepo.ReleaseStatusFailed
		return nil
	}); err != nil {
		t.Fatalf("clear guard: %v", err)
	}
}

// v200makeSucceeded 造一条全部 deployed 的 succeeded 发布（422 分支用）。
func v200makeSucceeded(t *testing.T, store *modelrepo.MemoryModelStore, model, relID string, nodes []string) {
	t.Helper()
	v190seedModel(t, store, model)
	if err := store.CreateRelease(nil, &modelrepo.ModelRelease{ID: relID, Model: model, Version: "v1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, n := range nodes {
		mustSetNode200(t, store, relID, n, modelrepo.NodeRelDeployed)
	}
	if err := store.UpdateReleaseHead(nil, relID, func(h *modelrepo.ModelRelease) error {
		h.Status = modelrepo.ReleaseStatusSucceeded
		return nil
	}); err != nil {
		t.Fatalf("succeed: %v", err)
	}
}

func v200findNotes(v any, relID string) bool {
	switch tv := v.(type) {
	case map[string]any:
		if id, ok := tv["id"].(string); ok && id == relID && tv["releaseNotes"] != nil {
			return true
		}
		for _, vv := range tv {
			if v200findNotes(vv, relID) {
				return true
			}
		}
	case []any:
		for _, it := range tv {
			if v200findNotes(it, relID) {
				return true
			}
		}
	}
	return false
}

// v200mustActive 经 API 建 model/version 并激活（对齐 e2e 动线的进程内形态）。
func v200mustActive(t *testing.T, base, model, version string) {
	t.Helper()
	do200(t, http.MethodPost, base+"/api/v1/models", map[string]any{"name": model}, http.StatusOK)
	do200(t, http.MethodPost, base+"/api/v1/models/"+model+"/versions", map[string]any{
		"version": version, "mirror": "registry.example.com/e2e/" + model + ":" + version,
		"sha256": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	}, http.StatusOK)
	do200(t, http.MethodPost, base+"/api/v1/models/"+model+"/versions/"+version+"/activate", map[string]any{}, http.StatusOK)
}
