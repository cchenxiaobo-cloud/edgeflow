// Mapper 装配（WBS 5.1/5.5 与 M3 设备链路集成）：把 Mapper 注册表接入
// DeviceTwin 指令链路与周期上报。
//
// 协作说明（与 Mapper Agent 的契约，见 edge/pkg/mapper/mapper.go 与
// docs/MAPPER-GUIDE.md §5）：
//   - 指令下发：EdgeHub 收到 TypeDeviceCommand → handleDeviceCommand →
//     本文件 mapperCommandExecutor.ExecuteCommand → 注册表 Dispatch
//     （按 deviceName 路由到具体 Mapper 的 HandleCommand）→ 返回的最新
//     状态快照写入 Twin.Reported（记录报告），Desired 由调用方写入；
//   - 周期上报：上报循环在快照 Twin 前先执行 collectMapperReports——
//     把各 Mapper 的 Collect() 结果汇入 Twin.Reported，影子是上报的
//     唯一数据源（Mapper 只负责采集，不感知上报链路）。
package main

import (
	"fmt"
	"os"

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/edge/pkg/eventbus"
	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"

	mocksensor "edgeflow/mappers/mock_sensor"
	modbusmapper "edgeflow/mappers/modbus"
)

// mapperCommandExecutor 把 Mapper 注册表适配成 devicetwin.DeviceCommandExecutor
// （装配层薄封装：只做"注册表查找 + 调用 + 结果落影子"，不改动任一方接口）。
type mapperCommandExecutor struct {
	reg   *mapper.MapperRegistry
	twins *devicetwin.TwinStore
}

// ExecuteCommand 执行一条设备指令：按 deviceName 路由到注册表 Dispatch，
// 成功后把 Mapper 返回的最新状态快照（DeviceReport）写入影子 Reported。
// 返回 error 时由 handleDeviceCommand 透传（云端收 502，Desired 仍写入）。
func (e *mapperCommandExecutor) ExecuteCommand(deviceName, namespace, property string, value float64) error {
	if e == nil || e.reg == nil {
		return fmt.Errorf("mapper 注册表未装配")
	}
	report, err := e.reg.Dispatch(mapper.DeviceCommand{
		DeviceName: deviceName,
		Namespace:  namespace,
		Property:   property,
		Value:      value,
	})
	if err != nil {
		return err
	}
	// 记录报告：指令执行后的最新状态快照汇入影子，
	// 下一轮周期上报即推送云端（影子是上报的唯一数据源）。
	if e.twins != nil {
		e.twins.UpsertReported(report.DeviceName, report.Namespace, report.Properties, report.ReportedAt)
	}
	log.Infof("Mapper 指令执行完成（%s/%s.%s=%v），快照 %d 个属性已写入影子",
		namespace, deviceName, property, value, len(report.Properties))
	return nil
}

// buildMapperRegistry 构造 Mapper 注册表并注册内置设备。
//
// 当前注册模拟温湿度传感器（sensor-01，mappers/mock_sensor）：
//   - bus 非 nil：传感器进入 MQTT 模式（WBS 3.6 设备数据面）——采集结果
//     同时发布到 devices/default/sensor-01/telemetry，订阅 command 主题
//     接收本地指令；云边通道（DeviceCommand/DeviceReport）照常工作，
//     构成双通道（语义见 docs/MAPPER-GUIDE.md §8）；
//   - bus 为 nil（未装配或连接失败降级）：纯本地模式（默认行为）。
//
// Modbus Mapper（WBS 5.2，mappers/modbus）：设置环境变量
// EDGEFLOW_MODBUS_ADDR（如 127.0.0.1:15020）即接入 Modbus TCP 设备
// （默认设备 mb-sensor-01）。显式设置才注册：避免没有 Modbus 设备的
// 部署在每轮采集时报连接错误；设备晚于 edgecore 启动也没关系——Mapper
// 每次操作前按需连接/重连。ledger 非 nil 时所有操作落台账（SQLite，
// 保留 30 天，见 docs/MODBUS-GUIDE.md）。
//
// 真实设备接入时以对应 Mapper 实现替换本函数内的注册即可
// （协议适配只新增 DeviceMapper 实现，框架与装配无需改动）。
func buildMapperRegistry(bus *eventbus.EventBus, ledger *metamanager.Ledger) *mapper.MapperRegistry {
	reg := mapper.NewRegistry()
	if err := reg.Register(mocksensor.New("sensor-01", mocksensor.WithEventBus(bus))); err != nil {
		// 注册失败只告警：设备链路退化为"指令找不到 Mapper → 502"，
		// 不影响 edgecore 其余能力（Pod/节点链路）。
		log.Warnf("注册模拟传感器失败: %v", err)
	}
	// Modbus 设备接入（显式 opt-in，见函数头注释）
	if addr := os.Getenv(modbusmapper.EnvAddr); addr != "" {
		if err := reg.Register(modbusmapper.New(addr, modbusmapper.WithLedger(ledger))); err != nil {
			log.Warnf("注册 Modbus Mapper 失败: %v", err)
		} else {
			log.Infof("Modbus Mapper 已注册（addr=%s，设备 mb-sensor-01，台账 %v）", addr, ledger != nil)
		}
	}
	if bus != nil {
		log.Infof("Mapper 注册完成：mock-sensor 已启用 MQTT 数据面（遥测 %s，指令 %s）",
			"devices/default/sensor-01/telemetry", "devices/default/sensor-01/command")
	}
	return reg
}

// deviceNamesOf 返回一个 Mapper 管理的设备名列表：
// 实现了 DeviceNameResolver 的取 DeviceNames()，否则退化"注册名即设备名"
// （与注册表 Route 的回退规则一致）。
func deviceNamesOf(m mapper.DeviceMapper) []string {
	if res, ok := m.(mapper.DeviceNameResolver); ok {
		return res.DeviceNames()
	}
	return []string{m.Name()}
}

// collectMapperReports 把各 Mapper 的最新采集值汇入影子（Collect → Twin.Reported）。
// 上报循环每轮先执行本步骤再快照 Twin：影子因此始终持有设备最新数据，
// 周期上报无需感知 Mapper 的存在。reg 为 nil 时静默跳过（纯影子模式）。
//
// 命名空间说明：Collect 只返回属性值、不带命名空间，此处按 default 汇入
// （当前内置 Mapper 均为 default 命名空间）；多命名空间场景可在 Mapper
// 采集结果扩展时一并解决。
func collectMapperReports(reg *mapper.MapperRegistry, twins *devicetwin.TwinStore, now int64) {
	if reg == nil || twins == nil {
		return
	}
	for _, m := range reg.List() {
		props, err := m.Collect()
		if err != nil {
			// 单台采集失败不影响其余：只记 Warn，本轮按影子旧值上报
			log.Warnf("Mapper %s 采集失败: %v", m.Name(), err)
			continue
		}
		for _, deviceName := range deviceNamesOf(m) {
			twins.UpsertReported(deviceName, devicetwin.DefaultNamespace, props, now)
		}
	}
}
