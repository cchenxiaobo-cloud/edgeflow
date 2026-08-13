package edgehub

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/protocol"
	"edgeflow/pkg/version"

	"github.com/gorilla/websocket"
)

// mockConfig 控制 mock 云端的可测行为。
type mockConfig struct {
	// rejectRegister 为 true 时 RegisterAck 返回 accepted=false。
	rejectRegister bool
	// closeAfterRegisterCount 应答完第 N 个连接上的 Register 后主动断开
	// （0 表示不断开）。用于模拟云端在注册后掉线。
	closeAfterRegisterCount int
	// sendPodSync 为 true 时注册成功后向客户端推送一条 PodSync。
	sendPodSync bool
	// sendOversized 为 true 时注册成功后向客户端推送一条超过
	// maxReadBytes（1 MiB）的消息，验证客户端 SetReadLimit 生效。
	sendOversized bool
}

// mockCloud 是进程内的模拟 CloudHub 服务端：
// 监听 /v1/edge，自动应答 Register/Heartbeat，记录收到的消息，
// 并支持按配置主动断开连接。
type mockCloud struct {
	t   *testing.T
	cfg mockConfig
	srv *httptest.Server
	url string // ws://... 形式的完整地址（含 /v1/edge）

	mu         sync.Mutex
	sendMu     sync.Mutex             // 串行化 mock 端 WebSocket 写（gorilla 不允许并发写）
	upgrades   int                    // 累计建立的连接数（第几个连接）
	conn       *websocket.Conn        // 最近一次升级的连接（测试主动推送用）
	received   chan *protocol.Message // 收到的所有消息
	registers  chan *protocol.Message // 收到的 Register（带缓冲，服务端不阻塞）
	heartbeats chan struct{}          // 每收到一条 Heartbeat 通知一次
	connCount  chan struct{}          // 每次建立新连接通知一次
}

// startMockCloud 启动 mock 云端，测试结束自动关闭。
func startMockCloud(t *testing.T, cfg mockConfig) *mockCloud {
	t.Helper()
	m := &mockCloud{
		t:          t,
		cfg:        cfg,
		received:   make(chan *protocol.Message, 256),
		registers:  make(chan *protocol.Message, 64),
		heartbeats: make(chan struct{}, 256),
		connCount:  make(chan struct{}, 64),
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	m.url = "ws" + strings.TrimPrefix(m.srv.URL, "http") + channelPath
	return m
}

// handle 是 mock 云端的 WebSocket 处理器。
func (m *mockCloud) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != channelPath {
		http.Error(w, "路径错误: "+r.URL.Path, http.StatusNotFound)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	m.mu.Lock()
	m.upgrades++
	n := m.upgrades
	m.conn = conn // 记录当前连接，供测试 push 使用
	m.mu.Unlock()
	m.connCount <- struct{}{}

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return // 客户端断开
		}
		msg, err := protocol.Decode(data)
		if err != nil {
			m.t.Errorf("mock cloud 收到无法解析的消息: %v", err)
			continue
		}
		m.received <- msg
		switch msg.Type {
		case protocol.TypeRegister:
			m.registers <- msg
			ackPayload := RegisterAckPayload{Accepted: true, NodeName: "cloud-" + m.nodeIDOf(msg), Message: "ok"}
			if m.cfg.rejectRegister {
				ackPayload = RegisterAckPayload{Accepted: false, NodeName: "", Message: "node not authorized"}
			}
			ack, _ := protocol.NewMessage(protocol.TypeRegisterAck, targetCloud, msg.Source, ackPayload)
			if err := m.send(conn, ack); err != nil {
				return
			}
			if m.cfg.closeAfterRegisterCount > 0 && n == m.cfg.closeAfterRegisterCount {
				// 注册应答已发出，主动断开：客户端应感知断线并重连
				return
			}
			if m.cfg.sendPodSync {
				podSync, _ := protocol.NewMessage(protocol.TypePodSync, targetCloud, msg.Source,
					map[string]any{"pod": "demo", "namespace": "default"})
				if err := m.send(conn, podSync); err != nil {
					return
				}
			}
			if m.cfg.sendOversized {
				// 构造超过 1 MiB 的负载：客户端应触发 SetReadLimit 拒绝并断开
				oversized, _ := protocol.NewMessage(protocol.TypeConfigSync, targetCloud, msg.Source,
					map[string]any{"data": strings.Repeat("x", maxReadBytes)})
				if err := m.send(conn, oversized); err != nil {
					return
				}
			}
		case protocol.TypeHeartbeat:
			ack, _ := protocol.NewMessage(protocol.TypeHeartbeatAck, targetCloud, msg.Source,
				HeartbeatAckPayload{NodeStatus: "Ready"})
			if err := m.send(conn, ack); err != nil {
				return
			}
			select {
			case m.heartbeats <- struct{}{}:
			default:
			}
		}
	}
}

// nodeIDOf 从消息的 Source 中提取节点 ID（mock 应答时用作 nodeName 前缀）。
func (m *mockCloud) nodeIDOf(msg *protocol.Message) string {
	return msg.Source
}

// send 以文本帧发送一条消息（并发安全：gorilla/websocket 不允许并发写）。
func (m *mockCloud) send(conn *websocket.Conn, msg *protocol.Message) error {
	data, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	return conn.WriteMessage(websocket.TextMessage, data)
}

// push 通过当前活动连接向客户端推送一条消息（测试主动注入用）。
func (m *mockCloud) push(msg *protocol.Message) error {
	m.mu.Lock()
	conn := m.conn
	m.mu.Unlock()
	if conn == nil {
		return errors.New("mock cloud 无活动连接")
	}
	return m.send(conn, msg)
}

// waitRegister 等待 mock 收到一条 Register（带超时）。
func (m *mockCloud) waitRegister(t *testing.T) *protocol.Message {
	t.Helper()
	select {
	case msg := <-m.registers:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内未收到 Register")
		return nil
	}
}

// waitType 等待 mock 收到指定类型的消息（带超时），跳过其他类型。
func (m *mockCloud) waitType(t *testing.T, msgType string) *protocol.Message {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-m.received:
			if msg.Type == msgType {
				return msg
			}
		case <-deadline:
			t.Fatalf("5s 内未收到 %s 消息", msgType)
			return nil
		}
	}
}

// waitHeartbeat 等待 mock 收到一条 Heartbeat（带超时）。
func (m *mockCloud) waitHeartbeat(t *testing.T) {
	t.Helper()
	select {
	case <-m.heartbeats:
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内未收到 Heartbeat")
	}
}

// waitStatus 等待客户端状态回调，并断言其值。
func waitStatus(t *testing.T, ch <-chan bool, want bool) {
	t.Helper()
	select {
	case v := <-ch:
		if v != want {
			t.Fatalf("状态回调 = %v，期望 %v", v, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("5s 内未收到状态回调 %v", want)
	}
}

// TestRegisterOnConnect 验证：连接建立后客户端第一条消息是 Register，
// 字段与契约一致（Source=nodeID、Target=cloud、负载含 nodeID/arch/os/
// edgecoreVersion/cpu/memory），收到 accepted=true 后进入已连接状态。
func TestRegisterOnConnect(t *testing.T) {
	mock := startMockCloud(t, mockConfig{})
	client := New(Options{CloudAddr: mock.url, NodeID: "test-node-001"})

	statusCh := make(chan bool, 8)
	client.SetStatusHandler(func(v bool) { statusCh <- v })
	client.Start()
	defer client.Stop()

	msg := mock.waitRegister(t)
	if msg.Type != protocol.TypeRegister {
		t.Fatalf("第一条消息类型 = %s，期望 %s", msg.Type, protocol.TypeRegister)
	}
	if msg.Source != "test-node-001" {
		t.Errorf("Register.Source = %q，期望 nodeID", msg.Source)
	}
	if msg.Target != targetCloud {
		t.Errorf("Register.Target = %q，期望 cloud", msg.Target)
	}

	var p RegisterPayload
	if err := msg.DecodePayload(&p); err != nil {
		t.Fatalf("解析 Register 负载失败: %v", err)
	}
	if p.NodeID != "test-node-001" {
		t.Errorf("payload.nodeID = %q", p.NodeID)
	}
	if p.Arch != runtime.GOARCH {
		t.Errorf("payload.arch = %q，期望 %q", p.Arch, runtime.GOARCH)
	}
	if p.OS != runtime.GOOS {
		t.Errorf("payload.os = %q，期望 %q", p.OS, runtime.GOOS)
	}
	if p.EdgeCoreVersion != version.Get().String() {
		t.Errorf("payload.edgecoreVersion = %q，期望 %q", p.EdgeCoreVersion, version.Get().String())
	}
	if p.CPU != runtime.NumCPU() {
		t.Errorf("payload.cpu = %d，期望 %d", p.CPU, runtime.NumCPU())
	}
	if runtime.GOOS == "linux" && p.Memory == 0 {
		t.Errorf("payload.memory 在 Linux 上不应为 0")
	}

	// 注册成功后：状态回调 true + IsConnected 为 true + nodeName 已记录
	waitStatus(t, statusCh, true)
	if !client.IsConnected() {
		t.Fatal("IsConnected() 应为 true")
	}
	if got := client.NodeName(); got != "cloud-test-node-001" {
		t.Errorf("NodeName() = %q，期望云端分配的 cloud-test-node-001", got)
	}
}

// TestHeartbeatLoop 验证：客户端按间隔发送 Heartbeat（负载带时间戳），
// 收到 HeartbeatAck 后更新 LastHeartbeatAckTime。
func TestHeartbeatLoop(t *testing.T) {
	mock := startMockCloud(t, mockConfig{})
	client := New(Options{
		CloudAddr:         mock.url,
		NodeID:            "hb-node",
		HeartbeatInterval: 50 * time.Millisecond,
	})
	client.Start()
	defer client.Stop()

	mock.waitRegister(t)
	for i := 0; i < 2; i++ {
		mock.waitHeartbeat(t)
	}

	// 校验心跳负载：timestamp 应为正数（毫秒时间戳）
	found := false
	for {
		select {
		case m := <-mock.received:
			if m.Type != protocol.TypeHeartbeat {
				continue
			}
			var p HeartbeatPayload
			if err := m.DecodePayload(&p); err != nil {
				t.Fatalf("解析 Heartbeat 负载失败: %v", err)
			}
			if p.Timestamp <= 0 {
				t.Fatalf("Heartbeat.timestamp = %d，应为正数", p.Timestamp)
			}
			found = true
		case <-time.After(2 * time.Second):
			t.Fatal("收到的消息中未找到 Heartbeat")
		}
		if found {
			break
		}
	}

	// 收到 HeartbeatAck：lastAck 应为近期时间
	ack := client.LastHeartbeatAckTime()
	if ack.IsZero() {
		t.Fatal("LastHeartbeatAckTime() 应为非零值")
	}
	if since := time.Since(ack); since > 2*time.Second {
		t.Fatalf("LastHeartbeatAckTime 距今 %v，应接近当前时间", since)
	}
	if !client.IsConnected() {
		t.Fatal("心跳正常时 IsConnected() 应为 true")
	}
}

// TestReconnectAfterServerClose 验证：服务端主动断开后，客户端按退避
// 重连成功，并重新发送 Register（新消息 ID），状态回调经历 true→false→true。
func TestReconnectAfterServerClose(t *testing.T) {
	mock := startMockCloud(t, mockConfig{closeAfterRegisterCount: 1})
	client := New(Options{
		CloudAddr:         mock.url,
		NodeID:            "reconnect-node",
		HeartbeatInterval: 50 * time.Millisecond,
		BackoffBase:       50 * time.Millisecond,
		BackoffMax:        200 * time.Millisecond,
	})
	statusCh := make(chan bool, 8)
	client.SetStatusHandler(func(v bool) { statusCh <- v })
	client.Start()
	defer client.Stop()

	r1 := mock.waitRegister(t) // 第一个连接上的注册
	waitStatus(t, statusCh, true)
	waitStatus(t, statusCh, false) // 服务端断开 → 状态转 false

	r2 := mock.waitRegister(t) // 重连后重新注册
	if r1.ID == r2.ID {
		t.Fatal("重连后的 Register 应为新消息（ID 不同）")
	}
	waitStatus(t, statusCh, true)
	if !client.IsConnected() {
		t.Fatal("重连注册成功后 IsConnected() 应为 true")
	}

	// 重连后的连接应能继续正常心跳
	mock.waitHeartbeat(t)
}

// TestNextBackoffSequence 验证默认退避序列：1s,2s,4s,8s,16s,32s,60s,60s（契约）。
func TestNextBackoffSequence(t *testing.T) {
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second,
	}
	b := DefaultBackoffBase
	got := make([]time.Duration, 0, len(want))
	for i := 0; i < len(want); i++ {
		got = append(got, b)
		b = nextBackoff(b, DefaultBackoffMax)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("退避序列第 %d 项 = %v，期望 %v（完整序列 %v）", i, got[i], want[i], got)
		}
	}
}

// TestNextBackoffSequenceInjected 验证注入短间隔时序列按比例生效（测试加速用）。
func TestNextBackoffSequenceInjected(t *testing.T) {
	b, max := 50*time.Millisecond, 200*time.Millisecond
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond}
	got := make([]time.Duration, 0, len(want))
	for i := 0; i < len(want); i++ {
		got = append(got, b)
		b = nextBackoff(b, max)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("注入序列第 %d 项 = %v，期望 %v", i, got[i], want[i])
		}
	}
}

// TestFirstReconnectDelayAtLeastOneSecond 验证：使用默认退避参数时，
// 首次连接/注册失败后的重试延迟 ≥1s（契约的退避下限）。
func TestFirstReconnectDelayAtLeastOneSecond(t *testing.T) {
	mock := startMockCloud(t, mockConfig{rejectRegister: true})
	client := New(Options{CloudAddr: mock.url, NodeID: "slow-backoff"}) // 默认 BackoffBase=1s
	client.Start()
	defer client.Stop()

	mock.waitRegister(t) // 第一次注册（被拒）
	start := time.Now()
	mock.waitRegister(t) // 第二次注册尝试：应间隔 ≥1s 才发起
	elapsed := time.Since(start)
	if elapsed < time.Second {
		t.Fatalf("首次失败后的重试间隔 = %v，应 ≥1s", elapsed)
	}
}

// TestRegisterRejectedRetries 验证：accepted=false 时客户端记录错误并
// 按退避继续重试（决策：拒绝不特殊处理，统一走重连路径）。
func TestRegisterRejectedRetries(t *testing.T) {
	mock := startMockCloud(t, mockConfig{rejectRegister: true})
	client := New(Options{
		CloudAddr:   mock.url,
		NodeID:      "rejected-node",
		BackoffBase: 20 * time.Millisecond,
		BackoffMax:  100 * time.Millisecond,
	})
	client.Start()
	defer client.Stop()

	mock.waitRegister(t)
	mock.waitRegister(t) // 被拒后应再次发起注册
	if client.IsConnected() {
		t.Fatal("注册被拒绝时 IsConnected() 应为 false")
	}
}

// TestMessageHandler 验证：非应答类消息（PodSync）回调给上层，
// 而 RegisterAck/HeartbeatAck 不会进入消息回调。
func TestMessageHandler(t *testing.T) {
	mock := startMockCloud(t, mockConfig{sendPodSync: true})
	client := New(Options{
		CloudAddr:         mock.url,
		NodeID:            "pod-node",
		HeartbeatInterval: 50 * time.Millisecond,
	})
	got := make(chan *protocol.Message, 8)
	client.SetMessageHandler(func(m *protocol.Message) { got <- m })
	client.Start()
	defer client.Stop()

	mock.waitRegister(t)
	select {
	case m := <-got:
		if m.Type != protocol.TypePodSync {
			t.Fatalf("回调消息类型 = %s，期望 %s", m.Type, protocol.TypePodSync)
		}
		if m.Source != targetCloud || m.Target != "pod-node" {
			t.Errorf("PodSync 信封 Source/Target = %q/%q", m.Source, m.Target)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内未收到 PodSync 回调")
	}

	// 应答类消息不应进入回调：等若干心跳周期后回调通道应为空
	time.Sleep(250 * time.Millisecond)
	select {
	case m := <-got:
		t.Fatalf("消息回调不应收到应答类消息，实际收到 %s", m.Type)
	default:
	}
}

// TestReadLimitRejectsOversizedMessage 验证 P2-2：云端下发超过 1 MiB 的
// 消息时，客户端按 SetReadLimit 拒绝（连接断开、状态回调 false），随后
// 按退避重连并重新注册成功（与云端 maxMessageBytes=1MiB 对称的防护，
// 防止超大消息灌入导致内存膨胀）。
func TestReadLimitRejectsOversizedMessage(t *testing.T) {
	mock := startMockCloud(t, mockConfig{sendOversized: true})
	client := New(Options{
		CloudAddr:   mock.url,
		NodeID:      "readlimit-node",
		BackoffBase: 50 * time.Millisecond,
		BackoffMax:  200 * time.Millisecond,
	})
	statusCh := make(chan bool, 8)
	client.SetStatusHandler(func(v bool) { statusCh <- v })
	client.Start()
	defer client.Stop()

	mock.waitRegister(t)           // 第一次注册成功（随后云端推送超大消息）
	waitStatus(t, statusCh, true)  // 已连接
	waitStatus(t, statusCh, false) // 收到超大消息 → 连接被拒绝并断开
	mock.waitRegister(t)           // 重连后重新注册
	waitStatus(t, statusCh, true)  // 恢复已连接
	if !client.IsConnected() {
		t.Fatal("重连注册成功后 IsConnected() 应为 true")
	}
}

// TestConnectRefusedRetries 验证：云端不可达（连接被拒）时客户端按退避
// 持续重试，不会退出。
func TestConnectRefusedRetries(t *testing.T) {
	// 占用端口后立即释放，保证该地址必然拒绝连接
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "ws://" + ln.Addr().String()
	_ = ln.Close()

	cd := &countingDialer{}
	client := New(Options{
		CloudAddr:   addr,
		NodeID:      "refused-node",
		BackoffBase: 20 * time.Millisecond,
		BackoffMax:  50 * time.Millisecond,
		Dialer:      cd,
	})
	client.Start()
	time.Sleep(150 * time.Millisecond) // 足够 2 次以上重试
	client.Stop()

	if cd.count() < 2 {
		t.Fatalf("连接被拒后应重试，实际拨号 %d 次", cd.count())
	}
	if client.IsConnected() {
		t.Fatal("连接失败时 IsConnected() 应为 false")
	}
}

// waitConnected 轮询等待客户端进入已连接状态（注册应答往返完成的信号）。
func waitConnected(t *testing.T, client *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if client.IsConnected() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("5s 内客户端未进入已连接状态")
}

// TestStop 验证：Stop 能优雅关闭（不挂起），之后 IsConnected 为 false。
func TestStop(t *testing.T) {
	mock := startMockCloud(t, mockConfig{})
	client := New(Options{CloudAddr: mock.url, NodeID: "stop-node"})
	client.Start()
	mock.waitRegister(t)
	waitConnected(t, client)

	done := make(chan struct{})
	go func() {
		client.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 超时未返回")
	}
	if client.IsConnected() {
		t.Fatal("Stop 后 IsConnected() 应为 false")
	}
}

// TestStopWithoutStart 验证：未 Start 直接 Stop 不 panic、不挂起。
func TestStopWithoutStart(t *testing.T) {
	client := New(Options{CloudAddr: "ws://127.0.0.1:1", NodeID: "x"})
	client.Stop()
}

// TestEnsureChannelPath 验证通道路径补全规则。
func TestEnsureChannelPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"ws://127.0.0.1:10000", "ws://127.0.0.1:10000/v1/edge"},
		{"ws://127.0.0.1:10000/", "ws://127.0.0.1:10000/v1/edge"},
		{"ws://cloud.example.com:10000", "ws://cloud.example.com:10000/v1/edge"},
		{"ws://cloud.example.com:10000/v1/edge", "ws://cloud.example.com:10000/v1/edge"}, // 显式路径保持原样
		{"wss://gateway:443/edge", "wss://gateway:443/edge"},                             // 网关转发路径保持原样
		{"not-a-url", "not-a-url"},                                                       // 非法地址原样返回
	}
	for _, c := range cases {
		if got := ensureChannelPath(c.in); got != c.want {
			t.Errorf("ensureChannelPath(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestNewAppliesDefaults 验证 New 的默认值补齐与路径补全。
func TestNewAppliesDefaults(t *testing.T) {
	client := New(Options{})
	if client.opts.CloudAddr != "ws://127.0.0.1:10000/v1/edge" {
		t.Errorf("默认 CloudAddr = %q", client.opts.CloudAddr)
	}
	if client.opts.HeartbeatInterval != DefaultHeartbeatInterval {
		t.Errorf("默认心跳间隔 = %v", client.opts.HeartbeatInterval)
	}
	if client.opts.BackoffBase != DefaultBackoffBase || client.opts.BackoffMax != DefaultBackoffMax {
		t.Errorf("默认退避 = %v/%v", client.opts.BackoffBase, client.opts.BackoffMax)
	}
	if client.opts.NodeID == "" {
		t.Error("默认 NodeID 不应为空")
	}
}

// countingDialer 包装拨号器，统计拨号次数（注入测试用）。
type countingDialer struct {
	mu    sync.Mutex
	dials int
}

func (d *countingDialer) Dial(urlStr string, h http.Header) (*websocket.Conn, *http.Response, error) {
	d.mu.Lock()
	d.dials++
	d.mu.Unlock()
	return websocket.DefaultDialer.Dial(urlStr, h)
}

func (d *countingDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}
