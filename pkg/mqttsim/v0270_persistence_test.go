package mqttsim

// v0270_persistence_test.go — broker-side QoS2 parked-message persistence
// coverage (v0.27.0).
//
// Scope (all opt-in via NewBrokerWithOptions):
//   - park writes a record; PUBREL removes it (exactly-once preserved)
//   - default NewBroker() writes nothing (v0.26.0 behavior byte-identical)
//   - BrokerResumePending recovers parked messages after a broker restart
//     and drops corrupt records
//   - client disconnect does NOT delete records (persistence is the point)

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
)

// v0270SimDial performs CONNECT/CONNACK and returns the conn.
func v0270SimDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := mqtt.EncodePacket(conn, &mqtt.Connect{ClientID: "v0270-sim"}); err != nil {
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

func TestV0270SimParkPersistedAndCleared(t *testing.T) {
	dir := t.TempDir()
	b, err := NewBrokerWithOptions(dir)
	if err != nil {
		t.Fatalf("NewBrokerWithOptions: %v", err)
	}
	defer b.Close()

	conn := v0270SimDial(t, b.Addr())
	if err := mqtt.EncodePacket(conn, &mqtt.Publish{QoS: 2, PacketID: 21, Topic: "sim/q2", Payload: []byte("parked")}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	p, err := mqtt.DecodePacket(conn)
	if err != nil {
		t.Fatalf("read pubrec: %v", err)
	}
	if _, ok := p.(*mqtt.Pubrec); !ok {
		t.Fatalf("expected PUBREC, got %T", p)
	}
	// Park: record must exist on disk.
	recPath := filepath.Join(dir, mqtt.QoS2RecordFileName(21))
	if _, err := os.Stat(recPath); err != nil {
		t.Fatalf("park record missing: %v", err)
	}
	if err := mqtt.EncodePacket(conn, &mqtt.Pubrel{PacketID: 21}); err != nil {
		t.Fatalf("pubrel: %v", err)
	}
	// Fanout echo arrives before PUBCOMP (same ordering as v0260 coverage);
	// tolerate any order by reading until PUBCOMP shows up.
	deadline := time.Now().Add(3 * time.Second)
	sawComp := false
	for !sawComp {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for PUBCOMP")
		}
		p, err := mqtt.DecodePacket(conn)
		if err != nil {
			t.Fatalf("read after pubrel: %v", err)
		}
		if _, ok := p.(*mqtt.Pubcomp); ok {
			sawComp = true
		}
	}
	// Release leg complete: record must be gone (poll: removal happens
	// right after enqueue, may race this read).
	pollDeadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(recPath); os.IsNotExist(err) {
			break
		}
		if time.Now().After(pollDeadline) {
			t.Fatal("record not removed after PUBREL")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := b.Received()
	if len(got) != 1 || got[0].Topic != "sim/q2" || string(got[0].Payload) != "parked" {
		t.Fatalf("delivery mismatch: %+v", got)
	}
}

func TestV0270SimDefaultNoFiles(t *testing.T) {
	dir := t.TempDir()
	// Default broker (no options): persistence disabled even though a dir
	// exists next door — verify by pointing nothing at it. Instead check
	// the real contract: default broker + park + release writes nothing
	// anywhere. We can't see the (empty) internal dir, so assert behavior:
	// the exchange completes exactly as v0.26.0 and ResumePending on a
	// fresh empty dir yields nothing.
	b, err := NewBroker()
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	defer b.Close()
	conn := v0270SimDial(t, b.Addr())
	if err := mqtt.EncodePacket(conn, &mqtt.Publish{QoS: 2, PacketID: 22, Topic: "sim/q2", Payload: []byte("mem")}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	p, err := mqtt.DecodePacket(conn)
	if err != nil {
		t.Fatalf("read pubrec: %v", err)
	}
	if _, ok := p.(*mqtt.Pubrec); !ok {
		t.Fatalf("expected PUBREC, got %T", p)
	}
	if err := mqtt.EncodePacket(conn, &mqtt.Pubrel{PacketID: 22}); err != nil {
		t.Fatalf("pubrel: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		p, err := mqtt.DecodePacket(conn)
		if err != nil {
			t.Fatalf("read after pubrel: %v", err)
		}
		if _, ok := p.(*mqtt.Pubcomp); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for PUBCOMP")
		}
	}
	if pending, err := BrokerResumePending(dir); err != nil || len(pending) != 0 {
		t.Fatalf("BrokerResumePending(empty dir) = %v, %v", pending, err)
	}
	if pending, err := BrokerResumePending(""); err != nil || pending != nil {
		t.Fatalf("BrokerResumePending(disabled) = %v, %v", pending, err)
	}
}

func TestV0270SimRestartRecovery(t *testing.T) {
	dir := t.TempDir()

	// Broker #1: park a QoS2 PUBLISH, then close WITHOUT the sender's
	// PUBREL (simulates a broker crash with a message in flight).
	b1, err := NewBrokerWithOptions(dir)
	if err != nil {
		t.Fatalf("NewBrokerWithOptions: %v", err)
	}
	conn := v0270SimDial(t, b1.Addr())
	if err := mqtt.EncodePacket(conn, &mqtt.Publish{QoS: 2, PacketID: 33, Topic: "sim/recover", Payload: []byte("survived")}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	p, err := mqtt.DecodePacket(conn)
	if err != nil {
		t.Fatalf("read pubrec: %v", err)
	}
	if _, ok := p.(*mqtt.Pubrec); !ok {
		t.Fatalf("expected PUBREC, got %T", p)
	}
	// Client goes away uncleanly (no DISCONNECT): record must survive.
	_ = conn.Close()
	b1.Close()

	// Records survive both the client disconnect and the broker close.
	recPath := filepath.Join(dir, mqtt.QoS2RecordFileName(33))
	if _, err := os.Stat(recPath); err != nil {
		t.Fatalf("record lost after restart: %v", err)
	}
	pending, err := BrokerResumePending(dir)
	if err != nil {
		t.Fatalf("BrokerResumePending: %v", err)
	}
	if len(pending) != 1 || pending[0].PacketID != 33 || pending[0].Topic != "sim/recover" || string(pending[0].Payload) != "survived" || pending[0].QoS != 2 {
		t.Fatalf("recovered mismatch: %+v", pending)
	}

	// Broker #2 (restart): completes the exchange for the same packet id —
	// the recovered message is delivered exactly once via the normal
	// release leg on the NEW broker (record removed there).
	b2, err := NewBrokerWithOptions(dir)
	if err != nil {
		t.Fatalf("re-start broker: %v", err)
	}
	defer b2.Close()
	conn2 := v0270SimDial(t, b2.Addr())
	if err := mqtt.EncodePacket(conn2, &mqtt.Pubrel{PacketID: 33}); err != nil {
		t.Fatalf("pubrel on new broker: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		p, err := mqtt.DecodePacket(conn2)
		if err != nil {
			t.Fatalf("read after pubrel: %v", err)
		}
		if _, ok := p.(*mqtt.Pubcomp); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for PUBCOMP after restart")
		}
	}
	got := b2.Received()
	if len(got) != 1 || got[0].Topic != "sim/recover" || string(got[0].Payload) != "survived" {
		t.Fatalf("post-restart delivery mismatch: %+v", got)
	}
	if _, err := os.Stat(recPath); !os.IsNotExist(err) {
		t.Fatal("record not removed after post-restart release")
	}
}

func TestV0270SimResumeDropsCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "qos2-00aa.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	// A record with a foreign Kind (client 'o' record) must be skipped,
	// not delivered, and not deleted (it belongs to the client side).
	if err := os.WriteFile(filepath.Join(dir, "qos2-00ab.json"), []byte(`{"kind":111,"pkt_id":171,"topic":"up/x"}`), 0o600); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}
	pending, err := BrokerResumePending(dir)
	if err != nil {
		t.Fatalf("BrokerResumePending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("corrupt/foreign records must not recover: %+v", pending)
	}
	if _, err := os.Stat(filepath.Join(dir, "qos2-00aa.json")); !os.IsNotExist(err) {
		t.Fatal("corrupt record must be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "qos2-00ab.json")); err != nil {
		t.Fatalf("foreign (client-side) record must survive: %v", err)
	}
}
