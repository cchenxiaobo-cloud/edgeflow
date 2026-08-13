// 可靠投递（WBS 4.6）：QoS 1（至少一次）+ Ack 确认 + 超时重试 + 幂等。
//
// 协议契约（与边缘侧 Agent 约定，勿单方面修改）：
//   - 边缘收到非 Ack 类消息后，回一条 Type=TypeAck、CorrelationID=被确认消息的 ID、
//     Payload={"code":"ok"|"error","message":"..."} 的消息，Source=nodeID、Target="cloud"
//   - 云端重发时保持 msg.ID 不变；接收方（边缘）按 msg.ID 去重，重复消息不重复执行
//
// 错误语义（供上层控制器判断重试策略）：
//   - ErrAckTimeout：重试耗尽仍未收到确认——消息可能已送达（边缘宕机/链路抖动），
//     上层应结合业务语义决定是否以新 ID 重试或告警
//   - ErrAckFailed：对端回 Ack 但 code="error"——消息已到达但处理失败，重发同样会失败，
//     上层应修正消息内容而不是盲目重试
//   - ErrShuttingDown：Server 正在关闭（Shutdown 已触发），在途等待被立即终止——
//     上层应停止新下发并释放资源，而不是重试
//   - 发送失败（如 ErrNodeOffline）立即返回，不消耗重试次数：节点离线时重发无意义
//
// 幂等说明：云端只保证「重发同 ID」；去重执行由接收方负责。调用方需保证同一时刻
// 在途的 ReliableSend 使用不同的 msg.ID（同 ID 并发在途属调用方错误，见 registerPending）。
package cloudhub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// 可靠投递错误（导出，供上层用 errors.Is 判断）。
var (
	// ErrAckTimeout 表示重试耗尽仍未收到确认（默认 5s 超时 × MaxRetries+1 次尝试）。
	ErrAckTimeout = errors.New("cloudhub: ack timeout after retries exhausted")
	// ErrAckFailed 表示节点回 Ack 但 code="error"（消息已到达、处理失败）。
	ErrAckFailed = errors.New("cloudhub: node rejected message with error ack")
	// ErrShuttingDown 表示 Server 正在关闭：在途等待被立即终止（P2：Shutdown 时
	// 关闭全部 pending 通道唤醒等待者），不再等待确认。
	ErrShuttingDown = errors.New("cloudhub: server shutting down")
	// ErrEmptyMsgID 表示消息缺少 ID：可靠投递依赖 ID 关联 Ack 并保证重发幂等。
	ErrEmptyMsgID = errors.New("cloudhub: reliable send requires message id")
)

// 默认可靠投递参数（ReliableOptions 零值时生效；与协议草案 §4.5 对齐）。
const (
	// DefaultAckTimeout 是单次等待确认的超时（5s）。
	DefaultAckTimeout = 5 * time.Second
	// DefaultMaxRetries 是超时后的最大重试次数（2 次重试 = 最多 3 次尝试，
	// 对应协议草案 §4.5「上限 3 次」的尝试次数口径）。
	DefaultMaxRetries = 2
)

// ReliableOptions 是 ReliableSend 的注入参数；零值使用默认值
// （Timeout=5s、MaxRetries=2），测试中可注入更小值缩短等待。
type ReliableOptions struct {
	// Timeout 是单次尝试等待确认的超时（<=0 时用默认 5s）。
	Timeout time.Duration
	// MaxRetries 是超时后的最大重试次数（<0 时用默认 2）。
	MaxRetries int
}

// withDefaults 用默认值填充零值字段。
func (o ReliableOptions) withDefaults() ReliableOptions {
	if o.Timeout <= 0 {
		o.Timeout = DefaultAckTimeout
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = DefaultMaxRetries
	}
	return o
}

// pendingEntry 是一条在途可靠消息的等待状态。
type pendingEntry struct {
	// nodeID 是期望回 Ack 的节点：校验 Ack 来源，防止其他节点的迟到 Ack 误匹配。
	nodeID string
	// ch 是收到匹配 Ack 时投递的通道。缓冲 1：Ack 可能先于 ReliableSend 的
	// select 到达（边缘处理极快），缓冲保证不丢。
	ch chan *protocol.Message
}

// ReliableSend 可靠发送（QoS 1）：发送后按 msg.ID 匹配 Ack（CorrelationID），
// 超时未确认则重发（保持原 msg.ID 以便对端幂等去重），重试耗尽返回 ErrAckTimeout。
//
// 行为细节：
//   - msg 为 nil 或缺少 ID → 返回错误（幂等依赖 ID，不自动生成，避免静默改语义）
//   - 发送失败（如节点离线）立即返回，不消耗重试次数
//   - Ack code="error" → 返回 ErrAckFailed（不再重试：消息已到达，重发无意义）
//   - Shutdown 期间在途等待被终止 → 返回 ErrShuttingDown（不再空等至超时）
//   - 并发安全：可与任意数量的 ReliableSend 并行（-race 下有测试覆盖）
//
// 旧签名，为向后兼容保留；行为等价于 ReliableSendContext(context.Background(), ...)。
// 需要取消语义（请求断开/优雅关闭）的调用方请改用 ReliableSendContext。
func (s *Server) ReliableSend(nodeID string, msg *protocol.Message, opts ReliableOptions) error {
	return s.ReliableSendContext(context.Background(), nodeID, msg, opts)
}

// ReliableSendContext 是 ReliableSend 的 context 版本（P2-3）：等待确认期间
// 监听 ctx.Done()，取消时立即返回（错误包装 ctx.Err()，可用
// errors.Is(err, context.Canceled/DeadlineExceeded) 判定），不再空等至单次超时。
// 其余语义（重试/错误分类/并发安全）与 ReliableSend 完全一致。
func (s *Server) ReliableSendContext(ctx context.Context, nodeID string, msg *protocol.Message, opts ReliableOptions) error {
	if msg == nil {
		return fmt.Errorf("cloudhub: 消息为空（nil）")
	}
	if msg.ID == "" {
		return fmt.Errorf("%w: nodeID=%s（NewMessage 会自动生成 ID）", ErrEmptyMsgID, nodeID)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	opts = opts.withDefaults()

	// 先注册等待项再发送：边缘可能在发送返回前就回 Ack（快路径），
	// 缓冲通道保证不丢；返回时无论成功失败都清理（只清自己注册的项，
	// 见 unregisterPending 的归属校验）。
	//
	// Shutdown 竞态说明：registerPending 前后各查一次 shuttingDown，
	// 与 Shutdown 的 failAllPending 构成两段式互斥——要么本等待项在
	// 注册后被 failAllPending 关闭通道唤醒（返回 ErrShuttingDown），
	// 要么在此直接返回，不存在「注册后无人唤醒、空等至超时」的窗口。
	if s.shuttingDown.Load() {
		return fmt.Errorf("%w: nodeID=%s msgID=%s", ErrShuttingDown, nodeID, msg.ID)
	}
	e := s.registerPending(nodeID, msg.ID)
	defer s.unregisterPending(msg.ID, e)
	if s.shuttingDown.Load() {
		return fmt.Errorf("%w: nodeID=%s msgID=%s", ErrShuttingDown, nodeID, msg.ID)
	}

	totalAttempts := opts.MaxRetries + 1
	for attempt := 0; attempt < totalAttempts; attempt++ {
		if attempt > 0 {
			log.Infof("重发可靠消息（第 %d/%d 次重试）: nodeID=%s msgID=%s type=%s",
				attempt, opts.MaxRetries, nodeID, msg.ID, msg.Type)
		}
		// 发送失败（节点离线/连接不可用）立即返回：离线时重发无意义，
		// 且 ErrNodeOffline 对上层比继续空等更可操作。
		if err := s.SendToNode(nodeID, msg); err != nil {
			return err
		}
		select {
		case ack := <-e.ch:
			// nil 是 Shutdown 关闭通道的信号（见 failAllPending）
			if ack == nil {
				return fmt.Errorf("%w: nodeID=%s msgID=%s", ErrShuttingDown, nodeID, msg.ID)
			}
			return ackResult(ack)
		case <-ctx.Done():
			// 调用方取消（如 HTTP 请求断开/优雅关闭）：立即返回，不再重试
			return fmt.Errorf("%w: nodeID=%s msgID=%s（等待确认期间取消）", ctx.Err(), nodeID, msg.ID)
		case <-time.After(opts.Timeout):
			// 超时未确认：进入下一轮重发（保持 msg.ID 不变，幂等依赖于此）
		}
	}
	return fmt.Errorf("%w: nodeID=%s msgID=%s type=%s（%d 次尝试均未收到确认）",
		ErrAckTimeout, nodeID, msg.ID, msg.Type, totalAttempts)
}

// ackResult 把节点的 Ack 转成错误：code="error" 返回 ErrAckFailed，
// 其余（ok/未知 code/payload 不可解析）视为确认成功——确认语义由
// 信封的 CorrelationID 匹配决定，payload 只携带附加信息。
func ackResult(ack *protocol.Message) error {
	var p AckPayload
	if err := ack.DecodePayload(&p); err != nil {
		return nil
	}
	if p.Code == "error" {
		return fmt.Errorf("%w: node=%s msgID=%s message=%q",
			ErrAckFailed, ack.Source, ack.CorrelationID, p.Message)
	}
	return nil
}

// registerPending 登记一条在途可靠消息，返回等待 Ack 的条目（含通道）。
//
// 并发约定：同 msgID 已有在途等待项时替换旧项并告警——同 ID 并发在途
// 属调用方错误（Ack 关联会串），旧等待者只能超时返回 ErrAckTimeout。
func (s *Server) registerPending(nodeID, msgID string) *pendingEntry {
	e := &pendingEntry{nodeID: nodeID, ch: make(chan *protocol.Message, 1)}
	s.ackMu.Lock()
	if _, exists := s.pending[msgID]; exists {
		log.Warnf("msgID=%s 已有在途可靠发送被替换：调用方需保证并发在途消息 ID 唯一", msgID)
	}
	s.pending[msgID] = e
	s.ackMu.Unlock()
	return e
}

// unregisterPending 清理在途项（ReliableSend/ReliableSendContext 返回时调用）。
//
// 归属校验（P2-1 交叉清理）：只删除「自己注册的」条目——同 ID 并发在途时，
// 旧等待者的条目已被新等待者替换，旧等待者返回时不得误删新等待者的条目，
// 否则新等待者即使收到 Ack 也无法匹配、只能耗尽重试报 ErrAckTimeout。
// 迟到的 Ack 会因查不到匹配项而被忽略（见 resolvePending）。
func (s *Server) unregisterPending(msgID string, e *pendingEntry) {
	s.ackMu.Lock()
	if s.pending[msgID] == e {
		delete(s.pending, msgID)
	}
	s.ackMu.Unlock()
}

// failAllPending 在 Shutdown 时终止全部在途可靠投递等待（P2-1）：
// 关闭等待通道并清空 pending 表。等待方在 select 中读到关闭信号（nil）
// 后返回 ErrShuttingDown，不再空等至单次超时（默认 5s）。
//
// 并发安全：整个关闭过程持有 ackMu——resolvePending 同样在持锁时做
// 非阻塞投递（见其注释），二者互斥，不会出现「查表取到条目后、通道
// 已被关闭」的向已关闭通道发送的竞态。
func (s *Server) failAllPending() {
	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	n := len(s.pending)
	for id, e := range s.pending {
		close(e.ch)
		delete(s.pending, id)
	}
	if n > 0 {
		log.Infof("CloudHub 关闭：已终止 %d 个在途可靠投递等待", n)
	}
}

// resolvePending 把收到的 Ack 投递给匹配的等待者（CorrelationID == 在途 msg.ID）。
// 在消息读取循环（readLoop → handleAck）中同步调用，不阻塞读循环：
//   - 无匹配等待项（普通 Ack 或迟到 Ack）→ 静默忽略，仅记日志
//   - Ack 来源与等待节点不符 → 忽略（防其他节点的迟到 Ack 误匹配）
//   - 等待通道已满（等待者已退出）→ 丢弃迟到 Ack
//
// 并发安全：查表、来源校验与投递全程持有 ackMu。投递是非阻塞的
// （select+default 永不阻塞），因此持锁不会卡死；持锁可保证与
// failAllPending 互斥——后者关闭通道时不会出现本方法正在向其发送。
func (s *Server) resolvePending(m *protocol.Message) {
	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	e := s.pending[m.CorrelationID]
	if e == nil {
		return
	}
	if e.nodeID != "" && e.nodeID != m.Source {
		log.Warnf("忽略来源不符的 Ack: msgID=%s 等待节点=%s 实际来源=%s",
			m.CorrelationID, e.nodeID, m.Source)
		return
	}
	select {
	case e.ch <- m:
	default:
		// 等待者已返回/通道已满：丢弃迟到 Ack（可能来自重发副本，边缘会按 ID 去重）
	}
}
