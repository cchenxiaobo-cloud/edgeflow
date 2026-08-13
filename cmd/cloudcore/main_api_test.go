package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1alpha1 "edgeflow/apis/edge/v1alpha1"
	"edgeflow/cloud/pkg/registry"
)

// newAPIServer 构造带节点查询路由的测试 HTTP 服务（两个视角：
// /api/v1/nodes 与 /api/v1/edgenodes 共用同一注册表）。
func newAPIServer(t *testing.T, reg *registry.Registry) *httptest.Server {
	t.Helper()
	api := &nodeAPI{reg: reg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/nodes", api.listNodes)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}", api.getNode)
	mux.HandleFunc("GET /api/v1/edgenodes", api.listEdgeNodes)
	mux.HandleFunc("GET /api/v1/edgenodes/{nodeID}", api.getEdgeNode)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestNodeAPIList 验证 GET /api/v1/nodes 返回全部节点（JSON 数组、按 NodeID 排序）。
func TestNodeAPIList(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.NodeInfo{NodeID: "node-b", Arch: "arm64"})
	reg.Register(registry.NodeInfo{NodeID: "node-a", Arch: "amd64"})

	srv := newAPIServer(t, reg)
	resp, err := http.Get(srv.URL + "/api/v1/nodes")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}

	var nodes []registry.NodeInfo
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("节点数 = %d，期望 2", len(nodes))
	}
	if nodes[0].NodeID != "node-a" || nodes[1].NodeID != "node-b" {
		t.Errorf("节点应按 NodeID 排序: %+v", nodes)
	}
	if nodes[0].Arch != "amd64" {
		t.Errorf("JSON 应携带 NodeInfo 字段: %+v", nodes[0])
	}
}

// TestNodeAPIGet 验证单节点查询与 404。
func TestNodeAPIGet(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.NodeInfo{NodeID: "node-1", Status: registry.StatusReady})

	srv := newAPIServer(t, reg)

	// 存在：返回详情
	resp, err := http.Get(srv.URL + "/api/v1/nodes/node-1")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	var node registry.NodeInfo
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if node.NodeID != "node-1" {
		t.Errorf("nodeID = %q，期望 node-1", node.NodeID)
	}

	// 不存在：404 + JSON 错误信息
	resp, err = http.Get(srv.URL + "/api/v1/nodes/nope")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在节点状态码 = %d，期望 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析 404 响应失败: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("404 响应应含错误信息: %v", body)
	}
}

// TestNodeAPIMethodNotAllowed 验证非 GET 方法返回 405。
func TestNodeAPIMethodNotAllowed(t *testing.T) {
	srv := newAPIServer(t, registry.New())
	resp, err := http.Post(srv.URL+"/api/v1/nodes", "application/json", nil)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d，期望 405", resp.StatusCode)
	}
}

// TestEdgeNodeAPIList 验证 GET /api/v1/edgenodes 返回 K8s List 风格的
// EdgeNode 对象列表（kind/apiVersion + items，按 Name 排序）。
func TestEdgeNodeAPIList(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.NodeInfo{NodeID: "node-b", Arch: "amd64", IP: "10.0.0.2"})
	reg.Register(registry.NodeInfo{NodeID: "node-a", Arch: "arm64", IP: "10.0.0.1"})

	srv := newAPIServer(t, reg)
	resp, err := http.Get(srv.URL + "/api/v1/edgenodes")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}

	var list edgeNodeList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if list.Kind != "EdgeNodeList" || list.APIVersion != v1alpha1.SchemeGroupVersion.String() {
		t.Errorf("List 元信息 = %s/%s，期望 EdgeNodeList/%s",
			list.Kind, list.APIVersion, v1alpha1.SchemeGroupVersion.String())
	}
	if len(list.Items) != 2 {
		t.Fatalf("items 长度 = %d，期望 2", len(list.Items))
	}
	if list.Items[0].Name != "node-a" || list.Items[1].Name != "node-b" {
		t.Errorf("items 应按 Name 排序: %+v", list.Items)
	}
	first := list.Items[0]
	if first.Kind != "EdgeNode" || first.Spec.NodeID != "node-a" {
		t.Errorf("item 应为完整 EdgeNode 对象: %+v", first)
	}
	if first.Status.Phase != v1alpha1.NodePhaseRunning {
		t.Errorf("注册后 Phase = %q，期望 Running", first.Status.Phase)
	}
	if len(first.Spec.Addresses) != 1 || first.Spec.Addresses[0].Address != "10.0.0.1" {
		t.Errorf("IP 应映射为 InternalIP 地址: %+v", first.Spec.Addresses)
	}
}

// TestEdgeNodeAPIGet 验证单 EdgeNode 查询与 404。
func TestEdgeNodeAPIGet(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.NodeInfo{NodeID: "node-1", OS: "linux"})

	srv := newAPIServer(t, reg)

	// 存在：返回完整 EdgeNode 对象
	resp, err := http.Get(srv.URL + "/api/v1/edgenodes/node-1")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	var node v1alpha1.EdgeNode
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if node.Kind != "EdgeNode" || node.Spec.NodeID != "node-1" {
		t.Errorf("应返回完整 EdgeNode: %+v", node)
	}
	if node.Labels["edgeflow.io/os"] != "linux" {
		t.Errorf("Labels 应含 os: %+v", node.Labels)
	}
	if node.Status.Phase != v1alpha1.NodePhaseRunning {
		t.Errorf("Phase = %q，期望 Running", node.Status.Phase)
	}

	// 不存在：404 + JSON 错误信息
	resp, err = http.Get(srv.URL + "/api/v1/edgenodes/nope")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在节点状态码 = %d，期望 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析 404 响应失败: %v", err)
	}
	if body["error"] == "" || body["nodeID"] != "nope" {
		t.Errorf("404 响应应含错误信息与 nodeID: %v", body)
	}
}

// TestEdgeNodeAPIEmptyList 验证空注册表时 items 为 [] 而非 null。
func TestEdgeNodeAPIEmptyList(t *testing.T) {
	srv := newAPIServer(t, registry.New())
	resp, err := http.Get(srv.URL + "/api/v1/edgenodes")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if string(raw["items"]) != "[]" {
		t.Errorf("空列表 items 应为 []，实际 %s", raw["items"])
	}
}
