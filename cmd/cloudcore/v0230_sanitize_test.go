package main

import (
	"strings"
	"testing"
)

// v0.23.0（SEC-06）：CRLF/控制字符消毒 helper 单测。
// 背景：PodStatus/DeviceReport 上报回调把边缘可控字段打进日志
// （main.go PodStatus phase 白名单告警等），恶意边缘节点可在字段内
// 携带 \r\n 伪造日志行——入库前剥离。
func TestSanitizeEdgeField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain ascii", "nginx-1", "nginx-1"},
		{"crlf stripped", "pod\r\nINJECTED: yes", "podINJECTED: yes"},
		{"lone cr", "a\rb", "ab"},
		{"lone lf", "a\nb", "ab"},
		{"tab stripped", "a\tb", "ab"},
		{"del stripped", "a\x7fb", "ab"},
		{"control chars stripped", "a\x01\x02b", "ab"},
		{"utf8 preserved", "应用-pod✅", "应用-pod✅"},
		{"mixed", "ns\r\npod\t名\x01称", "nspod名称"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeEdgeField(c.in); got != c.want {
				t.Errorf("sanitizeEdgeField(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// 消毒结果不再包含任何 CRLF/控制字符（对随机脏输入的性质测试）。
func TestSanitizeEdgeFieldNoControlCharsRemain(t *testing.T) {
	dirty := "pod\r\nfake-log-line\x1b[31m\x00\x7f\t tail"
	got := sanitizeEdgeField(dirty)
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Errorf("output %q still contains control char %U", got, r)
		}
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("output %q still contains CRLF", got)
	}
}
