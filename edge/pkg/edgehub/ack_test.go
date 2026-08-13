package edgehub

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/protocol"
)

// podSyncPayload 构造一条符合契约的 PodSync 消息负载（operation + pod）。
func podSyncPayload(t *testing.T, operation string) map[string]any {
	t.Helper()
	return map[string]any{
		"operation": operation,
		"pod": map[string]any{
			"name":      "nginx",
			"namespace": "default",
			"image":     "nginx:1.25",
			"replicas":  1,
		},
	}
}

// newPodSync 构造一条发往指定节点的 PodSync 消息。
func newPodSync(t *testing.T, nodeID string, payload any) *protocol.Message {
	t.Helper()
	msg, err := protocol.NewMessage(protocol.TypePodSync, targetCloud, nodeID, payload)
	if err != nil {
		t.Fatalf("构造 PodSync 失败: %v", err)
	}
	return msg
}

// TestAutoAckOnPodSync 验证 WBS 4.6 自动 Ack：mock 云端推送一条 PodSync，
// 客户端处理后回 Ack，断言 correlationId=被确认消息的 ID、payload.code=ok、
// Source=nodeID、Target=cloud。
func TestAutoAckOnPodSync(t *testing.T) {
	mock := startMockCloud(t, mockConfig{})
	client := New(Options{CloudAddr: mock.url, NodeID: "ack-node"})

	handlerCalls := 0
	client.SetMessageHandlerFunc(func(msg *protocol.Message) error {
		handlerCalls++
		if msg.Type != protocol.TypePodSync {
			t.Errorf("handler 收到的类型 = %s，期望 PodSync", msg.Type)
		}
		return nil
	})
	client.Start()
	defer client.Stop()
	mock.waitRegister(t)

	podSync := newPodSync(t, "ack-node", podSyncPayload(t, "add"))
	if err := mock.push(podSync); err != nil {
		t.Fatalf("mock 推送 PodSync 失败: %v", err)
	}

	ack := mock.waitType(t, protocol.TypeAck)
	if ack.CorrelationID != podSync.ID {
		t.Errorf("Ack.CorrelationID = %q，期望 %q（被确认消息的 ID）", ack.CorrelationID, podSync.ID)
	}
	if ack.Source != "ack-node" {
		t.Errorf("Ack.Source = %q，期望 nodeID", ack.Source)
	}
	if ack.Target != targetCloud {
		t.Errorf("Ack.Target = %q，期望 cloud", ack.Target)
	}
	if ack.Version != protocol.Version {
		t.Errorf("Ack.Version = %q，期望 %q", ack.Version, protocol.Version)
	}
	var p AckPayload
	if err := ack.DecodePayload(&p); err != nil {
		t.Fatalf("解析 Ack 负载失败: %v", err)
	}
	if p.Code != "ok" {
		t.Errorf("Ack.code = %q，期望 ok（message=%q）", p.Code, p.Message)
	}
	if handlerCalls != 1 {
		t.Errorf("handler 调用次数 = %d，期望 1", handlerCalls)
	}
}

// TestAutoAckIdempotent 验证幂等去重：同一 ID 的 PodSync 推送两次，
// handler 只被调用一次，且两次都回 Ack code=ok（第二次直接命中缓存）。
func TestAutoAckIdempotent(t *testing.T) {
	mock := startMockCloud(t, mockConfig{})
	client := New(Options{CloudAddr: mock.url, NodeID: "idem-node"})

	var mu sync.Mutex
	calls := 0
	client.SetMessageHandlerFunc(func(msg *protocol.Message) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	})
	client.Start()
	defer client.Stop()
	mock.waitRegister(t)

	podSync := newPodSync(t, "idem-node", podSyncPayload(t, "update"))
	if err := mock.push(podSync); err != nil {
		t.Fatalf("第一次推送失败: %v", err)
	}
	// 云端重发同 ID（QoS 1 重试场景）
	if err := mock.push(podSync); err != nil {
		t.Fatalf("第二次推送失败: %v", err)
	}

	// 两次都应收到 Ack，且都回 ok
	for i := 0; i < 2; i++ {
		ack := mock.waitType(t, protocol.TypeAck)
		if ack.CorrelationID != podSync.ID {
			t.Errorf("第 %d 次 Ack.CorrelationID = %q，期望 %q", i+1, ack.CorrelationID, podSync.ID)
		}
		var p AckPayload
		if err := ack.DecodePayload(&p); err != nil {
			t.Fatalf("解析 Ack 负载失败: %v", err)
		}
		if p.Code != "ok" {
			t.Errorf("第 %d 次 Ack.code = %q，期望 ok（message=%q）", i+1, p.Code, p.Message)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("handler 调用次数 = %d，期望 1（同 ID 只处理一次）", calls)
	}
}

// TestAutoAckHandlerError 验证：handler 返回 error 时回 Ack code=error，
// 且不入幂等缓存——云端重试同 ID 时允许重新执行；重试成功后回 ok。
func TestAutoAckHandlerError(t *testing.T) {
	mock := startMockCloud(t, mockConfig{})
	client := New(Options{CloudAddr: mock.url, NodeID: "err-node"})

	calls := 0
	client.SetMessageHandlerFunc(func(msg *protocol.Message) error {
		calls++
		if calls == 1 {
			return errors.New("模拟处理失败")
		}
		return nil
	})
	client.Start()
	defer client.Stop()
	mock.waitRegister(t)

	podSync := newPodSync(t, "err-node", podSyncPayload(t, "add"))
	if err := mock.push(podSync); err != nil {
		t.Fatalf("第一次推送失败: %v", err)
	}

	// 第一次：处理失败 → Ack code=error
	ack1 := mock.waitType(t, protocol.TypeAck)
	if ack1.CorrelationID != podSync.ID {
		t.Errorf("Ack1.CorrelationID = %q，期望 %q", ack1.CorrelationID, podSync.ID)
	}
	var p1 AckPayload
	if err := ack1.DecodePayload(&p1); err != nil {
		t.Fatalf("解析 Ack1 负载失败: %v", err)
	}
	if p1.Code != "error" {
		t.Errorf("Ack1.code = %q，期望 error（message=%q）", p1.Code, p1.Message)
	}

	// 云端重试同 ID：应重新执行（不入缓存）
	if err := mock.push(podSync); err != nil {
		t.Fatalf("第二次推送失败: %v", err)
	}
	ack2 := mock.waitType(t, protocol.TypeAck)
	var p2 AckPayload
	if err := ack2.DecodePayload(&p2); err != nil {
		t.Fatalf("解析 Ack2 负载失败: %v", err)
	}
	if p2.Code != "ok" {
		t.Errorf("Ack2.code = %q，期望 ok（重试成功后）（message=%q）", p2.Code, p2.Message)
	}
	if calls != 2 {
		t.Errorf("handler 调用次数 = %d，期望 2（失败后重试应重新执行）", calls)
	}
}

// TestAutoAckNoHandler 验证：未注册 handler 时回 Ack code=error
// （诚实告知云端消息未被消费，云端可据此处理而非误认为已生效）。
func TestAutoAckNoHandler(t *testing.T) {
	mock := startMockCloud(t, mockConfig{})
	client := New(Options{CloudAddr: mock.url, NodeID: "nohandler-node"})
	client.Start()
	defer client.Stop()
	mock.waitRegister(t)

	podSync := newPodSync(t, "nohandler-node", podSyncPayload(t, "add"))
	if err := mock.push(podSync); err != nil {
		t.Fatalf("mock 推送失败: %v", err)
	}
	ack := mock.waitType(t, protocol.TypeAck)
	var p AckPayload
	if err := ack.DecodePayload(&p); err != nil {
		t.Fatalf("解析 Ack 负载失败: %v", err)
	}
	if p.Code != "error" {
		t.Errorf("Ack.code = %q，期望 error（无 handler 时不应回 ok）", p.Code)
	}
}

// TestSend 验证 Send 方法：连接建立后向上发送消息，mock 云端能收到
// 且信封字段保持原样（ID/Type/Source/Target）。
func TestSend(t *testing.T) {
	mock := startMockCloud(t, mockConfig{})
	client := New(Options{CloudAddr: mock.url, NodeID: "send-node"})
	client.Start()
	defer client.Stop()
	mock.waitRegister(t)

	msg, err := protocol.NewMessage(protocol.TypePodStatus, "send-node", targetCloud,
		map[string]any{"pod": "nginx", "phase": "Running"})
	if err != nil {
		t.Fatalf("构造 PodStatus 失败: %v", err)
	}
	if err := client.Send(msg); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	got := mock.waitType(t, protocol.TypePodStatus)
	if got.ID != msg.ID {
		t.Errorf("云端收到消息 ID = %q，期望 %q", got.ID, msg.ID)
	}
	if got.Type != protocol.TypePodStatus || got.Source != "send-node" || got.Target != targetCloud {
		t.Errorf("信封字段被篡改: type/source/target = %q/%q/%q",
			got.Type, got.Source, got.Target)
	}
}

// TestSendNotConnected 验证：未连接（未 Start）时 Send 返回错误。
func TestSendNotConnected(t *testing.T) {
	client := New(Options{CloudAddr: "ws://127.0.0.1:1", NodeID: "x"})
	msg, err := protocol.NewMessage(protocol.TypePodStatus, "x", targetCloud, map[string]any{})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	if err := client.Send(msg); err == nil {
		t.Fatal("未连接时 Send 应返回错误")
	}
}

// TestProcessedCacheEviction 验证幂等缓存上限：超过 maxProcessedIDs 后
// 按 FIFO 淘汰最旧 ID，最新 ID 保留（防止缓存无限增长）。
func TestProcessedCacheEviction(t *testing.T) {
	client := New(Options{CloudAddr: "ws://127.0.0.1:1", NodeID: "x"})

	// 插入超过上限的 ID
	for i := 0; i < maxProcessedIDs+5; i++ {
		client.markProcessed(fmt.Sprintf("id-%d", i))
	}
	// 最旧的 5 个应被淘汰
	for i := 0; i < 5; i++ {
		if client.isProcessed(fmt.Sprintf("id-%d", i)) {
			t.Errorf("id-%d 应已被淘汰（FIFO 超限）", i)
		}
	}
	// 第 5 个及之后应仍在缓存中
	if !client.isProcessed("id-5") {
		t.Error("id-5 不应被淘汰")
	}
	if !client.isProcessed(fmt.Sprintf("id-%d", maxProcessedIDs+4)) {
		t.Error("最新的 ID 应仍在缓存中")
	}
	client.procMu.Lock()
	queueLen := len(client.processedQ)
	mapLen := len(client.processed)
	client.procMu.Unlock()
	if queueLen != maxProcessedIDs || mapLen != maxProcessedIDs {
		t.Errorf("缓存长度 = queue %d / map %d，期望均 %d", queueLen, mapLen, maxProcessedIDs)
	}
}

// TestAutoAckIgnoresAckAndHeartbeat 验证：云端发来的 Ack 与 Heartbeat
// 不会触发自动 Ack（不会出现 Ack 套 Ack / 心跳被误 Ack）。
// 断言方式：推送后等一个窗口，检查客户端回给云端的所有 Ack 中
// 不存在 CorrelationID 指向被推送消息 ID 的（即没有自动 Ack 回声）。
func TestAutoAckIgnoresAckAndHeartbeat(t *testing.T) {
	mock := startMockCloud(t, mockConfig{})
	client := New(Options{CloudAddr: mock.url, NodeID: "guard-node"})
	client.SetMessageHandlerFunc(func(msg *protocol.Message) error {
		t.Errorf("handler 不应收到 %s 类型消息", msg.Type)
		return nil
	})
	client.Start()
	defer client.Stop()
	mock.waitRegister(t)

	// 云端推送一条 Ack（模拟对边侧上报的确认）
	ack, err := protocol.NewMessage(protocol.TypeAck, targetCloud, "guard-node", AckPayload{Code: "ok"})
	if err != nil {
		t.Fatalf("构造 Ack 失败: %v", err)
	}
	ack.CorrelationID = "some-msg-id"
	if err := mock.push(ack); err != nil {
		t.Fatalf("mock 推送 Ack 失败: %v", err)
	}
	// 云端推送一条 Heartbeat（异常场景：云端不应发 Heartbeat，防御测试）
	hb, err := protocol.NewMessage(protocol.TypeHeartbeat, targetCloud, "guard-node", HeartbeatPayload{Timestamp: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("构造 Heartbeat 失败: %v", err)
	}
	if err := mock.push(hb); err != nil {
		t.Fatalf("mock 推送 Heartbeat 失败: %v", err)
	}

	// 给读循环留出处理窗口（若误触发自动 Ack，此时已发回）
	time.Sleep(300 * time.Millisecond)

	// 清点客户端回给云端的所有消息：不得出现指向被推送消息的自动 Ack
	for {
		select {
		case m := <-mock.received:
			if m.Type == protocol.TypeAck && (m.CorrelationID == ack.ID || m.CorrelationID == hb.ID) {
				t.Fatalf("Ack/Heartbeat 不应触发自动 Ack（收到指向 %s 的回声）", m.CorrelationID)
			}
		default:
			return // 通道已排空，无自动 Ack 回声
		}
	}
}
