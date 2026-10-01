// v0400_test.go：统一告警中心存储单测（spec 0013 US-3）。
package alarmstore

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/pkg/alarm"
)

// fakeKV 是内存 KVStore（与 rulestore 测试同形态；单连接语义不模拟）。
type fakeKV struct {
	mu   sync.Mutex
	kv   map[string][]byte
	fail bool
}

func newFakeKV() *fakeKV { return &fakeKV{kv: map[string][]byte{}} }

func (f *fakeKV) Put(ctx context.Context, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("kv down")
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	f.kv[key] = cp
	return nil
}

func (f *fakeKV) Get(ctx context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.kv[key]
	if !ok {
		return nil, nil
	}
	return v, nil
}

func (f *fakeKV) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.kv, key)
	return nil
}

func (f *fakeKV) DeleteRange(ctx context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.kv {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(f.kv, k)
		}
	}
	return nil
}

func (f *fakeKV) Close() error { return nil }

func (f *fakeKV) ListByPrefix(ctx context.Context, prefix string) ([]etcdstore.KVEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]etcdstore.KVEntry, 0, len(f.kv))
	for k, v := range f.kv {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			cp := make([]byte, len(v))
			copy(cp, v)
			out = append(out, etcdstore.KVEntry{Key: k, Value: cp})
		}
	}
	return out, nil
}

// testCloudAlarm 构造云端入库告警。
func testCloudAlarm(id string) alarm.Alarm {
	return alarm.Alarm{
		AlarmID: id, NodeID: "node-1", Source: alarm.SourceRule, RuleID: "rule-a",
		Namespace: "default", DeviceName: "sensor-01",
		Severity: alarm.SeverityCritical, State: alarm.StateRaised,
		Message: "越限", Count: 1, RaisedAt: 1000, UpdatedAt: 1000,
	}
}

// recordingTickets 记录工单回调。
type recordingTickets struct {
	mu       sync.Mutex
	assigned []alarm.Alarm
}

func (r *recordingTickets) OnAssign(a alarm.Alarm) {
	r.mu.Lock()
	r.assigned = append(r.assigned, a)
	r.mu.Unlock()
}

// TestAlarmStoreUpsertMerge 覆盖合并语义（count 取大 / updatedAt 取新 / 状态不回退）。
func TestAlarmStoreUpsertMerge(t *testing.T) {
	s := NewStore(nil, nil)
	ctx := context.Background()
	a := testCloudAlarm("alm-1")
	if err := s.Upsert(ctx, a); err != nil {
		t.Fatalf("首触入库失败: %v", err)
	}
	// 迟到重发：count 更大、消息更新。
	a.Count = 7
	a.UpdatedAt = 2000
	a.Message = "越限（持续）"
	if err := s.Upsert(ctx, a); err != nil {
		t.Fatalf("重发入库失败: %v", err)
	}
	got, err := s.Get("alm-1")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.Count != 7 || got.Message != "越限（持续）" || got.UpdatedAt != 2000 {
		t.Fatalf("合并语义不符: %+v", got)
	}
	// 更小的 count/updatedAt 不回退。
	a.Count = 2
	a.UpdatedAt = 1500
	_ = s.Upsert(ctx, a)
	got, _ = s.Get("alm-1")
	if got.Count != 7 || got.UpdatedAt != 2000 {
		t.Fatalf("应取大: %+v", got)
	}
	// closed 后迟到重发整体忽略。
	got.State = alarm.StateClosed
	got.ClosedBy = "op"
	got.ClosedAt = 3000
	if err := s.putKV(ctx, got); err != nil {
		t.Fatalf("写穿 closed 失败: %v", err)
	}
	s.mu.Lock()
	cp := got
	s.alarms["alm-1"] = &cp
	s.mu.Unlock()
	stale := testCloudAlarm("alm-1")
	stale.Count = 99
	stale.UpdatedAt = 4000
	if err := s.Upsert(ctx, stale); err != nil {
		t.Fatalf("closed 后重发不应报错: %v", err)
	}
	final, _ := s.Get("alm-1")
	if final.Count == 99 {
		t.Fatalf("closed 后不应合并: %+v", final)
	}
	// 非法形态拒绝。
	bad := testCloudAlarm("alm-bad")
	bad.Severity = "fatal"
	if err := s.Upsert(ctx, bad); err == nil {
		t.Fatalf("非法告警应拒绝")
	}
}

// TestAlarmStoreTransitions 覆盖 ack/assign/close 状态机与错误映射。
func TestAlarmStoreTransitions(t *testing.T) {
	tickets := &recordingTickets{}
	s := NewStore(nil, tickets)
	ctx := context.Background()
	_ = s.Upsert(ctx, testCloudAlarm("alm-2"))

	// ack：raised → acked。
	got, err := s.Ack(ctx, "alm-2", "alice", 2000)
	if err != nil || got.State != alarm.StateAcked || got.AckedBy != "alice" {
		t.Fatalf("ack 失败: %v, %+v", err, got)
	}
	// acked → acked 非法（409）。
	if _, err := s.Ack(ctx, "alm-2", "bob", 3000); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("重复 ack 应 409: %v", err)
	}
	// acked → assigned 合法 + 工单回调。
	got, err = s.Assign(ctx, "alm-2", "alice", "维护班", "GD-001", 4000)
	if err != nil || got.State != alarm.StateAssigned || got.AssignedTo != "维护班" {
		t.Fatalf("assign 失败: %v, %+v", err, got)
	}
	tickets.mu.Lock()
	n := len(tickets.assigned)
	tickets.mu.Unlock()
	if n != 1 {
		t.Fatalf("工单应回调 1 次，实际 %d", n)
	}
	// assigned → closed 合法（终态）。
	got, err = s.Close(ctx, "alm-2", "alice", 5000)
	if err != nil || got.State != alarm.StateClosed || got.ClosedAt != 5000 {
		t.Fatalf("close 失败: %v, %+v", err, got)
	}
	// closed 后任何操作非法。
	if _, err := s.Ack(ctx, "alm-2", "bob", 6000); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("closed 后 ack 应 409: %v", err)
	}
	// 未知 ID → 404。
	if _, err := s.Ack(ctx, "nope", "bob", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知 ID 应 404: %v", err)
	}
}

// TestAlarmStoreListAndStats 覆盖过滤与统计。
func TestAlarmStoreListAndStats(t *testing.T) {
	s := NewStore(nil, nil)
	ctx := context.Background()
	for _, id := range []string{"a1", "a2", "a3"} {
		_ = s.Upsert(ctx, testCloudAlarm(id))
	}
	b := testCloudAlarm("a4")
	b.Severity = alarm.SeverityInfo
	b.NodeID = "node-2"
	_ = s.Upsert(ctx, b)
	_, _ = s.Ack(ctx, "a2", "op", 2000)

	if got := s.List("node-2", "", "", 0); len(got) != 1 || got[0].AlarmID != "a4" {
		t.Fatalf("nodeID 过滤不符: %+v", got)
	}
	if got := s.List("", alarm.StateAcked, "", 0); len(got) != 1 || got[0].AlarmID != "a2" {
		t.Fatalf("state 过滤不符: %+v", got)
	}
	if got := s.List("", "", alarm.SeverityInfo, 0); len(got) != 1 {
		t.Fatalf("severity 过滤不符: %+v", got)
	}
	if got := s.List("", "", "", 2); len(got) != 2 {
		t.Fatalf("limit 不符: %+v", got)
	}
	st := s.Stats()
	if st.Total != 4 || st.ByState[alarm.StateRaised] != 3 || st.BySeverity[alarm.SeverityCritical] != 3 {
		t.Fatalf("统计不符: %+v", st)
	}
}

// TestAlarmStoreLoadAndWriteThrough 覆盖 etcd 写穿与启动恢复。
func TestAlarmStoreLoadAndWriteThrough(t *testing.T) {
	kv := newFakeKV()
	s := NewStore(kv, nil)
	ctx := context.Background()
	_ = s.Upsert(ctx, testCloudAlarm("alm-kv"))
	raw, ok := kv.kv[KeyPrefixAlarms+"alm-kv"]
	if !ok {
		t.Fatalf("应写穿 etcd")
	}
	var back alarm.Alarm
	if err := json.Unmarshal(raw, &back); err != nil || back.AlarmID != "alm-kv" {
		t.Fatalf("etcd 值不符: %v", err)
	}
	// 新实例 Load 恢复。
	s2 := NewStore(kv, nil)
	if err := s2.Load(ctx); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if s2.Count() != 1 {
		t.Fatalf("恢复条数 = %d", s2.Count())
	}
	// 写穿失败：内存不动。
	kv.fail = true
	if err := s.Upsert(ctx, testCloudAlarm("alm-fail")); err == nil {
		t.Fatalf("kv 故障应报错")
	}
	if _, err := s.Get("alm-fail"); err == nil {
		t.Fatalf("写穿失败不应入内存")
	}
	// 文件持久化形态（走真 etcd 由 e2e 覆盖；此处验证 nil-kv 纯内存合法）。
	mem := NewStore(nil, nil)
	_ = mem.Upsert(ctx, testCloudAlarm("alm-mem"))
	if err := mem.Load(ctx); err != nil {
		t.Fatalf("nil-kv Load 应空转: %v", err)
	}
	_ = filepath.Join(t.TempDir(), "x") // 保持 t.TempDir 语义一致（无文件断言）
}
