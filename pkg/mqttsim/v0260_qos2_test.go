package mqttsim

// v0260_qos2_test.go — sim broker QoS 2 upstream handshake (v0.26.0).
//
// Bare-socket coverage:
//   - PUBLISH(QoS2) → PUBREC parked, no delivery
//   - PUBREL → PUBCOMP + exactly-once delivery into Received()
//   - QoS1 path regression: PUBACK + immediate delivery, untouched

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
)

// v0260Dial performs a minimal CONNECT/CONNACK exchange and returns the conn.
func v0260Dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := mqtt.EncodePacket(conn, &mqtt.Connect{ClientID: "v0260-sim"}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	p, err := mqtt.DecodePacket(conn)
	if err != nil {
		t.Fatalf("read connack: %v", err)
	}
	if _, ok := p.(*mqtt.Connack); !ok {
		t.Fatalf("expected CONNACK, got %T", p)
	}
	return conn
}

func v0260Expect(t *testing.T, conn net.Conn, want mqtt.Packet) {
	t.Helper()
	p, err := mqtt.DecodePacket(conn)
	if err != nil {
		t.Fatalf("read %T: %v", want, err)
	}
	if got, ok := p.(*mqtt.Pubrec); ok {
		w, isRec := want.(*mqtt.Pubrec)
		if !isRec || got.PacketID != w.PacketID {
			t.Fatalf("expected %T{%d}, got %+v", want, w.PacketID, p)
		}
		return
	}
	switch w := want.(type) {
	case *mqtt.Puback:
		g, ok := p.(*mqtt.Puback)
		if !ok || g.PacketID != w.PacketID {
			t.Fatalf("expected PUBACK{%d}, got %#v", w.PacketID, p)
		}
	case *mqtt.Pubcomp:
		g, ok := p.(*mqtt.Pubcomp)
		if !ok || g.PacketID != w.PacketID {
			t.Fatalf("expected PUBCOMP{%d}, got %#v", w.PacketID, p)
		}
	case *mqtt.Suback:
		if _, ok := p.(*mqtt.Suback); !ok {
			t.Fatalf("expected SUBACK, got %#v", p)
		}
	default:
		t.Fatalf("unsupported expectation %T", want)
	}
}

func TestV0260SimQoS2Handshake(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	defer b.Close()

	conn := v0260Dial(t, b.Addr())

	// Subscribe first so the fanout after release has a receiver.
	if err := mqtt.EncodePacket(conn, &mqtt.Subscribe{PacketID: 5, Topics: []mqtt.TopicFilter{{Topic: "test/q2", QoS: 0}}}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	v0260Expect(t, conn, &mqtt.Suback{})

	// Upstream QoS2: PUBREC must come back, message stays parked.
	if err := mqtt.EncodePacket(conn, &mqtt.Publish{QoS: 2, PacketID: 9, Topic: "test/q2", Payload: []byte("once")}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	v0260Expect(t, conn, &mqtt.Pubrec{PacketID: 9})
	time.Sleep(50 * time.Millisecond)
	if got := len(b.Received()); got != 0 {
		t.Fatalf("parked QoS2 leaked %d deliveries before PUBREL", got)
	}

	// Release leg: PUBREL → PUBCOMP + exactly-once delivery. The broker
	// fans out to the subscribed test connection first, so read that echo
	// before the PUBCOMP.
	if err := mqtt.EncodePacket(conn, &mqtt.Pubrel{PacketID: 9}); err != nil {
		t.Fatalf("pubrel: %v", err)
	}
	echo, err := mqtt.DecodePacket(conn)
	if err != nil {
		t.Fatalf("read fanout echo: %v", err)
	}
	ep, ok := echo.(*mqtt.Publish)
	if !ok || ep.Topic != "test/q2" || string(ep.Payload) != "once" {
		t.Fatalf("fanout echo mismatch: %#v", echo)
	}
	v0260Expect(t, conn, &mqtt.Pubcomp{PacketID: 9})
	time.Sleep(50 * time.Millisecond)
	got := b.Received()
	if len(got) != 1 || got[0].Topic != "test/q2" || string(got[0].Payload) != "once" || got[0].QoS != 2 {
		b, _ := json.Marshal(got)
		t.Fatalf("post-release delivery mismatch: %s", b)
	}
}

func TestV0260SimQoS1Regression(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	defer b.Close()

	conn := v0260Dial(t, b.Addr())
	if err := mqtt.EncodePacket(conn, &mqtt.Subscribe{PacketID: 6, Topics: []mqtt.TopicFilter{{Topic: "test/q1", QoS: 0}}}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	v0260Expect(t, conn, &mqtt.Suback{})

	if err := mqtt.EncodePacket(conn, &mqtt.Publish{QoS: 1, PacketID: 10, Topic: "test/q1", Payload: []byte("reliable")}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	v0260Expect(t, conn, &mqtt.Puback{PacketID: 10})
	time.Sleep(50 * time.Millisecond)
	got := b.Received()
	if len(got) != 1 || got[0].Topic != "test/q1" || string(got[0].Payload) != "reliable" {
		t.Fatalf("QoS1 regression: got %+v", got)
	}
}

// TestV0260SimQoS2PerConnIsolation is the P1 regression from the v0.26.0
// review: two clients using the SAME packet id concurrently must not
// interfere — parked exchanges are keyed per connection, and each PUBREL
// releases only its own connection's message.
func TestV0260SimQoS2PerConnIsolation(t *testing.T) {
	b, err := NewBroker()
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	defer b.Close()

	connA := v0260Dial(t, b.Addr())
	connB := v0260Dial(t, b.Addr())

	// Both clients park a QoS2 PUBLISH with the same packet id 7.
	if err := mqtt.EncodePacket(connA, &mqtt.Publish{QoS: 2, PacketID: 7, Topic: "test/iso", Payload: []byte("from-A")}); err != nil {
		t.Fatalf("publish A: %v", err)
	}
	v0260Expect(t, connA, &mqtt.Pubrec{PacketID: 7})
	if err := mqtt.EncodePacket(connB, &mqtt.Publish{QoS: 2, PacketID: 7, Topic: "test/iso", Payload: []byte("from-B")}); err != nil {
		t.Fatalf("publish B: %v", err)
	}
	v0260Expect(t, connB, &mqtt.Pubrec{PacketID: 7})
	time.Sleep(50 * time.Millisecond)
	if got := len(b.Received()); got != 0 {
		t.Fatalf("parked QoS2 leaked %d deliveries before any PUBREL", got)
	}

	// A releases: only A's message may be delivered. B's parked exchange
	// (same packet id) must stay untouched.
	if err := mqtt.EncodePacket(connA, &mqtt.Pubrel{PacketID: 7}); err != nil {
		t.Fatalf("pubrel A: %v", err)
	}
	v0260Expect(t, connA, &mqtt.Pubcomp{PacketID: 7})
	time.Sleep(50 * time.Millisecond)
	got := b.Received()
	if len(got) != 1 || got[0].Topic != "test/iso" || string(got[0].Payload) != "from-A" {
		b, _ := json.Marshal(got)
		t.Fatalf("A release must deliver only from-A: %s", b)
	}

	// B releases: now B's message arrives, still exactly once.
	if err := mqtt.EncodePacket(connB, &mqtt.Pubrel{PacketID: 7}); err != nil {
		t.Fatalf("pubrel B: %v", err)
	}
	v0260Expect(t, connB, &mqtt.Pubcomp{PacketID: 7})
	time.Sleep(50 * time.Millisecond)
	got = b.Received()
	if len(got) != 2 || string(got[1].Payload) != "from-B" {
		b, _ := json.Marshal(got)
		t.Fatalf("B release must deliver from-B exactly once: %s", b)
	}
}
