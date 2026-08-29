package mqtt

import (
	"net"
	"sync"
	"testing"
	"time"
)

// fakeBroker is a minimal MQTT broker built directly on the codec primitives
// for self-contained client-layer tests. Per connection it:
//   - answers CONNECT with CONNACK{returnCode}
//   - answers SUBSCRIBE with SUBACK echoing each requested QoS
//   - answers QoS1 PUBLISH with PUBACK and records every PUBLISH
//   - counts PINGREQ
//   - optionally pushes one PUBLISH{pushTopic} to the client after the
//     pushAfter-th received PUBLISH (0 disables)
type fakeBroker struct {
	ln          net.Listener
	connackCode byte
	pushAfter   int
	pushTopic   string

	mu     sync.Mutex
	pubs   []*Publish
	subs   []*Subscribe
	pings  int
	conns  int
	pushed bool
}

func startBroker(t *testing.T) *fakeBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	b := &fakeBroker{ln: ln, pushTopic: "broker/push"}
	go b.acceptLoop()
	return b
}

func (b *fakeBroker) addr() string { return b.ln.Addr().String() }

func (b *fakeBroker) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.serve(conn)
	}
}

func (b *fakeBroker) serve(conn net.Conn) {
	b.mu.Lock()
	b.conns++
	b.mu.Unlock()
	defer conn.Close()
	for {
		p, err := decodePacket(conn)
		if err != nil {
			return
		}
		switch pv := p.(type) {
		case *Connect:
			_ = encodePacket(conn, &Connack{ReturnCode: b.connackCode})
		case *Subscribe:
			b.mu.Lock()
			b.subs = append(b.subs, pv)
			b.mu.Unlock()
			codes := make([]byte, len(pv.Topics))
			for i, tf := range pv.Topics {
				codes[i] = tf.QoS
			}
			_ = encodePacket(conn, &Suback{PacketID: pv.PacketID, Codes: codes})
		case *Publish:
			b.mu.Lock()
			b.pubs = append(b.pubs, pv)
			n := len(b.pubs)
			push := b.pushAfter > 0 && n >= b.pushAfter && !b.pushed
			b.pushed = b.pushed || push
			b.mu.Unlock()
			if pv.QoS == 1 {
				_ = encodePacket(conn, &Puback{PacketID: pv.PacketID})
			}
			if push {
				_ = encodePacket(conn, &Publish{Topic: b.pushTopic, Payload: []byte("hello")})
			}
		case *Pingreq:
			b.mu.Lock()
			b.pings++
			b.mu.Unlock()
		case *Disconnect:
			return
		}
	}
}

func (b *fakeBroker) recvPubs() []*Publish {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*Publish, len(b.pubs))
	copy(out, b.pubs)
	return out
}

func (b *fakeBroker) pingCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pings
}

// TestMatchTopic covers MQTT-4.7 wildcard matching: '+' single-level,
// trailing '#' (including the parent topic), empty levels from
// leading/trailing '/', and the MQTT-4.7.2 non-normative $-topic convention.
func TestMatchTopic(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"a/b/c", "a/b/c", true},
		{"a/b/c", "a/b/d", false},
		{"a/b/c", "a/b/c/d", false},
		{"a/+/c", "a/b/c", true},
		{"a/+/c", "a//c", true},     // '+' matches an empty level
		{"a/+/c", "a/b/c/d", false}, // '+' matches exactly one level
		{"a/+", "a/b", true},
		{"a/+", "a", false},
		{"a/+", "a/b/c", false},
		{"+/b/c", "a/b/c", true},
		{"#", "a", true},
		{"#", "a/b/c", true},
		{"a/#", "a", true}, // "x/#" matches "x" itself
		{"a/#", "a/b", true},
		{"a/#", "a/b/c/d", true},
		{"a/#", "b", false},
		{"a/#/c", "a/b/c", false}, // '#' must be the last level
		{"/finance", "/finance", true},
		{"/finance", "finance", false}, // leading '/' is an empty level
		{"finance", "/finance", false},
		{"/+", "/finance", true},
		{"/+", "/a/b", false},
		{"finance/", "finance/", true}, // trailing '/' is an empty level
		{"finance/+", "finance/", true},
		// MQTT-4.7.2 non-normative: $-topics are not matched by top-level
		// wildcards, only by filters starting with the same $-level.
		{"#", "$SYS/broker", false},
		{"+", "$SYS/broker", false},
		{"$SYS/#", "$SYS/broker", true},
		{"$SYS/#", "$SYS/broker/load", true},
		{"$SYS/+", "$SYS/broker", true},
		{"$SYS/#", "a/b", false},
	}
	for _, tc := range cases {
		if got := MatchTopic(tc.filter, tc.topic); got != tc.want {
			t.Errorf("MatchTopic(%q, %q) = %v, want %v", tc.filter, tc.topic, got, tc.want)
		}
	}
}

// TestDialOK verifies the CONNECT/CONNACK handshake against the fake broker.
func TestDialOK(t *testing.T) {
	b := startBroker(t)
	c, err := Dial(b.addr(), Options{ClientID: "dial-ok", ConnectTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if b.pingCount() != 0 {
		t.Fatalf("unexpected pings: %d", b.pingCount())
	}
}

// TestDialBadReturnCode verifies Dial fails when CONNACK reports refusal.
func TestDialBadReturnCode(t *testing.T) {
	b := startBroker(t)
	b.connackCode = 2 // bad client-ID / not authorized family, non-zero
	_, err := Dial(b.addr(), Options{ClientID: "dial-bad", ConnectTimeout: 2 * time.Second})
	if err == nil {
		t.Fatal("Dial with non-zero CONNACK return code: want error, got nil")
	}
}

// TestSubscribeBrokerPush subscribes '#', publishes once so the broker
// pushes a message back, and asserts the handler receives it.
func TestSubscribeBrokerPush(t *testing.T) {
	b := startBroker(t)
	b.pushAfter = 1
	b.pushTopic = "broker/push"

	c, err := Dial(b.addr(), Options{ClientID: "sub-push", ConnectTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	got := make(chan string, 4)
	err = c.Subscribe("#", 0, func(topic string, payload []byte) {
		got <- topic
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Publish("a/b", 0, []byte("trigger")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case topic := <-got:
		if topic != "broker/push" {
			t.Fatalf("handler got topic %q, want broker/push", topic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for broker-pushed PUBLISH to reach handler")
	}
	pubs := b.recvPubs()
	if len(pubs) != 1 || pubs[0].Topic != "a/b" || string(pubs[0].Payload) != "trigger" {
		t.Fatalf("broker received %+v, want one PUBLISH a/b=trigger", pubs)
	}
}

// TestPublishQoS0AndQoS1 asserts both QoS levels arrive at the broker side.
func TestPublishQoS0AndQoS1(t *testing.T) {
	b := startBroker(t)
	c, err := Dial(b.addr(), Options{ClientID: "pub-qos", ConnectTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Publish("t/zero", 0, []byte("p0")); err != nil {
		t.Fatalf("Publish QoS0: %v", err)
	}
	if err := c.Publish("t/one", 1, []byte("p1")); err != nil {
		t.Fatalf("Publish QoS1: %v", err)
	}
	if err := c.Publish("t/bad", 2, []byte("p2")); err == nil {
		t.Fatal("Publish QoS2: want error, got nil")
	}
	pubs := b.recvPubs()
	if len(pubs) != 2 {
		t.Fatalf("broker received %d PUBLISH, want 2", len(pubs))
	}
	if pubs[0].Topic != "t/zero" || string(pubs[0].Payload) != "p0" || pubs[0].QoS != 0 {
		t.Fatalf("QoS0 PUBLISH mismatch: %+v", pubs[0])
	}
	if pubs[1].Topic != "t/one" || string(pubs[1].Payload) != "p1" || pubs[1].QoS != 1 || pubs[1].PacketID == 0 {
		t.Fatalf("QoS1 PUBLISH mismatch: %+v", pubs[1])
	}
}

// TestWildcardDispatch pushes one PUBLISH on w/1/x and checks that both
// matching wildcard subscriptions fire while a non-matching one does not.
func TestWildcardDispatch(t *testing.T) {
	b := startBroker(t)
	b.pushAfter = 1
	b.pushTopic = "w/1/x"

	c, err := Dial(b.addr(), Options{ClientID: "wild", ConnectTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	hPlus := make(chan string, 4) // filter w/+/x  -> must fire
	hHash := make(chan string, 4) // filter w/1/#  -> must fire
	hMiss := make(chan string, 4) // filter s/+    -> must NOT fire
	if err := c.Subscribe("w/+/x", 0, func(topic string, _ []byte) { hPlus <- topic }); err != nil {
		t.Fatalf("Subscribe w/+/x: %v", err)
	}
	if err := c.Subscribe("w/1/#", 0, func(topic string, _ []byte) { hHash <- topic }); err != nil {
		t.Fatalf("Subscribe w/1/#: %v", err)
	}
	if err := c.Subscribe("s/+", 0, func(topic string, _ []byte) { hMiss <- topic }); err != nil {
		t.Fatalf("Subscribe s/+: %v", err)
	}
	if err := c.Publish("trigger", 0, []byte("x")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for _, ch := range []chan string{hPlus, hHash} {
		select {
		case topic := <-ch:
			if topic != "w/1/x" {
				t.Fatalf("handler got topic %q, want w/1/x", topic)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for wildcard handler")
		}
	}
	select {
	case topic := <-hMiss:
		t.Fatalf("s/+ handler unexpectedly fired for %q", topic)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestKeepAlivePing verifies the pinger sends PINGREQ on the configured
// interval until the broker has seen at least two of them.
func TestKeepAlivePing(t *testing.T) {
	b := startBroker(t)
	c, err := Dial(b.addr(), Options{ClientID: "ping", KeepAlive: 50 * time.Millisecond, ConnectTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for b.pingCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	c.Close()
	if got := b.pingCount(); got < 2 {
		t.Fatalf("broker saw %d PINGREQ, want >= 2", got)
	}
}

// TestCloseIdempotent verifies Close is safe to call repeatedly and that
// operations after Close fail instead of blocking.
func TestCloseIdempotent(t *testing.T) {
	b := startBroker(t)
	c, err := Dial(b.addr(), Options{ClientID: "close", ConnectTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close (idempotence): %v", err)
	}
	if err := c.Publish("after/close", 0, []byte("x")); err == nil {
		t.Fatal("Publish after Close: want error, got nil")
	}
	if err := c.Subscribe("after/#", 0, func(string, []byte) {}); err == nil {
		t.Fatal("Subscribe after Close: want error, got nil")
	}
}
