package nodecontroller

import (
	"sync"
	"testing"
	"time"

	"edgeflow/cloud/pkg/registry"
)

// 时间注入方案说明：
// 采用"可注入 now func"而非"把 interval 缩到毫秒级"做超时测试——
// 超时判定只依赖 scanOnce 里的 now（与注册表时间戳比较），注入假时钟
// 可以让"心跳停滞 200s"在零等待内完成断言，确定性最好；真实时钟只
// 用于 Start/Stop 生命周期测试（interval 缩到 10ms，等待可接受）。
// 注册表时间戳用 UpdateHeartbeat 的显式 ts 参数写入假时钟时间，
// 避免真实 time.Now 混入导致基准错乱。

// fakeClock 是可推进的假时钟（并发安全）。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// Now 返回当前假时间。
func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Advance 把假时间前进 d。
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// newController 创建注入假时钟的控制器（timeout 默认 180s）。
func newController(reg *registry.Registry, clock *fakeClock) *NodeController {
	return New(reg, WithTimeout(180*time.Second), WithNow(clock.Now))
}

// TestScanOnceHeartbeatHealthy 验证心跳正常（间隔小于 timeout）不误判。
func TestScanOnceHeartbeatHealthy(t *testing.T) {
	reg := registry.New()
	clock := &fakeClock{t: time.Now()}
	c := newController(reg, clock)

	// 两个节点心跳都健康：一个刚心跳，一个 10s 前心跳（< 180s）
	reg.Register(registry.NodeInfo{NodeID: "n1"})
	reg.Register(registry.NodeInfo{NodeID: "n2"})
	reg.UpdateHeartbeat("n1", clock.Now().UnixMilli())
	reg.UpdateHeartbeat("n2", clock.Now().Add(-10*time.Second).UnixMilli())

	// 时间前进 30s（一个扫描周期），仍远小于 timeout
	clock.Advance(30 * time.Second)
	c.scanOnce()

	for _, id := range []string{"n1", "n2"} {
		info, ok := reg.Get(id)
		if !ok {
			t.Fatalf("节点 %s 应存在", id)
		}
		if info.Status != registry.StatusReady {
			t.Errorf("节点 %s 心跳正常却被误判为 %s，期望 Ready", id, info.Status)
		}
	}
}

// TestScanOnceStaleMarkOffline 验证心跳停滞超过 timeout 被标记 Offline。
func TestScanOnceStaleMarkOffline(t *testing.T) {
	reg := registry.New()
	clock := &fakeClock{t: time.Now()}
	c := newController(reg, clock)

	// n1 心跳停滞 200s（> 180s timeout）；n2 心跳健康（对照组）
	reg.Register(registry.NodeInfo{NodeID: "n1"})
	reg.Register(registry.NodeInfo{NodeID: "n2"})
	reg.UpdateHeartbeat("n1", clock.Now().Add(-200*time.Second).UnixMilli())
	reg.UpdateHeartbeat("n2", clock.Now().UnixMilli())

	c.scanOnce()

	if info, _ := reg.Get("n1"); info.Status != registry.StatusOffline {
		t.Errorf("停滞节点 n1 应被标记 Offline，实际 %s", info.Status)
	}
	if info, _ := reg.Get("n2"); info.Status != registry.StatusReady {
		t.Errorf("健康节点 n2 不应被误判，实际 %s", info.Status)
	}
}

// TestScanOnceStaleAlreadyOffline 验证已 Offline 节点重复扫描：
// 状态不变（不重复标记、不重复告警），且不影响其他节点。
func TestScanOnceStaleAlreadyOffline(t *testing.T) {
	reg := registry.New()
	clock := &fakeClock{t: time.Now()}
	c := newController(reg, clock)

	reg.Register(registry.NodeInfo{NodeID: "n1"})
	reg.UpdateHeartbeat("n1", clock.Now().Add(-200*time.Second).UnixMilli())
	c.scanOnce() // 第一次：n1 → Offline

	// 再次扫描（模拟下一周期）：n1 已 Offline，应被跳过；n2 健康不受影响
	reg.Register(registry.NodeInfo{NodeID: "n2"})
	reg.UpdateHeartbeat("n2", clock.Now().UnixMilli())
	clock.Advance(10 * time.Second)
	c.scanOnce()

	if info, _ := reg.Get("n1"); info.Status != registry.StatusOffline {
		t.Errorf("已 Offline 节点 n1 状态不应变化，实际 %s", info.Status)
	}
	if info, _ := reg.Get("n2"); info.Status != registry.StatusReady {
		t.Errorf("健康节点 n2 不应被误判，实际 %s", info.Status)
	}
}

// TestRecoverAfterHeartbeat 验证状态机闭环：
// Offline（静默超时）→ 重新心跳（UpdateHeartbeat）→ Ready 自动恢复。
func TestRecoverAfterHeartbeat(t *testing.T) {
	reg := registry.New()
	clock := &fakeClock{t: time.Now()}
	c := newController(reg, clock)

	reg.Register(registry.NodeInfo{NodeID: "n1"})
	reg.UpdateHeartbeat("n1", clock.Now().Add(-200*time.Second).UnixMilli())
	c.scanOnce()
	if info, _ := reg.Get("n1"); info.Status != registry.StatusOffline {
		t.Fatalf("前置条件不成立：n1 应为 Offline，实际 %s", info.Status)
	}

	// 节点恢复心跳：时间戳刷新为当前
	clock.Advance(300 * time.Second) // 又过了 5 分钟
	reg.UpdateHeartbeat("n1", clock.Now().UnixMilli())
	c.scanOnce()

	info, ok := reg.Get("n1")
	if !ok {
		t.Fatal("n1 应存在")
	}
	if info.Status != registry.StatusReady {
		t.Errorf("Offline 节点恢复心跳后应回到 Ready，实际 %s", info.Status)
	}
	if info.LastHeartbeatAt != clock.Now().UnixMilli() {
		t.Errorf("LastHeartbeatAt 应刷新为最新心跳时间，实际 %d", info.LastHeartbeatAt)
	}
}

// TestStartStopLifecycle 验证 Start/Stop：
//   - Start 后按周期扫描，停滞节点被标记 Offline
//   - Stop 后扫描停止，恢复心跳的节点不再被扫描触碰（保持 Ready）
func TestStartStopLifecycle(t *testing.T) {
	reg := registry.New()
	// 生命周期测试用真实时钟：interval 缩到 10ms、timeout 1s
	c := New(reg, WithInterval(10*time.Millisecond), WithTimeout(1*time.Second))

	// 注册即"停滞"：把心跳时间拨回 2s（> 1s timeout），首个周期就会被标记
	reg.Register(registry.NodeInfo{NodeID: "n1"})
	reg.UpdateHeartbeat("n1", time.Now().Add(-2*time.Second).UnixMilli())

	c.Start()
	defer c.Stop()

	// 轮询等待扫描生效：n1 应在 1s 内变 Offline
	deadline := time.Now().Add(2 * time.Second)
	for {
		info, _ := reg.Get("n1")
		if info.Status == registry.StatusOffline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Start 后扫描未在超时时间内把停滞节点标记 Offline")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Stop：节点恢复心跳后，不再有扫描进程会再次标记/触碰它
	reg.UpdateHeartbeat("n1", time.Now().UnixMilli())
	c.Stop()
	// 等待远超 2 个扫描周期，确认扫描确实停止
	time.Sleep(100 * time.Millisecond)

	info, _ := reg.Get("n1")
	if info.Status != registry.StatusReady {
		t.Errorf("Stop 后节点应保持 Ready，实际 %s（扫描未停止）", info.Status)
	}
}

// TestStopIdempotent 验证 Stop 幂等（未 Start 直接 Stop 不 panic）。
func TestStopIdempotent(t *testing.T) {
	reg := registry.New()
	c := New(reg)
	c.Stop() // 未 Start：应直接返回
	c.Start()
	c.Stop()
	c.Stop() // 重复 Stop：应直接返回
}

// TestDurationsFromEnv 验证环境变量解析：默认值 / Go duration / 秒数 / 非法值。
func TestDurationsFromEnv(t *testing.T) {
	// 未设置：默认 30s / 180s
	t.Setenv(EnvScanInterval, "")
	t.Setenv(EnvTimeout, "")
	interval, timeout, err := DurationsFromEnv()
	if err != nil {
		t.Fatalf("默认值解析不应报错: %v", err)
	}
	if interval != DefaultScanInterval || timeout != DefaultTimeout {
		t.Errorf("默认值不符: interval=%v timeout=%v", interval, timeout)
	}

	// Go duration 格式
	t.Setenv(EnvScanInterval, "5s")
	t.Setenv(EnvTimeout, "1m30s")
	interval, timeout, err = DurationsFromEnv()
	if err != nil {
		t.Fatalf("Go duration 解析不应报错: %v", err)
	}
	if interval != 5*time.Second || timeout != 90*time.Second {
		t.Errorf("Go duration 解析不符: interval=%v timeout=%v", interval, timeout)
	}

	// 纯秒数格式
	t.Setenv(EnvScanInterval, "10")
	t.Setenv(EnvTimeout, "180")
	interval, timeout, err = DurationsFromEnv()
	if err != nil {
		t.Fatalf("秒数解析不应报错: %v", err)
	}
	if interval != 10*time.Second || timeout != 180*time.Second {
		t.Errorf("秒数解析不符: interval=%v timeout=%v", interval, timeout)
	}

	// 非法值：报错
	t.Setenv(EnvScanInterval, "abc")
	t.Setenv(EnvTimeout, "")
	if _, _, err := DurationsFromEnv(); err == nil {
		t.Error("非法 interval 应报错")
	}
	t.Setenv(EnvScanInterval, "")
	t.Setenv(EnvTimeout, "-5s")
	if _, _, err := DurationsFromEnv(); err == nil {
		t.Error("非正 timeout 应报错")
	}
}
