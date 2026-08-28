// 重启去重集成测试（CHN-03 / v0.22.0，T-07 验收 2）。
//
// 场景：edgecore 重启前，云端下发某 ID 消息被成功处理并落去重库；
// 重启后（内存缓存清空），云端按 QoS 1 重试同 ID —— 必须仍被去重
// （IsProcessed=true），不重复执行。
//
// 实现方式：同一 SQLite 文件先后构建两个 Client（模拟两次进程生命周期）。
// 第一个 Client 经真实 WS 链路处理消息（MarkProcessed 走生产路径：
// 双写 SQLite + 内存缓存）；随后 Stop（模拟进程退出，内存态清零）。
// 第二个 Client 用同一 DedupStore（同库文件重新 Open + NewDedupStore）
// 构造，仅调用其去重入口断言——不重复启动链路（链路装配已被 client_test
// 全覆盖，本用例聚焦持久化语义跨 Client 实例的传递）。
package edgehub

import (
	"path/filepath"
	"testing"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/protocol"
)

// TestDedupRestartAcrossClients 验证重启去重语义（CHN-03 核心）：
// Client A（进程 1）处理消息 ID 后；Client B（进程 2，同库重建）对同 ID
// IsProcessed 必须 true —— 云端重试同 ID 被去重。
func TestDedupRestartAcrossClients(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "edgeflow.db")

	// ---------- 进程 1：真实链路处理一条下发消息 ----------
	s1, err := metamanager.Open(dbPath)
	if err != nil {
		t.Fatalf("进程 1 打开库失败: %v", err)
	}
	d1, err := metamanager.NewDedupStore(s1)
	if err != nil {
		s1.Close()
		t.Fatalf("进程 1 构造 DedupStore 失败: %v", err)
	}

	mock := startMockCloud(t, mockConfig{})
	clientA := New(Options{CloudAddr: mock.url, NodeID: "dedup-restart", DedupStore: d1})
	handled := 0
	clientA.SetMessageHandlerFunc(func(msg *protocol.Message) error {
		handled++
		return nil
	})
	clientA.Start()
	mock.waitRegister(t)

	msgID := newPodSync(t, "dedup-restart", podSyncPayload(t, "add")).ID
	if err := mock.push(newPodSyncWithID(t, "dedup-restart", msgID)); err != nil {
		t.Fatalf("进程 1 推送 PodSync 失败: %v", err)
	}
	ack := mock.waitType(t, protocol.TypeAck)
	if code := ackCode(t, ack); code != "ok" {
		t.Fatalf("进程 1 首次 Ack = %s，期望 ok", code)
	}
	if handled != 1 {
		t.Fatalf("进程 1 handler 执行次数 = %d，期望 1", handled)
	}
	clientA.Stop()
	_ = s1.Close() // 进程 1 退出：内存去重缓存随之清零，仅剩 SQLite

	// ---------- 进程 2（重启）：同库文件重建，内存缓存为空 ----------
	s2, err := metamanager.Open(dbPath)
	if err != nil {
		t.Fatalf("进程 2 打开库失败: %v", err)
	}
	defer s2.Close()
	d2, err := metamanager.NewDedupStore(s2)
	if err != nil {
		t.Fatalf("进程 2 构造 DedupStore 失败: %v", err)
	}

	clientB := New(Options{CloudAddr: "ws://127.0.0.1:1", NodeID: "dedup-restart", DedupStore: d2})
	// 重启后云端重试同 ID：IsProcessed 必须 true（去重窗口跨重启保留）
	if !clientB.isProcessed(msgID) {
		t.Fatal("重启后同 ID 消息应被去重（isProcessed=true）——云端重试被拦截")
	}
	// 生产入口等价验证：downlinkDedup 适配器直查同库适配实例
	if !newDedupAdapter(d2).IsProcessed(msgID) {
		t.Fatal("dedupAdapter（进程 2）同 ID 应命中持久去重")
	}
	// 新 ID 不受污染
	if newDedupAdapter(d2).IsProcessed("brand-new-id") {
		t.Fatal("重启后新 ID 不应被误去重")
	}
}

// TestDedupDuplicateDeliveryNoReexecute 验证同进程内（不重启）同 ID 二次投递
// 不重复执行 handler（对照基线：去重行为本身未被持久化改造破坏）。
func TestDedupDuplicateDeliveryNoReexecute(t *testing.T) {
	s, err := metamanager.Open(filepath.Join(t.TempDir(), "edgeflow.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer s.Close()
	d, err := metamanager.NewDedupStore(s)
	if err != nil {
		t.Fatalf("构造 DedupStore 失败: %v", err)
	}

	mock := startMockCloud(t, mockConfig{})
	client := New(Options{CloudAddr: mock.url, NodeID: "dedup-same", DedupStore: d})
	handled := 0
	client.SetMessageHandlerFunc(func(msg *protocol.Message) error {
		handled++
		return nil
	})
	client.Start()
	defer client.Stop()
	mock.waitRegister(t)

	msgID := newPodSync(t, "dedup-same", podSyncPayload(t, "add")).ID
	for i := 0; i < 2; i++ {
		if err := mock.push(newPodSyncWithID(t, "dedup-same", msgID)); err != nil {
			t.Fatalf("第 %d 次推送失败: %v", i+1, err)
		}
		ack := mock.waitType(t, protocol.TypeAck)
		if code := ackCode(t, ack); code != "ok" {
			t.Fatalf("第 %d 次投递 Ack = %s，期望 ok", i+1, code)
		}
	}
	if handled != 1 {
		t.Fatalf("同 ID 两次投递 handler 执行次数 = %d，期望 1（第二次被去重）", handled)
	}
}

// newPodSyncWithID 构造指定 ID 的 PodSync 消息（模拟云端重试携带原 ID）。
func newPodSyncWithID(t *testing.T, nodeID, id string) *protocol.Message {
	t.Helper()
	msg := newPodSync(t, nodeID, podSyncPayload(t, "add"))
	msg.ID = id
	return msg
}

// ackCode 解析 Ack 负载中的 code 字段（解析失败即测试失败）。
func ackCode(t *testing.T, ack *protocol.Message) string {
	t.Helper()
	var p AckPayload
	if err := ack.DecodePayload(&p); err != nil {
		t.Fatalf("解析 Ack 负载失败: %v", err)
	}
	return p.Code
}
