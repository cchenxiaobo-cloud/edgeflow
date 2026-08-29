package mqttsim

// Tests for the simulated MQTT broker. Clients are built directly on the
// pkg/mqtt codec primitives (net.Dial + encodePacket/decodePacket); the M2a
// client.go is intentionally NOT imported (parallel development).

import (
	"net"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
)

const (
	dialWait   = 2 * time.Second
	deadline   = 3 * time.Second
	testTopicA = "a/+/b"
)

func newTestBroker(t *testing.T) *Broker {
	t.Helper()
	b, err := NewBroker()
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func dialRaw(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, dialWait)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	if err := conn.SetDeadline(time.Now().Add(deadline)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	return conn
}

// connectClient dials, performs the CONNECT/CONNACK handshake and returns
// the live connection on which the deadline is already set.
func connectClient(t *testing.T, addr, id string) net.Conn {
	t.Helper()
	conn := dialRaw(t, addr)
	if err := encodePacket(conn, &mqtt.Connect{ClientID: id, KeepAlive: 60, CleanSession: true}); err != nil {
		t.Fatalf("encode CONNECT: %v", err)
	}
	pkt, err := decodePacket(conn)
	if err != nil {
		t.Fatalf("decode CONNACK: %v", err)
	}
	ca, ok := pkt.(*mqtt.Connack)
	if !ok {
		t.Fatalf("want *mqtt.Connack, got %T", pkt)
	}
	if ca.ReturnCode != 0 {
		t.Fatalf("CONNACK return code = %d, want 0", ca.ReturnCode)
	}
	return conn
}

// subscribe sends SUBSCRIBE and asserts the SUBACK comes back, returning codes.
func subscribe(t *testing.T, conn net.Conn, packetID uint16, filters ...mqtt.TopicFilter) []byte {
	t.Helper()
	if err := encodePacket(conn, &mqtt.Subscribe{PacketID: packetID, Topics: filters}); err != nil {
		t.Fatalf("encode SUBSCRIBE: %v", err)
	}
	pkt, err := decodePacket(conn)
	if err != nil {
		t.Fatalf("decode SUBACK: %v", err)
	}
	sa, ok := pkt.(*mqtt.Suback)
	if !ok {
		t.Fatalf("want *mqtt.Suback, got %T", pkt)
	}
	return sa.Codes
}

// pingBarrier flushes the wire up to and including PINGRESP. Any
// server->client packet ahead of the PINGRESP is a failure (FIFO order),
// which makes "no delivery happened" assertions airtight.
func pingBarrier(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := encodePacket(conn, &mqtt.Pingreq{}); err != nil {
		t.Fatalf("encode PINGREQ: %v", err)
	}
	pkt, err := decodePacket(conn)
	if err != nil {
		t.Fatalf("decode after PINGREQ: %v", err)
	}
	if _, ok := pkt.(*mqtt.Pingresp); !ok {
		t.Fatalf("want PINGRESP immediately after PINGREQ, got %T (unexpected delivery?)", pkt)
	}
}

// expectPublish reads packets until a PUBLISH arrives.
func expectPublish(t *testing.T, conn net.Conn) *mqtt.Publish {
	t.Helper()
	for {
		pkt, err := decodePacket(conn)
		if err != nil {
			t.Fatalf("decode PUBLISH: %v", err)
		}
		if p, ok := pkt.(*mqtt.Publish); ok {
			return p
		}
	}
}

// expectClosed asserts the peer closes the connection (EOF / reset / timeout
// with no data) rather than answering.
func expectClosed(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	buf := make([]byte, 32)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return // EOF, reset or deadline: connection was torn down
		}
		if n > 0 {
			t.Fatalf("%s: server sent %d unexpected bytes instead of closing", what, n)
		}
	}
}

func TestConnectConnack(t *testing.T) {
	b := newTestBroker(t)
	conn := connectClient(t, b.Addr(), "c1")
	defer conn.Close()
	if got := b.PingCount(); got != 0 {
		t.Fatalf("PingCount = %d, want 0 on fresh broker", got)
	}
}

func TestSubscribeSubackEchoAndInvalidFilter(t *testing.T) {
	b := newTestBroker(t)
	conn := connectClient(t, b.Addr(), "c1")
	defer conn.Close()
	codes := subscribe(t, conn, 1,
		mqtt.TopicFilter{Topic: testTopicA, QoS: 0},
		mqtt.TopicFilter{Topic: "a/#/b", QoS: 0}, // '#' not last level -> 0x80
	)
	if len(codes) != 2 {
		t.Fatalf("SUBACK codes = %v, want 2 entries", codes)
	}
	if codes[0] != 0 {
		t.Fatalf("SUBACK code[0] = %#x, want 0x00 (echo of granted QoS)", codes[0])
	}
	if codes[1] != 0x80 {
		t.Fatalf("SUBACK code[1] = %#x, want 0x80 for invalid filter", codes[1])
	}
}

func TestBrokerPublishMatchAndNonMatch(t *testing.T) {
	b := newTestBroker(t)
	conn := connectClient(t, b.Addr(), "sub")
	defer conn.Close()

	// Publish with no subscribers must not error.
	if err := b.Publish("nobody/listens", []byte("void")); err != nil {
		t.Fatalf("Publish without subscribers: %v", err)
	}

	codes := subscribe(t, conn, 1, mqtt.TopicFilter{Topic: testTopicA, QoS: 0})
	if codes[0] != 0 {
		t.Fatalf("SUBACK code = %#x, want 0x00", codes[0])
	}

	if err := b.Publish("a/x/b", []byte("hello")); err != nil {
		t.Fatalf("Publish a/x/b: %v", err)
	}
	p := expectPublish(t, conn)
	if p.Topic != "a/x/b" {
		t.Fatalf("delivered topic = %q, want a/x/b", p.Topic)
	}
	if string(p.Payload) != "hello" {
		t.Fatalf("delivered payload = %q, want hello", p.Payload)
	}
	if p.QoS != 0 {
		t.Fatalf("delivered QoS = %d, want 0 (server push is QoS 0)", p.QoS)
	}

	// Non-matching topic must NOT be delivered: the very next packet after
	// PINGREQ has to be PINGRESP (wire FIFO guarantees no hidden publish).
	if err := b.Publish("a/b", []byte("nope")); err != nil {
		t.Fatalf("Publish a/b: %v", err)
	}
	pingBarrier(t, conn)
}

func TestClientPublishQoS1PubackAndReceived(t *testing.T) {
	b := newTestBroker(t)
	conn := connectClient(t, b.Addr(), "pub")
	defer conn.Close()

	codes := subscribe(t, conn, 1, mqtt.TopicFilter{Topic: "t/1", QoS: 0})
	if codes[0] != 0 {
		t.Fatalf("SUBACK code = %#x, want 0x00", codes[0])
	}

	// Client-originated QoS 1 publish; the sender is itself subscribed.
	if err := encodePacket(conn, &mqtt.Publish{
		QoS:      1,
		PacketID: 42,
		Topic:    "t/1",
		Payload:  []byte("q1"),
	}); err != nil {
		t.Fatalf("encode PUBLISH: %v", err)
	}

	// First PUBACK for the QoS 1 message, then the fan-out copy (QoS 0,
	// sender included as a subscriber).
	pkt, err := decodePacket(conn)
	if err != nil {
		t.Fatalf("decode PUBACK: %v", err)
	}
	pa, ok := pkt.(*mqtt.Puback)
	if !ok {
		t.Fatalf("want *mqtt.Puback, got %T", pkt)
	}
	if pa.PacketID != 42 {
		t.Fatalf("PUBACK packet id = %d, want 42", pa.PacketID)
	}
	p := expectPublish(t, conn)
	if p.Topic != "t/1" || string(p.Payload) != "q1" {
		t.Fatalf("fan-out got %q/%q, want t/1/q1", p.Topic, p.Payload)
	}
	if p.QoS != 0 || p.PacketID != 0 {
		t.Fatalf("fan-out QoS/PacketID = %d/%d, want 0/0", p.QoS, p.PacketID)
	}

	got := b.Received()
	if len(got) != 1 {
		t.Fatalf("Received() len = %d, want 1", len(got))
	}
	if got[0].Topic != "t/1" || got[0].QoS != 1 || got[0].PacketID != 42 || string(got[0].Payload) != "q1" {
		t.Fatalf("Received()[0] = %+v, want t/1 QoS1 id42 q1", got[0])
	}

	// Deep-copy check: mutating the returned slice must not leak into the
	// broker's stored record.
	got[0].Payload[0] = 'X'
	again := b.Received()
	if string(again[0].Payload) != "q1" {
		t.Fatalf("Received() not deep-copied: payload mutated to %q", again[0].Payload)
	}
}

func TestPingreqPingrespCount(t *testing.T) {
	b := newTestBroker(t)
	conn := connectClient(t, b.Addr(), "c1")
	defer conn.Close()
	before := b.PingCount()
	if err := encodePacket(conn, &mqtt.Pingreq{}); err != nil {
		t.Fatalf("encode PINGREQ: %v", err)
	}
	pkt, err := decodePacket(conn)
	if err != nil {
		t.Fatalf("decode PINGRESP: %v", err)
	}
	if _, ok := pkt.(*mqtt.Pingresp); !ok {
		t.Fatalf("want *mqtt.Pingresp, got %T", pkt)
	}
	if got := b.PingCount(); got != before+1 {
		t.Fatalf("PingCount = %d, want %d", got, before+1)
	}
}

func TestEmptyClientIDClosed(t *testing.T) {
	b := newTestBroker(t)
	conn := dialRaw(t, b.Addr())
	defer conn.Close()
	// Hand-rolled CONNECT with empty ClientID: the codec's validateConnect
	// refuses to encode one, but a wire peer can still send it.
	body := []byte{0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04, 0x02, 0x00, 0x3C, 0x00, 0x00}
	raw := append([]byte{0x10, byte(len(body))}, body...)
	if _, err := conn.Write(raw); err != nil {
		t.Fatalf("write raw CONNECT: %v", err)
	}
	expectClosed(t, conn, "empty ClientID CONNECT")
}

func TestMalformedConnectClosed(t *testing.T) {
	b := newTestBroker(t)

	// Case 1: truncated CONNECT — fixed header claims 10 remaining bytes,
	// we send 4 and half-close, so the server's read hits EOF and the decode
	// fails with a short body; the server must tear the connection down.
	conn := dialRaw(t, b.Addr())
	if _, err := conn.Write([]byte{0x10, 0x0A, 0x00, 0x04}); err != nil {
		t.Fatalf("write truncated CONNECT: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	expectClosed(t, conn, "truncated CONNECT")
	conn.Close()

	// Case 2: complete packet with the reserved packet type 15 — decode
	// fails immediately and the server must close without a reply.
	conn2 := dialRaw(t, b.Addr())
	defer conn2.Close()
	if _, err := conn2.Write([]byte{0xF0, 0x00}); err != nil {
		t.Fatalf("write reserved-type packet: %v", err)
	}
	expectClosed(t, conn2, "reserved-type packet")
}

func TestCloseIdempotentAndListenerDown(t *testing.T) {
	b := newTestBroker(t)
	addr := b.Addr()
	conn := connectClient(t, addr, "c1") // established client gets reaped too

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil (idempotent)", err)
	}

	// Listener must be down: new dials are refused.
	if c, err := net.DialTimeout("tcp", addr, dialWait); err == nil {
		c.Close()
		t.Fatal("dial to closed broker unexpectedly succeeded")
	}

	// The previously established client connection must have been closed.
	if err := conn.SetReadDeadline(time.Now().Add(dialWait)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := conn.Read(make([]byte, 8)); err == nil {
		t.Fatal("client connection still open after broker Close")
	}
}

func TestSimMatchTopic(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"a/+/b", "a/x/b", true},
		{"a/+/b", "a/b", false},     // too few levels
		{"a/+/b", "a/x/y/b", false}, // too many levels
		{"a/#", "a", true},          // '#' matches the parent level too
		{"a/#", "a/b/c", true},
		{"#", "x/y/z", true},
		{"a/b", "a/b", true},
		{"a/b", "a/c", false},
		{"+/+", "a/b", true},
		{"a/b", "a", false},
	}
	for _, tc := range cases {
		if got := simMatchTopic(tc.filter, tc.topic); got != tc.want {
			t.Errorf("simMatchTopic(%q, %q) = %v, want %v", tc.filter, tc.topic, got, tc.want)
		}
	}
}
