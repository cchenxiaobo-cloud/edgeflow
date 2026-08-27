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
	"strings"

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/edge/pkg/eventbus"
	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"

	mocksensor "edgeflow/mappers/mock_sensor"
	modbusmapper "edgeflow/mappers/modbus"
	opcuamapper "edgeflow/mappers/opcua"
)

// EnvEnableMapper 是 Mapper 装配开关的环境变量名（WBS 5.1 装配门控）。
//
// 默认开启（true）——兼容存量行为：tests/e2e/device_e2e_test.go 的
// DeviceCommand → Mapper 执行 → Properties 周期上报链路依赖 Mapper 装配，
// 默认关闭会破坏该链路；v0.1.1 已发布制品同样是默认装配，改变默认值
// 属于行为变更而非兼容增强。显式设置 EDGEFLOW_EDGECORE_ENABLE_MAPPER
// =false（或 0/off/no，大小写不敏感）时关闭装配：不注册任何 Mapper、
// 不启动采集循环、指令执行器保持 nil（handleDeviceCommand 走骨架路径，
// 仅更新 Twin.Desired），行为与 Mapper 框架接入前完全一致（纯影子模式）。
// "无设备时更安全"的诉求由 Modbus 的显式 opt-in 门控承担：mock_sensor
// 是纯本地模拟、无外部依赖、失败只告警，默认开启不引入新风险。
const EnvEnableMapper = "EDGEFLOW_EDGECORE_ENABLE_MAPPER"

// mapperEnabledFromEnv 解析装配开关（大小写不敏感、容忍首尾空白）：
//   - 未设置 → true（默认开启，兼容存量行为与既有 e2e）；
//   - false/0/off/no → false（显式关闭）；
//   - 其余值 → true（未知值不改变默认行为，记警告后按默认开启）。
func mapperEnabledFromEnv() bool {
	v := os.Getenv(EnvEnableMapper)
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return true
	case "false", "0", "off", "no":
		return false
	case "true", "1", "on", "yes":
		return true
	default:
		log.Warnf("%s=%q 不是合法开关值（支持 true/false/1/0/on/off/yes/no），按默认开启处理",
			EnvEnableMapper, v)
		return true
	}
}

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
//
// 装配开关（EDGEFLOW_EDGECORE_ENABLE_MAPPER，见 mapperEnabledFromEnv）：
// 关闭时不做任何注册、直接返回 nil——调用方（main.go）据此跳过生命周期
// 管理（StartAll/StopAll）并保持指令执行器为 nil（骨架路径），行为与
// Mapper 框架接入前完全一致（纯影子模式）。
func buildMapperRegistry(bus *eventbus.EventBus, ledger *metamanager.Ledger) *mapper.MapperRegistry {
	if !mapperEnabledFromEnv() {
		log.Infof("Mapper 装配已关闭（%s=false）：不注册 Mapper、不启动采集循环，设备指令仅更新 Twin.Desired",
			EnvEnableMapper)
		return nil
	}
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
	// OPC-UA 设备接入（显式 opt-in，WBS 5.2 第二阶段）：
	// EDGEFLOW_OPCUA_ENDPOINT 非空即注册（默认设备 opcua-device-01）；
	// 点位表 EDGEFLOW_OPCUA_NODES 非法 → 注册失败仅 Warn（不影响其余 Mapper）。
	if ep := os.Getenv(opcuamapper.EnvEndpoint); ep != "" {
		nodes, perr := opcuamapper.ParseNodes(os.Getenv(opcuamapper.EnvNodes))
		if perr != nil {
			log.Warnf("OPC-UA Mapper 点位解析失败，跳过注册: %v", perr)
		} else if m, merr := opcuamapper.New(ep, opcuamapper.WithPoints(nodes), opcuamapper.WithLedger(ledger)); merr != nil {
			log.Warnf("注册 OPC-UA Mapper 失败: %v", merr)
		} else if err := reg.Register(m); err != nil {
			log.Warnf("注册 OPC-UA Mapper 失败: %v", err)
		} else {
			mode := "轮询"
			if opcuamapper.SubscriptionEnabled() {
				mode = "订阅推送"
			}
			log.Infof("OPC-UA Mapper 已注册（endpoint=%s，点位 %d 个，设备 opcua-device-01，模式 %s）", ep, len(nodes), mode)
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
// 命名空间说明：Collect 只返回属性值、不带命名空间，写入影子时按 Mapper
// 自身声明的命名空间补全（namespaceOfMapper，与注册表路由索引同一判定
// 规则）——实现 mapper.DeviceNamespaceResolver 的 Mapper（如 Modbus 配置
// EDGEFLOW_MODBUS_NAMESPACE=plant-a）采集值落在对应 ns 的影子下，与指令
// 路径（Route 按 ns 路由）一致；未实现该接口的 Mapper 按 default 汇入。
// 默认部署（所有内置 Mapper 均为 default 命名空间）行为与 v0.2.0 逐字节
// 一致；多命名空间部署不再出现 default/x 与 plant-a/x 双条目
// （KNOWN-ISSUES §1 ①）。
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
		ns := namespaceOfMapper(m)
		for _, deviceName := range deviceNamesOf(m) {
			twins.UpsertReported(deviceName, ns, props, now)
		}
	}
}

// namespaceOfMapper 返回 Mapper 的设备命名空间：实现 DeviceNamespaceResolver
// 且声明非空命名空间时用之；否则回退 default。判定规则与注册表路由索引
// （mapper 包 Register 的 ns 解析 / deviceKeyOf）同源——采集汇入影子与指令
// 路由对同一 Mapper 使用同一命名空间判定，两条路径不可能出现双条目。
func namespaceOfMapper(m mapper.DeviceMapper) string {
	if nsRes, ok := m.(mapper.DeviceNamespaceResolver); ok && nsRes.DeviceNamespace() != "" {
		return nsRes.DeviceNamespace()
	}
	return devicetwin.DefaultNamespace
}
