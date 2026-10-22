// v0650_test.go：Import 保留源 ID / 去重跳过 / 非法条目 / tls:// 校验
// （v0.65.0 spec 0038；R5 复核 P2-1 + P3-1 处置补测）。
package forwardstore

import (
	"testing"
	"time"
)

// US-1 导出→导入 round-trip：源 ID 保留；同批二次导入全部 skipped（幂等）。
func TestV0650ImportRoundTripKeepsID(t *testing.T) {
	s1 := New("")
	if _, err := s1.Create("webhook-a", "webhook", "https://a.example/hook", "", "sec-a", "", "", false, []string{"alarm.raised"}, true, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	m, err := s1.Create("mqtt-b", "mqtt", "tcp://broker:1883", "t/1", "", "u", "p", true, []string{"alarm.closed"}, false, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}

	// 导出视图（List）→ ImportRule（模拟 API 层映射，含源 ID）。
	// 导出视图（List→RuleView 脱敏面）→ ImportRule（模拟 API 层映射，含源 ID）。
	// 脱敏语义：Secret/MqttPassword 不在视图（has* 布尔），导出→导入不携带密钥。
	var batch []ImportRule
	for _, r := range s1.List() {
		batch = append(batch, ImportRule{ID: r.ID, Name: r.Name, SinkType: r.SinkType,
			TargetURL: r.TargetURL, Topic: r.Topic,
			MqttUsername: r.MqttUsername, MqttTLS: r.MqttTLS,
			Events: r.Events, Enabled: r.Enabled})
	}

	s2 := New("")
	res := s2.Import(batch, time.Now().UnixMilli())
	if res.Imported != 2 || res.Skipped != 0 || len(res.Errors) != 0 {
		t.Fatalf("首次导入结果不符: %+v", res)
	}
	got := s2.List()
	if len(got) != 2 {
		t.Fatalf("导入后规则数 %d", len(got))
	}
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ID] = true
		if r.ID == m.ID && !r.MqttTLS {
			t.Fatalf("mqtt 规则字段丢失: %+v", r)
		}
	}
	if !ids[m.ID] {
		t.Fatalf("源 ID 未保留: got %v, want %s", ids, m.ID)
	}
	// 同批二次导入 → 全部 skipped（ID 已存在）。
	res2 := s2.Import(batch, time.Now().UnixMilli())
	if res2.Imported != 0 || res2.Skipped != 2 {
		t.Fatalf("二次导入应全跳过: %+v", res2)
	}
}

// US-2 非法条目跳过计数：合法条目照常导入，非法条目入 errors 不阻断。
func TestV0650ImportInvalidEntriesCounted(t *testing.T) {
	s := New("")
	batch := []ImportRule{
		{ID: "fw-ok", Name: "ok", SinkType: "webhook", TargetURL: "https://a.example/h", Events: []string{"alarm.raised"}, Enabled: true},
		{ID: "fw-bad-url", Name: "bad-url", SinkType: "webhook", TargetURL: "ftp://x", Events: []string{"alarm.raised"}, Enabled: true},
		{ID: "fw-bad-ev", Name: "bad-ev", SinkType: "webhook", TargetURL: "https://a.example/h", Events: []string{"nope"}, Enabled: true},
	}
	res := s.Import(batch, time.Now().UnixMilli())
	if res.Imported != 1 || len(res.Errors) != 2 {
		t.Fatalf("导入结果不符: %+v", res)
	}
}

// US-3 tls:// 前缀校验放行（R5 复核 P3-1 处置：mqttPublishQoS1 的 tls://
// 分支可达——校验器剥前缀后按 host:port 校验）。
func TestV0650ValidateMqttTLS(t *testing.T) {
	if err := validateRuleFields("n", "mqtt", "tls://broker:8883", "t/1", "", "", "", []string{"alarm.raised"}); err != nil {
		t.Fatalf("tls:// 应放行: %v", err)
	}
	if err := validateRuleFields("n", "mqtt", "tls:///bad/path", "t/1", "", "", "", []string{"alarm.raised"}); err == nil {
		t.Fatal("tls:///bad/path 应拒绝")
	}
}
