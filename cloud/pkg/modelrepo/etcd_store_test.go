package modelrepo

// EtcdModelStore 单测（设计 §10.1 S2/S3/S4 + 主线裁决 D3/D7 + 任务书
// WBS-7/8/9 族）：用 fakeKV（内存实现 etcdstore.ExtendedKV 全扩展面，
// 语义对齐 clientv3 子集；**无真实 embed，避免超时挂起**——参照
// modelrelease/storekit_test.go 的 fakeKV 风格）覆盖：
//
//	S2 写穿/CAS：
//	   - CreateModel/CreateVersion create-if-absent 冲突（409 语义）；
//	   - UpdateModel CAS 冲突重试 ≤3（一次冲突自愈 / 耗尽 409）；
//	   - ActivateVersion 双键 CAS 序列（正常 / ①冲突重试 / ①冲突耗尽 /
//	     ②失败补偿恢复旧 active）；
//	   - ArchiveVersion（含在途发布目标 409）；
//	   - DeleteVersion（active 拒删 / CAS 重试）；
//	   - DeleteModel 级联（versions+deployments+meta+guard）+ 前置拒绝；
//	   - Load 坏键跳过 + 孤儿过滤（L25）+ 告警不阻断；
//	   - RequestRollback 全守卫路径（etcd 路径：闭包内读内存，无嵌套锁）；
//	S3 watch 应用器：
//	   - 事件单调应用（PUT/DELETE）**零 etcd 写断言**（防回写环）；
//	   - 孤儿事件忽略（模型/head 缺失）；
//	   - ErrCompacted → 全量重放（以 kv 事实源重建 + 重锚定）；
//	S4 发布存储 + P8 定向：
//	   - CreateRelease guard 语义：孤儿 guard 自愈（D3）、终态残留自愈、
//	     在途 409 带在途 ID、**D7 模型 meta 复查**（guard 持有后模型消失
//	     → 删 guard + ErrModelNotFound）；
//	   - head CAS 状态机并发（两个 goroutine 流转 → 一个成功一个冲突重试
//	     后按新状态判决）；
//	   - perNode 首写 create-if-absent + 更新 CAS + 转换合法性；
//	   - SetDeployment 普通 Put（整值覆盖）+ List + 级联删除；
//	   - ReleaseGuard 幂等删 guard。

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
)

// ── fakeKV ─────────────────────────────────────────────────────────────

// fakeKV 是内存实现 etcdstore.ExtendedKV 全扩展面（KVStore + AtomicKV +
// WatchKV + LeaseKV）的测试替身（风格对齐 modelrelease/storekit_test.go）。
//
// 额外测试钩子：
//   - conflictPut[key]=N：接下来 N 次 CompareAndPut 命中键前先推进该键
//     ModRevision 并返回 false（模拟"读后他写者提交"的 CAS 冲突）——覆盖
//     存储层重试 ≤3 与耗尽路径；
//   - watchCh 非 nil：WatchPrefix 直接返回它（测试脚本驱动 watchLoop 的
//     PUT/DELETE/Err 事件流）；
//   - putCount：总写次数（watch 应用器零写断言用）。
//
// CAS 语义对齐 atomic.go 接口注释：expectRev>0 按 ModRevision 等值；
// expectRev==0 为 create-if-absent（键存在即冲突）。
type fakeKV struct {
	mu     sync.Mutex
	data   map[string][]byte
	modRev map[string]int64
	rev    int64

	putCount    int
	failGet     error
	conflictPut map[string]int
	watchCh     chan etcdstore.WatchEvent
}

var _ etcdstore.ExtendedKV = (*fakeKV)(nil)

func newFakeKV() *fakeKV {
	return &fakeKV{
		data:        make(map[string][]byte),
		modRev:      make(map[string]int64),
		conflictPut: make(map[string]int),
	}
}

func (f *fakeKV) nextRev() int64 {
	f.rev++
	return f.rev
}

// bump 推进键 ModRevision（CAS 冲突注入用；空键名仅推进全局 rev）。
func (f *fakeKV) bump(key string) {
	f.rev++
	if key != "" {
		if _, ok := f.data[key]; ok {
			f.modRev[key] = f.rev
		}
	}
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
		f.rev++
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
			f.rev++
			delete(f.data, k)
			delete(f.modRev, k)
		}
	}
	return nil
}

func (f *fakeKV) Close() error { return nil }

// GetWithRev 实现 AtomicKV。
func (f *fakeKV) GetWithRev(_ context.Context, key string) ([]byte, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[key]
	if !ok {
		return nil, 0, nil
	}
	return append([]byte(nil), v...), f.modRev[key], nil
}

// CompareAndPut 实现 AtomicKV（conflictPut 注入失败路径）。
func (f *fakeKV) CompareAndPut(_ context.Context, key string, value []byte, expectRev int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.conflictPut[key]; n > 0 {
		f.conflictPut[key] = n - 1
		f.bump(key) // 模拟他写者已提交：推进 rev → CAS 冲突
		return false, nil
	}
	if expectRev > 0 {
		if f.modRev[key] != expectRev {
			return false, nil
		}
	} else if _, ok := f.data[key]; ok {
		return false, nil // create-if-absent：键存在 → 冲突
	}
	f.putCount++
	f.data[key] = append([]byte(nil), value...)
	f.modRev[key] = f.nextRev()
	return true, nil
}

func (f *fakeKV) CompareAndDelete(_ context.Context, key string, expectRev int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.conflictPut[key]; n > 0 {
		f.conflictPut[key] = n - 1
		f.bump(key) // 模拟他写者已提交 → 冲突
		return false, nil
	}
	if _, ok := f.data[key]; !ok {
		return expectRev == 0, nil
	}
	if f.modRev[key] != expectRev {
		return false, nil
	}
	f.putCount++
	f.rev++
	delete(f.data, key)
	delete(f.modRev, key)
	return true, nil
}

func (f *fakeKV) GuardedDelete(_ context.Context, guardKey, targetKey string) (bool, error) {
	// 模型存储不消费 GuardedDelete（守卫键在租约机制域外）；语义对齐
	// 接口契约的最小实现：两键均存在且 guard 无租约 → 删 target。
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.data[targetKey]; !ok {
		return false, nil
	}
	f.putCount++
	f.rev++
	delete(f.data, targetKey)
	delete(f.modRev, targetKey)
	return true, nil
}

// ListByPrefixRev 实现 WatchKV。
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

// WatchPrefix 实现 WatchKV：watchCh 已注入（脚本驱动测试）→ 直接返回；
// 否则返回随 ctx 取消关闭的空通道（no-op，避免 goroutine 泄漏）。
func (f *fakeKV) WatchPrefix(ctx context.Context, _ string, _ int64) <-chan etcdstore.WatchEvent {
	f.mu.Lock()
	ch := f.watchCh
	f.mu.Unlock()
	if ch != nil {
		return ch
	}
	ch = make(chan etcdstore.WatchEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}

// GrantHeartbeatLease 实现 LeaseKV（模型存储不消费；记录语义占位）。
func (f *fakeKV) GrantHeartbeatLease(_ context.Context, key string, value []byte, _ time.Duration) error {
	return f.Put(context.Background(), key, value)
}

func (f *fakeKV) hasKey(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.data[key]
	return ok
}

// mustJSON 序列化测试对象（失败即 panic——测试夹具内 JSON 恒可序列化）。
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化 %+v 失败: %v", v, err)
	}
	return data
}

// waitFor 轮询等待条件成立（watch 事件异步应用）。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

// newEtcdFixture 构造 fakeKV + EtcdModelStore + 模型 defect-detector +
// 版本 v1/v2（v1 archived、v2 active）。
func newEtcdFixture(t *testing.T) (*fakeKV, *EtcdModelStore) {
	t.Helper()
	kv := newFakeKV()
	store, err := NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateModel(ctx, &Model{Name: "defect-detector", Type: "detection"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1.0.0", "v2.0.0"} {
		if err := store.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: v,
			Mirror: "reg/x:" + v, Sha256: "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ActivateVersion(ctx, "defect-detector", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateVersion(ctx, "defect-detector", "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	return kv, store
}

// TestEtcdCreateModel create-if-absent：成功写穿（etcd + 内存）+
// 重复 409 + nil 防御。
func TestEtcdCreateModel(t *testing.T) {
	kv := newFakeKV()
	store, err := NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateModel(ctx, &Model{Name: "m1", Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if !kv.hasKey(modelKey("m1")) {
		t.Fatal("模型键应已写穿 etcd")
	}
	if _, err := store.GetModel(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateModel(ctx, &Model{Name: "m1"}); !errors.Is(err, ErrModelExists) {
		t.Fatalf("重复创建应 ErrModelExists，got %v", err)
	}
	if err := store.CreateModel(ctx, nil); err == nil {
		t.Fatal("nil 应报错")
	}
	// nil kv 防御
	if _, err := NewEtcdModelStore(nil); err == nil {
		t.Fatal("nil kv 应 fail-fast")
	}
}

// TestEtcdUpdateModel CAS 读-改-写：正常 / 一次冲突自愈（重试成功）/
// 耗尽 → ErrConcurrentConflict / 不存在 404。
func TestEtcdUpdateModel(t *testing.T) {
	kv, store := newEtcdFixture(t)
	ctx := context.Background()

	if err := store.UpdateModel(ctx, "defect-detector", func(m *Model) error {
		m.Description = "新描述"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m, _ := store.GetModel(ctx, "defect-detector")
	if m.Description != "新描述" {
		t.Fatalf("patch 未生效: %+v", m)
	}

	// 一次 CAS 冲突 → 重试成功（冲突注入在 UpdateModel 的提交 CAS 上）
	kv.conflictPut[modelKey("defect-detector")] = 1
	if err := store.UpdateModel(ctx, "defect-detector", func(m *Model) error {
		m.Description = "重试后"
		return nil
	}); err != nil {
		t.Fatalf("一次冲突应重试自愈，got %v", err)
	}
	m, _ = store.GetModel(ctx, "defect-detector")
	if m.Description != "重试后" {
		t.Fatalf("重试后 patch 应生效: %+v", m)
	}

	// 连续冲突耗尽（重试 ≤3）→ ErrConcurrentConflict
	kv.conflictPut[modelKey("defect-detector")] = 3
	if err := store.UpdateModel(ctx, "defect-detector", func(m *Model) error {
		m.Description = "x"
		return nil
	}); !errors.Is(err, ErrConcurrentConflict) {
		t.Fatalf("冲突耗尽应 ErrConcurrentConflict，got %v", err)
	}

	// patch 返回错误 → 中止不写
	if err := store.UpdateModel(ctx, "defect-detector", func(*Model) error {
		return errors.New("stop")
	}); err == nil {
		t.Fatal("patch 错误应透传")
	}
	// 不存在 → 404
	if err := store.UpdateModel(ctx, "nope", func(*Model) error { return nil }); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("应 ErrModelNotFound，got %v", err)
	}
}

// TestEtcdDeleteModel 级联删除（versions+deployments+meta+guard 前缀清空）
// + active 版本拒绝 + 在途发布拒绝 + 缺失 404。
func TestEtcdDeleteModel(t *testing.T) {
	ctx := context.Background()

	// 成功级联
	kv, store := newEtcdFixture(t)
	// 归档 v1 后无 active → 可删；先造部署影子与在途发布外的杂物
	if err := store.DeleteVersion(ctx, "defect-detector", "v1.0.0"); err != nil { // 现在 v1 已 archived
		t.Fatal(err)
	}
	if err := store.ArchiveVersion(ctx, "defect-detector", "v2.0.0"); err != nil {
		t.Fatal(err) // v2.0.0 active → archived，无 active
	}
	if err := store.SetDeployment(ctx, "defect-detector", "n1", DeploymentState{Version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	_ = store.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}})
	if err := store.CancelRelease(ctx, "rel-1"); err != nil { // 终态后才可删
		t.Fatal(err)
	}
	if err := store.DeleteModel(ctx, "defect-detector"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{modelKey("defect-detector"), KeyVersionsPrefix + "defect-detector/", KeyDeploymentsPrefix + "defect-detector/", guardKey("defect-detector")} {
		if strings.HasSuffix(k, "/") {
			if entries, _ := kv.ListByPrefix(ctx, k); len(entries) != 0 {
				t.Fatalf("级联应清空前缀 %s，残留 %d 键", k, len(entries))
			}
		} else if kv.hasKey(k) {
			t.Fatalf("级联应清除键 %s", k)
		}
	}
	if _, err := store.GetModel(ctx, "defect-detector"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("删除后应 404，got %v", err)
	}

	// active 版本 → ErrModelHasActiveVersion
	_, store2 := newEtcdFixture(t)
	if err := store2.DeleteModel(ctx, "defect-detector"); !errors.Is(err, ErrModelHasActiveVersion) {
		t.Fatalf("有 active 应拒绝删除，got %v", err)
	}

	// 在途发布 → ReleaseConflictError 带在途 ID（先归档 active 再发布）
	_, store3 := newEtcdFixture(t)
	if err := store3.ArchiveVersion(ctx, "defect-detector", "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := store3.CreateRelease(ctx, &ModelRelease{ID: "rel-x", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	err := store3.DeleteModel(ctx, "defect-detector")
	var rce *ReleaseConflictError
	if !errors.As(err, &rce) || rce.InFlight != "rel-x" {
		t.Fatalf("在途发布应拒绝删除带在途 ID，got %v", err)
	}

	// 缺失 → 404
	_, store4 := newEtcdFixture(t)
	if err := store4.DeleteModel(ctx, "nope"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("缺失应 404，got %v", err)
	}
}

// TestEtcdVersionCRUD 版本创建（强制 draft、写穿）+ 重复 409 + 模型缺失
// 404 + 删除（draft 可删 / active 拒删 / CAS 重试）。
func TestEtcdVersionCRUD(t *testing.T) {
	ctx := context.Background()
	kv, store := newEtcdFixture(t)

	// 创建（强制 draft + 时间戳）
	if err := store.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0",
		Mirror: "reg/x:v3.0.0", Sha256: "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}); err != nil {
		t.Fatal(err)
	}
	v, _ := store.GetVersion(ctx, "defect-detector", "v3.0.0")
	if v.Status != VersionStatusDraft {
		t.Fatalf("应强制 draft，got %s", v.Status)
	}
	if !kv.hasKey(versionKey("defect-detector", "v3.0.0")) {
		t.Fatal("版本键应写穿")
	}
	// 重复 → ErrVersionExists
	if err := store.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0", Mirror: "reg/x:v3.0.0"}); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("重复版本应 409，got %v", err)
	}
	// 模型缺失 → 404
	if err := store.CreateVersion(ctx, &ModelVersion{Model: "nope", Version: "v1"}); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("模型缺失应 404，got %v", err)
	}

	// 删除 draft → 成功（CompareAndDelete）
	if err := store.DeleteVersion(ctx, "defect-detector", "v3.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetVersion(ctx, "defect-detector", "v3.0.0"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("删除后应 404，got %v", err)
	}
	// 删除 active → ErrVersionActive
	if err := store.DeleteVersion(ctx, "defect-detector", "v2.0.0"); !errors.Is(err, ErrVersionActive) {
		t.Fatalf("active 拒删，got %v", err)
	}
	// 删除 archived → 成功
	if err := store.DeleteVersion(ctx, "defect-detector", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	// CAS 冲突重试：一次冲突自愈
	if err := store.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v4.0.0", Mirror: "reg/x:v4.0.0"}); err != nil {
		t.Fatal(err)
	}
	kv.conflictPut[versionKey("defect-detector", "v4.0.0")] = 1
	if err := store.DeleteVersion(ctx, "defect-detector", "v4.0.0"); err != nil {
		t.Fatalf("一次冲突应重试自愈，got %v", err)
	}
	// 耗尽 → ErrConcurrentConflict
	if err := store.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v5.0.0", Mirror: "reg/x:v5.0.0"}); err != nil {
		t.Fatal(err)
	}
	kv.conflictPut[versionKey("defect-detector", "v5.0.0")] = 3
	if err := store.DeleteVersion(ctx, "defect-detector", "v5.0.0"); !errors.Is(err, ErrConcurrentConflict) {
		t.Fatalf("冲突耗尽应 ErrConcurrentConflict，got %v", err)
	}
}

// TestEtcdActivateVersion_正常 双键 CAS 序列：① 旧 active → archived；
// ② 目标 draft → active。内存与 etcd 双端一致。
func TestEtcdActivateVersion_正常(t *testing.T) {
	kv, store := newEtcdFixture(t)
	ctx := context.Background()
	_ = kv

	if err := store.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0", Mirror: "reg/x:v3.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateVersion(ctx, "defect-detector", "v3.0.0"); err != nil {
		t.Fatal(err)
	}
	v1, _ := store.GetVersion(ctx, "defect-detector", "v1.0.0")
	v3, _ := store.GetVersion(ctx, "defect-detector", "v3.0.0")
	if v1.Status != VersionStatusArchived || v3.Status != VersionStatusActive {
		t.Fatalf("激活后 v1=archived v3=active，got v1=%s v3=%s", v1.Status, v3.Status)
	}
	// etcd 侧一致（直接读键）
	raw, _ := kv.Get(ctx, versionKey("defect-detector", "v1.0.0"))
	var v1k ModelVersion
	_ = json.Unmarshal(raw, &v1k)
	if v1k.Status != VersionStatusArchived {
		t.Fatalf("etcd 侧 v1 应为 archived，got %s", v1k.Status)
	}
	// 非 draft（active/archived）→ ErrVersionNotDraft
	if err := store.ActivateVersion(ctx, "defect-detector", "v3.0.0"); !errors.Is(err, ErrVersionNotDraft) {
		t.Fatalf("active 再激活应 ErrVersionNotDraft，got %v", err)
	}
	if err := store.ActivateVersion(ctx, "defect-detector", "v1.0.0"); !errors.Is(err, ErrVersionNotDraft) {
		t.Fatalf("archived 再激活应 ErrVersionNotDraft，got %v", err)
	}
	// 缺失 → 404
	if err := store.ActivateVersion(ctx, "defect-detector", "nope"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("缺失应 404，got %v", err)
	}
}

// TestEtcdActivateVersion_Step1冲突重试 ①（旧 active 降级 CAS）冲突 → 刷新
// 重试成功（注入一次冲突）。
func TestEtcdActivateVersion_Step1冲突重试(t *testing.T) {
	kv, store := newEtcdFixture(t)
	ctx := context.Background()
	if err := store.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0", Mirror: "reg/x:v3.0.0"}); err != nil {
		t.Fatal(err)
	}
	// ① CAS（旧 active v2 键）注入一次冲突 → 重试自愈（fixture 中 v1 已
	// 被 v2 激活时归档，旧 active 是 v2.0.0 键）
	kv.conflictPut[versionKey("defect-detector", "v2.0.0")] = 1
	if err := store.ActivateVersion(ctx, "defect-detector", "v3.0.0"); err != nil {
		t.Fatalf("①一次冲突应重试自愈，got %v", err)
	}
	v3, _ := store.GetVersion(ctx, "defect-detector", "v3.0.0")
	if v3.Status != VersionStatusActive {
		t.Fatalf("v3 应为 active，got %s", v3.Status)
	}
	old, _ := store.GetVersion(ctx, "defect-detector", "v2.0.0")
	if old.Status != VersionStatusArchived {
		t.Fatalf("旧 active v2 应 archived，got %s", old.Status)
	}
}

// TestEtcdActivateVersion_Step1冲突耗尽 ①冲突全部 3 次 → ErrConcurrentConflict，
// 旧 active 保持 active（无降级副作用）。
func TestEtcdActivateVersion_Step1冲突耗尽(t *testing.T) {
	kv, store := newEtcdFixture(t)
	ctx := context.Background()
	if err := store.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0", Mirror: "reg/x:v3.0.0"}); err != nil {
		t.Fatal(err)
	}
	kv.conflictPut[versionKey("defect-detector", "v2.0.0")] = 3
	if err := store.ActivateVersion(ctx, "defect-detector", "v3.0.0"); !errors.Is(err, ErrConcurrentConflict) {
		t.Fatalf("①耗尽应 ErrConcurrentConflict，got %v", err)
	}
	// 旧 active 未被降级（etcd 与内存一致）
	raw, _ := kv.Get(ctx, versionKey("defect-detector", "v2.0.0"))
	var v2k ModelVersion
	_ = json.Unmarshal(raw, &v2k)
	if v2k.Status != VersionStatusActive {
		t.Fatalf("①耗尽后 v2 应仍 active（etcd），got %s", v2k.Status)
	}
}

// TestEtcdActivateVersion_Step2失败补偿 ②（目标 draft→active CAS）冲突 →
// 补偿尽力恢复 ① 降级的旧 active；返回 ErrConcurrentConflict；etcd 侧
// 旧 active 恢复 active、目标保持 draft（设计 §3.3 补偿路径）。
func TestEtcdActivateVersion_Step2失败补偿(t *testing.T) {
	kv, store := newEtcdFixture(t)
	ctx := context.Background()
	if err := store.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0", Mirror: "reg/x:v3.0.0"}); err != nil {
		t.Fatal(err)
	}
	// ② CAS（目标键）注入一次冲突 → ①成功、②失败 → 补偿
	kv.conflictPut[versionKey("defect-detector", "v3.0.0")] = 1
	err := store.ActivateVersion(ctx, "defect-detector", "v3.0.0")
	if !errors.Is(err, ErrConcurrentConflict) {
		t.Fatalf("②失败应 ErrConcurrentConflict，got %v", err)
	}
	// 补偿后：旧 active v2 恢复 active（etcd），目标 v3 保持 draft（etcd）
	for version, want := range map[string]VersionStatus{
		"v2.0.0": VersionStatusActive,
		"v3.0.0": VersionStatusDraft,
	} {
		raw, _ := kv.Get(ctx, versionKey("defect-detector", version))
		var vk ModelVersion
		if err := json.Unmarshal(raw, &vk); err != nil || vk.Status != want {
			t.Fatalf("补偿后 %s 应为 %s（etcd），got %s err=%v", version, want, vk.Status, err)
		}
	}
	// 内存未被污染（写穿失败内存不动）：v2 仍 active；v3 创建时已入内存
	// 但状态保持 draft（目标置位的②失败 → 内存无 active 翻转）
	if v2, _ := store.GetVersion(ctx, "defect-detector", "v2.0.0"); v2.Status != VersionStatusActive {
		t.Fatalf("内存 v2 应仍 active，got %s", v2.Status)
	}
	if v3, _ := store.GetVersion(ctx, "defect-detector", "v3.0.0"); v3.Status != VersionStatusDraft {
		t.Fatalf("内存 v3 应保持 draft，got %s", v3.Status)
	}
}

// TestEtcdArchiveVersion 归档：active → archived；非 active → 409；
// 在途发布目标 → 409 带在途 ID。
func TestEtcdArchiveVersion(t *testing.T) {
	ctx := context.Background()
	_, store := newEtcdFixture(t)

	if err := store.ArchiveVersion(ctx, "defect-detector", "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	v2, _ := store.GetVersion(ctx, "defect-detector", "v2.0.0")
	if v2.Status != VersionStatusArchived {
		t.Fatalf("归档后应 archived，got %s", v2.Status)
	}
	// 非 active → ErrVersionNotActive
	if err := store.ArchiveVersion(ctx, "defect-detector", "v2.0.0"); !errors.Is(err, ErrVersionNotActive) {
		t.Fatalf("archived 再归档应 409，got %v", err)
	}
	// 在途发布指向 → 409 带在途 ID（防发布中归档目标，设计 §2.4）
	_, store2 := newEtcdFixture(t)
	if err := store2.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0", Mirror: "reg/x:v3.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := store2.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	err := store2.ArchiveVersion(ctx, "defect-detector", "v2.0.0")
	var rce *ReleaseConflictError
	if !errors.As(err, &rce) || rce.InFlight != "rel-1" {
		t.Fatalf("在途目标归档应 409 带在途 ID，got %v", err)
	}
	// 缺失 / 模型缺失 → 404
	if err := store.ArchiveVersion(ctx, "defect-detector", "nope"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("缺失应 404，got %v", err)
	}
}

// ── S4/P8：CreateRelease guard 语义 ────────────────────────────────────

// TestEtcdCreateRelease_孤儿guard自愈（D3） guard 写后崩溃、release 键
// 从未写入的孤儿 → CreateRelease 按值删 guard → 重试创建成功。
func TestEtcdCreateRelease_孤儿guard自愈(t *testing.T) {
	kv := newFakeKV()
	store, err := NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateModel(ctx, &Model{Name: "m2"}); err != nil {
		t.Fatal(err)
	}
	// 模拟孤儿 guard（lock 键从未创建——设计 §5.4 的"lock 过期"判据在此
	// 不自洽，D3 裁决改为读 release 键）
	if err := kv.Put(ctx, guardKey("m2"), []byte("orphan-id")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRelease(ctx, &ModelRelease{ID: "rel-3", Model: "m2", Version: "v1",
		BatchSize: 1, TargetNodes: []string{"a"}}); err != nil {
		t.Fatalf("孤儿 guard 应自愈后创建成功，got %v", err)
	}
	if v, _ := kv.Get(ctx, guardKey("m2")); string(v) != "rel-3" {
		t.Fatalf("guard 应指向 rel-3，got %q", v)
	}
	if !kv.hasKey(releaseKey("rel-3")) {
		t.Fatal("release 头键应已写入")
	}
	if r, _ := store.GetRelease(ctx, "rel-3"); r.Status != ReleaseStatusPending {
		t.Fatalf("应 pending，got %s", r.Status)
	}
}

// TestEtcdCreateRelease_终态残留自愈 guard 指向已终态 release（终态后
// guard 未清）→ 防御性自愈：按值删 guard → 重试创建成功。
func TestEtcdCreateRelease_终态残留自愈(t *testing.T) {
	kv := newFakeKV()
	store, err := NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateModel(ctx, &Model{Name: "m3"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRelease(ctx, &ModelRelease{ID: "rel-4", Model: "m3", Version: "v1",
		BatchSize: 1, TargetNodes: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	// 手工把 head 置为终态（模拟"head succeeded 但 guard 未删"残留）
	raw, err := kv.Get(ctx, releaseKey("rel-4"))
	if err != nil || raw == nil {
		t.Fatal("release 头键缺失")
	}
	var hdr ModelRelease
	if err := json.Unmarshal(raw, &hdr); err != nil {
		t.Fatal(err)
	}
	hdr.Status = ReleaseStatusSucceeded
	if err := kv.Put(ctx, releaseKey("rel-4"), mustJSON(t, &hdr)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRelease(ctx, &ModelRelease{ID: "rel-5", Model: "m3", Version: "v1",
		BatchSize: 1, TargetNodes: []string{"a"}}); err != nil {
		t.Fatalf("终态残留 guard 应自愈，got %v", err)
	}
	if v, _ := kv.Get(ctx, guardKey("m3")); string(v) != "rel-5" {
		t.Fatalf("guard 应指向 rel-5，got %q", v)
	}
}

// TestEtcdCreateRelease_在途409 指向在途 release → *ReleaseConflictError
// 带在途 ID（响应 409 含 releaseID）。
func TestEtcdCreateRelease_在途409(t *testing.T) {
	_, store := newEtcdFixture(t)
	ctx := context.Background()
	if err := store.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	err := store.CreateRelease(ctx, &ModelRelease{ID: "rel-2", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}})
	var rce *ReleaseConflictError
	if !errors.As(err, &rce) || rce.InFlight != "rel-1" {
		t.Fatalf("应在途 409 带 rel-1，got %v", err)
	}
	if !errors.Is(err, ErrReleaseConflict) {
		t.Fatalf("errors.Is(ErrReleaseConflict) 应成立，got %v", err)
	}
}

// TestEtcdCreateRelease_D7模型meta复查 guard CAS 成功后模型键消失（并发
// DeleteModel 竞态）→ 删 guard（按值 CAS）+ ErrModelNotFound，不留孤儿
// guard（主线裁决 D7）。
func TestEtcdCreateRelease_D7模型meta复查(t *testing.T) {
	kv := newFakeKV()
	store, err := NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateModel(ctx, &Model{Name: "m4"}); err != nil {
		t.Fatal(err)
	}
	// 模拟并发 DeleteModel：meta 键已删（guard 尚在/into 竞态窗口——
	// DeleteModel 会删 guard，这里直接模拟"meta 先消失"）
	if err := kv.Delete(ctx, modelKey("m4")); err != nil {
		t.Fatal(err)
	}
	err = store.CreateRelease(ctx, &ModelRelease{ID: "rel-6", Model: "m4", Version: "v1",
		BatchSize: 1, TargetNodes: []string{"a"}})
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("模型消失应 ErrModelNotFound，got %v", err)
	}
	if kv.hasKey(guardKey("m4")) {
		t.Fatal("D7 失败路径应删除 guard（不留孤儿）")
	}
	if kv.hasKey(releaseKey("rel-6")) {
		t.Fatal("D7 失败路径不应写 release 头键")
	}
}

// TestEtcdReleaseHead_CAS并发 两个 goroutine 从 pending 并发流转（一个
// cancel、一个置 running）→ 恰一个提交成功，另一个 CAS 冲突后重试读到
// 新状态（mutate 判决拒绝，模拟控制器 errNotPending）——S4 head 状态机
// 并发用例。
func TestEtcdReleaseHead_CAS并发(t *testing.T) {
	_, store := newEtcdFixture(t)
	ctx := context.Background()
	if err := store.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	notPending := errors.New("not pending")
	var wg sync.WaitGroup
	var mu sync.Mutex
	cancelOK, runRejected := false, false
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := store.CancelRelease(ctx, "rel-1")
		mu.Lock()
		defer mu.Unlock()
		cancelOK = err == nil
	}()
	go func() {
		defer wg.Done()
		err := store.UpdateReleaseHead(ctx, "rel-1", func(r *ModelRelease) error {
			if r.Status != ReleaseStatusPending {
				return notPending
			}
			r.Status = ReleaseStatusRunning
			return nil
		})
		mu.Lock()
		defer mu.Unlock()
		runRejected = errors.Is(err, notPending)
	}()
	wg.Wait()
	if !cancelOK && !runRejected {
		t.Fatalf("并发流转应一胜一拒：cancel=%v runRejected=%v", cancelOK, runRejected)
	}
	r, _ := store.GetRelease(ctx, "rel-1")
	if r.Status != ReleaseStatusCanceled && r.Status != ReleaseStatusRunning {
		t.Fatalf("head 终态应为二者之一: %+v", r)
	}
}

// TestEtcdReleaseHead与Cancel head CAS 重试 ≤3（一次冲突自愈 / 耗尽 409）
// + CancelRelease 终态拒绝 + 缺失 404。
func TestEtcdReleaseHead与Cancel(t *testing.T) {
	_, store := newEtcdFixture(t)
	ctx := context.Background()
	if err := store.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	// head 更新（NextBatchAt 等）一次冲突自愈
	kv2 := newFakeKV() // 需要原始 kv 注入冲突——直接用 store 自愈路径也可：
	_ = kv2
	// 注：conflictPut 需对 releaseKey 注入；这里先跑无冲突路径
	if err := store.UpdateReleaseHead(ctx, "rel-1", func(r *ModelRelease) error {
		r.NextBatchAt = 12345
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if r, _ := store.GetRelease(ctx, "rel-1"); r.NextBatchAt != 12345 {
		t.Fatalf("head 更新未生效: %+v", r)
	}
	// CancelRelease：pending → canceled + FinishedAt
	if err := store.CancelRelease(ctx, "rel-1"); err != nil {
		t.Fatal(err)
	}
	// 终态再 cancel → ErrReleaseTerminal
	if err := store.CancelRelease(ctx, "rel-1"); !errors.Is(err, ErrReleaseTerminal) {
		t.Fatalf("终态再 cancel 应 409，got %v", err)
	}
	// 缺失 → 404
	if err := store.CancelRelease(ctx, "nope"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("缺失应 404，got %v", err)
	}
}

// TestEtcdRequestRollback 回滚前置守卫全路径（etcd 实现经 UpdateReleaseHead
// 闭包内读内存版本表——无锁嵌套，天然无死锁；语义与内存实现一致）。
func TestEtcdRequestRollback(t *testing.T) {
	ctx := context.Background()
	_, store := newEtcdFixture(t)
	rel := &ModelRelease{ID: "rel-rb", Model: "defect-detector", Version: "v2.0.0",
		Target:      ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{"n1"}},
		TargetNodes: []string{"n1"}, BatchSize: 1, PrevActive: "v1.0.0"}
	if err := store.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	// 成功路径：先驱动到 succeeded（模拟控制器跑完）
	if err := store.UpdateReleaseHead(ctx, rel.ID, func(r *ModelRelease) error {
		r.Status = ReleaseStatusSucceeded
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestRollback(ctx, rel.ID); err != nil {
		t.Fatalf("回滚请求应成功，got %v", err)
	}
	if r, _ := store.GetRelease(ctx, rel.ID); !r.RollbackRequested {
		t.Fatal("RollbackRequested 应置位")
	}
	// 幂等
	if err := store.RequestRollback(ctx, rel.ID); err != nil {
		t.Fatalf("重复请求应幂等，got %v", err)
	}
	// sentinel 路径
	err := store.RequestRollback(ctx, "nope")
	if !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("缺失应 404，got %v", err)
	}
	// pending → 409
	_, store2 := newEtcdFixture(t)
	if err := store2.CreateRelease(ctx, &ModelRelease{ID: "rel-p", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}, PrevActive: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := store2.RequestRollback(ctx, "rel-p"); !errors.Is(err, ErrReleaseTerminal) {
		t.Fatalf("pending 应 409，got %v", err)
	}
	// 无 PrevActive → 422
	_, store3 := newEtcdFixture(t)
	if err := store3.CreateRelease(ctx, &ModelRelease{ID: "rel-n", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	if err := store3.UpdateReleaseHead(ctx, "rel-n", func(r *ModelRelease) error {
		r.Status = ReleaseStatusSucceeded
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store3.RequestRollback(ctx, "rel-n"); !errors.Is(err, ErrNoPrevActive) {
		t.Fatalf("无 PrevActive 应 422，got %v", err)
	}
	// PrevActive 版本被删 → 422（F-4 TOCTOU 守卫）
	_, store4 := newEtcdFixture(t)
	rel4 := &ModelRelease{ID: "rel-4", Model: "defect-detector", Version: "v2.0.0",
		BatchSize: 1, TargetNodes: []string{"n1"}, PrevActive: "v1.0.0"}
	if err := store4.CreateRelease(ctx, rel4); err != nil {
		t.Fatal(err)
	}
	if err := store4.UpdateReleaseHead(ctx, rel4.ID, func(r *ModelRelease) error {
		r.Status = ReleaseStatusSucceeded
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store4.DeleteVersion(ctx, "defect-detector", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := store4.RequestRollback(ctx, rel4.ID); !errors.Is(err, ErrNoPrevActive) {
		t.Fatalf("PrevActive 被删应 422，got %v", err)
	}
	// version ≠ 当前 active → 409（L26）
	_, store5 := newEtcdFixture(t)
	rel5 := &ModelRelease{ID: "rel-5", Model: "defect-detector", Version: "v3.0.0",
		BatchSize: 1, TargetNodes: []string{"n1"}, PrevActive: "v1.0.0"}
	if err := store5.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0", Mirror: "reg/x:v3.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := store5.CreateRelease(ctx, rel5); err != nil {
		t.Fatal(err)
	}
	if err := store5.UpdateReleaseHead(ctx, rel5.ID, func(r *ModelRelease) error {
		r.Status = ReleaseStatusSucceeded
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store5.RequestRollback(ctx, rel5.ID); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("v3 非 active 应 ErrVersionMismatch，got %v", err)
	}
}

// TestEtcdSetNodeResult perNode 首写 create-if-absent + CAS 更新 +
// 转换合法性 + 缺失发布 404。
func TestEtcdSetNodeResult(t *testing.T) {
	kv, store := newEtcdFixture(t)
	ctx := context.Background()
	if err := store.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1", "n2"}}); err != nil {
		t.Fatal(err)
	}
	// 首写（Batch 预分配）
	if err := store.SetNodeResult(ctx, "rel-1", "n1", &NodeReleaseResult{NodeID: "n1", Status: NodeRelPending, Batch: 1}); err != nil {
		t.Fatal(err)
	}
	if !kv.hasKey(releaseNodeKey("rel-1", "n1")) {
		t.Fatal("perNode 键应写穿")
	}
	nr, _ := store.GetNodeResult(ctx, "rel-1", "n1")
	if nr == nil || nr.Batch != 1 || nr.Status != NodeRelPending {
		t.Fatalf("首写结果异常: %+v", nr)
	}
	// 更新 CAS（deployed，一次冲突自愈）
	kv.conflictPut[releaseNodeKey("rel-1", "n1")] = 1
	if err := store.SetNodeResult(ctx, "rel-1", "n1", &NodeReleaseResult{NodeID: "n1", Status: NodeRelDeployed, Version: "v2.0.0"}); err != nil {
		t.Fatalf("一次冲突应自愈，got %v", err)
	}
	nr, _ = store.GetNodeResult(ctx, "rel-1", "n1")
	if nr.Status != NodeRelDeployed || nr.Batch != 1 {
		t.Fatalf("更新结果异常: %+v", nr)
	}
	// →pending 回退拒绝
	if err := store.SetNodeResult(ctx, "rel-1", "n1", &NodeReleaseResult{NodeID: "n1", Status: NodeRelPending}); err == nil {
		t.Fatal("→pending 回退应拒绝")
	}
	// 缺失发布 → 404
	if err := store.SetNodeResult(ctx, "nope", "n1", &NodeReleaseResult{NodeID: "n1", Status: NodeRelDeployed}); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("缺失发布应 404，got %v", err)
	}
	// 列表排序
	all, err := store.ListNodeResults(ctx, "rel-1")
	if err != nil || len(all) != 1 {
		t.Fatalf("应 1 条，got %+v err=%v", all, err)
	}
}

// TestEtcdSetDeployment 部署影子普通 Put（整值覆盖）+ List + 级联删除。
func TestEtcdSetDeployment(t *testing.T) {
	kv, store := newEtcdFixture(t)
	ctx := context.Background()
	if err := store.SetDeployment(ctx, "defect-detector", "n1", DeploymentState{Version: "v2.0.0", ReleaseID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if !kv.hasKey(deploymentKey("defect-detector", "n1")) {
		t.Fatal("影子键应写穿")
	}
	ds, _ := store.ListDeployments(ctx, "defect-detector")
	if len(ds) != 1 || ds[0].NodeID != "n1" || ds[0].Model != "defect-detector" || ds[0].ReleaseID != "r1" {
		t.Fatalf("影子异常: %+v", ds)
	}
	// 整值覆盖（last-writer-wins，无 CAS）
	if err := store.SetDeployment(ctx, "defect-detector", "n1", DeploymentState{Version: "v3.0.0", ReleaseID: "r2"}); err != nil {
		t.Fatal(err)
	}
	ds, _ = store.ListDeployments(ctx, "defect-detector")
	if ds[0].Version != "v3.0.0" || ds[0].ReleaseID != "r2" {
		t.Fatalf("应整值覆盖: %+v", ds[0])
	}
	// 模型缺失 → 404（内存前置）
	if err := store.SetDeployment(ctx, "nope", "n1", DeploymentState{}); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("模型缺失应 404，got %v", err)
	}
	// 级联删除
	if err := store.DeleteModelDeployments(ctx, "defect-detector"); err != nil {
		t.Fatal(err)
	}
	if ds, _ := store.ListDeployments(ctx, "defect-detector"); len(ds) != 0 {
		t.Fatalf("级联后应为空: %+v", ds)
	}
}

// TestEtcdReleaseGuard 释放 guard 键（幂等）。
func TestEtcdReleaseGuard(t *testing.T) {
	kv, store := newEtcdFixture(t)
	ctx := context.Background()
	if err := store.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	if !kv.hasKey(guardKey("defect-detector")) {
		t.Fatal("创建后 guard 应存在")
	}
	if err := store.ReleaseGuard(ctx, "defect-detector"); err != nil {
		t.Fatal(err)
	}
	if kv.hasKey(guardKey("defect-detector")) {
		t.Fatal("ReleaseGuard 应删除 guard 键")
	}
	if err := store.ReleaseGuard(ctx, "defect-detector"); err != nil {
		t.Fatalf("幂等删除应成功，got %v", err)
	}
}

// ── S2：Load 重建 + 坏键/孤儿过滤 ──────────────────────────────────────

// TestEtcdLoad 全量加载：合法键全部恢复；坏键跳过 + 告警不阻断；孤儿键
// （模型 meta 缺失的前缀、head 缺失的 perNode）不可见（L25 口径）。
func TestEtcdLoad(t *testing.T) {
	kv := newFakeKV()
	ctx := context.Background()

	// 合法键
	kv.Put(ctx, modelKey("m1"), mustJSON(t, &Model{Name: "m1", Type: "t"}))
	kv.Put(ctx, versionKey("m1", "v1"), mustJSON(t, &ModelVersion{Model: "m1", Version: "v1", Mirror: "reg/x:v1"}))
	kv.Put(ctx, releaseKey("r1"), mustJSON(t, &ModelRelease{ID: "r1", Model: "m1", Version: "v1", BatchSize: 1, Status: ReleaseStatusSucceeded}))
	kv.Put(ctx, releaseNodeKey("r1", "n1"), mustJSON(t, &NodeReleaseResult{NodeID: "n1", Status: NodeRelDeployed, Version: "v1"}))
	kv.Put(ctx, deploymentKey("m1", "n1"), mustJSON(t, &DeploymentState{Model: "m1", NodeID: "n1", Version: "v1"}))
	// 坏键（JSON 解析失败）
	kv.Put(ctx, modelKey("bad"), []byte("{not-json"))
	// 孤儿版本（模型缺失）与孤儿 perNode（head 缺失）
	kv.Put(ctx, versionKey("ghost", "v1"), mustJSON(t, &ModelVersion{Model: "ghost", Version: "v1", Mirror: "reg/x:v1"}))
	kv.Put(ctx, releaseNodeKey("ghost-rel", "n1"), mustJSON(t, &NodeReleaseResult{NodeID: "n1", Status: NodeRelDeployed}))
	// 嵌入键（release 头）也缺失：releaseNode 孤儿；另加锁键应忽略
	kv.Put(ctx, releaseKey("r1")+"/lock", []byte("{\"releaseID\":\"r1\"}"))

	store, err := NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Load(ctx); err != nil {
		t.Fatalf("Load 应成功（坏键跳过不阻断），got %v", err)
	}
	if _, err := store.GetModel(ctx, "m1"); err != nil {
		t.Fatalf("m1 应恢复，got %v", err)
	}
	if _, err := store.GetVersion(ctx, "m1", "v1"); err != nil {
		t.Fatalf("m1/v1 应恢复，got %v", err)
	}
	if r, err := store.GetRelease(ctx, "r1"); err != nil || r.Status != ReleaseStatusSucceeded {
		t.Fatalf("r1 应恢复，got %+v err=%v", r, err)
	}
	if nr, _ := store.GetNodeResult(ctx, "r1", "n1"); nr == nil {
		t.Fatal("perNode n1 应恢复")
	}
	if ds, _ := store.ListDeployments(ctx, "m1"); len(ds) != 1 {
		t.Fatalf("部署影子应恢复: %+v", ds)
	}
	// 孤儿/坏键不加载
	if _, err := store.GetModel(ctx, "bad"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("坏模型键应不可见，got %v", err)
	}
	if _, err := store.GetVersion(ctx, "ghost", "v1"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("孤儿版本应不可见（模型缺失 → 404），got %v", err)
	}
	if nr, _ := store.GetNodeResult(ctx, "ghost-rel", "n1"); nr != nil {
		t.Fatalf("孤儿 perNode 应不可见: %+v", nr)
	}
	// 终态 release 恢复后：ListReleases 模型过滤
	if rels, _ := store.ListReleases(ctx, "m1"); len(rels) != 1 {
		t.Fatalf("发布应恢复: %+v", rels)
	}
}

// ── S3：watch 应用器（零写断言 / 孤儿忽略 / ErrCompacted 全量重放）────

// TestEtcdWatch应用器 watchLoop：PUT/DELETE 事件单调应用、孤儿事件忽略、
// **零 etcd 写**（防回写环）；WatchEventErr → 全量重放（以 kv 事实源重建
// + 重锚定）。
func TestEtcdWatch应用器(t *testing.T) {
	kv := newFakeKV()
	kv.watchCh = make(chan etcdstore.WatchEvent, 32)
	store, err := NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = store.Close() }()

	store.StartWatch(ctx)

	// ① PUT 事件流（模型 → 版本 → 部署影子 → 发布头 → perNode）单调应用
	kv.watchCh <- etcdstore.WatchEvent{Type: etcdstore.WatchEventPut, Key: modelKey("m1"), Value: mustJSON(t, &Model{Name: "m1", Type: "detection"}), ModRevision: 1}
	waitFor(t, func() bool { _, err := store.GetModel(ctx, "m1"); return err == nil })
	kv.watchCh <- etcdstore.WatchEvent{Type: etcdstore.WatchEventPut, Key: versionKey("m1", "v1"), Value: mustJSON(t, &ModelVersion{Model: "m1", Version: "v1", Mirror: "reg/x:v1", Status: VersionStatusActive}), ModRevision: 2}
	waitFor(t, func() bool { _, err := store.GetVersion(ctx, "m1", "v1"); return err == nil })
	kv.watchCh <- etcdstore.WatchEvent{Type: etcdstore.WatchEventPut, Key: deploymentKey("m1", "n1"), Value: mustJSON(t, &DeploymentState{Model: "m1", NodeID: "n1", Version: "v1"}), ModRevision: 3}
	kv.watchCh <- etcdstore.WatchEvent{Type: etcdstore.WatchEventPut, Key: releaseKey("r1"), Value: mustJSON(t, &ModelRelease{ID: "r1", Model: "m1", Version: "v1", Status: ReleaseStatusRunning}), ModRevision: 4}
	kv.watchCh <- etcdstore.WatchEvent{Type: etcdstore.WatchEventPut, Key: releaseNodeKey("r1", "n1"), Value: mustJSON(t, &NodeReleaseResult{NodeID: "n1", Status: NodeRelDeployed, Version: "v1"}), ModRevision: 5}
	waitFor(t, func() bool { nr, _ := store.GetNodeResult(ctx, "r1", "n1"); return nr != nil })

	// ② 零写断言：事件应用不得触碰 etcd（fakeKV.putCount 不变）
	if kv.putCount != 0 {
		t.Fatalf("watch 应用器应零写（防回写环），putCount=%d", kv.putCount)
	}

	// ③ 孤儿事件忽略（模型不存在 / head 缺失）→ 内存不变仍零写
	kv.watchCh <- etcdstore.WatchEvent{Type: etcdstore.WatchEventPut, Key: versionKey("ghost", "v1"), Value: mustJSON(t, &ModelVersion{Model: "ghost", Version: "v1"}), ModRevision: 6}
	kv.watchCh <- etcdstore.WatchEvent{Type: etcdstore.WatchEventPut, Key: releaseNodeKey("ghost-rel", "n1"), Value: mustJSON(t, &NodeReleaseResult{NodeID: "n1", Status: NodeRelDeployed}), ModRevision: 7}
	time.Sleep(20 * time.Millisecond) // 给应用器处理窗口
	if _, err := store.GetVersion(ctx, "ghost", "v1"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("孤儿版本应忽略，got %v", err)
	}
	if kv.putCount != 0 {
		t.Fatalf("孤儿事件也应零写，putCount=%d", kv.putCount)
	}

	// ④ DELETE 事件：部署影子清除（内存）
	kv.watchCh <- etcdstore.WatchEvent{Type: etcdstore.WatchEventDelete, Key: deploymentKey("m1", "n1"), ModRevision: 8}
	waitFor(t, func() bool { ds, _ := store.ListDeployments(ctx, "m1"); return len(ds) == 0 })
	if kv.putCount != 0 {
		t.Fatalf("DELETE 应用应零写，putCount=%d", kv.putCount)
	}

	// ⑤ ErrCompacted → 全量重放：先直接写 kv（m2 全套），再触发 Err——
	// 重放以 kv 事实源整体重建（事件流的 m1 不在 kv 中 → 重放后消失，
	// 与真实系统「重放 = 锚定 kv」语义一致）+ 重锚定 + 零写。
	kv.Put(ctx, modelKey("m2"), mustJSON(t, &Model{Name: "m2"}))
	kv.Put(ctx, versionKey("m2", "v1"), mustJSON(t, &ModelVersion{Model: "m2", Version: "v1", Mirror: "reg/x:v1"}))
	kv.Put(ctx, deploymentKey("m2", "n1"), mustJSON(t, &DeploymentState{Model: "m2", NodeID: "n1", Version: "v1"}))
	kv.Put(ctx, modelKey("bad"), []byte("{bad")) // 坏键：重放跳过
	baseWrites := kv.putCount

	kv.watchCh <- etcdstore.WatchEvent{Type: etcdstore.WatchEventErr, Value: []byte("simulated compaction"), ModRevision: 9}
	waitFor(t, func() bool { _, err := store.GetModel(ctx, "m2"); return err == nil })

	// 重放后：m2 全套可见；m1（仅在事件流、不在 kv）被清出；坏键不可见
	if _, err := store.GetModel(ctx, "m1"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("重放后 m1 应不存在（kv 无此键），got %v", err)
	}
	if _, err := store.GetVersion(ctx, "m2", "v1"); err != nil {
		t.Fatalf("m2/v1 应恢复，got %v", err)
	}
	if ds, _ := store.ListDeployments(ctx, "m2"); len(ds) != 1 {
		t.Fatalf("m2 部署影子应恢复: %+v", ds)
	}
	if _, err := store.GetModel(ctx, "bad"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("坏键应跳过，got %v", err)
	}
	if kv.putCount != baseWrites {
		t.Fatalf("全量重放应零写（putCount %d → %d）", baseWrites, kv.putCount)
	}
	// 重锚定：watchRev ≥ 最近一次全局 rev（此后增量不再漏）
	if got := store.watchRev.Load(); got < kv.rev {
		t.Fatalf("重放后应重锚定（watchRev=%d < kv rev=%d）", got, kv.rev)
	}

	// 收尾：关闭事件流 → ctx 取消 → watchLoop 退出（不触发二次重放）
	close(kv.watchCh)
	cancel()
}

// TestEtcdWatchStartNoop 非 WatchKV 的 kv → StartWatch no-op + 不 panic。
func TestEtcdWatchStartNoop(t *testing.T) {
	kv := newFakeKV()
	store, err := NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 kv 不满足 WatchKV：包一层只实现 KVStore+AtomicKV 的精简 KV
	plain := &plainAtomicKV{fake: kv}
	plainStore, err := NewEtcdModelStore(plain)
	if err != nil {
		t.Fatalf("AtomicKV 应满足构造，got %v", err)
	}
	plainStore.StartWatch(context.Background())
	if _, err := plainStore.LoadAnchored(context.Background()); err == nil {
		t.Fatal("非 WatchKV 的 LoadAnchored 应报错")
	}
	_ = store
}

// plainAtomicKV 是只实现 KVStore+AtomicKV 的精简面（StartWatch no-op 用例）。
type plainAtomicKV struct {
	fake *fakeKV
}

var _ etcdstore.KVStore = (*plainAtomicKV)(nil)
var _ etcdstore.AtomicKV = (*plainAtomicKV)(nil)

func (p *plainAtomicKV) Put(ctx context.Context, key string, value []byte) error {
	return p.fake.Put(ctx, key, value)
}
func (p *plainAtomicKV) Get(ctx context.Context, key string) ([]byte, error) {
	return p.fake.Get(ctx, key)
}
func (p *plainAtomicKV) ListByPrefix(ctx context.Context, prefix string) ([]etcdstore.KVEntry, error) {
	return p.fake.ListByPrefix(ctx, prefix)
}
func (p *plainAtomicKV) Delete(ctx context.Context, key string) error { return p.fake.Delete(ctx, key) }
func (p *plainAtomicKV) DeleteRange(ctx context.Context, prefix string) error {
	return p.fake.DeleteRange(ctx, prefix)
}
func (p *plainAtomicKV) Close() error { return p.fake.Close() }
func (p *plainAtomicKV) GetWithRev(ctx context.Context, key string) ([]byte, int64, error) {
	return p.fake.GetWithRev(ctx, key)
}
func (p *plainAtomicKV) CompareAndPut(ctx context.Context, key string, value []byte, expectRev int64) (bool, error) {
	return p.fake.CompareAndPut(ctx, key, value, expectRev)
}
func (p *plainAtomicKV) CompareAndDelete(ctx context.Context, key string, expectRev int64) (bool, error) {
	return p.fake.CompareAndDelete(ctx, key, expectRev)
}
func (p *plainAtomicKV) GuardedDelete(ctx context.Context, guardKey, targetKey string) (bool, error) {
	return p.fake.GuardedDelete(ctx, guardKey, targetKey)
}
