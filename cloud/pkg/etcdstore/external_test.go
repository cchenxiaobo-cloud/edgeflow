// 外部 etcd 模式测试（package etcdstore_test：需要跨包组合
// registry/devicestatus 包装验证"三存储零改动"，故用外部测试包）。
//
// 基建约定：起一个临时 embed 模拟"外部 etcd 集群"，被测代码全部走公开 API
// （etcdstore.Start/ClientURL/NewKVStore/NewKVStoreWithTLS/EnsureSchemaVersion），
// 与生产外部模式（clientv3 直连远端端点）同一条代码路径。
package etcdstore_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"edgeflow/cloud/pkg/devicestatus"
	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/cloud/pkg/registry"
)

var extCtx = context.Background()

func extFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("探测空闲端口失败: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// extConfig 构造独立临时 embed 配置（独立数据目录 + 随机空闲端口）。
func extConfig(t *testing.T) etcdstore.Config {
	t.Helper()
	return etcdstore.Config{
		Enabled:                 true,
		DataDir:                 t.TempDir(),
		ClientURL:               fmt.Sprintf("http://127.0.0.1:%d", extFreePort(t)),
		PeerURL:                 fmt.Sprintf("http://127.0.0.1:%d", extFreePort(t)),
		QuotaBackendBytes:       64 * 1024 * 1024,
		AutoCompactionMode:      "periodic",
		AutoCompactionRetention: time.Hour,
	}
}

// startExternalEtcd 启动临时 embed 模拟外部集群，返回其 client URL 与
// 一个明文直连客户端（NewKVStore = 外部模式使用的同一工厂）。
func startExternalEtcd(t *testing.T) (string, etcdstore.KVStore) {
	t.Helper()
	et, err := etcdstore.Start(extConfig(t))
	if err != nil {
		t.Fatalf("临时 embed 启动失败（模拟外部 etcd）: %v", err)
	}
	t.Cleanup(func() { _ = et.Close() })
	kv, err := etcdstore.NewKVStore([]string{et.ClientURL()})
	if err != nil {
		t.Fatalf("NewKVStore 直连失败: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })
	return et.ClientURL(), kv
}

// --- 外部直连 CRUD + 前缀扫描 ---

func TestExternalKVStoreCRUD(t *testing.T) {
	_, kv := startExternalEtcd(t)

	if err := kv.Put(extCtx, "/edgeflow/registry/single/n1", []byte(`{"nodeID":"n1"}`)); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	got, err := kv.Get(extCtx, "/edgeflow/registry/single/n1")
	if err != nil || string(got) != `{"nodeID":"n1"}` {
		t.Fatalf("Get = %q err=%v", got, err)
	}
	if got, err := kv.Get(extCtx, "missing"); err != nil || got != nil {
		t.Fatalf("Get missing = %v err=%v，应为 nil,nil", got, err)
	}

	keys := map[string]string{
		"/edgeflow/registry/nodes/a": "1",
		"/edgeflow/registry/nodes/b": "2",
		"/edgeflow/registry/other":   "3",
	}
	for k, v := range keys {
		if err := kv.Put(extCtx, k, []byte(v)); err != nil {
			t.Fatalf("Put %s 失败: %v", k, err)
		}
	}
	entries, err := kv.ListByPrefix(extCtx, "/edgeflow/registry/nodes/")
	if err != nil {
		t.Fatalf("ListByPrefix 失败: %v", err)
	}
	if len(entries) != 2 || entries[0].Key != "/edgeflow/registry/nodes/a" || entries[1].Key != "/edgeflow/registry/nodes/b" {
		t.Fatalf("ListByPrefix = %+v，应为按序 [a b]", entries)
	}
	if string(entries[0].Value) != "1" {
		t.Fatalf("值不匹配: %+v", entries[0])
	}

	if err := kv.DeleteRange(extCtx, "/edgeflow/registry/nodes/"); err != nil {
		t.Fatalf("DeleteRange 失败: %v", err)
	}
	if entries, _ := kv.ListByPrefix(extCtx, "/edgeflow/registry/nodes/"); len(entries) != 0 {
		t.Fatalf("DeleteRange 后仍有 %d 条", len(entries))
	}
	if got, _ := kv.Get(extCtx, "/edgeflow/registry/other"); string(got) != "3" {
		t.Fatalf("DeleteRange 误删前缀外键: %q", got)
	}

	if err := kv.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	// Close 幂等（外部模式 closeEtcd 会经由 deviceStore.Close + kv.Close 两次调用）
	if err := kv.Close(); err != nil {
		t.Fatalf("二次 Close 应幂等（closeOnce）: %v", err)
	}
}

// --- 三存储零改动验证：registry/devicestatus 包装直连外部 client ---

func TestExternalRegistryWriteThrough(t *testing.T) {
	endpoint, _ := startExternalEtcd(t)

	// 第一个 cloudcore 副本：直连外部集群，写穿登记
	kv1, err := etcdstore.NewKVStore([]string{endpoint})
	if err != nil {
		t.Fatalf("kv1 失败: %v", err)
	}
	defer kv1.Close()
	reg1, err := registry.NewEtcdRegistry(kv1, registry.WithOfflineTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewEtcdRegistry 失败: %v", err)
	}
	defer reg1.Close()
	if err := reg1.Register(registry.NodeInfo{
		NodeID:          "node-a",
		NodeName:        "node-a",
		LastHeartbeatAt: time.Now().UnixMilli(),
		Status:          registry.StatusReady,
	}); err != nil {
		t.Fatalf("Register（外部写穿）失败: %v", err)
	}

	// 第二个副本（重启后）：同集群 Load 恢复 → 写穿持久化成立
	kv2, err := etcdstore.NewKVStore([]string{endpoint})
	if err != nil {
		t.Fatalf("kv2 失败: %v", err)
	}
	defer kv2.Close()
	reg2, err := registry.NewEtcdRegistry(kv2, registry.WithOfflineTTL(time.Hour))
	if err != nil {
		t.Fatalf("注册表恢复实例失败: %v", err)
	}
	defer reg2.Close()
	if err := reg2.Load(extCtx); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	got, ok := reg2.Get("node-a")
	if !ok || got.NodeID != "node-a" {
		t.Fatalf("副本2 Get node-a = %+v ok=%v，应恢复（外部 etcd 共享事实源）", got, ok)
	}
}

func TestExternalDeviceStoreWriteThrough(t *testing.T) {
	endpoint, _ := startExternalEtcd(t)

	kv1, _ := etcdstore.NewKVStore([]string{endpoint})
	defer kv1.Close()
	ds1, err := devicestatus.NewEtcdDeviceStore(kv1)
	if err != nil {
		t.Fatalf("NewEtcdDeviceStore 失败: %v", err)
	}
	// devicestatus 写穿契约（v0.4.0）：reported 瞬态（Upsert）不落盘，
	// 云端指令 Desired 落盘（SetDesired）——外部模式下契约不变。
	if err := ds1.SetDesired("node-a", "default", "d1", "temperature", 30); err != nil {
		t.Fatalf("SetDesired（外部写穿）失败: %v", err)
	}

	// 副本2 Load 恢复
	kv2, _ := etcdstore.NewKVStore([]string{endpoint})
	defer kv2.Close()
	ds2, err := devicestatus.NewEtcdDeviceStore(kv2)
	if err != nil {
		t.Fatalf("设备存储恢复实例失败: %v", err)
	}
	if err := ds2.Load(extCtx); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	st, ok := ds2.Get("node-a", "default", "d1")
	if !ok || st.DeviceName != "d1" || st.Desired["temperature"] != 30 {
		t.Fatalf("副本2 Get = %+v ok=%v，应恢复设备影子（Desired）", st, ok)
	}
}

// --- schemaVersion 迁移钩子三场景 ---

func TestEnsureSchemaVersion(t *testing.T) {
	// 场景1：新库 → Put 当前版本
	_, kv := startExternalEtcd(t)
	if err := etcdstore.EnsureSchemaVersion(extCtx, kv, etcdstore.DefaultSchemaVersion); err != nil {
		t.Fatalf("新库应写入版本: %v", err)
	}
	got, err := kv.Get(extCtx, etcdstore.SchemaVersionKey)
	if err != nil || string(got) != etcdstore.DefaultSchemaVersion {
		t.Fatalf("写入版本 = %q err=%v", got, err)
	}

	// 场景2：已匹配 → nil（不动键）
	if err := etcdstore.EnsureSchemaVersion(extCtx, kv, etcdstore.DefaultSchemaVersion); err != nil {
		t.Fatalf("版本匹配应 nil: %v", err)
	}

	// 场景3：不匹配 → *SchemaVersionMismatchError（告警语义，不阻断由装配层体现）
	if err := kv.Put(extCtx, etcdstore.SchemaVersionKey, []byte("2")); err != nil {
		t.Fatalf("Put 版本 2 失败: %v", err)
	}
	err = etcdstore.EnsureSchemaVersion(extCtx, kv, etcdstore.DefaultSchemaVersion)
	var mismatch *etcdstore.SchemaVersionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("不匹配应返回 *SchemaVersionMismatchError，实际 %v", err)
	}
	if mismatch.Got != "2" || mismatch.Want != etcdstore.DefaultSchemaVersion {
		t.Errorf("mismatch = %+v", mismatch)
	}
	// 版本键未被误改
	if got, _ := kv.Get(extCtx, etcdstore.SchemaVersionKey); string(got) != "2" {
		t.Errorf("不匹配时不得改写版本键: %q", got)
	}
}

// --- 启动探活（R3）：成功路径（连模拟外部集群）+ 失败路径（连接拒绝快速失败） ---

func TestProbeAlive(t *testing.T) {
	// 成功：clientv3 直连模拟外部集群，Get 探针通过。
	url, kv := startExternalEtcd(t)
	_ = url
	if err := etcdstore.ProbeAlive(kv); err != nil {
		t.Fatalf("探活应成功（外部集群可达）: %v", err)
	}

	// 失败：监听后立即关闭的端口 → connection refused 立即失败（非超时），
	// 3 次尝试 ≈ 3×RTT + 2×1s sleep，快速返回错误。
	port := extFreePort(t)
	badKV, err := etcdstore.NewKVStore([]string{fmt.Sprintf("http://127.0.0.1:%d", port)})
	if err != nil {
		t.Fatalf("NewKVStore（懒连接）不应在此失败: %v", err)
	}
	defer badKV.Close()
	if err := etcdstore.ProbeAlive(badKV); err == nil {
		t.Fatal("不可达端点探活应失败（fail-fast 依据）")
	}
}

// --- 断连恢复单元级（复核 ⚠️-7 / 设计 §10.3 R4）：Close → Put 报错不 panic →
// 同数据目录重启 → clientv3 自动重连 → Put 成功且旧键仍在 ---
func TestExternalKVReconnectAfterRestart(t *testing.T) {
	dir := t.TempDir()
	port := extFreePort(t)
	cfg := etcdstore.Config{
		Enabled:                 true,
		DataDir:                 dir,
		ClientURL:               fmt.Sprintf("http://127.0.0.1:%d", port),
		PeerURL:                 fmt.Sprintf("http://127.0.0.1:%d", extFreePort(t)),
		QuotaBackendBytes:       64 * 1024 * 1024,
		AutoCompactionMode:      "periodic",
		AutoCompactionRetention: time.Hour,
	}
	et, err := etcdstore.Start(cfg)
	if err != nil {
		t.Fatalf("临时 embed 启动失败: %v", err)
	}
	kv, err := etcdstore.NewKVStore([]string{et.ClientURL()})
	if err != nil {
		t.Fatalf("NewKVStore 失败: %v", err)
	}
	defer kv.Close()

	key := "/edgeflow/_test/reconnect"
	if err := kv.Put(context.Background(), key, []byte("v1")); err != nil {
		t.Fatalf("初始 Put 失败: %v", err)
	}

	// 关停后端 → 写失败（内存态不动：报错、不 panic、不提交）
	if err := et.Close(); err != nil {
		t.Fatalf("etcd 关停失败: %v", err)
	}
	if err := kv.Put(context.Background(), key, []byte("v2")); err == nil {
		t.Fatal("etcd 关停后 Put 应报错（stale-but-consistent）")
	}

	// 同数据目录重启（同端口）→ clientv3 自动重连（轮询等待写恢复）
	et2, err := etcdstore.Start(cfg)
	if err != nil {
		t.Fatalf("同数据目录重启失败: %v", err)
	}
	defer et2.Close()

	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := kv.Put(context.Background(), key, []byte("v2")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("自动重连后写入未在 15s 内恢复: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}

	// 断连期旧键保留且读到 v2（重连后写入成功，键空间一致）
	got, err := kv.Get(context.Background(), key)
	if err != nil || string(got) != "v2" {
		t.Fatalf("重连后 Get 应读到 v2: got=%q err=%v", got, err)
	}
}
