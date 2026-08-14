// 设备状态上报（WBS 5.3 云端侧）：DeviceReport 消息的接收与分发。
//
// 与 PodStatus 同构：CloudHub 只负责协议层校验与回调分发，不感知具体
// 存储实现——由 cmd/cloudcore 装配时注入 store.Upsert 的适配函数。
// 新增回调对既有行为零影响（未注册回调时 DeviceReport 仅记日志），
// 向后兼容。
package cloudhub

import (
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// DeviceReportPayload 是 DeviceReport 消息的负载（边→云，WBS 5.3 契约）。
// 字段与协议契约一致，勿单独修改；存储侧对应的快照类型见
// edgeflow/cloud/pkg/devicestatus.DeviceStatus（由装配层做字段映射）。
type DeviceReportPayload struct {
	DeviceName string             `json:"deviceName"` // 设备名称
	Namespace  string             `json:"namespace"`  // 命名空间（缺省按 default 处理，存储侧兜底）
	Properties map[string]float64 `json:"properties"` // 设备实际上报值：属性名 → 值
	ReportedAt int64              `json:"reportedAt"` // 上报时间（毫秒时间戳）
}

// DeviceReportHandler 处理边侧上报的 DeviceReport 消息（依赖注入，WBS 5.3）。
// 与 PodStatusHandler 同风格：CloudHub 不感知具体存储实现，由 cmd/cloudcore
// 装配时注入 store.Upsert 的适配函数。
//
// 并发与性能约定（与 PodStatusHandler 一致）：
//   - 回调在 CloudHub 内部锁之外、连接处理 goroutine 中同步调用
//   - 实现方应尽快返回、不得执行阻塞操作，也不得反向调用 CloudHub 的方法（防止死锁）
type DeviceReportHandler func(nodeID string, dr DeviceReportPayload)

// SetDeviceReportHandler 注册 DeviceReport 消息回调（nil 表示取消）。
// 可在任意时刻调用；与 SetPodStatusHandler 并存、互不影响
// （向后兼容：未注册回调时 DeviceReport 仅记日志）。
func (s *Server) SetDeviceReportHandler(h DeviceReportHandler) {
	s.mu.Lock()
	s.deviceReportHandler = h
	s.mu.Unlock()
}

// notifyDeviceReport 在锁外安全地调用 DeviceReport 回调：先在锁内取回调
// 快照，再在锁外执行，保证回调执行期间不持有任何 CloudHub 锁（防止
// 回调反向调用 CloudHub 方法时死锁，与 notifyPodStatus 同约定）。
func (s *Server) notifyDeviceReport(nodeID string, dr DeviceReportPayload) {
	s.mu.RLock()
	h := s.deviceReportHandler
	s.mu.RUnlock()
	if h != nil {
		h(nodeID, dr)
	}
}

// handleDeviceReport 处理边侧上报的 DeviceReport 消息：
//   - 未注册连接上报 → 回 not_registered Ack 拒绝
//   - payload 解析失败 / 缺少 deviceName → 回 invalid_message Ack
//   - 校验通过 → 调用注入的 DeviceReportHandler 回调（锁外调用），
//     不另行回 Ack（上报是单向流式语义，与 PodStatus 一致，
//     契约未约定应答；边缘侧无需等待确认）
func (s *Server) handleDeviceReport(c *conn, m *protocol.Message) {
	if !c.registered.Load() {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeNotRegistered, Message: "节点未注册，拒绝 DeviceReport"})
		return
	}
	var dr DeviceReportPayload
	if err := m.DecodePayload(&dr); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "DeviceReport payload 解析失败: " + err.Error()})
		return
	}
	if dr.DeviceName == "" {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "DeviceReport payload 缺少 deviceName"})
		return
	}
	s.notifyDeviceReport(m.Source, dr)
	log.Infof("收到节点 %s 的 DeviceReport: %s/%s 属性 %d 个",
		m.Source, dr.Namespace, dr.DeviceName, len(dr.Properties))
}
