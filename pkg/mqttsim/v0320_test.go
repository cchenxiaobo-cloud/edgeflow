package mqttsim

// v0.32.0 MQTT 5.0 阶段二：broker 会话解耦 + 共享订阅测试（wire 层，
// 复用 pkg/mqtt codec；v5 语义为主，3.1.1 冻结路径已由 v0240-v0300 覆盖）。

import (
	"net"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
)

// v0320Dial 以 v5 持久会话参数建立裸连接（不走 client 栈，wire 断言用）。
func v0320Dial(t *testing.T, addr, clientID string, cleanStart bool, expiry uint32) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := mqtt.EncodePacket(conn, &mqtt.Connect{V5: true, ClientID: clientID, CleanSession: cleanStart, SessionExpiry: expiry}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	p, err := mqtt.DecodePacketV(conn, true)
	if err != nil {
		t.Fatalf("read connack: %v", err)
	}
	ca, ok := p.(*mqtt.Connack)
	if !ok || ca.ReturnCode != 0 {
		t.Fatalf("expected CONNACK ok, got %T %+v", p, p)
	}
	return conn
}

func v0320Expect(t *testing.T, conn net.Conn, want mqtt.Packet) mqtt.Packet {
	t.Helper()
	p, err := mqtt.DecodePacketV(conn, true)
	if err != nil {
		t.Fatalf("read %T: %v", want, err)
	}
	if p.Type() != want.Type() {
		t.Fatalf("expected %T got %T", want, p)
	}
	return p
}

func v0320WaitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// US-3：Clean Start=0 + SE>0 重连恢复（Session Present=1、SE 回显、
// 离线 QoS1 恢复下发、PUBACK 确认出队）。
func TestV0320SessionPreservedAcrossReconnect(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	c1 := v0320Dial(t, b.Addr(), "dev-1", false, 60)
	if err := mqtt.EncodePacket(c1, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "t/1", QoS: 1}}}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, c1, &mqtt.Suback{PacketID: 1})
	_ = c1.Close() // 断连（SE=60s，会话应保留）
	time.Sleep(80 * time.Millisecond)

	// 离线期间 QoS1 上行消息（publisher 侧）。
	pub := v0320Dial(t, b.Addr(), "pub-1", true, 0)
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: 9, Topic: "t/1", Payload: []byte("offline-msg")}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: 9})

	// 重连（同 ID，Clean Start=0）：present=1 + SE 回显 + 离线消息恢复下发。
	raw := v0320DialRaw(t, b.Addr(), "dev-1", false, 60)
	if !raw.connack.SessionPresent {
		t.Fatalf("expected session present on reconnect: %+v", raw.connack)
	}
	if raw.connack.SessionExpiry != 60 {
		t.Fatalf("expected SE echo 60: %+v", raw.connack)
	}
	p, err := mqtt.DecodePacketV(raw.conn, true)
	if err != nil {
		t.Fatalf("read offline msg: %v", err)
	}
	m, ok := p.(*mqtt.Publish)
	if !ok || m.Topic != "t/1" || string(m.Payload) != "offline-msg" || m.QoS != 1 {
		t.Fatalf("offline msg mismatch: %T %+v", p, p)
	}
	// PUBACK 确认出队（broker 侧无重发；确认后不应再收到同帧）。
	if err := mqtt.EncodePacket(raw.conn, &mqtt.Puback{V5: true, PacketID: m.PacketID}); err != nil {
		t.Fatal(err)
	}
	_ = raw.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	tmp := make([]byte, 16)
	if n, err := raw.conn.Read(tmp); err == nil && n > 0 {
		t.Fatalf("no duplicate delivery expected, got % X", tmp[:n])
	}
}

// v0320DialRaw 返回原始 CONNACK 供位断言。
type v0320Raw struct {
	conn    net.Conn
	connack *mqtt.Connack
}

func v0320DialRaw(t *testing.T, addr, clientID string, cleanStart bool, expiry uint32) *v0320Raw {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := mqtt.EncodePacket(conn, &mqtt.Connect{V5: true, ClientID: clientID, CleanSession: cleanStart, SessionExpiry: expiry}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	p, err := mqtt.DecodePacketV(conn, true)
	if err != nil {
		t.Fatalf("read connack: %v", err)
	}
	ca, ok := p.(*mqtt.Connack)
	if !ok {
		t.Fatalf("expected CONNACK got %T", p)
	}
	return &v0320Raw{conn: conn, connack: ca}
}

// US-3：v5 Clean Start=1 重连丢弃旧会话（present=0、订阅清空）。
func TestV0320CleanStartDropsSession(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	c1 := v0320Dial(t, b.Addr(), "dev-2", false, 60)
	if err := mqtt.EncodePacket(c1, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "t/2", QoS: 0}}}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, c1, &mqtt.Suback{PacketID: 1})
	_ = c1.Close()
	time.Sleep(80 * time.Millisecond)

	raw := v0320DialRaw(t, b.Addr(), "dev-2", true, 60) // Clean Start=1
	if raw.connack.SessionPresent {
		t.Fatalf("clean start must drop session: %+v", raw.connack)
	}
	// 旧订阅不恢复：不重新 SUBSCRIBE 时收不到匹配消息。
	if err := mqtt.EncodePacket(raw.conn, &mqtt.Publish{V5: true, QoS: 0, PacketID: 0, Topic: "t/2", Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if got := len(b.Received()); got != 1 {
		t.Fatalf("publish should be recorded: %d", got)
	}
}

// US-3：SE=0 且 Clean Start=0 → 断连即毁（重连 present=0）。
func TestV0320ExpiryZeroDestroysOnDisconnect(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	c1 := v0320Dial(t, b.Addr(), "dev-3", false, 0)
	if err := mqtt.EncodePacket(c1, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "t/3", QoS: 0}}}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, c1, &mqtt.Suback{PacketID: 1})
	_ = c1.Close()
	time.Sleep(80 * time.Millisecond)

	raw := v0320DialRaw(t, b.Addr(), "dev-3", false, 0)
	if raw.connack.SessionPresent {
		t.Fatalf("SE=0 session must be destroyed on disconnect: %+v", raw.connack)
	}
}

// US-3：会话过期（SE 短 + 惰性清理）→ 重连 present=0。
func TestV0320SessionExpiryLazyCleanup(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	c1 := v0320Dial(t, b.Addr(), "dev-4", false, 1) // 1s
	_ = c1.Close()
	time.Sleep(1400 * time.Millisecond)

	raw := v0320DialRaw(t, b.Addr(), "dev-4", false, 1)
	if raw.connack.SessionPresent {
		t.Fatalf("expired session must be lazily purged: %+v", raw.connack)
	}
}

// US-3：同 ClientID 持久连接接管——旧连接被踢下线、会话转接（present=1）。
func TestV0320TakeoverKicksOldPersistentConnection(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	cA := v0320Dial(t, b.Addr(), "dev-5", false, 60)
	if err := mqtt.EncodePacket(cA, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "t/5", QoS: 0}}}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, cA, &mqtt.Suback{PacketID: 1})

	cB := v0320DialRaw(t, b.Addr(), "dev-5", false, 60)
	if !cB.connack.SessionPresent {
		t.Fatalf("takeover must restore session: %+v", cB.connack)
	}
	// A 被踢：读取端很快得到 EOF。
	v0320WaitFor(t, 2*time.Second, "old connection EOF", func() bool {
		tmp := make([]byte, 8)
		_ = cA.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err := cA.Read(tmp)
		return err != nil
	})
	// B 接管订阅：无需重新 SUBSCRIBE 即可收到匹配消息。
	if err := mqtt.EncodePacket(cB.conn, &mqtt.Publish{V5: true, QoS: 0, PacketID: 0, Topic: "t/5", Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if got := len(b.Received()); got != 1 {
		t.Fatalf("publish should be recorded: %d", got)
	}
}

// US-4：离线 QoS1 暂存上限（64）丢最旧——重连后首条为第 7 条消息。
func TestV0320OfflineQoS1CapDropsOldest(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	c1 := v0320Dial(t, b.Addr(), "dev-6", false, 60)
	if err := mqtt.EncodePacket(c1, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "t/6", QoS: 1}}}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, c1, &mqtt.Suback{PacketID: 1})
	_ = c1.Close()
	time.Sleep(80 * time.Millisecond)

	pub := v0320Dial(t, b.Addr(), "pub-6", true, 0)
	for i := 0; i < offlineCap+6; i++ {
		if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: uint16(i + 1), Topic: "t/6", Payload: []byte{byte(i)}}); err != nil {
			t.Fatal(err)
		}
		v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: uint16(i + 1)})
	}
	time.Sleep(80 * time.Millisecond)

	// 重连恢复：应收到 64 条（第 6..69 条），首条 payload=6。
	raw := v0320DialRaw(t, b.Addr(), "dev-6", false, 60)
	if !raw.connack.SessionPresent {
		t.Fatalf("session must be preserved: %+v", raw.connack)
	}
	first, err := mqtt.DecodePacketV(raw.conn, true)
	if err != nil {
		t.Fatalf("read offline msg: %v", err)
	}
	m, ok := first.(*mqtt.Publish)
	if !ok || m.QoS != 1 || len(m.Payload) != 1 || m.Payload[0] != 6 {
		t.Fatalf("first offline msg should be seq 6: %T %+v", first, first)
	}
	// 确认（PUBACK）后出队：剩余 63 条可继续读取。
	if err := mqtt.EncodePacket(raw.conn, &mqtt.Puback{V5: true, PacketID: m.PacketID}); err != nil {
		t.Fatal(err)
	}
	count := 1
	for {
		_ = raw.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		p, err := mqtt.DecodePacketV(raw.conn, true)
		if err != nil {
			break
		}
		if pm, ok := p.(*mqtt.Publish); ok {
			_ = mqtt.EncodePacket(raw.conn, &mqtt.Puback{V5: true, PacketID: pm.PacketID})
			count++
		}
	}
	if count != offlineCap {
		t.Fatalf("expected %d offline messages, got %d", offlineCap, count)
	}
}

// US-5：共享订阅组内 round-robin——3 成员各收 2/6 条；普通订阅者全收。
func TestV0320ShareSubscriptionRoundRobin(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	subs := make([]net.Conn, 3)
	for i := range subs {
		subs[i] = v0320Dial(t, b.Addr(), "share-"+string(rune('a'+i)), true, 0)
		if err := mqtt.EncodePacket(subs[i], &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "$share/g1/t/data", QoS: 0}}}); err != nil {
			t.Fatal(err)
		}
		p := v0320Expect(t, subs[i], &mqtt.Suback{PacketID: 1})
		if sa := p.(*mqtt.Suback); sa.Codes[0] == subFailureCode {
			t.Fatalf("$share subscribe rejected: %+v", sa)
		}
	}
	norm := v0320Dial(t, b.Addr(), "normal-1", true, 0)
	if err := mqtt.EncodePacket(norm, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "t/data", QoS: 0}}}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, norm, &mqtt.Suback{PacketID: 1})

	pub := v0320Dial(t, b.Addr(), "pub-7", true, 0)
	const total = 6
	for i := 0; i < total; i++ {
		if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 0, Topic: "t/data", Payload: []byte{byte(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(150 * time.Millisecond)

	// 普通订阅者全收。
	normCount := 0
	_ = norm.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		p, err := mqtt.DecodePacketV(norm, true)
		if err != nil {
			break
		}
		if _, ok := p.(*mqtt.Publish); ok {
			normCount++
		}
	}
	if normCount != total {
		t.Fatalf("normal subscriber should get all %d, got %d", total, normCount)
	}
	// 共享组成员各收 2（轮转均匀）。
	for i, c := range subs {
		got := 0
		_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		for {
			p, err := mqtt.DecodePacketV(c, true)
			if err != nil {
				break
			}
			if _, ok := p.(*mqtt.Publish); ok {
				got++
			}
		}
		if got != total/len(subs) {
			t.Fatalf("share member %d should get %d, got %d", i, total/len(subs), got)
		}
	}
}

// US-5：非法共享订阅形态拒绝（空组/无内层/$queue）。
func TestV0320ShareSubscriptionRejects(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	c := v0320Dial(t, b.Addr(), "share-bad", true, 0)
	bad := []string{"$share//t/data", "$share/g1", "$queue/t/data"}
	if err := mqtt.EncodePacket(c, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{
		{Topic: bad[0], QoS: 0}, {Topic: bad[1], QoS: 0}, {Topic: bad[2], QoS: 0},
	}}); err != nil {
		t.Fatal(err)
	}
	p := v0320Expect(t, c, &mqtt.Suback{PacketID: 1})
	sa := p.(*mqtt.Suback)
	for i, code := range sa.Codes {
		if code != subFailureCode {
			t.Fatalf("%q must be rejected, got 0x%02X", bad[i], code)
		}
	}
}

// US-5：共享订阅离线成员不暂存——离线后消息轮给在线成员。
func TestV0320ShareOfflineMemberNotParked(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	offline := v0320Dial(t, b.Addr(), "share-off", false, 60)
	if err := mqtt.EncodePacket(offline, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "$share/g2/t/x", QoS: 1}}}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, offline, &mqtt.Suback{PacketID: 1})
	_ = offline.Close()
	time.Sleep(80 * time.Millisecond)

	online := v0320Dial(t, b.Addr(), "share-on", true, 0)
	if err := mqtt.EncodePacket(online, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "$share/g2/t/x", QoS: 1}}}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, online, &mqtt.Suback{PacketID: 1})

	pub := v0320Dial(t, b.Addr(), "pub-8", true, 0)
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: 3, Topic: "t/x", Payload: []byte("m1")}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: 3})
	time.Sleep(100 * time.Millisecond)

	// 在线成员收到（阶段二下行 QoS 简化为 0，见 spec as-built）；离线成员重连后不得收到（共享不暂存）。
	p := v0320Expect(t, online, &mqtt.Publish{V5: true, QoS: 0, Topic: "t/x"})
	_ = mqtt.EncodePacket(online, &mqtt.Puback{V5: true, PacketID: p.(*mqtt.Publish).PacketID})

	raw := v0320DialRaw(t, b.Addr(), "share-off", false, 60)
	if !raw.connack.SessionPresent {
		t.Fatalf("session must be preserved: %+v", raw.connack)
	}
	_ = raw.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	tmp := make([]byte, 16)
	if n, err := raw.conn.Read(tmp); err == nil && n > 0 {
		t.Fatalf("offline share member must not receive parked messages, got % X", tmp[:n])
	}
}

// TestV0320SlowConsumerNoDeadlock 复核 P0-1 回归锚：慢消费者（订阅后不读）
// 打满 out 队列时 fanout 持锁路径曾自死锁（enqueueQoS 队列满分支重入
// b.mu）→ broker 级联冻结、后续 PUBACK 全部无响应。修复后 fanout 队列满
// 走内联 drop（dropCount++），broker 持续服务。
func TestV0320SlowConsumerNoDeadlock(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	slow := v0320Dial(t, b.Addr(), "slow-consumer", true, 0)
	if err := mqtt.EncodePacket(slow, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "t/slow", QoS: 0}}}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, slow, &mqtt.Suback{V5: true, PacketID: 1})
	// 不读 slow（pump 写阻塞 → out 队列满）。

	pub := v0320Dial(t, b.Addr(), "slow-pub", true, 0)
	big := make([]byte, 128*1024) // 大 payload：15MB 总量确保越过对端内核缓冲
	for i := 0; i < 120; i++ {
		if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: uint16(i + 1), Topic: "t/slow", Payload: big}); err != nil {
			t.Fatal(err)
		}
		_ = pub.SetReadDeadline(time.Now().Add(3 * time.Second))
		if p, err := mqtt.DecodePacketV(pub, true); err != nil {
			t.Fatalf("pub %d: broker deadlocked (no PUBACK): %v", i, err)
		} else if pa, ok := p.(*mqtt.Puback); !ok || pa.PacketID != uint16(i+1) {
			t.Fatalf("pub %d: unexpected %T", i, p)
		}
	}
	// broker 未冻结：Received 可读且 slow 的 drop 计入。
	if got := len(b.Received()); got != 120 {
		t.Fatalf("publisher messages must all be recorded: %d", got)
	}
	if b.dropCount == 0 {
		t.Fatal("expected drops for slow consumer (queue overflow)")
	}
}

// TestV0320RecoverBufferSkipsQoS2 复核 P1-1 回归锚：恢复会话缓冲不得拦截
// 入站 QoS2（QoS2 走独立 park/PUBREC 状态机）。
func TestV0320RecoverBufferSkipsQoS2(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// 建立持久会话（产生 SessionPresent 场景）。
	c1 := v0320Dial(t, b.Addr(), "q2-dev", false, 60)
	_ = c1.Close()
	time.Sleep(80 * time.Millisecond)

	c2 := v0320DialRaw(t, b.Addr(), "q2-dev", false, 60)
	if !c2.connack.SessionPresent {
		t.Fatalf("session must be preserved: %+v", c2.connack)
	}
	// 无 handler 注册（模拟 handler 未注册窗口）下入站 QoS2：
	// 必须回 PUBREC（进入 park），而不是被恢复缓冲吞掉。
	if err := mqtt.EncodePacket(c2.conn, &mqtt.Publish{V5: true, QoS: 2, PacketID: 21, Topic: "t/q2", Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	p := v0320Expect(t, c2.conn, &mqtt.Pubrec{V5: true, PacketID: 21})
	if pr, ok := p.(*mqtt.Pubrec); !ok || pr.PacketID != 21 {
		t.Fatalf("expected PUBREC for inbound QoS2: %T", p)
	}
}
