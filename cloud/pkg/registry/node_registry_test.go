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

// TestMarkOfflineTwiceKeepsClock 验证重复 MarkOffline 不刷新离线时钟
// （复核 M1）：已处于 Offline 的节点再次收到 MarkOffline（心跳超时扫描
// 与连接断开双路径先后触发）时，offlineSince 保持首次离线时刻，
// TTL/GC 自首次离线起算。
func TestMarkOfflineTwiceKeepsClock(t *testing.T) {
	r := New()
	r.Register(sampleInfo("node-1"))
	r.MarkOffline("node-1")
	first, ok := r.offlineSince["node-1"]
	if !ok {
		t.Fatal("首次 MarkOffline 应记录 offlineSince")
	}

	// 再次标记离线：时钟不应被刷新
	r.MarkOffline("node-1")
	second, ok := r.offlineSince["node-1"]
	if !ok {
		t.Fatal("重复 MarkOffline 后 offlineSince 应仍存在")
	}
	if second != first {
		t.Errorf("重复 MarkOffline 不应刷新离线时钟：first=%d second=%d", first, second)
	}

	// 恢复在线后再次离线：应重新记录（新离线周期）
	r.UpdateHeartbeat("node-1", time.Now().UnixMilli())
	if _, ok := r.offlineSince["node-1"]; ok {
		t.Error("恢复在线应清除 offlineSince")
	}
	r.MarkOffline("node-1")
	third, ok := r.offlineSince["node-1"]
	if !ok {
		t.Fatal("重新离线应重新记录 offlineSince")
	}
	if third < second {
		t.Errorf("新离线周期时间戳不应小于旧值：second=%d third=%d", second, third)
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

// TestOfflineTTLGC 验证 TTL/GC 核心语义（CODE-REVIEW-M1B P2-3）：
// 过期 Offline 节点被清理、未过期 Offline 节点保留、Ready 节点不误删。
func TestOfflineTTLGC(t *testing.T) {
	r := New() // 默认 TTL 24h
	now := time.Now().UnixMilli()
	const ttlMs = int64(OfflineTTLDefault / time.Millisecond)

	r.Register(sampleInfo("expired"))
	r.Register(sampleInfo("fresh"))
	r.Register(sampleInfo("ready"))
	r.UpdateHeartbeat("ready", now) // 保持 Ready
	r.MarkOffline("expired")
	r.MarkOffline("fresh")

	// 白盒改写离线时间戳：expired 超过 TTL，fresh 未超过
	r.offlineSince["expired"] = now - ttlMs - 1
	r.offlineSince["fresh"] = now - ttlMs + 1

	r.gcLocked(now)

	if _, ok := r.Get("expired"); ok {
		t.Error("超过 TTL 的 Offline 节点应被 GC 清理")
	}
	if got, ok := r.Get("fresh"); !ok || got.Status != StatusOffline {
		t.Errorf("未超过 TTL 的 Offline 节点应保留且状态 Offline，实际 ok=%v status=%v", ok, got.Status)
	}
	if got, ok := r.Get("ready"); !ok || got.Status != StatusReady {
		t.Errorf("Ready 节点不应被 GC 误删，实际 ok=%v status=%v", ok, got.Status)
	}
	// 边界：恰好等于 TTL 也算过期（>= 语义）
	r.Register(sampleInfo("expired2"))
	r.MarkOffline("expired2")
	r.offlineSince["expired2"] = now - ttlMs
	r.gcLocked(now)
	if _, ok := r.Get("expired2"); ok {
		t.Error("离线时长恰等于 TTL 的节点应被清理（>= 语义）")
	}
}

// TestLazyGCOnWrite 验证惰性清理：任意写操作会顺带清理过期 Offline 节点，
// 不需要外部定时器；同时验证重连保留 RegisteredAt 的既有语义不受影响。
func TestLazyGCOnWrite(t *testing.T) {
	r := New()
	now := time.Now().UnixMilli()
	const ttlMs = int64(OfflineTTLDefault / time.Millisecond)

	r.Register(sampleInfo("old-node"))
	r.Register(sampleInfo("new-node"))
	first, _ := r.Get("old-node")
	r.MarkOffline("old-node")
	r.offlineSince["old-node"] = now - ttlMs - 1 // 已过期

	// 任何一次写操作（这里是新节点注册）都应触发清理
	r.Register(sampleInfo("trigger"))

	if _, ok := r.Get("old-node"); ok {
		t.Error("写操作应触发惰性 GC 清理过期 Offline 节点")
	}
	if r.Count() != 2 { // trigger + new-node
		t.Errorf("GC 后 Count = %d，期望 2", r.Count())
	}

	// 重连语义：过期的 Offline 节点重新注册 = 全新节点，RegisteredAt 重新计；
	// 未过期的 Offline 节点重连保留 RegisteredAt（既有语义，回归锚点）
	r.Register(sampleInfo("old-node"))
	re, _ := r.Get("old-node")
	if re.RegisteredAt != first.RegisteredAt {
		t.Errorf("重新注册不应重置 RegisteredAt（节点尚未被 GC 清除时）：首次 %d，重连后 %d",
			first.RegisteredAt, re.RegisteredAt)
	}
	if re.Status != StatusReady {
		t.Errorf("重新注册后状态应为 Ready，实际 %s", re.Status)
	}
}

// TestTTLDisabled 验证 WithOfflineTTL(0) 禁用 GC：Offline 节点永久保留（旧行为）。
func TestTTLDisabled(t *testing.T) {
	r := New(WithOfflineTTL(0))
	now := time.Now().UnixMilli()
	r.Register(sampleInfo("ghost"))
	r.MarkOffline("ghost")
	r.offlineSince["ghost"] = now - 365*24*3600*1000 // 一年前离线

	r.Register(sampleInfo("trigger")) // 写操作触发 gcLocked，但 ttl=0 应直接返回
	if _, ok := r.Get("ghost"); !ok {
		t.Error("WithOfflineTTL(0) 应禁用 GC，Offline 节点不应被清理")
	}
}

// TestOfflineSinceLifecycle 验证 offlineSince 与节点生命周期同步：
// 心跳/重连清除离线标记（离线节点不会被残留时间戳误删），
// 未注册节点的 MarkOffline 不产生离线记录。
func TestOfflineSinceLifecycle(t *testing.T) {
	r := New()
	now := time.Now().UnixMilli()
	r.Register(sampleInfo("node-1"))
	r.MarkOffline("node-1")
	if _, ok := r.offlineSince["node-1"]; !ok {
		t.Fatal("MarkOffline 应记录离线时间戳")
	}

	// 心跳恢复在线 → 离线标记清除
	r.UpdateHeartbeat("node-1", now)
	if _, ok := r.offlineSince["node-1"]; ok {
		t.Error("心跳恢复在线后应清除离线标记")
	}
	// 即使残留了过期时间戳，Ready 节点也不应被误删
	r.offlineSince["node-1"] = now - int64(OfflineTTLDefault/time.Millisecond) - 1
	r.gcLocked(now)
	if _, ok := r.Get("node-1"); !ok {
		t.Error("Ready 节点即使有残留 offlineSince 也不应被 GC 误删")
	}

	// 未注册节点 MarkOffline：不新增节点、不产生离线记录
	r.MarkOffline("ghost")
	if _, ok := r.offlineSince["ghost"]; ok {
		t.Error("未注册节点 MarkOffline 不应产生离线记录")
	}
}

// TestConcurrentAccessWithGC 验证 TTL/GC 与并发读写共存（配合 -race 检测
// 数据竞争）：写路径触发的惰性 GC 与并发 Get/List/Count 交错执行。
func TestConcurrentAccessWithGC(t *testing.T) {
	r := New(WithOfflineTTL(time.Hour))
	now := time.Now().UnixMilli()
	const workers = 8
	const perWorker = 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := "node-" + string(rune('a'+w))
				switch i % 5 {
				case 0:
					r.Register(sampleInfo(id))
				case 1:
					r.UpdateHeartbeat(id, time.Now().UnixMilli())
				case 2:
					r.MarkOffline(id)
				case 3:
					// 模拟过期节点：持锁改写内部离线时间戳后触发写操作
					r.MarkOffline(id)
					r.mu.Lock()
					r.offlineSince[id] = now - 2*int64(time.Hour/time.Millisecond)
					r.mu.Unlock()
					r.Register(sampleInfo("gc-trigger-" + string(rune('a'+w)) + "-" + string(rune('0'+i%10))))
				default:
					_, _ = r.Get(id)
					_ = r.List()
					_ = r.Count()
				}
			}
		}(w)
	}
	wg.Wait()

	// 所有 gc-trigger-* 节点都在（Ready，不应被误删）
	for w := 0; w < workers; w++ {
		for i := 0; i < perWorker; i++ {
			if i%5 == 3 {
				id := "gc-trigger-" + string(rune('a'+w)) + "-" + string(rune('0'+i%10))
				if _, ok := r.Get(id); !ok {
					t.Fatalf("Ready 节点 %s 不应在并发 GC 中被误删", id)
				}
			}
		}
	}
}
