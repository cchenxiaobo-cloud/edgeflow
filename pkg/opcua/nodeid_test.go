package opcua

import (
	"testing"
)

// TestParseNodeID 表测 ParseNodeID 的合法/非法输入。
func TestParseNodeID(t *testing.T) {
	tests := []struct {
		in   string
		want NodeId
		ok   bool
	}{
		{in: "1001", want: NewNodeID(0, 1001), ok: true},
		{in: "ns=2;i=1001", want: NewNodeID(2, 1001), ok: true},
		{in: "ns=0;i=0", want: NewNodeID(0, 0), ok: true},
		{in: "ns=4294967295;i=4294967295", want: NodeId{Namespace: 0xFFFFFFFF, Type: NodeIDNumeric, Numeric: 0xFFFFFFFF}, ok: true},
		{in: "ns=0;s=temperature", want: NewStringNodeID(0, "temperature"), ok: true},
		{in: "ns=1;g=00112233-4455-6677-8899-aabbccddeeff", want: NewGuidNodeID(1, mustGuid(t, "00112233445566778899aabbccddeeff")), ok: true},
		{in: "ns=1;b=0011FF", want: NewByteStringNodeID(1, []byte{0x00, 0x11, 0xFF}), ok: true},
		// 非法
		{in: "", ok: false},
		{in: "ns=abc;i=1", ok: false},
		{in: "ns=-1;i=1", ok: false},
		{in: "ns=2;i=abc", ok: false},
		{in: "ns=2;s=", ok: false},
		{in: "ns=2;x=1", ok: false},
		{in: "ns=2", ok: false},
		{in: "ns=2;i=1;x=2", ok: false},
		{in: "ns=1;g=zzzz", ok: false},
		{in: "ns=1;b=0FF", ok: false},
		{in: "abc", ok: false},
	}
	for _, tt := range tests {
		got, err := ParseNodeID(tt.in)
		if tt.ok {
			if err != nil {
				t.Fatalf("ParseNodeID(%q) 意外错误: %v", tt.in, err)
			}
			if got.String() != tt.want.String() {
				t.Fatalf("ParseNodeID(%q) = %v, want %v", tt.in, got.String(), tt.want.String())
			}
			// 互逆：String() → ParseNodeID 再 String() 恒等
			again, err := ParseNodeID(got.String())
			if err != nil || again.String() != got.String() {
				t.Fatalf("String 互逆失败: %q → %v → %v (err %v)", tt.in, got.String(), again.String(), err)
			}
		} else if err == nil {
			t.Fatalf("ParseNodeID(%q) 应报错，got %v", tt.in, got.String())
		}
	}
}

func mustGuid(t *testing.T, s string) Guid {
	t.Helper()
	g, err := parseGUID(s)
	if err != nil {
		t.Fatalf("parseGUID(%q): %v", s, err)
	}
	return g
}
