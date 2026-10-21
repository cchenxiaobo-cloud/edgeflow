// v0530_test.go：告警状态迁移挂点单测（spec 0026，forwardstore 装配面）。
package alarmstore

import (
	"context"
	"sync"
	"testing"

	"edgeflow/pkg/alarm"
)

// US-1 挂点事件序列：Upsert（新）→ raised；Ack → acked；Close → closed；
// 合并路径（同 ID 二次 Upsert）不发事件；终态后迟到重发不发事件。
func TestV0530TransitionHook(t *testing.T) {
	s := NewStore(nil, nil)
	var mu sync.Mutex
	var events []string
	s.SetTransitionHook(func(event string, a alarm.Alarm) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event+"|"+a.AlarmID)
	})
	ctx := context.Background()
	base := alarm.Alarm{AlarmID: "a1", NodeID: "n1", Source: alarm.SourceRule, RuleID: "r-1",
		Severity: alarm.SeverityWarning, State: alarm.StateRaised, Count: 1,
		RaisedAt: 1000, UpdatedAt: 1000}
	if err := s.Upsert(ctx, base); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// 合并路径（同 ID 重发，状态不变）→ 无事件
	if err := s.Upsert(ctx, base); err != nil {
		t.Fatalf("Upsert 重发: %v", err)
	}
	if _, err := s.Ack(ctx, "a1", "op", 2000); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if _, err := s.Close(ctx, "a1", "op", 3000); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 终态后迟到重发 → 无事件
	if err := s.Upsert(ctx, base); err != nil {
		t.Fatalf("终态后 Upsert: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"alarm.raised|a1", "alarm.acked|a1", "alarm.closed|a1"}
	if len(events) != len(want) {
		t.Fatalf("事件序列=%v（want %v）", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("事件序列[%d]=%s（want %s）", i, events[i], want[i])
		}
	}
}
