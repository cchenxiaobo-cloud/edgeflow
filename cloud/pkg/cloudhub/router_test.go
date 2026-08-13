// 路由层测试（WBS 4.3）：SendToNode / Broadcast / Deliver 的行为与并发安全。
// 复用 server_test.go 中的辅助函数（newTestServer/dial/register/readMsg/waitFor）。
//
// 注意：gorilla/websocket 的超时读会永久毒化连接（readErr 缓存，hideTempErr 把
// 临时超时转成永久错误），因此 assertNoMessage 只能放在该连接最后一次读取之后。
package cloudhub

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"edgeflow/pkg/protocol"

	"github.com/gorilla/websocket"
)

// podSyncMsg 构造一条 PodSync 测试消息（构造失败直接终止测试；
// 注意：内部含 t.Fatalf，只能在测试 goroutine 中调用，不能在子 goroutine 里用）。
func podSyncMsg(t *testing.T, target string) *protocol.Message {
	t.Helper()
	m, err := protocol.NewMessage(protocol.TypePodSync, "cloud", target,
		map[string]string{"pod": "nginx", "ns": "default"})
	if err != nil {
		t.Fatalf("构造 PodSync 消息失败: %v", err)
	}
	return m
}

// assertNoMessage 断言在超时时间内没有消息到达（用于验证路由隔离）。
// ⚠️ 调用后该连接不可再读（gorilla 超时毒化），只能作为连接的收尾检查。
func assertNoMessage(t *testing.T, ws *websocket.Conn, d time.Duration) {
	t.Helper()
	if err := ws.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("期望没有消息到达，却收到了消息")
	}
}

// TestSendToNodeDelivers 验证单播成功：消息经写循环送达目标节点，字段原样透传。
func TestSendToNodeDelivers(t *testing.T) {
	srv, url := newTestServer(t)
	ws := dial(t, url)
	register(t, ws, "edge-001")

	msg := podSyncMsg(t, "edge-001")
	if err := srv.SendToNode("edge-001", msg); err != nil {
		t.Fatalf("SendToNode 返回错误: %v", err)
	}
	got := readMsg(t, ws)
	if got.Type != protocol.TypePodSync {
		t.Errorf("type = %q, want %q", got.Type, protocol.TypePodSync)
	}
	if got.Source != "cloud" {
		t.Errorf("source = %q, want cloud", got.Source)
	}
	if got.Target != "edge-001" {
		t.Errorf("target = %q, want edge-001", got.Target)
	}
	if got.ID != msg.ID {
		t.Errorf("id = %q, want %q（消息应原样透传）", got.ID, msg.ID)
	}
	var payload map[string]string
	if err := got.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 payload 失败: %v", err)
	}
	if payload["pod"] != "nginx" {
		t.Errorf("payload.pod = %q, want nginx", payload["pod"])
	}
}

// TestSendToNodeFillsDefaults 验证 Source/Target 为空时按云→边约定补全，
// 且补全发生在拷贝上，不修改调用方传入的消息。
func TestSendToNodeFillsDefaults(t *testing.T) {
	srv, url := newTestServer(t)
	ws := dial(t, url)
	register(t, ws, "edge-001")

	msg, err := protocol.NewMessage(protocol.TypePodSync, "", "", map[string]string{"pod": "nginx"})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	if err := srv.SendToNode("edge-001", msg); err != nil {
		t.Fatalf("SendToNode 返回错误: %v", err)
	}
	got := readMsg(t, ws)
	if got.Source != "cloud" {
		t.Errorf("source = %q, want cloud（应自动补全）", got.Source)
	}
	if got.Target != "edge-001" {
		t.Errorf("target = %q, want edge-001（应自动补全）", got.Target)
	}
	// 调用方的消息不应被修改
	if msg.Source != "" || msg.Target != "" {
		t.Errorf("原消息被修改: source=%q target=%q", msg.Source, msg.Target)
	}
}

// TestSendToNodeOffline 验证离线语义：未注册与断线两种场景都返回 ErrNodeOffline。
func TestSendToNodeOffline(t *testing.T) {
	srv, url := newTestServer(t)

	// 1) 节点从未注册
	if err := srv.SendToNode("ghost", podSyncMsg(t, "ghost")); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("未注册节点期望 ErrNodeOffline，得到: %v", err)
	}

	// 2) 节点注册后断开：从注册表移除后同样返回 ErrNodeOffline
	ws := dial(t, url)
	register(t, ws, "edge-001")
	_ = ws.Close()
	waitFor(t, 2*time.Second, func() bool { return srv.NodeCount() == 0 }, "节点断开后应从注册表移除")
	if err := srv.SendToNode("edge-001", podSyncMsg(t, "edge-001")); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("断线节点期望 ErrNodeOffline，得到: %v", err)
	}
}

// TestBroadcast 验证广播：全部已注册节点都收到；无节点在线时返回 0 而非报错。
func TestBroadcast(t *testing.T) {
	srv, url := newTestServer(t)
	ids := []string{"edge-001", "edge-002", "edge-003"}
	conns := make([]*websocket.Conn, 0, len(ids))
	for _, id := range ids {
		ws := dial(t, url)
		register(t, ws, id)
		conns = append(conns, ws)
	}

	msg := podSyncMsg(t, TargetBroadcast)
	if n := srv.Broadcast(msg); n != len(ids) {
		t.Fatalf("Broadcast 成功数 = %d, want %d", n, len(ids))
	}
	for i, ws := range conns {
		got := readMsg(t, ws)
		if got.Target != TargetBroadcast {
			t.Errorf("节点 %s 收到 target = %q, want %q", ids[i], got.Target, TargetBroadcast)
		}
		if got.Source != "cloud" {
			t.Errorf("节点 %s 收到 source = %q, want cloud", ids[i], got.Source)
		}
		if got.ID != msg.ID {
			t.Errorf("节点 %s 收到 id = %q, want %q（广播应同一条消息）", ids[i], got.ID, msg.ID)
		}
	}

	// 无节点在线：返回 0，不算错误
	srvEmpty, _ := newTestServer(t)
	if n := srvEmpty.Broadcast(podSyncMsg(t, TargetBroadcast)); n != 0 {
		t.Errorf("无节点时 Broadcast 成功数 = %d, want 0", n)
	}
}

// TestDeliverRoutes 验证统一路由入口：单播只达目标、广播达全部、
// 空 Target 拒绝、未知节点报 ErrNodeOffline。
//
// 顺序说明：单播不串扰的证明放在广播读取里——若单播误发给 ws2，
// ws2 收到的第一条消息就会是 target=edge-001 而非 "*"。
// assertNoMessage 放在最后（gorilla 超时读会毒化连接，之后不可再读）。
func TestDeliverRoutes(t *testing.T) {
	srv, url := newTestServer(t)
	ws1 := dial(t, url)
	register(t, ws1, "edge-001")
	ws2 := dial(t, url)
	register(t, ws2, "edge-002")

	// 单播：目标节点收到
	if err := srv.Deliver(podSyncMsg(t, "edge-001")); err != nil {
		t.Fatalf("Deliver 单播返回错误: %v", err)
	}
	if got := readMsg(t, ws1); got.Target != "edge-001" {
		t.Errorf("单播 target = %q, want edge-001", got.Target)
	}

	// 广播：所有节点收到；同时 ws2 的第一条消息必须是广播
	// （若单播误发 ws2，这里会先读到 target=edge-001 而失败）
	if err := srv.Deliver(podSyncMsg(t, TargetBroadcast)); err != nil {
		t.Fatalf("Deliver 广播返回错误: %v", err)
	}
	for i, ws := range []*websocket.Conn{ws1, ws2} {
		if got := readMsg(t, ws); got.Target != TargetBroadcast {
			t.Errorf("节点 %d 收到 target = %q, want %q", i+1, got.Target, TargetBroadcast)
		}
	}

	// Target 为空：拒绝路由
	if err := srv.Deliver(podSyncMsg(t, "")); !errors.Is(err, ErrEmptyTarget) {
		t.Fatalf("空 Target 期望 ErrEmptyTarget，得到: %v", err)
	}

	// 节点不存在：ErrNodeOffline
	if err := srv.Deliver(podSyncMsg(t, "ghost")); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("未知节点期望 ErrNodeOffline，得到: %v", err)
	}

	// 收尾：两条连接都不应再有消息（gorilla 超时毒化后不再读取）
	assertNoMessage(t, ws1, 200*time.Millisecond)
	assertNoMessage(t, ws2, 200*time.Millisecond)
}

// TestConcurrentRouting 并发混合发送（单播/广播/Deliver），配合 -race 验证：
//   - 发送路径并发安全（写循环串行写出，无直接写连接）
//   - 注册表读（路由查询/快照）与写（注册/注销）并发安全
//   - 并发复用同一消息对象时不被修改
//
// 节奏说明：发送带 1ms 节流。不节流时 8 个 goroutine 的突发会在几毫秒内灌满
// 连接发送缓冲（64 条），触发 trySend 的慢客户端保护（关闭连接）——那是服务端
// 既有设计，不代表路由缺陷；真实控制器不会以该速率突发。
func TestConcurrentRouting(t *testing.T) {
	srv, url := newTestServer(t)

	const (
		senders    = 8  // 并发发送 goroutine 数
		iterations = 50 // 每个 goroutine 的发送次数
	)
	stable := []string{"edge-001", "edge-002", "edge-003"}

	// 期望计数：按调度规律精确模拟（单播只到目标节点，广播到所有节点）。
	// 不硬编码数字，避免与测试逻辑脱节。
	expected := make([]int, len(stable))
	for g := 0; g < senders; g++ {
		for i := 0; i < iterations; i++ {
			target := stable[(g+i)%len(stable)]
			switch i % 3 {
			case 0, 1: // 单播：只发给目标节点
				for j, id := range stable {
					if id == target {
						expected[j]++
					}
				}
			default: // 广播：发给所有稳定节点
				for j := range stable {
					expected[j]++
				}
			}
		}
	}

	// 稳定节点：各配一个后台读协程，统计收到的消息数并做基本校验
	var counts [3]atomic.Int64
	var readErrs atomic.Int64
	stableWS := make([]*websocket.Conn, 0, len(stable))
	for i, id := range stable {
		ws := dial(t, url)
		register(t, ws, id)
		stableWS = append(stableWS, ws)
		go func(i int, id string, ws *websocket.Conn) {
			for {
				_, data, err := ws.ReadMessage()
				if err != nil {
					return // 测试收尾主动关闭连接，正常退出
				}
				m, err := protocol.Decode(data)
				if err != nil {
					readErrs.Add(1)
					continue
				}
				// 稳定节点只会收到广播（Target="*"）或发给自己的单播
				if m.Source != "cloud" || m.Type != protocol.TypePodSync ||
					(m.Target != TargetBroadcast && m.Target != id) {
					readErrs.Add(1)
					continue
				}
				counts[i].Add(1)
			}
		}(i, id, ws)
	}

	// 瞬态节点 edge-004：注册后中途断开，制造「路由读注册表 vs 注销写」竞争。
	// 它不参与稳定节点的期望计数；向它发送允许 ErrNodeOffline。
	transientWS := dial(t, url)
	register(t, transientWS, "edge-004")
	go func() {
		for {
			if _, _, err := transientWS.ReadMessage(); err != nil {
				return
			}
		}
	}()
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = transientWS.Close()
	}()

	// 预构造消息（podSyncMsg 内含 t.Fatalf，不能在子 goroutine 中调用）
	unicast := make(map[string]*protocol.Message, len(stable)+1)
	for _, id := range append(append([]string{}, stable...), "edge-004") {
		unicast[id] = podSyncMsg(t, id)
	}
	broadcastMsg := podSyncMsg(t, TargetBroadcast)

	// 并发发送：单播 / Deliver 单播 / 广播 轮流，带节流
	var sendWg sync.WaitGroup
	for g := 0; g < senders; g++ {
		sendWg.Add(1)
		go func(g int) {
			defer sendWg.Done()
			for i := 0; i < iterations; i++ {
				target := stable[(g+i)%len(stable)]
				var err error
				switch i % 3 {
				case 0:
					err = srv.SendToNode(target, unicast[target])
				case 1:
					err = srv.Deliver(unicast[target])
				default:
					srv.Broadcast(broadcastMsg)
				}
				if err != nil {
					t.Errorf("发送失败（target=%s）: %v", target, err)
				}
				time.Sleep(time.Millisecond)
			}
		}(g)
	}

	// 另起一个 goroutine 持续向瞬态节点发送（允许其离线错误）
	var transientWg sync.WaitGroup
	transientWg.Add(1)
	go func() {
		defer transientWg.Done()
		for i := 0; i < iterations; i++ {
			if err := srv.Deliver(unicast["edge-004"]); err != nil && !errors.Is(err, ErrNodeOffline) {
				t.Errorf("向瞬态节点发送失败: %v", err)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	sendWg.Wait()
	transientWg.Wait()

	// 每个稳定节点的实收数必须与期望完全一致
	for i, id := range stable {
		want := int64(expected[i])
		waitFor(t, 3*time.Second, func() bool { return counts[i].Load() == want },
			fmt.Sprintf("节点 %s 应收到 %d 条消息，实际 %d", id, want, counts[i].Load()))
	}
	if n := readErrs.Load(); n != 0 {
		t.Errorf("%d 条消息未通过字段校验", n)
	}

	// 收尾：关闭稳定连接，让读协程退出
	for _, ws := range stableWS {
		_ = ws.Close()
	}
}
