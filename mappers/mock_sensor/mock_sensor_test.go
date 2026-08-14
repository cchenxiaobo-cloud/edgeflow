package mocksensor

import (
	"context"
	"testing"
	"time"

	"edgeflow/edge/pkg/mapper"
)

// newTestSensor 创建带固定种子的测试传感器（短周期，加速收敛验证）。
func newTestSensor(t *testing.T, deviceName string, interval time.Duration) *MockSensor {
	t.Helper()
	return New(deviceName, WithSeed(42), WithInterval(interval))
}

// assertInRange 断言 v 落在 [lo, hi] 内。
func assertInRange(t *testing.T, label string, v, lo, hi float64) {
	t.Helper()
	if v < lo || v > hi {
		t.Errorf("%s = %v 超出合法范围 [%v, %v]", label, v, lo, hi)
	}
}

// assertPropsValid 断言采集属性都在合法范围内且字段齐全。
func assertPropsValid(t *testing.T, props map[string]float64) {
	t.Helper()
	if len(props) != 2 {
		t.Fatalf("属性数量 = %d, want 2（temperature+humidity）: %v", len(props), props)
	}
	assertInRange(t, "temperature", props["temperature"], minTemp, maxTemp)
	assertInRange(t, "humidity", props["humidity"], minHumidity, maxHumidity)
}

// waitUntil 轮询等待 cond 成立，超时返回 false。
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestCollectValidRange 采集始终返回合法范围内的值（含波动过程中的多轮采样）。
func TestCollectValidRange(t *testing.T) {
	s := newTestSensor(t, "sensor-01", 5*time.Millisecond)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = s.Stop() }()

	// 启动后多轮采集：任何时刻都应在合法范围
	for i := 0; i < 40; i++ {
		props, err := s.Collect()
		if err != nil {
			t.Fatalf("Collect 返回错误: %v", err)
		}
		assertPropsValid(t, props)
		time.Sleep(3 * time.Millisecond)
	}
}

// TestHandleCommandTargetTempConverges 指令设置目标温度后，
// 采集值向新目标收敛（先收敛到高温端，再收敛到低温端，双向验证）。
func TestHandleCommandTargetTempConverges(t *testing.T) {
	s := newTestSensor(t, "sensor-01", 5*time.Millisecond)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = s.Stop() }()

	// 阶段一：设置 targetTemp=35（高温端），等待温度收敛到 32 以上
	report, err := s.HandleCommand(mapper.DeviceCommand{
		DeviceName: "sensor-01", Namespace: "default",
		Property: "targetTemp", Value: maxTemp,
	})
	if err != nil {
		t.Fatalf("设置 targetTemp 指令失败: %v", err)
	}
	if s.TargetTemp() != maxTemp {
		t.Errorf("TargetTemp = %v, want %v（指令应生效）", s.TargetTemp(), maxTemp)
	}
	assertPropsValid(t, report.Properties)
	if !waitUntil(t, 3*time.Second, func() bool {
		props, _ := s.Collect()
		return props["temperature"] > 32
	}) {
		t.Fatalf("设置 targetTemp=%v 后温度未收敛到 32 以上", maxTemp)
	}

	// 阶段二：设置 targetTemp=20（低温端），等待温度收敛到 23 以下
	if _, err := s.HandleCommand(mapper.DeviceCommand{
		DeviceName: "sensor-01", Property: "targetTemp", Value: minTemp,
	}); err != nil {
		t.Fatalf("下调 targetTemp 指令失败: %v", err)
	}
	if !waitUntil(t, 3*time.Second, func() bool {
		props, _ := s.Collect()
		return props["temperature"] < 23
	}) {
		t.Fatalf("设置 targetTemp=%v 后温度未收敛到 23 以下", minTemp)
	}
	// 全程湿度保持在合法范围
	props, _ := s.Collect()
	assertInRange(t, "humidity", props["humidity"], minHumidity, maxHumidity)
}

// TestHandleCommandTargetTempOutOfRange 越界的目标温度应被拒绝。
func TestHandleCommandTargetTempOutOfRange(t *testing.T) {
	s := newTestSensor(t, "sensor-01", time.Second)
	for _, v := range []float64{minTemp - 1, maxTemp + 1, 100} {
		if _, err := s.HandleCommand(mapper.DeviceCommand{
			DeviceName: "sensor-01", Property: "targetTemp", Value: v,
		}); err == nil {
			t.Errorf("targetTemp=%v 越界应报错", v)
		}
	}
	// 非法指令：未知属性 / 缺属性 / 设备名不符
	if _, err := s.HandleCommand(mapper.DeviceCommand{DeviceName: "sensor-01", Property: "bogus"}); err == nil {
		t.Errorf("未知属性应报错")
	}
	if _, err := s.HandleCommand(mapper.DeviceCommand{DeviceName: "sensor-01"}); err == nil {
		t.Errorf("缺 property 应报错")
	}
	if _, err := s.HandleCommand(mapper.DeviceCommand{DeviceName: "other-dev", Property: "reset"}); err == nil {
		t.Errorf("设备名不符应报错")
	}
}

// TestHandleCommandReset reset 指令恢复出厂状态：目标温度回默认值、
// 温度/湿度重新随机且仍在合法范围。
func TestHandleCommandReset(t *testing.T) {
	s := newTestSensor(t, "sensor-01", time.Second)
	if _, err := s.HandleCommand(mapper.DeviceCommand{
		DeviceName: "sensor-01", Property: "targetTemp", Value: 30,
	}); err != nil {
		t.Fatalf("设置 targetTemp 失败: %v", err)
	}
	if s.TargetTemp() != 30 {
		t.Fatalf("前置条件不满足：TargetTemp = %v", s.TargetTemp())
	}
	report, err := s.HandleCommand(mapper.DeviceCommand{DeviceName: "sensor-01", Property: "reset"})
	if err != nil {
		t.Fatalf("reset 指令失败: %v", err)
	}
	if s.TargetTemp() != DefaultTargetTemp {
		t.Errorf("reset 后 TargetTemp = %v, want %v", s.TargetTemp(), DefaultTargetTemp)
	}
	assertPropsValid(t, report.Properties)
}

// TestStartStopLifecycle 生命周期：Start 幂等、Stop 后状态冻结、
// Stop 幂等、停止后可以重新 Start。
func TestStartStopLifecycle(t *testing.T) {
	s := newTestSensor(t, "sensor-01", 5*time.Millisecond)
	// 未 Start 就 Stop：不应报错
	if err := s.Stop(); err != nil {
		t.Errorf("未启动时 Stop 不应报错: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// Start 幂等
	if err := s.Start(ctx); err != nil {
		t.Errorf("重复 Start 不应报错: %v", err)
	}
	// 运行一段时间让波动发生
	time.Sleep(60 * time.Millisecond)
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	// Stop 幂等
	if err := s.Stop(); err != nil {
		t.Errorf("重复 Stop 不应报错: %v", err)
	}
	// 停止后状态冻结：间隔采样应完全一致
	props1, _ := s.Collect()
	time.Sleep(80 * time.Millisecond)
	props2, _ := s.Collect()
	if props1["temperature"] != props2["temperature"] || props1["humidity"] != props2["humidity"] {
		t.Errorf("Stop 后状态应冻结：%v != %v", props1, props2)
	}
	// 停止后可以重新 Start（重启场景）
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("重启 Start 失败: %v", err)
	}
	defer func() { _ = s.Stop() }()
	// 重启后波动恢复：状态应再次发生变化
	time.Sleep(60 * time.Millisecond)
	props3, _ := s.Collect()
	if props3["temperature"] == props2["temperature"] && props3["humidity"] == props2["humidity"] {
		t.Errorf("重启后状态应继续波动：%v", props3)
	}
}

// TestCollectNotStarted 未启动也可直接采集（采集与生命周期解耦）。
func TestCollectNotStarted(t *testing.T) {
	s := newTestSensor(t, "sensor-01", time.Second)
	props, err := s.Collect()
	if err != nil {
		t.Fatalf("未启动 Collect 失败: %v", err)
	}
	assertPropsValid(t, props)
}

// TestNamespaceOption 命名空间选项生效，并体现在 DeviceReport 中。
func TestNamespaceOption(t *testing.T) {
	s := New("sensor-01", WithSeed(1), WithInterval(time.Second), WithNamespace("edge-test"))
	report, err := s.HandleCommand(mapper.DeviceCommand{DeviceName: "sensor-01", Property: "reset"})
	if err != nil {
		t.Fatalf("HandleCommand 失败: %v", err)
	}
	if report.Namespace != "edge-test" {
		t.Errorf("report.Namespace = %q, want edge-test", report.Namespace)
	}
	if report.DeviceName != "sensor-01" {
		t.Errorf("report.DeviceName = %q, want sensor-01", report.DeviceName)
	}
	if report.ReportedAt <= 0 {
		t.Errorf("report.ReportedAt 应非零")
	}
}
