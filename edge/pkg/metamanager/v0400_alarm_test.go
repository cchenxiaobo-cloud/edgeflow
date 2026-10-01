// v0400_alarm_test.go：告警台账与设定值缓存单测（spec 0013 US-2/US-5）。
package metamanager

import (
	"path/filepath"
	"testing"

	"edgeflow/pkg/alarm"
)

// openTestAlarmLedger 在临时目录打开 Store + AlarmLedger。
func openTestAlarmLedger(t *testing.T) (*AlarmLedger, *Store) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("打开测试 Store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	l, err := NewAlarmLedger(st)
	if err != nil {
		t.Fatalf("创建告警台账失败: %v", err)
	}
	return l, st
}

// testEdgeAlarm 构造边侧告警。
func testEdgeAlarm(id, dedup string) alarm.Alarm {
	return alarm.Alarm{
		AlarmID: id, NodeID: "node-1", Source: alarm.SourceRule, RuleID: "rule-a",
		Namespace: "default", DeviceName: "sensor-01",
		Severity: alarm.SeverityWarning, State: alarm.StateRaised,
		Message: "越限", Count: 1, RaisedAt: 1000, UpdatedAt: 1000,
	}
}

// TestAlarmLedgerUpsertAndActive 覆盖写穿、按 dedupKey 查活跃、closed 排除。
func TestAlarmLedgerUpsertAndActive(t *testing.T) {
	l, _ := openTestAlarmLedger(t)
	a := testEdgeAlarm("alm-1", "")
	if err := l.UpsertAlarm(a); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, err := l.ActiveAlarmByDedupKey(a.DedupKey())
	if err != nil || got == nil {
		t.Fatalf("应查到活跃告警: %v, %+v", err, got)
	}
	if got.AlarmID != "alm-1" {
		t.Fatalf("alarmId = %s, want alm-1", got.AlarmID)
	}
	// 聚合更新（同 ID 覆盖：count=3）。
	a.Count = 3
	a.UpdatedAt = 2000
	if err := l.UpsertAlarm(a); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	got, _ = l.ActiveAlarmByDedupKey(a.DedupKey())
	if got.Count != 3 {
		t.Fatalf("聚合 count = %d, want 3", got.Count)
	}
	// 置 closed 后不再视为活跃。
	a.State = alarm.StateClosed
	if err := l.UpsertAlarm(a); err != nil {
		t.Fatalf("closed 写入失败: %v", err)
	}
	got, err = l.ActiveAlarmByDedupKey(a.DedupKey())
	if err != nil || got != nil {
		t.Fatalf("closed 后应无活跃告警: %v, %+v", err, got)
	}
	// 另一 dedupKey 不串。
	other := testEdgeAlarm("alm-2", "")
	other.RuleID = "rule-b"
	other.AlarmID = "alm-2"
	if err := l.UpsertAlarm(other); err != nil {
		t.Fatalf("写入第二条失败: %v", err)
	}
	got, _ = l.ActiveAlarmByDedupKey(other.DedupKey())
	if got == nil || got.AlarmID != "alm-2" {
		t.Fatalf("另一 episode 应独立: %+v", got)
	}
}

// TestAlarmLedgerPersistAcrossReopen 覆盖重开持久（断网本地缓存语义的数据面）。
func TestAlarmLedgerPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	l, err := NewAlarmLedger(st)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	a := testEdgeAlarm("alm-keep", "")
	if err := l.UpsertAlarm(a); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	_ = st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer func() { _ = st2.Close() }()
	l2, err := NewAlarmLedger(st2)
	if err != nil {
		t.Fatalf("重创建失败: %v", err)
	}
	got, err := l2.ActiveAlarmByDedupKey(a.DedupKey())
	if err != nil || got == nil || got.AlarmID != "alm-keep" {
		t.Fatalf("重开后活跃告警应仍在: %v, %+v", err, got)
	}
}

// TestSetpointCacheUpsertAndGet 覆盖 (namespace, property) UPSERT 与读取。
func TestSetpointCacheUpsertAndGet(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	c, err := NewSetpointCache(st)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	rec := SetpointCacheRecord{Namespace: "default", Property: "targetTemp",
		DeviceName: "sensor-01", Value: 25.5, SetpointID: "sp-1"}
	if err := c.SaveSetpointCache(rec); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, err := c.GetSetpointCache("default", "targetTemp")
	if err != nil || got == nil {
		t.Fatalf("应读到缓存: %v, %+v", err, got)
	}
	if got.Value != 25.5 || got.SetpointID != "sp-1" {
		t.Fatalf("缓存值不符: %+v", got)
	}
	// 同键覆盖（新值 + 新建单）。
	rec.Value = 30
	rec.SetpointID = "sp-2"
	rec.Ts = 999
	if err := c.SaveSetpointCache(rec); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	got, _ = c.GetSetpointCache("default", "targetTemp")
	if got.Value != 30 || got.SetpointID != "sp-2" || got.Ts != 999 {
		t.Fatalf("覆盖后不符: %+v", got)
	}
	// 无记录 → (nil, nil)。
	got, err = c.GetSetpointCache("default", "absent")
	if err != nil || got != nil {
		t.Fatalf("无记录应 (nil, nil): %v, %+v", err, got)
	}
	// 缺键拒绝。
	if err := c.SaveSetpointCache(SetpointCacheRecord{Property: "p"}); err == nil {
		t.Fatalf("缺 namespace 应拒绝")
	}
}
