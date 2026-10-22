// v0570_test.go：共享订阅 granted-QoS1 下行单测（v0.57.0，spec 0030 B4）。
package mqttsim

import (
	"net"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
)

// US-1 共享订阅 QoS1 下行：v5 成员 $share 订阅 granted=1 → 发布 QoS1 后
// 直发 QoS1（PacketID 由 broker 分配）；client Puback 被 broker 容忍。
func TestV0570ShareQoS1Downlink(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub := v0320Dial(t, b.Addr(), "share-sub", true, 0)
	if got := v0330Subscribe(t, sub, 1, "$share/g1/t/s1", 1, false, false); got != 1 {
		t.Fatalf("共享订阅应 granted 1: %d", got)
	}
	pub := v0320Dial(t, b.Addr(), "share-pub", true, 0)
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: 9, Topic: "t/s1", Payload: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: 9})

	m := v0330ExpectPublish(t, sub, 1)
	if m.PacketID == 0 {
		t.Fatal("共享 QoS1 下行必须携带 packet id")
	}
	if m.Topic != "t/s1" {
		t.Fatalf("下行主题不符: %s", m.Topic)
	}
	if err := mqtt.EncodePacket(sub, &mqtt.Puback{V5: true, PacketID: m.PacketID}); err != nil {
		t.Fatal(err)
	}
}

// US-2 轮转保持：同组两成员各订 granted=1 → 两条 QoS1 消息恰好各得一份
// （round-robin 语义不变，QoS 升级不改变路由）。
func TestV0570ShareQoS1RoundRobin(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	subA := v0320Dial(t, b.Addr(), "share-a", true, 0)
	if got := v0330Subscribe(t, subA, 1, "$share/g2/t/s2", 1, false, false); got != 1 {
		t.Fatalf("成员 A granted: %d", got)
	}
	subB := v0320Dial(t, b.Addr(), "share-b", true, 0)
	if got := v0330Subscribe(t, subB, 1, "$share/g2/t/s2", 1, false, false); got != 1 {
		t.Fatalf("成员 B granted: %d", got)
	}
	pub := v0320Dial(t, b.Addr(), "share-pub", true, 0)
	for i := 0; i < 2; i++ {
		if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: uint16(10 + i), Topic: "t/s2", Payload: []byte("m")}); err != nil {
			t.Fatal(err)
		}
		v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: uint16(10 + i)})
	}
	// 轮转：A、B 各收恰好 1 条 QoS1。
	mA := v0330ExpectPublish(t, subA, 1)
	mB := v0330ExpectPublish(t, subB, 1)
	if mA.PacketID == 0 || mB.PacketID == 0 {
		t.Fatal("共享 QoS1 下行必须携带 packet id")
	}
	// 额外投递应为零（短超时无帧）——恰好一次路由。
	if err := subA.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err == nil {
		if p, err := mqtt.DecodePacketV(subA, true); err == nil {
			t.Fatalf("成员 A 不应有额外投递: %T", p)
		}
	}
}

// US-3 降级锚：v3.1.1 成员共享订阅 → 下行维持 QoS0 best-effort（冻结语义不变）。
func TestV0570ShareQoS0FallbackV3(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	c3, err := net.Dial("tcp", b.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	if err := mqtt.EncodePacket(c3, &mqtt.Connect{ClientID: "share-v3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mqtt.DecodePacketV(c3, false); err != nil {
		t.Fatal(err)
	}
	if err := mqtt.EncodePacket(c3, &mqtt.Subscribe{PacketID: 1, Topics: []mqtt.TopicFilter{{Topic: "$share/g3/t/s3", QoS: 1}}}); err != nil {
		t.Fatal(err)
	}
	p := v0330ExpectV3(t, c3, &mqtt.Suback{PacketID: 1})
	if sa := p.(*mqtt.Suback); sa.Codes[0] != 1 {
		t.Fatalf("v3 SUBACK 应回显请求 QoS: %d", sa.Codes[0])
	}

	pub := v0320Dial(t, b.Addr(), "share-pub-v3", true, 0)
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: 9, Topic: "t/s3", Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: 9})

	// v3 成员收 QoS0（无 packet id 要求）。
	m := v0330ExpectPublishV3(t, c3, 0)
	if m.Topic != "t/s3" {
		t.Fatalf("下行主题不符: %s", m.Topic)
	}
}
