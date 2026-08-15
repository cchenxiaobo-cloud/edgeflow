// Pod 状态上报循环（WBS 6.3）测试：消息构造（纯函数）与循环生命周期。
// 端到端链路（真实 WebSocket 通道 + 云端接收/落库）依赖 e2e / mock-cloudhub。
package main

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"edgeflow/edge/pkg/edged"
	"edgeflow/edge/pkg/edgehub"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/protocol"
)

// TestBuildStatusMessages 验证上报循环的消息构造（纯函数部分）：
// 每条负载 → 一条 PodStatus 消息；信封字段与契约一致
// （Source=nodeID、Target="cloud"、Type=PodStatus、version/id 齐全）；
// Payload 可无损解析回契约结构。
func TestBuildStatusMessages(t *testing.T) {
	now := time.Now().UnixMilli()
	payloads := []edged.PodStatusPayload{
		{NodeID: "edge-001", PodName: "web-demo", Namespace: "default", Phase: "Running", Message: "", LastReconcileAt: now},
		{NodeID: "edge-001", PodName: "broken", Namespace: "default", Phase: "Error", Message: "docker daemon 不可用", LastReconcileAt: now},
	}
	msgs := buildStatusMessages("edge-001", payloads)
	if len(msgs) != len(payloads) {
		t.Fatalf("消息数 = %d，期望 %d", len(msgs), len(payloads))
	}
	for i, msg := range msgs {
		if msg.Type != protocol.TypePodStatus {
			t.Errorf("msg[%d].Type = %q，期望 %q", i, msg.Type, protocol.TypePodStatus)
		}
		if msg.Source != "edge-001" {
			t.Errorf("msg[%d].Source = %q，期望 edge-001", i, msg.Source)
		}
		if msg.Target != targetCloud {
			t.Errorf("msg[%d].Target = %q，期望 %q", i, msg.Target, targetCloud)
		}
		if msg.Version != protocol.Version || msg.ID == "" {
			t.Errorf("msg[%d] 信封字段不完整: version=%q id=%q", i, msg.Version, msg.ID)
		}
		var back edged.PodStatusPayload
		if err := msg.DecodePayload(&back); err != nil {
			t.Fatalf("msg[%d] 解析负载失败: %v", i, err)
		}
		if back != payloads[i] {
			t.Errorf("msg[%d] 负载往返不一致: %+v != %+v", i, back, payloads[i])
		}
	}
}

// TestRunStatusReportLoopExitsOnStop 验证上报循环生命周期：
// client 未启动（Send 必然失败）时，发送失败只记 Warn——不 panic、
// 不退出循环、不阻塞主流程；关闭 stopCh 后优雅退出。
// 完整链路（真实连接 + 云端接收）由 e2e 覆盖（本测试用 10ms 周期加速）。
func TestRunStatusReportLoopExitsOnStop(t *testing.T) {
	t.Setenv("EDGEFLOW_EDGECORE_DB_PATH", filepath.Join(t.TempDir(), "edgeflow.db"))
	store, err := metamanager.Open(filepath.Join(t.TempDir(), "edgeflow.db"))
	if err != nil {
		t.Fatalf("打开 Store 失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 未 Start 的 client：Send 返回"未连接"错误，模拟断线窗口
	client := edgehub.New(edgehub.Options{CloudAddr: "ws://127.0.0.1:1", NodeID: "edge-001"})
	edgedSvc := edged.New(store, edged.NewMockRuntime(), time.Hour)

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runStatusReportLoop(client, edgedSvc, "edge-001", func() time.Duration { return 10 * time.Millisecond }, stopCh)
	}()

	// 至少跑几轮（每轮 Send 失败 → Warn，循环应继续），然后停止
	time.Sleep(60 * time.Millisecond)
	close(stopCh)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("关闭 stopCh 后上报循环 2s 未退出")
	}
}

// TestDurationFromEnv 验证时长环境变量解析：合法值生效，非法/非正数/未设置回落默认。
func TestDurationFromEnv(t *testing.T) {
	const key = "EDGEFLOW_TEST_DUR"
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"3s", 3 * time.Second},
		{"bogus", 30 * time.Second},
		{"-5s", 30 * time.Second},
		{"0", 30 * time.Second},
		{"", 30 * time.Second},
	}
	for _, c := range cases {
		t.Setenv(key, c.val)
		if got := durationFromEnv(key, 30*time.Second); got != c.want {
			t.Errorf("durationFromEnv(%q) = %v，期望 %v", c.val, got, c.want)
		}
	}
}

// TestRunStatusReportLoopHotInterval 验证周期热重载（WBS 2.7）：
// intervalFn 返回值变化后，ticker 周期被重置——用 intervalFn 调用频率
// 断言：10ms 周期时高频调用，切到 1s 后调用频率骤降。
func TestRunStatusReportLoopHotInterval(t *testing.T) {
	t.Setenv("EDGEFLOW_EDGECORE_DB_PATH", filepath.Join(t.TempDir(), "edgeflow.db"))
	store, err := metamanager.Open(filepath.Join(t.TempDir(), "edgeflow.db"))
	if err != nil {
		t.Fatalf("打开 Store 失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	client := edgehub.New(edgehub.Options{CloudAddr: "ws://127.0.0.1:1", NodeID: "edge-001"})
	edgedSvc := edged.New(store, edged.NewMockRuntime(), time.Hour)

	var calls atomic.Int64
	var period atomic.Int64
	period.Store(int64(10 * time.Millisecond))
	intervalFn := func() time.Duration {
		calls.Add(1)
		return time.Duration(period.Load())
	}
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runStatusReportLoop(client, edgedSvc, "edge-001", intervalFn, stopCh)
	}()

	// 10ms 周期下跑 60ms：intervalFn 应被高频调用（初始 + 每 tick 一次）
	time.Sleep(60 * time.Millisecond)
	fastCalls := calls.Load()
	if fastCalls < 3 {
		t.Fatalf("10ms 周期下 intervalFn 调用次数 = %d，期望 ≥3（周期未生效？）", fastCalls)
	}

	// 热切换：周期改为 1s；随后 100ms 内不应再有高频调用
	period.Store(int64(time.Second))
	time.Sleep(100 * time.Millisecond)
	if after := calls.Load(); after-fastCalls > 2 {
		t.Errorf("周期切换后 intervalFn 仍被高频调用（+%d 次），ticker 未重置", after-fastCalls)
	}

	close(stopCh)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("关闭 stopCh 后上报循环未退出")
	}
}
