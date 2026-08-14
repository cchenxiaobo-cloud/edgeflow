package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"edgeflow/cloud/pkg/podstatus"
	"edgeflow/cloud/pkg/registry"
)

// newPodAPIServer 构造带 Pod 状态查询路由的测试 HTTP 服务
// （/api/v1/pods、/api/v1/nodes/{nodeID}/pods），返回服务与共享存储。
func newPodAPIServer(t *testing.T, reg *registry.Registry, pods *podstatus.PodStatusStore) *httptest.Server {
	t.Helper()
	api := &nodeAPI{reg: reg, pods: pods}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pods", api.listPods)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}/pods", api.listNodePods)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// decodePodStatusList 解析 Pod 状态列表响应。
func decodePodStatusList(t *testing.T, resp *http.Response) podStatusList {
	t.Helper()
	var list podStatusList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	return list
}

// TestPodStatusAPIList 验证 GET /api/v1/pods：K8s List 风格包装、
// 按 nodeID/namespace/podName 排序、内容与契约字段一致。
func TestPodStatusAPIList(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.NodeInfo{NodeID: "edge-002"})
	reg.Register(registry.NodeInfo{NodeID: "edge-001"})
	pods := podstatus.NewStore()
	pods.Upsert("edge-002", podstatus.PodStatus{PodName: "b", Namespace: "default", Phase: podstatus.PhaseRunning, LastReconcileAt: 1755168000000})
	pods.Upsert("edge-001", podstatus.PodStatus{PodName: "a", Namespace: "kube-system", Phase: podstatus.PhaseStopped, LastReconcileAt: 1755168000001})
	pods.Upsert("edge-001", podstatus.PodStatus{PodName: "a", Namespace: "default", Phase: podstatus.PhaseRunning, LastReconcileAt: 1755168000002})

	srv := newPodAPIServer(t, reg, pods)
	resp, err := http.Get(srv.URL + "/api/v1/pods")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}

	list := decodePodStatusList(t, resp)
	if list.Kind != "PodStatusList" || list.APIVersion != "v1" {
		t.Errorf("kind/apiVersion 不符: %+v", list)
	}
	if len(list.Items) != 3 {
		t.Fatalf("条目数 = %d，期望 3", len(list.Items))
	}
	// 排序：nodeID → namespace → podName
	expect := []string{
		"edge-001/default/a",
		"edge-001/kube-system/a",
		"edge-002/default/b",
	}
	for i, e := range expect {
		it := list.Items[i]
		got := it.NodeID + "/" + it.Namespace + "/" + it.PodName
		if got != e {
			t.Errorf("items[%d] = %q，期望 %q", i, got, e)
		}
	}
	// 契约字段完整透出
	it := list.Items[0]
	if it.Phase != podstatus.PhaseRunning || it.LastReconcileAt != 1755168000002 {
		t.Errorf("JSON 字段未按契约透出: %+v", it)
	}
}

// TestPodStatusAPIEmptyArray 验证无数据时 items 为 [] 而非 null。
func TestPodStatusAPIEmptyArray(t *testing.T) {
	pods := podstatus.NewStore()
	srv := newPodAPIServer(t, registry.New(), pods)

	resp, err := http.Get(srv.URL + "/api/v1/pods")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	// 原始字节级断言：items 必须是 [] 而不是 null
	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	raw := strings.TrimSpace(string(rawBytes))
	if !strings.Contains(raw, `"items":[]`) {
		t.Errorf("空数据 items 应为 []，实际响应: %s", raw)
	}
	if strings.Contains(raw, "null") {
		t.Errorf("空数据响应不应含 null: %s", raw)
	}

	// 解码路径验证：items 应为非 nil 空切片
	var list podStatusList
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if list.Items == nil {
		t.Error("items 应为非 nil 空切片")
	}
}

// TestPodStatusAPINodePods 验证 GET /api/v1/nodes/{nodeID}/pods 三态语义：
//   - 节点存在且有 Pod → 200 + 条目
//   - 节点存在但无 Pod → 200 + 空数组（不是 404）
//   - 节点不存在（从未注册）→ 404
func TestPodStatusAPINodePods(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.NodeInfo{NodeID: "edge-001"})
	reg.Register(registry.NodeInfo{NodeID: "edge-002"})
	pods := podstatus.NewStore()
	pods.Upsert("edge-001", podstatus.PodStatus{PodName: "web-demo", Namespace: "default", Phase: podstatus.PhaseRunning})

	srv := newPodAPIServer(t, reg, pods)

	// 有 Pod 的节点：200 + 条目
	resp, err := http.Get(srv.URL + "/api/v1/nodes/edge-001/pods")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	func() {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("有 Pod 节点状态码 = %d，期望 200", resp.StatusCode)
		}
		list := decodePodStatusList(t, resp)
		if len(list.Items) != 1 || list.Items[0].PodName != "web-demo" {
			t.Errorf("条目不符: %+v", list.Items)
		}
	}()

	// 无 Pod 的已注册节点：200 + 空数组
	resp, err = http.Get(srv.URL + "/api/v1/nodes/edge-002/pods")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	func() {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("无 Pod 节点状态码 = %d，期望 200（空数组而非 404）", resp.StatusCode)
		}
		rawBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读取响应失败: %v", err)
		}
		if !strings.Contains(string(rawBytes), `"items":[]`) {
			t.Errorf("无 Pod 节点 items 应为 []，实际: %s", string(rawBytes))
		}
	}()

	// 未知节点：404
	resp, err = http.Get(srv.URL + "/api/v1/nodes/nope/pods")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知节点状态码 = %d，期望 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析 404 响应失败: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("404 响应应含错误信息: %v", body)
	}
}

// TestPodStatusAPINilStore 验证存储未注入时接口仍可用（返回空数组，不 panic），
// 保证旧装配（nodeAPI 只注入 reg）的测试与代码路径不受影响。
func TestPodStatusAPINilStore(t *testing.T) {
	srv := newPodAPIServer(t, registry.New(), nil)

	resp, err := http.Get(srv.URL + "/api/v1/pods")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	list := decodePodStatusList(t, resp)
	if list.Items == nil || len(list.Items) != 0 {
		t.Errorf("items 应为空数组，实际 %#v", list.Items)
	}
}
