package main

// 安全与可观测性装配的端到端测试（WBS 10.1 / 7.2 / 7.5）：
// 通过 run() 启动真实 cloudcore，验证：
//   - /metrics 端点输出 Prometheus 文本格式指标（G3）；
//   - Token 认证开关：默认 off 向后兼容、on 时 401/200 行为（G4 7.2）；
//   - 审计台账记录每次 API 调用（G4 7.5）。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/pkg/config"
)

// cloudcoreProc 是测试中运行中的 cloudcore 进程句柄。
// stopped 标记避免清理函数对已正常停止的进程重复发送 SIGTERM
// （stopCloudcore 已消费 done 通道的值，清理时的 select 会走 default 分支）。
type cloudcoreProc struct {
	done    chan int
	stopped bool
}

// stop 发送 SIGTERM 并等待优雅退出（退出码必须为 0）。
func (p *cloudcoreProc) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("发送 SIGTERM 失败: %v", err)
	}
	select {
	case code := <-p.done:
		if code != 0 {
			t.Fatalf("cloudcore 退出码 = %d，期望 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cloudcore 未在超时时间内退出")
	}
}

// startCloudcore 启动 cloudcore（run 的可测试入口），等待 /healthz 就绪。
// 返回进程句柄；t.Cleanup 兜底发送 SIGTERM，避免测试失败时进程泄漏。
func startCloudcore(t *testing.T, httpPort string) *cloudcoreProc {
	t.Helper()
	t.Setenv(config.PortEnvVar, "")
	t.Setenv(cloudhub.EnvHubPort, "0") // CloudHub 随机端口，避免冲突
	// 审计台账写入临时目录，避免测试产物落在仓库内；
	// 调用方已显式设置路径时（如认证测试需要事后读取台账）不覆盖。
	if os.Getenv("EDGEFLOW_CLOUDCORE_AUDIT_PATH") == "" {
		t.Setenv("EDGEFLOW_CLOUDCORE_AUDIT_PATH", filepath.Join(t.TempDir(), "audit-ledger.jsonl"))
	}
	proc := &cloudcoreProc{done: make(chan int, 1)}
	go func() {
		proc.done <- run([]string{"--port", httpPort, "--config", "not-exist.json"}, &bytes.Buffer{}, &bytes.Buffer{})
	}()
	// 清理兜底：测试失败也能让 run 收到退出信号
	t.Cleanup(func() {
		proc.stop(t)
	})

	// 轮询 /healthz，确认服务就绪
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + httpPort + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return proc
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("cloudcore 未在超时时间内就绪")
	return nil
}

// getBody 发起 GET 请求并返回状态码与响应体。
func getBody(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return resp.StatusCode, string(body)
}

// TestRunMetricsEndpoint 验证 /metrics 端到端（G3）：启动 cloudcore 后
// 访问 /metrics 返回 200，且包含全部 5 个指标名；访问 API 后计数自增。
func TestRunMetricsEndpoint(t *testing.T) {
	proc := startCloudcore(t, "18081")

	code, body := getBody(t, "http://127.0.0.1:18081/metrics", "")
	if code != http.StatusOK {
		t.Fatalf("/metrics 状态码 = %d，期望 200", code)
	}
	for _, name := range []string{
		"edgeflow_cloudcore_nodes_total",
		"edgeflow_cloudcore_pods_total",
		"edgeflow_cloudcore_devices_total",
		"edgeflow_cloudcore_active_connections",
		"edgeflow_cloudcore_http_requests_total",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics 缺少指标 %s", name)
		}
	}

	// 访问一次 API 后，对应分桶计数应为 1（认证默认关闭，直接可访问）
	code, _ = getBody(t, "http://127.0.0.1:18081/api/v1/nodes", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/nodes 状态码 = %d，期望 200（认证默认关闭）", code)
	}
	// 再次抓取 /metrics：计数发生在响应渲染之后，因此第二次抓取才能
	// 看到第一次 /metrics 请求与 API 请求的分桶（含 path 标签路由模式）
	code, body = getBody(t, "http://127.0.0.1:18081/metrics", "")
	if code != http.StatusOK {
		t.Fatalf("/metrics 状态码 = %d，期望 200", code)
	}
	if !strings.Contains(body, `edgeflow_cloudcore_http_requests_total{path="/metrics"`) {
		t.Errorf("/metrics 自身请求未被计数:\n%s", body)
	}
	if !strings.Contains(body, `edgeflow_cloudcore_http_requests_total{path="/api/v1/nodes",code="200"} 1`) {
		t.Errorf("API 请求计数缺失:\n%s", body)
	}
	proc.stop(t)
}

// TestRunAuthOffBackwardCompat 验证认证默认关闭（G4 7.2 向后兼容）：
// 未设置任何环境变量时，无令牌访问管理 API 返回 200。
func TestRunAuthOffBackwardCompat(t *testing.T) {
	t.Setenv("EDGEFLOW_CLOUDCORE_AUTH", "")
	t.Setenv("EDGEFLOW_CLOUDCORE_API_TOKEN", "")
	proc := startCloudcore(t, "18082")

	code, body := getBody(t, "http://127.0.0.1:18082/api/v1/nodes", "")
	if code != http.StatusOK {
		t.Fatalf("认证关闭时无令牌访问状态码 = %d，期望 200（向后兼容）: %s", code, body)
	}
	proc.stop(t)
}

// TestRunAuthEnforced 验证认证开启（G4 7.2）：无令牌 401、错误令牌 401、
// 正确令牌 200；审计台账同时记录 401 与 200（G4 7.5）。
func TestRunAuthEnforced(t *testing.T) {
	t.Setenv("EDGEFLOW_CLOUDCORE_AUTH", "on")
	t.Setenv("EDGEFLOW_CLOUDCORE_API_TOKEN", "secret-token")
	auditPath := filepath.Join(t.TempDir(), "audit-ledger.jsonl")
	t.Setenv("EDGEFLOW_CLOUDCORE_AUDIT_PATH", auditPath)
	proc := startCloudcore(t, "18083")

	base := "http://127.0.0.1:18083"

	// 无令牌 → 401（且带 WWW-Authenticate）
	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/nodes", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌状态码 = %d，期望 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 响应应携带 WWW-Authenticate 响应头")
	}

	// 错误令牌 → 401
	code, _ := getBody(t, base+"/api/v1/nodes", "wrong-token")
	if code != http.StatusUnauthorized {
		t.Fatalf("错误令牌状态码 = %d，期望 401", code)
	}

	// 正确令牌 → 200
	code, body := getBody(t, base+"/api/v1/nodes", "secret-token")
	if code != http.StatusOK {
		t.Fatalf("正确令牌状态码 = %d，期望 200: %s", code, body)
	}

	// /healthz 与 /metrics 不受认证影响（探活/采集通道不能因令牌缺失而挂）
	code, _ = getBody(t, base+"/healthz", "")
	if code != http.StatusOK {
		t.Fatalf("认证开启时 /healthz 状态码 = %d，期望 200（不参与认证）", code)
	}
	code, _ = getBody(t, base+"/metrics", "")
	if code != http.StatusOK {
		t.Fatalf("认证开启时 /metrics 状态码 = %d，期望 200（不参与认证）", code)
	}

	proc.stop(t)

	// 审计台账可查（G4 7.5）：至少 3 条 API 记录 —— 2 条 401（operator=anonymous）
	// + 1 条 200（operator=token）；字段齐全
	entries := readAuditEntries(t, auditPath)
	if len(entries) < 3 {
		t.Fatalf("审计记录数 = %d，期望 ≥ 3（2×401 + 1×200）", len(entries))
	}
	var saw401, saw200 bool
	for _, e := range entries {
		if e.Path != "/api/v1/nodes" {
			t.Errorf("审计路径 = %q，期望 /api/v1/nodes", e.Path)
		}
		if e.Method != "GET" || e.Action == "" || e.IP == "" || e.Ts <= 0 {
			t.Errorf("审计字段不完整: %+v", e)
		}
		if e.Status == http.StatusUnauthorized {
			saw401 = true
			if e.Operator != "anonymous" {
				t.Errorf("401 记录 operator = %q，期望 anonymous", e.Operator)
			}
		}
		if e.Status == http.StatusOK {
			saw200 = true
			if e.Operator != "token" {
				t.Errorf("200 记录 operator = %q，期望 token", e.Operator)
			}
		}
	}
	if !saw401 {
		t.Error("审计台账缺少 401 记录（未授权访问必须留痕）")
	}
	if !saw200 {
		t.Error("审计台账缺少 200 记录")
	}
}

// TestRunAuthOnWithoutTokenFailsFast 验证 fail-fast：认证开启但令牌未配置时拒绝启动。
func TestRunAuthOnWithoutTokenFailsFast(t *testing.T) {
	t.Setenv(config.PortEnvVar, "")
	t.Setenv(cloudhub.EnvHubPort, "0")
	t.Setenv("EDGEFLOW_CLOUDCORE_AUTH", "on")
	t.Setenv("EDGEFLOW_CLOUDCORE_API_TOKEN", "")
	t.Setenv("EDGEFLOW_CLOUDCORE_AUDIT_PATH", filepath.Join(t.TempDir(), "audit-ledger.jsonl"))
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("认证开启无令牌退出码 = %d，期望 1（fail-fast）", code)
	}
}

// auditEntry 是审计台账行的测试形态（与 cloud/pkg/audit.Entry 字段一致）。
type auditEntry struct {
	Ts       int64  `json:"ts"`
	Action   string `json:"action"`
	Path     string `json:"path"`
	Method   string `json:"method"`
	Status   int    `json:"status"`
	Operator string `json:"operator"`
	IP       string `json:"ip"`
}

// readAuditEntries 读取审计台账全部记录。
func readAuditEntries(t *testing.T, path string) []auditEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取审计台账失败: %v", err)
	}
	var entries []auditEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e auditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("解析审计行失败 %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	return entries
}
