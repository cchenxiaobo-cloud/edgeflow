package registry

import (
	"sync"
	"testing"
	"time"
)

// sampleInfo 构造一个测试用节点信息。
func sampleInfo(nodeID string) NodeInfo {
	return NodeInfo{
		NodeID:          nodeID,
		NodeName:        nodeID,
		Arch:            "arm64",
		OS:              "linux",
		EdgecoreVersion: "v0.1.0",
		CPU:             4,
		Memory:          8 << 30,
		IP:              "192.168.1.10",
	}
}

// TestRegisterAndGet 验证注册后 Get 返回完整字段、状态 Ready、时间戳已填充。
func TestRegisterAndGet(t *testing.T) {
	r := New()
	before := time.Now().UnixMilli()
	r.Register(sampleInfo("node-1"))
	after := time.Now().UnixMilli()

	info, ok := r.Get("node-1")
	if !ok {
		t.Fatal("Get(node-1) 应返回存在")
	}
	if info.NodeID != "node-1" || info.Arch != "arm64" || info.OS != "linux" {
		t.Errorf("字段不符: %+v", info)
	}
	if info.EdgecoreVersion != "v0.1.0" || info.CPU != 4 || info.Memory != 8<<30 {
		t.Errorf("字段不符: %+v", info)
	}
	if info.Status != StatusReady {
		t.Errorf("注册后状态应为 Ready，实际 %s", info.Status)
	}
	if info.RegisteredAt < before || info.RegisteredAt > after {
		t.Errorf("RegisteredAt 应在当前时间附近，实际 %d", info.RegisteredAt)
	}
	if info.LastHeartbeatAt != info.RegisteredAt {
		t.Errorf("注册时 LastHeartbeatAt 应等于 RegisteredAt，实际 %d", info.LastHeartbeatAt)
	}
}

// TestRegisterReconnectKeepsRegisteredAt 验证重连注册更新元数据但保留首次注册时间。
func TestRegisterReconnectKeepsRegisteredAt(t *testing.T) {
	r := New()
	r.Register(sampleInfo("node-1"))
	first, _ := r.Get("node-1")

	// 模拟重连：IP 变化、RegisteredAt 不传（适配器每次都会带新时间）
	info := sampleInfo("node-1")
	info.IP = "10.0.0.9"
	r.Register(info)

	got, _ := r.Get("node-1")
	if got.RegisteredAt != first.RegisteredAt {
		t.Errorf("重连不应重置 RegisteredAt: 首次 %d，重连后 %d", first.RegisteredAt, got.RegisteredAt)
	}
	if got.IP != "10.0.0.9" {
		t.Errorf("重连应更新元数据 IP，实际 %s", got.IP)
	}
}

// TestUpdateHeartbeat 验证心跳刷新时间并恢复 Ready（离线后可恢复）。
func TestUpdateHeartbeat(t *testing.T) {
	r := New()
	r.Register(sampleInfo("node-1"))
	r.MarkOffline("node-1")

	ts := time.Now().UnixMilli()
	r.UpdateHeartbeat("node-1", ts)

	info, _ := r.Get("node-1")
	if info.Status != StatusReady {
		t.Errorf("心跳后状态应为 Ready，实际 %s", info.Status)
	}
	if info.LastHeartbeatAt != ts {
		t.Errorf("LastHeartbeatAt 应等于传入时间 %d，实际 %d", ts, info.LastHeartbeatAt)
	}

	// 未注册节点的心跳应被忽略（不 panic、不新增）
	r.UpdateHeartbeat("ghost", time.Now().UnixMilli())
	if r.Count() != 1 {
		t.Errorf("未注册节点心跳不应新增节点，Count = %d", r.Count())
	}
}

// TestMarkOffline 验证下线保留元数据、状态 Offline。
func TestMarkOffline(t *testing.T) {
	r := New()
	r.Register(sampleInfo("node-1"))
	r.MarkOffline("node-1")

	info, ok := r.Get("node-1")
	if !ok {
		t.Fatal("下线后节点元数据应保留")
	}
	if info.Status != StatusOffline {
		t.Errorf("下线后状态应为 Offline，实际 %s", info.Status)
	}
	if info.Arch != "arm64" {
		t.Errorf("下线不应清空元数据: %+v", info)
	}

	// 未注册节点下线应被忽略
	r.MarkOffline("ghost")
	if r.Count() != 1 {
		t.Errorf("未注册节点下线不应新增节点，Count = %d", r.Count())
	}
}

// TestGetMissing 验证查询不存在的节点返回 false。
func TestGetMissing(t *testing.T) {
	r := New()
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(nope) 应返回不存在")
	}
}

// TestListSortedAndCopy 验证列表按 NodeID 排序且返回拷贝（修改不污染内部）。
func TestListSortedAndCopy(t *testing.T) {
	r := New()
	r.Register(sampleInfo("node-b"))
	r.Register(sampleInfo("node-a"))
	r.Register(sampleInfo("node-c"))

	list := r.List()
	if len(list) != 3 {
		t.Fatalf("List 长度 = %d，期望 3", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].NodeID >= list[i].NodeID {
			t.Errorf("List 未按 NodeID 排序: %v", list)
		}
	}

	// 修改返回的拷贝不应影响内部状态
	list[0].NodeID = "hacked"
	if got, _ := r.Get("node-a"); got.NodeID != "node-a" {
		t.Errorf("修改 List 返回值污染了内部状态: %+v", got)
	}
}

// TestCount 验证计数（含 Offline 节点）。
func TestCount(t *testing.T) {
	r := New()
	if r.Count() != 0 {
		t.Fatalf("空注册表 Count = %d，期望 0", r.Count())
	}
	r.Register(sampleInfo("node-1"))
	r.Register(sampleInfo("node-2"))
	r.MarkOffline("node-1")
	if r.Count() != 2 {
		t.Errorf("Count = %d，期望 2（含 Offline）", r.Count())
	}
}

// TestConcurrentAccess 验证并发读写安全（配合 -race 检测数据竞争）。
func TestConcurrentAccess(t *testing.T) {
	r := New()
	const workers = 8
	const perWorker = 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := "node-" + string(rune('a'+w))
				switch i % 4 {
				case 0:
					r.Register(sampleInfo(id))
				case 1:
					r.UpdateHeartbeat(id, time.Now().UnixMilli())
				case 2:
					r.MarkOffline(id)
				default:
					_, _ = r.Get(id)
					_ = r.List()
					_ = r.Count()
				}
			}
		}(w)
	}
	wg.Wait()

	// 全部注册后每个 worker 一个节点
	if r.Count() != workers {
		t.Errorf("并发注册后 Count = %d，期望 %d", r.Count(), workers)
	}
}
