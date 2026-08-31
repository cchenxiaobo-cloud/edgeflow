package mqtt

// v0260_qos2_test.go — MQTT QoS 2 (EXACTLY ONCE) coverage (v0.26.0).
//
// The frozen v0240 fake broker intentionally speaks only QoS 0/1, so these
// tests carry their own QoS2-capable broker. Scope:
//   - upstream Publish(QoS2) full PUBLISH→PUBREC→PUBREL→PUBCOMP handshake
//   - gate: QoS2 rejected before the wire unless Options.EnableQoS2
//   - inbound QoS2 delivered exactly once after PUBREL (PUBREC first)
//   - codec roundtrip of the three new packets + flag/body validation
//   - PUBREC timeout surfaces as an error (shortened ackTimeout)

import (
	"bufio"
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

// qos2Broker answers the full QoS2 handshake and records what it saw.
type qos2Broker struct {
	ln net.Listener

	mu       sync.Mutex
	pubQoS2  []*Publish // upstream QoS2 PUBLISH received
	pubrelID []uint16   // PUBREL packet ids received (upstream leg 2)
	downID   uint16     // id of the downstream QoS2 PUBLISH to push
	relDone  bool       // downstream PUBREL received
}

func startQoS2Broker(t *testing.T) *qos2Broker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &qos2Broker{ln: ln}
	go b.acceptLoop()
	return b
}

func (b *qos2Broker) addr() string { return b.ln.Addr().String() }

func (b *qos2Broker) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.serve(conn)
	}
}

func (b *qos2Broker) serve(conn net.Conn) {
	defer conn.Close()
	for {
		p, err := decodePacket(conn)
		if err != nil {
			return
		}
		switch pv := p.(type) {
		case *Connect:
			_ = encodePacket(conn, &Connack{})
		case *Subscribe:
			codes := make([]byte, len(pv.Topics))
			for i, tf := range pv.Topics {
				codes[i] = tf.QoS
			}
			_ = encodePacket(conn, &Suback{PacketID: pv.PacketID, Codes: codes})
		case *Publish:
			if pv.QoS == 2 {
				b.mu.Lock()
				b.pubQoS2 = append(b.pubQoS2, pv)
				b.mu.Unlock()
				_ = encodePacket(conn, &Pubrec{PacketID: pv.PacketID})
				continue // wait for PUBREL before treating it as delivered
			}
		case *Pubrel:
			b.mu.Lock()
			b.pubrelID = append(b.pubrelID, pv.PacketID)
			pushID := b.downID
			b.mu.Unlock()
			_ = encodePacket(conn, &Pubcomp{PacketID: pv.PacketID})
			if pushID != 0 {
				// Downstream QoS2 exercise: PUBLISH → (client PUBREC)
				// → PUBREL → (client PUBCOMP).
				_ = encodePacket(conn, &Publish{QoS: 2, PacketID: pushID, Topic: "down/q2", Payload: []byte("exactly-once")})
			}
		case *Pubrec:
			// Downstream PUBREC from the client for our pushed PUBLISH:
			// release it so the client can deliver and answer PUBCOMP.
			_ = encodePacket(conn, &Pubrel{PacketID: pv.PacketID})
		case *Pubcomp:
			b.mu.Lock()
			b.relDone = true
			b.mu.Unlock()
		case *Disconnect:
			return
		}
	}
}

func TestV0260PublishQoS2FullHandshake(t *testing.T) {
	b := startQoS2Broker(t)
	c, err := Dial(b.addr(), Options{ClientID: "q2", EnableQoS2: true})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Publish("up/q2", 2, []byte("once")); err != nil {
		t.Fatalf("Publish QoS2: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pubQoS2) != 1 || b.pubQoS2[0].Topic != "up/q2" || string(b.pubQoS2[0].Payload) != "once" {
		t.Fatalf("QoS2 PUBLISH mismatch: %+v", b.pubQoS2)
	}
	if len(b.pubrelID) != 1 || b.pubrelID[0] != b.pubQoS2[0].PacketID {
		t.Fatalf("PUBREL mismatch: rel=%v pub=%+v", b.pubrelID, b.pubQoS2[0])
	}
	if b.pubQoS2[0].PacketID == 0 {
		t.Fatal("QoS2 PUBLISH must carry a non-zero packet id")
	}
}

func TestV0260QoS2GateDefaultOff(t *testing.T) {
	b := startQoS2Broker(t)
	c, err := Dial(b.addr(), Options{ClientID: "q2gate"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Publish("up/q2", 2, []byte("nope")); err == nil {
		t.Fatal("Publish QoS2 without EnableQoS2: want error, got nil")
	}
	if err := c.Publish("up/q3", 3, []byte("nope")); err == nil {
		t.Fatal("Publish QoS3: want error, got nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pubQoS2) != 0 {
		t.Fatalf("gated QoS2 must not reach the wire, broker saw %+v", b.pubQoS2)
	}
}

func TestV0260InboundQoS2ExactlyOnce(t *testing.T) {
	b := startQoS2Broker(t)
	c, err := Dial(b.addr(), Options{ClientID: "q2down", EnableQoS2: true})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	b.mu.Lock()
	b.downID = 77
	b.mu.Unlock()

	got := make(chan string, 4)
	if err := c.Subscribe("down/#", 2, func(topic string, payload []byte) {
		got <- topic + ":" + string(payload)
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Trigger the downstream exchange by completing an upstream QoS2 pub.
	if err := c.Publish("up/q2", 2, []byte("kick")); err != nil {
		t.Fatalf("Publish QoS2: %v", err)
	}
	select {
	case m := <-got:
		if m != "down/q2:exactly-once" {
			t.Fatalf("downstream payload mismatch: %q", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("downstream QoS2 message not delivered after PUBREL")
	}
	select {
	case m := <-got:
		t.Fatalf("downstream QoS2 delivered more than once: %q", m)
	default:
	}
	// PUBCOMP is written by the read pump after the handler returns; give
	// the final leg a deadline instead of checking it synchronously.
	deadline := time.Now().Add(3 * time.Second)
	for {
		b.mu.Lock()
		done := b.relDone
		b.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client never sent PUBCOMP for the downstream exchange")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestV0260PublishQoS2PubrecTimeout(t *testing.T) {
	old := ackTimeout
	ackTimeout = 150 * time.Millisecond
	defer func() { ackTimeout = old }()

	// Mute broker: CONNACKs but never answers QoS2.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = encodePacket(c, &Connack{})
				r := bufio.NewReader(c)
				for {
					if _, err := decodePacket(r); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	c, err := Dial(ln.Addr().String(), Options{ClientID: "q2slow", EnableQoS2: true})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	start := time.Now()
	if err := c.Publish("up/q2", 2, []byte("stall")); err == nil {
		t.Fatal("Publish QoS2 without PUBREC: want timeout error, got nil")
	}
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Fatalf("Publish returned too fast (%s): timeout leg not exercised", el)
	}
}

func TestV0260QoS2CodecRoundtrip(t *testing.T) {
	pairs := []Packet{
		&Pubrec{PacketID: 0x0102},
		&Pubrel{PacketID: 0x0304},
		&Pubcomp{PacketID: 0x0506},
	}
	for _, p := range pairs {
		var buf bytes.Buffer
		if err := encodePacket(&buf, p); err != nil {
			t.Fatalf("encode %T: %v", p, err)
		}
		got, err := decodePacket(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("decode %T: %v", p, err)
		}
		if got.Type() != p.Type() {
			t.Fatalf("type mismatch: got %d want %d", got.Type(), p.Type())
		}
	}
	// Fixed-header flags: PUBREL must encode 0b0010, the others 0b0000.
	b1, err := fixedByte1(&Pubrel{PacketID: 1})
	if err != nil || b1 != PacketTypePUBREL<<4|0x02 {
		t.Fatalf("PUBREL fixed byte1 = %#02x (%v), want %#02x", b1, err, PacketTypePUBREL<<4|0x02)
	}
	if b1, _ = fixedByte1(&Pubrec{PacketID: 1}); b1 != PacketTypePUBREC<<4 {
		t.Fatalf("PUBREC fixed byte1 = %#02x, want flags 0", b1)
	}
	if b1, _ = fixedByte1(&Pubcomp{PacketID: 1}); b1 != PacketTypePUBCOMP<<4 {
		t.Fatalf("PUBCOMP fixed byte1 = %#02x, want flags 0", b1)
	}
}
