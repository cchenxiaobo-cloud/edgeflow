// v0390_test.go：事件在途缓冲单测（spec 0012 US-5）。
package rulestore

import (
	"context"
	"encoding/json"
	"testing"

	"edgeflow/pkg/rules"
)

// v0390testEvent 构造一条测试事件。
func v0390testEvent(id string) rules.Event {
	return rules.Event{RuleID: id, DeviceName: "dev-1", Severity: "warning", TriggeredAt: 1000, Value: 1}
}

// TestAppendEventBufferedWriteDelete 验证写→入环→删闭环（无残留）。
func TestAppendEventBufferedWriteDelete(t *testing.T) {
	kv := newFakeKV()
	s := New(kv)
	ctx := context.Background()
	if err := s.AppendEventBuffered(ctx, "node-1", v0390testEvent("r1")); err != nil {
		t.Fatalf("缓冲写入失败: %v", err)
	}
	if s.CountEvents() != 1 {
		t.Fatalf("ring 应有 1 条，实际 %d", s.CountEvents())
	}
	if entries, _ := kv.ListByPrefix(ctx, keyRuleEventsPrefix); len(entries) != 0 {
		t.Fatalf("缓冲应被清理，残留 %d 条", len(entries))
	}
}

// TestAppendEventBufferedPutError 验证写缓冲失败返回错误且不入环（装配层降级）。
func TestAppendEventBufferedPutError(t *testing.T) {
	kv := newFakeKV()
	kv.failPut = true
	s := New(kv)
	if err := s.AppendEventBuffered(context.Background(), "n", v0390testEvent("r1")); err == nil {
		t.Fatal("Put 失败应返回错误（装配层降级内存环）")
	}
	if s.CountEvents() != 0 {
		t.Fatal("Put 失败时不应入 ring（由装配层降级处理）")
	}
}

// TestLoadRestoresPendingEvents 验证启动恢复：pending 进 ring 并清除，损坏跳过。
func TestLoadRestoresPendingEvents(t *testing.T) {
	kv := newFakeKV()
	ctx := context.Background()
	raw1, _ := json.Marshal(v0390testEvent("r1"))
	raw2, _ := json.Marshal(v0390testEvent("r2"))
	_ = kv.Put(ctx, keyRuleEventsPrefix+"node-1/100-aa", raw1)
	_ = kv.Put(ctx, keyRuleEventsPrefix+"node-1/101-bb", raw2)
	_ = kv.Put(ctx, keyRuleEventsPrefix+"node-2/99-cc", []byte("{bad"))
	s := New(kv)
	if err := s.Load(ctx); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if s.CountEvents() != 2 {
		t.Fatalf("应恢复 2 条，实际 %d", s.CountEvents())
	}
	if entries, _ := kv.ListByPrefix(ctx, keyRuleEventsPrefix); len(entries) != 0 {
		t.Fatalf("恢复后缓冲应清空，残留 %d", len(entries))
	}
	if evs := s.ListEvents(EventFilter{Limit: 10}); len(evs) != 2 {
		t.Fatalf("ListEvents 应 2 条，实际 %d", len(evs))
	}
}

// TestAppendEventBufferedNilKV 验证纯内存形态（kv=nil）仅入环不报错。
func TestAppendEventBufferedNilKV(t *testing.T) {
	s := New(nil)
	if err := s.AppendEventBuffered(context.Background(), "n", v0390testEvent("r1")); err != nil {
		t.Fatalf("nil kv 不应报错: %v", err)
	}
	if s.CountEvents() != 1 {
		t.Fatal("nil kv 应仅入 ring")
	}
}
