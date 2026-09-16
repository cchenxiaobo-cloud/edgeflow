// v0.37.0 测试锚（云端存储）：规则包存储 CRUD、哨兵错误、版本单调、
// 治理策略替换、事件 ring、etcd 写穿与 Load 恢复、写失败不回写内存、
// 损坏条目跳过。
package rulestore

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/pkg/rules"
)

// ── fakeKV：KVStore 内存实现 ──────────────────────────────────────────

type fakeKV struct {
	mu      sync.Mutex
	data    map[string][]byte
	failPut bool
}

func newFakeKV() *fakeKV { return &fakeKV{data: map[string][]byte{}} }

func (f *fakeKV) Put(_ context.Context, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut {
		return errors.New("模拟 etcd 故障")
	}
	f.data[key] = append([]byte(nil), value...)
	return nil
}

func (f *fakeKV) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[key]
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), v...), nil
}

func (f *fakeKV) ListByPrefix(_ context.Context, prefix string) ([]etcdstore.KVEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]etcdstore.KVEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, etcdstore.KVEntry{Key: k, Value: append([]byte(nil), f.data[k]...)})
	}
	return out, nil
}

func (f *fakeKV) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, key)
	return nil
}

func (f *fakeKV) DeleteRange(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.data {
		if strings.HasPrefix(k, prefix) {
			delete(f.data, k)
		}
	}
	return nil
}

func (f *fakeKV) Close() error { return nil }

// ── 测试夹具 ─────────────────────────────────────────────────────────

// r0370rule 构造一条合法规则。
func r0370rule(id string, threshold float64) rules.Rule {
	return rules.Rule{
		RuleID:     id,
		DeviceName: "sensor-01",
		Property:   "temperature",
		Condition:  rules.Condition{Type: rules.ConditionThreshold, Op: rules.OpGT, Value: threshold},
		Action:     rules.Action{Type: rules.ActionEvent},
	}
}

// ── 用例 ─────────────────────────────────────────────────────────────

func TestStoreCRUDInMemory(t *testing.T) {
	ctx := context.Background()
	s := New(nil)
	// 创建
	if err := s.CreateRule(ctx, r0370rule("r1", 80)); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if s.CountRules() != 1 {
		t.Fatalf("计数不符: %d", s.CountRules())
	}
	// 查询
	got, ok := s.GetRule("r1")
	if !ok || got.RuleID != "r1" || got.Condition.Value != 80 {
		t.Fatalf("查询不符: %+v ok=%v", got, ok)
	}
	// 列表排序（先建 r3/r2，断言排序输出）
	for _, id := range []string{"r3", "r2"} {
		if err := s.CreateRule(ctx, r0370rule(id, 1)); err != nil {
			t.Fatal(err)
		}
	}
	list := s.ListRules()
	if len(list) != 3 || list[0].RuleID != "r1" || list[1].RuleID != "r2" || list[2].RuleID != "r3" {
		t.Fatalf("列表排序不符: %+v", list)
	}
	// 更新
	upd := r0370rule("r1", 95)
	if err := s.UpdateRule(ctx, upd); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if got, _ := s.GetRule("r1"); got.Condition.Value != 95 {
		t.Fatalf("更新未生效: %+v", got)
	}
	// 删除
	if err := s.DeleteRule(ctx, "r1"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, ok := s.GetRule("r1"); ok {
		t.Fatal("删除未生效")
	}
	if s.CountRules() != 2 {
		t.Fatalf("删除后计数不符: %d", s.CountRules())
	}
}

func TestStoreSentinels(t *testing.T) {
	ctx := context.Background()
	s := New(nil)
	if err := s.CreateRule(ctx, r0370rule("r1", 80)); err != nil {
		t.Fatal(err)
	}
	// 重复创建 → ErrExists
	if err := s.CreateRule(ctx, r0370rule("r1", 80)); !errors.Is(err, ErrExists) {
		t.Fatalf("应 ErrExists: %v", err)
	}
	// 更新不存在 → ErrNotFound
	if err := s.UpdateRule(ctx, r0370rule("absent", 80)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("应 ErrNotFound: %v", err)
	}
	// 删除不存在 → ErrNotFound
	if err := s.DeleteRule(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("应 ErrNotFound: %v", err)
	}
	// 非法规则 → ErrInvalid
	bad := r0370rule("r1", 80)
	bad.RuleID = "BAD ID"
	if err := s.CreateRule(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("应 ErrInvalid: %v", err)
	}
}

func TestStoreGovernanceSetAndClear(t *testing.T) {
	ctx := context.Background()
	s := New(nil)
	ps := []rules.GovernancePolicy{
		{DeviceName: "d1", Property: "t", Deadband: 1},
		{DeviceName: "d1", Property: "p2", Range: &rules.Bounds{Min: 0, Max: 10}},
	}
	if err := s.SetGovernance(ctx, ps); err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	if got := s.Governance(); len(got) != 2 {
		t.Fatalf("回读不符: %+v", got)
	}
	// 重复键 → ErrInvalid
	dup := []rules.GovernancePolicy{
		{DeviceName: "d1", Property: "t", Deadband: 1},
		{DeviceName: "d1", Property: "t", Deadband: 2},
	}
	if err := s.SetGovernance(ctx, dup); !errors.Is(err, ErrInvalid) {
		t.Fatalf("重复策略应 ErrInvalid: %v", err)
	}
	// 失败不改变现状
	if len(s.Governance()) != 2 {
		t.Fatal("失败替换不应改变现状")
	}
	// 清空
	if err := s.SetGovernance(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(s.Governance()) != 0 {
		t.Fatal("清空未生效")
	}
}

func TestStoreRuleSetPackaging(t *testing.T) {
	ctx := context.Background()
	s := New(nil)
	for _, id := range []string{"b", "a", "c"} {
		if err := s.CreateRule(ctx, r0370rule(id, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetGovernance(ctx, []rules.GovernancePolicy{{DeviceName: "d", Property: "p", Deadband: 1}}); err != nil {
		t.Fatal(err)
	}
	rs := s.RuleSet()
	if rs.Version != s.Version() || rs.Version < 1 {
		t.Fatalf("版本不符: %d vs %d", rs.Version, s.Version())
	}
	if len(rs.Rules) != 3 || rs.Rules[0].RuleID != "a" || rs.Rules[1].RuleID != "b" || rs.Rules[2].RuleID != "c" {
		t.Fatalf("打包排序不符: %+v", rs.Rules)
	}
	if len(rs.Governance) != 1 {
		t.Fatalf("治理打包不符: %+v", rs.Governance)
	}
	// 打包的规则包应可通过自身校验（可下发）
	if err := rs.Validate(); err != nil {
		t.Fatalf("打包规则包应合法: %v", err)
	}
}

func TestStoreVersionMonotonic(t *testing.T) {
	ctx := context.Background()
	s := New(nil)
	last := s.Version()
	if last != 0 {
		t.Fatalf("初始版本应为 0: %d", last)
	}
	ops := []func() error{
		func() error { return s.CreateRule(ctx, r0370rule("r1", 1)) },
		func() error { return s.UpdateRule(ctx, r0370rule("r1", 2)) },
		func() error {
			return s.SetGovernance(ctx, []rules.GovernancePolicy{{DeviceName: "d", Property: "p", Deadband: 1}})
		},
		func() error { return s.DeleteRule(ctx, "r1") },
	}
	for i, op := range ops {
		if err := op(); err != nil {
			t.Fatalf("操作 %d 失败: %v", i, err)
		}
		if v := s.Version(); v <= last {
			t.Fatalf("版本未递增: %d → %d", last, v)
		} else {
			last = v
		}
	}
}

func TestStoreEventsRing(t *testing.T) {
	s := New(nil)
	// 容量滚动：写入容量+5 条，保留最后容量条
	for i := 0; i < EventRingCapacity+5; i++ {
		s.AppendEvent(rules.Event{RuleID: "r1", DeviceName: "d1", Value: float64(i), TriggeredAt: int64(i)})
	}
	if s.CountEvents() != EventRingCapacity {
		t.Fatalf("ring 容量不符: %d", s.CountEvents())
	}
	// 默认 limit=50、倒序（最新在前）
	evs := s.ListEvents(EventFilter{})
	if len(evs) != 50 {
		t.Fatalf("默认 limit 不符: %d", len(evs))
	}
	if evs[0].TriggeredAt != int64(EventRingCapacity+4) {
		t.Fatalf("倒序不符（首条应最新）: %d", evs[0].TriggeredAt)
	}
	// 过滤
	s.AppendEvent(rules.Event{RuleID: "other", DeviceName: "d9", TriggeredAt: 99999})
	if got := s.ListEvents(EventFilter{RuleID: "other"}); len(got) != 1 || got[0].RuleID != "other" {
		t.Fatalf("ruleId 过滤不符: %+v", got)
	}
	if got := s.ListEvents(EventFilter{DeviceName: "d9"}); len(got) != 1 {
		t.Fatalf("device 过滤不符: %+v", got)
	}
	// limit
	if got := s.ListEvents(EventFilter{Limit: 3}); len(got) != 3 {
		t.Fatalf("limit 不符: %d", len(got))
	}
}

func TestStoreEtcdWriteThroughAndLoad(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s1 := New(kv)
	if err := s1.CreateRule(ctx, r0370rule("r1", 80)); err != nil {
		t.Fatalf("写穿创建失败: %v", err)
	}
	if err := s1.SetGovernance(ctx, []rules.GovernancePolicy{{DeviceName: "d", Property: "p", Deadband: 1}}); err != nil {
		t.Fatal(err)
	}
	v1 := s1.Version()

	// 模拟重启：新 Store 从 kv 恢复
	s2 := New(kv)
	if err := s2.Load(ctx); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if s2.CountRules() != 1 {
		t.Fatalf("规则未恢复: %d", s2.CountRules())
	}
	if got, ok := s2.GetRule("r1"); !ok || got.Condition.Value != 80 {
		t.Fatalf("规则内容不符: %+v", got)
	}
	if len(s2.Governance()) != 1 {
		t.Fatalf("治理未恢复: %d", len(s2.Governance()))
	}
	if s2.Version() != v1 {
		t.Fatalf("版本未恢复: %d vs %d", s2.Version(), v1)
	}
	// 键空间断言：条目/版本/治理三类键存在
	if _, err := kv.Get(ctx, keyVersion); err != nil {
		t.Fatal(err)
	}
	if raw, _ := kv.Get(ctx, keyItemsPrefix+"r1"); len(raw) == 0 {
		t.Fatal("规则条目键缺失")
	}
	if raw, _ := kv.Get(ctx, keyGovernance); len(raw) == 0 {
		t.Fatal("治理键缺失")
	}
}

func TestStoreWriteFailureKeepsMemory(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	kv.failPut = true
	s := New(kv)
	if err := s.CreateRule(ctx, r0370rule("r1", 80)); err == nil {
		t.Fatal("写穿失败应返回错误")
	}
	if s.CountRules() != 0 {
		t.Fatal("写穿失败不应更新内存")
	}
	if s.Version() != 0 {
		t.Fatal("写穿失败不应递增版本")
	}
}

func TestStoreLoadCorruptSkips(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	// 一条好的 + 一条坏的
	good := rules.Rule{
		RuleID: "ok", DeviceName: "d", Property: "p",
		Condition: rules.Condition{Type: rules.ConditionThreshold, Op: rules.OpGT, Value: 1},
		Action:    rules.Action{Type: rules.ActionEvent},
	}
	raw, _ := jsonMarshalRule(good)
	_ = kv.Put(ctx, keyItemsPrefix+"ok", raw)
	_ = kv.Put(ctx, keyItemsPrefix+"bad", []byte("{not-json"))
	s := New(kv)
	if err := s.Load(ctx); err != nil {
		t.Fatalf("Load 应容忍损坏条目: %v", err)
	}
	if s.CountRules() != 1 {
		t.Fatalf("应恢复 1 条: %d", s.CountRules())
	}
	if _, ok := s.GetRule("ok"); !ok {
		t.Fatal("合法条目应恢复")
	}
}

// jsonMarshalRule 序列化规则（测试辅助）。
func jsonMarshalRule(r rules.Rule) ([]byte, error) {
	return json.Marshal(r)
}
