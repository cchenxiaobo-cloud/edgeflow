// v0.6.0 SetDesired CAS 路径测试（fake ExtendedKV 驱动）：
//   - CAS 成功 → 内存更新 + etcd 记录（合并 Desired）
//   - 冲突 → 读最新基准重试（并发写 merge 不丢）
//   - 冲突耗尽 → ErrDesiredConflict + 内存不动
//   - create-if-absent（modRev=0）→ 首次指令成功
// （退化路径在 etcd_store_test.go 既有 fakeKV 覆盖；本文件专测扩展面）
package devicestatus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
)

// casKV 是测试用内存 ExtendedKV（revision 簿记 + 冲突注入）。
type casKV struct {
	mu         sync.Mutex
	m          map[string]casEntry
	nextRev    int64
	conflicts  int // 剩余强制冲突次数（每次 CompareAndPut 消耗 1；>0 时命中即冲突）
	failRead   bool
	failWrite  bool
}

type casEntry struct {
	val []byte
	rev int64
}

func newCASKV() *casKV { return &casKV{m: make(map[string]casEntry)} }

func (f *casKV) bump(key string, val []byte) {
	f.nextRev++
	f.m[key] = casEntry{val: val, rev: f.nextRev}
}

func (f *casKV) Put(_ context.Context, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWrite {
		return errors.New("put: injected")
	}
	f.bump(key, value)
	return nil
}

func (f *casKV) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRead {
		return nil, errors.New("get: injected")
	}
	e, ok := f.m[key]
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), e.val...), nil
}

func (f *casKV) ListByPrefix(_ context.Context, prefix string) ([]etcdstore.KVEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []etcdstore.KVEntry
	for k, e := range f.m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, etcdstore.KVEntry{Key: k, Value: append([]byte(nil), e.val...), Revision: e.rev})
		}
	}
	return out, nil
}

func (f *casKV) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key)
	return nil
}

func (f *casKV) DeleteRange(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.m {
		if strings.HasPrefix(k, prefix) {
			delete(f.m, k)
		}
	}
	return nil
}

func (f *casKV) Close() error { return nil }

// ---- AtomicKV ----

func (f *casKV) GetWithRev(_ context.Context, key string) ([]byte, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRead {
		return nil, 0, errors.New("getwithrev: injected")
	}
	e, ok := f.m[key]
	if !ok {
		return nil, 0, nil
	}
	return append([]byte(nil), e.val...), e.rev, nil
}

func (f *casKV) CompareAndPut(_ context.Context, key string, value []byte, expectRev int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWrite {
		return false, errors.New("cas: injected")
	}
	if f.conflicts > 0 {
		f.conflicts--
		if _, exists := f.m[key]; exists && expectRev > 0 {
			return false, nil // 模拟他写者已提交（rev 变了）
		}
	}
	e, exists := f.m[key]
	switch {
	case expectRev > 0:
		if !exists || e.rev != expectRev {
			return false, nil
		}
	case expectRev == 0:
		if exists {
			return false, nil
		}
	}
	f.bump(key, value)
	return true, nil
}

func (f *casKV) CompareAndDelete(_ context.Context, key string, expectRev int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, exists := f.m[key]
	if !exists {
		return true, nil
	}
	if e.rev != expectRev {
		return false, nil
	}
	delete(f.m, key)
	return true, nil
}

func (f *casKV) GuardedDelete(_ context.Context, guardKey, targetKey string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.m[guardKey]; ok {
		return false, nil
	}
	delete(f.m, targetKey)
	return true, nil
}

// ---- LeaseKV ----

func (f *casKV) GrantHeartbeatLease(_ context.Context, _ string, _ []byte, _ time.Duration) error { return nil }

// ---- WatchKV（测试不启 watch，停摆通道即满足接口） ----

func (f *casKV) ListByPrefixRev(_ context.Context, prefix string) ([]etcdstore.KVEntry, int64, error) {
	entries, err := f.ListByPrefix(context.Background(), prefix)
	if err != nil {
		return nil, 0, err
	}
	return entries, f.nextRev, nil
}

func (f *casKV) WatchPrefix(_ context.Context, _ string, _ int64) <-chan etcdstore.WatchEvent {
	return make(chan etcdstore.WatchEvent)
}

func newCASDeviceStore(f *casKV) (*EtcdDeviceStore, error) {
	return NewEtcdDeviceStore(f)
}

func casDeviceKey(nodeID, ns, dev string) string {
	return KeyPrefixDeviceStatus + "/" + nodeID + "/" + ns + "/" + dev
}

// 1) CAS 成功：内存更新 + etcd 记录含合并后的 Desired
func TestSetDesiredCASSuccess(t *testing.T) {
	f := newCASKV()
	s, err := newCASDeviceStore(f)
	if err != nil {
		t.Fatalf("NewEtcdDeviceStore: %v", err)
	}
	defer s.Close()
	if err := s.SetDesired("n1", "", "d1", "temp", 36.5); err != nil {
		t.Fatalf("SetDesired: %v", err)
	}
	// etcd 记录存在且 Desired 含 temp
	raw, err := f.Get(context.Background(), casDeviceKey("n1", "default", "d1"))
	if err != nil || raw == nil {
		t.Fatalf("etcd 记录缺失: raw=%v err=%v", raw, err)
	}
	var rec deviceShadowRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("记录反序列化失败: %v", err)
	}
	if rec.Desired["temp"] != 36.5 {
		t.Fatalf("Desired 未写入: %+v", rec.Desired)
	}
	// 内存一致
	st, ok := s.mem.Get("n1", "default", "d1")
	if !ok || st.Desired["temp"] != 36.5 {
		t.Fatalf("内存未更新: %+v ok=%v", st, ok)
	}
}

// 2) 冲突 → 读最新基准重试：并发写不同 property，最终两值并存（merge 不丢）
func TestSetDesiredCASConflictRetryMerges(t *testing.T) {
	f := newCASKV()
	s, err := newCASDeviceStore(f)
	if err != nil {
		t.Fatalf("NewEtcdDeviceStore: %v", err)
	}
	defer s.Close()
	// A 先写 temp
	if err := s.SetDesired("n1", "", "d1", "temp", 36.5); err != nil {
		t.Fatalf("SetDesired(temp): %v", err)
	}
	// 注入 1 次冲突：模拟他副本刚写了 humidity
	f.conflicts = 1
	_ = f.Put(context.Background(), casDeviceKey("n1", "default", "d1"), []byte(`{"nodeID":"n1","namespace":"default","deviceName":"d1","desired":{"humidity":60},"reported":{}}`))
	if err := s.SetDesired("n1", "", "d1", "temp", 37.0); err != nil {
		t.Fatalf("SetDesired(temp) 冲突重试后应成功: %v", err)
	}
	// 最终 etcd 记录两值并存
	raw, _ := f.Get(context.Background(), casDeviceKey("n1", "default", "d1"))
	var rec deviceShadowRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("记录反序列化失败: %v", err)
	}
	if rec.Desired["humidity"] != 60 || rec.Desired["temp"] != 37.0 {
		t.Fatalf("并发合并丢失: %+v", rec.Desired)
	}
	// 本副本内存：写穿路径成功后才更新（R3-3）——temp 已更新；humidity 是
	// 他副本写入，须经 watch 事件同步（本测试无 watch 流 → 手动派发等值
	// 事件验证幂等应用与最终一致）。
	st, _ := s.mem.Get("n1", "default", "d1")
	if st.Desired["temp"] != 37.0 {
		t.Fatalf("本副本写入未更新内存: %+v", st.Desired)
	}
	// watch 事件到达（他副本写入的 humidity 记录）→ 等值幂等应用
	s.applyPut(casDeviceKey("n1", "default", "d1"), raw)
	st, _ = s.mem.Get("n1", "default", "d1")
	if st.Desired["humidity"] != 60 {
		t.Fatalf("watch 同步后 humidity 应可见: %+v", st.Desired)
	}
}

// 3) 冲突耗尽 → ErrDesiredConflict + 内存不动
func TestSetDesiredCASConflictExhausted(t *testing.T) {
	f := newCASKV()
	s, err := newCASDeviceStore(f)
	if err != nil {
		t.Fatalf("NewEtcdDeviceStore: %v", err)
	}
	defer s.Close()
	if err := s.SetDesired("n1", "", "d1", "temp", 36.5); err != nil {
		t.Fatalf("SetDesired: %v", err)
	}
	f.conflicts = 99 // 连续冲突（每次重试读到的仍是旧 rev → 恒冲突）
	err = s.SetDesired("n1", "", "d1", "humidity", 60)
	if !errors.Is(err, ErrDesiredConflict) {
		t.Fatalf("应返回 ErrDesiredConflict，实际: %v", err)
	}
	// 内存未被更新（v0.5.0 语义：写失败内存不动）
	st, ok := s.mem.Get("n1", "default", "d1")
	if !ok {
		t.Fatal("记录不应被移除")
	}
	if _, exists := st.Desired["humidity"]; exists {
		t.Fatal("失败写入不应进入内存")
	}
	if st.Desired["temp"] != 36.5 {
		t.Fatal("既有 Desired 被破坏")
	}
}

// 4) create-if-absent：etcd 无记录时的首条指令成功
func TestSetDesiredCASCreateIfAbsent(t *testing.T) {
	f := newCASKV()
	s, err := newCASDeviceStore(f)
	if err != nil {
		t.Fatalf("NewEtcdDeviceStore: %v", err)
	}
	defer s.Close()
	if err := s.SetDesired("n9", "ns9", "dx", "on", 1); err != nil {
		t.Fatalf("SetDesired 首条指令: %v", err)
	}
	raw, err := f.Get(context.Background(), casDeviceKey("n9", "ns9", "dx"))
	if err != nil || raw == nil {
		t.Fatalf("记录缺失: %v", err)
	}
	var rec deviceShadowRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if rec.Desired["on"] != 1 {
		t.Fatalf("Desired 错误: %+v", rec.Desired)
	}
}

// 5) CAS 写失败（etcd 故障）→ error + 内存不动（v0.5.0 语义延续）
func TestSetDesiredCASWriteFailureKeepsMemory(t *testing.T) {
	f := newCASKV()
	s, err := newCASDeviceStore(f)
	if err != nil {
		t.Fatalf("NewEtcdDeviceStore: %v", err)
	}
	defer s.Close()
	if err := s.SetDesired("n1", "", "d1", "temp", 36.5); err != nil {
		t.Fatalf("SetDesired: %v", err)
	}
	f.failWrite = true
	err = s.SetDesired("n1", "", "d1", "humidity", 60)
	if err == nil {
		t.Fatal("写失败应返回 error")
	}
	st, _ := s.mem.Get("n1", "default", "d1")
	if _, exists := st.Desired["humidity"]; exists {
		t.Fatal("写失败不应更新内存")
	}
}