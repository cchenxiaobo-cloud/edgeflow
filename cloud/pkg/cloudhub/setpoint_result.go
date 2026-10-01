// 设定值执行反馈接收（v0.40.0 云端侧，spec 0013 US-5）：SetpointResult 消息。
//
// 与 RuleEvent 同构：CloudHub 只负责协议层校验与回调分发——由 cmd/cloudcore
// 装配时注入 setpointstore.ApplyResult 的适配函数。未注册回调时仅记日志。
package cloudhub

import (
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// SetpointResultPayload 是 SetpointResult 消息的负载（边→云，执行反馈）。
// 字段与边侧 setpointResultPayload JSON 形态一致（setpointId 关联云端建单）。
type SetpointResultPayload struct {
	SetpointID string  `json:"setpointId"`      // 云侧建单 ID（关联键）
	OK         bool    `json:"ok"`              // 执行结果（false 时 error 非空）
	Value      float64 `json:"value"`           // 本次应用的期望值
	Error      string  `json:"error,omitempty"` // 失败原因
	Ts         int64   `json:"ts"`              // 边侧执行时间（毫秒）
}

// SetpointResultHandler 处理边侧上报的 SetpointResult 消息（依赖注入，v0.40.0）。
// 并发约定与 RuleEventHandler 一致（锁外、连接 goroutine 中同步调用）。
type SetpointResultHandler func(nodeID string, r SetpointResultPayload)

// SetSetpointResultHandler 注册 SetpointResult 消息回调（nil 表示取消）。
func (s *Server) SetSetpointResultHandler(h SetpointResultHandler) {
	s.mu.Lock()
	s.setpointResultHandler = h
	s.mu.Unlock()
}

// notifySetpointResult 在锁外安全地调用 SetpointResult 回调。
func (s *Server) notifySetpointResult(nodeID string, r SetpointResultPayload) {
	s.mu.RLock()
	h := s.setpointResultHandler
	s.mu.RUnlock()
	if h != nil {
		h(nodeID, r)
	}
}

// handleSetpointResult 处理边侧上报的 SetpointResult 消息：
//   - 未注册连接上报 → 回 not_registered Ack 拒绝；
//   - payload 解析失败 / 缺少 setpointId → 回 invalid_message Ack；
//   - 校验通过 → 回调注入（不另回 Ack，同 RuleEvent 约定）。
//
// 未知 setpointID 由存储层返回 ErrNotFound（调用方计数忽略）：补传回告可能
// 早于建单恢复可见，属预期乱序（边界登记 KI §41）。
func (s *Server) handleSetpointResult(c *conn, m *protocol.Message) {
	if !c.registered.Load() {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeNotRegistered, Message: "节点未注册，拒绝 SetpointResult"})
		return
	}
	var r SetpointResultPayload
	if err := m.DecodePayload(&r); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "SetpointResult payload 解析失败: " + err.Error()})
		return
	}
	if r.SetpointID == "" {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "SetpointResult payload 缺少 setpointId"})
		return
	}
	s.notifySetpointResult(m.Source, r)
	log.Infof("收到节点 %s 的 SetpointResult: %s ok=%v value=%g（error=%q）",
		m.Source, r.SetpointID, r.OK, r.Value, r.Error)
}
