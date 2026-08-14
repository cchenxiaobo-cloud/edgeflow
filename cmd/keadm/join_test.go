package main

// join 参数校验与默认值测试（含 M4D P2-3 默认 node-id 清洗回归）。

import "testing"

// TestDefaultNodeIDSanitized 验证 M4D P2-3：默认 node-id 清洗（含点号主机名可用）。
func TestDefaultNodeIDSanitized(t *testing.T) {
	got := sanitizeNodeIDChars("MacdeMacBook-Pro.local")
	if got != "MacdeMacBook-Pro-local" {
		t.Errorf("sanitizeNodeIDChars = %q，期望 MacdeMacBook-Pro-local", got)
	}
	if got := sanitizeNodeIDChars("..a..b.."); got != "a-b" {
		t.Errorf("连续非法字符应压缩，got %q", got)
	}
	if got := sanitizeNodeIDChars("..."); got != "" {
		t.Errorf("全非法字符应得空串，got %q", got)
	}
	// 默认值必须能通过 join 校验（白名单）
	id := defaultNodeID()
	if err := (joinOptions{CloudCoreIP: "127.0.0.1", Token: "t", NodeID: id}).validate(); err != nil {
		t.Errorf("默认 node-id %q 应通过校验，实际: %v", id, err)
	}
}
