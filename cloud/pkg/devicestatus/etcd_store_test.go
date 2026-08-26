// EtcdDeviceStore 写穿包装测试：fake KVStore（内存 map）验证
// 写穿 / 不落盘 / 重启恢复 / 级联删除 / 合并语义（设计 §8.2 子集）。
package devicestatus

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

var testCtx = context.Background()

// fakeKV 是 KVStore 的内存实现（含故障注入开关）。
type fakeKV struct {
	mu         sync.Mutex
	data       map[string][]byte
	failPut    bool
	failDelete bool
	closed     bool
}

func newFakeKV() *fakeKV { return &fakeKV{data: make(map[string][]byte)} }

func (f *fakeKV) Put(_ context.Context, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut {
		return fmt.Errorf("fake put failure")
	}
	f.data[key] = append([]byte(nil), value...)
	return nil
}

func (f *fakeKV) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.data[key]; ok {
		return append([]byte(nil), v...), nil
	}
	return nil, nil // 与 clientv3 一致：缺键返回空而非错误
}

func (f *fakeKV) ListByPrefix(_ context.Context, prefix string) ([]KVEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []KVEntry
	for k, v := range f.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, KVEntry{Key: k, Value: append([]byte(nil), v...)})
		}
	}
	sortEntries(out)
	return out, nil
}

func (f *fakeKV) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete {
		return fmt.Errorf("fake delete failure")
	}
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

func (f *fakeKV) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// keys 返回全部键（排序，供断言）。
func (f *fakeKV) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.data))
	for k := range f.data {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// raw 返回指定键的原始值（不存在返回 nil）。
func (f *fakeKV) raw(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.data[key]; ok {
		return v
	}
	return nil
}

var _ KVStore = (*fakeKV)(nil)

// mustNew 构造包装并断言成功。
func mustNew(t *testing.T, kv KVStore) *EtcdDeviceStore {
	t.Helper()
	s, err := NewEtcdDeviceStore(kv)
	if err != nil {
		t.Fatalf("NewEtcdDeviceStore: %v", err)
	}
	return s
}

// TestEtcdDeviceStoreSetDesiredWriteThrough 验证 SetDesired 写穿：
// etcd 先写成功才更新内存；DTO 仅含 desired（无 properties/lastReportedAt）；
// 二次写入为字段级合并。
func TestEtcdDeviceStoreSetDesiredWriteThrough(t *testing.T) {
	kv := newFakeKV()
	s := mustNew(t, kv)

	if err := s.SetDesired("n1", "default", "d1", "temp", 30); err != nil {
		t.Fatalf("SetDesired: %v", err)
	}
	key := "/edgeflow/devicestatus/n1/default/d1"
	raw := kv.raw(key)
	if raw == nil {
		t.Fatalf("写穿失败：etcd 应存在键 %q", key)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["properties"]; ok {
		t.Error("DTO 不应包含 properties")
	}
	if _, ok := rec["lastReportedAt"]; ok {
		t.Error("DTO 不应包含 lastReportedAt")
	}
	desired, ok := rec["desired"].(map[string]any)
	if !ok || desired["temp"] != float64(30) {
		t.Errorf("desired 应含 temp=30: %v", rec["desired"])
	}

	// 同设备二次 SetDesired：etcd 值为字段级合并（两属性共存）
	if err := s.SetDesired("n1", "default", "d1", "fan", 2); err != nil {
		t.Fatal(err)
	}
	raw = kv.raw(key)
	rec = nil
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	desired = rec["desired"].(map[string]any)
	if desired["temp"] != float64(30) || desired["fan"] != float64(2) {
		t.Errorf("desired 应合并两属性: %v", desired)
	}
	got, ok := s.Get("n1", "default", "d1")
	if !ok || len(got.Desired) != 2 {
		t.Errorf("内存 Desired 应有两属性: %+v", got)
	}
	if n := len(kv.keys()); n != 1 {
		t.Errorf("应恰好 1 个键，实际 %d: %v", n, kv.keys())
	}
}

// TestEtcdDeviceStoreSetDesiredPutFailureKeepsMemory 验证写穿失败语义：
// etcd Put 失败 → error、内存不动、自动创建也不发生。
func TestEtcdDeviceStoreSetDesiredPutFailureKeepsMemory(t *testing.T) {
	kv := newFakeKV()
	s := mustNew(t, kv)
	key := "/edgeflow/devicestatus/n1/default/d1"

	if err := s.SetDesired("n1", "default", "d1", "p1", 1); err != nil {
		t.Fatal(err)
	}
	kv.failPut = true
	if err := s.SetDesired("n1", "default", "d1", "p2", 2); err == nil {
		t.Fatal("etcd Put 失败应返回 error")
	}
	got, ok := s.Get("n1", "default", "d1")
	if !ok {
		t.Fatal("记录应仍在内存")
	}
	if len(got.Desired) != 1 || got.Desired["p1"] != 1 {
		t.Errorf("Put 失败后内存 Desired 不应变化: %v", got.Desired)
	}
	var rec deviceShadowRecord
	if err := json.Unmarshal(kv.raw(key), &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Desired) != 1 || rec.Desired["p1"] != 1 {
		t.Errorf("Put 失败后 etcd 值不应变化: %v", rec.Desired)
	}

	// 全新设备：Put 失败 → 内存不自动创建
	if err := s.SetDesired("n1", "default", "d2", "p", 1); err == nil {
		t.Fatal("全新设备 Put 失败也应返回 error")
	}
	if _, ok := s.Get("n1", "default", "d2"); ok {
		t.Error("Put 失败时不应自动创建内存记录")
	}
}

// TestEtcdDeviceStoreUpsertPersists 验证 v0.10.0 reported 写穿：
// Upsert 写穿完整快照（含 Properties/LastReportedAt）且保 Desired；内存同步更新。
func TestEtcdDeviceStoreUpsertPersists(t *testing.T) {
	kv := newFakeKV()
	s := mustNew(t, kv)
	if err := s.SetDesired("n1", "default", "d1", "target", 25); err != nil {
		t.Fatal(err)
	}
	key := "/edgeflow/devicestatus/n1/default/d1"

	if err := s.Upsert("n1", DeviceStatus{
		DeviceName: "d1", Namespace: "default",
		Properties: map[string]float64{"temperature": 25.5}, LastReportedAt: 2000,
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(kv.keys()); n != 1 {
		t.Errorf("Upsert 应写穿既有键（键数 %d）: %v", n, kv.keys())
	}
	var rec deviceShadowRecord
	if err := json.Unmarshal(kv.raw(key), &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Desired) != 1 || rec.Desired["target"] != 25 {
		t.Errorf("Upsert 写穿应保留 Desired: %v", rec.Desired)
	}
	if rec.Properties["temperature"] != 25.5 || rec.LastReportedAt != 2000 {
		t.Errorf("Upsert 写穿应含 reported: %+v", rec)
	}
	got, _ := s.Get("n1", "default", "d1")
	if got.Properties["temperature"] != 25.5 || got.LastReportedAt != 2000 {
		t.Errorf("内存应含上报属性: %+v", got)
	}

	// 全新设备的 Upsert：写穿（v0.10.0 起 reported 持久化）→ 新增 etcd 键
	if err := s.Upsert("n1", DeviceStatus{DeviceName: "d9", Properties: map[string]float64{"v": 1}}); err != nil {
		t.Fatal(err)
	}
	if n := len(kv.keys()); n != 2 {
		t.Errorf("全新设备 Upsert 应写穿新增键（当前 %d）", n)
	}
	if _, ok := s.Get("n1", "default", "d9"); !ok {
		t.Error("内存应有 d9")
	}
}

// TestEtcdDeviceStoreRestartRecovery 验证重启恢复（设计 §8.2）：
// 写 → 重建包装 → Load → Desired 在、Properties 为空 map、LastReportedAt=0。
func TestEtcdDeviceStoreRestartRecovery(t *testing.T) {
	kv := newFakeKV()
	s1 := mustNew(t, kv)
	if err := s1.SetDesired("n1", "default", "d1", "temp", 30); err != nil {
		t.Fatal(err)
	}
	if err := s1.SetDesired("n2", "", "d2", "power", 1); err != nil { // 缺省 namespace
		t.Fatal(err)
	}
	// 上报瞬态（不落盘）
	if err := s1.Upsert("n1", DeviceStatus{DeviceName: "d1", Properties: map[string]float64{"t": 1}, LastReportedAt: 999}); err != nil {
		t.Fatal(err)
	}

	// 重启：全新内存态包装 + 同一 kv
	s2 := mustNew(t, kv)
	if err := s2.Load(testCtx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := s2.Count(); n != 2 {
		t.Fatalf("Count = %d，期望 2", n)
	}
	got, ok := s2.Get("n1", "default", "d1")
	if !ok {
		t.Fatal("d1 应恢复")
	}
	if got.Desired["temp"] != 30 {
		t.Errorf("Desired 应恢复: %v", got.Desired)
	}
	if got.Properties == nil {
		t.Errorf("Properties 应为非 nil（v0.10.0 起 reported 持久化，恢复应含上报属性）: %#v", got.Properties)
	}
	if got.LastReportedAt == 0 {
		t.Errorf("LastReportedAt 应保留（v0.10.0 起 reported 持久化）: %d", got.LastReportedAt)
	}
	got2, ok := s2.Get("n2", "default", "d2")
	if !ok || got2.Desired["power"] != 1 {
		t.Errorf("缺省 namespace 记录应恢复: ok=%v %+v", ok, got2)
	}
	if all := s2.ListAll(); len(all) != 2 {
		t.Errorf("ListAll = %d，期望 2", len(all))
	}
}

// TestEtcdDeviceStoreUpsertMergePreservesDesiredAfterLoad 验证加载后
// Upsert 合并保 Desired 语义依然成立（设计 §1 关键语义）。
func TestEtcdDeviceStoreUpsertMergePreservesDesiredAfterLoad(t *testing.T) {
	kv := newFakeKV()
	s1 := mustNew(t, kv)
	if err := s1.SetDesired("n1", "default", "d1", "targetTemp", 25); err != nil {
		t.Fatal(err)
	}

	s2 := mustNew(t, kv)
	if err := s2.Load(testCtx); err != nil {
		t.Fatal(err)
	}
	// 重启后边缘再上报一轮：Desired 不被覆盖
	if err := s2.Upsert("n1", DeviceStatus{
		DeviceName: "d1", Namespace: "default",
		Properties: map[string]float64{"temperature": 27}, LastReportedAt: 2000,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s2.Get("n1", "default", "d1")
	if got.Desired["targetTemp"] != 25 {
		t.Errorf("加载后 Upsert 不应清空 Desired: %v", got.Desired)
	}
	if got.Properties["temperature"] != 27 || got.LastReportedAt != 2000 {
		t.Errorf("上报应更新 Properties/LastReportedAt: %+v", got)
	}
}

// TestEtcdDeviceStoreDeleteByNode 验证节点级联删除（registry GC 调用面）：
// DeleteRange 子树 + 内存全清，其余节点不受影响，幂等。
func TestEtcdDeviceStoreDeleteByNode(t *testing.T) {
	kv := newFakeKV()
	s := mustNew(t, kv)
	if err := s.SetDesired("n1", "default", "d1", "p", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDesired("n1", "default", "d2", "p", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDesired("n2", "default", "d3", "p", 3); err != nil {
		t.Fatal(err)
	}
	if n := len(kv.keys()); n != 3 {
		t.Fatalf("应 3 键，实际 %d: %v", n, kv.keys())
	}

	if err := s.DeleteByNode("n1"); err != nil {
		t.Fatalf("DeleteByNode: %v", err)
	}
	keys := kv.keys()
	if len(keys) != 1 || !strings.Contains(keys[0], "/n2/") {
		t.Errorf("n1 子树应级联删除，剩余 %v", keys)
	}
	if _, ok := s.Get("n1", "default", "d1"); ok {
		t.Error("n1/d1 内存应删除")
	}
	if _, ok := s.Get("n1", "default", "d2"); ok {
		t.Error("n1/d2 内存应删除")
	}
	if _, ok := s.Get("n2", "default", "d3"); !ok {
		t.Error("n2 记录应保留")
	}
	// 幂等
	if err := s.DeleteByNode("n1"); err != nil {
		t.Errorf("重复 DeleteByNode 应幂等成功: %v", err)
	}
	if err := s.DeleteByNode(""); err != nil {
		t.Errorf("空 nodeID 应 no-op: %v", err)
	}
}

// TestEtcdDeviceStoreDeleteWriteThrough 验证 Delete 写穿：etcd 先删成功
// 再删内存；幂等；etcd 删除失败时内存不动（失败仅记日志，签名约束见
// Store 接口注释）。
func TestEtcdDeviceStoreDeleteWriteThrough(t *testing.T) {
	kv := newFakeKV()
	s := mustNew(t, kv)
	if err := s.SetDesired("n1", "default", "d1", "p", 1); err != nil {
		t.Fatal(err)
	}
	key := "/edgeflow/devicestatus/n1/default/d1"

	if ok := s.Delete("n1", "default", "d1"); !ok {
		t.Fatal("Delete 应返回 true")
	}
	if kv.raw(key) != nil {
		t.Error("etcd 键应已删除")
	}
	if _, ok := s.Get("n1", "default", "d1"); ok {
		t.Error("内存应已删除")
	}

	if ok := s.Delete("n1", "default", "d1"); ok {
		t.Error("重复删除应返回 false（幂等）")
	}
	if ok := s.Delete("", "default", "d1"); ok {
		t.Error("空 nodeID 应返回 false")
	}
	// etcd 删除失败 → 内存不动
	if err := s.SetDesired("n1", "default", "d2", "p", 1); err != nil {
		t.Fatal(err)
	}
	kv.failDelete = true
	if ok := s.Delete("n1", "default", "d2"); ok {
		t.Error("etcd 删除失败应返回 false（未删除）")
	}
	if _, ok := s.Get("n1", "default", "d2"); !ok {
		t.Error("etcd 删除失败后内存不应删除")
	}
	if kv.raw("/edgeflow/devicestatus/n1/default/d2") == nil {
		t.Error("etcd 删除失败后键应保留")
	}
}

// TestEtcdDeviceStoreKeyValidation 验证键空间硬约束（设计 §2）：
// 非法 nodeID/namespace/deviceName → 拒绝写入且不产生任何键。
func TestEtcdDeviceStoreKeyValidation(t *testing.T) {
	kv := newFakeKV()
	s := mustNew(t, kv)

	bad := []struct{ nodeID, ns, dev string }{
		{"n/1", "default", "d1"}, // nodeID 含 '/'
		{"n1", "default", "d/1"}, // deviceName 含 '/'
		{"n1", "a/b", "d1"},      // namespace 含 '/'
		{"n1 ", "default", "d1"}, // 空白字符不在 nodeID 白名单
	}
	for _, c := range bad {
		if err := s.SetDesired(c.nodeID, c.ns, c.dev, "p", 1); err == nil {
			t.Errorf("SetDesired(%q,%q,%q) 应拒绝写入", c.nodeID, c.ns, c.dev)
		}
	}
	if n := len(kv.keys()); n != 0 {
		t.Errorf("非法写入不应产生任何键: %v", kv.keys())
	}
	if n := s.Count(); n != 0 {
		t.Errorf("非法写入不应产生内存记录: %d", n)
	}
	if err := s.DeleteByNode("n/1"); err == nil {
		t.Error("DeleteByNode 非法 nodeID 应报错")
	}
	// 空参数：与内存语义一致忽略（nil error）
	if err := s.SetDesired("", "default", "d1", "p", 1); err != nil {
		t.Errorf("空 nodeID 应忽略: %v", err)
	}
	if err := s.SetDesired("n1", "default", "", "p", 1); err != nil {
		t.Errorf("空 deviceName 应忽略: %v", err)
	}
	if err := s.SetDesired("n1", "default", "d1", "", 1); err != nil {
		t.Errorf("空 property 应忽略: %v", err)
	}
}

// TestEtcdDeviceStoreLoadSkipsBadKeys 验证单键损坏不阻断全库（设计 §6.5）：
// 坏 JSON / 非法记录跳过，好键照常加载。
func TestEtcdDeviceStoreLoadSkipsBadKeys(t *testing.T) {
	kv := newFakeKV()
	s1 := mustNew(t, kv)
	if err := s1.SetDesired("n1", "default", "good", "p", 1); err != nil {
		t.Fatal(err)
	}
	// 手工注入坏键
	kv.data["/edgeflow/devicestatus/n1/default/garbage"] = []byte("{not-json")
	kv.data["/edgeflow/devicestatus/n1/default/odd"] = []byte(`{"nodeID":"n/1","deviceName":"odd","namespace":"default","desired":{"p":1}}`)

	s2 := mustNew(t, kv)
	if err := s2.Load(testCtx); err != nil {
		t.Fatalf("Load 不应因坏键失败: %v", err)
	}
	if n := s2.Count(); n != 1 {
		t.Errorf("坏键应跳过，Count = %d（期望 1）", n)
	}
	if _, ok := s2.Get("n1", "default", "good"); !ok {
		t.Error("好键应正常加载")
	}
}

// TestEtcdDeviceStoreLoadPrefixScoped 验证 WithKeyPrefix 自定义前缀
// 只扫描自己的子树（多租户/测试隔离）。
func TestEtcdDeviceStoreLoadPrefixScoped(t *testing.T) {
	kv := newFakeKV()
	s1, err := NewEtcdDeviceStore(kv, WithKeyPrefix("/test/shadow"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.SetDesired("n1", "default", "d1", "p", 1); err != nil {
		t.Fatal(err)
	}
	other, err := NewEtcdDeviceStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SetDesired("other", "default", "x", "p", 1); err != nil {
		t.Fatal(err)
	}

	s2, err := NewEtcdDeviceStore(kv, WithKeyPrefix("/test/shadow"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Load(testCtx); err != nil {
		t.Fatal(err)
	}
	if n := s2.Count(); n != 1 {
		t.Errorf("自定义前缀应只加载自己的子树，Count = %d", n)
	}
}

// TestEtcdDeviceStoreNewErrors 验证构造错误分支。
func TestEtcdDeviceStoreNewErrors(t *testing.T) {
	if _, err := NewEtcdDeviceStore(nil); err == nil {
		t.Error("nil KVStore 应报错")
	}
	if _, err := NewEtcdDeviceStore(newFakeKV(), WithKeyPrefix("")); err == nil {
		t.Error("空前缀应报错")
	}
	if _, err := NewEtcdDeviceStore(newFakeKV(), WithKeyPrefix("nope")); err == nil {
		t.Error("不以 '/' 开头的前缀应报错")
	}
}

// TestEtcdDeviceStoreClose 验证 Close 转发到底层 KV。
func TestEtcdDeviceStoreClose(t *testing.T) {
	kv := newFakeKV()
	s := mustNew(t, kv)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if !kv.closed {
		t.Error("Close 应转发到底层 KV")
	}
}

// TestDeviceShadowRecordRoundtrip 验证 DTO 收缩/还原：
// 丢瞬态（properties/lastReportedAt）、还原归一（Properties={}、时间为 0）。
func TestDeviceShadowRecordRoundtrip(t *testing.T) {
	ds := DeviceStatus{
		NodeID: "n1", DeviceName: "d1", Namespace: "default",
		Properties: map[string]float64{"t": 1}, Desired: map[string]float64{"target": 25},
		LastReportedAt: 123456,
	}
	data, err := json.Marshal(toDeviceShadowRecord(ds))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["properties"]; !ok {
		t.Error("序列化应含 properties（v0.10.0 起 reported 持久化）")
	}
	if _, ok := raw["lastReportedAt"]; !ok {
		t.Error("序列化应含 lastReportedAt（v0.10.0 起 reported 持久化）")
	}

	var rec deviceShadowRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	restored := rec.toDeviceStatus()
	if restored.Properties == nil || len(restored.Properties) == 0 {
		t.Errorf("Properties 应保留（v0.10.0 起 reported 持久化）: %#v", restored.Properties)
	}
	if restored.LastReportedAt == 0 {
		t.Errorf("LastReportedAt 应保留（v0.10.0 起 reported 持久化）: %d", restored.LastReportedAt)
	}
	if restored.Desired["target"] != 25 {
		t.Errorf("Desired 应保留: %v", restored.Desired)
	}
	if restored.Namespace != "default" {
		t.Errorf("Namespace 应归一: %q", restored.Namespace)
	}
}