package mqtt_test

// v0.35.0 MQTT 5.0 阶段四：保留消息（retain）client 端到端测试。

import (
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
	"edgeflow/pkg/mqttsim"
)

// 跨 client 全链：PublishRetain → 后订阅者收到 retained（内容断言）。
func TestV0350ClientRetainE2E(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "rt5-pub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	if err := pub.PublishRetain("rt5/dev1", 1, []byte("m-retain"), true); err != nil {
		t.Fatal(err)
	}

	sub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "rt5-sub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got := make(chan string, 4)
	if err := sub.Subscribe("rt5/dev1", 1, func(topic string, payload []byte) {
		got <- topic + "|" + string(payload)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		if g != "rt5/dev1|m-retain" {
			t.Fatalf("expected retained delivery, got %q", g)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for retained message")
	}
}

// 清除路径：空 payload + retain=true 删除保留消息 → 新订阅收不到。
func TestV0350ClientRetainClearE2E(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "rt5-clr-pub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	if err := pub.PublishRetain("rt5/clr", 1, []byte("m1"), true); err != nil {
		t.Fatal(err)
	}
	if err := pub.PublishRetain("rt5/clr", 1, nil, true); err != nil {
		t.Fatal(err)
	}

	sub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "rt5-clr-sub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got := make(chan string, 4)
	if err := sub.Subscribe("rt5/clr", 1, func(topic string, payload []byte) {
		got <- string(payload)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		t.Fatalf("cleared topic must not deliver, got %q", g)
	case <-time.After(300 * time.Millisecond):
	}
}

// RH=2 抑制：SubscribeWithOpts(RetainHandling=2) 不收 retained；
// RH=0 新订阅（另一 client）收到。
func TestV0350ClientRetainRH2E2E(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "rt5-rh2-pub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	if err := pub.PublishRetain("rt5/rh2", 1, []byte("m1"), true); err != nil {
		t.Fatal(err)
	}

	sub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "rt5-rh2-sub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got := make(chan string, 4)
	if err := sub.SubscribeWithOpts("rt5/rh2", mqtt.SubOpts{QoS: 1, RetainHandling: 2}, func(topic string, payload []byte) {
		got <- string(payload)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		t.Fatalf("RH=2 must suppress retained, got %q", g)
	case <-time.After(300 * time.Millisecond):
	}

	sub2, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "rt5-rh0-sub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub2.Close()
	got2 := make(chan string, 4)
	if err := sub2.Subscribe("rt5/rh2", 1, func(topic string, payload []byte) {
		got2 <- string(payload)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got2:
		if g != "m1" {
			t.Fatalf("expected m1 got %q", g)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting RH=0 retained")
	}
}

// QoS1 下发链路：retained（QoS1 存储）以 granted QoS1 下发，client 自动
// PUBACK 完成闭环（连续两轮订阅均可达）。
func TestV0350ClientRetainQoS1E2E(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "rt5-q1-pub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	if err := pub.PublishRetain("rt5/q1", 1, []byte("q1-payload"), true); err != nil {
		t.Fatal(err)
	}

	for i, cid := range []string{"rt5-q1-s1", "rt5-q1-s2"} {
		c, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: cid, ProtocolVersion5: true})
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan string, 4)
		if err := c.Subscribe("rt5/q1", 1, func(topic string, payload []byte) {
			got <- string(payload)
		}); err != nil {
			t.Fatal(err)
		}
		select {
		case g := <-got:
			if g != "q1-payload" {
				t.Fatalf("round %d: expected q1-payload got %q", i, g)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: timeout", i)
		}
		_ = c.Close()
	}
}
