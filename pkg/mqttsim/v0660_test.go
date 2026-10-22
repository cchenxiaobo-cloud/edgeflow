// v0660_test.go：$queue 队列订阅单测（v0.66.0，spec 0040，持续项 B5）。
package mqttsim

import (
	"testing"

	"edgeflow/pkg/mqtt"
)

// US-1 $queue 队列订阅轮转：两个 $queue 订阅者轮转接收（每次恰一份）。
func TestV0660QueueRoundRobin(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	subA := v0320Dial(t, b.Addr(), "queue-a", true, 0)
	if got := v0330Subscribe(t, subA, 1, "$queue/t/work", 1, false, false); got != 1 {
		t.Fatalf("成员 A granted: %d", got)
	}
	subB := v0320Dial(t, b.Addr(), "queue-b", true, 0)
	if got := v0330Subscribe(t, subB, 1, "$queue/t/work", 1, false, false); got != 1 {
		t.Fatalf("成员 B granted: %d", got)
	}
	pub := v0320Dial(t, b.Addr(), "queue-pub", true, 0)
	for i := 0; i < 2; i++ {
		if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: uint16(10 + i), Topic: "t/work", Payload: []byte("m")}); err != nil {
			t.Fatal(err)
		}
		v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: uint16(10 + i)})
	}
	// 轮转：A、B 各收恰好 1 条 QoS1。
	mA := v0330ExpectPublish(t, subA, 1)
	mB := v0330ExpectPublish(t, subB, 1)
	if mA.Topic != "t/work" || mB.Topic != "t/work" {
		t.Fatalf("下行主题不符: %s %s", mA.Topic, mB.Topic)
	}
}

// US-2 $queue 与 $share 隔离：同主题的 $queue 与 $share/g 订阅各自独立轮转
// （互不干扰）。
func TestV0660QueueShareIsolation(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	qSub := v0320Dial(t, b.Addr(), "queue-only", true, 0)
	if got := v0330Subscribe(t, qSub, 1, "$queue/t/mix", 1, false, false); got != 1 {
		t.Fatalf("$queue granted: %d", got)
	}
	sSub := v0320Dial(t, b.Addr(), "share-only", true, 0)
	if got := v0330Subscribe(t, sSub, 1, "$share/g3/t/mix", 1, false, false); got != 1 {
		t.Fatalf("$share granted: %d", got)
	}
	pub := v0320Dial(t, b.Addr(), "mix-pub", true, 0)
	if err := mqtt.EncodePacket(pub, &mqtt.Publish{V5: true, QoS: 1, PacketID: 9, Topic: "t/mix", Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	v0320Expect(t, pub, &mqtt.Puback{V5: true, PacketID: 9})

	// $queue 和 $share/g3 各收到一份（两个独立组）。
	mQ := v0330ExpectPublish(t, qSub, 1)
	mS := v0330ExpectPublish(t, sSub, 1)
	if mQ.Topic != "t/mix" || mS.Topic != "t/mix" {
		t.Fatalf("下行主题不符: %s %s", mQ.Topic, mS.Topic)
	}
}
