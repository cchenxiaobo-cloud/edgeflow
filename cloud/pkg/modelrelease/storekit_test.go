package modelrelease

// 测试用 fakeKV：内存实现 etcdstore.ExtendedKV 全扩展面
// （KVStore + AtomicKV + WatchKV + LeaseKV），语义对齐 clientv3 子集。
//
// 用途：EtcdModelStore 的控制器测试（P8-① guard 自愈、P8-③ cancel-
// before-claim 的 guard 释放与 perNode 补齐、崩溃点续跑断言）+ EtcdLockKV
// 测试。清单见 controller_test.go / lock_test.go。
//
// CAS 语义对齐 atomic.go 接口注释：
//   - CompareAndPut(expectRev>0)：ModRevision == expectRev 命中；
//   - CompareAndPut(expectRev==0)：CreateRevision == 0（键从未创建或已
//     被删除——delete 后重建键的 create-if-absent 语义与真实 etcd 一致）；
//   - GetAll/删除后 rev 采用全局递增计数器。
//
// 注意：本 fake 不是并发安全的完整实现（key 级互斥锁覆盖全部方法；
// -race 由 controller_test 的其他部分覆盖）。

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
)

type fakeKV struct {
	mu     sync.Mutex
	data   map[string][]byte
	modRev map[string]int64
	rev    int64
	lease  map[string]int64 // key → 活租约计数（GrantHeartbeatLease 记录；GuardedDelete 校验）
	// 以下为测试断言钩子
	putCount   int   // 总写次数（watch 应用器零写断言等用）
	leaseCount int   // GrantHeartbeatLease 次数
	failGet    error // 非 nil：Get 恒失败（模拟 etcd 不可用）
}

func newFakeKV() *fakeKV {
	return &fakeKV{
		data:   make(map[string][]byte),
		modRev: make(map[string]int64),
		lease:  make(map[string]int64),
	}
}

// nextRev 分配全局递增 ModRevision（etcd 语义：任何写都推进 revision）。
func (f *fakeKV) nextRev() int64 {
	f.rev++
	return f.rev
}

func (f *fakeKV) Put(_ context.Context, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCount++
	f.data[key] = append([]byte(nil), value...)
	f.modRev[key] = f.nextRev()
	return nil
}

func (f *fakeKV) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet != nil {
		return nil, f.failGet
	}
	v, ok := f.data[key]
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), v...), nil
}

func (f *fakeKV) ListByPrefix(_ context.Context, prefix string) ([]etcdstore.KVEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.data))
	for k := range f.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]etcdstore.KVEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, etcdstore.KVEntry{Key: k, Value: append([]byte(nil), f.data[k]...), Revision: f.modRev[k]})
	}
	return out, nil
}

func (f *fakeKV) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.data[key]; ok {
		f.putCount++
		f.nextRev() // 删除也推进 revision（etcd 行为）
	}
	delete(f.data, key)
	delete(f.modRev, key)
	return nil
}

func (f *fakeKV) DeleteRange(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.data {
		if strings.HasPrefix(k, prefix) {
			f.putCount++
			f.nextRev()
			delete(f.data, k)
			delete(f.modRev, k)
		}
	}
	return nil
}

func (f *fakeKV) Close() error { return nil }

// ── AtomicKV ───────────────────────────────────────────────────────────

func (f *fakeKV) GetWithRev(_ context.Context, key string) ([]byte, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[key]
	if !ok {
		return nil, 0, nil
	}
	return append([]byte(nil), v...), f.modRev[key], nil
}

func (f *fakeKV) CompareAndPut(_ context.Context, key string, value []byte, expectRev int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if expectRev > 0 {
		if f.modRev[key] != expectRev {
			return false, nil
		}
	} else {
		if _, ok := f.data[key]; ok {
			return false, nil // create-if-absent：键存在（含活键）→ 冲突
		}
	}
	f.putCount++
	f.data[key] = append([]byte(nil), value...)
	f.modRev[key] = f.nextRev()
	return true, nil
}

func (f *fakeKV) CompareAndDelete(_ context.Context, key string, expectRev int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.data[key]; !ok {
		// 本就缺失：expectRev==0 等值命中（原子接口契约：视同完成）
		return expectRev == 0, nil
	}
	if f.modRev[key] != expectRev {
		return false, nil
	}
	f.putCount++
	f.nextRev()
	delete(f.data, key)
	delete(f.modRev, key)
	return true, nil
}

func (f *fakeKV) GuardedDelete(_ context.Context, guardKey, targetKey string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lease[guardKey] > 0 {
		return false, nil // 守卫键存在活租约 → 拒绝删除
	}
	f.putCount++
	f.nextRev()
	delete(f.data, targetKey)
	delete(f.modRev, targetKey)
	return true, nil
}

// ── WatchKV ────────────────────────────────────────────────────────────

func (f *fakeKV) ListByPrefixRev(ctx context.Context, prefix string) ([]etcdstore.KVEntry, int64, error) {
	entries, err := f.ListByPrefix(ctx, prefix)
	if err != nil {
		return nil, 0, err
	}
	f.mu.Lock()
	rev := f.rev
	f.mu.Unlock()
	return entries, rev, nil
}

func (f *fakeKV) WatchPrefix(ctx context.Context, _ string, _ int64) <-chan etcdstore.WatchEvent {
	// 测试不驱动真实事件流（controller 测试直调 scanOnce）；返回一个随
	// ctx 取消关闭的空通道（避免 goroutine 泄漏；watchLoop 见 ctx.Err()
	// 后退出，不会误触发全量重放）。
	ch := make(chan etcdstore.WatchEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}

// ── LeaseKV ────────────────────────────────────────────────────────────

func (f *fakeKV) GrantHeartbeatLease(_ context.Context, key string, value []byte, _ time.Duration) error {
	// 语义对齐 LeaseKV 契约：Grant(ttl) + Put(key, value, WithLease)。
	// fake 侧租约过期不作真实计时（TryAcquire 的 expiresAt 值内编码承担
	// 独占判定），只记录次数供断言。
	f.mu.Lock()
	f.leaseCount++
	f.mu.Unlock()
	return f.Put(context.Background(), key, value)
}

// 编译期断言：fakeKV 满足完整扩展面。
var _ etcdstore.ExtendedKV = (*fakeKV)(nil)

// hasKey 断言键存在（测试辅助）。
func (f *fakeKV) hasKey(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.data[key]
	return ok
}

// lenKeys 返回键总数（测试辅助）。
func (f *fakeKV) lenKeys() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.data)
}
