package cloudhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/protocol"

	"github.com/gorilla/websocket"
)

// 测试辅助：用 httptest 启动 CloudHub 的 HTTP 路由（不占用真实端口）。
func newTestServer(t *testing.T, opts ...Option) (*Server, string) {
	t.Helper()
	srv := New("127.0.0.1:0", opts...)
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + PathEdge
	return srv, wsURL
}

// dial 建立一条测试客户端连接。
func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("拨号 %s 失败: %v", url, err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// sendMsg 发送一条消息（编码后写出）。
func sendMsg(t *testing.T, ws *websocket.Conn, m *protocol.Message) {
	t.Helper()
	data, err := protocol.Encode(m)
	if err != nil {
		t.Fatalf("编码消息失败: %v", err)
	}
	if err := ws.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("设置写超时失败: %v", err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("发送消息失败: %v", err)
	}
}

// readMsg 读取并解码一条消息（带 2s 读超时，防止测试挂死）。
func readMsg(t *testing.T, ws *websocket.Conn) *protocol.Message {
	t.Helper()
	if err := ws.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	_, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	m, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("解码消息失败: %v", err)
	}
	return m
}

// register 发送 Register 并断言收到 accepted=true 的 RegisterAck。
func register(t *testing.T, ws *websocket.Conn, nodeID string) *protocol.Message {
	t.Helper()
	m, err := protocol.NewMessage(protocol.TypeRegister, nodeID, "cloud", RegisterPayload{
		NodeID:          nodeID,
		Arch:            "arm64",
		OS:              "linux",
		EdgecoreVersion: "v0.1.0",
		CPU:             4,
		Memory:          8 << 30,
	})
	if err != nil {
		t.Fatalf("构造 Register 失败: %v", err)
	}
	sendMsg(t, ws, m)
	ack := readMsg(t, ws)
	if ack.Type != protocol.TypeRegisterAck {
		t.Fatalf("期望 RegisterAck，收到 %s", ack.Type)
	}
	var payload RegisterAckPayload
	if err := ack.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 RegisterAck payload 失败: %v", err)
	}
	if !payload.Accepted {
		t.Fatalf("注册被拒绝: %s", payload.Message)
	}
	return ack
}

// waitFor 轮询等待条件成立，超时则测试失败。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

// TestRegisterSuccess 验证注册成功：收到 accepted=true 的 RegisterAck，
// 且注册表/节点信息正确。
// TestRegisterSuccess 验证节点注册成功：Register → RegisterAck（accepted=true），
// 注册表状态与 NodeInfo 字段正确。
func TestRegisterSuccess(t *testing.T) {
	srv, url := newTestServer(t)
	ws := dial(t, url)

	m, err := protocol.NewMessage(protocol.TypeRegister, "edge-001", "cloud", RegisterPayload{
		NodeID:          "edge-001",
		Arch:            "arm64",
		OS:              "linux",
		EdgecoreVersion: "v0.1.0",
		CPU:             4,
		Memory:          8 << 30,
	})
	if err != nil {
		t.Fatalf("构造 Register 失败: %v", err)
	}
	sendMsg(t, ws, m)

	ack := readMsg(t, ws)
	if ack.Type != protocol.TypeRegisterAck {
		t.Fatalf("期望 RegisterAck，收到 %s", ack.Type)
	}
	// 信封约定：云发往边缘的消息 Source=cloud、Target=nodeID，并关联请求 ID
	if ack.Source != "cloud" || ack.Target != "edge-001" {
		t.Errorf("RegisterAck 信封 = source=%q target=%q，期望 cloud/edge-001", ack.Source, ack.Target)
	}
	if ack.CorrelationID != m.ID {
		t.Errorf("RegisterAck CorrelationID = %q，期望 %q", ack.CorrelationID, m.ID)
	}
	var payload RegisterAckPayload
	if err := ack.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 RegisterAck payload 失败: %v", err)
	}
	if !payload.Accepted {
		t.Errorf("注册未被接受: %s", payload.Message)
	}
	if payload.NodeName != "edge-001" {
		t.Errorf("NodeName = %q，期望 edge-001", payload.NodeName)
	}

	// 注册表状态
	if got := srv.NodeCount(); got != 1 {
		t.Errorf("NodeCount = %d，期望 1", got)
	}
	if ids := srv.NodeIDs(); len(ids) != 1 || ids[0] != "edge-001" {
		t.Errorf("NodeIDs = %v，期望 [edge-001]", ids)
	}
	info, ok := srv.NodeInfo("edge-001")
	if !ok {
		t.Fatal("NodeInfo(edge-001) 不存在")
	}
	if info.Arch != "arm64" || info.OS != "linux" || info.CPU != 4 || info.Memory != 8<<30 {
		t.Errorf("NodeInfo 字段不符: %+v", info)
	}
	if info.RemoteIP == "" {
		t.Error("NodeInfo.RemoteIP 为空")
	}
	if info.RegisteredAt.IsZero() {
		t.Error("NodeInfo.RegisteredAt 为零值")
	}
}

// TestRegisterTokenAuth 验证 WBS 7.3 设备认证（token 消费）：
//   - 云端未启用（nodeToken 为空）→ 任意 token 均可注册（向后兼容）；
//   - 云端启用 → 缺失/错误 token 拒绝（accepted=false），正确 token 通过。
func TestRegisterTokenAuth(t *testing.T) {
	t.Run("未启用时任意token可注册", func(t *testing.T) {
		_, url := newTestServer(t) // 默认无 nodeToken
		ws := dial(t, url)
		m, _ := protocol.NewMessage(protocol.TypeRegister, "edge-t1", "cloud", RegisterPayload{
			NodeID: "edge-t1", Arch: "arm64", OS: "linux", EdgecoreVersion: "v0.1.0",
			CPU: 2, Memory: 1 << 30, Token: "whatever",
		})
		sendMsg(t, ws, m)
		ack := readMsg(t, ws)
		var p RegisterAckPayload
		if err := ack.DecodePayload(&p); err != nil {
			t.Fatalf("解析 RegisterAck payload 失败: %v", err)
		}
		if !p.Accepted {
			t.Errorf("未启用校验时注册被拒绝: %s", p.Message)
		}
	})

	t.Run("启用后缺失token被拒绝", func(t *testing.T) {
		srv, url := newTestServer(t, WithNodeToken("s3cret"))
		ws := dial(t, url)
		m, _ := protocol.NewMessage(protocol.TypeRegister, "edge-t2", "cloud", RegisterPayload{
			NodeID: "edge-t2", Arch: "arm64", OS: "linux", EdgecoreVersion: "v0.1.0",
			CPU: 2, Memory: 1 << 30, // 无 Token
		})
		sendMsg(t, ws, m)
		ack := readMsg(t, ws)
		var p RegisterAckPayload
		if err := ack.DecodePayload(&p); err != nil {
			t.Fatalf("解析 RegisterAck payload 失败: %v", err)
		}
		if p.Accepted {
			t.Errorf("缺失 token 竟然注册成功")
		}
		if got := srv.NodeCount(); got != 0 {
			t.Errorf("拒绝后 NodeCount = %d，期望 0", got)
		}
	})

	t.Run("启用后错误token被拒绝", func(t *testing.T) {
		srv, url := newTestServer(t, WithNodeToken("s3cret"))
		ws := dial(t, url)
		m, _ := protocol.NewMessage(protocol.TypeRegister, "edge-t3", "cloud", RegisterPayload{
			NodeID: "edge-t3", Arch: "arm64", OS: "linux", EdgecoreVersion: "v0.1.0",
			CPU: 2, Memory: 1 << 30, Token: "wrong-token",
		})
		sendMsg(t, ws, m)
		ack := readMsg(t, ws)
		var p RegisterAckPayload
		if err := ack.DecodePayload(&p); err != nil {
			t.Fatalf("解析 RegisterAck payload 失败: %v", err)
		}
		if p.Accepted {
			t.Errorf("错误 token 竟然注册成功")
		}
		if got := srv.NodeCount(); got != 0 {
			t.Errorf("拒绝后 NodeCount = %d，期望 0", got)
		}
	})

	t.Run("启用后正确token注册成功", func(t *testing.T) {
		srv, url := newTestServer(t, WithNodeToken("s3cret"))
		ws := dial(t, url)
		m, _ := protocol.NewMessage(protocol.TypeRegister, "edge-t4", "cloud", RegisterPayload{
			NodeID: "edge-t4", Arch: "arm64", OS: "linux", EdgecoreVersion: "v0.1.0",
			CPU: 2, Memory: 1 << 30, Token: "s3cret",
		})
		sendMsg(t, ws, m)
		ack := readMsg(t, ws)
		var p RegisterAckPayload
		if err := ack.DecodePayload(&p); err != nil {
			t.Fatalf("解析 RegisterAck payload 失败: %v", err)
		}
		if !p.Accepted {
			t.Errorf("正确 token 被拒绝: %s", p.Message)
		}
		if got := srv.NodeCount(); got != 1 {
			t.Errorf("NodeCount = %d，期望 1", got)
		}
	})
}

// TestHeartbeat 验证心跳：Heartbeat → HeartbeatAck（nodeStatus=Ready）。
func TestHeartbeat(t *testing.T) {
	srv, url := newTestServer(t)
	ws := dial(t, url)
	register(t, ws, "edge-002")

	hb, err := protocol.NewMessage(protocol.TypeHeartbeat, "edge-002", "cloud",
		HeartbeatPayload{Timestamp: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("构造 Heartbeat 失败: %v", err)
	}
	sendMsg(t, ws, hb)

	ack := readMsg(t, ws)
	if ack.Type != protocol.TypeHeartbeatAck {
		t.Fatalf("期望 HeartbeatAck，收到 %s", ack.Type)
	}
	if ack.CorrelationID != hb.ID {
		t.Errorf("HeartbeatAck CorrelationID = %q，期望 %q", ack.CorrelationID, hb.ID)
	}
	var payload HeartbeatAckPayload
	if err := ack.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 HeartbeatAck payload 失败: %v", err)
	}
	if payload.NodeStatus != "Ready" {
		t.Errorf("NodeStatus = %q，期望 Ready", payload.NodeStatus)
	}
	if got := srv.NodeCount(); got != 1 {
		t.Errorf("NodeCount = %d，期望 1", got)
	}
}

// TestDuplicateRegisterKicksOld 验证同 nodeID 重复注册：旧连接收到
// conflict Ack 后被关闭，注册表只保留新连接。
func TestDuplicateRegisterKicksOld(t *testing.T) {
	srv, url := newTestServer(t)
	oldWS := dial(t, url)
	register(t, oldWS, "edge-003")

	newWS := dial(t, url)
	register(t, newWS, "edge-003")

	// 旧连接应收到 conflict Ack
	ack := readMsg(t, oldWS)
	if ack.Type != protocol.TypeAck {
		t.Fatalf("期望旧连接收到 Ack（conflict），收到 %s", ack.Type)
	}
	var ackPayload AckPayload
	if err := ack.DecodePayload(&ackPayload); err != nil {
		t.Fatalf("解析 Ack payload 失败: %v", err)
	}
	if ackPayload.Code != CodeConflict {
		t.Errorf("Ack code = %q，期望 %q", ackPayload.Code, CodeConflict)
	}

	// 旧连接随后应被服务端关闭
	if err := oldWS.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	if _, _, err := oldWS.ReadMessage(); err == nil {
		t.Error("旧连接应被关闭，但仍可读到消息")
	}

	// 注册表只保留新连接，且新连接仍可用（心跳正常）
	if got := srv.NodeCount(); got != 1 {
		t.Errorf("NodeCount = %d，期望 1", got)
	}
	hb, err := protocol.NewMessage(protocol.TypeHeartbeat, "edge-003", "cloud",
		HeartbeatPayload{Timestamp: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("构造 Heartbeat 失败: %v", err)
	}
	sendMsg(t, newWS, hb)
	if ack := readMsg(t, newWS); ack.Type != protocol.TypeHeartbeatAck {
		t.Errorf("新连接心跳未得到 HeartbeatAck，收到 %s", ack.Type)
	}
}

// TestInvalidMessages 验证非法消息被拒绝：
//   - 缺少必填字段的消息 → Ack code=invalid_message
//   - 注册 payload 缺少 nodeID → RegisterAck accepted=false
//   - 消息 Source 与 payload.nodeID 不一致 → RegisterAck accepted=false
//   - 未注册先发心跳 → Ack code=not_registered
func TestInvalidMessages(t *testing.T) {
	srv, url := newTestServer(t)
	ws := dial(t, url)

	// 1) 缺少 type 字段（协议必填字段缺失）
	rawMissingType := `{"id":"m1","version":"v1","source":"edge-004","target":"cloud","timestamp":1}`
	if err := ws.WriteMessage(websocket.TextMessage, []byte(rawMissingType)); err != nil {
		t.Fatalf("发送缺 type 消息失败: %v", err)
	}
	ack := readMsg(t, ws)
	if ack.Type != protocol.TypeAck {
		t.Fatalf("期望 Ack，收到 %s", ack.Type)
	}
	var ackPayload AckPayload
	if err := ack.DecodePayload(&ackPayload); err != nil {
		t.Fatalf("解析 Ack payload 失败: %v", err)
	}
	if ackPayload.Code != CodeInvalidMessage {
		t.Errorf("Ack code = %q，期望 %q", ackPayload.Code, CodeInvalidMessage)
	}

	// 2) 注册 payload 缺少 nodeID
	m, err := protocol.NewMessage(protocol.TypeRegister, "edge-004", "cloud",
		map[string]any{"arch": "arm64"})
	if err != nil {
		t.Fatalf("构造 Register 失败: %v", err)
	}
	sendMsg(t, ws, m)
	regAck := readMsg(t, ws)
	if regAck.Type != protocol.TypeRegisterAck {
		t.Fatalf("期望 RegisterAck，收到 %s", regAck.Type)
	}
	var regPayload RegisterAckPayload
	if err := regAck.DecodePayload(&regPayload); err != nil {
		t.Fatalf("解析 RegisterAck payload 失败: %v", err)
	}
	if regPayload.Accepted {
		t.Error("缺少 nodeID 的注册不应被接受")
	}

	// 3) Source 与 payload.nodeID 不一致
	m, err = protocol.NewMessage(protocol.TypeRegister, "edge-004", "cloud",
		RegisterPayload{NodeID: "edge-other"})
	if err != nil {
		t.Fatalf("构造 Register 失败: %v", err)
	}
	sendMsg(t, ws, m)
	regAck = readMsg(t, ws)
	if err := regAck.DecodePayload(&regPayload); err != nil {
		t.Fatalf("解析 RegisterAck payload 失败: %v", err)
	}
	if regPayload.Accepted {
		t.Error("Source 与 nodeID 不一致的注册不应被接受")
	}

	// 4) 未注册先发心跳
	hb, err := protocol.NewMessage(protocol.TypeHeartbeat, "edge-004", "cloud",
		HeartbeatPayload{Timestamp: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("构造 Heartbeat 失败: %v", err)
	}
	sendMsg(t, ws, hb)
	ack = readMsg(t, ws)
	if ack.Type != protocol.TypeAck {
		t.Fatalf("期望 Ack，收到 %s", ack.Type)
	}
	if err := ack.DecodePayload(&ackPayload); err != nil {
		t.Fatalf("解析 Ack payload 失败: %v", err)
	}
	if ackPayload.Code != CodeNotRegistered {
		t.Errorf("Ack code = %q，期望 %q", ackPayload.Code, CodeNotRegistered)
	}

	// 全程未注册成功
	if got := srv.NodeCount(); got != 0 {
		t.Errorf("NodeCount = %d，期望 0", got)
	}
}

// TestNodeCountAndIDs 验证多节点注册与断开后的注册表变化。
func TestNodeCountAndIDs(t *testing.T) {
	srv, url := newTestServer(t)
	wsA := dial(t, url)
	wsB := dial(t, url)
	register(t, wsA, "edge-a")
	register(t, wsB, "edge-b")

	if got := srv.NodeCount(); got != 2 {
		t.Errorf("NodeCount = %d，期望 2", got)
	}
	ids := srv.NodeIDs()
	if len(ids) != 2 || ids[0] != "edge-a" || ids[1] != "edge-b" {
		t.Errorf("NodeIDs = %v，期望 [edge-a edge-b]", ids)
	}

	// 断开一个节点：服务端感知连接关闭后应从注册表移除
	_ = wsA.Close()
	waitFor(t, 3*time.Second, func() bool { return srv.NodeCount() == 1 },
		"断开节点后注册表未收敛")
	if ids := srv.NodeIDs(); len(ids) != 1 || ids[0] != "edge-b" {
		t.Errorf("NodeIDs = %v，期望 [edge-b]", ids)
	}
	if _, ok := srv.NodeInfo("edge-a"); ok {
		t.Error("已断开节点 edge-a 仍存在注册信息")
	}
}

// TestHeartbeatTimeout 验证心跳超时：超时后服务端主动断开连接并移除注册。
func TestHeartbeatTimeout(t *testing.T) {
	srv, url := newTestServer(t, WithHeartbeatTimeout(300*time.Millisecond))
	ws := dial(t, url)
	register(t, ws, "edge-timeout")

	// 不发任何消息，等待服务端因超时主动断开
	if err := ws.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("期望服务端主动断开连接")
	}
	waitFor(t, 3*time.Second, func() bool { return srv.NodeCount() == 0 },
		"超时节点未从注册表移除")
}

// TestStartShutdown 验证完整生命周期：Start 监听 → 注册 → Shutdown
// 优雅关闭（连接被关闭、注册表清空、Start 返回 nil）。
func TestStartShutdown(t *testing.T) {
	srv := New("127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()

	waitFor(t, 3*time.Second, func() bool { return srv.Addr() != "" }, "CloudHub 未开始监听")
	ws := dial(t, "ws://"+srv.Addr()+PathEdge)
	register(t, ws, "edge-lifecycle")
	if got := srv.NodeCount(); got != 1 {
		t.Fatalf("NodeCount = %d，期望 1", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start 返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start 未在 Shutdown 后退出")
	}

	// 连接应被关闭，注册表清空
	if got := srv.NodeCount(); got != 0 {
		t.Errorf("Shutdown 后 NodeCount = %d，期望 0", got)
	}
	if err := ws.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Error("Shutdown 后连接应被关闭")
	}
}

// TestServeWSAfterShutdownStarted 验证 P2-1 修复的确定性路径：
// Shutdown 已开始（shuttingDown 置位）后到达的 Upgrade，在 trackConn
// 复查时被拒绝——连接被立即关闭（收到 close frame），且不启动连接
// goroutine（wg 计数守恒，随后的正式 Shutdown 快速返回、Start 正常退出）。
func TestServeWSAfterShutdownStarted(t *testing.T) {
	srv := New("127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()
	waitFor(t, 3*time.Second, func() bool { return srv.Addr() != "" }, "CloudHub 未开始监听")

	// 直接置位关闭标志，模拟 Shutdown 已执行到 closeAllConns 之前的时刻
	srv.shuttingDown.Store(true)
	ws, _, err := websocket.DefaultDialer.Dial("ws://"+srv.Addr()+PathEdge, nil)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = ws.Close() }()
	// 关闭流程中新建的连接应被服务端立即主动关闭（收到 close frame），
	// 而不是挂起等读超时——这是 serveWS 拒绝路径的直接证据
	if err := ws.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("关闭流程中新建的连接应被立即关闭")
	} else {
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) {
			t.Fatalf("连接应被服务端主动关闭（close frame），实际错误: %v", err)
		}
	}

	// 随后完整执行 Shutdown：wg 计数守恒，应快速返回，Start 正常退出
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start 返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start 未在 Shutdown 后退出")
	}
}

// TestShutdownDuringNewConnections 验证 P2-1 的并发窗口竞态：
// 多轮“并发拨号 + Shutdown 交错”下，Shutdown 必须快速返回（旧实现可能
// 因漏关连接而阻塞到心跳超时 ~90s），Start 正常退出。
func TestShutdownDuringNewConnections(t *testing.T) {
	for i := 0; i < 15; i++ {
		srv := New("127.0.0.1:0")
		done := make(chan error, 1)
		go func() { done <- srv.Start() }()
		waitFor(t, 3*time.Second, func() bool { return srv.Addr() != "" }, "CloudHub 未开始监听")

		url := "ws://" + srv.Addr() + PathEdge
		// 并发建立一批连接，让 Upgrade 与 Shutdown 的关闭快照尽可能交错
		var dialWG sync.WaitGroup
		for j := 0; j < 8; j++ {
			dialWG.Add(1)
			go func() {
				defer dialWG.Done()
				ws, _, err := websocket.DefaultDialer.Dial(url, nil)
				if err != nil {
					return // 监听器已关闭：拨号失败属正常
				}
				_ = ws.Close()
			}()
		}
		// 拨号完成后再启动 Shutdown，尽量覆盖“Upgrade 之后、trackConn 之前”窗口
		dialWG.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownDone := make(chan error, 1)
		go func() { shutdownDone <- srv.Shutdown(ctx) }()
		select {
		case err := <-shutdownDone:
			if err != nil {
				cancel()
				t.Fatalf("第 %d 轮：Shutdown 返回错误: %v", i, err)
			}
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatalf("第 %d 轮：Shutdown 阻塞超过 3s（新建连接窗口竞态未修复）", i)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("第 %d 轮：Start 返回错误: %v", i, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("第 %d 轮：Start 未在 Shutdown 后退出", i)
		}
	}
}

// TestRegisterMemoryAsUint64 验证 P2-3：Memory 字段契约统一为 uint64——
// 超过 int64 正数上限的内存字节数（1<<63，8 EiB）也能无损解析并记录
// （int64 时代该值会导致 JSON 解码失败、注册被拒）。
func TestRegisterMemoryAsUint64(t *testing.T) {
	srv, url := newTestServer(t)
	ws := dial(t, url)

	m, err := protocol.NewMessage(protocol.TypeRegister, "edge-mem", "cloud", RegisterPayload{
		NodeID:          "edge-mem",
		Arch:            "arm64",
		OS:              "linux",
		EdgecoreVersion: "v0.1.0",
		CPU:             4,
		Memory:          uint64(1) << 63, // 超出 int64 正数范围，验证 uint64 契约
	})
	if err != nil {
		t.Fatalf("构造 Register 失败: %v", err)
	}
	sendMsg(t, ws, m)

	ack := readMsg(t, ws)
	if ack.Type != protocol.TypeRegisterAck {
		t.Fatalf("期望 RegisterAck，收到 %s", ack.Type)
	}
	var payload RegisterAckPayload
	if err := ack.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 RegisterAck payload 失败: %v", err)
	}
	if !payload.Accepted {
		t.Fatalf("注册被拒绝: %s", payload.Message)
	}
	info, ok := srv.NodeInfo("edge-mem")
	if !ok {
		t.Fatal("NodeInfo(edge-mem) 不存在")
	}
	if info.Memory != uint64(1)<<63 {
		t.Errorf("NodeInfo.Memory = %d，期望 %d（uint64 契约）", info.Memory, uint64(1)<<63)
	}
}

// TestEnvelopeJSON 验证客户端以原生 JSON 交互时信封字段与契约一致
// （顺带覆盖真实 wire 格式，避免测试只依赖本包结构体）。
func TestEnvelopeJSON(t *testing.T) {
	_, url := newTestServer(t)
	ws := dial(t, url)

	raw := `{"id":"raw-1","type":"Register","version":"v1","source":"edge-raw","target":"cloud","timestamp":` +
		`1720000000000,"payload":{"nodeID":"edge-raw","arch":"amd64","os":"linux","edgecoreVersion":"v0.1.0","cpu":2,"memory":4096}}`
	if err := ws.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
		t.Fatalf("发送原生 JSON 失败: %v", err)
	}
	if err := ws.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	_, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("读取 RegisterAck 失败: %v", err)
	}
	// 用纯 JSON 断言 wire 格式，不经过 protocol.Decode
	var envelope struct {
		ID            string          `json:"id"`
		Type          string          `json:"type"`
		Version       string          `json:"version"`
		Source        string          `json:"source"`
		Target        string          `json:"target"`
		Timestamp     int64           `json:"timestamp"`
		CorrelationID string          `json:"correlationId"`
		Payload       json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("解析 wire 消息失败: %v", err)
	}
	if envelope.Type != protocol.TypeRegisterAck || envelope.Source != "cloud" || envelope.Target != "edge-raw" {
		t.Errorf("wire 信封 = type=%q source=%q target=%q", envelope.Type, envelope.Source, envelope.Target)
	}
	if envelope.CorrelationID != "raw-1" {
		t.Errorf("wire CorrelationID = %q，期望 raw-1", envelope.CorrelationID)
	}
	var payload RegisterAckPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("解析 wire payload 失败: %v", err)
	}
	if !payload.Accepted {
		t.Errorf("wire 注册未被接受: %s", payload.Message)
	}
}
