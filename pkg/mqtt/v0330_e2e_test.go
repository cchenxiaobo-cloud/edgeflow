package mqtt_test

import (
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
	"edgeflow/pkg/mqttsim"
)

// 复核 P1-2 补测：client 出站 Topic Alias 端到端——同主题第二包走
// alias-only 帧，broker 解映射后订阅者仍按完整主题收到两条。
func TestV0330ClientPublishTopicAliasE2E(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "al-sub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 4)
	if err := sub.Subscribe("t/alias/#", 0, func(topic string, payload []byte) {
		got <- topic + "|" + string(payload)
	}); err != nil {
		t.Fatal(err)
	}

	pub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "al-pub", ProtocolVersion5: true, PublishTopicAlias: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish("t/alias/dev1", 0, []byte("m1")); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish("t/alias/dev1", 0, []byte("m2")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"t/alias/dev1|m1", "t/alias/dev1|m2"} {
		select {
		case g := <-got:
			if g != want {
				t.Fatalf("expected %q got %q", want, g)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting for %q", want)
		}
	}
}

// 复核 P1-2 补测：SubscribeWithOpts（NoLocal）端到端——自发不收、他人收。
func TestV0330ClientSubscribeOptsE2E(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	self, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "nl-self", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := self.SubscribeWithOpts("t/nl", mqtt.SubOpts{QoS: 0, NoLocal: true}, func(topic string, payload []byte) {
		t.Fatalf("self must not receive own publish (nolocal), got %s/%s", topic, payload)
	}); err != nil {
		t.Fatal(err)
	}

	other, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "nl-other", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 2)
	if err := other.Subscribe("t/nl", 0, func(topic string, payload []byte) {
		got <- string(payload)
	}); err != nil {
		t.Fatal(err)
	}

	if err := self.Publish("t/nl", 0, []byte("from-self")); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		if g != "from-self" {
			t.Fatalf("unexpected %q", g)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for other's delivery")
	}
	// 自身静默验证：短窗口无回调即通过（handler 内 Fatal 兜底）。
	time.Sleep(300 * time.Millisecond)
}
