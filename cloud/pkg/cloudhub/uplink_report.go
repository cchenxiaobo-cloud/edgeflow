// 上行补传状态上报（v0.39.0，spec 0012 US-6/US-7）：UplinkReport 消息的
// 接收与回调分发（与 RuleEvent 同构：CloudHub 只做协议层校验与锁外回调，
// 不感知存储实现——缓存由 cmd/cloudcore 装配时注入）。
// 未注册回调时仅记日志（对既有行为零影响）。
package cloudhub

import (
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// UplinkReportPayload 是边侧上行队列状态（与 cmd/edgecore 同构）。
type UplinkReportPayload struct {
	Depth    int   `json:"depth"`    // 当前积压总数
	Dropped  int64 `json:"dropped"`  // 累计容量丢弃
	Sent     int64 `json:"sent"`     // 累计成功上送
	OldestTs int64 `json:"oldestTs"` // 最老积压条目入队时间（毫秒；空为 0）
}

// UplinkReportHandler 处理边侧上行状态上报（依赖注入，v0.39.0）。
//
// 并发约定（与 RuleEventHandler 一致）：回调在 CloudHub 内部锁之外、
// 连接处理 goroutine 中同步调用；实现方应尽快返回、不得执行阻塞操作。
type UplinkReportHandler func(nodeID string, r UplinkReportPayload)

// SetUplinkReportHandler 注册 UplinkReport 消息回调（nil 表示取消）。
// 可在任意时刻调用；与既有回调并存、互不影响。
func (s *Server) SetUplinkReportHandler(h UplinkReportHandler) {
	s.mu.Lock()
	s.uplinkReportHandler = h
	s.mu.Unlock()
}

// notifyUplinkReport 在锁外安全地调用回调（先快照后执行）。
func (s *Server) notifyUplinkReport(nodeID string, r UplinkReportPayload) {
	s.mu.RLock()
	h := s.uplinkReportHandler
	s.mu.RUnlock()
	if h != nil {
		h(nodeID, r)
	}
}

// handleUplinkReport 处理边侧上报的 UplinkReport 消息：
//   - 未注册连接上报 → 回 not_registered Ack 拒绝；
//   - payload 解析失败 → 回 invalid_message Ack；
//   - 校验通过 → 调用注入回调（锁外），不另行回 Ack（与 RuleEvent 一致的
//     单向流式语义；边侧上报失败静默跳过即可）。
func (s *Server) handleUplinkReport(c *conn, m *protocol.Message) {
	if !c.registered.Load() {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeNotRegistered, Message: "节点未注册，拒绝 UplinkReport"})
		return
	}
	var r UplinkReportPayload
	if err := m.DecodePayload(&r); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "UplinkReport payload 解析失败: " + err.Error()})
		return
	}
	if r.Depth < 0 || r.Dropped < 0 || r.Sent < 0 {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "UplinkReport payload 字段为负"})
		return
	}
	s.notifyUplinkReport(m.Source, r)
	log.Infof("收到节点 %s 的 UplinkReport: 积压 %d（丢弃 %d，已上送 %d）",
		m.Source, r.Depth, r.Dropped, r.Sent)
}
