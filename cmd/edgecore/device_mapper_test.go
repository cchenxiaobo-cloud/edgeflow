// Mapper 装配（WBS 5.1/5.5）测试：指令执行器适配（Dispatch 路由 +
// 快照落影子）、采集汇入影子、内置模拟传感器注册。
package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/edge/pkg/edgehub"
	"edgeflow/edge/pkg/eventbus"
	"edgeflow/edge/pkg/mapper"
	"edgeflow/pkg/protocol"

	mocksensor "edgeflow/mappers/mock_sensor"
	opcuamapper "edgeflow/mappers/opcua"
)

// fakeMapper 是测试用 DeviceMapper：记录收到的指令、可配置失败、
// 可配置采集值（避免测试依赖 mappers/ 下具体实现）。
// namespace 非空时实现 DeviceNamespaceResolver（模拟多 ns 部署，
// 如 Modbus 配置 EDGEFLOW_MODBUS_NAMESPACE）。
type fakeMapper struct {
	name       string
	deviceName string
	namespace  string
	props      map[string]float64
	cmdErr     error
	collectErr error
	handled    []mapper.DeviceCommand
}

func (f *fakeMapper) Name() string { return f.name }
func (f *fakeMapper) DeviceNames() []string {
	return []string{f.deviceName}
}
func (f *fakeMapper) DeviceNamespace() string     { return f.namespace }
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

// TestCollectMapperReportsNamespace 验证采集汇入影子的命名空间判定
// （KNOWN-ISSUES §1 ① 修复）：
//   - 实现 DeviceNamespaceResolver 且声明非空 ns 的 Mapper（如 Modbus
//     配置 EDGEFLOW_MODBUS_NAMESPACE=plant-a）采集值写入对应 ns 的影子，
//     default 下不再出现双条目；
//   - 未实现/空 ns 的 Mapper 按 default 汇入——与 v0.2.0 行为逐字节一致
//     （默认部署单 ns 不重复）。
func TestCollectMapperReportsNamespace(t *testing.T) {
	// 显式 ns（多 ns 部署）：影子落在 plant-a 下，default 下无条目
	f := &fakeMapper{name: "modbus", deviceName: "mb-sensor-01",
		namespace: "plant-a", props: map[string]float64{"temperature": 41.2}}
	twins := devicetwin.NewStore()
	collectMapperReports(newFakeRegistry(t, f), twins, 999)

	twin, ok := twins.Get("mb-sensor-01", "plant-a")
	if !ok {
		t.Fatal("显式 ns 的 Mapper 采集应写入 plant-a 影子")
	}
	if twin.Reported["temperature"] != 41.2 {
		t.Errorf("采集值未写入 plant-a 影子 Reported: %v", twin.Reported)
	}
	if _, dup := twins.Get("mb-sensor-01", "default"); dup {
		t.Error("显式 ns 的 Mapper 不应再写 default 影子（v0.2.0 双条目问题）")
	}
	if n := len(twins.SnapshotAll()); n != 1 {
		t.Errorf("影子条目数 = %d，期望 1（单 ns 不重复）", n)
	}

	// 默认 ns：与 v0.2.0 行为逐字节一致
	f2 := &fakeMapper{name: "fake", deviceName: "sensor-01",
		props: map[string]float64{"temperature": 27.5}}
	twins2 := devicetwin.NewStore()
	collectMapperReports(newFakeRegistry(t, f2), twins2, 999)
	if _, ok := twins2.Get("sensor-01", "default"); !ok {
		t.Error("默认 ns 的 Mapper 采集应写入 default 影子（v0.2.0 兼容）")
	}
	if n := len(twins2.SnapshotAll()); n != 1 {
		t.Errorf("默认 ns 部署影子条目数 = %d，期望 1", n)
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
	if _, ok := reg.Route("default", "sensor-01"); !ok {
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
	if _, ok := reg.Route("default", "mb-sensor-01"); !ok {
		t.Error("设备 mb-sensor-01 应按设备名可路由")
	}
}

// TestMapperEnabledFromEnv 验证装配开关解析（大小写不敏感、容忍空白）：
// 未设置/true 系 → 默认开启；false 系 → 关闭；未知值 → 按默认开启。
func TestMapperEnabledFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", true}, // 默认开启（兼容存量行为与既有 e2e）
		{"true", true}, {"1", true}, {"on", true}, {"yes", true}, {"TRUE", true}, {" On ", true},
		{"false", false}, {"0", false}, {"off", false}, {"no", false}, {"False", false},
		{"bogus", true}, // 未知值：按默认开启（记警告，不改变默认行为）
	}
	for _, c := range cases {
		t.Setenv(EnvEnableMapper, c.val)
		if got := mapperEnabledFromEnv(); got != c.want {
			t.Errorf("mapperEnabledFromEnv() 在 %s=%q 时 = %v，期望 %v", EnvEnableMapper, c.val, got, c.want)
		}
	}
}

// TestBuildMapperRegistryDisabled 验证装配开关关闭态：
// EDGEFLOW_EDGECORE_ENABLE_MAPPER=false → buildMapperRegistry 返回 nil
// （不注册任何 Mapper、不进入 MQTT 模式），无论 EventBus 是否已装配。
func TestBuildMapperRegistryDisabled(t *testing.T) {
	t.Setenv("EDGEFLOW_MODBUS_ADDR", "127.0.0.1:15020") // 即使显式配置 Modbus 也不注册
	t.Setenv(EnvEnableMapper, "false")

	if reg := buildMapperRegistry(nil, nil); reg != nil {
		t.Errorf("开关关闭（bus=nil）应返回 nil 注册表，实际 %+v", reg)
	}
	bus := eventbus.New("tcp://127.0.0.1:1") // 仅验证装配，不 Connect
	if reg := buildMapperRegistry(bus, nil); reg != nil {
		t.Errorf("开关关闭（bus 非 nil）应返回 nil 注册表，实际 %+v", reg)
	}
}

// TestBuildMapperRegistryEnabledExplicit 验证开关显式开启：
// EDGEFLOW_EDGECORE_ENABLE_MAPPER=true → 正常装配（与默认行为一致）。
func TestBuildMapperRegistryEnabledExplicit(t *testing.T) {
	t.Setenv("EDGEFLOW_MODBUS_ADDR", "")
	t.Setenv(EnvEnableMapper, "true")
	reg := buildMapperRegistry(nil, nil)
	if reg == nil || len(reg.List()) != 1 {
		t.Fatalf("开关开启应正常装配 1 个 Mapper，实际 %+v", reg)
	}
	if _, ok := reg.Route("default", "sensor-01"); !ok {
		t.Error("模拟传感器应按设备名 sensor-01 可路由")
	}
}

// nsFakeMapper 是带命名空间声明的测试 Mapper：实现
// DeviceNamespaceResolver，用于验证「namespace+deviceName」路由
// （跨命名空间同名设备互不冲突，M3A P2-1）。
type nsFakeMapper struct {
	fakeMapper
	namespace string
}

// DeviceNamespace 声明本 Mapper 管理的设备命名空间。
func (f *nsFakeMapper) DeviceNamespace() string { return f.namespace }

// HandleCommand 返回带自身命名空间的快照（fakeMapper 硬编码 default，
// 多命名空间路由验证需要按实例区分）。
func (f *nsFakeMapper) HandleCommand(cmd mapper.DeviceCommand) (mapper.DeviceReport, error) {
	report, err := f.fakeMapper.HandleCommand(cmd)
	if err != nil {
		return report, err
	}
	report.Namespace = f.namespace
	return report, nil
}

// TestMapperExecutorNamespaceRouting 验证命令路由的 namespace 匹配：
// 同名设备 sensor-01 分别注册在 default 与 factory-a 两个命名空间，
// ExecuteCommand 按 namespace+deviceName 精确路由到各自 Mapper，
// 互不串扰；未注册的命名空间返回错误。
func TestMapperExecutorNamespaceRouting(t *testing.T) {
	def := &nsFakeMapper{fakeMapper: fakeMapper{name: "fake-default", deviceName: "sensor-01"}, namespace: "default"}
	fac := &nsFakeMapper{fakeMapper: fakeMapper{name: "fake-factory", deviceName: "sensor-01"}, namespace: "factory-a"}
	reg := mapper.NewRegistry()
	if err := reg.Register(def); err != nil {
		t.Fatalf("注册 default Mapper 失败: %v", err)
	}
	if err := reg.Register(fac); err != nil {
		t.Fatalf("注册 factory-a Mapper 失败: %v", err)
	}
	twins := devicetwin.NewStore()
	exec := &mapperCommandExecutor{reg: reg, twins: twins}

	// factory-a 的命令只路由到 factory-a 的 Mapper
	if err := exec.ExecuteCommand("sensor-01", "factory-a", "targetTemp", 22); err != nil {
		t.Fatalf("factory-a 指令执行失败: %v", err)
	}
	if len(fac.handled) != 1 || len(def.handled) != 0 {
		t.Errorf("namespace 路由串扰: factory=%d default=%d", len(fac.handled), len(def.handled))
	}
	// default 的命令只路由到 default 的 Mapper
	if err := exec.ExecuteCommand("sensor-01", "default", "targetTemp", 23); err != nil {
		t.Fatalf("default 指令执行失败: %v", err)
	}
	if len(def.handled) != 1 || len(fac.handled) != 1 {
		t.Errorf("namespace 路由串扰: factory=%d default=%d", len(fac.handled), len(def.handled))
	}
	// 影子按命名空间隔离（default/sensor-01 与 factory-a/sensor-01 并存）
	if _, ok := twins.Get("sensor-01", "default"); !ok {
		t.Error("default/sensor-01 影子应已创建")
	}
	if _, ok := twins.Get("sensor-01", "factory-a"); !ok {
		t.Error("factory-a/sensor-01 影子应已创建")
	}
	// 未注册的命名空间返回错误（云端收 502）
	if err := exec.ExecuteCommand("sensor-01", "ghost-ns", "targetTemp", 24); err == nil {
		t.Error("未注册命名空间应返回错误")
	}
}

// tickingMapper 是递增采集的测试 Mapper：每轮 Collect 返回递增的
// round 属性值，用于验证周期采集确实在逐轮驱动影子更新。
type tickingMapper struct {
	fakeMapper
	rounds int64
}

// Collect 每轮递增 rounds 并返回当前轮次。
func (f *tickingMapper) Collect() (map[string]float64, error) {
	f.rounds++
	return map[string]float64{"round": float64(f.rounds)}, nil
}

// TestRunDeviceReportLoopPeriodicCollection 验证周期采集触发上报链路的
// 前半段（采集 → 影子）：上报循环每轮先执行 collectMapperReports
// （假 Mapper 注入，round 值逐轮递增），影子随之更新——证明周期采集
// 在真实驱动数据汇入；发送失败（client 未启动）不影响循环存活。
func TestRunDeviceReportLoopPeriodicCollection(t *testing.T) {
	f := &tickingMapper{fakeMapper: fakeMapper{name: "tick", deviceName: "sensor-01"}}
	reg := mapper.NewRegistry()
	if err := reg.Register(f); err != nil {
		t.Fatalf("注册 tickingMapper 失败: %v", err)
	}
	client := edgehub.New(edgehub.Options{CloudAddr: "ws://127.0.0.1:1", NodeID: "edge-dev-1"})
	twins := devicetwin.NewStore()

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDeviceReportLoop(client, reg, twins, "edge-dev-1", func() time.Duration { return 10 * time.Millisecond }, stopCh)
	}()

	time.Sleep(80 * time.Millisecond) // 跑多轮（含启动即上报一轮）
	close(stopCh)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("上报循环未在 stopCh 关闭后退出")
	}

	// 周期采集确实逐轮驱动影子更新（多轮后 round > 1）
	if f.rounds < 2 {
		t.Fatalf("Collect 调用次数 = %d，期望 ≥2（周期采集未逐轮驱动）", f.rounds)
	}
	twin, ok := twins.Get("sensor-01", "default")
	if !ok {
		t.Fatal("采集后应有影子")
	}
	if twin.Reported["round"] != float64(f.rounds) {
		t.Errorf("影子 round = %v，期望 %v（最新一轮采集值）", twin.Reported["round"], float64(f.rounds))
	}
}

// flakyMapper 是前 N 轮采集失败的测试 Mapper：用于验证「采集失败
// 不影响链路存活」——失败只记错误日志并跳过该轮，后续轮次恢复后
// 影子照常更新、上报循环不退出。
type flakyMapper struct {
	fakeMapper
	failuresLeft int
}

// Collect 前 failuresLeft 轮返回错误，之后正常返回属性值。
func (f *flakyMapper) Collect() (map[string]float64, error) {
	if f.failuresLeft > 0 {
		f.failuresLeft--
		return nil, errors.New("采集失败（模拟）")
	}
	return map[string]float64{"temperature": 26.5}, nil
}

// TestRunDeviceReportLoopCollectFailureSurvives 验证采集失败不影响链路
// 存活：前 2 轮 Collect 失败（Warn + 跳过该 Mapper），第 3 轮起恢复——
// 循环不 panic、不退出，恢复后采集值照常汇入影子（错误日志 + 继续）。
func TestRunDeviceReportLoopCollectFailureSurvives(t *testing.T) {
	f := &flakyMapper{fakeMapper: fakeMapper{name: "flaky", deviceName: "sensor-01"}, failuresLeft: 2}
	reg := mapper.NewRegistry()
	if err := reg.Register(f); err != nil {
		t.Fatalf("注册 flakyMapper 失败: %v", err)
	}
	client := edgehub.New(edgehub.Options{CloudAddr: "ws://127.0.0.1:1", NodeID: "edge-dev-1"})
	twins := devicetwin.NewStore()

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDeviceReportLoop(client, reg, twins, "edge-dev-1", func() time.Duration { return 10 * time.Millisecond }, stopCh)
	}()

	time.Sleep(80 * time.Millisecond) // 覆盖失败轮 + 恢复轮
	close(stopCh)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("上报循环未在 stopCh 关闭后退出")
	}

	if f.failuresLeft != 0 {
		t.Errorf("失败次数未耗尽（%d），采集失败路径未被执行", f.failuresLeft)
	}
	twin, ok := twins.Get("sensor-01", "default")
	if !ok {
		t.Fatal("恢复后应有影子")
	}
	if twin.Reported["temperature"] != 26.5 {
		t.Errorf("恢复后采集值未汇入影子: %v", twin.Reported)
	}
}

// TestBuildMapperRegistryOPCUAGated 验证 OPC-UA Mapper 装配门控：
//   - EDGEFLOW_OPCUA_ENDPOINT 空 → 不注册（默认零行为变化）；
//   - ENDPOINT 非空 + NODES 合法 → 注册 opcua-mapper 并按设备名可路由；
//   - NODES 非法 → 仅告警跳过注册（不影响其余 Mapper）。
func TestBuildMapperRegistryOPCUAGated(t *testing.T) {
	t.Setenv(opcuamapper.EnvEndpoint, "")
	t.Setenv("EDGEFLOW_MODBUS_ADDR", "")
	reg := buildMapperRegistry(nil, nil)
	if _, ok := reg.Get("opcua-mapper"); ok {
		t.Fatal("ENDPOINT 未设置时不应注册 opcua-mapper")
	}

	t.Setenv(opcuamapper.EnvEndpoint, "opc.tcp://127.0.0.1:14840")
	t.Setenv(opcuamapper.EnvNodes, "temperature=ns=2;i=1001,setpoint=ns=2;i=3001")
	reg2 := buildMapperRegistry(nil, nil)
	m, ok := reg2.Get("opcua-mapper")
	if !ok {
		t.Fatal("ENDPOINT 设置后 opcua-mapper 应已注册")
	}
	if _, ok := reg2.Route("default", "opcua-device-01"); !ok {
		t.Error("设备 opcua-device-01 应按设备名可路由")
	}
	var names []string
	if res, ok := m.(interface{ DeviceNames() []string }); ok {
		names = res.DeviceNames()
	}
	if len(names) != 1 || names[0] != "opcua-device-01" {
		t.Errorf("OPC-UA 设备名 = %v，期望 [opcua-device-01]", names)
	}

	// NODES 非法 → 仅告警跳过（装配失败不 panic、不影响 mock-sensor）
	t.Setenv(opcuamapper.EnvNodes, "bad=node!!")
	reg3 := buildMapperRegistry(nil, nil)
	if _, ok := reg3.Get("opcua-mapper"); ok {
		t.Error("NODES 非法时不应注册 opcua-mapper")
	}
	if _, ok := reg3.Route("default", "sensor-01"); !ok {
		t.Error("非法 NODES 不应影响模拟传感器注册")
	}
}
