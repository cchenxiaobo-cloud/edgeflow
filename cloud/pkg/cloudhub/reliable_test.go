// 可靠投递测试（WBS 4.6）：ReliableSend 的 Ack 确认、超时重试、同 ID 幂等重发、
// 并发安全与错误语义。复用 server_test.go 的辅助函数（newTestServer/dial/register）。
//
// mock 节点行为：后台读消息循环，按契约对非应答类消息回 Ack
// （Type=TypeAck、CorrelationID=被确认消息 ID、Source=nodeID、Target="cloud"），
// 可配置不回 Ack（模拟丢确认）或回 error 码（模拟对端处理失败）。
package cloudhub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"edgeflow/pkg/protocol"

	"github.com/gorilla/websocket"
)

// mockNode 模拟边缘节点：后台读消息并记录，可配置自动回 Ack。
type mockNode struct {
	ws       *websocket.Conn
	nodeID   string
	ackCode  string                 // 自动回 Ack 的 code（"" 表示不回 Ack）
	ackDelay time.Duration          // 回 Ack 前的延迟（模拟慢节点，0 表示立即）
	received chan *protocol.Message // 收到的全部消息（供断言）
	recvCnt  atomic.Int64           // 收到消息计数
}

// startMockNode 建立注册完成的 mock 节点并启动读循环。
func startMockNode(t *testing.T, url, nodeID, ackCode string) *mockNode {
	t.Helper()
	ws := dial(t, url)
	register(t, ws, nodeID)
	n := &mockNode{
		ws:       ws,
		nodeID:   nodeID,
		ackCode:  ackCode,
		received: make(chan *protocol.Message, 256),
	}
	go n.readLoop()
	return n
}

// readLoop 是 mock 节点的消息循环：记录收到的消息，并按契约回 Ack。
// 连接关闭（测试收尾）时读错误退出，不阻塞测试清理。
func (n *mockNode) readLoop() {
	for {
		_, data, err := n.ws.ReadMessage()
		if err != nil {
			return
		}
		m, err := protocol.Decode(data)
		if err != nil {
			continue
		}
		n.recvCnt.Add(1)
		n.received <- m
		// 按契约：边缘只对非应答类消息回 Ack（Ack/RegisterAck/HeartbeatAck 不回）
		if m.Type == protocol.TypeAck || m.Type == protocol.TypeRegisterAck || m.Type == protocol.TypeHeartbeatAck {
			continue
		}
		if n.ackCode == "" {
			continue // 配置为不回 Ack：模拟节点失联/确认丢失
		}
		if n.ackDelay > 0 {
			time.Sleep(n.ackDelay) // 模拟慢节点：延迟回 Ack
		}
		n.sendAck(m)
	}
}

// sendAck 按契约回 Ack：CorrelationID=被确认消息的 ID，Source=nodeID，Target=cloud。
func (n *mockNode) sendAck(m *protocol.Message) {
	ack, err := protocol.NewMessage(protocol.TypeAck, n.nodeID, "cloud",
		AckPayload{Code: n.ackCode, Message: "mock-node"})
	if err != nil {
		return
	}
	ack.CorrelationID = m.ID
	data, err := protocol.Encode(ack)
	if err != nil {
		return
	}
	if err := n.ws.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return
	}
	_ = n.ws.WriteMessage(websocket.TextMessage, data)
}

// drainReceived 非阻塞取出 mock 节点已记录的全部消息。
func drainReceived(n *mockNode) []*protocol.Message {
	msgs := make([]*protocol.Message, 0, n.recvCnt.Load())
	for {
		select {
		case m := <-n.received:
			msgs = append(msgs, m)
		default:
			return msgs
		}
	}
}

// TestReliableSendSuccess 正常链路：节点回 Ack(code=ok) → ReliableSend 返回 nil，
// 且只送达一次（无重试）。
func TestReliableSendSuccess(t *testing.T) {
	srv, url := newTestServer(t)
	n := startMockNode(t, url, "edge-001", "ok")

	msg := podSyncMsg(t, "edge-001")
	if err := srv.ReliableSend("edge-001", msg,
		ReliableOptions{Timeout: 500 * time.Millisecond, MaxRetries: 0}); err != nil {
		t.Fatalf("ReliableSend 返回错误: %v", err)
	}
	// ReliableSend 返回即 Ack 已消费；节点应恰好收到 1 条（无重试）
	got := drainReceived(n)
	if len(got) != 1 {
		t.Fatalf("节点收到 %d 条消息，期望 1 条", len(got))
	}
	if got[0].ID != msg.ID {
		t.Errorf("节点收到 id=%q，期望 %q", got[0].ID, msg.ID)
	}
}

// TestReliableSendTimeoutRetrySameID 超时重试链路：节点不回 Ack →
// 云端重发且保持原 msg.ID（幂等键不变，对端去重的前提）→ 重试耗尽返回 ErrAckTimeout。
func TestReliableSendTimeoutRetrySameID(t *testing.T) {
	srv, url := newTestServer(t)
	n := startMockNode(t, url, "edge-001", "") // 不回 Ack

	msg := podSyncMsg(t, "edge-001")
	err := srv.ReliableSend("edge-001", msg,
		ReliableOptions{Timeout: 80 * time.Millisecond, MaxRetries: 1})
	if !errors.Is(err, ErrAckTimeout) {
		t.Fatalf("期望 ErrAckTimeout，得到: %v", err)
	}

	// 断言 1：重发生效——节点至少收到 2 次（初始发送 + 1 次重试）
	var got []*protocol.Message
	waitFor(t, 3*time.Second, func() bool {
		got = drainReceived(n)
		return len(got) >= 2
	}, "节点应至少收到 2 次消息（初始发送 + 重试）")
	// 断言 2（关键）：重发必须保持原 msg.ID——接收方按 ID 去重的前提
	for i, m := range got {
		if m.ID != msg.ID {
			t.Errorf("第 %d 次收到的 id=%q，期望恒为 %q（重发必须同 ID）", i+1, m.ID, msg.ID)
		}
	}
}

// TestReliableSendErrorAck 对端回 Ack(code=error) → ErrAckFailed，且不重试：
// 消息已到达、处理失败，重发同样会失败，交给上层修正内容。
func TestReliableSendErrorAck(t *testing.T) {
	srv, url := newTestServer(t)
	n := startMockNode(t, url, "edge-001", "error")

	msg := podSyncMsg(t, "edge-001")
	err := srv.ReliableSend("edge-001", msg,
		ReliableOptions{Timeout: 200 * time.Millisecond, MaxRetries: 1})
	if !errors.Is(err, ErrAckFailed) {
		t.Fatalf("期望 ErrAckFailed，得到: %v", err)
	}
	if got := drainReceived(n); len(got) != 1 {
		t.Errorf("节点收到 %d 条消息，期望 1 条（error Ack 后不应重试）", len(got))
	}
}

// TestReliableSendOffline 节点不在线：立即返回 ErrNodeOffline，
// 不空等超时、不消耗重试次数（离线重发无意义）。
func TestReliableSendOffline(t *testing.T) {
	srv, _ := newTestServer(t)
	err := srv.ReliableSend("ghost", podSyncMsg(t, "ghost"),
		ReliableOptions{Timeout: time.Hour, MaxRetries: 5})
	if !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("期望 ErrNodeOffline，得到: %v", err)
	}
}

// TestReliableSendContextCancel 验证 context 取消（P2-3）：等待确认期间
// ctx 被取消 → 立即返回（错误包装 context.Canceled），不再空等至超时。
func TestReliableSendContextCancel(t *testing.T) {
	srv, url := newTestServer(t)
	startMockNode(t, url, "edge-001", "") // 不回 Ack：等待者会一直等

	msg := podSyncMsg(t, "edge-001")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ReliableSendContext(ctx, "edge-001", msg,
			ReliableOptions{Timeout: 10 * time.Second, MaxRetries: 0})
	}()

	// 确认等待已开始（发送应已投递），再取消
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("期望 context.Canceled，得到: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("context 取消后 ReliableSendContext 未及时返回")
	}
}

// TestReliableSendShutdownWakeup 验证 Shutdown 终止在途等待（P2-1）：
// 等待 Ack 期间 Shutdown → pending 通道被关闭 → 返回 ErrShuttingDown
// （而非空等至单次超时）；Shutdown 之后再调用 ReliableSend 也直接返回
// ErrShuttingDown（不尝试发送）。
func TestReliableSendShutdownWakeup(t *testing.T) {
	srv, url := newTestServer(t)
	startMockNode(t, url, "edge-001", "") // 不回 Ack

	msg := podSyncMsg(t, "edge-001")
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ReliableSend("edge-001", msg,
			ReliableOptions{Timeout: 10 * time.Second, MaxRetries: 0})
	}()

	// 等待发送进入在途等待，再触发 Shutdown
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("期望 ErrShuttingDown，得到: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown 后在途等待未及时返回 ErrShuttingDown")
	}

	// Shutdown 之后的新调用：直接返回 ErrShuttingDown，不尝试发送
	err := srv.ReliableSend("edge-001", podSyncMsg(t, "edge-001"),
		ReliableOptions{Timeout: time.Second, MaxRetries: 0})
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Shutdown 后 ReliableSend 期望 ErrShuttingDown，得到: %v", err)
	}
}

// TestReliableSendSameIDNoCrossCleanup 验证同 ID 并发在途的交叉清理（P2-1）：
// 旧等待者的条目被新等待者替换后，旧等待者超时返回时不得误删新等待者的
// pending 条目——否则新等待者即使收到 Ack 也无法匹配，只能耗尽重试报
// ErrAckTimeout。
//
// 时序（mock 节点延迟 800ms 回 Ack）：
//   - t=0    A 发起 ReliableSend(msgID=X, 超时 400ms) → 条目 A 注册
//   - t≈50ms B 发起 ReliableSend(同 msgID=X, 超时 5s) → 条目 A 被替换为条目 B
//   - t=400ms A 超时返回 ErrAckTimeout → 清理时不得删除条目 B（归属校验）
//   - t=800ms 节点回 Ack → 条目 B 收到 → B 成功返回 nil
func TestReliableSendSameIDNoCrossCleanup(t *testing.T) {
	srv, url := newTestServer(t)
	n := startMockNode(t, url, "edge-001", "ok")
	n.ackDelay = 800 * time.Millisecond

	msgA := podSyncMsg(t, "edge-001")
	msgB := podSyncMsg(t, "edge-001")
	msgB.ID = msgA.ID // 构造同 ID 并发在途（调用方错误场景，验证清理副作用被消除）

	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() {
		errA <- srv.ReliableSend("edge-001", msgA,
			ReliableOptions{Timeout: 400 * time.Millisecond, MaxRetries: 0})
	}()
	time.Sleep(50 * time.Millisecond) // 确保 A 先注册，B 后替换
	go func() {
		errB <- srv.ReliableSend("edge-001", msgB,
			ReliableOptions{Timeout: 5 * time.Second, MaxRetries: 0})
	}()

	// A：超时返回 ErrAckTimeout（其条目已被 B 替换，正常超时）
	select {
	case err := <-errA:
		if !errors.Is(err, ErrAckTimeout) {
			t.Fatalf("等待者 A 期望 ErrAckTimeout，得到: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待者 A 未在超时后返回")
	}

	// B：关键断言——A 的清理不得误删 B 的条目，B 应能收到 Ack 并成功
	select {
	case err := <-errB:
		if err != nil {
			t.Fatalf("等待者 B 期望成功收到 Ack，得到: %v（旧等待者误删了新等待者的 pending 条目）", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待者 B 未收到 Ack（交叉清理破坏了新等待者）")
	}
}

// TestReliableSendRequiresID 消息缺少 ID 直接报错：幂等键缺失不静默生成，
// 避免调用方误以为消息会被可靠投递。
func TestReliableSendRequiresID(t *testing.T) {
	srv, url := newTestServer(t)
	startMockNode(t, url, "edge-001", "ok")

	msg := podSyncMsg(t, "edge-001")
	msg.ID = ""
	if err := srv.ReliableSend("edge-001", msg, ReliableOptions{}); !errors.Is(err, ErrEmptyMsgID) {
		t.Fatalf("期望 ErrEmptyMsgID，得到: %v", err)
	}
}

// TestConcurrentReliableSend 并发可靠发送（配合 -race）：
// 多 goroutine 并行 ReliableSend，各自独立 msg.ID，节点逐条回 Ack；
// 验证 pending 表加锁、Ack 匹配与通道投递在并发下安全且不丢。
func TestConcurrentReliableSend(t *testing.T) {
	srv, url := newTestServer(t)
	n := startMockNode(t, url, "edge-001", "ok")

	const (
		senders   = 8
		perSender = 10
	)
	total := senders * perSender
	// 预构造消息（podSyncMsg 内含 t.Fatalf，只能在测试 goroutine 中调用）
	msgs := make([]*protocol.Message, 0, total)
	for i := 0; i < total; i++ {
		msgs = append(msgs, podSyncMsg(t, "edge-001"))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, total)
	for g := 0; g < senders; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perSender; i++ {
				m := msgs[g*perSender+i]
				if err := srv.ReliableSend("edge-001", m,
					ReliableOptions{Timeout: 2 * time.Second, MaxRetries: 0}); err != nil {
					errCh <- fmt.Errorf("ReliableSend(%s) 失败: %w", m.ID, err)
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// 全部送达：无重试时每条消息恰好一次，且 ID 与发送端一一对应
	var got []*protocol.Message
	waitFor(t, 3*time.Second, func() bool {
		got = drainReceived(n)
		return len(got) >= total
	}, "节点应收齐所有并发消息")
	if len(got) != total {
		t.Fatalf("节点收到 %d 条，期望 %d", len(got), total)
	}
	seen := make(map[string]int, total)
	for _, m := range got {
		seen[m.ID]++
	}
	for id, cnt := range seen {
		if cnt != 1 {
			t.Errorf("msgID=%s 收到 %d 次，期望 1 次（无重试时不应重复）", id, cnt)
		}
	}
}
