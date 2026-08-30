package mqtt

import (
	"bytes"
	"testing"
)

// Tests for the R-6 exported codec surface (EncodePacket/DecodePacket/
// ValidateTopicFilter): thin wrappers over the unexported implementations,
// exported in v0.25.0 so pkg/mqttsim and external test brokers reuse a
// single wire implementation instead of carrying private copies.

func TestV0250EncodeDecodeRoundtripConnect(t *testing.T) {
	var buf bytes.Buffer
	in := &Connect{ClientID: "dev-1", KeepAlive: 60, CleanSession: true}
	if err := EncodePacket(&buf, in); err != nil {
		t.Fatalf("EncodePacket(CONNECT) = %v", err)
	}
	out, err := DecodePacket(&buf)
	if err != nil {
		t.Fatalf("DecodePacket(CONNECT) = %v", err)
	}
	c, ok := out.(*Connect)
	if !ok {
		t.Fatalf("decoded type = %T, want *Connect", out)
	}
	if c.ClientID != in.ClientID || c.KeepAlive != in.KeepAlive || !c.CleanSession {
		t.Fatalf("CONNECT roundtrip mismatch: got %+v, want %+v", c, in)
	}
}

func TestV0250EncodeDecodeRoundtripPublishSubscribe(t *testing.T) {
	// PUBLISH QoS 1 roundtrip.
	var buf bytes.Buffer
	pub := &Publish{Topic: "a/b", Payload: []byte("hi"), QoS: 1, PacketID: 7}
	if err := EncodePacket(&buf, pub); err != nil {
		t.Fatalf("EncodePacket(PUBLISH) = %v", err)
	}
	out, err := DecodePacket(&buf)
	if err != nil {
		t.Fatalf("DecodePacket(PUBLISH) = %v", err)
	}
	p, ok := out.(*Publish)
	if !ok {
		t.Fatalf("decoded type = %T, want *Publish", out)
	}
	if p.Topic != pub.Topic || string(p.Payload) != string(pub.Payload) ||
		p.QoS != pub.QoS || p.PacketID != pub.PacketID {
		t.Fatalf("PUBLISH roundtrip mismatch: got %+v", p)
	}

	// SUBSCRIBE roundtrip.
	buf.Reset()
	sub := &Subscribe{PacketID: 3, Topics: []TopicFilter{{Topic: "x/+/y", QoS: 0}}}
	if err := EncodePacket(&buf, sub); err != nil {
		t.Fatalf("EncodePacket(SUBSCRIBE) = %v", err)
	}
	out, err = DecodePacket(&buf)
	if err != nil {
		t.Fatalf("DecodePacket(SUBSCRIBE) = %v", err)
	}
	s, ok := out.(*Subscribe)
	if !ok {
		t.Fatalf("decoded type = %T, want *Subscribe", out)
	}
	if s.PacketID != sub.PacketID || len(s.Topics) != 1 ||
		s.Topics[0].Topic != "x/+/y" || s.Topics[0].QoS != 0 {
		t.Fatalf("SUBSCRIBE roundtrip mismatch: got %+v", s)
	}
}

func TestV0250DecodePacketRejectsGarbage(t *testing.T) {
	// Truncated packet body must surface a read/parse error.
	var buf bytes.Buffer
	if err := EncodePacket(&buf, &Publish{Topic: "a/b", Payload: []byte("payload")}); err != nil {
		t.Fatalf("EncodePacket = %v", err)
	}
	truncated := buf.Bytes()[:buf.Len()-3]
	if _, err := DecodePacket(bytes.NewReader(truncated)); err == nil {
		t.Fatal("DecodePacket(truncated) = nil error, want failure")
	}

	// Reserved packet type 0xF0 must be rejected.
	if _, err := DecodePacket(bytes.NewReader([]byte{0xF0, 0x00})); err == nil {
		t.Fatal("DecodePacket(reserved 0xF0) = nil error, want failure")
	}
}

func TestV0250ValidateTopicFilterExported(t *testing.T) {
	valid := []string{"devices/+/state", "a/b/#", "#", "+", "a/b/c", "a//b"}
	invalid := []string{"", "a/#/b", "a+/b", "#/x", "a/#b"}
	for _, f := range valid {
		if err := ValidateTopicFilter(f); err != nil {
			t.Errorf("ValidateTopicFilter(%q) = %v, want nil", f, err)
		}
	}
	for _, f := range invalid {
		if err := ValidateTopicFilter(f); err == nil {
			t.Errorf("ValidateTopicFilter(%q) = nil, want error", f)
		}
	}
}
