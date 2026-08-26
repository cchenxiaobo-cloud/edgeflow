package registry

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fake KVStore：内存 map 实现（测试专用，放测试文件内） ----
//
// 语义对齐 etcd clientv3 KV 子集：
//   - Put 覆盖写；failPut 非 nil 时模拟 etcd 故障（不写入）
//   - ListByPrefix 按键字典序返回（对齐 etcd Range 语义）
//   - DeleteRange(prefix) 删除所有以 prefix 开头的键
//   - 全部方法可并发调用

type fakeKV struct {
	mu      sync.Mutex
	m       map[string][]byte
	failPut error // 非 nil：Put 恒失败（模拟 etcd 不可用）
}

func newFakeKV() *fakeKV { return &fakeKV{m: make(map[string][]byte)} }

func (f *fakeKV) Put(_ context.Context, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil {
		return f.failPut
	}
	f.m[key] = append([]byte(nil), value...)
	return nil
}

func (f *fakeKV) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[key]
	if !ok {
		return nil, nil // etcd Get 不存在返回 nil,nil（配合调用方判断）
	}
	return append([]byte(nil), v...), nil
}

func (f *fakeKV) ListByPrefix(_ context.Context, prefix string) ([]KVEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.m))
	for k := range f.m {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]KVEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, KVEntry{Key: k, Value: append([]byte(nil), f.m[k]...)})
	}
	return out, nil
}

func (f *fakeKV) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key)
	return nil
}

func (f *fakeKV) DeleteRange(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.m {
		if strings.HasPrefix(k, prefix) {
			delete(f.m, k)
		}
	}
	return nil
}

func (f *fakeKV) Close() error { return nil }

// 键个数与键集合的断言辅助。
func (f *fakeKV) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.m))
	for k := range f.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- 编译期断言：两个实现都满足 Store ----

// ---- 测试 ----

// TestEtcdRegisterWriteThrough 验证写穿顺序与持久化 DTO 形态：
// Register 成功后 etcd 存在 /edgeflow/registry/nodes/<id>，JSON 字段
// 为小驼峰台账（含 registeredAt），不含瞬态字段 lastHeartbeatAt/status；
// 内存态为 Ready。
func TestEtcdRegisterWriteThrough(t *testing.T) {
	kv := newFakeKV()
	e, err := NewEtcdRegistry(kv, WithOfflineTTL(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	info := sampleInfo("node-1")
	info.RegisteredAt = 1720000000000
	if err := e.Register(info); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}

	// etcd 键存在且值是指定 DTO（不含瞬态字段）
	data, err := kv.Get(context.Background(), nodeKey("node-1"))
	if err != nil || data == nil {
		t.Fatalf("etcd 应有节点台账键 %s（err=%v）", nodeKey("node-1"), err)
	}
	var rec nodeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("台账 JSON 解析失败: %v", err)
	}
	if rec.NodeID != "node-1" || rec.NodeName != "node-1" || rec.Arch != "arm64" ||
		rec.OS != "linux" || rec.EdgecoreVersion != "v0.1.0" || rec.CPU != 4 ||
		rec.Memory != 8<<30 || rec.IP != "192.168.1.10" || rec.RegisteredAt != 1720000000000 {
		t.Errorf("台账字段不符: %+v", rec)
	}
	if strings.Contains(string(data), "lastHeartbeatAt") {
		t.Error("台账 JSON 不应包含 lastHeartbeatAt（瞬态不落盘）")
	}
	if strings.Contains(string(data), `"status"`) {
		t.Error("台账 JSON 不应包含 status（瞬态不落盘）")
	}

	// 内存态：Register 语义不变（Ready）
	got, ok := e.Get("node-1")
	if !ok {
		t.Fatal("Get(node-1) 应存在")
	}
	if got.Status != StatusReady {
		t.Errorf("注册后内存态应为 Ready，实际 %s", got.Status)
	}
}

// TestEtcdRegisterPutFailureKeepsMemory 验证写穿顺序：etcd Put 失败时
// 返回 error 且内存不变（"写成功 = 已持久化"，失败则什么都不算发生）。
func TestEtcdRegisterPutFailureKeepsMemory(t *testing.T) {
	kv := newFakeKV()
	e, _ := NewEtcdRegistry(kv)
	defer e.Close()

	kv.failPut = context.DeadlineExceeded
	if err := e.Register(sampleInfo("node-1")); err == nil {
		t.Fatal("etcd Put 失败时 Register 应返回 error")
	}
	if e.Count() != 0 {
		t.Errorf("Put 失败后内存不应有节点，Count = %d", e.Count())
	}
	if _, err := kv.Get(context.Background(), nodeKey("node-1")); err != nil {
		t.Errorf("读取假 etcd 失败: %v", err)
	}
	if ks := kv.keys(); len(ks) != 0 {
		t.Errorf("Put 失败后 etcd 不应有任何键，实际 %v", ks)
	}

	// 恢复后可正常注册（不残留失败状态）
	kv.failPut = nil
	if err := e.Register(sampleInfo("node-1")); err != nil {
		t.Fatalf("恢复后 Register 应成功: %v", err)
	}
	if e.Count() != 1 {
		t.Errorf("恢复后 Count = %d，期望 1", e.Count())
	}
}

// TestEtcdHeartbeatOfflineNotPersisted 验证心跳/离线仅内存：注册键之外
// etcd 不再增加任何键，内存状态照常翻转。
func TestEtcdHeartbeatOfflineNotPersisted(t *testing.T) {
	kv := newFakeKV()
	e, _ := NewEtcdRegistry(kv)
	defer e.Close()

	if err := e.Register(sampleInfo("node-1")); err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateHeartbeat("node-1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := e.MarkOffline("node-1"); err != nil {
		t.Fatal(err)
	}

	if ks := kv.keys(); len(ks) != 1 || ks[0] != nodeKey("node-1") {
		t.Errorf("etcd 应只有注册键 %s，实际 %v", nodeKey("node-1"), ks)
	}
	if got, _ := e.Get("node-1"); got.Status != StatusOffline {
		t.Errorf("内存状态应为 Offline，实际 %s", got.Status)
	}
}

// TestEtcdLoadRebuild 验证重启恢复：等二实例（etcd 同一份）重建，
// 台账完整、瞬态按约定清空（Unknown + LastHeartbeatAt=0）。
func TestEtcdLoadRebuild(t *testing.T) {
	kv := newFakeKV()

	// 实例 A：注册两个节点（不同 RegisteredAt、一个带 IP）
	a, _ := NewEtcdRegistry(kv)
	info1 := sampleInfo("node-a")
	info1.RegisteredAt = 1000
	if err := a.Register(info1); err != nil {
		t.Fatal(err)
	}
	info2 := sampleInfo("node-b")
	info2.RegisteredAt = 2000
	if err := a.Register(info2); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()

	// 实例 B：模拟重启，Load 重建
	b, _ := NewEtcdRegistry(kv, WithOfflineTTL(0)) // TTL 禁用，避免启动清理干扰断言
	defer b.Close()
	if err := b.Load(context.Background()); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if b.Count() != 2 {
		t.Fatalf("重建后 Count = %d，期望 2", b.Count())
	}
	for _, id := range []string{"node-a", "node-b"} {
		got, ok := b.Get(id)
		if !ok {
			t.Fatalf("重建后应存在 %s", id)
		}
		if got.Status != StatusUnknown {
			t.Errorf("%s 重建后状态应为 Unknown（重启不显示陈旧 Ready），实际 %s", id, got.Status)
		}
		if got.LastHeartbeatAt != 0 {
			t.Errorf("%s 重建后 LastHeartbeatAt 应为 0（瞬态不落盘），实际 %d", id, got.LastHeartbeatAt)
		}
		if got.Arch != "arm64" || got.CPU != 4 {
			t.Errorf("%s 台账元数据应恢复: %+v", id, got)
		}
	}
	if got, _ := b.Get("node-a"); got.RegisteredAt != 1000 {
		t.Errorf("RegisteredAt 应保留: %d", got.RegisteredAt)
	}
}

// TestEtcdSeedStartupCleanup 验证 Seed 播种 + 启动清理：过期节点从内存
// 移除、etcd 台账键同步删除、事件经 TakeGCEvents 外露（级联用）；
// 未过期节点保留。
func TestEtcdSeedStartupCleanup(t *testing.T) {
	kv := newFakeKV()
	e, _ := NewEtcdRegistry(kv, WithOfflineTTL(50*time.Millisecond))
	defer e.Close()

	// 预置 etcd：两个"云边同死"的旧节点 + 一个刚注册的新节点
	old1 := sampleInfo("stale-a")
	old1.RegisteredAt = 111
	old2 := sampleInfo("stale-b")
	old2.RegisteredAt = 222
	fresh := sampleInfo("fresh")
	fresh.RegisteredAt = 333
	for _, n := range []NodeInfo{old1, old2, fresh} {
		data, _ := json.Marshal(fromNodeInfo(n))
		if err := kv.Put(context.Background(), nodeKey(n.NodeID), data); err != nil {
			t.Fatal(err)
		}
	}

	// Seed 播种：since 对全部节点统一起算（启动时刻语义）。
	// 第一轮：两个旧节点以"很久以前"为 since（超保留期）→ 启动清理删掉
	since := time.Now().UnixMilli() - time.Hour.Milliseconds()
	if err := e.Seed([]NodeInfo{old1, old2}, since); err != nil {
		t.Fatal(err)
	}
	if e.Count() != 0 {
		t.Fatalf("启动清理后 Count = %d，期望 0（stale 全过期）", e.Count())
	}
	// etcd 键同步删除
	if ks := kv.keys(); len(ks) != 1 || ks[0] != nodeKey("fresh") {
		t.Errorf("etcd 应只剩 fresh 台账键，实际 %v", ks)
	}
	// 级联事件外露（供集成层删 /edgeflow/devicestatus/<id>/）
	events := e.TakeGCEvents()
	sort.Strings(events)
	if len(events) != 2 || events[0] != "stale-a" || events[1] != "stale-b" {
		t.Errorf("TakeGCEvents 应返回 [stale-a stale-b]，实际 %v", events)
	}
	if again := e.TakeGCEvents(); again != nil {
		t.Errorf("排空语义：第二次 TakeGCEvents 应为空，实际 %v", again)
	}

	// 第二轮：刚播种的 fresh（since<=0 → 当前时刻）→ 保留且 Unknown
	if err := e.Seed([]NodeInfo{fresh}, 0); err != nil {
		t.Fatal(err)
	}
	if e.Count() != 1 {
		t.Fatalf("第二轮 Seed 后 Count = %d，期望 1（仅 fresh）", e.Count())
	}
	if got, ok := e.Get("fresh"); !ok || got.Status != StatusUnknown {
		t.Errorf("fresh 应保留且 Unknown，实际 ok=%v got=%+v", ok, got)
	}
}

// TestEtcdCleanupLoop 验证定期清理循环：过期节点到期后被内存 GC 移除、
// etcd 键同步删除、事件外露；未过期节点保留；Close 停止循环。
func TestEtcdCleanupLoop(t *testing.T) {
	kv := newFakeKV()
	e, _ := NewEtcdRegistry(kv, WithOfflineTTL(50*time.Millisecond))

	if err := e.Register(sampleInfo("expired")); err != nil {
		t.Fatal(err)
	}
	if err := e.Register(sampleInfo("fresh")); err != nil {
		t.Fatal(err)
	}
	_ = e.MarkOffline("expired")
	_ = e.MarkOffline("fresh")
	// 白盒改写离线时间戳：expired 超保留期、fresh 未超
	now := time.Now().UnixMilli()
	e.reg.mu.Lock()
	e.reg.offlineSince["expired"] = now - 2*int64(time.Hour/time.Millisecond)
	e.reg.offlineSince["fresh"] = now
	e.reg.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	e.CleanupLoop(ctx, 20*time.Millisecond)
	defer func() { cancel(); _ = e.Close() }()

	// 等待循环跑数轮
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := e.Get("expired"); !ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := e.Get("expired"); ok {
		t.Fatal("CleanupLoop 应移除过期节点 expired")
	}
	if _, ok := e.Get("fresh"); !ok {
		t.Fatal("CleanupLoop 不应移除未过期节点 fresh")
	}
	ks := kv.keys()
	if len(ks) != 1 || ks[0] != nodeKey("fresh") {
		t.Errorf("etcd 应只剩 fresh 键（expired 已删），实际 %v", ks)
	}
	events := e.TakeGCEvents()
	if len(events) != 1 || events[0] != "expired" {
		t.Errorf("级联事件应含 expired，实际 %v", events)
	}
}

// TestEtcdNodeIDKeyConstraint 验证键约束：nodeID 含 '/' 被拒绝（内存与
// etcd 双保险），合法形态（UUID/主机名/带点带横线）放行。
func TestEtcdNodeIDKeyConstraint(t *testing.T) {
	kv := newFakeKV()
	e, _ := NewEtcdRegistry(kv)
	defer e.Close()

	if err := e.Register(sampleInfo("a/b")); err == nil {
		t.Error("nodeID 含 '/' 应被 Register 拒绝")
	}
	if err := e.Seed([]NodeInfo{sampleInfo("c/d")}, 0); err == nil {
		t.Error("nodeID 含 '/' 应被 Seed 拒绝")
	}
	// 纯内存实现同样拒绝（写入口统一校验）
	mem := New()
	if err := mem.Register(sampleInfo("x/y")); err == nil {
		t.Error("内存实现也应拒绝含 '/' 的 nodeID")
	}
	if ks := kv.keys(); len(ks) != 0 {
		t.Errorf("拒绝写入后 etcd 不应有键，实际 %v", ks)
	}

	// 合法形态放行（UUID/主机名/带点带横线）
	for _, id := range []string{"2f4a1b0c-9e3d-4a2b-8c1d-0f1234567890", "edge-node-01", "node.bj.01"} {
		if err := e.Register(sampleInfo(id)); err != nil {
			t.Errorf("合法 nodeID %q 不应被拒绝: %v", id, err)
		}
	}
}

// TestEtcdStoreInterfaceContract 验证两个实现都满足 Store（编译期锁定的
// 契约在运行时再抽查一次关键行为面）。
func TestEtcdStoreInterfaceContract(t *testing.T) {
	var _ Store = (*Registry)(nil)
	var _ Store = (*EtcdRegistry)(nil)

	kv := newFakeKV()
	e, _ := NewEtcdRegistry(kv)
	defer e.Close()

	// 委托的读路径与内存实现一致
	_ = e.Register(sampleInfo("node-1"))
	if _, ok := e.Get("node-1"); !ok {
		t.Error("Get 应委托成功")
	}
	if e.List() == nil || len(e.List()) != 1 {
		t.Error("List 应委托成功")
	}
	edgeNodes := e.ListEdgeNodes()
	if len(edgeNodes) != 1 || edgeNodes[0].Name != "node-1" {
		t.Errorf("ListEdgeNodes 应委托成功: %+v", edgeNodes)
	}
	if n := e.ToEdgeNode(&NodeInfo{NodeID: "n", NodeName: "n", Status: StatusReady}); n == nil || n.Status.Phase == "" {
		t.Error("ToEdgeNode 应委托成功")
	}
}
