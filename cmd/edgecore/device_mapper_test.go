// Mapper 装配（WBS 5.1/5.5）测试：指令执行器适配（Dispatch 路由 +
// 快照落影子）、采集汇入影子、内置模拟传感器注册。
package main

import (
	"context"
	"errors"
	"testing"

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/edge/pkg/eventbus"
	"edgeflow/edge/pkg/mapper"
	"edgeflow/pkg/protocol"

	mocksensor "edgeflow/mappers/mock_sensor"
)

// fakeMapper 是测试用 DeviceMapper：记录收到的指令、可配置失败、
// 可配置采集值（避免测试依赖 mappers/ 下具体实现）。
type fakeMapper struct {
	name       string
	deviceName string
	props      map[string]float64
	cmdErr     error
	collectErr error
	handled    []mapper.DeviceCommand
}

func (f *fakeMapper) Name() string { return f.name }
func (f *fakeMapper) DeviceNames() []string {
	return []string{f.deviceName}
}
func (f *fakeMapper) Start(context.Context) error { return nil }
func (f *fakeMapper) Stop() error                 { return nil }
func (f *fakeMapper) HandleCommand(cmd mapper.DeviceCommand) (mapper.DeviceReport, error) {
	f.handled = append(f.handled, cmd)
	if f.cmdErr != nil {
		return mapper.DeviceReport{}, f.cmdErr
	}
	return mapper.DeviceReport{
		DeviceName: f.deviceName,
		Namespace:  "default",
		Properties: map[string]float64{"temperature": 26.0, "humidity": 61.0},
		ReportedAt: 1234,
	}, nil
}
func (f *fakeMapper) Collect() (map[string]float64, error) {
	if f.collectErr != nil {
		return nil, f.collectErr
	}
	return f.props, nil
}

// newFakeRegistry 构造注册了 fakeMapper 的注册表（helper）。
func newFakeRegistry(t *testing.T, f *fakeMapper) *mapper.MapperRegistry {
	t.Helper()
	reg := mapper.NewRegistry()
	if err := reg.Register(f); err != nil {
		t.Fatalf("注册 fakeMapper 失败: %v", err)
	}
	return reg
}

// TestMapperExecutorDispatch 验证指令执行器适配：
// ExecuteCommand → 注册表按 deviceName 路由 → Mapper 处理 →
// 返回的快照写入影子 Reported（记录报告）。
func TestMapperExecutorDispatch(t *testing.T) {
	f := &fakeMapper{name: "fake", deviceName: "sensor-01"}
	reg := newFakeRegistry(t, f)
	twins := devicetwin.NewStore()
	exec := &mapperCommandExecutor{reg: reg, twins: twins}

	if err := exec.ExecuteCommand("sensor-01", "default", "targetTemp", 25); err != nil {
		t.Fatalf("执行指令失败: %v", err)
	}
	// 指令路由正确（deviceName/namespace/property/value 原样透传）
	if len(f.handled) != 1 {
		t.Fatalf("Mapper 收到指令数 = %d，期望 1", len(f.handled))
	}
	cmd := f.handled[0]
	if cmd.DeviceName != "sensor-01" || cmd.Namespace != "default" ||
		cmd.Property != "targetTemp" || cmd.Value != 25 {
		t.Errorf("路由到 Mapper 的指令不符: %+v", cmd)
	}
	// 快照已写入影子（记录报告）
	twin, ok := twins.Get("sensor-01", "default")
	if !ok {
		t.Fatal("影子应已创建")
	}
	if twin.Reported["temperature"] != 26.0 || twin.Reported["humidity"] != 61.0 {
		t.Errorf("快照未写入 Reported: %v", twin.Reported)
	}
	if twin.LastReportedAt != 1234 {
		t.Errorf("LastReportedAt = %d，期望 1234（Mapper 快照时间）", twin.LastReportedAt)
	}
}

// TestMapperExecutorError 验证执行器错误路径：
// Mapper 处理失败 → 返回 error（云端收 502）；不写影子、不 panic。
func TestMapperExecutorError(t *testing.T) {
	f := &fakeMapper{name: "fake", deviceName: "sensor-01", cmdErr: errors.New("设备执行失败（模拟）")}
	reg := newFakeRegistry(t, f)
	twins := devicetwin.NewStore()
	exec := &mapperCommandExecutor{reg: reg, twins: twins}

	if err := exec.ExecuteCommand("sensor-01", "default", "targetTemp", 25); err == nil {
		t.Fatal("执行失败应返回错误")
	}
	if len(twins.SnapshotAll()) != 0 {
		t.Errorf("执行失败不应写影子: %+v", twins.SnapshotAll())
	}
}

// TestMapperExecutorUnknownDevice 验证未注册设备的指令路由失败。
func TestMapperExecutorUnknownDevice(t *testing.T) {
	f := &fakeMapper{name: "fake", deviceName: "sensor-01"}
	reg := newFakeRegistry(t, f)
	exec := &mapperCommandExecutor{reg: reg, twins: devicetwin.NewStore()}

	err := exec.ExecuteCommand("ghost-device", "default", "targetTemp", 25)
	if err == nil {
		t.Fatal("未注册设备应返回错误")
	}
}

// TestMapperExecutorNilRegistry 验证注册表未装配时返回明确错误（不 panic）。
func TestMapperExecutorNilRegistry(t *testing.T) {
	exec := &mapperCommandExecutor{reg: nil, twins: devicetwin.NewStore()}
	if err := exec.ExecuteCommand("sensor-01", "default", "targetTemp", 25); err == nil {
		t.Fatal("nil 注册表应返回错误")
	}
}

// TestCollectMapperReports 验证采集汇入影子：Collect 值按设备名写入
// Twin.Reported（namespace 按 default）；采集失败只跳过该 Mapper。
func TestCollectMapperReports(t *testing.T) {
	f := &fakeMapper{name: "fake", deviceName: "sensor-01",
		props: map[string]float64{"temperature": 27.5, "humidity": 55}}
	reg := newFakeRegistry(t, f)
	twins := devicetwin.NewStore()

	collectMapperReports(reg, twins, 999)
	twin, ok := twins.Get("sensor-01", "default")
	if !ok {
		t.Fatal("采集后应有影子")
	}
	if twin.Reported["temperature"] != 27.5 || twin.Reported["humidity"] != 55 {
		t.Errorf("采集值未写入 Reported: %v", twin.Reported)
	}
	if twin.LastReportedAt != 999 {
		t.Errorf("LastReportedAt = %d，期望 999（采集时间）", twin.LastReportedAt)
	}
}

// TestCollectMapperReportsErrors 验证异常路径：采集失败的 Mapper 被跳过、
// nil 注册表静默返回，均不 panic。
func TestCollectMapperReportsErrors(t *testing.T) {
	f := &fakeMapper{name: "fake", deviceName: "sensor-01",
		collectErr: errors.New("采集失败（模拟）")}
	reg := newFakeRegistry(t, f)
	twins := devicetwin.NewStore()

	collectMapperReports(reg, twins, 1) // 失败只 Warn，不写影子
	if len(twins.SnapshotAll()) != 0 {
		t.Errorf("采集失败不应写影子: %+v", twins.SnapshotAll())
	}
	collectMapperReports(nil, twins, 1) // nil 注册表静默跳过
}

// TestHandleDeviceCommandFullChain 验证完整指令链路（装配级）：
// handleDeviceCommand + mapperCommandExecutor + fakeMapper →
// Desired 与 Reported 同时落影子（期望态 + 执行快照）。
func TestHandleDeviceCommandFullChain(t *testing.T) {
	f := &fakeMapper{name: "fake", deviceName: "sensor-01"}
	reg := newFakeRegistry(t, f)
	twins := devicetwin.NewStore()
	exec := &mapperCommandExecutor{reg: reg, twins: twins}

	msg, err := protocol.NewMessage(protocol.TypeDeviceCommand, "cloud", "edge-001",
		devicetwin.DeviceCommandPayload{DeviceName: "sensor-01", Namespace: "default", Property: "targetTemp", Value: 25})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	if err := handleDeviceCommand(twins, exec, msg); err != nil {
		t.Fatalf("完整链路不应返回错误: %v", err)
	}

	twin, _ := twins.Get("sensor-01", "default")
	if twin.Desired["targetTemp"] != 25 {
		t.Errorf("Desired.targetTemp = %v，期望 25", twin.Desired["targetTemp"])
	}
	if twin.Reported["temperature"] != 26.0 {
		t.Errorf("Reported.temperature = %v，期望 26.0（Mapper 执行快照）", twin.Reported["temperature"])
	}
}

// TestBuildMapperRegistry 验证内置装配：注册表含模拟传感器且可按
// 设备名路由（M3 端到端演示链路的默认装配）。
func TestBuildMapperRegistry(t *testing.T) {
	t.Setenv("EDGEFLOW_MODBUS_ADDR", "") // 未显式配置 Modbus：不注册（装配门控）
	reg := buildMapperRegistry(nil, nil) // 未装配 EventBus：纯本地模式
	if len(reg.List()) != 1 {
		t.Fatalf("注册 Mapper 数 = %d，期望 1（模拟传感器）", len(reg.List()))
	}
	if _, ok := reg.Route("sensor-01"); !ok {
		t.Error("模拟传感器应按设备名 sensor-01 可路由")
	}
}

// TestBuildMapperRegistryMqttMode 装配 EventBus 后：传感器进入 MQTT 模式
// （无需真实 broker——本用例只验证装配形态，连接/收发由 mocksensor 的
// 集成用例覆盖）。
func TestBuildMapperRegistryMqttMode(t *testing.T) {
	t.Setenv("EDGEFLOW_MODBUS_ADDR", "")
	bus := eventbus.New("tcp://127.0.0.1:1") // 仅验证装配，不 Connect
	reg := buildMapperRegistry(bus, nil)
	if len(reg.List()) != 1 {
		t.Fatalf("注册 Mapper 数 = %d，期望 1（模拟传感器）", len(reg.List()))
	}
	m, ok := reg.Get("mock-sensor")
	if !ok {
		t.Fatal("mock-sensor 应已注册")
	}
	sensor, ok := m.(*mocksensor.MockSensor)
	if !ok {
		t.Fatalf("注册的 Mapper 类型 = %T，期望 *mocksensor.MockSensor", m)
	}
	if !sensor.MqttEnabled() {
		t.Error("装配 EventBus 后传感器应处于 MQTT 模式")
	}
	// 降级路径：bus 为 nil 时传感器为纯本地模式
	reg2 := buildMapperRegistry(nil, nil)
	m2, _ := reg2.Get("mock-sensor")
	if m2.(*mocksensor.MockSensor).MqttEnabled() {
		t.Error("未装配 EventBus 时传感器不应处于 MQTT 模式")
	}
}

// TestBuildMapperRegistryModbusGated 设置 EDGEFLOW_MODBUS_ADDR 后：
// Modbus Mapper 注册、按设备名 mb-sensor-01 可路由（无需真实设备，
// 连接发生在操作时而非注册时）。
func TestBuildMapperRegistryModbusGated(t *testing.T) {
	t.Setenv("EDGEFLOW_MODBUS_ADDR", "127.0.0.1:15020")
	reg := buildMapperRegistry(nil, nil)
	if len(reg.List()) != 2 {
		t.Fatalf("注册 Mapper 数 = %d，期望 2（mock-sensor + modbus-mapper）", len(reg.List()))
	}
	if _, ok := reg.Get("modbus-mapper"); !ok {
		t.Error("modbus-mapper 应已注册")
	}
	if _, ok := reg.Route("mb-sensor-01"); !ok {
		t.Error("设备 mb-sensor-01 应按设备名可路由")
	}
}
