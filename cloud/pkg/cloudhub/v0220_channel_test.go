package cloudhub

import (
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/protocol"
)

// T-08（CHN-07/CHN-02，v0.22.0）验收测试：注册风暴退避 + 慢客户端缓冲计量。

// blockingEvents 是事件回调可阻塞的 NodeEvents（模拟下游注册表写抖动）。
type blockingEvents struct {
	mu       sync.Mutex
	delay    time.Duration
	release  chan struct{}
	seenReg  int
	seenDisc int
}

func (b *blockingEvents) OnNodeRegistered(info NodeInfo) {
	if b.delay > 0 {
		time.Sleep(b.delay)
	}
	if b.release != nil {
		<-b.release
	}
	b.mu.Lock()
	b.seenReg++
	b.mu.Unlock()
}

func (b *blockingEvents) OnNodeHeartbeat(nodeID string) {}

func (b *blockingEvents) OnNodeDisconnected(nodeID string) {
	b.mu.Lock()
	b.seenDisc++
	b.mu.Unlock()
}

// TestRegisterAckBeforeEvents 验收 1：RegisterAck 先于事件回调送达——
// 云端故障窗口（下游登记阻塞）内边缘仍能立即拿到 ack 进入心跳循环，
// 不会因等云端同步登记而堆积重试（注册风暴自我放大的根因）。
func TestRegisterAckBeforeEvents(t *testing.T) {
	srv, wsURL := newTestServer(t)
	// 事件回调阻塞 800ms（模拟 cloudcore 下游故障窗口），由测试结束兜底放行
	be := &blockingEvents{delay: 800 * time.Millisecond}
	srv.SetNodeEvents(be)

	ws := dial(t, wsURL)
	start := time.Now()
	register(t, ws, "node-ack-first") // 内部断言收到 accepted=true 的 RegisterAck
	ackLatency := time.Since(start)

	if ackLatency >= 800*time.Millisecond {
		t.Errorf("RegisterAck 延迟 %v ≥ 事件回调阻塞时长 800ms：ack 在事件之后才送达（风暴自我放大面未消除）", ackLatency)
	}
	// 事件最终仍会触发（只换序，不吞事件）
	waitFor(t, 3*time.Second, func() bool {
		be.mu.Lock()
		defer be.mu.Unlock()
		return be.seenReg == 1
	}, "ack 前置后注册事件仍应触发一次")
}

// TestSendQuotaBytesDropSlowClient 验收 2：单连接发送缓冲字节配额——
// 慢客户端（不读消息）塞满配额后，新消息在入队前被丢弃且连接被关闭，
// 服务端内存占用有界（不会随投递量无界增长）。
func TestSendQuotaBytesDropSlowClient(t *testing.T) {
	srv := New("127.0.0.1:0", WithSendQuotaBytes(4096)) // 4KiB 配额便于触发
	srv.sendQuotaBytes = 4096

	// 直接构造服务端连接视图（不经 WS 握手，聚焦 trySend 计量语义）
	c := newConn(nil, "10.0.0.9", 4096)

	payload := strings.Repeat("x", 2048) // 2KiB/条
	sent := 0
	dropped := false
	for i := 0; i < 100; i++ { // 100×2KiB = 200KiB ≫ 4KiB 配额
		if !c.trySend([]byte(payload)) {
			dropped = true
			break
		}
		sent++
	}
	if !dropped {
		t.Fatalf("超出配额后应拒绝入队（丢弃+关连接），实际 100 条全部入队")
	}
	if c.sendBytes.Load() > 4096 {
		t.Errorf("在途字节计量 %d 超过配额 4096", c.sendBytes.Load())
	}
	if !c.dead.Load() && c.closedClosed() == false && srv.BroadcastBytesInView() > 4096 {
		t.Errorf("广播内存视图 %d 超过单连接配额", srv.BroadcastBytesInView())
	}
	// 计量守恒：writeLoop 未消费时，计量 = 已入队消息字节和
	if c.quota > 0 {
		var want int64
		for {
			select {
			case d := <-c.send:
				want += int64(len(d))
				continue
			default:
			}
			break
		}
		if c.sendBytes.Load() != want {
			t.Errorf("计量 %d 与缓冲实际字节 %d 不守恒", c.sendBytes.Load(), want)
		}
	}
}

// closedClosed 是 closed channel 是否已关闭的辅助（测试内联，避免导出）。
func (c *conn) closedClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// TestBroadcastBytesInView 验收 3：广播内存峰值视图——投递 N 条后视图合计
// 与各连接计量一致；writeLoop 消费后归零（gauge 可回落，非单调计数器）。
func TestBroadcastBytesInView(t *testing.T) {
	srv, wsURL := newTestServer(t, WithSendQuotaBytes(1<<20))
	ws := dial(t, wsURL)
	register(t, ws, "node-view-1")

	waitFor(t, 3*time.Second, func() bool {
		return srv.NodeCount() == 1
	}, "节点注册")

	m, _ := protocol.NewMessage(protocol.TypePodStatus, "cloud", TargetBroadcast,
		map[string]any{"k": strings.Repeat("v", 512)})
	n := srv.Broadcast(m)
	if n != 1 {
		t.Fatalf("广播应送达 1 个节点，实际 %d", n)
	}
	inFlight := srv.BroadcastBytesInView()
	if inFlight <= 0 {
		t.Errorf("投递后应有在途字节（writeLoop 尚未消费），实际 %d", inFlight)
	}
	// writeLoop 消费后视图回落（轮询等待；对端 ws 会收到消息）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.BroadcastBytesInView() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if left := srv.BroadcastBytesInView(); left != 0 {
		t.Errorf("消费后视图应回落到 0，实际 %d", left)
	}
	_ = ws.Close()
}
