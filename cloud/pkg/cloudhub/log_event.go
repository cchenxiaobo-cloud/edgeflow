// 日志同步接收（v0.48.0 云端侧，spec 0021 US-2）：LogSync 消息的接收与分发。
//
// 与 AlarmEvent/RuleEvent 同构：CloudHub 只负责协议层校验与回调分发，
// 不感知具体存储实现——由 cmd/cloudcore 装配时注入 logstore.Append 适配。
// 未注册回调时 LogSync 仅记日志（对既有行为零影响）。
//
// 幂等（spec §3.3）：uplink 补传「写出成功即删行」，AckUplink 失败窗口的
// 重发由 msgID 滚动去重消化（send-once 语义，与 RuleEvent 同构——重复批
// 整体丢弃；日志条目无独立业务键，不允许部分合并）。
package cloudhub

import (
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// LogSyncHandler 处理边侧上报的 LogSync 消息（依赖注入，v0.48.0）。
//
// 并发与性能约定（与 AlarmEventHandler 一致）：回调在 CloudHub 内部锁之外、
// 连接处理 goroutine 中同步调用；实现方应尽快返回、不得执行阻塞操作。
type LogSyncHandler func(nodeID string, b protocol.LogBatch)

// SetLogSyncHandler 注册 LogSync 消息回调（nil 表示取消）。
func (s *Server) SetLogSyncHandler(h LogSyncHandler) {
	s.mu.Lock()
	s.logSyncHandler = h
	s.mu.Unlock()
}

// notifyLogSync 在锁外安全地调用 LogSync 回调（先快照后执行）。
func (s *Server) notifyLogSync(nodeID string, b protocol.LogBatch) {
	s.mu.RLock()
	h := s.logSyncHandler
	s.mu.RUnlock()
	if h != nil {
		h(nodeID, b)
	}
}

// handleLogSync 处理边侧上报的 LogSync 消息：
//   - 未注册连接上报 → 回 not_registered Ack 拒绝；
//   - payload 解析失败 / 校验失败 → 回 invalid_message Ack；
//   - msgID 重复（uplink 重发窗口）→ 静默丢弃（与 RuleEvent 一致不回 Ack，仅日志留痕）；
//   - 校验通过 → 调用注入的 LogSyncHandler 回调（锁外调用），不另回业务 Ack
//     （与 AlarmEvent 一致的单向流式语义）。
func (s *Server) handleLogSync(c *conn, m *protocol.Message) {
	if !c.registered.Load() {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeNotRegistered, Message: "节点未注册，拒绝 LogSync"})
		return
	}
	var b protocol.LogBatch
	if err := m.DecodePayload(&b); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "LogSync payload 解析失败: " + err.Error()})
		return
	}
	// nodeID 逐消息比对注册身份（复核 P2-6 加固：防被攻陷连接伪造他人
	// nodeID 污染日志环；AlarmEvent/RuleEvent 为既有同构模式——仅注册时
	// 校验，统一加固留后续）。
	if id, _ := c.nodeID.Load().(string); id == "" || id != m.Source {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "LogSync Source 与注册身份不符"})
		return
	}
	if err := b.Validate(); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "LogSync payload 校验失败: " + err.Error()})
		return
	}
	if s.ensureLogDedup().checkAndCount(m.Source, m.ID) {
		log.Infof("LogSync 重复消息（msgID=%s node=%s）丢弃", m.ID, m.Source)
		return
	}
	s.notifyLogSync(m.Source, b)
}

// ensureLogDedup 懒初始化 LogSync 去重组件（Server 零值防御；与 ruleDedup
// 同构但独立实例——日志去重不混入 RuleEvent 观测计数）。
func (s *Server) ensureLogDedup() *ruleEventDedup {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logDedup == nil {
		s.logDedup = newRuleEventDedup(defaultRuleDedupCapacity)
	}
	return s.logDedup
}
