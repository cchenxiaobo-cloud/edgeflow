// ModbusMapper 命名空间感知接口测试（M3A P2-1 接口补齐）：
// 覆盖 default ns、显式非 default ns（选项/环境变量）、多 ns 共存路由、
// 空配置回退 default，以及 HandleCommand 返回的 DeviceReport.Namespace
// 与 DeviceNamespace() 同源一致（与注册表索引键联动）。
package modbus

import (
	"context"
	"testing"
	"time"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/pkg/modbussim"

	mocksensor "edgeflow/mappers/mock_sensor"
)

// assertNamespaceResolver 断言 ModbusMapper 实现 DeviceNamespaceResolver
// 接口（注册表 Register 据此建立 namespace/deviceName 路由索引）。
func assertNamespaceResolver(t *testing.T, m *ModbusMapper) {
	t.Helper()
	if _, ok := any(m).(mapper.DeviceNamespaceResolver); !ok {
		t.Error("ModbusMapper 应实现 mapper.DeviceNamespaceResolver 接口")
	}
	if _, ok := any(m).(mapper.DeviceNameResolver); !ok {
		t.Error("ModbusMapper 应实现 mapper.DeviceNameResolver 接口")
	}
	if _, ok := any(m).(mapper.DeviceMapper); !ok {
		t.Error("ModbusMapper 应实现 mapper.DeviceMapper 接口")
	}
}

// TestDeviceNamespaceDefault 未配置 namespace（无选项、无环境变量）时，
// DeviceNamespace 回退 default。
func TestDeviceNamespaceDefault(t *testing.T) {
	t.Setenv(EnvNamespace, "") // 隔离外部环境变量
	m := New("127.0.0.1:15020")
	assertNamespaceResolver(t, m)
	if got := m.DeviceNamespace(); got != DefaultNamespace {
		t.Errorf("DeviceNamespace() = %q, want %q（未配置应回退 default）", got, DefaultNamespace)
	}
	if m.namespace != DefaultNamespace {
		t.Errorf("内部 namespace 字段 = %q, want %q", m.namespace, DefaultNamespace)
	}
}

// TestDeviceNamespaceExplicitOption 显式 WithNamespace 生效（非 default ns）。
func TestDeviceNamespaceExplicitOption(t *testing.T) {
	t.Setenv(EnvNamespace, "env-ns") // 环境变量存在时选项应优先
	m := New("127.0.0.1:15020", WithNamespace("factory-a"))
	assertNamespaceResolver(t, m)
	if got := m.DeviceNamespace(); got != "factory-a" {
		t.Errorf("DeviceNamespace() = %q, want factory-a（WithNamespace 应优先于环境变量）", got)
	}
}

// TestDeviceNamespaceEnvVar 未传选项时从环境变量 EDGEFLOW_MODBUS_NAMESPACE
// 解析 namespace（与 EDGEFLOW_MODBUS_ADDR 的 env 约定一致）。
func TestDeviceNamespaceEnvVar(t *testing.T) {
	t.Setenv(EnvNamespace, "factory-b")
	m := New("127.0.0.1:15020")
	assertNamespaceResolver(t, m)
	if got := m.DeviceNamespace(); got != "factory-b" {
		t.Errorf("DeviceNamespace() = %q, want factory-b（应读取 %s）", got, EnvNamespace)
	}
}

// TestDeviceNamespaceEmptyFallsBackToDefault 空配置（显式空串选项 + 环境变量
// 未设置/为空）统一回退 default：DeviceNamespace() 与快照 Namespace 恒非空。
func TestDeviceNamespaceEmptyFallsBackToDefault(t *testing.T) {
	t.Setenv(EnvNamespace, "")
	for _, m := range []*ModbusMapper{
		New("127.0.0.1:15020", WithNamespace("")), // 显式空串
		New("127.0.0.1:15020"),                    // 完全未配置
	} {
		if got := m.DeviceNamespace(); got != DefaultNamespace {
			t.Errorf("DeviceNamespace() = %q, want %q（空配置应回退 default）", got, DefaultNamespace)
		}
	}
}

// TestRegistryRouteNonDefaultNamespace 注册表联动：非 default ns 的 ModbusMapper
// 注册后，Route 按 namespace/deviceName 精确命中；default ns 下不命中
// （修复前 DeviceNamespaceResolver 缺失时该场景路由失效）。
func TestRegistryRouteNonDefaultNamespace(t *testing.T) {
	t.Setenv(EnvNamespace, "")
	reg := mapper.NewRegistry()
	m := New("127.0.0.1:15020", WithNamespace("factory-a"))
	if err := reg.Register(m); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	// 非 default ns 精确命中
	if got, ok := reg.Route("factory-a", DefaultDeviceName); !ok || got != m {
		t.Error("Route(factory-a, mb-sensor-01) 应命中 ModbusMapper")
	}
	// default ns 不应命中（本 Mapper 未注册在 default）
	if got, ok := reg.Route("default", DefaultDeviceName); ok {
		t.Errorf("Route(default, mb-sensor-01) 不应命中，实际命中 %T", got)
	}
	// 空 namespace 归一为 default：同样不应命中
	if _, ok := reg.Route("", DefaultDeviceName); ok {
		t.Error("Route(\"\", mb-sensor-01) 归一为 default 后不应命中")
	}
}

// TestRegistryDefaultNamespaceRoute 未配置 namespace 的 ModbusMapper
// 按 default 建立路由索引（存量兼容：原部署路径行为不变）。
func TestRegistryDefaultNamespaceRoute(t *testing.T) {
	t.Setenv(EnvNamespace, "")
	reg := mapper.NewRegistry()
	m := New("127.0.0.1:15020")
	if err := reg.Register(m); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if got, ok := reg.Route("default", DefaultDeviceName); !ok || got != m {
		t.Error("Route(default, mb-sensor-01) 应命中（未配置 ns 回退 default）")
	}
}

// TestRegistryMultiNamespaceCoexistence 多 ns 场景：同名设备在不同 namespace
// 下共存注册、路由互不冲突（ModbusMapper + MockSensor 各占一个 ns，
// 设备名相同）。
func TestRegistryMultiNamespaceCoexistence(t *testing.T) {
	t.Setenv(EnvNamespace, "")
	reg := mapper.NewRegistry()
	mod := New("127.0.0.1:15020", WithNamespace("factory-a"))
	mock := mocksensor.New(DefaultDeviceName, mocksensor.WithNamespace("factory-b"))
	if err := reg.Register(mod); err != nil {
		t.Fatalf("注册 ModbusMapper 失败: %v", err)
	}
	if err := reg.Register(mock); err != nil {
		t.Fatalf("同名设备不同 namespace 应可共存: %v", err)
	}
	// 各 ns 精确命中各自 Mapper
	if got, ok := reg.Route("factory-a", DefaultDeviceName); !ok || got != mod {
		t.Error("Route(factory-a, mb-sensor-01) 应命中 ModbusMapper")
	}
	if got, ok := reg.Route("factory-b", DefaultDeviceName); !ok || got != mock {
		t.Error("Route(factory-b, mb-sensor-01) 应命中 MockSensor")
	}
	// default ns 下无人注册，不应命中
	if _, ok := reg.Route("default", DefaultDeviceName); ok {
		t.Error("Route(default, mb-sensor-01) 不应命中")
	}
	// Dispatch 按 ns 路由：MockSensor 侧可真实执行（reset 不依赖设备连接）
	rep, err := reg.Dispatch(mapper.DeviceCommand{
		DeviceName: DefaultDeviceName, Namespace: "factory-b", Property: "reset",
	})
	if err != nil {
		t.Fatalf("Dispatch(factory-b) 失败: %v", err)
	}
	if rep.Namespace != "factory-b" {
		t.Errorf("Dispatch 返回 Namespace = %q, want factory-b", rep.Namespace)
	}
}

// TestReportNamespaceMatchesResolver 端到端：HandleCommand 返回的
// DeviceReport.Namespace 与 DeviceNamespace() 同源一致（非 default ns）。
// 用真实模拟器走完整的写寄存器 + 回读验证链路。
func TestReportNamespaceMatchesResolver(t *testing.T) {
	t.Setenv(EnvNamespace, "")
	sim := modbussim.New("127.0.0.1:0", modbussim.WithSeed(42), modbussim.WithStep(30*time.Millisecond))
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	defer func() { _ = sim.Stop() }()

	m := New(sim.Addr(), WithNamespace("factory-a"), WithTimeout(2*time.Second))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = m.Stop() }()

	if got := m.DeviceNamespace(); got != "factory-a" {
		t.Fatalf("前置条件失败：DeviceNamespace() = %q", got)
	}
	report, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: DefaultDeviceName, Namespace: "factory-a",
		Property: "targetTemp", Value: 30.0,
	})
	if err != nil {
		t.Fatalf("HandleCommand 失败: %v", err)
	}
	if report.Namespace != "factory-a" {
		t.Errorf("report.Namespace = %q, want factory-a（应与 DeviceNamespace() 同源）", report.Namespace)
	}
	if report.DeviceName != DefaultDeviceName {
		t.Errorf("report.DeviceName = %q, want %q", report.DeviceName, DefaultDeviceName)
	}
	if report.ReportedAt <= 0 {
		t.Error("report.ReportedAt 应非零")
	}
}
