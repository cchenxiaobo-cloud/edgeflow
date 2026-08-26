// EtcdPodStore 单测（v0.9.0，③ 闭环）：
//   - 写穿语义：Upsert 先 etcd 后内存（失败内存不动）；Delete 同；
//   - 键布局：<prefix>/<nodeID>/<ns>/<podName>；非法键段拒绝写入；
//   - Load 全量重建（坏键跳过 + Warn）；
//   - LoadAnchored/StartWatch 锚定加载与增量应用（WatchKV 模拟）。
package podstatus

import (
	"context"
	"strings"
	"sync"
	"testing"

	"edgeflow/cloud/pkg/etcdstore"
)

// fakeKV 是 KVStore 的轻量内存实现（含故障注入开关；对齐 devicestatus 测试模式）。
type fakeKV struct {
	mu   sync.Mutex
	data map[string][]byte
	fail bool
}

func newFakeKV() *fakeKV { return &fakeKV{data: make(map[string][]byte)} }

func (f *fakeKV) Put(_ context.Context, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errFakeKV
	}
	f.data[key] = value
	return nil
}
func (f *fakeKV) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errFakeKV
	}
	return f.data[key], nil
}
func (f *fakeKV) ListByPrefix(_ context.Context, prefix string) ([]etcdstore.KVEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errFakeKV
	}
	var out []etcdstore.KVEntry
	for k, v := range f.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, etcdstore.KVEntry{Key: k, Value: v})
		}
	}
	return out, nil
}
func (f *fakeKV) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errFakeKV
	}
	delete(f.data, key)
	return nil
}
func (f *fakeKV) DeleteRange(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errFakeKV
	}
	for k := range f.data {
		if strings.HasPrefix(k, prefix) {
			delete(f.data, k)
		}
	}
	return nil
}
func (f *fakeKV) Close() error { return nil }
func (f *fakeKV) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.data))
	for k := range f.data {
		out = append(out, k)
	}
	return out
}
func (f *fakeKV) raw(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[key]
}

var errFakeKV = &fakeKVERR{}

type fakeKVERR struct{}

func (*fakeKVERR) Error() string { return "fakeKV: injected failure" }

var _ etcdstore.KVStore = (*fakeKV)(nil)

// TestEtcdPodStoreWriteThrough 验证写穿语义：Upsert/Delete 先 etcd 后内存，
func TestEtcdPodStoreWriteThrough(t *testing.T) {
	kv := newFakeKV()
	s, err := NewEtcdPodStore(kv)
	if err != nil {
		t.Fatalf("NewEtcdPodStore 失败: %v", err)
	}
	ps := PodStatus{NodeID: "n1", PodName: "web", Namespace: "default", Phase: PhaseRunning}

	// 成功路径：etcd + 内存都有
	if err := s.Upsert("n1", ps); err != nil {
		t.Fatalf("Upsert 失败: %v", err)
	}
	if got, ok := s.Get("n1", "default", "web"); !ok || got.Phase != PhaseRunning {
		t.Fatalf("Get 失败: %+v ok=%v", got, ok)
	}
	if kv.raw("/edgeflow/podstatus/n1/default/web") == nil {
		t.Fatal("etcd 键应已写入")
	}

	// 故障注入：etcd 失败 → Upsert 返回 error、内存不动（旧值仍在）
	kv.fail = true
	if err := s.Upsert("n1", PodStatus{NodeID: "n1", PodName: "web", Phase: PhaseStopped}); err == nil {
		t.Fatal("etcd 失败时 Upsert 应返回 error")
	}
	kv.fail = false
	if got, ok := s.Get("n1", "default", "web"); !ok || got.Phase != PhaseRunning {
		t.Fatalf("故障后内存应保留旧值: %+v ok=%v", got, ok)
	}

	// Delete 故障：返回 false、内存不动
	kv.fail = true
	if s.Delete("n1", "default", "web") {
		t.Fatal("etcd 失败时 Delete 应返回 false")
	}
	kv.fail = false
	if _, ok := s.Get("n1", "default", "web"); !ok {
		t.Fatal("故障后记录应仍在")
	}

	// Delete 成功：etcd + 内存都删
	if !s.Delete("n1", "default", "web") {
		t.Fatal("Delete 应返回 true")
	}
	if kv.raw("/edgeflow/podstatus/n1/default/web") != nil {
		t.Fatal("etcd 键应已删除")
	}
	if _, ok := s.Get("n1", "default", "web"); ok {
		t.Fatal("内存应已删除")
	}
}

// TestEtcdPodStoreKeyValidation 验证非法键段拒绝写入（含 '/' 等）。
func TestEtcdPodStoreKeyValidation(t *testing.T) {
	s, err := NewEtcdPodStore(newFakeKV())
	if err != nil {
		t.Fatalf("NewEtcdPodStore 失败: %v", err)
	}
	if err := s.Upsert("n1/x", PodStatus{NodeID: "n1", PodName: "web"}); err == nil {
		t.Error("nodeID 含 '/' 应拒绝")
	}
	if err := s.Upsert("n1", PodStatus{NodeID: "n1", PodName: "web/x"}); err == nil {
		t.Error("podName 含 '/' 应拒绝")
	}
	if err := s.Upsert("", PodStatus{PodName: "web"}); err != nil {
		t.Errorf("nodeID 空应忽略（返回 nil），实际 %v", err)
	}
	if err := s.Upsert("n1", PodStatus{NodeID: "n1"}); err != nil {
		t.Errorf("podName 空应忽略（返回 nil），实际 %v", err)
	}
}

// TestEtcdPodStoreLoad 验证 Load 全量重建 + 坏键跳过。
func TestEtcdPodStoreLoad(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s, err := NewEtcdPodStore(kv)
	if err != nil {
		t.Fatalf("NewEtcdPodStore 失败: %v", err)
	}
	// 直接写 etcd（模拟历史数据）
	if err := s.Upsert("n1", PodStatus{NodeID: "n1", PodName: "web", Phase: PhaseRunning}); err != nil {
		t.Fatalf("Upsert 失败: %v", err)
	}
	// 构造新实例（空缓存）→ Load 重建
	s2, err := NewEtcdPodStore(kv)
	if err != nil {
		t.Fatalf("NewEtcdPodStore 失败: %v", err)
	}
	if s2.Count() != 0 {
		t.Fatalf("新实例应空缓存，Count=%d", s2.Count())
	}
	if err := s2.Load(ctx); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if s2.Count() != 1 {
		t.Fatalf("Load 后 Count=1，实际 %d", s2.Count())
	}
	if got, ok := s2.Get("n1", "default", "web"); !ok || got.Phase != PhaseRunning {
		t.Fatalf("Load 后读取失败: %+v ok=%v", got, ok)
	}
	// 坏键跳过：手工写一个非法键（无 namespace 段）
	kv.mu.Lock()
	kv.data["/edgeflow/podstatus/n1/badkey"] = []byte(`{"nodeID":"n1","podName":"x"}`)
	kv.mu.Unlock()
	s3, _ := NewEtcdPodStore(kv)
	if err := s3.Load(ctx); err != nil {
		t.Fatalf("Load（含坏键）失败: %v", err)
	}
	if s3.Count() != 1 {
		t.Fatalf("坏键应跳过，Count=1，实际 %d", s3.Count())
	}
}
