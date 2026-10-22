// v0620_test.go：静默联动通知抑制单测（v0.62.0，spec 0035 裁决 1）。
package forwardstore

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"edgeflow/pkg/alarm"
)

// fakeStoreMutes 是 forwardstore.MuteSource 的 mutestore 语义替身：
// 节点+级别（severity 空 = 全级别）处于窗口 → MutedEvent true。
type fakeStoreMutes struct {
	nodes map[string]bool // 节点级静默（全级别）
}

func (f *fakeStoreMutes) MutedEvent(nodeID, severity string, nowMs int64) bool {
	return f.nodes[nodeID]
}

// US-1 静默联动：被静默节点的告警事件不投递（接收端零收包、台账零条目）；
// 未静默节点正常送达。
func TestV0620DispatchMuteSuppress(t *testing.T) {
	var hits int32
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer rcv.Close()

	s := New("")
	r, err := s.Create("mute-link", "", rcv.URL, "", "", "", "", false, []string{"alarm.raised"}, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	s.SetMutes(&fakeStoreMutes{nodes: map[string]bool{"n-muted": true}})

	// 静默节点事件 → 不投递（无台账、无收包）
	s.Dispatch("alarm.raised", alarm.Alarm{AlarmID: "alm-muted", NodeID: "n-muted", Source: alarm.SourceSystem,
		Severity: alarm.SeverityCritical, State: alarm.StateRaised, Count: 1, RaisedAt: 1000, UpdatedAt: 1000})
	time.Sleep(200 * time.Millisecond)
	if n, _, _ := bSnapshot(s, r.ID); n != 0 {
		t.Fatalf("静默节点事件不应产生台账: %d", n)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("静默节点事件不应送达")
	}

	// 未静默节点事件 → 正常送达
	s.Dispatch("alarm.raised", alarm.Alarm{AlarmID: "alm-live", NodeID: "n-live", Source: alarm.SourceSystem,
		Severity: alarm.SeverityCritical, State: alarm.StateRaised, Count: 1, RaisedAt: 1000, UpdatedAt: 1000})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if atomic.LoadInt32(&hits) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("非静默节点事件未送达")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n, _, _ := bSnapshot(s, r.ID); n != 1 {
		t.Fatalf("非静默事件台账应 1 条: %d", n)
	}
}

// bSnapshot 测试辅助：台账条数。
func bSnapshot(s *Store, ruleID string) (int, []Delivery, int) {
	list := s.Deliveries(ruleID, 100)
	return len(list), list, 0
}
