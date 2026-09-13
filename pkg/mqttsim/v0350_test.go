package mqttsim

// v0.35.0 MQTT 5.0 阶段四：保留消息（retain）面 + RH 语义测试（wire 层，
// 复用 v0320/v0330 辅助；v5 语义为主，3.1.1 基础覆盖 1 例）。

import (
	"errors"
	"net"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
)

// v0350DialV3 建立 3.1.1 裸连接（无属性区）。
func v0350DialV3(t *testing.T, addr, clientID string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := mqtt.EncodePacket(conn, &mqtt.Connect{ClientID: clientID}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	p, err := mqtt.DecodePacketV(conn, false)
	if err != nil {
		t.Fatalf("read connack: %v", err)
	}
	ca, ok := p.(*mqtt.Connack)
	if !ok || ca.ReturnCode != 0 {
		t.Fatalf("expected CONNACK ok, got %T %+v", p, p)
	}
	return conn
}

// v0350SubscribeRH v5 订阅（带 RH 选项），断言 SUBACK 并返回 granted。
func v0350SubscribeRH(t *testing.T, conn net.Conn, id uint16, filter string, reqQoS byte, rh byte) byte {
	t.Helper()
	tf := mqtt.TopicFilter{Topic: filter, QoS: reqQoS, RetainHandling: rh}
	if err := mqtt.EncodePacket(conn, &mqtt.Subscribe{V5: true, PacketID: id, Topics: []mqtt.TopicFilter{tf}}); err != nil {
		t.Fatal(err)
	}
	p := v0320Expect(t, conn, &mqtt.Suback{PacketID: id})
	sa, ok := p.(*mqtt.Suback)
	if !ok || len(sa.Codes) != 1 {
		t.Fatalf("expected SUBACK got %T %+v", p, p)
	}
	return sa.Codes[0]
}

// v0350ExpectNoPublish 断言 d 内无下行输出（读超时 = 通过；读到任何
// 字节 = 失败）。注意不能用 DecodePacketV 判定：其 io.ReadFull 失败会
// 统一返回 ErrMalformedFixedHeader（吞掉超时错误），无法区分「无数据」
// 与「有垃圾字节」。
func v0350ExpectNoPublish(t *testing.T, conn net.Conn, d time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, 1)
	nn, err := conn.Read(buf)
	if nn > 0 {
		t.Fatalf("expected no downlink, got %d byte(s): % x", nn, buf[:nn])
	}
	var ne net.Error
	if err == nil || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("expected read timeout, got err=%v", err)
	}
}

// v0350PubRetain 以 QoS1 发布一条 retain 消息并等待 PUBACK（发布完成 =
// retained 已存储，QoS1 收到 ack 时存储已生效）。
func v0350PubRetain(t *testing.T, conn net.Conn, id uint16, topic, payload string, qos byte) {
	t.Helper()
	pk := &mqtt.Publish{V5: true, QoS: qos, PacketID: id, Topic: topic, Payload: []byte(payload), Retain: true}
	if err := mqtt.EncodePacket(conn, pk); err != nil {
		t.Fatal(err)
	}
	if qos == 1 {
		v0320Expect(t, conn, &mqtt.Puback{PacketID: id})
	}
}

// US-1/US-2 基础：存储 + 新订阅下发（RETAIN=1、QoS=min、PUBACK 闭环）。
func TestV0350RetainStoreDeliverBasic(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub := v0320Dial(t, b.Addr(), "rt-pub", true, 0)
	v0350PubRetain(t, pub, 7, "rt/a", "m1", 1)

	sub := v0320Dial(t, b.Addr(), "rt-sub", true, 0)
	if g := v0350SubscribeRH(t, sub, 1, "rt/a", 1, 0); g != 1 {
		t.Fatalf("granted: %d", g)
	}
	// SUBACK 已先行（辅助内断言）；随后应为 retained 下发。
	p := v0320Expect(t, sub, &mqtt.Publish{})
	got := p.(*mqtt.Publish)
	if !got.Retain || got.QoS != 1 || got.Topic != "rt/a" || string(got.Payload) != "m1" {
		t.Fatalf("retained mismatch: %+v", got)
	}
	if err := mqtt.EncodePacket(sub, &mqtt.Puback{V5: true, PacketID: got.PacketID}); err != nil {
		t.Fatal(err)
	}
}

// US-1：覆盖——同主题只保留最新。
func TestV0350RetainOverwrite(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub := v0320Dial(t, b.Addr(), "rt-ow-pub", true, 0)
	v0350PubRetain(t, pub, 1, "rt/ow", "v1", 1)
	v0350PubRetain(t, pub, 2, "rt/ow", "v2", 1)

	sub := v0320Dial(t, b.Addr(), "rt-ow-sub", true, 0)
	v0350SubscribeRH(t, sub, 1, "rt/ow", 1, 0)
	got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
	if string(got.Payload) != "v2" {
		t.Fatalf("expected v2 got %q", got.Payload)
	}
	v0350ExpectNoPublish(t, sub, 150*time.Millisecond)
}

// US-1：空 payload 清除 + 空消息照常转发给现有订阅者；新订阅不再收到。
func TestV0350RetainClearEmptyPayload(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub := v0320Dial(t, b.Addr(), "rt-clr-pub", true, 0)
	v0350PubRetain(t, pub, 1, "rt/clr", "m1", 1)

	sub1 := v0320Dial(t, b.Addr(), "rt-clr-sub1", true, 0)
	v0350SubscribeRH(t, sub1, 1, "rt/clr", 1, 0)
	if got := v0320Expect(t, sub1, &mqtt.Publish{}).(*mqtt.Publish); string(got.Payload) != "m1" {
		t.Fatalf("pre-clear retained: %q", got.Payload)
	}

	// 空 payload + retain=1 → 清除；空消息本身照常转发。
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: 2, Topic: "rt/clr", Payload: nil, Retain: true}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, pub, &mqtt.Puback{PacketID: 2})
	empty := v0320Expect(t, sub1, &mqtt.Publish{}).(*mqtt.Publish)
	if len(empty.Payload) != 0 {
		t.Fatalf("expected empty forwarded payload, got %q", empty.Payload)
	}

	// 新订阅：无 retained。
	sub2 := v0320Dial(t, b.Addr(), "rt-clr-sub2", true, 0)
	v0350SubscribeRH(t, sub2, 1, "rt/clr", 1, 0)
	v0350ExpectNoPublish(t, sub2, 150*time.Millisecond)
}

// US-1/US-2：QoS0 retain 存储（「SHOULD store」选择）→ 下发 QoS0。
func TestV0350RetainQoS0(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub := v0320Dial(t, b.Addr(), "rt-q0-pub", true, 0)
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 0, Topic: "rt/q0", Payload: []byte("m0"), Retain: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond) // QoS0 无 ack：等待 broker 处理

	sub := v0320Dial(t, b.Addr(), "rt-q0-sub", true, 0)
	v0350SubscribeRH(t, sub, 1, "rt/q0", 0, 0)
	got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
	if !got.Retain || got.QoS != 0 || string(got.Payload) != "m0" {
		t.Fatalf("qos0 retained mismatch: %+v", got)
	}
}

// US-1/US-4：QoS2 retain——PUBREL 完成时存储（存量修复锚：parked 保留
// Retain 字段）；下发 QoS=min(2,granted)=1。
func TestV0350RetainQoS2(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub := v0320Dial(t, b.Addr(), "rt-q2-pub", true, 0)
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 2, PacketID: 5, Topic: "rt/q2", Payload: []byte("m2"), Retain: true}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, pub, &mqtt.Pubrec{PacketID: 5})
	if err := mqtt.EncodePacket(pub, &mqtt.Pubrel{PacketID: 5}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, pub, &mqtt.Pubcomp{PacketID: 5})

	sub := v0320Dial(t, b.Addr(), "rt-q2-sub", true, 0)
	if g := v0350SubscribeRH(t, sub, 1, "rt/q2", 1, 0); g != 1 {
		t.Fatalf("granted: %d", g)
	}
	got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
	if !got.Retain || got.QoS != 1 || string(got.Payload) != "m2" {
		t.Fatalf("qos2 retained mismatch: %+v", got)
	}
}

// US-2：RH=1——仅「新订阅」下发；重复订阅（同 filter）不下发。
func TestV0350RetainRH1NewOnly(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub := v0320Dial(t, b.Addr(), "rt-rh1-pub", true, 0)
	v0350PubRetain(t, pub, 1, "rt/rh1", "m1", 1)

	sub := v0320Dial(t, b.Addr(), "rt-rh1-sub", true, 0)
	if g := v0350SubscribeRH(t, sub, 1, "rt/rh1", 1, 1); g != 1 {
		t.Fatalf("granted: %d", g)
	}
	if got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish); string(got.Payload) != "m1" {
		t.Fatalf("first subscribe should deliver: %q", got.Payload)
	}
	// 重复订阅同 filter（已存在）→ RH=1 不下发。
	v0350SubscribeRH(t, sub, 2, "rt/rh1", 1, 1)
	v0350ExpectNoPublish(t, sub, 150*time.Millisecond)
}

// US-2：RH=2——不发送；改以 RH=0 重订（总是）→ 发送。
func TestV0350RetainRH2Suppress(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub := v0320Dial(t, b.Addr(), "rt-rh2-pub", true, 0)
	v0350PubRetain(t, pub, 1, "rt/rh2", "m1", 1)

	sub := v0320Dial(t, b.Addr(), "rt-rh2-sub", true, 0)
	v0350SubscribeRH(t, sub, 1, "rt/rh2", 1, 2)
	v0350ExpectNoPublish(t, sub, 150*time.Millisecond)

	// RH=0 重订（同 filter 覆盖）→ 仍发送（「总是」语义）。
	v0350SubscribeRH(t, sub, 2, "rt/rh2", 1, 0)
	if got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish); string(got.Payload) != "m1" {
		t.Fatalf("RH=0 resubscribe should deliver: %q", got.Payload)
	}
}

// US-2：通配匹配多条 + 字典序 + 一次 SUBSCRIBE 内按主题去重。
func TestV0350RetainWildcardOrderedDedup(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub := v0320Dial(t, b.Addr(), "rt-wc-pub", true, 0)
	v0350PubRetain(t, pub, 1, "rt/w/c", "c", 1)
	v0350PubRetain(t, pub, 2, "rt/w/a", "a", 1)
	v0350PubRetain(t, pub, 3, "rt/w/b", "b", 1)

	sub := v0320Dial(t, b.Addr(), "rt-wc-sub", true, 0)
	v0350SubscribeRH(t, sub, 1, "rt/w/#", 1, 0)
	for _, want := range []string{"a", "b", "c"} {
		got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
		if string(got.Payload) != want {
			t.Fatalf("expected %q got %q", want, got.Payload)
		}
	}
	// 一次 SUBSCRIBE 双 filter 重叠（rt/w/# 与 rt/w/a 均匹配 a）→ 去重后仍 3 条。
	if err := mqtt.EncodePacket(sub, &mqtt.Subscribe{V5: true, PacketID: 2, Topics: []mqtt.TopicFilter{
		{Topic: "rt/w/#", QoS: 1}, {Topic: "rt/w/a", QoS: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, sub, &mqtt.Suback{PacketID: 2})
	for _, want := range []string{"a", "b", "c"} {
		got := v0320Expect(t, sub, &mqtt.Publish{}).(*mqtt.Publish)
		if string(got.Payload) != want {
			t.Fatalf("dedup pass expected %q got %q", want, got.Payload)
		}
	}
	v0350ExpectNoPublish(t, sub, 150*time.Millisecond)
}

// US-3：RAP 转发——RAP=1 订阅保留 RETAIN=1；RAP=0（默认）转发 RETAIN=0。
func TestV0350RetainRAPForward(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	subRAP := v0320Dial(t, b.Addr(), "rt-rap1", true, 0)
	if g := v0330Subscribe(t, subRAP, 1, "rt/rap/o", 1, false, true); g != 1 {
		t.Fatalf("rap granted: %d", g)
	}
	subPlain := v0320Dial(t, b.Addr(), "rt-rap0", true, 0)
	if g := v0330Subscribe(t, subPlain, 1, "rt/rap/o", 1, false, false); g != 1 {
		t.Fatalf("plain granted: %d", g)
	}

	pub := v0320Dial(t, b.Addr(), "rt-rap-pub", true, 0)
	v0350PubRetain(t, pub, 1, "rt/rap/o", "m1", 1)

	gotRAP := v0320Expect(t, subRAP, &mqtt.Publish{}).(*mqtt.Publish)
	if !gotRAP.Retain {
		t.Fatalf("RAP=1 forward must keep RETAIN=1: %+v", gotRAP)
	}
	gotPlain := v0320Expect(t, subPlain, &mqtt.Publish{}).(*mqtt.Publish)
	if gotPlain.Retain {
		t.Fatalf("RAP=0 forward must clear RETAIN: %+v", gotPlain)
	}
}

// US-1：retain=0 发布不存储——现订阅者收到（RETAIN=0），新订阅收不到。
func TestV0350RetainPlainNoStore(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub1 := v0320Dial(t, b.Addr(), "rt-pl-sub1", true, 0)
	v0350SubscribeRH(t, sub1, 1, "rt/pl", 1, 0)
	// 无 retained（未发布过）→ 无下发；确认订阅已完成。
	v0350ExpectNoPublish(t, sub1, 100*time.Millisecond)

	pub := v0320Dial(t, b.Addr(), "rt-pl-pub", true, 0)
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: 1, Topic: "rt/pl", Payload: []byte("m1")}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, pub, &mqtt.Puback{PacketID: 1})
	got := v0320Expect(t, sub1, &mqtt.Publish{}).(*mqtt.Publish)
	if got.Retain {
		t.Fatalf("plain publish forward must clear RETAIN: %+v", got)
	}

	sub2 := v0320Dial(t, b.Addr(), "rt-pl-sub2", true, 0)
	v0350SubscribeRH(t, sub2, 1, "rt/pl", 1, 0)
	v0350ExpectNoPublish(t, sub2, 150*time.Millisecond)
}

// US-2：3.1.1 基础——发布/订阅均 3.1.1；retained 下发 RETAIN=1、下行
// QoS0（3.1.1 下行恒 0 现状）。
func TestV0350RetainV311(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub := v0350DialV3(t, b.Addr(), "rt-v3-pub")
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{QoS: 1, PacketID: 1, Topic: "rt/v3", Payload: []byte("m1"), Retain: true}); err != nil {
		t.Fatal(err)
	}
	v0330ExpectV3(t, pub, &mqtt.Puback{PacketID: 1})

	sub := v0350DialV3(t, b.Addr(), "rt-v3-sub")
	if err := mqtt.EncodePacket(sub, &mqtt.Subscribe{PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "rt/v3", QoS: 1}}}); err != nil {
		t.Fatal(err)
	}
	v0330ExpectV3(t, sub, &mqtt.Suback{PacketID: 1})
	got := v0330ExpectPublishV3(t, sub, 0)
	if !got.Retain || string(got.Payload) != "m1" {
		t.Fatalf("v3 retained mismatch: %+v", got)
	}
}

// US-2：QoS=min(存储, granted) 两方向锚。
func TestV0350RetainGrantMin(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub := v0320Dial(t, b.Addr(), "rt-min-pub", true, 0)
	v0350PubRetain(t, pub, 1, "rt/min1", "q1", 1)
	// QoS0 存储（无 ack；等待处理）。
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 0, Topic: "rt/min0", Payload: []byte("q0"), Retain: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	v0350PubRetain(t, pub, 3, "rt/min2", "q1b", 1)

	// granted 上限：订阅 QoS2 → granted=1；retained QoS1 → 下发 QoS1。
	sub1 := v0320Dial(t, b.Addr(), "rt-min-sub1", true, 0)
	if g := v0350SubscribeRH(t, sub1, 1, "rt/min1", 2, 0); g != 1 {
		t.Fatalf("granted: %d", g)
	}
	if got := v0320Expect(t, sub1, &mqtt.Publish{}).(*mqtt.Publish); got.QoS != 1 {
		t.Fatalf("min(1,1) expected QoS1: %+v", got)
	}

	// 订阅 granted=1，retained QoS0 → 下发 QoS0。
	sub2 := v0320Dial(t, b.Addr(), "rt-min-sub2", true, 0)
	v0350SubscribeRH(t, sub2, 1, "rt/min0", 1, 0)
	if got := v0320Expect(t, sub2, &mqtt.Publish{}).(*mqtt.Publish); got.QoS != 0 {
		t.Fatalf("min(0,1) expected QoS0: %+v", got)
	}

	// 订阅 granted=0，retained QoS1 → 下发 QoS0。
	sub3 := v0320Dial(t, b.Addr(), "rt-min-sub3", true, 0)
	v0350SubscribeRH(t, sub3, 1, "rt/min2", 0, 0)
	if got := v0320Expect(t, sub3, &mqtt.Publish{}).(*mqtt.Publish); got.QoS != 0 {
		t.Fatalf("min(1,0) expected QoS0: %+v", got)
	}
}

// v0350WaitSession 确定性等待会话条件（b.mu 下快照落定；sess.mu 下队列
// 条数）——避免固定 sleep 在重负载 / race 下的时序脆弱。
func v0350WaitSession(t *testing.T, b *Broker, cid string, wantOffline int) {
	t.Helper()
	v0320WaitFor(t, 2*time.Second, "session snapshot", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		s := b.sessions[cid]
		return s != nil && s.conn == nil && len(s.filters) > 0
	})
	if wantOffline >= 0 {
		v0320WaitFor(t, 2*time.Second, "offline stored", func() bool {
			b.mu.Lock()
			s := b.sessions[cid]
			b.mu.Unlock()
			if s == nil {
				return false
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			return len(s.offline) >= wantOffline
		})
	}
}

// US-3（复核 P1-1 修复锚）：RAP 离线重放——v5 持久会话 + RAP=1 的订阅者
// 断连期间发布 retain=1，重连恢复重放带 RETAIN=1；RAP=0 对照清 0。
func TestV0350RetainRAPOfflineReplay(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// RAP=1：订阅→断连（会话保留）→发布→重连→重放保留 RETAIN。
	s1 := v0320Dial(t, b.Addr(), "rap-off1", false, 60)
	if g := v0330Subscribe(t, s1, 1, "rt/off1", 1, false, true); g != 1 {
		t.Fatalf("rap granted: %d", g)
	}
	_ = s1.Close()
	v0350WaitSession(t, b, "rap-off1", -1) // 等断连快照落定
	pub := v0320Dial(t, b.Addr(), "rap-off1-pub", true, 0)
	v0350PubRetain(t, pub, 1, "rt/off1", "m1", 1)
	v0350WaitSession(t, b, "rap-off1", 1) // 等离线暂存完成
	r1 := v0320Dial(t, b.Addr(), "rap-off1", false, 60)
	got := v0320Expect(t, r1, &mqtt.Publish{}).(*mqtt.Publish)
	if !got.Retain {
		t.Fatalf("RAP=1 offline replay must keep RETAIN=1: %+v", got)
	}
	if string(got.Payload) != "m1" {
		t.Fatalf("payload: %q", got.Payload)
	}

	// RAP=0 对照：重放清 RETAIN。
	s2 := v0320Dial(t, b.Addr(), "rap-off2", false, 60)
	if g := v0330Subscribe(t, s2, 1, "rt/off2", 1, false, false); g != 1 {
		t.Fatalf("plain granted: %d", g)
	}
	_ = s2.Close()
	v0350WaitSession(t, b, "rap-off2", -1)
	v0350PubRetain(t, pub, 2, "rt/off2", "m2", 1)
	v0350WaitSession(t, b, "rap-off2", 1)
	r2 := v0320Dial(t, b.Addr(), "rap-off2", false, 60)
	got2 := v0320Expect(t, r2, &mqtt.Publish{}).(*mqtt.Publish)
	if got2.Retain {
		t.Fatalf("RAP=0 offline replay must clear RETAIN: %+v", got2)
	}
}

// US-2（复核低置信备注闭环）：畸形订阅选项字节——RH 超界（3）与保留位
// （bit6-7 非 0）被拒（连接关闭，无 SUBACK）。
func TestV0350SubscribeMalformedOptionsRejected(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	cases := map[string]byte{
		"rh3":     0x30, // RH=3 超界
		"resbits": 0xC0, // bit6-7 保留位非 0
	}
	for name, opt := range cases {
		c := v0320Dial(t, b.Addr(), "bad-opt-"+name, true, 0)
		// 手工字节：fixed(0x82) + rl(7) + pktID(2) + props(0) + filter("x") + options。
		wire := []byte{0x82, 0x07, 0x00, 0x01, 0x00, 0x00, 0x01, 'x', opt}
		if _, err := c.Write(wire); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 1)
		nn, err := c.Read(buf)
		var ne net.Error
		if nn > 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
			t.Fatalf("%s: expected connection close, got n=%d err=%v", name, nn, err)
		}
	}
}
