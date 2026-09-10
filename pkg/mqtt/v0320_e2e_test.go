package mqtt_test

// v0.32.0 MQTT 5.0 阶段二：client 栈持久会话端到端（真实 Dial +
// mqttsim broker；覆盖 Options.PersistentSession / SessionPresent() /
// 离线 QoS1 恢复下发 → handler 收帧）。

import (
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
	"edgeflow/pkg/mqttsim"
)

func TestV0320ClientPersistentSessionE2E(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// 第一连接：v5 + 持久会话（SE=60s），订阅 t/e2e。
	c1, err := mqtt.Dial(b.Addr(), mqtt.Options{
		ClientID: "e2e-dev", ProtocolVersion5: true,
		PersistentSession: true, SessionExpiryMs: 60_000,
	})
	if err != nil {
		t.Fatalf("dial1: %v", err)
	}
	if c1.SessionPresent() {
		t.Fatal("first connect must not restore a session")
	}
	got1 := make(chan string, 4)
	if err := c1.Subscribe("t/e2e", 1, func(topic string, payload []byte) {
		got1 <- string(payload)
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	_ = c1.Close()
	time.Sleep(100 * time.Millisecond)

	// 离线期间 server 侧 QoS0 下发（不暂存）与 QoS1 上行消息（暂存）。
	if err := b.Publish("t/e2e", []byte("q0-dropped")); err != nil {
		t.Fatal(err)
	}
	cPub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "e2e-pub"})
	if err != nil {
		t.Fatal(err)
	}
	defer cPub.Close()
	if err := cPub.Publish("t/e2e", 1, []byte("offline-qos1")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// 重连：SessionPresent=1 + 离线消息经 handler 到达。
	got2 := make(chan string, 4)
	c2, err := mqtt.Dial(b.Addr(), mqtt.Options{
		ClientID: "e2e-dev", ProtocolVersion5: true,
		PersistentSession: true, SessionExpiryMs: 60_000,
	})
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	defer c2.Close()
	if !c2.SessionPresent() {
		t.Fatal("reconnect must restore session (Session Present)")
	}
	if err := c2.Subscribe("t/e2e", 1, func(topic string, payload []byte) {
		got2 <- string(payload)
	}); err != nil {
		t.Fatalf("subscribe2: %v", err)
	}
	select {
	case msg := <-got2:
		if msg != "offline-qos1" {
			t.Fatalf("offline msg mismatch: %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for offline message")
	}
	// 不应收到离线期间的 QoS0（best-effort 不暂存）。
	select {
	case msg := <-got2:
		t.Fatalf("unexpected extra delivery: %q", msg)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestV0320ClientShareSubscriptionE2E(t *testing.T) {
	b, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	var mu sync.Mutex
	counts := map[string]int{}
	mkClient := func(id string) *mqtt.Client {
		c, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: id, ProtocolVersion5: true})
		if err != nil {
			t.Fatalf("dial %s: %v", id, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if err := c.Subscribe("$share/g1/t/s", 0, func(_ string, _ []byte) {
			mu.Lock()
			counts[id]++
			mu.Unlock()
		}); err != nil {
			t.Fatalf("subscribe %s: %v", id, err)
		}
		return c
	}
	a := mkClient("share-a")
	_ = a
	b2 := mkClient("share-b")
	_ = b2

	pub, err := mqtt.Dial(b.Addr(), mqtt.Options{ClientID: "share-pub"})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	for i := 0; i < 6; i++ {
		if err := pub.Publish("t/s", 0, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if counts["share-a"] != 3 || counts["share-b"] != 3 {
		t.Fatalf("round-robin should be even: %+v", counts)
	}
}
