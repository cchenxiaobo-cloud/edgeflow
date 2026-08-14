package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerReturnsMetrics 验证 /metrics 返回 200、Prometheus 文本格式，
// 且包含全部 5 个指标名（G3 验收：Prometheus 采集到指标）。
func TestHandlerReturnsMetrics(t *testing.T) {
	m := New(Providers{
		Nodes:             func() int { return 2 },
		Pods:              func() int { return 3 },
		Devices:           func() int { return 4 },
		ActiveConnections: func() int { return 1 },
	})
	// 造两条请求计数
	m.IncRequest("/api/v1/nodes", 200)
	m.IncRequest("/api/v1/nodes", 200)
	m.IncRequest("/api/v1/nodes", 404)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("请求 /metrics 失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	text := string(body)

	// 全部指标名必须出现（含 # HELP / # TYPE 元信息行）
	for _, name := range []string{
		"edgeflow_cloudcore_nodes_total",
		"edgeflow_cloudcore_pods_total",
		"edgeflow_cloudcore_devices_total",
		"edgeflow_cloudcore_active_connections",
		"edgeflow_cloudcore_http_requests_total",
	} {
		if !strings.Contains(text, name) {
			t.Errorf("响应缺少指标 %s:\n%s", name, text)
		}
	}
	// HELP/TYPE 行存在
	for _, meta := range []string{"# HELP ", "# TYPE ", " gauge", " counter"} {
		if !strings.Contains(text, meta) {
			t.Errorf("响应缺少元信息行 %q:\n%s", meta, text)
		}
	}
	// gauge 值正确
	for _, want := range []string{
		"edgeflow_cloudcore_nodes_total 2",
		"edgeflow_cloudcore_pods_total 3",
		"edgeflow_cloudcore_devices_total 4",
		"edgeflow_cloudcore_active_connections 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("响应缺少取值行 %q:\n%s", want, text)
		}
	}
	// counter 分桶正确（2 次 200 + 1 次 404）
	for _, want := range []string{
		`edgeflow_cloudcore_http_requests_total{path="/api/v1/nodes",code="200"} 2`,
		`edgeflow_cloudcore_http_requests_total{path="/api/v1/nodes",code="404"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("响应缺少计数行 %q:\n%s", want, text)
		}
	}
}

// TestHandlerNilProviders 验证 Provider 为 nil 时对应指标不输出（不 panic）。
func TestHandlerNilProviders(t *testing.T) {
	m := New(Providers{}) // 全 nil
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("请求 /metrics 失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "edgeflow_cloudcore_nodes_total") {
		t.Errorf("Provider 为 nil 时不应输出 nodes_total:\n%s", body)
	}
}

// TestMiddlewareCounts 验证 Middleware 按 路由模式+状态码 计数：
// 命中的路由用 r.Pattern（低基数），未匹配的 404 回退实际路径。
func TestMiddlewareCounts(t *testing.T) {
	m := New(Providers{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(m.Middleware(mux))
	defer srv.Close()

	// 3 次 200（同一路由模式，不同 nodeID → 同桶）
	for _, id := range []string{"node-a", "node-b", "node-c"} {
		resp, err := http.Get(srv.URL + "/api/v1/nodes/" + id)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		_ = resp.Body.Close()
	}
	// 1 次 500
	resp, err := http.Get(srv.URL + "/boom")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_ = resp.Body.Close()
	// 1 次 404（未注册路由，回退实际路径计数）
	resp, err = http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_ = resp.Body.Close()

	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[requestKey]uint64{
		{path: "/api/v1/nodes/{nodeID}", code: 200}: 3,
		{path: "/boom", code: 500}:                  1,
		{path: "/nope", code: 404}:                  1,
	}
	for k, n := range want {
		if got := m.requests[k]; got != n {
			t.Errorf("计数[%+v] = %d，期望 %d", k, got, n)
		}
	}
}

// TestStatusRecorder 验证状态码记录语义：隐式 200、显式 WriteHeader、
// 重复 WriteHeader 忽略。
func TestStatusRecorder(t *testing.T) {
	// 隐式 200
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	_, _ = rec.Write([]byte("ok"))
	if rec.statusCode() != http.StatusOK {
		t.Errorf("隐式写入状态码 = %d，期望 200", rec.statusCode())
	}
	// 显式 404
	rec = &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.WriteHeader(http.StatusNotFound)
	rec.WriteHeader(http.StatusOK) // 重复调用应被忽略
	if rec.statusCode() != http.StatusNotFound {
		t.Errorf("显式状态码 = %d，期望 404（重复 WriteHeader 应忽略）", rec.statusCode())
	}
}
