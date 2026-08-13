package cloudhub

import (
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/protocol"
)

// fakeEvents 是记录型 NodeEvents 实现（并发安全），用于断言事件触发顺序与内容。
type fakeEvents struct {
	mu           sync.Mutex
	registered   []NodeInfo
	heartbeats   []string
	disconnected []string
}

func (f *fakeEvents) OnNodeRegistered(info NodeInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, info)
}

func (f *fakeEvents) OnNodeHeartbeat(nodeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats = append(f.heartbeats, nodeID)
}

func (f *fakeEvents) OnNodeDisconnected(nodeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnected = append(f.disconnected, nodeID)
}

func (f *fakeEvents) disconnectedCount(nodeID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, id := range f.disconnected {
		if id == nodeID {
			n++
		}
	}
	return n
}

// TestNodeEventsLifecycle 验证事件链路：注册 → 心跳 → 断开。
func TestNodeEventsLifecycle(t *testing.T) {
	srv, wsURL := newTestServer(t)
	events := &fakeEvents{}
	srv.SetNodeEvents(events)
	ws := dial(t, wsURL)

	const nodeID = "node-events-1"
	register(t, ws, nodeID)

	// 心跳
	hb, err := protocol.NewMessage(protocol.TypeHeartbeat, nodeID, "cloud",
		HeartbeatPayload{Timestamp: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("构造心跳失败: %v", err)
	}
	sendMsg(t, ws, hb)
	ack := readMsg(t, ws)
	if ack.Type != protocol.TypeHeartbeatAck {
		t.Fatalf("期望 HeartbeatAck，收到 %s", ack.Type)
	}

	// 断开（客户端关闭连接，服务端 readLoop 触发 unregister）
	_ = ws.Close()
	waitFor(t, 3*time.Second, func() bool {
		events.mu.Lock()
		defer events.mu.Unlock()
		return len(events.registered) == 1 && len(events.heartbeats) == 1 && len(events.disconnected) == 1
	}, "注册/心跳/断开事件应各触发一次")

	events.mu.Lock()
	defer events.mu.Unlock()
	reg := events.registered[0]
	if reg.NodeID != nodeID || reg.Arch != "arm64" || reg.RemoteIP == "" {
		t.Errorf("注册事件信息不符: %+v", reg)
	}
	if events.heartbeats[0] != nodeID {
		t.Errorf("心跳事件 nodeID = %q，期望 %q", events.heartbeats[0], nodeID)
	}
	if events.disconnected[0] != nodeID {
		t.Errorf("断开事件 nodeID = %q，期望 %q", events.disconnected[0], nodeID)
	}
}

// TestNodeEventsDuplicateRegisterNoDisconnect 验证同 nodeID 重复注册（旧连接被踢）时，
// 节点仍在线，不应触发断开事件（仅新连接注册事件）。
func TestNodeEventsDuplicateRegisterNoDisconnect(t *testing.T) {
	srv, wsURL := newTestServer(t)
	events := &fakeEvents{}
	srv.SetNodeEvents(events)

	ws1 := dial(t, wsURL)
	register(t, ws1, "node-dup-1")

	// 第二条连接以同 nodeID 注册：旧连接收到 conflict Ack 后被踢
	ws2 := dial(t, wsURL)
	register(t, ws2, "node-dup-1")

	// 旧连接应收到 conflict Ack
	conflict := readMsg(t, ws1)
	if conflict.Type != protocol.TypeAck {
		t.Fatalf("旧连接应收到 Ack（conflict），实际 %s", conflict.Type)
	}
	var ack AckPayload
	if err := conflict.DecodePayload(&ack); err != nil {
		t.Fatalf("解析 Ack payload 失败: %v", err)
	}
	if ack.Code != CodeConflict {
		t.Fatalf("Ack code = %q，期望 %q", ack.Code, CodeConflict)
	}

	// 节点仍在线：注册事件 2 次（两连接各一次），断开事件 0 次
	waitFor(t, 3*time.Second, func() bool {
		events.mu.Lock()
		defer events.mu.Unlock()
		return len(events.registered) == 2
	}, "两次注册应各触发一次注册事件")
	time.Sleep(100 * time.Millisecond) // 给旧连接 unregister 留出时间窗口
	if n := events.disconnectedCount("node-dup-1"); n != 0 {
		t.Errorf("重复注册不应触发断开事件，实际 %d 次", n)
	}
}

// TestNodeEventsReRegisterNewID 验证同一连接更换 nodeID 重注册时，
// 旧 nodeID 触发断开事件（旧身份下线）。
func TestNodeEventsReRegisterNewID(t *testing.T) {
	srv, wsURL := newTestServer(t)
	events := &fakeEvents{}
	srv.SetNodeEvents(events)
	ws := dial(t, wsURL)

	register(t, ws, "node-old")
	register(t, ws, "node-new") // 同连接换 ID 重注册

	waitFor(t, 3*time.Second, func() bool {
		return events.disconnectedCount("node-old") == 1
	}, "旧 nodeID 应触发断开事件")
	if n := events.disconnectedCount("node-new"); n != 0 {
		t.Errorf("新 nodeID 不应触发断开事件，实际 %d 次", n)
	}
}

// TestNodeEventsUnset 验证未注册回调（nil）时一切照常，不 panic。
func TestNodeEventsUnset(t *testing.T) {
	srv, wsURL := newTestServer(t)
	// 不调用 SetNodeEvents
	ws := dial(t, wsURL)
	register(t, ws, "node-no-events")
	_ = ws.Close()
	waitFor(t, 3*time.Second, func() bool { return srv.NodeCount() == 0 }, "断开后节点应从注册表移除")
}
