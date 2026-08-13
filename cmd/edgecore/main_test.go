package main

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunVersion 验证 --version 打印版本信息后以 0 退出。
func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, &stdout, &stderr, make(chan os.Signal, 1))
	if code != 0 {
		t.Fatalf("run(--version) 退出码 = %d，期望 0", code)
	}
	if !strings.Contains(stdout.String(), "version=") {
		t.Errorf("--version 输出缺少版本信息: %q", stdout.String())
	}
}

// TestRunStartsEdgeHubAndExitsOnSignal 验证：启动后 EdgeHub 常驻运行，
// 收到 SIGTERM 后优雅退出（退出码 0，不挂起）。
func TestRunStartsEdgeHubAndExitsOnSignal(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		done <- run([]string{}, &stdout, &stderr, sigCh)
	}()

	time.Sleep(300 * time.Millisecond) // 给 EdgeHub 启动留时间（地址不可达时它应持续重试而非退出）
	sigCh <- syscall.SIGTERM

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("SIGTERM 后退出码 = %d，期望 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("发送 SIGTERM 后 5s 未退出")
	}
}
