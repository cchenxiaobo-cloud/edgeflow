package cloudhub

// v0.23.0 P2 新增用例（SEC-05 / SEC-06，主线实现，本文件仅测试）：
//   - SEC-05：Server.checkOrigin 握手来源校验（WithAllowedOrigins 注入白名单）
//   - SEC-06：sanitizeLogField 边缘可控字段 CRLF/控制字符消毒
// 本文件仅新增，不改动既有 *_test.go；实现见 server.go（主线落盘）。

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCheckOrigin 白名单语义（SEC-05）：
//   - 空白名单 = 全放行（默认行为不变，opt-in 收紧）
//   - 非空白名单：携带 Origin 且命中 → 放行；携带 Origin 未命中 → 拒绝；
//     缺失 Origin 头 → 放行（边缘 Go 原生客户端不发 Origin，浏览器必发——
//     跨站 WS 注入面只在「有 Origin」一侧）。
func TestCheckOrigin(t *testing.T) {
	t.Run("空白名单全放行", func(t *testing.T) {
		s := &Server{}
		for _, origin := range []string{"", "https://evil.example.com", "https://grafana.internal"} {
			req := httptest.NewRequest("GET", "/v1/edge", nil)
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			if !s.checkOrigin(req) {
				t.Errorf("空白名单 Origin=%q 应放行（默认行为不变）", origin)
			}
		}
	})

	t.Run("白名单命中放行", func(t *testing.T) {
		s := &Server{allowedOrigins: []string{"https://ops.example.com", "https://grafana.internal"}}
		req := httptest.NewRequest("GET", "/v1/edge", nil)
		req.Header.Set("Origin", "https://grafana.internal")
		if !s.checkOrigin(req) {
			t.Error("白名单命中应放行")
		}
	})

	t.Run("携带Origin未命中拒绝", func(t *testing.T) {
		s := &Server{allowedOrigins: []string{"https://ops.example.com"}}
		req := httptest.NewRequest("GET", "/v1/edge", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		if s.checkOrigin(req) {
			t.Error("携带 Origin 且未命中白名单应拒绝（跨站 WS 注入面）")
		}
		// 精确匹配语义：大小写/尾缀差异不命中。
		req2 := httptest.NewRequest("GET", "/v1/edge", nil)
		req2.Header.Set("Origin", "https://ops.example.com.evil.example.com")
		if s.checkOrigin(req2) {
			t.Error("后缀伪装 Origin 不应命中（精确匹配）")
		}
		req3 := httptest.NewRequest("GET", "/v1/edge", nil)
		req3.Header.Set("Origin", "https://OPS.example.com")
		if s.checkOrigin(req3) {
			t.Error("大小写不同不应命中（精确匹配，Origin 值区分大小写）")
		}
	})

	t.Run("缺失Origin放行", func(t *testing.T) {
		s := &Server{allowedOrigins: []string{"https://ops.example.com"}}
		req := httptest.NewRequest("GET", "/v1/edge", nil)
		if !s.checkOrigin(req) {
			t.Error("缺失 Origin 头应放行（边缘 Go 原生客户端不发 Origin）")
		}
	})
}

// TestSanitizeLogField 消毒规则：<0x20 与 0x7f 一律剥离（含 \r/\n/\t），
// 可打印字符（含中文/emoji 等 ≥0x80 rune）原样保留。
func TestSanitizeLogField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空串", "", ""},
		{"剥离CRLF", "edge-01\r\nfake: log line", "edge-01fake: log line"},
		{"剥离裸换行", "pod-a\npod-b", "pod-apod-b"},
		{"剥离Tab与控制字符", "a\tb\x00c\x1bd", "abcd"},
		{"剥离DEL", "ok\x7fno", "okno"},
		{"可打印保留", "edge-worker-01", "edge-worker-01"},
		{"中文保留", "节点-01", "节点-01"},
		{"高码位rune保留", "é中\U0001F600", "é中\U0001F600"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeLogField(tc.in); got != tc.want {
				t.Errorf("sanitizeLogField(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}

	// 注入场景验收：伪造日志行（注册字段内嵌 \n）不得产出第二行。
	injected := sanitizeLogField("victim\x1b[31m\n2026-08-28T00:00:00Z WARN forged")
	if strings.Contains(injected, "\n") {
		t.Errorf("消毒结果不应含换行: %q", injected)
	}
	if strings.Count(injected, "forged") != 1 {
		t.Errorf("内容应保留（剥离而非截断）: %q", injected)
	}
}
