package cloudhub

import (
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/protocol"
)

// fakePodStatusHandler 是记录型 PodStatus 回调（并发安全），用于断言触发内容。
type fakePodStatusHandler struct {
	mu    sync.Mutex
	calls []PodStatusPayload
}

func (f *fakePodStatusHandler) handle(nodeID string, ps PodStatusPayload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ps.NodeID = nodeID
	f.calls = append(f.calls, ps)
}

func (f *fakePodStatusHandler) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePodStatusHandler) first() PodStatusPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return PodStatusPayload{}
	}
	return f.calls[0]
}

// TestPodStatusHandlerInvoked 验证链路：已注册节点上报 PodStatus →
// 校验通过 → 注入的回调被调用（携带 Source 作为 nodeID）。
func TestPodStatusHandlerInvoked(t *testing.T) {
	srv, wsURL := newTestServer(t)
	handler := &fakePodStatusHandler{}
	srv.SetPodStatusHandler(handler.handle)

	ws := dial(t, wsURL)
	const nodeID = "edge-pod-1"
	register(t, ws, nodeID)

	// 契约示例：payload.nodeID 与 Source 一致
	msg, err := protocol.NewMessage(protocol.TypePodStatus, nodeID, "cloud",
		PodStatusPayload{
			NodeID:          nodeID,
			PodName:         "web-demo",
			Namespace:       "default",
			Phase:           "Running",
			Message:         "",
			LastReconcileAt: 1755168000000,
		})
	if err != nil {
		t.Fatalf("构造 PodStatus 消息失败: %v", err)
	}
	sendMsg(t, ws, msg)

	waitFor(t, 3*time.Second, func() bool { return handler.count() == 1 }, "回调应被调用一次")
	got := handler.first()
	if got.NodeID != nodeID || got.PodName != "web-demo" || got.Namespace != "default" ||
		got.Phase != "Running" || got.LastReconcileAt != 1755168000000 {
		t.Errorf("回调内容不符: %+v", got)
	}
}

// TestPodStatusNodeIDFromSource 验证 payload.nodeID 缺省时以消息 Source 为准。
func TestPodStatusNodeIDFromSource(t *testing.T) {
	srv, wsURL := newTestServer(t)
	handler := &fakePodStatusHandler{}
	srv.SetPodStatusHandler(handler.handle)

	ws := dial(t, wsURL)
	const nodeID = "edge-pod-2"
	register(t, ws, nodeID)

	msg, err := protocol.NewMessage(protocol.TypePodStatus, nodeID, "cloud",
		PodStatusPayload{PodName: "web-demo", Phase: "Running"})
	if err != nil {
		t.Fatalf("构造 PodStatus 消息失败: %v", err)
	}
	sendMsg(t, ws, msg)

	waitFor(t, 3*time.Second, func() bool { return handler.count() == 1 }, "回调应被调用一次")
	if got := handler.first(); got.NodeID != nodeID {
		t.Errorf("nodeID = %q，期望取消息 Source %q", got.NodeID, nodeID)
	}
}

// TestPodStatusRejectedCases 验证非法上报被拒绝且不触发回调：
//   - 未注册节点上报 → not_registered Ack
//   - payload 缺 podName → invalid_message Ack
//   - payload.nodeID 与 Source 不一致 → invalid_message Ack
func TestPodStatusRejectedCases(t *testing.T) {
	srv, wsURL := newTestServer(t)
	handler := &fakePodStatusHandler{}
	srv.SetPodStatusHandler(handler.handle)

	// 场景 1：未注册连接直接上报
	wsUnregistered := dial(t, wsURL)
	m, err := protocol.NewMessage(protocol.TypePodStatus, "edge-noreg", "cloud",
		PodStatusPayload{PodName: "web-demo", Phase: "Running"})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	sendMsg(t, wsUnregistered, m)
	ack := readMsg(t, wsUnregistered)
	if ack.Type != protocol.TypeAck {
		t.Fatalf("期望 Ack，收到 %s", ack.Type)
	}
	var payload AckPayload
	if err := ack.DecodePayload(&payload); err != nil || payload.Code != CodeNotRegistered {
		t.Errorf("期望 not_registered Ack，实际 %+v (err=%v)", payload, err)
	}

	// 场景 2：已注册但缺 podName
	ws := dial(t, wsURL)
	const nodeID = "edge-pod-3"
	register(t, ws, nodeID)

	m, err = protocol.NewMessage(protocol.TypePodStatus, nodeID, "cloud",
		PodStatusPayload{Namespace: "default", Phase: "Running"})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	sendMsg(t, ws, m)
	ack = readMsg(t, ws)
	var ack2 AckPayload
	if err := ack.DecodePayload(&ack2); err != nil || ack2.Code != CodeInvalidMessage {
		t.Errorf("期望 invalid_message Ack，实际 %+v (err=%v)", ack2, err)
	}

	// 场景 3：payload.nodeID 与 Source 不一致
	m, err = protocol.NewMessage(protocol.TypePodStatus, nodeID, "cloud",
		PodStatusPayload{NodeID: "edge-other", PodName: "web-demo", Phase: "Running"})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	sendMsg(t, ws, m)
	ack = readMsg(t, ws)
	var ack3 AckPayload
	if err := ack.DecodePayload(&ack3); err != nil || ack3.Code != CodeInvalidMessage {
		t.Errorf("期望 invalid_message Ack，实际 %+v (err=%v)", ack3, err)
	}

	// 三种非法上报均不应触发回调
	if n := handler.count(); n != 0 {
		t.Errorf("非法上报不应触发回调，实际触发 %d 次", n)
	}
}

// TestPodStatusNoHandlerNoPanic 验证未注册回调时 PodStatus 仅记日志、不 panic
// （向后兼容：旧装配不感知新消息类型）。
func TestPodStatusNoHandlerNoPanic(t *testing.T) {
	_, wsURL := newTestServer(t)
	ws := dial(t, wsURL)
	const nodeID = "edge-pod-4"
	register(t, ws, nodeID)

	m, err := protocol.NewMessage(protocol.TypePodStatus, nodeID, "cloud",
		PodStatusPayload{PodName: "web-demo", Phase: "Running"})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	sendMsg(t, ws, m)

	// 服务端应正常存活：心跳应答仍可用
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
}
