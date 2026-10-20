// TermResult 结果接收（v0.49.0 云端侧，spec 0022）：受控命令执行结果回传。
//
// 与 SetpointResult 同构：CloudHub 只负责协议层校验与回调分发（归属校验在
// termgw.DeliverResult——会话节点 vs 上报节点比对，v0.48 P2-6 同款加固）。
// 未注册回调时仅记日志。
package cloudhub

import (
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// TermResultHandler 处理边侧 TermResult（依赖注入）。并发约定与
// SetpointResultHandler 一致：锁外、连接读 goroutine 中同步调用——回调方
// （termgw.DeliverResult）仅做 chan 投递，不长活。
type TermResultHandler func(nodeID string, r *protocol.TermResultPayload)

// SetTermResultHandler 注册 TermResult 消息回调（nil 表示取消）。
func (s *Server) SetTermResultHandler(h TermResultHandler) {
	s.mu.Lock()
	s.termResultHandler = h
	s.mu.Unlock()
}

// notifyTermResult 在锁外安全地调用 TermResult 回调。
func (s *Server) notifyTermResult(nodeID string, r *protocol.TermResultPayload) {
	s.mu.RLock()
	h := s.termResultHandler
	s.mu.RUnlock()
	if h != nil {
		h(nodeID, r)
	}
}

// handleTermResult 处理边侧 TermResult：
//   - 未注册连接上报 → 回 not_registered Ack 拒绝；
//   - payload 解析失败 → 回 invalid_message Ack；
//   - Source 与 Target 不符（伪造他人节点结果）→ 回 invalid Ack 拒绝；
//   - 校验通过 → 回调注入（不另回 Ack，同 RuleEvent 约定）。
//
// 无 pending 的 execID 由 termgw 静默丢弃（超时已清理/重复结果）。
func (s *Server) handleTermResult(c *conn, m *protocol.Message) {
	if !c.registered.Load() {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeNotRegistered, Message: "节点未注册，拒绝 TermResult"})
		return
	}
	var r protocol.TermResultPayload
	if err := m.DecodePayload(&r); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "TermResult payload 解析失败: " + err.Error()})
		return
	}
	if err := r.Validate(); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "TermResult 校验失败: " + err.Error()})
		return
	}
	// 逐消息身份比对（v0.48 P2-6 同款）：边→云消息的节点身份在 Source
	// （Target="cloud" 为下行方向约定；run3 教训——误用 m.Target 使全部
	// TermResult 被身份校验拒绝）。
	if nid := c.nodeID.Load(); nid == nil || nid.(string) != m.Source {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "TermResult 节点身份不符"})
		return
	}
	log.Debugf("[term] 收到执行结果（node=%s exec=%s ok=%v rejected=%v）", m.Source, r.ExecID, r.OK, r.Rejected)
	s.notifyTermResult(m.Source, &r)
}
