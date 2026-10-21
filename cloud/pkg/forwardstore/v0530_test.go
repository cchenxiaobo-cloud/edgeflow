// v0530_test.go：forwardstore 单测（spec 0026）。
package forwardstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/alarm"
)

// sampleAlarm 构造合法告警（Validate 全过）。
func sampleAlarm(id string) alarm.Alarm {
	return alarm.Alarm{AlarmID: id, NodeID: "n1", Source: alarm.SourceRule, RuleID: "r-1",
		Severity: alarm.SeverityWarning, State: alarm.StateRaised, Count: 1,
		RaisedAt: 1000, UpdatedAt: 1000, Message: "温度超限"}
}

// waitDelivery 轮询台账直到谓词满足。
func waitDelivery(t *testing.T, s *Store, ruleID string, want func([]Delivery) bool, timeout time.Duration) []Delivery {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := s.Deliveries(ruleID, 10)
		if want(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待送达台账超时（当前 %+v）", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// US-1 校验面：合法创建通过；7 类非法输入拒绝（ErrInvalid）。
func TestV0530CreateValidate(t *testing.T) {
	s := New("")
	if _, err := s.Create("告警转发", "http://example.com/hook", "sec", []string{"alarm.raised"}, true, 1000); err != nil {
		t.Fatalf("合法创建失败: %v", err)
	}
	long65 := make([]byte, 65)
	long129 := make([]byte, 129)
	bad := []struct {
		name, url, secret string
		events            []string
	}{
		{"", "http://example.com", "", []string{"alarm.raised"}},
		{string(long65), "http://example.com", "", []string{"alarm.raised"}},
		{"r1", "ftp://example.com", "", []string{"alarm.raised"}},
		{"r1", "http://", "", []string{"alarm.raised"}},
		{"r1", "http://example.com", string(long129), []string{"alarm.raised"}},
		{"r1", "http://example.com", "", nil},
		{"r1", "http://example.com", "", []string{"alarm.opened"}},
	}
	for i, b := range bad {
		if _, err := s.Create(b.name, b.url, b.secret, b.events, true, 1000); err == nil {
			t.Fatalf("非法用例 #%d 应拒绝", i)
		}
	}
}

// US-2 更新与删除：部分更新（name/events/enabled/secret 清除/同值 URL）；未知 404；重复删除 404。
func TestV0530UpdateDelete(t *testing.T) {
	s := New("")
	r, err := s.Create("r1", "http://a.com/h", "sec", []string{"alarm.raised"}, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	nn := "r2"
	ne := []string{"alarm.closed", "alarm.acked"}
	dis := false
	got, err := s.Update(r.ID, UpdateReq{Name: &nn, Events: ne, Enabled: &dis}, 2000)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "r2" || got.Enabled || len(got.Events) != 2 || got.UpdatedAt != 2000 || got.CreatedAt != 1000 {
		t.Fatalf("更新结果不符: %+v", got)
	}
	empty := ""
	if _, err = s.Update(r.ID, UpdateReq{Secret: &empty}, 3000); err != nil {
		t.Fatalf("密钥清除失败: %v", err)
	}
	if view, _ := s.Get(r.ID); view.HasSecret {
		t.Fatalf("密钥清除后应 hasSecret=false: %+v", view)
	}
	keepURL := "http://a.com/h"
	if _, err = s.Update(r.ID, UpdateReq{TargetURL: &keepURL}, 3500); err != nil {
		t.Fatalf("同值更新应通过: %v", err)
	}
	if _, err = s.Update("fw-nope", UpdateReq{Name: &nn}, 1); err != ErrNotFound {
		t.Fatalf("未知 id 应 ErrNotFound: %v", err)
	}
	badURL := "ftp://x"
	if _, err = s.Update(r.ID, UpdateReq{TargetURL: &badURL}, 1); err == nil {
		t.Fatalf("非法 URL 更新应拒绝")
	}
	if err = s.Delete(r.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatalf("删除后清单应空")
	}
	if err = s.Delete(r.ID); err != ErrNotFound {
		t.Fatalf("重复删除应 ErrNotFound: %v", err)
	}
}

// US-3 投递 + HMAC 签名：X-EdgeFlow-Signature = sha256=<hmac(body, secret)>；payload 字段齐。
func TestV0530DispatchHMAC(t *testing.T) {
	var mu sync.Mutex
	var gotBody []byte
	var gotSig string
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		gotSig = r.Header.Get("X-EdgeFlow-Signature")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer rcv.Close()

	s := New("")
	r, err := s.Create("hmac", rcv.URL, "s3cret", []string{"alarm.raised"}, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	s.Dispatch("alarm.raised", sampleAlarm("alm-1"))
	got := waitDelivery(t, s, r.ID, func(d []Delivery) bool {
		return len(d) == 1 && d[0].Status == "delivered"
	}, 5*time.Second)
	if got[0].Attempts != 1 || got[0].RespCode != 200 || got[0].AlarmID != "alm-1" {
		t.Fatalf("delivered 台账不符: %+v", got[0])
	}
	mu.Lock()
	defer mu.Unlock()
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(gotBody)
	if gotSig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("签名不符: %s", gotSig)
	}
	var p payload
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("payload 解析: %v", err)
	}
	if p.Event != "alarm.raised" || p.AlarmID != "alm-1" || p.NodeID != "n1" ||
		p.State != alarm.StateRaised || p.Severity != alarm.SeverityWarning {
		t.Fatalf("payload 不符: %+v", p)
	}
}

// US-4 失败重试：目标恒 500 → attempts=3（500ms/2s 退避）→ failed 台账。
func TestV0530DispatchRetryFail(t *testing.T) {
	var mu sync.Mutex
	count := 0
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer rcv.Close()

	s := New("")
	r, err := s.Create("fail", rcv.URL, "", []string{"alarm.acked"}, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	s.Dispatch("alarm.acked", sampleAlarm("alm-2"))
	got := waitDelivery(t, s, r.ID, func(d []Delivery) bool {
		return len(d) == 1 && d[0].Status == "failed"
	}, 8*time.Second)
	if got[0].Attempts != 3 || got[0].RespCode != 500 || got[0].Error != "HTTP 500" {
		t.Fatalf("重试台账不符: %+v", got[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 3 {
		t.Fatalf("应尝试 3 次: %d", count)
	}
}

// US-5 事件过滤与禁用：未订阅事件不投递；enabled=false 不投递。
func TestV0530EventFilterAndDisabled(t *testing.T) {
	var mu sync.Mutex
	count := 0
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer rcv.Close()

	s := New("")
	r, err := s.Create("flt", rcv.URL, "", []string{"alarm.closed"}, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	s.Dispatch("alarm.raised", sampleAlarm("alm-3"))
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	if count != 0 {
		mu.Unlock()
		t.Fatalf("未订阅事件不应投递（实投 %d）", count)
	}
	mu.Unlock()

	s.Dispatch("alarm.closed", sampleAlarm("alm-3"))
	waitDelivery(t, s, r.ID, func(d []Delivery) bool {
		return len(d) == 1 && d[0].Status == "delivered"
	}, 5*time.Second)

	dis := false
	if _, err := s.Update(r.ID, UpdateReq{Enabled: &dis}, 2000); err != nil {
		t.Fatal(err)
	}
	s.Dispatch("alarm.closed", sampleAlarm("alm-4"))
	time.Sleep(300 * time.Millisecond)
	if len(s.Deliveries(r.ID, 10)) != 1 {
		t.Fatalf("禁用后不应新增台账")
	}
}

// US-6 持久化 roundtrip：规则+台账重启恢复（含 hasSecret）。
func TestV0530PersistenceRoundtrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "forward.json")
	s := New(file)
	if err := s.Load(); err != nil {
		t.Fatalf("空载 Load: %v", err)
	}
	r, err := s.Create("persist", "http://a.com/h", "k", []string{"alarm.raised"}, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	s.recordDelivery(Delivery{ID: "dl-1", RuleID: r.ID, Event: "alarm.raised", AlarmID: "a", Status: "delivered", At: 1000})
	s2 := New(file)
	if err := s2.Load(); err != nil {
		t.Fatalf("恢复 Load: %v", err)
	}
	lst := s2.List()
	if len(lst) != 1 || lst[0].ID != r.ID || !lst[0].HasSecret {
		t.Fatalf("恢复规则不符: %+v", lst)
	}
	d := s2.Deliveries(r.ID, 10)
	if len(d) != 1 || d[0].ID != "dl-1" {
		t.Fatalf("恢复台账不符: %+v", d)
	}
}

// US-7 台账环形：每规则保留最近 100 条（新→旧）。
func TestV0530LedgerRing(t *testing.T) {
	s := New("")
	r, err := s.Create("ring", "http://a.com/h", "", []string{"alarm.raised"}, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 102; i++ {
		s.recordDelivery(Delivery{ID: fmt.Sprintf("dl-%03d", i), RuleID: r.ID, Status: "delivered", At: int64(i)})
	}
	got := s.Deliveries(r.ID, 200)
	if len(got) != 100 {
		t.Fatalf("环形上限应 100: %d", len(got))
	}
	if got[0].ID != "dl-101" || got[99].ID != "dl-002" {
		t.Fatalf("环形顺序不符: 首 %s 末 %s", got[0].ID, got[99].ID)
	}
}
