// v0520_test.go：CountByNodeState 单测（spec 0025 裁决 2，KI §52#1 修复锚）。
package alarmstore

import (
	"context"
	"testing"

	"edgeflow/pkg/alarm"
)

// US-1 节点×状态计数：多节点多状态精确计数；总数与 store 一致。
func TestV0520CountByNodeState(t *testing.T) {
	s := NewStore(nil, nil)
	ctx := context.Background()
	alarms := []alarm.Alarm{
		{AlarmID: "a1", NodeID: "n1", Source: alarm.SourceRule, RuleID: "r-1", Severity: alarm.SeverityWarning, State: alarm.StateRaised, Count: 1, RaisedAt: 1000, UpdatedAt: 1000},
		{AlarmID: "a2", NodeID: "n1", Source: alarm.SourceRule, RuleID: "r-1", Severity: alarm.SeverityWarning, State: alarm.StateRaised, Count: 1, RaisedAt: 1000, UpdatedAt: 1000},
		{AlarmID: "a3", NodeID: "n1", Source: alarm.SourceDevice, Severity: alarm.SeverityInfo, State: alarm.StateClosed, Count: 1, RaisedAt: 1000, UpdatedAt: 1000},
		{AlarmID: "a4", NodeID: "n2", Source: alarm.SourceSystem, Severity: alarm.SeverityCritical, State: alarm.StateAcked, Count: 1, RaisedAt: 1000, UpdatedAt: 1000},
	}
	for _, a := range alarms {
		if err := s.Upsert(ctx, a); err != nil {
			t.Fatalf("Upsert %s: %v", a.AlarmID, err)
		}
	}
	got := s.CountByNodeState()
	if len(got) != 2 {
		t.Fatalf("节点数=%d（want 2）", len(got))
	}
	if got["n1"][alarm.StateRaised] != 2 || got["n1"][alarm.StateClosed] != 1 {
		t.Fatalf("n1 计数不符: %+v", got["n1"])
	}
	if got["n2"][alarm.StateAcked] != 1 {
		t.Fatalf("n2 计数不符: %+v", got["n2"])
	}
	if s.Count() != 4 {
		t.Fatalf("store 总数=%d（want 4）", s.Count())
	}
}

// US-2 空库返回空 map（非 nil）；状态迁移后计数跟随（Ack → acked）。
func TestV0520CountAfterTransition(t *testing.T) {
	s := NewStore(nil, nil)
	empty := s.CountByNodeState()
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空库应返回空 map: %+v", empty)
	}
	ctx := context.Background()
	if err := s.Upsert(ctx, alarm.Alarm{AlarmID: "a1", NodeID: "n1", Source: alarm.SourceRule, RuleID: "r-1", Severity: alarm.SeverityWarning, State: alarm.StateRaised, Count: 1, RaisedAt: 1000, UpdatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ack(ctx, "a1", "op", 2000); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	got := s.CountByNodeState()
	if got["n1"][alarm.StateAcked] != 1 || got["n1"][alarm.StateRaised] != 0 {
		t.Fatalf("迁移后计数不符: %+v", got["n1"])
	}
}
