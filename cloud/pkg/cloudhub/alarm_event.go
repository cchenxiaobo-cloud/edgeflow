// 告警事件接收（v0.40.0 云端侧，spec 0013 US-3）：AlarmEvent 消息的接收与分发。
//
// 与 RuleEvent 同构：CloudHub 只负责协议层校验与回调分发，不感知具体存储
// 实现——由 cmd/cloudcore 装配时注入 alarmstore.Upsert 的适配函数。
// 未注册回调时 AlarmEvent 仅记日志（对既有行为零影响）。
package cloudhub

import (
	"edgeflow/pkg/alarm"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// AlarmEventHandler 处理边侧上报的 AlarmEvent 消息（依赖注入，v0.40.0）。
//
// 并发与性能约定（与 RuleEventHandler 一致）：回调在 CloudHub 内部锁之外、
// 连接处理 goroutine 中同步调用；实现方应尽快返回、不得执行阻塞操作。
type AlarmEventHandler func(nodeID string, a alarm.Alarm)

// SetAlarmEventHandler 注册 AlarmEvent 消息回调（nil 表示取消）。
func (s *Server) SetAlarmEventHandler(h AlarmEventHandler) {
	s.mu.Lock()
	s.alarmEventHandler = h
	s.mu.Unlock()
}

// notifyAlarmEvent 在锁外安全地调用 AlarmEvent 回调（先快照后执行）。
func (s *Server) notifyAlarmEvent(nodeID string, a alarm.Alarm) {
	s.mu.RLock()
	h := s.alarmEventHandler
	s.mu.RUnlock()
	if h != nil {
		h(nodeID, a)
	}
}

// handleAlarmEvent 处理边侧上报的 AlarmEvent 消息：
//   - 未注册连接上报 → 回 not_registered Ack 拒绝；
//   - payload 解析失败 / 校验失败 → 回 invalid_message Ack；
//   - 校验通过 → 调用注入的 AlarmEventHandler 回调（锁外调用），不另行回
//     Ack（与 RuleEvent 一致的单向流式语义）。
//
// 幂等说明：同 alarmID 的重发（聚合节流）由云端 alarmstore.Upsert 合并
// （count/updatedAt 取大、状态只进不退），不走 ruleDedup——告警重发是
// 预期行为（携带最新 count），不能按消息 ID 去重丢弃。
func (s *Server) handleAlarmEvent(c *conn, m *protocol.Message) {
	if !c.registered.Load() {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeNotRegistered, Message: "节点未注册，拒绝 AlarmEvent"})
		return
	}
	var a alarm.Alarm
	if err := m.DecodePayload(&a); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "AlarmEvent payload 解析失败: " + err.Error()})
		return
	}
	// nodeID 以连接注册身份为准（防伪造 Source 字段）。
	a.NodeID = m.Source
	if err := a.Validate(); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "AlarmEvent payload 校验失败: " + err.Error()})
		return
	}
	s.notifyAlarmEvent(m.Source, a)
	log.Infof("收到节点 %s 的 AlarmEvent: %s（%s/%s severity=%s count=%d state=%s）",
		m.Source, a.AlarmID, a.Namespace, a.DeviceName, a.Severity, a.Count, a.State)
}
