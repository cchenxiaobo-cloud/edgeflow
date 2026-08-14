// 设备链路装配（WBS 3.5/5.3 边缘侧）测试：DeviceCommand 处理
// （骨架路径/执行器接入）与 DeviceReport 消息构造、上报循环生命周期。
package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/edge/pkg/edgehub"
	"edgeflow/pkg/protocol"
)

// TestBuildDeviceReportMessages 验证上报消息构造（纯函数部分）：
// 每条影子 → 一条 DeviceReport 消息；信封字段与契约一致
// （Source=nodeID、Target="cloud"、Type=DeviceReport、version/id 齐全）；
// Payload 可无损解析回契约结构；按影子排序输出。
func TestBuildDeviceReportMessages(t *testing.T) {
	twins := devicetwin.NewStore()
	twins.SetDesired("sensor-01", "default", "targetTemp", 25)
	twins.UpsertReported("sensor-01", "default", map[string]float64{"temperature": 25.5, "humidity": 60}, 1000)
	twins.SetDesired("actuator-01", "factory-a", "power", 1)

	const reportedAt = 1755168000000
	msgs := buildDeviceReportMessages("edge-001", twins, reportedAt)
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d，期望 2", len(msgs))
	}
	for i, msg := range msgs {
		if msg.Type != protocol.TypeDeviceReport {
			t.Errorf("msg[%d].Type = %q，期望 %q", i, msg.Type, protocol.TypeDeviceReport)
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
		var back devicetwin.DeviceReportPayload
		if err := msg.DecodePayload(&back); err != nil {
			t.Fatalf("msg[%d] 解析负载失败: %v", i, err)
		}
		if back.ReportedAt != reportedAt {
			t.Errorf("msg[%d] reportedAt = %d，期望 %d", i, back.ReportedAt, reportedAt)
		}
	}

	// 排序（namespace → deviceName）与内容
	if msgs[0].Target != targetCloud {
		t.Fatal("契约信封不符")
	}
	var first devicetwin.DeviceReportPayload
	var second devicetwin.DeviceReportPayload
	if err := msgs[0].DecodePayload(&first); err != nil {
		t.Fatal(err)
	}
	if err := msgs[1].DecodePayload(&second); err != nil {
		t.Fatal(err)
	}
	if first.Namespace+"/"+first.DeviceName != "default/sensor-01" ||
		second.Namespace+"/"+second.DeviceName != "factory-a/actuator-01" {
		t.Errorf("消息顺序应按 namespace/deviceName 排序: %q / %q",
			first.Namespace+"/"+first.DeviceName, second.Namespace+"/"+second.DeviceName)
	}
	if first.Properties["temperature"] != 25.5 || first.Properties["humidity"] != 60 {
		t.Errorf("sensor-01 properties 不符: %v", first.Properties)
	}
}

// TestBuildDeviceReportMessagesEmpty 验证空影子存储不产生消息（非 nil 空切片）。
func TestBuildDeviceReportMessagesEmpty(t *testing.T) {
	msgs := buildDeviceReportMessages("edge-001", devicetwin.NewStore(), 1)
	if msgs == nil || len(msgs) != 0 {
		t.Errorf("空影子应产生空消息列表: %#v", msgs)
	}
}

// TestHandleDeviceCommandSkeleton 验证骨架路径（执行器未接入，nil）：
// 指令解析成功 → 返回 nil（云端收 200）→ Twin.Desired 已更新。
func TestHandleDeviceCommandSkeleton(t *testing.T) {
	twins := devicetwin.NewStore()
	msg, err := protocol.NewMessage(protocol.TypeDeviceCommand, "cloud", "edge-001",
		devicetwin.DeviceCommandPayload{DeviceName: "sensor-01", Namespace: "default", Property: "targetTemp", Value: 25})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}

	if err := handleDeviceCommand(twins, nil, msg); err != nil {
		t.Fatalf("骨架路径不应返回错误: %v", err)
	}
	twin, ok := twins.Get("sensor-01", "default")
	if !ok {
		t.Fatal("指令处理后应有影子")
	}
	if twin.Desired["targetTemp"] != 25 {
		t.Errorf("Desired.targetTemp = %v，期望 25", twin.Desired["targetTemp"])
	}
}

// TestHandleDeviceCommandNamespaceDefault 验证 namespace 缺省补 default。
func TestHandleDeviceCommandNamespaceDefault(t *testing.T) {
	twins := devicetwin.NewStore()
	msg, _ := protocol.NewMessage(protocol.TypeDeviceCommand, "cloud", "edge-001",
		map[string]any{"deviceName": "sensor-01", "property": "targetTemp", "value": 26})

	if err := handleDeviceCommand(twins, nil, msg); err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	twin, ok := twins.Get("sensor-01", "")
	if !ok || twin.Namespace != devicetwin.DefaultNamespace {
		t.Errorf("namespace 应补 default: ok=%v %+v", ok, twin)
	}
}

// fakeExecutor 是记录型执行器：可配置失败，记录调用参数。
type fakeExecutor struct {
	fail  bool
	calls []string
}

// ExecuteCommand 记录调用；fail 为 true 时返回错误。
func (f *fakeExecutor) ExecuteCommand(deviceName, namespace, property string, value float64) error {
	f.calls = append(f.calls, namespace+"/"+deviceName+"."+property)
	if f.fail {
		return errors.New("设备执行失败（模拟）")
	}
	return nil
}

// TestHandleDeviceCommandWithExecutor 验证执行器接入路径：
// 指令路由到执行器（按 deviceName 定位），成功时返回 nil 且 Desired 更新。
func TestHandleDeviceCommandWithExecutor(t *testing.T) {
	twins := devicetwin.NewStore()
	exec := &fakeExecutor{}
	msg, _ := protocol.NewMessage(protocol.TypeDeviceCommand, "cloud", "edge-001",
		devicetwin.DeviceCommandPayload{DeviceName: "sensor-01", Namespace: "factory-a", Property: "targetTemp", Value: 30})

	if err := handleDeviceCommand(twins, exec, msg); err != nil {
		t.Fatalf("执行成功不应返回错误: %v", err)
	}
	if len(exec.calls) != 1 || exec.calls[0] != "factory-a/sensor-01.targetTemp" {
		t.Errorf("执行器未按 deviceName 正确路由: %v", exec.calls)
	}
	twin, _ := twins.Get("sensor-01", "factory-a")
	if twin.Desired["targetTemp"] != 30 {
		t.Errorf("Desired.targetTemp = %v，期望 30", twin.Desired["targetTemp"])
	}
}

// TestHandleDeviceCommandExecutorError 验证执行器失败路径：
// 返回 error（云端收 502），但期望值仍写入影子（指令语义是声明期望态）。
func TestHandleDeviceCommandExecutorError(t *testing.T) {
	twins := devicetwin.NewStore()
	exec := &fakeExecutor{fail: true}
	msg, _ := protocol.NewMessage(protocol.TypeDeviceCommand, "cloud", "edge-001",
		devicetwin.DeviceCommandPayload{DeviceName: "sensor-01", Property: "targetTemp", Value: 30})

	err := handleDeviceCommand(twins, exec, msg)
	if err == nil {
		t.Fatal("执行失败应返回错误")
	}
	if !strings.Contains(err.Error(), "执行设备指令失败") {
		t.Errorf("错误文案不符: %v", err)
	}
	twin, ok := twins.Get("sensor-01", "default")
	if !ok || twin.Desired["targetTemp"] != 30 {
		t.Errorf("执行失败后期望值仍应写入: ok=%v %+v", ok, twin)
	}
}

// TestHandleDeviceCommandErrors 验证错误路径：
// 坏 JSON / 缺 deviceName / 缺 property / nil 消息 → 返回 error（不发 200）。
func TestHandleDeviceCommandErrors(t *testing.T) {
	twins := devicetwin.NewStore()

	// nil 消息
	if err := handleDeviceCommand(twins, nil, nil); err == nil {
		t.Error("nil 消息应返回错误")
	}
	// 坏 JSON
	badMsg := &protocol.Message{ID: "m1", Type: protocol.TypeDeviceCommand, Version: protocol.Version,
		Source: "cloud", Target: "edge-001", Timestamp: 1, Payload: []byte(`{"deviceName":`)}
	if err := handleDeviceCommand(twins, nil, badMsg); err == nil {
		t.Error("坏 JSON 应返回错误")
	}
	// 缺 deviceName
	noName, _ := protocol.NewMessage(protocol.TypeDeviceCommand, "cloud", "edge-001",
		map[string]any{"property": "targetTemp", "value": 25})
	if err := handleDeviceCommand(twins, nil, noName); err == nil {
		t.Error("缺 deviceName 应返回错误")
	}
	// 缺 property
	noProp, _ := protocol.NewMessage(protocol.TypeDeviceCommand, "cloud", "edge-001",
		map[string]any{"deviceName": "sensor-01", "value": 25})
	if err := handleDeviceCommand(twins, nil, noProp); err == nil {
		t.Error("缺 property 应返回错误")
	}
	// 错误路径不应写入影子
	if len(twins.SnapshotAll()) != 0 {
		t.Errorf("错误路径不应写入影子: %+v", twins.SnapshotAll())
	}
}

// TestRunDeviceReportLoopExitsOnStop 验证上报循环生命周期：
// client 未启动（Send 必然失败）时，发送失败只记 Warn——不 panic、
// 不退出循环、不阻塞主流程；关闭 stopCh 后优雅退出（10ms 周期加速）。
func TestRunDeviceReportLoopExitsOnStop(t *testing.T) {
	client := edgehub.New(edgehub.Options{CloudAddr: "ws://127.0.0.1:1", NodeID: "edge-dev-1"})
	twins := devicetwin.NewStore()
	twins.SetDesired("sensor-01", "default", "targetTemp", 25)
	twins.UpsertReported("sensor-01", "default", map[string]float64{"temperature": 25.5}, 1)

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDeviceReportLoop(client, nil, twins, "edge-dev-1", 10*time.Millisecond, stopCh)
	}()

	time.Sleep(50 * time.Millisecond) // 跑几轮（Send 失败只 Warn，循环继续）
	close(stopCh)
	select {
	case <-done:
		// 优雅退出
	case <-time.After(3 * time.Second):
		t.Fatal("上报循环未在 stopCh 关闭后退出")
	}
}
