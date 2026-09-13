package mqtt_test

// v0.36.0 MQTT 5.0 阶段五：遗嘱消息（will）client 端到端测试。

import (
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
	"edgeflow/pkg/mqttsim"
)

// US-4：client 配置 will 后正常 Close（DISCONNECT rc=0）不发布。
func TestV0360ClientWillNormalCloseNoPublish(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "w6-sub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got := make(chan string, 4)
	if err := sub.Subscribe("w6/dev", 1, func(topic string, payload []byte) {
		got <- topic + "|" + string(payload)
	}); err != nil {
		t.Fatal(err)
	}

	c, err := mqtt.Dial(b.Addr(), mqtt.Options{
		ClientID: "w6-client", ProtocolVersion5: true,
		WillTopic: "w6/dev", WillMessage: "should-not-appear",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil { // 正常关闭：发 DISCONNECT rc=0
		t.Fatal(err)
	}
	select {
	case g := <-got:
		t.Fatalf("normal close must not publish will, got %q", g)
	case <-time.After(600 * time.Millisecond):
	}
}

// US-4：会话接管踢连接（异常断连）→ will 发布（含重连 e2e）。
func TestV0360ClientWillTakeoverPublish(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "w6-tk-sub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got := make(chan string, 4)
	if err := sub.Subscribe("w6/tk", 1, func(topic string, payload []byte) {
		got <- topic + "|" + string(payload)
	}); err != nil {
		t.Fatal(err)
	}

	a, err := mqtt.Dial(b.Addr(), mqtt.Options{
		ClientID: "w6-tk", ProtocolVersion5: true, PersistentSession: true, SessionExpiryMs: 60000,
		WillTopic: "w6/tk", WillMessage: "gone",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = a

	// 同 ClientID 新连接（持久意图）→ 接管踢掉 a → will 发布。
	r, err := mqtt.Dial(b.Addr(), mqtt.Options{
		ClientID: "w6-tk", ProtocolVersion5: true, PersistentSession: true, SessionExpiryMs: 60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	select {
	case g := <-got:
		if g != "w6/tk|gone" {
			t.Fatalf("takeover will: %q", g)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for takeover will")
	}
}

// US-4：Will Retain=1 → 发布进入 retained store，后续订阅者收到。
func TestV0360ClientWillRetainStoreE2E(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	a, err := mqtt.Dial(b.Addr(), mqtt.Options{
		ClientID: "w6-rt", ProtocolVersion5: true, PersistentSession: true, SessionExpiryMs: 60000,
		WillTopic: "w6/rt", WillMessage: "kept", WillRetain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = a
	r, err := mqtt.Dial(b.Addr(), mqtt.Options{ // 接管踢 a → will 发布 + 落 store
		ClientID: "w6-rt", ProtocolVersion5: true, PersistentSession: true, SessionExpiryMs: 60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	sub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "w6-rt-sub", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got := make(chan string, 4)
	if err := sub.Subscribe("w6/rt", 1, func(topic string, payload []byte) {
		got <- topic + "|" + string(payload)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		if g != "w6/rt|kept" {
			t.Fatalf("retained will: %q", g)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for retained will")
	}
}
