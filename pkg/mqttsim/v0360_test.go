package mqttsim

// v0.36.0 MQTT 5.0 阶段五：遗嘱消息（will）面测试（wire 层，复用
// v0320/v0330/v0350 辅助）。

import (
	"net"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
)

// v0360DialWill 建立 v5 连接并携带 will 配置，断言 CONNACK。
func v0360DialWill(t *testing.T, addr, cid, wtopic, wpayload string, wqos byte, wretain bool, wdelay uint32, clean bool, expiry uint32) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := mqtt.EncodePacket(conn, &mqtt.Connect{
		V5: true, ClientID: cid, CleanSession: clean, SessionExpiry: expiry,
		WillTopic: wtopic, WillMessage: wpayload, WillQoS: wqos, WillRetain: wretain, WillDelay: wdelay,
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	p, err := mqtt.DecodePacketV(conn, true)
	if err != nil {
		t.Fatalf("read connack: %v", err)
	}
	if ca, ok := p.(*mqtt.Connack); !ok || ca.ReturnCode != 0 {
		t.Fatalf("expected CONNACK ok, got %T %+v", p, p)
	}
	return conn
}

// v0360DialWillV3 建立 3.1.1 连接并携带 will（无属性区）。
func v0360DialWillV3(t *testing.T, addr, cid, wtopic, wpayload string, wqos byte, wretain bool) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := mqtt.EncodePacket(conn, &mqtt.Connect{
		ClientID: cid, WillTopic: wtopic, WillMessage: wpayload, WillQoS: wqos, WillRetain: wretain,
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	p, err := mqtt.DecodePacketV(conn, false)
	if err != nil {
		t.Fatalf("read connack: %v", err)
	}
	if ca, ok := p.(*mqtt.Connack); !ok || ca.ReturnCode != 0 {
		t.Fatalf("expected CONNACK ok, got %T %+v", p, p)
	}
	return conn
}

// v0360WaitDetached 等待会话与连接解耦（conn 摘除；与快照写入同一
// b.mu 临界区——观察到 nil 即快照完成）。无订阅会话（will 发送方）
// 亦适用（v0350WaitSession 的 filters>0 条件不覆盖该场景）。
func v0360WaitDetached(t *testing.T, b *Broker, cid string) {
	t.Helper()
	v0320WaitFor(t, 2*time.Second, "session detached", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		s := b.sessions[cid]
		return s != nil && s.conn == nil
	})
}

// v0360DialAuth 建立带凭证的 v5 连接（鉴权 broker 场景），断言 CONNACK。
func v0360DialAuth(t *testing.T, addr, cid, user, pass string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := mqtt.EncodePacket(conn, &mqtt.Connect{
		V5: true, ClientID: cid, Username: user, Password: pass,
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	p, err := mqtt.DecodePacketV(conn, true)
	if err != nil {
		t.Fatalf("read connack: %v", err)
	}
	if ca, ok := p.(*mqtt.Connack); !ok || ca.ReturnCode != 0 {
		t.Fatalf("expected CONNACK ok, got %T %+v", p, p)
	}
	return conn
}

// v0360WaitPending 等待 clientID 的待发 will 注册（delay 场景同步点）。
func v0360WaitPending(t *testing.T, b *Broker, cid string) {
	t.Helper()
	v0320WaitFor(t, 2*time.Second, "pending will", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.pendingWills[cid] != nil
	})
}

// US-2：异常断连（裸 socket 关闭）发布 will（QoS0）。
func TestV0360AbnormalPublishBasic(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-basic-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/basic", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	a := v0360DialWill(t, b.Addr(), "w-basic", "w/basic", "bye", 0, false, 0, true, 0)
	_ = a.Close() // 裸断（无 DISCONNECT）
	got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
	if got.Topic != "w/basic" || string(got.Payload) != "bye" {
		t.Fatalf("will: %+v", got)
	}
	if got.Retain {
		t.Fatalf("plain will must not carry RETAIN: %+v", got)
	}
}

// US-2：正常 DISCONNECT（rc=0）抑制 will。
func TestV0360NormalDisconnectSuppressed(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-norm-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/norm", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	a := v0360DialWill(t, b.Addr(), "w-norm", "w/norm", "no", 0, false, 0, true, 0)
	if err := mqtt.EncodePacket(a, &mqtt.Disconnect{V5: true}); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	v0350ExpectNoPublish(t, sub, 500*time.Millisecond)
}

// US-2：3.1.1 连接带 will，异常断连同样发布（规范共有行为）。
func TestV0360V311WillPublish(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-v3-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/v3", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	a := v0360DialWillV3(t, b.Addr(), "w-v3", "w/v3", "old", 0, false)
	_ = a.Close()
	got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
	if string(got.Payload) != "old" {
		t.Fatalf("v3 will: %+v", got)
	}
}

// US-2：3.1.1 正常 DISCONNECT（空体）抑制 will。
func TestV0360V311NormalDisconnectSuppressed(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-v3n-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/v3n", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	a := v0360DialWillV3(t, b.Addr(), "w-v3n", "w/v3n", "no", 0, false)
	if err := mqtt.EncodePacket(a, &mqtt.Disconnect{}); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	v0350ExpectNoPublish(t, sub, 500*time.Millisecond)
}

// US-5：无 will 连接异常断连零消息（回归锚）。
func TestV0360NoWillNoPublish(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-none-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/none", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	a := v0320Dial(t, b.Addr(), "w-none", true, 0) // 无 will
	_ = a.Close()
	v0350ExpectNoPublish(t, sub, 500*time.Millisecond)
}

// US-2：v5 DISCONNECT rc=0x04（disconnect-with-will）发布 will。
func TestV0360DisconnectRC04Publishes(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-rc4-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/rc4", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	a := v0360DialWill(t, b.Addr(), "w-rc4", "w/rc4", "forced", 0, false, 0, true, 0)
	if err := mqtt.EncodePacket(a, &mqtt.Disconnect{V5: true, ReasonCode: 4}); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
	if string(got.Payload) != "forced" {
		t.Fatalf("rc4 will: %+v", got)
	}
}

// US-2：will QoS1 → v5 granted≥1 订阅者收 QoS1 下行（在途窗口）。
func TestV0360WillQoS1Downlink(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-q1-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/q1", 1, false, false); g != 1 {
		t.Fatalf("granted: %d", g)
	}
	a := v0360DialWill(t, b.Addr(), "w-q1", "w/q1", "m1", 1, false, 0, true, 0)
	_ = a.Close()
	got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
	if got.QoS != 1 || got.PacketID == 0 {
		t.Fatalf("expected QoS1 downlink with PacketID: %+v", got)
	}
	if err := mqtt.EncodePacket(sub, &mqtt.Puback{PacketID: got.PacketID, V5: true}); err != nil {
		t.Fatal(err)
	}
}

// US-2：will QoS1 → 离线持久会话暂存，重连恢复重放。
func TestV0360WillOfflineStoreReplay(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-off", false, 60)
	if g := v0330Subscribe(t, sub, 1, "w/off", 1, false, false); g != 1 {
		t.Fatalf("granted: %d", g)
	}
	_ = sub.Close()
	v0350WaitSession(t, b, "w-off", -1)
	a := v0360DialWill(t, b.Addr(), "w-off-pub", "w/off", "stored", 1, false, 0, true, 0)
	_ = a.Close()
	v0350WaitSession(t, b, "w-off", 1) // 等 will 进入离线暂存
	r := v0320Dial(t, b.Addr(), "w-off", false, 60)
	got := v0320Expect(t, r, &mqtt.Publish{}).(*mqtt.Publish)
	if string(got.Payload) != "stored" {
		t.Fatalf("offline will replay: %+v", got)
	}
}

// US-2：Will Retain=1 → 发布后进入 retained store（v0.35 交叉）。
func TestV0360WillRetainStored(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	a := v0360DialWill(t, b.Addr(), "w-ret", "w/ret", "keep", 0, true, 0, true, 0)
	_ = a.Close()
	v0320WaitFor(t, 2*time.Second, "retained stored", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.retained["w/ret"] != nil
	})
	c := v0320Dial(t, b.Addr(), "w-ret-sub", true, 0)
	if g := v0330Subscribe(t, c, 1, "w/ret", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	got := v0320Expect(t, c, &mqtt.Publish{}).(*mqtt.Publish)
	if !got.Retain || string(got.Payload) != "keep" {
		t.Fatalf("retained will: %+v", got)
	}
}

// US-3：delay=1 → 等待窗内无消息，约 1s 后发布。
func TestV0360WillDelayDeferred(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-delay-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/delay", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	a := v0360DialWill(t, b.Addr(), "w-delay", "w/delay", "late", 0, false, 1, true, 0)
	_ = a.Close()
	v0350ExpectNoPublish(t, sub, 400*time.Millisecond) // delay 未到：无消息
	_ = sub.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))
	got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
	_ = sub.SetReadDeadline(time.Time{})
	if string(got.Payload) != "late" {
		t.Fatalf("delayed will: %+v", got)
	}
}

// US-3：delay=2 内同 ClientID 重连（恢复会话）→ 取消，不再发布。
func TestV0360WillDelayCancelOnReconnect(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-cancel-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/cancel", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	a := v0360DialWill(t, b.Addr(), "w-cancel", "w/cancel", "nope", 0, false, 2, false, 60)
	_ = a.Close()
	v0360WaitDetached(t, b, "w-cancel")
	v0360WaitPending(t, b, "w-cancel") // 等定时器注册完成再重连
	r := v0360DialWill(t, b.Addr(), "w-cancel", "", "", 0, false, 0, false, 60)
	v0320WaitFor(t, 2*time.Second, "canceled", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.pendingWills["w-cancel"] == nil
	})
	v0350ExpectNoPublish(t, sub, 2500*time.Millisecond) // 超过 delay 仍无消息
	_ = r
}

// US-3：Broker.Close 停止待发定时器（不 panic、无泄漏路径）。
func TestV0360WillDelayBrokerClose(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	a := v0360DialWill(t, b.Addr(), "w-close", "w/close", "x", 0, false, 5, true, 0)
	_ = a.Close()
	v0360WaitPending(t, b, "w-close")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	left := len(b.pendingWills)
	b.mu.Unlock()
	if left != 0 {
		t.Fatalf("pendingWills not cleared on Close: %d left", left)
	}
	time.Sleep(50 * time.Millisecond)
}

// US-2：会话接管（同 ClientID 持久意图）踢旧连接 → will 发布。
func TestV0360TakeoverPublish(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-tk-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/tk", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	a := v0360DialWill(t, b.Addr(), "w-tk", "w/tk", "taken", 0, false, 0, false, 60)
	r := v0320Dial(t, b.Addr(), "w-tk", false, 60) // 同 ClientID 接管
	got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
	if string(got.Payload) != "taken" {
		t.Fatalf("takeover will: %+v", got)
	}
	_ = r
	_ = a
}

// US-2：鉴权失败（CONNECT 被拒）不发布 will。
func TestV0360AuthRejectNoWill(t *testing.T) {
	cfg := BrokerConfig{Username: "u", Password: "p"}
	b, err := NewBrokerWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0360DialAuth(t, b.Addr(), "w-auth-sub", "u", "p")
	if g := v0330Subscribe(t, sub, 1, "w/auth", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	conn, err := net.Dial("tcp", b.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := mqtt.EncodePacket(conn, &mqtt.Connect{
		V5: true, ClientID: "w-auth", Username: "u", Password: "bad",
		WillTopic: "w/auth", WillMessage: "leak", WillQoS: 0,
	}); err != nil {
		t.Fatal(err)
	}
	p, err := mqtt.DecodePacketV(conn, true)
	if err != nil {
		t.Fatalf("read connack: %v", err)
	}
	if ca, ok := p.(*mqtt.Connack); !ok || ca.ReturnCode != mqtt.MQTTV5BadUserpass {
		t.Fatalf("expected auth reject, got %T %+v", p, p)
	}
	_ = conn.Close()
	v0350ExpectNoPublish(t, sub, 500*time.Millisecond)
}

// US-2 边界：will 空 payload + retain=1 → 清 store + 照常转发（v0.35 语义）。
func TestV0360WillEmptyPayloadRetain(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sub := v0320Dial(t, b.Addr(), "w-empty-sub", true, 0)
	if g := v0330Subscribe(t, sub, 1, "w/empty", 0, false, false); g != 0 {
		t.Fatalf("granted: %d", g)
	}
	pub := v0320Dial(t, b.Addr(), "w-empty-pub", true, 0)
	v0350PubRetain(t, pub, 1, "w/empty", "old", 1)
	first := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish) // 读掉先前的普通 fanout
	if string(first.Payload) != "old" {
		t.Fatalf("first publish: %+v", first)
	}
	v0320WaitFor(t, 2*time.Second, "stored first", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.retained["w/empty"] != nil
	})
	a := v0360DialWill(t, b.Addr(), "w-empty", "w/empty", "", 0, true, 0, true, 0)
	_ = a.Close()
	got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
	if len(got.Payload) != 0 {
		t.Fatalf("empty will forwarded payload: %+v", got)
	}
	v0320WaitFor(t, 2*time.Second, "store cleared", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.retained["w/empty"] == nil
	})
}
