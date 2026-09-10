package mqttsim

import (
	"bytes"
	"net"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
)

// v0330ExpectV3 按 3.1.1 形态解码（v0320Expect 恒 v5）。
func v0330ExpectV3(t *testing.T, conn net.Conn, want mqtt.Packet) mqtt.Packet {
	t.Helper()
	p, err := mqtt.DecodePacketV(conn, false)
	if err != nil {
		t.Fatalf("read %T (v3): %v", want, err)
	}
	if p.Type() != want.Type() {
		t.Fatalf("expected %T got %T", want, p)
	}
	return p
}

// v0330 辅助：v5 订阅并断言 granted code。
func v0330Subscribe(t *testing.T, conn net.Conn, id uint16, filter string, reqQoS byte, noLocal, rap bool) byte {
	t.Helper()
	tf := mqtt.TopicFilter{Topic: filter, QoS: reqQoS, NoLocal: noLocal, RetainAsPublished: rap}
	if err := mqtt.EncodePacket(conn, &mqtt.Subscribe{V5: true, PacketID: id, Topics: []mqtt.TopicFilter{tf}}); err != nil {
		t.Fatal(err)
	}
	p := v0320Expect(t, conn, &mqtt.Suback{V5: true, PacketID: id})
	sa, ok := p.(*mqtt.Suback)
	if !ok || len(sa.Codes) != 1 {
		t.Fatalf("expected SUBACK got %T", p)
	}
	return sa.Codes[0]
}

// v0330 辅助：读一个 PUBLISH（任意 pktID），校验 QoS。
func v0330ExpectPublish(t *testing.T, conn net.Conn, qos byte) *mqtt.Publish {
	t.Helper()
	p := v0320Expect(t, conn, &mqtt.Publish{})
	pub, ok := p.(*mqtt.Publish)
	if !ok {
		t.Fatalf("expected PUBLISH got %T", p)
	}
	if pub.QoS != qos {
		t.Fatalf("expected downlink QoS %d got %d", qos, pub.QoS)
	}
	return pub
}

// v0330ExpectPublishV3 按 3.1.1 形态读下行 PUBLISH（v3 帧无属性区）。
func v0330ExpectPublishV3(t *testing.T, conn net.Conn, qos byte) *mqtt.Publish {
	t.Helper()
	p := v0330ExpectV3(t, conn, &mqtt.Publish{})
	pub, ok := p.(*mqtt.Publish)
	if !ok {
		t.Fatalf("expected PUBLISH got %T", p)
	}
	if pub.QoS != qos {
		t.Fatalf("expected downlink QoS %d got %d", qos, pub.QoS)
	}
	return pub
}

// US-1：v5 订阅 QoS 请求 2 授予 1（cap，不拒绝）；请求 0 授予 0。
// v3.1.1 冻结锚：订阅 QoS1 → SUBACK 1、下行仍 QoS0。
func TestV0330GrantedQoSCap(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	c1 := v0320Dial(t, b.Addr(), "g1-dev", true, 0)
	if got := v0330Subscribe(t, c1, 1, "t/g1", 2, false, false); got != 1 {
		t.Fatalf("v5 request QoS2 must be granted 1: %d", got)
	}
	if got := v0330Subscribe(t, c1, 2, "t/g1b", 0, false, false); got != 0 {
		t.Fatalf("v5 request QoS0 must be granted 0: %d", got)
	}

	// v3.1.1 冻结锚。
	c3, err := net.Dial("tcp", b.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	if err := mqtt.EncodePacket(c3, &mqtt.Connect{ClientID: "g1-v3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mqtt.DecodePacketV(c3, false); err != nil {
		t.Fatal(err)
	}
	if err := mqtt.EncodePacket(c3, &mqtt.Subscribe{PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "t/v3", QoS: 1}}}); err != nil {
		t.Fatal(err)
	}
	p := v0330ExpectV3(t, c3, &mqtt.Suback{PacketID: 1})
	sa := p.(*mqtt.Suback)
	if sa.Codes[0] != 1 {
		t.Fatalf("v3 SUBACK must echo requested QoS (frozen): %d", sa.Codes[0])
	}

	// v3.1.1 冻结锚：发布 → v3 订阅者（granted 存档 1）下行仍 QoS0。
	if err := mqtt.EncodePacket(c3, &mqtt.Publish{QoS: 0, Topic: "t/v3", Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	v0330ExpectPublishV3(t, c3, 0)

	// v5 granted=0 下行仍 QoS0：c3 再发一条到 c1 的 granted=0 订阅主题。
	if err := mqtt.EncodePacket(c3, &mqtt.Publish{QoS: 0, Topic: "t/g1b", Payload: []byte("y")}); err != nil {
		t.Fatal(err)
	}
	v0330ExpectPublish(t, c1, 0)
}

// US-1/US-2 端到端：v5 订阅 QoS1 → 下行 QoS1 + PUBACK 闭环（granted 流）。
func TestV0330DownlinkQoS1EndToEnd(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub := v0320Dial(t, b.Addr(), "q1-sub", true, 0)
	if got := v0330Subscribe(t, sub, 1, "t/q1", 1, false, false); got != 1 {
		t.Fatal(got)
	}
	pub := v0320Dial(t, b.Addr(), "q1-pub", true, 0)
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: 9, Topic: "t/q1", Payload: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: 9})

	m := v0330ExpectPublish(t, sub, 1)
	if m.PacketID == 0 {
		t.Fatal("QoS1 downlink must carry packet id")
	}
	if err := mqtt.EncodePacket(sub, &mqtt.Puback{V5: true, PacketID: m.PacketID}); err != nil {
		t.Fatal(err)
	}
}

// US-2：窗口背压——单订阅灌入超窗口量 QoS1（慢消费者不 PUBACK），
// 后续条目积压会话队列（无崩溃、无死锁）；恢复 PUBACK 后滑动补发。
func TestV0330InflightWindowBackpressure(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub := v0320Dial(t, b.Addr(), "bp-sub", true, 0)
	if got := v0330Subscribe(t, sub, 1, "t/bp", 1, false, false); got != 1 {
		t.Fatal(got)
	}
	pub := v0320Dial(t, b.Addr(), "bp-pub", true, 0)
	for i := 0; i < 30; i++ {
		if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: uint16(i + 1), Topic: "t/bp", Payload: []byte("m")}); err != nil {
			t.Fatal(err)
		}
		v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: uint16(i + 1)})
	}
	// 订阅者只读前 recoverWindow(16) 条（直发），剩余 14 条积压队列。
	seen := map[uint16]bool{}
	for i := 0; i < 16; i++ {
		m := v0330ExpectPublish(t, sub, 1)
		seen[m.PacketID] = true
	}
	// PUBACK 全部在途 → 队列滑动补发剩余条目。
	for i := 0; i < 14; i++ {
		if err := mqtt.EncodePacket(sub, &mqtt.Puback{V5: true, PacketID: 0}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	count := 0
	for {
		_ = sub.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		p, err := mqtt.DecodePacketV(sub, true)
		if err != nil {
			break
		}
		if m, ok := p.(*mqtt.Publish); ok {
			count++
			seen[m.PacketID] = true
		}
	}
	if count == 0 {
		t.Fatal("expected slid backlog delivery after PUBACKs")
	}
}

// US-3：持久会话断连时在途条目 → 重连 DUP=1 重发；未下发条目 DUP=0。
func TestV0330ReconnectReplayDUP(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub := v0320Dial(t, b.Addr(), "dup-dev", false, 60)
	if got := v0330Subscribe(t, sub, 1, "t/dup", 1, false, false); got != 1 {
		t.Fatal(got)
	}
	pub := v0320Dial(t, b.Addr(), "dup-pub", true, 0)
	for i := 0; i < 20; i++ {
		if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: uint16(i + 1), Topic: "t/dup", Payload: []byte("m")}); err != nil {
			t.Fatal(err)
		}
		v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: uint16(i + 1)})
	}
	// 读全部 16 条直发在途，PUBACK 前 12 条 → dispatched 16→4，队列
	// 滑动补发 1 条（dispatched=5）；offline = [m13..m20]（8 条）。
	for i := 0; i < 16; i++ {
		m := v0330ExpectPublish(t, sub, 1)
		if i < 12 {
			if err := mqtt.EncodePacket(sub, &mqtt.Puback{V5: true, PacketID: m.PacketID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	time.Sleep(150 * time.Millisecond) // 等补发 m17 直发
	_ = sub.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if m, err := mqtt.DecodePacketV(sub, true); err != nil {
		t.Fatalf("expected slid delivery m17: %v", err)
	} else if mm := m.(*mqtt.Publish); mm.QoS != 1 {
		t.Fatalf("unexpected slid frame %T", m)
	}
	_ = sub.Close()
	time.Sleep(120 * time.Millisecond)

	sub2raw := v0320DialRaw(t, b.Addr(), "dup-dev", false, 60)
	if !sub2raw.connack.SessionPresent {
		t.Fatal("session must be preserved")
	}
	// 恢复下发 8 条：断连时刻全部曾入连接队列在途（本场景 offline ==
	// dispatched == 8）→ 全 DUP=1（spec 0006 US-3 重发语义）。
	for i := 0; i < 8; i++ {
		m := v0330ExpectPublish(t, sub2raw.conn, 1)
		if m.Dup != 1 {
			t.Fatalf("entry %d must be DUP=1 (was in-flight)", i)
		}
	}
}

// US-3 补充（复核 P1-1 回归锚）：断连期间离线暂存的条目（append 不动
// dispatched）从未下发 → 重连首传必须 DUP=0。
func TestV0330OfflineBacklogFirstDeliveryDUP0(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub := v0320Dial(t, b.Addr(), "off-dup", false, 60)
	if got := v0330Subscribe(t, sub, 1, "t/off", 1, false, false); got != 1 {
		t.Fatal(got)
	}
	_ = sub.Close() // 断连（会话保留，dispatched=0）
	time.Sleep(120 * time.Millisecond)

	// 离线期间两条 QoS1 上行 → 暂存会话队列（从未下发）。
	for i := 0; i < 2; i++ {
		pub := v0320Dial(t, b.Addr(), "off-pub", true, 0)
		if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: uint16(i + 1), Topic: "t/off", Payload: []byte("m")}); err != nil {
			t.Fatal(err)
		}
		v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: uint16(i + 1)})
		_ = pub.Close()
	}
	time.Sleep(80 * time.Millisecond)

	sub2raw := v0320DialRaw(t, b.Addr(), "off-dup", false, 60)
	if !sub2raw.connack.SessionPresent {
		t.Fatal("session must be preserved")
	}
	for i := 0; i < 2; i++ {
		m := v0330ExpectPublish(t, sub2raw.conn, 1)
		if m.Dup != 0 {
			t.Fatalf("offline-backlog entry %d must be DUP=0 (never delivered): %d", i, m.Dup)
		}
	}
}

// 复核 P2：0x23 为 PUBLISH 专用属性——CONNACK 属性区携带 → malformed。
func TestV0330TopicAliasPropScopeRejected(t *testing.T) {
	var buf bytes.Buffer
	// 手工构造 CONNACK（rc=0）+ 属性区 0x23。
	buf.Write([]byte{0x20, 0x08, 0x00, 0x00, 0x05, 0x23, 0x00, 0x05, 0x00})
	if _, err := mqtt.DecodePacketV(&buf, true); err == nil {
		t.Fatal("CONNACK carrying Topic Alias must be rejected")
	}
}

// 复核 P2：$share 订阅携带 NoLocal = 协议错误 → SUBACK 0x80。
func TestV0330ShareNoLocalRejected(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c := v0320Dial(t, b.Addr(), "share-nl", true, 0)
	if err := mqtt.EncodePacket(c, &mqtt.Subscribe{V5: true, PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "$share/g1/t/s", QoS: 0, NoLocal: true}}}); err != nil {
		t.Fatal(err)
	}
	p := v0320Expect(t, c, &mqtt.Suback{V5: true, PacketID: 1})
	sa := p.(*mqtt.Suback)
	if sa.Codes[0] != 0x80 {
		t.Fatalf("$share+NoLocal must be rejected 0x80, got 0x%02X", sa.Codes[0])
	}
}

// codec：v5 SUBSCRIBE decode 必须消费属性长度字节（阶段一起 encode/decode
// 不对称的存量缺陷修复锚——decodePermissive 之外的一般路径）。
func TestV0330SubscribeV5PropsLenDecode(t *testing.T) {
	var buf bytes.Buffer
	if err := mqtt.EncodePacket(&buf, &mqtt.Subscribe{V5: true, PacketID: 7, Topics: []mqtt.TopicFilter{{Topic: "t/x", QoS: 1, NoLocal: true}}}); err != nil {
		t.Fatal(err)
	}
	p, err := mqtt.DecodePacketV(&buf, true)
	if err != nil {
		t.Fatalf("v5 SUBSCRIBE decode: %v", err)
	}
	sub := p.(*mqtt.Subscribe)
	if sub.PacketID != 7 || len(sub.Topics) != 1 || sub.Topics[0].Topic != "t/x" || sub.Topics[0].QoS != 1 || !sub.Topics[0].NoLocal {
		t.Fatalf("v5 SUBSCRIBE roundtrip broken: %+v", sub)
	}
}

// US-4：NoLocal——发布者自己的匹配订阅不收（他人正常收到）。
func TestV0330NoLocal(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	self := v0320Dial(t, b.Addr(), "nl-self", true, 0)
	if got := v0330Subscribe(t, self, 1, "t/nl", 0, true, false); got != 0 {
		t.Fatal(got)
	}
	other := v0320Dial(t, b.Addr(), "nl-other", true, 0)
	if got := v0330Subscribe(t, other, 1, "t/nl", 0, false, false); got != 0 {
		t.Fatal(got)
	}

	if err := mqtt.EncodePacket(self, &mqtt.Publish{V5: true, QoS: 0, Topic: "t/nl", Payload: []byte("from-self")}); err != nil {
		t.Fatal(err)
	}
	v0330ExpectPublish(t, other, 0) // 他人正常收到
	// 自身不收：短超时读应超时。
	_ = self.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	if p, err := mqtt.DecodePacketV(self, true); err == nil {
		t.Fatalf("self must not receive own publish (nolocal), got %T", p)
	}
}

// US-5：Topic Alias 首包建映射、alias-only 路由正确；未知 alias 断开 0x94。
func TestV0330TopicAliasMapping(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub := v0320Dial(t, b.Addr(), "al-sub", true, 0)
	if got := v0330Subscribe(t, sub, 1, "t/al/#", 0, false, false); got != 0 {
		t.Fatal(got)
	}
	pub := v0320Dial(t, b.Addr(), "al-pub", true, 0)
	// 首包：主题 + alias 5 建映射。
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 0, Topic: "t/al/dev1", TopicAlias: 5, Payload: []byte("m1")}); err != nil {
		t.Fatal(err)
	}
	m1 := v0330ExpectPublish(t, sub, 0)
	if m1.Topic != "t/al/dev1" {
		t.Fatalf("first message must map to full topic: %q", m1.Topic)
	}
	// 第二包：alias-only（Topic=""）。
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 0, Topic: "", TopicAlias: 5, Payload: []byte("m2")}); err != nil {
		t.Fatal(err)
	}
	m2 := v0330ExpectPublish(t, sub, 0)
	if m2.Topic != "t/al/dev1" || string(m2.Payload) != "m2" {
		t.Fatalf("alias-only must resolve to mapped topic: %q", m2.Topic)
	}
	// 未知 alias（7）→ broker DISCONNECT 0x94 断开。
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 0, Topic: "", TopicAlias: 7, Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	p := v0320Expect(t, pub, &mqtt.Disconnect{V5: true, ReasonCode: mqtt.MQTTV5TopicAliasInvalid})
	if dc, ok := p.(*mqtt.Disconnect); !ok || dc.ReasonCode != mqtt.MQTTV5TopicAliasInvalid {
		t.Fatalf("expected DISCONNECT 0x94, got %T", p)
	}
}

// US-5：alias 映射表超限（>16 个不同别名）→ DISCONNECT 0x94。
func TestV0330TopicAliasOverflow(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub := v0320Dial(t, b.Addr(), "ao-sub", true, 0)
	if got := v0330Subscribe(t, sub, 1, "t/ao/#", 0, false, false); got != 0 {
		t.Fatal(got)
	}
	pub := v0320Dial(t, b.Addr(), "ao-pub", true, 0)
	for i := 1; i <= 16; i++ {
		if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 0, Topic: "t/ao/x" + string(rune('a'+i)), TopicAlias: uint16(i), Payload: []byte("m")}); err != nil {
			t.Fatal(err)
		}
	}
	// 第 17 个新别名 → 断开。
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 0, Topic: "t/ao/over", TopicAlias: 17, Payload: []byte("m")}); err != nil {
		t.Fatal(err)
	}
	p := v0320Expect(t, pub, &mqtt.Disconnect{V5: true, ReasonCode: mqtt.MQTTV5TopicAliasInvalid})
	if dc, ok := p.(*mqtt.Disconnect); !ok || dc.ReasonCode != mqtt.MQTTV5TopicAliasInvalid {
		t.Fatalf("expected DISCONNECT 0x94 on alias overflow, got %T", p)
	}
}

// codec：alias-only 帧编解码往返（client 与 broker 同用此 codec）。
func TestV0330AliasOnlyFrameRoundtrip(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// 直接构造 alias-only 帧并交给 broker（由 TestV0330TopicAliasMapping 覆盖
	// 线上路径）；此处校验 codec 层往返。
	pk := &mqtt.Publish{V5: true, QoS: 0, Topic: "", TopicAlias: 3, Payload: []byte("z")}
	buf := &bytes.Buffer{}
	if err := mqtt.EncodePacket(buf, pk); err != nil {
		t.Fatal(err)
	}
	back, err := mqtt.DecodePacketV(buf, true)
	if err != nil {
		t.Fatal(err)
	}
	got := back.(*mqtt.Publish)
	if got.TopicAlias != 3 || got.Topic != "" {
		t.Fatalf("alias-only roundtrip broken: %+v", got)
	}
}
