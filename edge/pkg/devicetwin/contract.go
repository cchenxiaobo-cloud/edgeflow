// 设备消息契约（与 Mapper Agent / 云端约定，字段不可改）：
//
//   - DeviceCommand（云→边，TypeDeviceCommand）：
//     {"deviceName":"sensor-01","namespace":"default","property":"targetTemp","value":25}
//   - DeviceReport（边→云，TypeDeviceReport）：
//     {"deviceName":"sensor-01","namespace":"default",
//     "properties":{"temperature":25.5,"humidity":60},"reportedAt":1755168000000}
//
// 方向约定：DeviceCommand 云端可靠下发（QoS 1，边缘回 Ack）；
// DeviceReport 边缘周期上报（尽力而为、最终一致，云端不回 Ack）。
//
// 本文件定义消息负载结构与设备指令执行器接口——它们是设备链路的
// 契约面：负载结构供装配层（cmd/edgecore）与云端对端共同使用，
// 执行器接口是 Mapper 框架的接入点（见 DeviceCommandExecutor 注释）。
package devicetwin

// DeviceCommandPayload 是 DeviceCommand 消息的负载（云→边契约）。
type DeviceCommandPayload struct {
	DeviceName string  `json:"deviceName"` // 目标设备名称
	Namespace  string  `json:"namespace"`  // 命名空间（缺省 "default"）
	Property   string  `json:"property"`   // 目标属性名
	Value      float64 `json:"value"`      // 期望值
}

// DeviceReportPayload 是 DeviceReport 消息的负载（边→云契约）。
type DeviceReportPayload struct {
	DeviceName string             `json:"deviceName"` // 设备名称
	Namespace  string             `json:"namespace"`  // 命名空间（缺省 "default"）
	Properties map[string]float64 `json:"properties"` // 设备实际上报值：属性名 → 值
	ReportedAt int64              `json:"reportedAt"` // 上报时间（毫秒时间戳）
}

// DeviceCommandExecutor 是设备指令执行器（依赖注入，解耦 Mapper）。
// 云端下发的 DeviceCommand 解析后按 deviceName 路由到执行器，由执行器
// 把指令真正下发到物理设备（经 Mapper 框架的设备实例）。
//
// 接入说明（与 Mapper Agent 的协作点）：Mapper 框架（edge/pkg/mapper，
// 另一 Agent 并行开发）就绪后，由 cmd/edgecore 装配层把 Mapper 注册表
// 适配成本接口并注入 handleDeviceCommand；适配逻辑只做"注册表查找 +
// 调用"的薄封装，不改动本接口契约。当前装配层以 nil 占位（骨架路径：
// 指令只更新 Twin.Desired，见 cmd/edgecore/device_handlers.go）。
type DeviceCommandExecutor interface {
	// ExecuteCommand 执行一条设备指令。
	// deviceName/namespace 标识目标设备（namespace 已归一为 "default" 或
	// 契约原值）；返回 error 表示执行失败（指令已送达但设备侧拒绝/异常）。
	ExecuteCommand(deviceName, namespace, property string, value float64) error
}
