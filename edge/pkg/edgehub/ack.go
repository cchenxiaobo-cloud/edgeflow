// 自动 Ack 与幂等去重（WBS 4.6 可靠投递·接收侧）。
//
// 契约（与云端 ReliableSend 对齐，字段不可改）：
//   - 收到下发类消息（非 Ack 且非注册/心跳类）后，回一条 Type=TypeAck 消息，
//     CorrelationID=被确认消息的 ID，Payload={"code":"ok"|"error","message":"..."}，
//     Source=nodeID，Target="cloud"；
//   - 幂等：同一 ID 的消息只执行一次（成功处理后入缓存），云端重发同 ID
//     时直接回 Ack code=ok，不重复处理；
//   - 处理失败回 Ack code=error 且不入幂等缓存——云端重试同 ID 时允许重新执行，
//     符合 QoS 1（至少一次 + 去重）语义。
package edgehub

import (
	"strings"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// maxProcessedIDs 是幂等缓存的上限：成功处理过的消息 ID 最多保留 1000 条，
// 超出后按 FIFO 淘汰最旧。量级评估：下发类消息（PodSync/DeviceCommand）
// 在正常工况下远低于该频率；缓存仅用于"云端重发同 ID"的去重窗口，
// 而非长期状态存储（Pod 元数据本体由 MetaManager 落盘）。
const maxProcessedIDs = 1000

// maxAckMessageLen 是 Ack 错误信息写入负载的最大长度（避免超长错误文本撑爆负载）。
const maxAckMessageLen = 512

// AckPayload 是 Ack 消息的负载（与云端 ReliableSend 契约一致，字段不可改）。
type AckPayload struct {
	Code    string `json:"code"`    // "ok" 或 "error"
	Message string `json:"message"` // 附加说明（成功提示 / 错误原因）
}

// handleDownlink 处理一条下发类消息（在 readLoop 中调用）：
//  1. 幂等去重：同 ID 已成功处理过 → 直接回 Ack code=ok，不重复执行；
//  2. 分发给 msgHandler，返回 nil → 回 Ack code=ok 并记入幂等缓存；
//  3. handler 返回 error → 回 Ack code=error，不入缓存（云端可重试）。
//
// 原子性说明（P2-4）：isProcessed 检查与 markProcessed 记录之间隔着
// handler 执行，理论上非原子。正常路径下每条连接只有一个 readLoop
// goroutine 串行调用本方法，不存在并发执行；但重连窗口（旧连接关闭
// 与新连接注册交错，如注册失败快速重连）两条连接的 readLoop 可能
// 短暂并存，极端情况下云端对两条连接投递同一 msg.ID 会并发进入。
// 因此用 downlinkMu 把「检查→执行→标记」整体串行化（防御性加固，
// 成本极低），从根上消除该窗口；锁内执行 handler 是安全的——handler
// （如 MetaManager 落盘）不会反向调用本 Client 的加锁方法。
func (c *Client) handleDownlink(msg *protocol.Message) {
	c.downlinkMu.Lock()
	defer c.downlinkMu.Unlock()

	// 防御：注册/心跳类与 Ack 不参与自动 Ack（避免 Ack 套 Ack）
	switch msg.Type {
	case protocol.TypeRegister, protocol.TypeHeartbeat, protocol.TypeAck:
		return
	}

	// 幂等去重：已成功处理过的 ID 直接回 Ack，不重复执行
	if c.isProcessed(msg.ID) {
		c.sendAck(msg.ID, true, "duplicate message, already processed")
		return
	}

	c.mu.Lock()
	h := c.msgHandler
	c.mu.Unlock()
	if h == nil {
		// 无处理器：视为未处理，回 error Ack（诚实告知云端消息未被消费）
		c.sendAck(msg.ID, false, "no message handler registered")
		return
	}

	if err := h(msg); err != nil {
		// 处理失败：回 error Ack，不入幂等缓存——云端重试同 ID 时允许重新执行
		c.sendAck(msg.ID, false, err.Error())
		return
	}
	// 处理成功：记入幂等缓存后回 Ack
	c.markProcessed(msg.ID)
	c.sendAck(msg.ID, true, "ok")
}

// sendAck 回一条 Ack：CorrelationID=被确认消息的 ID，Source=nodeID，Target=cloud。
// 发送失败只记日志不回抛——Ack 本身是尽力而为，云端超时后按重试协议兜底。
func (c *Client) sendAck(correlationID string, ok bool, message string) {
	code := "ok"
	if !ok {
		code = "error"
	}
	// 截断过长的错误信息，防止异常文本撑爆负载
	if len(message) > maxAckMessageLen {
		message = message[:maxAckMessageLen]
	}
	ack, err := protocol.NewMessage(protocol.TypeAck, c.opts.NodeID, targetCloud, AckPayload{
		Code:    code,
		Message: strings.TrimSpace(message),
	})
	if err != nil {
		log.Errorf("EdgeHub 构造 Ack 失败: %v", err)
		return
	}
	ack.CorrelationID = correlationID
	if err := c.Send(ack); err != nil {
		log.Warnf("EdgeHub 回 Ack 失败（correlationID=%s）: %v", correlationID, err)
	}
}

// isProcessed 报告消息 ID 是否已成功处理过（幂等去重查询）。
func (c *Client) isProcessed(id string) bool {
	c.procMu.Lock()
	defer c.procMu.Unlock()
	_, ok := c.processed[id]
	return ok
}

// markProcessed 记录消息 ID 为已成功处理；缓存超过上限时按 FIFO 淘汰最旧。
func (c *Client) markProcessed(id string) {
	c.procMu.Lock()
	defer c.procMu.Unlock()
	if c.processed == nil {
		c.processed = make(map[string]struct{})
	}
	if _, ok := c.processed[id]; ok {
		return // 已存在：不重复入队
	}
	c.processed[id] = struct{}{}
	c.processedQ = append(c.processedQ, id)
	for len(c.processedQ) > maxProcessedIDs {
		oldest := c.processedQ[0]
		c.processedQ = c.processedQ[1:]
		delete(c.processed, oldest)
	}
}
