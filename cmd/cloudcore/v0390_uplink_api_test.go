// v0.39.0 测试锚（云端）：上行补传可视化 API 端点（spec 0012 US-7）。
// 依赖既有 doJSON 辅助（model_api_test.go）。
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"edgeflow/cloud/pkg/cloudhub"
)

// v0390newUplinkEnv 构造上行 API 测试环境（hub 为真实空 Server，state 可注入上报）。
func v0390newUplinkEnv(t *testing.T) (*httptest.Server, *uplinkState) {
	t.Helper()
	st := newUplinkState()
	mux := http.NewServeMux()
	(&uplinkAPI{hub: &cloudhub.Server{}, state: st}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

// TestV0390UplinkOverviewEmpty 验证空数据概览（空数组，不是 404）。
func TestV0390UplinkOverviewEmpty(t *testing.T) {
	srv, _ := v0390newUplinkEnv(t)
	code, body := doJSON(t, "GET", srv.URL+"/api/v1/uplink/overview", nil)
	if code != http.StatusOK {
		t.Fatalf("空概览应 200: %d %#v", code, body)
	}
	nodes, ok := body["nodes"].([]any)
	if !ok || len(nodes) != 0 {
		t.Fatalf("空概览 nodes 应为空数组: %#v", body)
	}
}

// TestV0390UplinkStateMergeAndSort 验证上报缓存合并与排序、单节点查询字段。
func TestV0390UplinkStateMergeAndSort(t *testing.T) {
	srv, st := v0390newUplinkEnv(t)
	st.update("node-b", cloudhub.UplinkReportPayload{Depth: 5, Dropped: 1, Sent: 10, OldestTs: 111}, 1730000001000)
	st.update("node-a", cloudhub.UplinkReportPayload{Depth: 0, Sent: 20}, 1730000002000)
	code, body := doJSON(t, "GET", srv.URL+"/api/v1/uplink/overview", nil)
	if code != http.StatusOK {
		t.Fatalf("overview 应 200: %d", code)
	}
	nodes := body["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("应 2 个节点: %#v", nodes)
	}
	if first := nodes[0].(map[string]any); first["nodeId"] != "node-a" {
		t.Fatalf("应按 nodeId 排序，首条 = %v", first["nodeId"])
	}
	code, single := doJSON(t, "GET", srv.URL+"/api/v1/nodes/node-b/uplink", nil)
	if code != http.StatusOK || single["depth"].(float64) != 5 || single["sent"].(float64) != 10 {
		t.Fatalf("单节点查询不符: %d %#v", code, single)
	}
	if single["oldestTs"].(float64) != 111 || single["lastReportTs"].(float64) != 1730000001000 {
		t.Fatalf("时间字段不符: %#v", single)
	}
}

// TestV0390UplinkNodeNotFound 验证未知节点 404 + JSON error 体。
func TestV0390UplinkNodeNotFound(t *testing.T) {
	srv, _ := v0390newUplinkEnv(t)
	code, body := doJSON(t, "GET", srv.URL+"/api/v1/nodes/ghost/uplink", nil)
	if code != http.StatusNotFound {
		t.Fatalf("未知节点应 404: %d %#v", code, body)
	}
	if body["error"] == nil {
		t.Fatalf("404 应为 JSON error 体: %#v", body)
	}
}

// TestV0390UplinkStateOverwrite 验证覆盖式更新（最近一次生效）。
func TestV0390UplinkStateOverwrite(t *testing.T) {
	srv, st := v0390newUplinkEnv(t)
	st.update("n1", cloudhub.UplinkReportPayload{Depth: 9}, 1000)
	st.update("n1", cloudhub.UplinkReportPayload{Depth: 2}, 2000)
	code, single := doJSON(t, "GET", srv.URL+"/api/v1/nodes/n1/uplink", nil)
	if code != http.StatusOK || single["depth"].(float64) != 2 || single["lastReportTs"].(float64) != 2000 {
		t.Fatalf("覆盖式更新不符: %d %#v", code, single)
	}
}
