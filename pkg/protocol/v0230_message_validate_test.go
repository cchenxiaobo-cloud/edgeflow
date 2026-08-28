package protocol

import (
	"strings"
	"testing"
)

// v0.23.0（CHN-15 版本字段策略回归）：Validate 的 Version 宽松格式校验
// （^v[0-9]+$，不硬钉 "v1"）与 Timestamp 显式不校验（策略登记见
// message.go Validate 注释头）。
func TestValidateVersionFormat(t *testing.T) {
	base := func(v string) *Message {
		return &Message{ID: "1", Type: TypeAck, Version: v, Source: "cloud", Target: "edge-001", Timestamp: 1}
	}
	cases := []struct {
		name    string
		version string
		wantErr string
	}{
		{"current v1 ok", Version, ""},
		{"future v2 accepted (upgrade window)", "v2", ""},
		{"v10 accepted", "v10", ""},
		{"garbage rejected", "hello", "invalid version"},
		{"spaces rejected", "v1 beta", "invalid version"},
		{"uppercase rejected", "V1", "invalid version"},
		{"missing v prefix rejected", "1", "invalid version"},
		{"injection chars rejected", "v1\nINJ", "invalid version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := base(c.version).Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want contains %q", err, c.wantErr)
			}
		})
	}
}

// Timestamp 策略钉住：零值/负值不拒绝（显式登记策略，message.go 注释头）；
// 若未来收紧，本测试随策略变更一起裁决（先改这里，再改实现）。
func TestValidateTimestampPolicyUnvalidated(t *testing.T) {
	for _, ts := range []int64{0, -1, 1756377600000} {
		m := &Message{ID: "1", Type: TypeAck, Version: Version, Source: "cloud", Target: "edge-001", Timestamp: ts}
		if err := m.Validate(); err != nil {
			t.Errorf("Timestamp=%d should be accepted under registered policy, got err=%v", ts, err)
		}
	}
}
