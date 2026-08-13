package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"edgeflow/pkg/config"
)

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
	if code := serve(srv); code != 1 {
		t.Fatalf("serve 退出码 = %d，期望 1", code)
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
