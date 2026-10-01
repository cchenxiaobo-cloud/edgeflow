// v0400_alarm_api_test.go：统一告警中心 API 单测（spec 0013 US-3）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"edgeflow/cloud/pkg/alarmstore"
	"edgeflow/pkg/alarm"
)

// v0400Do 发请求并返回状态码与响应体（不因非 200 而 Fatal）。
func v0400Do(t *testing.T, method, url string, body any) (int, string) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// newAlarmTestMux 构造告警 API 测试路由。
func newAlarmTestMux(t *testing.T) (*http.ServeMux, *alarmstore.Store) {
	t.Helper()
	st := alarmstore.NewStore(nil, nil)
	mux := http.NewServeMux()
	(&alarmAPI{store: st}).Register(mux)
	return mux, st
}

// TestAlarmAPIListAndStats 覆盖空态与统计端点。
func TestAlarmAPIListAndStats(t *testing.T) {
	mux, st := newAlarmTestMux(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	code, body := v0400Do(t, "GET", srv.URL+"/api/v1/alarms", nil)
	if code != 200 {
		t.Fatalf("空列表应 200: %d %s", code, body)
	}
	var listResp struct {
		Alarms []alarm.Alarm `json:"alarms"`
		Count  int           `json:"count"`
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil || listResp.Count != 0 {
		t.Fatalf("空列表不符: %s", body)
	}

	a := alarm.Alarm{AlarmID: "alm-1", NodeID: "node-1", Source: alarm.SourceRule, RuleID: "r1",
		Severity: alarm.SeverityWarning, State: alarm.StateRaised,
		Count: 1, RaisedAt: 1000, UpdatedAt: 1000}
	if err := st.Upsert(context.Background(), a); err != nil {
		t.Fatalf("入库失败: %v", err)
	}
	code, body = v0400Do(t, "GET", srv.URL+"/api/v1/alarms?severity=warning", nil)
	if code != 200 {
		t.Fatalf("列表应 200: %d", code)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil || listResp.Count != 1 {
		t.Fatalf("列表不符: %s", body)
	}
	code, body = v0400Do(t, "GET", srv.URL+"/api/v1/alarms/stats", nil)
	if code != 200 {
		t.Fatalf("统计应 200: %d %s", code, body)
	}
	var stats struct {
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal([]byte(body), &stats); err != nil || stats.Total != 1 {
		t.Fatalf("统计不符: %s", body)
	}
}

// TestAlarmAPITransitionEndpoints 覆盖 ack/assign/close 与 404/409/400。
func TestAlarmAPITransitionEndpoints(t *testing.T) {
	mux, st := newAlarmTestMux(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 未知告警 → 404。
	code, _ := v0400Do(t, "POST", srv.URL+"/api/v1/alarms/nope/ack", map[string]string{"operator": "op"})
	if code != 404 {
		t.Fatalf("未知告警应 404: %d", code)
	}
	// 缺 operator → 400。
	if err := st.Upsert(context.Background(), alarm.Alarm{AlarmID: "alm-9", NodeID: "n", Source: alarm.SourceRule,
		RuleID: "r", Severity: alarm.SeverityInfo, State: alarm.StateRaised, Count: 1, RaisedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("入库失败: %v", err)
	}
	code, _ = v0400Do(t, "POST", srv.URL+"/api/v1/alarms/alm-9/ack", map[string]string{})
	if code != 400 {
		t.Fatalf("缺 operator 应 400: %d", code)
	}
	// ack → 200（状态 acked）。
	code, body := v0400Do(t, "POST", srv.URL+"/api/v1/alarms/alm-9/ack", map[string]string{"operator": "op"})
	if code != 200 {
		t.Fatalf("ack 应 200: %d %s", code, body)
	}
	var got alarm.Alarm
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.State != alarm.StateAcked {
		t.Fatalf("ack 结果不符: %s", body)
	}
	// 重复 ack → 409。
	code, _ = v0400Do(t, "POST", srv.URL+"/api/v1/alarms/alm-9/ack", map[string]string{"operator": "op"})
	if code != 409 {
		t.Fatalf("重复 ack 应 409: %d", code)
	}
	// assign 缺 assignee → 400。
	code, _ = v0400Do(t, "POST", srv.URL+"/api/v1/alarms/alm-9/assign", map[string]string{"operator": "op"})
	if code != 400 {
		t.Fatalf("缺 assignee 应 400: %d", code)
	}
	// assign → assigned。
	code, body = v0400Do(t, "POST", srv.URL+"/api/v1/alarms/alm-9/assign",
		map[string]string{"operator": "op", "assignee": "维护班", "ticketRef": "GD-1"})
	if code != 200 {
		t.Fatalf("assign 应 200: %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.State != alarm.StateAssigned || got.TicketRef != "GD-1" {
		t.Fatalf("assign 结果不符: %s", body)
	}
	// close → closed（终态）。
	code, body = v0400Do(t, "POST", srv.URL+"/api/v1/alarms/alm-9/close", map[string]string{"operator": "op"})
	if code != 200 {
		t.Fatalf("close 应 200: %d %s", code, body)
	}
	// closed 后 close 再来 → 409。
	code, _ = v0400Do(t, "POST", srv.URL+"/api/v1/alarms/alm-9/close", map[string]string{"operator": "op"})
	if code != 409 {
		t.Fatalf("终态后 close 应 409: %d", code)
	}
}
