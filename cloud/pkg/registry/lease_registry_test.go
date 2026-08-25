// v0.6.0「真多活」LeaseEtcdRegistry 核心契约测试（fake ExtendedKV 驱动）：
//   1) Register/UpdateHeartbeat → 台账 CAS 写 + 心跳续约入队（grant-per-heartbeat）
//   2) 判活 = hb 键存在性：hb PUT → Ready；hb DELETE（非服务）→ Offline
//   3) 本地覆盖规则（R2-4）：hb 键被删但本副本仍在服务 → 忽略 + 修复性重写
//   4) watch 应用器只读铁律（应用期间零扩展面写调用）
//   5) GuardedDelete：rev 不匹配（重注册）→ 放弃删除不级联；网络错误 → pending 重试
//   6) EtcdHealthyWithin / LoadAnchored 锚定加载
package registry

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
)

// ---- fakeExtKV：内存实现 ExtendedKV（带 revision 簿记 + 故障注入） ----

type fakeExtEntry struct {
	val []byte
	rev int64
}

type fakeExtKV struct {
	mu      sync.Mutex
	m       map[string]fakeExtEntry
	nextRev int64
	failPut error // 非 nil：Put/Grant 恒失败（模拟 etcd 故障）

	// watch 面：活跃通道集合（inject 广播）
	watchers map[chan etcdstore.WatchEvent]struct{}
}

func newFakeExtKV() *fakeExtKV {
	return &fakeExtKV{m: make(map[string]fakeExtEntry), watchers: make(map[chan etcdstore.WatchEvent]struct{})}
}

func (f *fakeExtKV) bump(key string, val []byte) {
	f.nextRev++
	f.m[key] = fakeExtEntry{val: val, rev: f.nextRev}
}

func (f *fakeExtKV) Put(_ context.Context, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil {
		return f.failPut
	}
	f.bump(key, value)
	return nil
}

func (f *fakeExtKV) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[key]
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), e.val...), nil
}

func (f *fakeExtKV) ListByPrefix(_ context.Context, prefix string) ([]etcdstore.KVEntry, error) {
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

func (f *fakeExtKV) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key)
	return nil
}

func (f *fakeExtKV) DeleteRange(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.m {
		if strings.HasPrefix(k, prefix) {
			delete(f.m, k)
		}
	}
	return nil
}

func (f *fakeExtKV) Close() error { return nil }

// ---- AtomicKV ----

func (f *fakeExtKV) GetWithRev(_ context.Context, key string) ([]byte, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[key]
	if !ok {
		return nil, 0, nil
	}
	return append([]byte(nil), e.val...), e.rev, nil
}

func (f *fakeExtKV) CompareAndPut(_ context.Context, key string, value []byte, expectRev int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil {
		return false, f.failPut
	}
	e, exists := f.m[key]
	switch {
	case expectRev > 0:
		if !exists || e.rev != expectRev {
			return false, nil // 冲突
		}
	case expectRev == 0:
		if exists {
			return false, nil // create-if-absent 撞已有键
		}
	default:
		return false, nil
	}
	f.bump(key, value)
	return true, nil
}

func (f *fakeExtKV) CompareAndDelete(_ context.Context, key string, expectRev int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil {
		return false, f.failPut
	}
	e, exists := f.m[key]
	if !exists {
		return true, nil // 本就缺失，视同完成（语义见 AtomicKV 注释）
	}
	if e.rev != expectRev {
		return false, nil // 守卫阻止
	}
	delete(f.m, key)
	return true, nil
}

func (f *fakeExtKV) GuardedDelete(_ context.Context, guardKey, targetKey string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil {
		return false, f.failPut
	}
	if _, ok := f.m[guardKey]; ok {
		return false, nil // guard 键存在（活租约近似）→ 拒绝删除
	}
	delete(f.m, targetKey)
	return true, nil
}

// ---- LeaseKV ----

func (f *fakeExtKV) GrantHeartbeatLease(_ context.Context, key string, value []byte, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil {
		return f.failPut
	}
	f.bump(key, value)
	return nil
}

// ---- WatchKV ----

func (f *fakeExtKV) ListByPrefixRev(_ context.Context, prefix string) ([]etcdstore.KVEntry, int64, error) {
	entries, err := f.ListByPrefix(context.Background(), prefix)
	if err != nil {
		return nil, 0, err
	}
	return entries, f.nextRev, nil
}

func (f *fakeExtKV) WatchPrefix(_ context.Context, _ string, _ int64) <-chan etcdstore.WatchEvent {
	ch := make(chan etcdstore.WatchEvent)
	f.mu.Lock()
	f.watchers[ch] = struct{}{}
	f.mu.Unlock()
	return ch
}

// closeWatch 移除并关闭指定 watch 通道（模拟断线）。
func (f *fakeExtKV) closeWatch(ch chan etcdstore.WatchEvent) {
	f.mu.Lock()
	if _, ok := f.watchers[ch]; ok {
		delete(f.watchers, ch)
		close(ch)
	}
	f.mu.Unlock()
}

// inject 向所有活跃 watch 广播事件（近实时投递：goroutine 内加锁发送）。
func (f *fakeExtKV) inject(ev etcdstore.WatchEvent) {
	f.mu.Lock()
	var targets []chan etcdstore.WatchEvent
	for ch := range f.watchers {
		targets = append(targets, ch)
	}
	f.mu.Unlock()
	for _, ch := range targets {
		ch <- ev
	}
}

// keys 返回当前全部键（测试断言用）。
func (f *fakeExtKV) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.m {
		out = append(out, k)
	}
	return out
}

// ---- 测试 ----

func newLeaseReg(f *fakeExtKV) (*LeaseEtcdRegistry, error) {
	return NewLeaseEtcdRegistry(f, LeaseRegOptions{
		OfflineTTL:   24 * time.Hour,
		LeaseTTL:     300 * time.Second,
		ScanInterval: 30 * time.Second,
		RenewWorkers: 1,
	})
}

func testInfo(id string) NodeInfo {
	return NodeInfo{NodeID: id, NodeName: id, Status: StatusUnknown, RegisteredAt: 1700000000000}
}

// 1) Register → 台账 CAS 写；UpdateHeartbeat → 心跳键出现（续约 worker 消费后）
func TestLeaseRegRegisterWritesNodeRecord(t *testing.T) {
	f := newFakeExtKV()
	r, err := newLeaseReg(f)
	if err != nil {
		t.Fatalf("NewLeaseEtcdRegistry: %v", err)
	}
	defer r.Close()
	if err := r.Register(testInfo("n1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := f.m[KeyNodesPrefix+"n1"]; !ok {
		t.Fatalf("台账键未写入 etcd: keys=%v", f.keys())
	}
}

// 2) 判活 = hb 键存在性：watch hb PUT → Ready；hb DELETE（非本副本服务）→ Offline
func TestLeaseRegHeartbeatDrivesStatus(t *testing.T) {
	f := newFakeExtKV()
	r, err := newLeaseReg(f)
	if err != nil {
		t.Fatalf("NewLeaseEtcdRegistry: %v", err)
	}
	defer r.Close()
	if err := r.Seed([]NodeInfo{testInfo("n1")}, 0); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	// hb PUT 事件（他副本写入）→ Ready
	hbVal, _ := json.Marshal(map[string]int64{"lastSeen": 1700000001000})
	r.applyPut(KeyHeartbeatsPrefix+"n1", hbVal)
	info, ok := r.Get("n1")
	if !ok || info.Status != StatusReady {
		t.Fatalf("hb PUT 后应 Ready: %+v ok=%v", info, ok)
	}
	// hb DELETE 事件（本副本不在服务）→ Offline
	r.applyDelete(KeyHeartbeatsPrefix + "n1")
	info, ok = r.Get("n1")
	if !ok || info.Status != StatusOffline {
		t.Fatalf("hb DELETE 后应 Offline: %+v ok=%v", info, ok)
	}
}

// 3) 本地覆盖规则（R2-4）：hb 键被删但本副本仍在服务 → 忽略 + 修复性重写（入续约队列）
func TestLeaseRegApplyDeleteServingNodeIgnoredAndRenewed(t *testing.T) {
	f := newFakeExtKV()
	r, err := newLeaseReg(f)
	if err != nil {
		t.Fatalf("NewLeaseEtcdRegistry: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer r.Close()
	r.StartWatch(ctx) // 启动续约 worker（修复性重写入队后必须有人消费）
	if err := r.Seed([]NodeInfo{testInfo("n1")}, 0); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	// 本副本正在服务：UpdateHeartbeat 注册 locallyServing（用当前墙钟）
	if err := r.UpdateHeartbeat("n1", time.Now().UnixMilli()); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}
	// hb 键被删（模拟租约抖动/他方误删）
	r.applyDelete(KeyHeartbeatsPrefix + "n1")
	info, ok := r.Get("n1")
	if !ok {
		t.Fatal("节点被从内存移除（幽灵节点）")
	}
	if info.Status != StatusReady {
		t.Fatalf("在服务节点不应被 hb 删除事件判离线: %+v", info)
	}
	// 修复性重写已入续约队列：等 worker 消费后 hb 键必须重新出现
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := f.m[KeyHeartbeatsPrefix+"n1"]; ok {
			return // 修复性重写成功
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("修复性重写未发生（hb 键未恢复）: keys=%v", f.keys())
}

// 4) watch 应用器只读铁律：applyPut/applyDelete 不产生任何 kv 写
func TestLeaseRegApplierIsReadOnly(t *testing.T) {
	f := newFakeExtKV()
	r, err := newLeaseReg(f)
	if err != nil {
		t.Fatalf("NewLeaseEtcdRegistry: %v", err)
	}
	defer r.Close()
	before := f.nextRev
	hbVal, _ := json.Marshal(map[string]int64{"lastSeen": 1700000003000})
	r.applyPut(KeyHeartbeatsPrefix+"n1", hbVal)   // 非服务节点 hb put
	r.applyDelete(KeyHeartbeatsPrefix + "n1")     // 非服务节点 hb delete
	r.applyPut(KeyNodesPrefix+"n1", []byte(`{}`)) // 台账 put
	r.applyDelete(KeyNodesPrefix + "n1")          // 台账 delete
	if f.nextRev != before {
		t.Fatalf("应用器违反了只读铁律：rev 从 %d 变为 %d", before, f.nextRev)
	}
}

// 5a) GuardedDelete 守卫：hb 键仍存在（活租约近似）→ 拒绝删除
func TestLeaseRegGuardedDeleteBlockedByLiveGuard(t *testing.T) {
	f := newFakeExtKV()
	r, err := newLeaseReg(f)
	if err != nil {
		t.Fatalf("NewLeaseEtcdRegistry: %v", err)
	}
	defer r.Close()
	// 台账 + hb 都在（节点看似活着）
	_ = f.Put(context.Background(), KeyNodesPrefix+"n1", []byte(`{}`))
	_ = f.GrantHeartbeatLease(context.Background(), KeyHeartbeatsPrefix+"n1", []byte(`{"lastSeen":1}`), 300*time.Second)
	// 模拟 GC 判死但 guard 存在：GuardedDelete 必须被守卫拦下
	deleted, err := f.GuardedDelete(context.Background(), KeyHeartbeatsPrefix+"n1", KeyNodesPrefix+"n1")
	if err != nil {
		t.Fatalf("GuardedDelete: %v", err)
	}
	if deleted {
		t.Fatal("守卫缺失：guard 键存在时仍删除了台账键")
	}
	if _, ok := f.m[KeyNodesPrefix+"n1"]; !ok {
		t.Fatal("台账键被误删（活节点）")
	}
}

// 5b) GuardedDelete：guard 键已消失（租约到期）→ 允许删除
func TestLeaseRegGuardedDeleteProceedsWhenGuardGone(t *testing.T) {
	f := newFakeExtKV()
	r, err := newLeaseReg(f)
	if err != nil {
		t.Fatalf("NewLeaseEtcdRegistry: %v", err)
	}
	defer r.Close()
	_ = f.Put(context.Background(), KeyNodesPrefix+"n1", []byte(`{}`))
	deleted, err := f.GuardedDelete(context.Background(), KeyHeartbeatsPrefix+"n1", KeyNodesPrefix+"n1")
	if err != nil {
		t.Fatalf("GuardedDelete: %v", err)
	}
	if !deleted {
		t.Fatal("guard 已消失却拒绝删除")
	}
	if _, ok := f.m[KeyNodesPrefix+"n1"]; ok {
		t.Fatal("台账键未被删除")
	}
}

// 6) EtcdHealthyWithin：接触后 true；超期 false
func TestLeaseRegEtcdHealthyWithin(t *testing.T) {
	f := newFakeExtKV()
	r, err := newLeaseReg(f)
	if err != nil {
		t.Fatalf("NewLeaseEtcdRegistry: %v", err)
	}
	defer r.Close()
	start := time.Now()
	r.markContact()
	if !r.EtcdHealthyWithin(300 * time.Second) {
		t.Fatal("刚接触应健康")
	}
	// 未接触超过 staleAfter → 不健康（contactMu 直接改时间戳模拟）
	r.contactMu.Lock()
	r.lastContact = start.Add(-301 * time.Second)
	r.contactMu.Unlock()
	if r.EtcdHealthyWithin(300 * time.Second) {
		t.Fatal("超期未接触应判不健康")
	}
}

// 8) watch 事件流单调推进 watchRev（断线重放只补增量，R2-3 修复锚点）
func TestLeaseRegWatchRevAdvances(t *testing.T) {
	f := newFakeExtKV()
	r, err := newLeaseReg(f)
	if err != nil {
		t.Fatalf("NewLeaseEtcdRegistry: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer r.Close()
	// 先锚定（LoadAnchored 设置初始 rev）
	if _, err := r.LoadAnchored(ctx); err != nil {
		t.Fatalf("LoadAnchored: %v", err)
	}
	base := r.watchRev.Load()
	r.StartWatch(ctx)
	// 等待 watchLoop goroutine 完成订阅注册，避免注入竞态丢事件
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.watchers)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 注入 put 事件（模拟他副本写台账，ModRevision 递增）
	hbVal, _ := json.Marshal(map[string]int64{"lastSeen": 1700000005000})
	f.inject(etcdstore.WatchEvent{Type: etcdstore.WatchEventPut, Key: KeyHeartbeatsPrefix + "n1", Value: hbVal, ModRevision: base + 5})
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r.watchRev.Load() >= base+5 {
			return // 锚点已推进
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("watch 事件后 watchRev 未推进（base=%d cur=%d）", base, r.watchRev.Load())
}

// 9) LoadAnchored：台账 + hb 双空间恢复 → alive 集合与状态正确
func TestLeaseRegLoadAnchored(t *testing.T) {
	f := newFakeExtKV()
	r, err := newLeaseReg(f)
	if err != nil {
		t.Fatalf("NewLeaseEtcdRegistry: %v", err)
	}
	defer r.Close()
	// etcd 侧现状：n1 台账+hb 都在（Ready）；n2 只有台账（Offline）
	_ = f.Put(context.Background(), KeyNodesPrefix+"n1", []byte(`{"nodeID":"n1"}`))
	_ = f.Put(context.Background(), KeyNodesPrefix+"n2", []byte(`{"nodeID":"n2"}`))
	_ = f.GrantHeartbeatLease(context.Background(), KeyHeartbeatsPrefix+"n1", []byte(`{"lastSeen":1700000004000}`), 300*time.Second)
	rev, err := r.LoadAnchored(context.Background())
	if err != nil {
		t.Fatalf("LoadAnchored: %v", err)
	}
	if rev <= 0 {
		t.Fatalf("锚定 revision 无效: %d", rev)
	}
	info1, ok1 := r.Get("n1")
	info2, ok2 := r.Get("n2")
	if !ok1 || info1.Status != StatusReady {
		t.Fatalf("n1 应 Ready: %+v ok=%v", info1, ok1)
	}
	// n2：台账在 + hb 不在 → Load 瞬时态 Unknown；对账（源 2）后收敛 Offline
	if !ok2 || info2.Status != StatusUnknown {
		t.Fatalf("n2 Load 瞬时态应 Unknown: %+v ok=%v", info2, ok2)
	}
	r.rescanOnce(context.Background())
	info2, ok2 = r.Get("n2")
	if !ok2 || info2.Status != StatusOffline {
		t.Fatalf("n2 对账后应 Offline: %+v ok=%v", info2, ok2)
	}
	// 双空间键都在
	keys := f.keys()
	foundHB, foundNode := false, false
	for _, k := range keys {
		if k == KeyHeartbeatsPrefix+"n1" {
			foundHB = true
		}
		if k == KeyNodesPrefix+"n1" {
			foundNode = true
		}
	}
	if !foundHB || !foundNode {
		t.Fatalf("LoadAnchored 不应写入任何键（只读加载）: keys=%v", keys)
	}
}