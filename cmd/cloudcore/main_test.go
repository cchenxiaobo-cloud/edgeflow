package main

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/nodecontroller"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/config"
)

// TestParseSANList 覆盖 SAN 列表解析：合法组合 / 非法 IP / 未知前缀 / 空 DNS / 空条目
// （M4C P2-① 修复：非法条目从 Warn 跳过改为报错）。
func TestParseSANList(t *testing.T) {
	// 合法：IP + DNS 混合，允许逗号后带空格
	ips, dns, err := parseSANList("IP:192.168.1.10, DNS:cloudcore.svc")
	if err != nil {
		t.Fatalf("合法 SAN 不应报错: %v", err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.ParseIP("192.168.1.10")) {
		t.Errorf("IP 解析结果 = %v，期望 [192.168.1.10]", ips)
	}
	if len(dns) != 1 || dns[0] != "cloudcore.svc" {
		t.Errorf("DNS 解析结果 = %v，期望 [cloudcore.svc]", dns)
	}

	// 非法 IP（解析失败）
	if _, _, err := parseSANList("IP:999.1.1.1"); err == nil {
		t.Error("非法 IP 应报错")
	}
	// 无法识别的前缀（M4C P2 场景：拼写错误静默透传）
	if _, _, err := parseSANList("HOST:cloudcore"); err == nil {
		t.Error("未知前缀应报错")
	}
	// 空 DNS 名
	if _, _, err := parseSANList("DNS:"); err == nil {
		t.Error("空 DNS 名应报错")
	}
	// 空条目（多余逗号）
	if _, _, err := parseSANList("IP:1.2.3.4,"); err == nil {
		t.Error("空条目应报错")
	}
}

// TestRunInvalidTLSSAN 验证 TLS SAN 含非法条目时启动即报错退出（M4C P2-① fail-fast）。
func TestRunInvalidTLSSAN(t *testing.T) {
	t.Setenv(config.PortEnvVar, "")
	t.Setenv(cloudhub.EnvHubPort, "0")
	t.Setenv("EDGEFLOW_CLOUDCORE_TLS", "on")
	t.Setenv("EDGEFLOW_CLOUDCORE_CERT_DIR", t.TempDir())
	t.Setenv("EDGEFLOW_CLOUDCORE_TLS_SAN", "IP:192.168.1.10,DNS:cloudcore.svc,BAD:oops")
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("run(TLS SAN 含非法条目) 退出码 = %d，期望 1（fail-fast）", code)
	}
}

// TestRunVersion 验证 --version 打印版本信息后以 0 退出。
func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--version) 退出码 = %d，期望 0", code)
	}
	if !strings.Contains(stdout.String(), "version=") {
		t.Errorf("--version 输出缺少版本信息: %q", stdout.String())
	}
}

// TestRunInvalidPortFlag 验证非法 --port 在启动前报错退出（退出码 1）。
func TestRunInvalidPortFlag(t *testing.T) {
	t.Setenv(config.PortEnvVar, "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--port", "70000"}, &stdout, &stderr); code != 1 {
		t.Fatalf("run(--port 70000) 退出码 = %d，期望 1", code)
	}
}

// TestRunInvalidEnv 验证非法环境变量端口在启动前报错退出（退出码 1）。
func TestRunInvalidEnv(t *testing.T) {
	t.Setenv(config.PortEnvVar, "70000")
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("run(env=70000) 退出码 = %d，期望 1", code)
	}
}

// TestRunBadConfigFile 验证配置文件存在但解析失败时报错退出（退出码 1）。
func TestRunBadConfigFile(t *testing.T) {
	t.Setenv(config.PortEnvVar, "")
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"port": 9090`), 0o644); err != nil {
		t.Fatalf("写入临时配置文件失败: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("run(坏配置文件) 退出码 = %d，期望 1", code)
	}
}

// TestRunServerLifecycle 验证完整生命周期：启动服务 → /healthz 200 → SIGTERM 优雅退出。
func TestRunServerLifecycle(t *testing.T) {
	t.Setenv(config.PortEnvVar, "")
	// CloudHub 用随机端口，避免与开发机上的 10000 端口冲突
	t.Setenv(cloudhub.EnvHubPort, "0")
	// 审计台账写入临时目录，避免测试产物落在仓库内
	t.Setenv("EDGEFLOW_CLOUDCORE_AUDIT_PATH", filepath.Join(t.TempDir(), "audit-ledger.jsonl"))
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"--port", "18080", "--config", "not-exist.json"}, &stdout, &stderr)
	}()

	// 轮询 /healthz，确认服务就绪
	healthy := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:18080/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("healthz 未在超时时间内返回 200")
	}

	// 发送 SIGTERM，验证优雅退出
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("发送 SIGTERM 失败: %v", err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run 退出码 = %d，期望 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run 未在超时时间内退出")
	}
}

// TestServeListenError 验证监听失败（非法地址）时 serve 返回 1。
func TestServeListenError(t *testing.T) {
	srv := &http.Server{Addr: ":99999", Handler: http.NewServeMux()}
	hub := cloudhub.New("127.0.0.1:0")
	// NodeController 用空注册表即可：本测试只验证监听失败路径
	nc := nodecontroller.New(registry.New())
	if code := serve(srv, hub, nc); code != 1 {
		t.Fatalf("serve 退出码 = %d，期望 1", code)
	}
}

// TestServeHubPortInvalid 验证 CloudHub 端口环境变量非法时 run 返回 1。
func TestServeHubPortInvalid(t *testing.T) {
	t.Setenv(config.PortEnvVar, "")
	t.Setenv(cloudhub.EnvHubPort, "not-a-port")
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("run(hub 端口非法) 退出码 = %d，期望 1", code)
	}
}

// TestParseFlags 验证参数解析：--port 显式检测、--config 覆盖、未知参数报错。
func TestParseFlags(t *testing.T) {
	var buf bytes.Buffer

	// 显式指定 --port 与 --config
	opts, err := parseFlags([]string{"--port", "7070", "--config", "/tmp/x.json"}, &buf)
	if err != nil {
		t.Fatalf("parseFlags 不应报错: %v", err)
	}
	if !opts.portSet || opts.port != 7070 {
		t.Errorf("port = %d, portSet = %v，期望 7070/true", opts.port, opts.portSet)
	}
	if opts.config != "/tmp/x.json" {
		t.Errorf("config = %q，期望 /tmp/x.json", opts.config)
	}

	// 未指定 --port 时 portSet 应为 false
	opts, err = parseFlags(nil, &buf)
	if err != nil {
		t.Fatalf("parseFlags(nil) 不应报错: %v", err)
	}
	if opts.portSet {
		t.Error("未指定 --port 时 portSet 应为 false")
	}

	// 未知参数应报错
	if _, err := parseFlags([]string{"--nope"}, &buf); err == nil {
		t.Error("未知参数应报错")
	}
}
