// 媒资上传接收（v0.43.0 云端侧，spec 0016 US-1）：MediaUpload 消息的接收与
// 分发。
//
// 与 AlarmEvent 同构：CloudHub 只负责协议层校验与回调分发，不感知具体存储
// 实现——由 cmd/cloudcore 装配时注入 mediastore.PutChunk + videostream 挂接
// 的适配函数。未注册回调时 MediaUpload 静默忽略（不落日志——处置复核
// P2-4 对齐注释；对既有行为零影响）。
package cloudhub

import (
	"edgeflow/pkg/mediaup"
	"edgeflow/pkg/protocol"
)

// MediaUploadHandler 处理边侧上报的 MediaUpload 分片消息（依赖注入，v0.43.0）。
//
// 并发与性能约定（与 AlarmEventHandler 一致）：回调在 CloudHub 内部锁之外、
// 连接处理 goroutine 中同步调用；实现方应尽快返回（分片落盘为小块文件写）。
type MediaUploadHandler func(nodeID string, up mediaup.UploadChunk)

// SetMediaUploadHandler 注册 MediaUpload 消息回调（nil 表示取消）。
func (s *Server) SetMediaUploadHandler(h MediaUploadHandler) {
	s.mu.Lock()
	s.mediaUploadHandler = h
	s.mu.Unlock()
}

// notifyMediaUpload 在锁外安全地调用 MediaUpload 回调（先快照后执行）。
func (s *Server) notifyMediaUpload(nodeID string, up mediaup.UploadChunk) {
	s.mu.RLock()
	h := s.mediaUploadHandler
	s.mu.RUnlock()
	if h != nil {
		h(nodeID, up)
	}
}

// handleMediaUpload 处理边侧上报的 MediaUpload 分片消息：
//   - 未注册连接上报 → 回 not_registered Ack 拒绝；
//   - payload 解析失败 / 校验失败 → 回 invalid_message Ack；
//   - 校验通过 → 调用注入回调（锁外），不另行回 Ack（与 AlarmEvent 一致的单向
//     流式语义；上传完成语义由云端重组后的状态体现——重发由分片幂等消化）。
func (s *Server) handleMediaUpload(c *conn, m *protocol.Message) {
	if !c.registered.Load() {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeNotRegistered, Message: "节点未注册，拒绝 MediaUpload"})
		return
	}
	var up mediaup.UploadChunk
	if err := m.DecodePayload(&up); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "MediaUpload payload 解析失败: " + err.Error()})
		return
	}
	// nodeID 以连接注册身份为准（防伪造 Source 字段）。
	up.NodeID = m.Source
	if err := up.Validate(); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "MediaUpload 校验失败: " + err.Error()})
		return
	}
	s.notifyMediaUpload(m.Source, up)
}
