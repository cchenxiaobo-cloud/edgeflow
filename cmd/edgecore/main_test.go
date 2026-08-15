package main

import (
	"bytes"
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
// 数据库重定向到临时目录，避免测试污染仓库内 data/。
func TestRunStartsEdgeHubAndExitsOnSignal(t *testing.T) {
	t.Setenv("EDGEFLOW_EDGECORE_DB_PATH", filepath.Join(t.TempDir(), "edgeflow.db"))
	// 测试环境没有 broker：缩短 EventBus 建连等待，让 run() 快速进入降级路径
	// （否则 Connect 默认等待 5s，超出本用例的退出超时窗口）。
	t.Setenv("EDGEFLOW_EDGECORE_MQTT_CONNECT_TIMEOUT", "1s")
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

// TestApplyEdgeCoreReload 验证 edgecore 热重载策略（WBS 2.7）：
// 上报周期热生效（透传）；cloudAddr/nodeID/reconcileInterval 需重启
// （回写旧值并保持快照诚实）。
func TestApplyEdgeCoreReload(t *testing.T) {
	old := &config.EdgeCoreConfig{
		CloudAddr:            "ws://127.0.0.1:10000",
		NodeID:               "edge-old",
		PodReportInterval:    30 * time.Second,
		DeviceReportInterval: 30 * time.Second,
		ReconcileInterval:    5 * time.Second,
	}

	t.Run("上报周期变更热生效", func(t *testing.T) {
		next := *old // 拷贝
		next.PodReportInterval = 10 * time.Second
		next.DeviceReportInterval = 20 * time.Second
		if err := applyEdgeCoreReload(old, &next); err != nil {
			t.Fatalf("上报周期变更不应报错: %v", err)
		}
		if next.PodReportInterval != 10*time.Second || next.DeviceReportInterval != 20*time.Second {
			t.Errorf("上报周期应透传新值: %v/%v", next.PodReportInterval, next.DeviceReportInterval)
		}
	})

	t.Run("cloudAddr 变更需重启：回写旧值不报错", func(t *testing.T) {
		next := *old
		next.CloudAddr = "ws://10.0.0.1:10000"
		if err := applyEdgeCoreReload(old, &next); err != nil {
			t.Fatalf("cloudAddr 变更不应报错: %v", err)
		}
		if next.CloudAddr != old.CloudAddr {
			t.Errorf("cloudAddr 应回写旧值 %s，实际 %s", old.CloudAddr, next.CloudAddr)
		}
	})

	t.Run("nodeID 变更需重启：回写旧值", func(t *testing.T) {
		next := *old
		next.NodeID = "edge-new"
		if err := applyEdgeCoreReload(old, &next); err != nil {
			t.Fatalf("nodeID 变更不应报错: %v", err)
		}
		if next.NodeID != old.NodeID {
			t.Errorf("nodeID 应回写旧值 %s，实际 %s", old.NodeID, next.NodeID)
		}
	})

	t.Run("reconcileInterval 变更需重启：回写旧值", func(t *testing.T) {
		next := *old
		next.ReconcileInterval = time.Minute
		if err := applyEdgeCoreReload(old, &next); err != nil {
			t.Fatalf("reconcileInterval 变更不应报错: %v", err)
		}
		if next.ReconcileInterval != old.ReconcileInterval {
			t.Errorf("reconcileInterval 应回写旧值 %v，实际 %v", old.ReconcileInterval, next.ReconcileInterval)
		}
	})

	t.Run("配置未变化：no-op", func(t *testing.T) {
		next := *old
		if err := applyEdgeCoreReload(old, &next); err != nil {
			t.Fatalf("未变化不应报错: %v", err)
		}
		if next.CloudAddr != old.CloudAddr || next.NodeID != old.NodeID {
			t.Error("未变化不应改动任何字段")
		}
	})
}
