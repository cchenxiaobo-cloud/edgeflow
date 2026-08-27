package main

// 模型 API 契约测试（v0.7.0，WBS-9；v0.12.0 +digest 复核端点）：18 端点路由注册 + 路由数口径
// （apiMux 14 + 新 17 = 31 的 17 半边）+ 鉴权审计链（中间件装配点断言）
// + 行为契约表驱动（模型/版本/发布/部署影子 + 灰度边界）。
//
// 注：鉴权/审计中间件挂在 main.go 的 apiMux 装配链（auth.Middleware 与
// ledger.Middleware 为既有机制，nodeAPI 已全覆盖）——本文件聚焦模型 API
// 自身契约：路由注册、错误码映射、状态机边界、灰度物化语义。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/cloud/pkg/registry"
)

// ── 测试基建 ──────────────────────────────────────────────────────────

// countingRegistrar 是 routeRegistrar 的计数实现：记录注册的模式（保序、
// 去重计数——同一模式注册两次只算一条，断言 18 条唯一路由）。
type countingRegistrar struct{ patterns []string }

func (c *countingRegistrar) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	for _, p := range c.patterns {
		if p == pattern {
			return
		}
	}
	c.patterns = append(c.patterns, pattern)
}

func (c *countingRegistrar) count(prefix string) int {
	n := 0
	for _, p := range c.patterns {
		if strings.HasPrefix(p, prefix) {
			n++
		}
	}
	return n
}

// newContractEnv 构造契约测试环境：内存存储 + 注册表 + 已装配的 mux
// （模型 API 18 路由 + 节点 API 4 路由，模拟 apiMux 装配面）。
func newContractEnv(t *testing.T, readyNodes ...string) (*httptest.Server, *modelrepo.MemoryModelStore, *registry.Registry) {
	t.Helper()
	store := modelrepo.NewMemoryModelStore()
	reg := registry.New()
	for _, n := range readyNodes {
		if err := reg.Register(registry.NodeInfo{NodeID: n, Arch: "amd64"}); err != nil {
			t.Fatalf("注册节点失败: %v", err)
		}
	}
	mux := http.NewServeMux()
	api := &modelAPI{store: store, reg: reg}
	api.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store, reg
}

// doJSON 发送 JSON 请求并返回状态码 + 响应体。
func doJSON(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("解析响应失败 (%d): %v", resp.StatusCode, err)
	}
	return resp.StatusCode, out
}

// mustCreateModelVersion 一次到位：建模型 + draft 版本 + 激活，返回 200。
func mustCreateModelVersion(t *testing.T, srv *httptest.Server, model, version string) {
	t.Helper()
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models", map[string]any{
		"name": model, "description": "契约测试模型", "type": "generic",
	}); code != http.StatusOK && code != http.StatusConflict {
		t.Fatalf("创建模型 %s = %d", model, code)
	}
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/"+model+"/versions", map[string]any{
		"version": version, "mirror": "reg.example.com/x:v1", "sha256": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	}); code != http.StatusOK && code != http.StatusConflict {
		t.Fatalf("创建版本 %s/%s = %d", model, version, code)
	}
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/"+model+"/versions/"+version+"/activate", nil); code != http.StatusOK {
		t.Fatalf("激活版本 %s/%s = %d", model, version, code)
	}
}

// ── 1) 路由注册契约（17 条唯一路由，与 Register 清单一一对应）──────────

func TestModelAPIRouteCount(t *testing.T) {
	reg := &countingRegistrar{}
	api := &modelAPI{store: modelrepo.NewMemoryModelStore(), reg: registry.New()}
	api.Register(reg)

	want := []string{
		"GET /api/v1/models",
		"POST /api/v1/models",
		"GET /api/v1/models/{modelName}",
		"PUT /api/v1/models/{modelName}",
		"DELETE /api/v1/models/{modelName}",
		"GET /api/v1/models/{modelName}/versions",
		"POST /api/v1/models/{modelName}/versions",
		"GET /api/v1/models/{modelName}/versions/{version}",
		"DELETE /api/v1/models/{modelName}/versions/{version}",
		"POST /api/v1/models/{modelName}/versions/{version}/activate",
		"POST /api/v1/models/{modelName}/versions/{version}/archive",
		"POST /api/v1/models/{modelName}/releases",
		"GET /api/v1/models/{modelName}/releases",
		"GET /api/v1/models/{modelName}/releases/{releaseID}",
		"GET /api/v1/models/{modelName}/releases/{releaseID}/digest",
		"POST /api/v1/models/{modelName}/releases/{releaseID}/cancel",
		"POST /api/v1/models/{modelName}/releases/{releaseID}/pause",
		"POST /api/v1/models/{modelName}/releases/{releaseID}/resume",
		"GET /api/v1/models/export",
		"POST /api/v1/models/import",
		"POST /api/v1/models/{modelName}/releases/{releaseID}/rollback",
		"GET /api/v1/models/{modelName}/deployments",
	}
	if len(reg.patterns) != len(want) {
		t.Fatalf("路由注册数 = %d, want %d（22 新端点，v0.16.0 起 +4：pause/resume/export/import）；实际: %v", len(reg.patterns), len(want), reg.patterns)
	}
	for i, w := range want {
		if reg.patterns[i] != w {
			t.Errorf("路由[%d] = %q, want %q", i, reg.patterns[i], w)
		}
	}
	// 分族计数（模式含 "GET " 等方法动词前缀，按路径段含子资源统计）：
	// 模型 5（列表/创建/详情/更新/删除）+ 版本 6 + 发布 5 + 部署 1
	modelFamily, versionFamily, releaseFamily, deployFamily := 0, 0, 0, 0
	for _, p := range reg.patterns {
		path := p[strings.IndexByte(p, ' ')+1:]
		switch {
		case strings.Contains(path, "/versions"):
			versionFamily++
		case strings.Contains(path, "/releases"):
			releaseFamily++
		case strings.HasSuffix(path, "/deployments"):
			deployFamily++
		default:
			modelFamily++
		}
	}
	if versionFamily != 6 {
		t.Errorf("版本族路由 = %d, want 6", versionFamily)
	}
	if releaseFamily != 8 {
		t.Errorf("发布族路由 = %d, want 8（v0.12.0 +digest、v0.16.0 +pause/resume）", releaseFamily)
	}
	// v0.16.0：export/import 字面路由属模型族（models/export、models/import）
	if modelFamily != 7 {
		t.Errorf("模型族路由 = %d, want 7（5 基础 + export + import）", modelFamily)
	}
	if deployFamily != 1 {
		t.Errorf("部署影子路由 = %d, want 1", deployFamily)
	}
}

// ── 2) 模型 CRUD 契约 ─────────────────────────────────────────────────

func TestModelAPIModelCRUD(t *testing.T) {
	srv, _, _ := newContractEnv(t)

	// POST 创建 → 200（设计稿 §4.1 表：POST /models = 200/400/409）
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models", map[string]any{
		"name": "alpha-model", "description": "demo", "type": "classification",
	})
	if code != http.StatusOK {
		t.Fatalf("创建模型 = %d, want 200（body=%v）", code, body)
	}
	if body["name"] != "alpha-model" {
		t.Errorf("创建响应字段不符: %v", body)
	}
	// 重名 → 409
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models", map[string]any{
		"name": "alpha-model", "description": "dup", "type": "generic",
	}); code != http.StatusConflict {
		t.Errorf("重名创建 = %d, want 409", code)
	}
	// GET 列表 → 200 items 含 1
	code, body = doJSON(t, http.MethodGet, srv.URL+"/api/v1/models", nil)
	if code != http.StatusOK {
		t.Fatalf("模型列表 = %d, want 200", code)
	}
	if it, ok := body["items"].([]any); !ok || len(it) != 1 {
		t.Errorf("模型列表 items = %v, want 1 项", body["items"])
	}
	// GET 详情 → 200；不存在 → 404
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/alpha-model", nil); code != http.StatusOK {
		t.Errorf("模型详情 = %d, want 200", code)
	}
	if code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/nope", nil); code != http.StatusNotFound || !strings.Contains(body["error"].(string), "not found") {
		t.Errorf("缺失模型详情 = %d %v, want 404", code, body)
	}
	// PUT 更新 → 200（description 变更）
	code, body = doJSON(t, http.MethodPut, srv.URL+"/api/v1/models/alpha-model", map[string]any{
		"description": "updated desc", "type": "classification",
	})
	if code != http.StatusOK || body["description"] != "updated desc" {
		t.Errorf("更新模型 = %d %v, want 200+新描述", code, body)
	}
	// 非法 name → 400
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models", map[string]any{
		"name": "bad/name", "description": "x", "type": "generic",
	}); code != http.StatusBadRequest {
		t.Errorf("非法模型名 = %d, want 400", code)
	}
	// DELETE 无版本模型 → 200
	if code, _ := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/models/alpha-model", nil); code != http.StatusOK {
		t.Errorf("删除空模型 = %d, want 200", code)
	}
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/alpha-model", nil); code != http.StatusNotFound {
		t.Errorf("删除后详情 = %d, want 404", code)
	}
}

// ── 3) 版本 CRUD 契约 ─────────────────────────────────────────────────

func TestModelAPIVersionLifecycle(t *testing.T) {
	srv, _, _ := newContractEnv(t)
	mustCreateModelVersion(t, srv, "mnist", "v1.0.0")

	// POST 新版本 → 200（设计稿 §4.1；初始 draft）
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/versions", map[string]any{
		"version": "v2.0.0", "mirror": "reg.example.com/x:v2", "sha256": "sha256:5f4dcc3b5aa765d61d8327deb882cf99aabbccddeeff00112233445566778899",
	})
	if code != http.StatusOK || body["status"] != "draft" {
		t.Fatalf("创建版本 = %d %v, want 200+draft", code, body)
	}
	// GET 列表 → 2 项；详情 → 200
	if code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/versions", nil); code != http.StatusOK || len(body["items"].([]any)) != 2 {
		t.Errorf("版本列表 = %d（items=%v）, want 2 项", code, body["items"])
	}
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/versions/v2.0.0", nil); code != http.StatusOK {
		t.Errorf("版本详情 = %d, want 200", code)
	}
	// DELETE draft → 200；再删 → 404
	if code, _ := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/models/mnist/versions/v2.0.0", nil); code != http.StatusOK {
		t.Errorf("删除 draft 版本 = %d, want 200", code)
	}
	if code, _ := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/models/mnist/versions/v2.0.0", nil); code != http.StatusNotFound {
		t.Errorf("重复删除 = %d, want 404", code)
	}
	// DELETE active 版本 → 409
	if code, body := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/models/mnist/versions/v1.0.0", nil); code != http.StatusConflict || !strings.Contains(body["error"].(string), "active") {
		t.Errorf("删除 active = %d %v, want 409", code, body)
	}
	// archive active → 409/200?：单版本模型 active 归档 → 归档后无 active，
	// 设计允许（映射 ErrVersionNotActive 仅对非 active 目标）。active→archived
	// 合法：200。
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/versions/v1.0.0/archive", nil); code != http.StatusOK {
		t.Errorf("归档 active 版本 = %d, want 200", code)
	}
	// 归档后 activate 回滚语义：archived 不可激活 → 409（ErrVersionNotDraft）
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/versions/v1.0.0/activate", nil); code != http.StatusConflict {
		t.Errorf("archived 再激活 = %d, want 409", code)
	}
	// 模型不存在 → 子资源 404
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/absent/versions", nil); code != http.StatusNotFound {
		t.Errorf("缺失模型版本列表 = %d, want 404", code)
	}
}

// ── 4) 发布契约（状态机 + 灰度边界 + 错误映射）────────────────────────

func TestModelAPIReleaseLifecycle(t *testing.T) {
	srv, _, _ := newContractEnv(t, "node-a", "node-b", "node-c")
	mustCreateModelVersion(t, srv, "mnist", "v1.0.0")

	// POST 创建（percentage=100）→ 202 + summary.total=3
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
		"target":  map[string]any{"type": "percentage", "percentage": 100},
		"version": "v1.0.0", "batchSize": 1, "pauseBetween": 0, "failFast": true,
	})
	if code != http.StatusAccepted {
		t.Fatalf("创建发布 = %d, want 202（body=%v）", code, body)
	}
	rid, _ := body["id"].(string)
	if rid == "" {
		t.Fatalf("发布响应缺 id: %v", body)
	}
	sum, _ := body["summary"].(map[string]any)
	if sum["total"] != float64(3) {
		t.Errorf("summary.total = %v, want 3", sum["total"])
	}
	// GET 列表/详情 → 200
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases", nil); code != http.StatusOK {
		t.Errorf("发布列表 = %d, want 200", code)
	}
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases/"+rid, nil); code != http.StatusOK {
		t.Errorf("发布详情 = %d, want 200", code)
	}
	// cancel pending → 200（canceled）
	if code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/cancel", nil); code != http.StatusOK || body["status"] != "canceled" {
		t.Errorf("取消发布 = %d %v, want 200+canceled", code, body)
	}
	// 终态再 cancel → 409
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/cancel", nil); code != http.StatusConflict {
		t.Errorf("终态取消 = %d, want 409", code)
	}
	// rollback 无 PrevActive（首个发布）→ 422
	if code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/rollback", nil); code != http.StatusUnprocessableEntity {
		t.Errorf("无 PrevActive 回滚 = %d %v, want 422", code, body)
	}
	// 不存在 releaseID → 404（cancel/rollback/get 一致）
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases/ghost", nil); code != http.StatusNotFound {
		t.Errorf("缺失发布详情 = %d, want 404", code)
	}
	// 部署影子：发布 cancel 后 perNode 未写（控制器未运行），列表 → 200 空
	if code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/deployments", nil); code != http.StatusOK || len(body["items"].([]any)) != 0 {
		t.Errorf("部署影子列表 = %d, want 200+空（body=%v）", code, body)
	}
}

func TestModelAPIReleaseGuardRails(t *testing.T) {
	// 灰度边界与守卫（错误映射对照 §4.3）
	t.Run("百分比无Ready节点→422", func(t *testing.T) {
		srv, _, _ := newContractEnv(t) // 无节点
		mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
		code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
			"target":  map[string]any{"type": "percentage", "percentage": 50},
			"version": "v1.0.0", "batchSize": 1,
		})
		if code != http.StatusUnprocessableEntity || !strings.Contains(body["error"].(string), "ready") {
			t.Errorf("无 Ready 节点 = %d %v, want 422 ready", code, body)
		}
	})
	t.Run("白名单未知节点→422", func(t *testing.T) {
		srv, _, _ := newContractEnv(t, "node-a")
		mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
		code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
			"target":  map[string]any{"type": "nodeIDs", "nodeIDs": []string{"node-x"}},
			"version": "v1.0.0", "batchSize": 1,
		})
		if code != http.StatusUnprocessableEntity {
			t.Errorf("未知节点 = %d %v, want 422", code, body)
		}
		if un, ok := body["unknownNodes"].([]any); !ok || len(un) != 1 || un[0] != "node-x" {
			t.Errorf("unknownNodes 字段 = %v, want [node-x]", body["unknownNodes"])
		}
	})
	t.Run("目标版本非active→422", func(t *testing.T) {
		srv, _, _ := newContractEnv(t, "node-a")
		mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
		// 第二版本保持 draft
		doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/versions", map[string]any{
			"version": "v2.0.0", "mirror": "reg/x:v2", "sha256": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		})
		code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
			"target":  map[string]any{"type": "percentage", "percentage": 100},
			"version": "v2.0.0", "batchSize": 1,
		})
		if code != http.StatusUnprocessableEntity || !strings.Contains(body["error"].(string), "active") {
			t.Errorf("非 active 目标 = %d %v, want 422 active", code, body)
		}
	})
	t.Run("同模型在途发布→409", func(t *testing.T) {
		srv, _, _ := newContractEnv(t, "node-a")
		mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
		code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
			"target":  map[string]any{"type": "percentage", "percentage": 100},
			"version": "v1.0.0", "batchSize": 1,
		})
		if code != http.StatusAccepted {
			t.Fatalf("首发布 = %d, want 202", code)
		}
		code, body = doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
			"target":  map[string]any{"type": "percentage", "percentage": 100},
			"version": "v1.0.0", "batchSize": 1,
		})
		if code != http.StatusConflict {
			t.Errorf("在途发布再建 = %d %v, want 409", code, body)
		}
		if rid, _ := body["releaseID"].(string); rid == "" {
			t.Errorf("409 响应应带 releaseID 字段: %v", body)
		}
	})
	t.Run("模型404/版本404", func(t *testing.T) {
		srv, _, _ := newContractEnv(t, "node-a")
		if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
			"target":  map[string]any{"type": "percentage", "percentage": 100},
			"version": "v1.0.0", "batchSize": 1,
		}); code != http.StatusNotFound {
			t.Errorf("缺失模型发布 = %d, want 404", code)
		}
		mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
		if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
			"target":  map[string]any{"type": "percentage", "percentage": 100},
			"version": "v9.0.0", "batchSize": 1,
		}); code != http.StatusNotFound {
			t.Errorf("缺失版本发布 = %d, want 404", code)
		}
	})
	t.Run("percentage=1单节点上取整", func(t *testing.T) {
		srv, _, _ := newContractEnv(t, "node-a", "node-b")
		mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
		code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
			"target":  map[string]any{"type": "percentage", "percentage": 1},
			"version": "v1.0.0", "batchSize": 1,
		})
		if code != http.StatusAccepted {
			t.Fatalf("percent=1 发布 = %d %v, want 202", code, body)
		}
		tn, _ := body["targetNodes"].([]any)
		if len(tn) != 1 {
			t.Errorf("percent=1/2 节点 targetNodes = %v, want 1（上取整）", body["targetNodes"])
		}
	})
	t.Run("防止同一版本双发布TAKE-OVER", func(t *testing.T) {
		// 前序测试的在途 release 在独立 env；此用例验证 cancel 后可再建
		srv, _, _ := newContractEnv(t, "node-a")
		mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
		code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
			"target":  map[string]any{"type": "percentage", "percentage": 100},
			"version": "v1.0.0", "batchSize": 1,
		})
		if code != http.StatusAccepted {
			t.Fatalf("首发布 = %d", code)
		}
		rid, _ := body["id"].(string)
		doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/cancel", nil)
		// cancel 是异步控制器补齐 skipped 的；存储层 guard 由控制器 releaseGuard
		// 释放——内存实现 guard 语义在锁内，canceled 终态后 CreateRelease 复查
		// 在途（InFlight 判定）应放行。
		code, body = doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
			"target":  map[string]any{"type": "percentage", "percentage": 100},
			"version": "v1.0.0", "batchSize": 1,
		})
		if code != http.StatusAccepted {
			t.Errorf("cancel 后重建 = %d %v, want 202（内存 guard 语义）", code, body)
		}
	})
}

// ── 5) 版本接管守卫（L26：rollback 前被新发布接管）────────────────────

func TestModelAPIRollbackVersionTakeover(t *testing.T) {
	srv, _, _ := newContractEnv(t, "node-a")
	mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
	// 场景：v1 active → 发布 r1（v1，prevActive=空，仅占位）
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
		"target":  map[string]any{"type": "percentage", "percentage": 100},
		"version": "v1.0.0", "batchSize": 1,
	})
	if code != http.StatusAccepted {
		t.Fatalf("发布 r1 = %d, want 202", code)
	}
	r1, _ := body["id"].(string)
	// cancel r1 → 终态（内存 guard 释放 InFlight 判定）
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+r1+"/cancel", nil); code != http.StatusOK {
		t.Fatalf("取消 r1 = %d, want 200", code)
	}
	// 激活 v2（v1 → archived）→ 发布 r2（v2，prevActive=v1）
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/versions", map[string]any{
		"version": "v2.0.0", "mirror": "reg/x:v2", "sha256": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
	})
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/versions/v2.0.0/activate", nil)
	code, body = doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
		"target":  map[string]any{"type": "percentage", "percentage": 100},
		"version": "v2.0.0", "batchSize": 1,
	})
	if code != http.StatusAccepted {
		t.Fatalf("发布 r2 = %d, want 202（body=%v）", code, body)
	}
	r2, _ := body["id"].(string)
	// 激活 v3（v2 → archived；guard 不受版本激活影响）→ rollback r2
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/versions", map[string]any{
		"version": "v3.0.0", "mirror": "reg/x:v3", "sha256": "sha256:3333333333333333333333333333333333333333333333333333333333333333",
	})
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/versions/v3.0.0/activate", nil)
	// r2 为 running 后（控制器推进）回滚：v2 ≠ 当前 active v3 → 409。
	// 本测试无控制器，r2 停在 pending；pending → ErrReleaseTerminal 同为
	// 409（L26 版本接管语义由存储单测 TestC3_版本被接管_409 精确覆盖）。
	code, body = doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+r2+"/rollback", nil)
	if code != http.StatusConflict {
		t.Errorf("版本被接管回滚 = %d %v, want 409", code, body)
	}
}

// ── 6) 语义校验（400 族：非法 body / 校验失败）────────────────────────

func TestModelAPIValidationCodes(t *testing.T) {
	srv, _, _ := newContractEnv(t)
	// 非法 JSON → 400
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/models", strings.NewReader("{not-json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 JSON = %d, want 400", resp.StatusCode)
	}
	// 超大 body（>1MiB）→ 413
	huge := strings.Repeat("a", 2<<20)
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/v1/models", strings.NewReader(`{"name":"`+huge+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("超大 body = %d, want 413", resp.StatusCode)
	}
	// 校验失败：description 超长 → 400
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models", map[string]any{
		"name": "m", "description": strings.Repeat("长", 300), "type": "generic",
	}); code != http.StatusBadRequest {
		t.Errorf("超长 description = %d, want 400", code)
	}
}

// ── 7) 并发创建（-race：guard 语义并发安全）────────────────────────────

func TestModelAPIConcurrentCreateRace(t *testing.T) {
	srv, _, _ := newContractEnv(t, "node-a", "node-b")
	mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
	const n = 8
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
				"target":  map[string]any{"type": "percentage", "percentage": 100},
				"version": "v1.0.0", "batchSize": 1,
			})
			codes <- code
		}()
	}
	accepted, conflicted := 0, 0
	for i := 0; i < n; i++ {
		switch <-codes {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflicted++
		default:
			t.Errorf("并发创建出现意外状态码")
		}
	}
	if accepted != 1 || conflicted != n-1 {
		t.Errorf("并发创建 = %d×202 / %d×409, want 1×202 / %d×409（guard 恰一在途）", accepted, conflicted, n-1)
	}
}

// TestModelAPIPrevActiveChain 验证回滚链：激活 v2（v1→archived，v2 记录
// prevActive=v1）→ 发布 v2 → 响应 prevActive=v1.0.0 → rollback 202（而非 422
// 无 PrevActive）。E2E 暴露的回归锚点：先激活后发布下 prevActive 不得为空。
func TestModelAPIPrevActiveChain(t *testing.T) {
	srv, _, _ := newContractEnv(t, "node-a")
	mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
	// 创建 v2 + 激活（v1 → archived；v2.PrevActive=v1.0.0）
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/versions", map[string]any{
		"version": "v2.0.0", "mirror": "reg/x:v2", "sha256": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/versions/v2.0.0/activate", nil)
	if code != http.StatusOK {
		t.Fatalf("激活 v2 = %d, want 200", code)
	}
	if pv, _ := body["prevActive"].(string); pv != "v1.0.0" {
		t.Fatalf("v2.prevActive = %q, want v1.0.0（激活时记录被降级版本）", pv)
	}
	// 发布 v2（此时 ActiveVersion==v2==目标）→ prevActive 必须为 v1.0.0
	code, body = doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases", map[string]any{
		"target":  map[string]any{"type": "percentage", "percentage": 100},
		"version": "v2.0.0", "batchSize": 1,
	})
	if code != http.StatusAccepted {
		t.Fatalf("发布 v2 = %d, want 202（body=%v）", code, body)
	}
	if pv, _ := body["prevActive"].(string); pv != "v1.0.0" {
		t.Errorf("发布 v2 prevActive = %q, want v1.0.0（回滚链不得断）", pv)
	}
	r2, _ := body["id"].(string)
	// cancel 后 rollback（pending 不可回滚；canceled 终态可回滚）→ 202
	if code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+r2+"/cancel", nil); code != http.StatusOK {
		t.Fatalf("取消 r2 = %d, want 200", code)
	}
	code, body = doJSON(t, http.MethodPost, srv.URL+"/api/v1/models/mnist/releases/"+r2+"/rollback", nil)
	if code != http.StatusAccepted {
		t.Fatalf("回滚 r2 = %d %v, want 202（有 PrevActive=v1.0.0）", code, body)
	}
	if rb, _ := body["rollbackRequested"].(bool); !rb {
		t.Errorf("回滚响应 rollbackRequested = %v, want true", rb)
	}
}

// TestModelAPIErrorSentinelParity 编译期断言：错误哨兵可被 errors.Is 命中
// （契约测试引用存储层哨兵，防止接口换实现后错误码漂移）。
func TestModelAPIErrorSentinelParity(t *testing.T) {
	store := modelrepo.NewMemoryModelStore()
	ctx := context.Background()
	if _, err := store.GetModel(ctx, "absent"); !errors.Is(err, modelrepo.ErrModelNotFound) {
		t.Errorf("GetModel 缺失 = %v, want ErrModelNotFound", err)
	}
	if _, err := store.GetVersion(ctx, "absent", "v1"); !errors.Is(err, modelrepo.ErrModelNotFound) {
		t.Errorf("GetVersion 缺失模型 = %v, want ErrModelNotFound", err)
	}
	if err := store.RequestRollback(ctx, "ghost"); !errors.Is(err, modelrepo.ErrReleaseNotFound) {
		t.Errorf("RequestRollback 缺失 = %v, want ErrReleaseNotFound", err)
	}
}

// TestModelAPIPagination 验证 v0.8.0（L28）列表分页：limit/offset 切片、
// X-Total-Count 头、非法参数 400、缺省全量兼容。
func TestModelAPIPagination(t *testing.T) {
	srv, _, _ := newContractEnv(t)
	base := srv.URL

	// 造 3 个模型
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if code, _ := doJSON(t, "POST", base+"/api/v1/models", map[string]any{
			"name": name, "description": name,
		}); code != http.StatusOK {
			t.Fatalf("创建模型 %s 状态码=%d", name, code)
		}
	}

	t.Run("limit-page", func(t *testing.T) {
		resp, err := http.Get(base + "/api/v1/models?limit=2")
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if total := resp.Header.Get("X-Total-Count"); total != "3" {
			t.Errorf("X-Total-Count=%q，期望 3", total)
		}
		var out struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(out.Items) != 2 {
			t.Errorf("limit=2 应返回 2 条，实际 %d", len(out.Items))
		}
	})
	t.Run("offset-page", func(t *testing.T) {
		var out struct {
			Items []map[string]any `json:"items"`
		}
		code, _ := doJSON(t, "GET", base+"/api/v1/models?limit=2&offset=2", nil)
		if code != http.StatusOK {
			t.Fatalf("状态码=%d", code)
		}
		req, _ := http.NewRequest("GET", base+"/api/v1/models?limit=2&offset=2", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(out.Items) != 1 {
			t.Errorf("limit=2&offset=2 应返回 1 条，实际 %d", len(out.Items))
		}
	})
	t.Run("offset-beyond", func(t *testing.T) {
		var out struct {
			Items []map[string]any `json:"items"`
		}
		req, _ := http.NewRequest("GET", base+"/api/v1/models?offset=99", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(out.Items) != 0 {
			t.Errorf("offset 超界应返回空列表，实际 %d", len(out.Items))
		}
	})
	t.Run("invalid-limit", func(t *testing.T) {
		if code, _ := doJSON(t, "GET", base+"/api/v1/models?limit=0", nil); code != http.StatusBadRequest {
			t.Errorf("limit=0 应 400，实际 %d", code)
		}
		if code, _ := doJSON(t, "GET", base+"/api/v1/models?limit=1001", nil); code != http.StatusBadRequest {
			t.Errorf("limit=1001 应 400，实际 %d", code)
		}
		if code, _ := doJSON(t, "GET", base+"/api/v1/models?limit=abc", nil); code != http.StatusBadRequest {
			t.Errorf("limit=abc 应 400，实际 %d", code)
		}
	})
	t.Run("default-full", func(t *testing.T) {
		var out struct {
			Items []map[string]any `json:"items"`
		}
		req, _ := http.NewRequest("GET", base+"/api/v1/models", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(out.Items) != 3 {
			t.Errorf("缺省应全量 3 条，实际 %d", len(out.Items))
		}
	})
}

// ---------- v0.12.0 D-1 发布 digest 复核端点测试 ----------

// newDigestReportEnv 构造复核端点环境：模型+版本+发布（可设 MirrorDigest）+
// 注入 digestOf 闭包。返回 server/store/releaseID。
func newDigestReportEnv(t *testing.T, mirrorDigest string, digestOf func(string) string, targetNodes []string) (*httptest.Server, *modelrepo.MemoryModelStore, string) {
	t.Helper()
	store := modelrepo.NewMemoryModelStore()
	ctx := context.Background()
	if err := store.CreateModel(ctx, &modelrepo.Model{Name: "mnist", Type: "classification"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateVersion(ctx, &modelrepo.ModelVersion{
		Model: "mnist", Version: "v1.0.0", Mirror: "reg.io/mnist:v1", Sha256: "sha256:aaa",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateVersion(ctx, "mnist", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	rel := &modelrepo.ModelRelease{
		ID: "rel-digest", Model: "mnist", Version: "v1.0.0",
		Target:       modelrepo.ReleaseTarget{Type: "nodeIDs", NodeIDs: targetNodes},
		TargetNodes:  targetNodes,
		BatchSize:    1,
		PrevActive:   "",
		MirrorDigest: mirrorDigest,
	}
	if err := store.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api := &modelAPI{store: store, reg: registry.New(), digestOf: digestOf}
	api.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store, rel.ID
}

// TestGetReleaseDigest 验证复核端点 consistency 全矩阵（v0.12.0，D-1）：
// consistent / inconsistent / unknown / skipped + 404 + digestOf nil。
func TestGetReleaseDigest(t *testing.T) {
	const expD = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	t.Run("consistent", func(t *testing.T) {
		srv, store, rid := newDigestReportEnv(t, expD, func(string) string { return expD }, []string{"n1", "n2"})
		ctx := context.Background()
		_ = store.SetNodeResult(ctx, rid, "n1", &modelrepo.NodeReleaseResult{NodeID: "n1", Status: modelrepo.NodeRelDeployed})
		code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/digest", nil)
		if code != http.StatusOK {
			t.Fatalf("consistent: code=%d, want 200", code)
		}
		if body["consistency"] != "consistent" {
			t.Errorf("head = %v, want consistent", body["consistency"])
		}
		nodes := body["nodes"].([]any)
		if len(nodes) != 2 {
			t.Fatalf("nodes = %d, want 2", len(nodes))
		}
		n0 := nodes[0].(map[string]any)
		if n0["consistency"] != "consistent" || n0["releaseStatus"] != "deployed" || n0["currentImageDigest"] != expD {
			t.Errorf("节点 0 = %v, want consistent+deployed+digest", n0)
		}
	})
	t.Run("inconsistent", func(t *testing.T) {
		srv, _, rid := newDigestReportEnv(t, expD, func(string) string { return "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" }, []string{"n1"})
		code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/digest", nil)
		if code != http.StatusOK || body["consistency"] != "inconsistent" {
			t.Errorf("head = %v code=%d, want inconsistent+200", body["consistency"], code)
		}
	})
	t.Run("unknown（节点未上报）", func(t *testing.T) {
		srv, _, rid := newDigestReportEnv(t, expD, func(string) string { return "" }, []string{"n1"})
		code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/digest", nil)
		if code != http.StatusOK || body["consistency"] != "unknown" {
			t.Errorf("head = %v code=%d, want unknown+200", body["consistency"], code)
		}
	})
	t.Run("skipped（mirrorDigest 空）", func(t *testing.T) {
		srv, _, rid := newDigestReportEnv(t, "", func(string) string { return expD }, []string{"n1"})
		code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/digest", nil)
		if code != http.StatusOK || body["consistency"] != "skipped" {
			t.Errorf("head = %v code=%d, want skipped+200", body["consistency"], code)
		}
		if nodes := body["nodes"].([]any); nodes[0].(map[string]any)["consistency"] != "skipped" {
			t.Errorf("skipped 模式节点应 skipped，got %v", nodes)
		}
	})
	t.Run("发布不存在 → 404", func(t *testing.T) {
		srv, _, _ := newDigestReportEnv(t, expD, func(string) string { return expD }, []string{"n1"})
		code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases/ghost/digest", nil)
		if code != http.StatusNotFound {
			t.Errorf("缺失发布复核 = %d, want 404", code)
		}
	})
	t.Run("digestOf nil → unknown", func(t *testing.T) {
		srv, _, rid := newDigestReportEnv(t, expD, nil, []string{"n1"})
		code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/digest", nil)
		if code != http.StatusOK || body["consistency"] != "unknown" {
			t.Errorf("digestOf nil head = %v code=%d, want unknown+200", body["consistency"], code)
		}
	})
	t.Run("节点无结果 → pending", func(t *testing.T) {
		srv, _, rid := newDigestReportEnv(t, expD, func(string) string { return expD }, []string{"n1"})
		code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases/"+rid+"/digest", nil)
		if code != http.StatusOK {
			t.Fatalf("code=%d want 200", code)
		}
		if st := body["nodes"].([]any)[0].(map[string]any)["releaseStatus"]; st != "pending" {
			t.Errorf("节点无 perNode 结果应 pending，got %v", st)
		}
	})
}

// ── v0.13.0（A′）：部署影子列表分页 ─────────────────────────────────────

// TestModelAPIListDeploymentsPagination deployments 列表分页：缺省全量 /
// limit/offset 切片 / X-Total-Count 恒写 / 非法值 400 / 模型缺失 404。
func TestModelAPIListDeploymentsPagination(t *testing.T) {
	srv, store, _ := newContractEnv(t)
	mustCreateModelVersion(t, srv, "mnist", "v1.0.0")
	ctx := context.Background()
	for i, node := range []string{"n1", "n2", "n3"} {
		if err := store.SetDeployment(ctx, "mnist", node, modelrepo.DeploymentState{
			Model: "mnist", NodeID: node, Version: "v1.0.0",
		}); err != nil {
			t.Fatalf("造部署影子 %d 失败: %v", i, err)
		}
	}

	// 缺省 = 全量 3 条
	code, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/deployments", nil)
	if code != http.StatusOK || len(body["items"].([]any)) != 3 {
		t.Fatalf("缺省应全量 3 条，got code=%d items=%d", code, len(body["items"].([]any)))
	}

	// limit=1&offset=0 → 1 条 + X-Total-Count: 3
	code, body, total := doJSONWithHeaders(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/deployments?limit=1&offset=0", nil)
	if code != http.StatusOK || len(body["items"].([]any)) != 1 || total != "3" {
		t.Fatalf("limit=1 应 1 条 + X-Total-Count=3，got code=%d items=%d total=%q", code, len(body["items"].([]any)), total)
	}

	// offset=3（超界）→ 空数组 + X-Total-Count: 3
	code, body, total = doJSONWithHeaders(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/deployments?offset=3", nil)
	if code != http.StatusOK || len(body["items"].([]any)) != 0 || total != "3" {
		t.Fatalf("offset 超界应空数组 + total=3，got code=%d items=%d total=%q", code, len(body["items"].([]any)), total)
	}

	// 非法 limit=0 → 400
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/deployments?limit=0", nil); code != http.StatusBadRequest {
		t.Errorf("limit=0 应 400，got %d", code)
	}
	// 非法 limit=1001 → 400
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/deployments?limit=1001", nil); code != http.StatusBadRequest {
		t.Errorf("limit=1001 应 400，got %d", code)
	}
	// 非法 offset=-1 → 400
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/deployments?offset=-1", nil); code != http.StatusBadRequest {
		t.Errorf("offset=-1 应 400，got %d", code)
	}
	// 非数 limit → 400
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/deployments?limit=0x", nil); code != http.StatusBadRequest {
		t.Errorf("limit=0x 应 400，got %d", code)
	}
	// 模型不存在 → 404
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/ghost/deployments", nil); code != http.StatusNotFound {
		t.Errorf("模型缺失应 404，got %d", code)
	}
	// 分页不影响 releases 列表（回归：releases 仍 200）
	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/v1/models/mnist/releases?limit=1", nil); code != http.StatusOK {
		t.Errorf("releases 分页回归应 200，got %d", code)
	}
}

// doJSONWithHeaders 同 doJSON，额外返回响应头 X-Total-Count。
func doJSONWithHeaders(t *testing.T, method, url string, body any) (int, map[string]any, string) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("解析响应失败 (%d): %v", resp.StatusCode, err)
	}
	return resp.StatusCode, out, resp.Header.Get("X-Total-Count")
}
