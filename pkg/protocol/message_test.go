package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewMessage(t *testing.T) {
	payload := map[string]string{"nodeID": "edge-001"}
	m, err := NewMessage(TypeRegister, "edge-001", "cloud", payload)
	if err != nil {
		t.Fatalf("NewMessage error: %v", err)
	}
	if m.Type != TypeRegister {
		t.Errorf("type = %q, want %q", m.Type, TypeRegister)
	}
	if m.Version != Version {
		t.Errorf("version = %q, want %q", m.Version, Version)
	}
	if m.ID == "" {
		t.Error("id should not be empty")
	}
	if m.Timestamp == 0 {
		t.Error("timestamp should not be zero")
	}
	// 校验 payload 可解析回原值
	var got map[string]string
	if err := m.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload error: %v", err)
	}
	if got["nodeID"] != "edge-001" {
		t.Errorf("payload nodeID = %q, want edge-001", got["nodeID"])
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	m, err := NewMessage(TypeHeartbeat, "edge-001", "cloud", map[string]string{"ts": "1"})
	if err != nil {
		t.Fatalf("NewMessage error: %v", err)
	}
	data, err := Encode(m)
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	// 必须以换行结尾
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("encoded data should end with newline")
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if got.Type != m.Type || got.Source != m.Source || got.Target != m.Target {
		t.Errorf("round trip mismatch: %+v vs %+v", got, m)
	}
	if got.ID != m.ID {
		t.Errorf("id mismatch: %q vs %q", got.ID, m.ID)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		msg     Message
		wantErr string
	}{
		{"valid", Message{ID: "1", Type: TypeAck, Version: Version, Source: "cloud", Target: "edge-001"}, ""},
		{"missing type", Message{ID: "1", Version: Version, Source: "cloud", Target: "edge-001"}, "type is required"},
		{"missing version", Message{ID: "1", Type: TypeAck, Source: "cloud", Target: "edge-001"}, "version is required"},
		{"missing source", Message{ID: "1", Type: TypeAck, Version: Version, Target: "edge-001"}, "source is required"},
		{"missing target", Message{ID: "1", Type: TypeAck, Version: Version, Source: "cloud"}, "target is required"},
		{"missing id", Message{Type: TypeAck, Version: Version, Source: "cloud", Target: "edge-001"}, "id is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.msg.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want contains %q", err, c.wantErr)
			}
		})
	}
}

func TestDecodeInvalidJSON(t *testing.T) {
	_, err := Decode([]byte("{not json"))
	if err == nil {
		t.Error("expected error for invalid json")
	}
}

func TestDecodeEmptyPayload(t *testing.T) {
	m := &Message{ID: "1", Type: TypeAck, Version: Version, Source: "cloud", Target: "edge-001", Payload: json.RawMessage(`{}`)}
	var out map[string]string
	if err := m.DecodePayload(&out); err != nil {
		t.Fatalf("DecodePayload error: %v", err)
	}
	m2 := &Message{ID: "1", Type: TypeAck, Version: Version, Source: "cloud", Target: "edge-001"}
	if err := m2.DecodePayload(&out); err == nil {
		t.Error("expected error for empty payload")
	}
}
