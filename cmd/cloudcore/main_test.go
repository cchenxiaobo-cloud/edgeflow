package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

// TestRunPortInUse 验证启动时 HTTP 监听端口已被占用 → 退出码 1
// （监听失败 fail-fast，替代原 TestServeListenError 的监听错误路径）。
func TestRunPortInUse(t *testing.T) {
	t.Setenv(config.PortEnvVar, "")
	t.Setenv(cloudhub.EnvHubPort, "0")
	// 占住一个通配地址端口（macOS 上仅占 127.0.0.1 无法阻止 :PORT 绑定）
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--port", strconv.Itoa(port), "--config", "not-exist.json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("run(端口被占用) 退出码 = %d，期望 1（监听失败 fail-fast）", code)
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

// TestServeHubStartError 验证 serve 的致命错误路径：CloudHub 启动失败
// （监听失败）→ errCh 上报 → 尽力关闭全部服务 → 返回 1。
func TestServeHubStartError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{Handler: http.NewServeMux()}
	// 非法监听地址：cloudhub.Start 必然失败，命中 errCh 致命路径
	hub := cloudhub.New("bad-address")
	nc := nodecontroller.New(registry.New())
	if code := serve(srv, ln, hub, nc); code != 1 {
		t.Fatalf("serve(hub 启动失败) 退出码 = %d，期望 1", code)
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

// ---- WBS 2.7：配置热重载（SIGHUP + 端口热切换） ----

// waitHealthy 轮询指定端口 /healthz，直到返回 200 或超时。
func waitHealthy(t *testing.T, port int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestRunConfigHotReload 验证端到端热重载：修改配置文件 + SIGHUP →
// HTTP 监听端口热切换（新端口 /healthz 200，旧端口停止服务）。
func TestRunConfigHotReload(t *testing.T) {
	t.Setenv(config.PortEnvVar, "")
	t.Setenv(config.HubPortEnvVar, "0") // CloudHub 随机端口，避免冲突
	t.Setenv("EDGEFLOW_CLOUDCORE_AUDIT_PATH", filepath.Join(t.TempDir(), "audit-ledger.jsonl"))
	cfgPath := filepath.Join(t.TempDir(), "cloudcore.json")
	if err := os.WriteFile(cfgPath, []byte(`{"port": 18181}`), 0o644); err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"--config", cfgPath}, &stdout, &stderr)
	}()

	if !waitHealthy(t, 18181, 5*time.Second) {
		t.Fatal("初始端口 18181 未在超时时间内就绪")
	}

	// 修改配置文件（端口 18181 → 18182）并发送 SIGHUP
	if err := os.WriteFile(cfgPath, []byte(`{"port": 18182}`), 0o644); err != nil {
		t.Fatalf("改写配置文件失败: %v", err)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("发送 SIGHUP 失败: %v", err)
	}

	if !waitHealthy(t, 18182, 5*time.Second) {
		t.Fatal("SIGHUP 后新端口 18182 未就绪（热切换未生效）")
	}
	// 旧端口应已停止接受连接
	if resp, err := http.Get("http://127.0.0.1:18181/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Errorf("旧端口 18181 仍可访问，期望热切换后已关闭（status=%d）", resp.StatusCode)
	}

	// 优雅退出
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

// TestRunConfigReloadInvalidKeepsRunning 验证重载失败 fail-safe：
// 配置文件写坏 + SIGHUP → 进程继续运行（旧端口仍服务），退出码不受影响。
func TestRunConfigReloadInvalidKeepsRunning(t *testing.T) {
	t.Setenv(config.PortEnvVar, "")
	t.Setenv(config.HubPortEnvVar, "0")
	t.Setenv("EDGEFLOW_CLOUDCORE_AUDIT_PATH", filepath.Join(t.TempDir(), "audit-ledger.jsonl"))
	cfgPath := filepath.Join(t.TempDir(), "cloudcore.json")
	if err := os.WriteFile(cfgPath, []byte(`{"port": 18183}`), 0o644); err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"--config", cfgPath}, &stdout, &stderr)
	}()

	if !waitHealthy(t, 18183, 5*time.Second) {
		t.Fatal("初始端口 18183 未就绪")
	}
	// 写坏配置 + SIGHUP：重载失败，旧配置继续运行
	if err := os.WriteFile(cfgPath, []byte(`{"port": 99999`), 0o644); err != nil {
		t.Fatalf("写入坏配置失败: %v", err)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("发送 SIGHUP 失败: %v", err)
	}
	if !waitHealthy(t, 18183, 3*time.Second) {
		t.Fatal("重载失败后旧端口 18183 应继续服务（fail-safe）")
	}

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

// TestApplyConfigReload 验证热重载策略（纯单元）：
// hubPort 变更需重启（保持旧值）、port 热切换、绑定失败拒绝重载。
func TestApplyConfigReload(t *testing.T) {
	t.Setenv(config.PortEnvVar, "")
	t.Setenv(config.HubPortEnvVar, "")

	// 起一个真实的 HTTP 服务供 httpReloader 使用
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	oldPort := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: http.NewServeMux()}
	go func() { _ = srv.Serve(ln) }()
	hr := &httpReloader{srv: srv, ln: ln}
	old := &config.Config{Port: oldPort, PortSource: config.SourceFile, HubPort: 10000, HubPortSource: config.SourceDefault, Compress: true}

	t.Run("hubPort 变更需重启：保持旧值不报错", func(t *testing.T) {
		next := &config.Config{Port: oldPort, PortSource: config.SourceFile, HubPort: 20000, HubPortSource: config.SourceFile}
		if err := applyConfigReload(old, next, hr); err != nil {
			t.Fatalf("hubPort 变更不应报错: %v", err)
		}
		if next.HubPort != 10000 || next.HubPortSource != config.SourceDefault {
			t.Errorf("hubPort 应回写旧值 10000（来源默认值），实际 %d/%q", next.HubPort, next.HubPortSource)
		}
		if hr.ln.Addr().(*net.TCPAddr).Port != oldPort {
			t.Error("hubPort 变更不应触发监听切换")
		}
	})

	t.Run("port 变更热切换监听", func(t *testing.T) {
		// 找一个空闲端口作为目标
		freeLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("探测空闲端口失败: %v", err)
		}
		newPort := freeLn.Addr().(*net.TCPAddr).Port
		_ = freeLn.Close()

		next := &config.Config{Port: newPort, PortSource: config.SourceFile, HubPort: 10000, HubPortSource: config.SourceDefault}
		if err := applyConfigReload(old, next, hr); err != nil {
			t.Fatalf("端口热切换不应报错: %v", err)
		}
		if got := hr.ln.Addr().(*net.TCPAddr).Port; got != newPort {
			t.Errorf("切换后监听端口 = %d，期望 %d", got, newPort)
		}
	})

	t.Run("端口被占用：拒绝重载保持旧监听", func(t *testing.T) {
		busyLn, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatalf("占位监听失败: %v", err)
		}
		defer func() { _ = busyLn.Close() }()
		busyPort := busyLn.Addr().(*net.TCPAddr).Port
		before := hr.ln.Addr().(*net.TCPAddr).Port

		next := &config.Config{Port: busyPort, PortSource: config.SourceFile, HubPort: 10000, HubPortSource: config.SourceDefault}
		if err := applyConfigReload(old, next, hr); err == nil {
			t.Fatal("目标端口被占用时应拒绝重载")
		}
		if got := hr.ln.Addr().(*net.TCPAddr).Port; got != before {
			t.Errorf("拒绝重载后监听端口 = %d，应保持 %d", got, before)
		}
	})

	t.Run("compress 变更需重启：回写旧值", func(t *testing.T) {
		next := &config.Config{Port: hr.ln.Addr().(*net.TCPAddr).Port, PortSource: config.SourceFile, HubPort: 10000, HubPortSource: config.SourceDefault, Compress: false}
		if err := applyConfigReload(old, next, hr); err != nil {
			t.Fatalf("compress 变更不应报错: %v", err)
		}
		if next.Compress != true {
			t.Error("compress 应回写旧值 true（压缩协商在连接注册时确定，需重启）")
		}
	})

	t.Run("配置未变化：no-op", func(t *testing.T) {
		cur := hr.ln.Addr().(*net.TCPAddr).Port
		next := &config.Config{Port: cur, PortSource: config.SourceFile, HubPort: 10000, HubPortSource: config.SourceDefault}
		if err := applyConfigReload(old, next, hr); err != nil {
			t.Fatalf("未变化不应报错: %v", err)
		}
		if hr.ln.Addr().(*net.TCPAddr).Port != cur {
			t.Error("未变化不应触发监听切换")
		}
	})
}

// TestNewHTTPServerTimeouts 验证 HTTP 服务超时配置（M1B P2-5）：
// WriteTimeout 必须显式设置，防止慢客户端（读响应缓慢/停滞）无限
// 占用写路径连接。
func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(":8080", nil)
	if srv.WriteTimeout != 15*time.Second {
		t.Errorf("WriteTimeout = %v，期望 15s（M1B P2-5）", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v，期望 5s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 10*time.Second {
		t.Errorf("ReadTimeout = %v，期望 10s", srv.ReadTimeout)
	}
	if srv.Addr != ":8080" {
		t.Errorf("Addr = %q，期望 :8080", srv.Addr)
	}
	if srv.Handler != nil {
		t.Errorf("Handler = %v，期望 nil（调用方通过参数传入）", srv.Handler)
	}
}

// failWriter 是 Write 恒失败的 ResponseWriter（模拟客户端断开场景）。
type failWriter struct {
	header http.Header
}

func (f *failWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}

func (f *failWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func (f *failWriter) WriteHeader(int) {}

// TestAPIEncodeErrorNoPanic 验证 M1B P2-6 修复：响应编码失败
// （典型场景：客户端已断开）时 handler 只记 Warn 日志、不 panic、
// 不改变正常请求流程（对比修复前的静默忽略，行为对调用方不变）。
func TestAPIEncodeErrorNoPanic(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.NodeInfo{NodeID: "node-a"})
	api := &nodeAPI{reg: reg}

	// 覆盖各编码出口：列表 / 单节点 / 404 / EdgeNode 视图
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"listNodes":     api.listNodes,
		"getNode":       api.getNode,
		"listEdgeNodes": api.listEdgeNodes,
		"getEdgeNode":   api.getEdgeNode,
	} {
		t.Run(name+"_客户端断开不panic", func(t *testing.T) {
			req := new(http.Request)
			req.SetPathValue("nodeID", "node-a")
			w := &failWriter{}
			call(w, req) // 不应 panic；修复前为 _ = 静默忽略，修复后多一条 Warn 日志
		})
	}
}
