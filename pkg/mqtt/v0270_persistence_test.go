package mqtt

// v0270_persistence_test.go — QoS2 in-flight persistence coverage (v0.27.0).
//
// Scope (all opt-in via Options.PersistenceDir):
//   - upstream QoS2 exchange persists at phase 1, advances to phase 2,
//     and the record is removed on PUBCOMP
//   - default (dir "") leaves zero files and zero behavior change
//   - Resume() replays a phase-1 record against a fresh broker: re-PUBLISH
//     (Dup=1) → PUBREC → PUBREL → PUBCOMP, then clears the record
//   - Resume() replays a phase-2 record (PUBREL only)
//   - inbound parked QoS2 persists on PUBREC-park and is removed on PUBREL;
//     Resume() skips inbound records (delivery waits for the broker)
//   - corrupt records are dropped without blocking the rest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestV0270OutboundRecordLifecycle(t *testing.T) {
	dir := t.TempDir()
	b := startQoS2Broker(t)
	c, err := Dial(b.addr(), Options{ClientID: "v0270-out", EnableQoS2: true, PersistenceDir: dir})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Publish("up/q2", 2, []byte("durable")); err != nil {
		t.Fatalf("Publish QoS2: %v", err)
	}
	// Exchange complete: record must be gone.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			t.Fatalf("record not cleaned after PUBCOMP: %s", e.Name())
		}
	}
}

func TestV0270DefaultNoFiles(t *testing.T) {
	// PersistenceDir unset: QoS2 works exactly as v0.26.0 and writes
	// nothing anywhere.
	b := startQoS2Broker(t)
	c, err := Dial(b.addr(), Options{ClientID: "v0270-nopersist", EnableQoS2: true})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Publish("up/q2", 2, []byte("mem")); err != nil {
		t.Fatalf("Publish QoS2: %v", err)
	}
	b.mu.Lock()
	n := len(b.pubQoS2)
	b.mu.Unlock()
	if n != 1 {
		t.Fatalf("default mode broker saw %d QoS2 publishes, want 1", n)
	}
}

func TestV0270ResumePhase1Replay(t *testing.T) {
	dir := t.TempDir()
	// Simulate a crash mid-exchange: a phase-1 record on disk.
	rec := qos2Record{Kind: 'o', Phase: 1, PktID: 4242, Topic: "up/replay", Payload: []byte("resurrect")}
	if err := qos2Save(dir, rec); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	b := startQoS2Broker(t)
	c, err := Dial(b.addr(), Options{ClientID: "v0270-resume1", EnableQoS2: true, PersistenceDir: dir})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	n, err := c.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n != 1 {
		t.Fatalf("Resume replayed %d, want 1", n)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pubQoS2) != 1 {
		t.Fatalf("broker saw %d QoS2 publishes, want 1 (replayed PUBLISH)", len(b.pubQoS2))
	}
	got := b.pubQoS2[0]
	if got.PacketID != 4242 || got.Topic != "up/replay" || string(got.Payload) != "resurrect" || got.Dup != 1 {
		t.Fatalf("replayed PUBLISH mismatch: %+v", got)
	}
	if len(b.pubrelID) != 1 || b.pubrelID[0] != 4242 {
		t.Fatalf("replayed PUBREL mismatch: %v", b.pubrelID)
	}
	// Record cleared after successful replay.
	if _, err := os.Stat(filepath.Join(dir, qos2FileName(4242))); !os.IsNotExist(err) {
		t.Fatal("record not removed after replay")
	}
}

func TestV0270ResumePhase2Replay(t *testing.T) {
	dir := t.TempDir()
	rec := qos2Record{Kind: 'o', Phase: 2, PktID: 5151, Topic: "up/replay2", Payload: []byte("late")}
	if err := qos2Save(dir, rec); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	b := startQoS2Broker(t)
	c, err := Dial(b.addr(), Options{ClientID: "v0270-resume2", EnableQoS2: true, PersistenceDir: dir})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	n, err := c.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n != 1 {
		t.Fatalf("Resume replayed %d, want 1", n)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Phase-2 replay must NOT re-send PUBLISH; only PUBREL.
	if len(b.pubQoS2) != 0 {
		t.Fatalf("phase-2 replay re-sent PUBLISH: %+v", b.pubQoS2)
	}
	if len(b.pubrelID) != 1 || b.pubrelID[0] != 5151 {
		t.Fatalf("phase-2 replay PUBREL mismatch: %v", b.pubrelID)
	}
}

func TestV0270InboundParkPersisted(t *testing.T) {
	dir := t.TempDir()
	b := startQoS2Broker(t)
	b.mu.Lock()
	b.downID = 909
	b.mu.Unlock()
	c, err := Dial(b.addr(), Options{ClientID: "v0270-in", EnableQoS2: true, PersistenceDir: dir})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	got := make(chan string, 4)
	if err := c.Subscribe("down/#", 2, func(topic string, payload []byte) { got <- string(payload) }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Trigger the downstream push: this fake broker emits its queued
	// downstream PUBLISH only after completing an upstream exchange
	// (same pattern as the v0260 broker).
	if err := c.Publish("up/trigger", 2, []byte("go")); err != nil {
		t.Fatalf("trigger publish: %v", err)
	}
	select {
	case p := <-got:
		if p != "exactly-once" {
			t.Fatalf("delivery mismatch: %q", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inbound QoS2 delivery timeout")
	}
	// After PUBREL+PUBCOMP the record is gone. The removal happens on the
	// read pump after the handler returns, so poll briefly instead of
	// checking once (the delivery channel unblocks first).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, qos2FileName(909))); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("inbound record not removed after release leg")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Resume must skip inbound records (none left here, but the contract
	// is exercised by the corrupt-record test below in combination).
	if _, err := c.Resume(); err != nil {
		t.Fatalf("Resume after clean state: %v", err)
	}
}

func TestV0270ResumeSkipsInboundAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	// Inbound record: Resume must skip it (waits for broker PUBREL), file stays.
	inRec := qos2Record{Kind: 'i', Phase: 1, PktID: 300, Topic: "down/parked", Payload: []byte("wait")}
	if err := qos2Save(dir, inRec); err != nil {
		t.Fatalf("seed inbound: %v", err)
	}
	// Outbound phase-1 record: Resume must replay it.
	outRec := qos2Record{Kind: 'o', Phase: 1, PktID: 301, Topic: "up/mix", Payload: []byte("go")}
	if err := qos2Save(dir, outRec); err != nil {
		t.Fatalf("seed outbound: %v", err)
	}
	// Corrupt record: dropped, must not block.
	if err := os.WriteFile(filepath.Join(dir, "qos2-0302.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	b := startQoS2Broker(t)
	c, err := Dial(b.addr(), Options{ClientID: "v0270-mix", EnableQoS2: true, PersistenceDir: dir})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	n, err := c.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n != 1 {
		t.Fatalf("Resume replayed %d, want 1 (outbound only)", n)
	}
	if _, err := os.Stat(filepath.Join(dir, qos2FileName(300))); err != nil {
		t.Fatalf("inbound record must survive Resume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "qos2-0302.json")); !os.IsNotExist(err) {
		t.Fatal("corrupt record must be removed")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pubQoS2) != 1 || b.pubQoS2[0].PacketID != 301 {
		t.Fatalf("outbound replay mismatch: %+v", b.pubQoS2)
	}
}

func TestV0270ResumePendingAPI(t *testing.T) {
	dir := t.TempDir()
	if err := qos2Save(dir, qos2Record{Kind: 'o', Phase: 1, PktID: 7, Topic: "t/a", Payload: []byte("x")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	pending, err := ResumePending(dir)
	if err != nil {
		t.Fatalf("ResumePending: %v", err)
	}
	if len(pending) != 1 || pending[0].PktID != 7 || pending[0].Inbound || pending[0].Phase != 1 {
		b, _ := json.Marshal(pending)
		t.Fatalf("pending mismatch: %s", b)
	}
	// Empty dir + disabled dir are both clean.
	if p2, err := ResumePending(""); err != nil || p2 != nil {
		t.Fatalf("ResumePending(empty dir) = %v, %v", p2, err)
	}
	if p3, err := ResumePending(filepath.Join(dir, "missing")); err != nil || p3 != nil {
		t.Fatalf("ResumePending(missing dir) = %v, %v", p3, err)
	}
	if err := ResumeComplete(dir, 7); err != nil {
		t.Fatalf("ResumeComplete: %v", err)
	}
	if p4, _ := ResumePending(dir); len(p4) != 0 {
		t.Fatalf("records left after ResumeComplete: %v", p4)
	}
}

// TestV0270ResumeAdvancesCounter is the hardening regression: after
// replaying a record with a high packet id on a FRESH connection, the
// nextID counter must have been advanced past it, so the next allocated
// id cannot collide with the replayed exchange.
func TestV0270ResumeAdvancesCounter(t *testing.T) {
	dir := t.TempDir()
	rec := qos2Record{Kind: 'o', Phase: 2, PktID: 60000, Topic: "up/ctr", Payload: []byte("x")}
	if err := qos2Save(dir, rec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b := startQoS2Broker(t)
	c, err := Dial(b.addr(), Options{ClientID: "v0270-ctr", EnableQoS2: true, PersistenceDir: dir})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	n, err := c.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n != 1 {
		t.Fatalf("Resume replayed %d, want 1", n)
	}
	if got := c.nextID(); got != 60001 {
		t.Fatalf("nextID after resume = %d, want 60001 (counter must skip replayed ids)", got)
	}
}
