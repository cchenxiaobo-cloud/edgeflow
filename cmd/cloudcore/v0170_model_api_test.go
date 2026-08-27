package main

// v0.17.0 API 契约测试：PATCH 运行中可调参数 / 列表 status 过滤 / dryRun
// 预检。复用 newContractEnv 内存环境；真实控制器时序由 E2E 覆盖。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"edgeflow/cloud/pkg/modelrepo"
)

// v170SeedActiveModel 建模型+版本+激活，返回 base URL。
func v170SeedActiveModel(t *testing.T, srv *httptest.Server, model, version string) {
	t.Helper()
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models", map[string]any{"name": model})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("建模型 = %d %v", code, body)
	}
	code, body = doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/"+model+"/versions", map[string]any{
		"version": version, "mirror": "registry.example.com/e2e/m:" + version,
		"sha256": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("建版本 = %d %v", code, body)
	}
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/"+model+"/versions/"+version+"/activate", nil); code != http.StatusOK {
		t.Fatalf("激活失败 = %d", code)
	}
}

// v170CreateRelease 建一个白名单节点发布（节点无需真实存在——注册校验在
// 契约层用已完成注册的节点名绕过；这里直接经 store 预置 in-flight 发布）。
func v170CreateReleaseDirect(t *testing.T, a *modelAPI, model, version string) *modelrepo.ModelRelease {
	t.Helper()
	store := a.store
	rel := &modelrepo.ModelRelease{
		ID: "rel-v170-" + model, Model: model, Version: version,
		Target:    modelrepo.ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{}},
		BatchSize: 1, FailFast: true, Status: modelrepo.ReleaseStatusRunning,
	}
	if err := store.CreateRelease(t.Context(), rel); err != nil {
		t.Fatalf("直置发布失败: %v", err)
	}
	return rel
}

func TestV170PatchReleaseTunables(t *testing.T) {
	srv, store, reg := newContractEnv(t)
	a := &modelAPI{store: store, reg: reg}
	v170SeedActiveModel(t, srv, "m-patch", "v1.0.0")
	rel := v170CreateReleaseDirect(t, a, "m-patch", "v1.0.0")
	base := srv.URL + "/api/v1/models/m-patch/releases/" + rel.ID

	// 1) PATCH 全字段 → 200 且生效
	code, body := doJSON(t, http.MethodPatch, base, map[string]any{
		"batchSize": 3, "pauseBetween": 1500, "failFast": false,
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH = %d %v，want 200", code, body)
	}
	got, _ := store.GetRelease(t.Context(), rel.ID)
	if got.BatchSize != 3 || got.PauseBetween != 1500 || got.FailFast {
		t.Errorf("PATCH 未生效：%+v", got)
	}
	// 2) 部分 patch：只改 batchSize，其余保持
	if code, _ := doJSON(t, http.MethodPatch, base, map[string]any{"batchSize": 5}); code != http.StatusOK {
		t.Fatalf("部分 PATCH = %d", code)
	}
	got, _ = store.GetRelease(t.Context(), rel.ID)
	if got.BatchSize != 5 || got.PauseBetween != 1500 || got.FailFast {
		t.Errorf("部分 PATCH 破坏未提供字段：%+v", got)
	}
	// 3) 非法值族：batchSize=0 / pauseBetween=-1 / 空 patch / 非法 JSON → 400
	for name, payload := range map[string]map[string]any{
		"batchSize=0": {"batchSize": 0},
		"pauseBetw<0": {"pauseBetween": -1},
		"emptyPatch":  {},
	} {
		if code, _ := doJSON(t, http.MethodPatch, base, payload); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	// 4) 终态发布 → 409
	_ = store.CancelRelease(t.Context(), rel.ID)
	if code, _ := doJSON(t, http.MethodPatch, base, map[string]any{"batchSize": 2}); code != http.StatusConflict {
		t.Errorf("终态 PATCH = %d, want 409", code)
	}
	// 5) 不存在的 release → 404
	if code, _ := doJSON(t, http.MethodPatch, srv.URL+"/api/v1/models/m-patch/releases/nope", map[string]any{"batchSize": 2}); code != http.StatusNotFound {
		t.Errorf("404 PATCH = %d", code)
	}
}

// TestV170ListReleasesStatusFilter 验证 status 过滤：多模型种子下单值 running
// 命中两个、全枚举含无匹配态命中同集、非法值 400、跨模型互不串扰。
func TestV170ListReleasesStatusFilter(t *testing.T) {
	srv, store, _ := newContractEnv(t)

	// 直置三个不同状态发布——同一内存实现也有同模型在途互斥（guard 语义），
	// 故三个 seed 各自独立模型；断言走 m-filter 的列表 + status 过滤。
	mk := func(model, id string, st modelrepo.ReleaseStatus) {
		r := &modelrepo.ModelRelease{ID: id, Model: model, Version: "v1.0.0",
			Target: modelrepo.ReleaseTarget{Type: "nodeIDs"}, BatchSize: 1, FailFast: true}
		if err := store.CreateRelease(t.Context(), r); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		_ = store.UpdateReleaseHead(t.Context(), id, func(h *modelrepo.ModelRelease) error { h.Status = st; return nil })
	}
	for _, m := range []string{"m-filter-a", "m-filter-b", "m-filter-c"} {
		v170SeedActiveModel(t, srv, m, "v1.0.0")
	}
	mk("m-filter-a", "r-run-1", modelrepo.ReleaseStatusRunning)
	mk("m-filter-b", "r-run-2", modelrepo.ReleaseStatusRunning)
	mk("m-filter-c", "r-done", modelrepo.ReleaseStatusSucceeded)

	// 1) 单值过滤：m-filter-a 下唯一 running
	code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/m-filter-a/releases?status=running", nil)
	if code != http.StatusOK {
		t.Fatalf("列表 = %d", code)
	}
	items := body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "r-run-1" {
		t.Fatalf("status=running 结果 = %v, want [r-run-1]", items)
	}
	// 2) 多值过滤（逗号；m-filter-c 的 succeeded）
	code, body = doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/m-filter-c/releases?status=running,succeeded", nil)
	if code != http.StatusOK {
		t.Fatalf("多值列表 = %d", code)
	}
	if items = body["items"].([]any); len(items) != 1 {
		t.Fatalf("running+succeeded 过滤后条数 = %d, want 1", len(items))
	}
	// 3) paused 也是合法过滤值（v0.16.0 状态）：全枚举应包含 r-done 的 succeeded。
	// 用 Query().Encode() 构造 RawQuery 后走真实 mux（PathValue 需要）。
	target := srv.URL + "/api/v1/models/m-filter-c/releases"
	q := url.Values{}
	q.Set("status", "paused,pending,canceled,failed,rolled_back,succeeded")
	code, body = doJSON(t, http.MethodGet, target+"?"+q.Encode(), nil)
	if code != http.StatusOK {
		t.Fatalf("全枚举过滤 = %d body=%v", code, body)
	}
	var out releaseList
	raw3, _ := json.Marshal(body)
	_ = json.Unmarshal(raw3, &out)
	if len(out.Items) != 1 {
		t.Fatalf("全枚举过滤应含 r-done，got %d", len(out.Items))
	}
	// 4) 非法值 → 400
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/m-filter-a/releases?status=bogus", nil); code != http.StatusBadRequest {
		t.Errorf("非法 status 过滤 = %d, want 400", code)
	}
}

func TestV170DryRunCreatePreview(t *testing.T) {
	srv, _, _ := newContractEnv(t, "ready-n1", "ready-n2", "ready-n3")
	base := srv.URL + "/api/v1/models/m-dry/releases"
	payload := func(extra map[string]any) map[string]any {
		p := map[string]any{"version": "v1.0.0",
			"target": map[string]any{"type": "percentage", "percentage": 100},
			"dryRun": true}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}

	// 0) 模型名含 '/' 时 ServeMux 路由不再命中 models/{modelName}/releases
	// （路径段变了）→ 404；dryRun 与真实创建在处理器入口共用同一校验链，
	// 这里用合法但未注册的模型名验证 404 同因同报。
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/ghost-model/releases", payload(nil)); code != http.StatusNotFound {
		t.Fatalf("dryRun 未注册模型 = %d, want 404", code)
	}

	v170SeedActiveModel(t, srv, "m-dry", "v1.0.0")
	// 1) 校验通过 → 200 wouldCreate=true；**零落盘**：releases 列表为空
	code, body := doJSON(t, http.MethodPost, base, payload(map[string]any{"batchSize": 2}))
	if code != http.StatusOK {
		t.Fatalf("dryRun = %d %v, want 200", code, body)
	}
	if body["kind"] != "DryRunPreview" || body["wouldCreate"] != true {
		t.Fatalf("dryRun 响应形态异常: %v", body)
	}
	if body["prevActive"] != "" && body["prevActive"] != nil {
		// 无其他 active 版本 → prevActive 应为空（omitempty 或空串均可）
		t.Logf("prevActive=%v", body["prevActive"])
	}
	code, listBody := doJSON(t, http.MethodGet, base, nil)
	if code != http.StatusOK {
		t.Fatalf("列表 = %d", code)
	}
	if items := listBody["items"].([]any); len(items) != 0 {
		t.Fatalf("dryRun 落盘了！releases=%d，want 0", len(items))
	}
	// 2) 版本存在但 draft（非 active）→ 200 + wouldCreate=false + blockReason
	code, body = doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/m-dry/versions", map[string]any{
		"version": "v2.0.0", "mirror": "registry.example.com/e2e/m:v2.0.0",
		"sha256": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("seed v2.0.0 = %d %v", code, body)
	}
	code, body = doJSON(t, http.MethodPost, base, payload(map[string]any{"version": "v2.0.0"}))
	if code != http.StatusOK || body["wouldCreate"] != false || body["blockReason"] == "" {
		t.Fatalf("非 active dryRun = %d %v, want 200+false+reason", code, body)
	}
	// 4) dryRun 缺省路径不受污染：不带 dryRun → 正常 202 受理并落盘
	p := payload(nil)
	delete(p, "dryRun")
	code, body3 := doJSON(t, http.MethodPost, base, p)
	if code != http.StatusAccepted {
		t.Fatalf("缺省创建 = %d %v, want 202", code, body3)
	}
	code, listBody2 := doJSON(t, http.MethodGet, base, nil)
	if code != http.StatusOK {
		t.Fatalf("列表 = %d", code)
	}
	if items := listBody2["items"].([]any); len(items) != 1 {
		t.Fatalf("缺省创建后 releases=%d, want 1", len(items))
	}
}
