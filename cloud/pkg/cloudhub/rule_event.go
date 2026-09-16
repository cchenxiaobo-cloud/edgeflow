// 规则触发事件（v0.37.0 云端侧）：RuleEvent 消息的接收与分发。
//
// 与 DeviceReport 同构：CloudHub 只负责协议层校验与回调分发，不感知
// 具体存储实现——由 cmd/cloudcore 装配时注入 rulestore.AppendEvent 的
// 适配函数。未注册回调时 RuleEvent 仅记日志（对既有行为零影响）。
package cloudhub

import (
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/rules"
)

// RuleEventHandler 处理边侧上报的 RuleEvent 消息（依赖注入，v0.37.0）。
//
// 并发与性能约定（与 DeviceReportHandler 一致）：
//   - 回调在 CloudHub 内部锁之外、连接处理 goroutine 中同步调用；
//   - 实现方应尽快返回、不得执行阻塞操作，也不得反向调用 CloudHub 的方法。
type RuleEventHandler func(nodeID string, ev rules.Event)

// SetRuleEventHandler 注册 RuleEvent 消息回调（nil 表示取消）。
// 可在任意时刻调用；与 SetDeviceReportHandler 并存、互不影响。
func (s *Server) SetRuleEventHandler(h RuleEventHandler) {
	s.mu.Lock()
	s.ruleEventHandler = h
	s.mu.Unlock()
}

// notifyRuleEvent 在锁外安全地调用 RuleEvent 回调（先快照后执行，
// 与 notifyDeviceReport 同约定）。
func (s *Server) notifyRuleEvent(nodeID string, ev rules.Event) {
	s.mu.RLock()
	h := s.ruleEventHandler
	s.mu.RUnlock()
	if h != nil {
		h(nodeID, ev)
	}
}

// handleRuleEvent 处理边侧上报的 RuleEvent 消息：
//   - 未注册连接上报 → 回 not_registered Ack 拒绝；
//   - payload 解析失败 / 缺少 ruleId 或 deviceName → 回 invalid_message Ack；
//   - 校验通过 → 调用注入的 RuleEventHandler 回调（锁外调用），不另行回
//     Ack（与 DeviceReport 一致的单向流式语义；边缘侧无需等待确认）。
func (s *Server) handleRuleEvent(c *conn, m *protocol.Message) {
	if !c.registered.Load() {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeNotRegistered, Message: "节点未注册，拒绝 RuleEvent"})
		return
	}
	var ev rules.Event
	if err := m.DecodePayload(&ev); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "RuleEvent payload 解析失败: " + err.Error()})
		return
	}
	if ev.RuleID == "" || ev.DeviceName == "" {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "RuleEvent payload 缺少 ruleId 或 deviceName"})
		return
	}
	s.notifyRuleEvent(m.Source, ev)
	log.Infof("收到节点 %s 的 RuleEvent: %s（%s/%s=%g，severity=%s）",
		m.Source, ev.RuleID, ev.Namespace, ev.DeviceName, ev.Value, ev.Severity)
}
