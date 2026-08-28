// v0.23.0 P2 回归测试：PRT-19（rebuildSubscription 锁外清理）、
// PRT-20（Stop 置 nil 与订阅循环 PubAck 竞态）。
//
// 只新增本文件，不改任何既有测试（opcua_mapper_test.go / mapper_exit_test.go）。
package opcua

import (
	"context"
	"net"
	"testing"
	"time"

	opcuapkg "edgeflow/pkg/opcua"
	"edgeflow/pkg/opcuasim"
)

// silentDropListener 返回一个"接受连接但永不响应"的 TCP 监听器
// （黑洞端点：客户端握手写出后读响应将一直等到超时）。
// 返回端点地址；测试结束经 t.Cleanup 关闭全部连接。
func silentDropListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("黑洞监听器启动失败: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				<-done // 吞掉连接：不读不写，客户端读应答将超时
				_ = c.Close()
			}(c)
		}
	}()
	t.Cleanup(func() {
		close(done)
		_ = ln.Close()
	})
	return "opc.tcp://" + ln.Addr().String()
}

// startSubscribedMapper 起模拟器 + 订阅模式 Mapper，等待首拍数据进缓存。
// 用 500ms 小超时（非 startMapper 的默认 5s）：客户端 Close 对活会话的
// DeleteSubscriptions 步会等满一个 client.timeout 才返回（既有 pkg/opcua
// 与 pkg/opcuasim 交互行为，本 worker 域外），小超时让 Stop 相关看门狗
// （3s）成立且不掩盖回归。
func startSubscribedMapper(t *testing.T) (*OPCUAMapper, *opcuasim.Simulator) {
	t.Helper()
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithStep(20*time.Millisecond), opcuasim.WithSeed(1))
	if err := sim.Start(); err != nil {
		t.Fatalf("模拟器启动失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	m, err := New("opc.tcp://"+sim.Addr(), WithPoints(simPoints), WithTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(context.TODO()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	if err := m.StartSubscription(); err != nil {
		t.Fatalf("StartSubscription: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		n := len(m.subValues)
		m.mu.Unlock()
		if n > 0 {
			return m, sim
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待订阅首拍数据超时")
	return nil, nil
}

// TestRebuildSubscriptionDeadClientReleasesLock（PRT-19）：
// 旧 client 已死（模拟器停止→连接被服务端关闭）时，rebuildSubscription
// 必须（a）快速返回不持锁悬挂——并发 Collect 在看门狗内完成；
// （b）锁内只换出指针，锁外异步清理死连接；（c）缓存复位、状态收敛。
func TestRebuildSubscriptionDeadClientReleasesLock(t *testing.T) {
	m, sim := startSubscribedMapper(t)

	// 制造死 client：先借真连接（模拟器存活时 Open 成功），再停模拟器
	// 杀掉 TCP（PRT-23 Stop 会关闭全部存活连接）。
	m.mu.Lock()
	dead := m.client
	m.mu.Unlock()
	if dead == nil {
		t.Fatal("订阅 Mapper 应已持有 client")
	}
	_ = sim.Stop() // 连接死亡；mapper.endpoint 指向已关闭端口 → 重连必败

	// 并发 Collect 看门狗：若 rebuild 仍持锁做逐协议清理（旧行为），
	// Collect 将被 m.mu 阻塞至清理结束。
	collectDone := make(chan error, 1)
	go func() {
		_, err := m.Collect()
		collectDone <- err
	}()

	start := time.Now()
	m.rebuildSubscription()
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("rebuildSubscription 耗时 %v（>2s）：死连接清理疑似仍持锁（PRT-19 回归）", elapsed)
	}

	select {
	case <-collectDone:
	case <-time.After(3 * time.Second):
		t.Fatal("rebuild 期间并发 Collect 3s 未完成：锁被悬挂（PRT-19 回归）")
	}

	// 缓存断言（最终一致）：pubCh 缓冲中的合法通知会被旧循环短暂重填
	// （新数据，非陈旧值）；旧循环退出后其自身 rebuild 再次复位，此后
	// 无写入者——轮询等待收敛为空。
	emptyDeadline := time.Now().Add(3 * time.Second)
	nilClient := false
	for {
		m.mu.Lock()
		cacheLen := len(m.subValues)
		nilClient = m.client == nil
		m.mu.Unlock()
		if cacheLen == 0 {
			break
		}
		if time.Now().After(emptyDeadline) {
			t.Fatalf("重建+循环退出后订阅缓存仍未复位（剩余 %d 项）", cacheLen)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !nilClient {
		t.Fatal("重建失败（端点已死）后 m.client 应为 nil，操作时再重试")
	}
}

// TestRebuildSubscriptionLockFreedDuringBlackHoleDial（PRT-19 锁释放断言）：
// 死 client + 黑洞端点（接受连接但不回包，Dial 握手等满 DefaultDialTimeout
// 10s）时，新实现的锁内段只做指针换出+缓存复位，随后的异步清理与拨号
// 都不持锁——主测试应能在 2s 内取得 m.mu；旧实现持锁逐协议清理+持锁
// 拨号会让 m.mu 长达约 10s 不可用（并发 Collect/HandleCommand 全堵）。
// 说明：拨号阶段固定 10s 超时是 pkg/opcua 传输层既有语义（本 worker
// 不越界改动），本测试只断言锁不被长期占用。
func TestRebuildSubscriptionLockFreedDuringBlackHoleDial(t *testing.T) {
	blackHole := silentDropListener(t)
	m, sim := startSubscribedMapper(t)

	m.mu.Lock()
	if m.client == nil {
		m.mu.Unlock()
		t.Fatal("订阅 Mapper 应已持有 client")
	}
	m.endpoint = blackHole // 重定向端点：重建拨号将落入黑洞
	m.mu.Unlock()
	_ = sim.Stop() // 死 client（RST），重建路径先清理再拨黑洞

	rebuilt := make(chan struct{})
	go func() { m.rebuildSubscription(); close(rebuilt) }()

	gotLock := make(chan struct{})
	go func() {
		m.mu.Lock()
		close(gotLock)
		m.mu.Unlock()
	}()
	select {
	case <-gotLock:
	case <-time.After(2 * time.Second):
		t.Fatal("rebuild 期间 m.mu 被长期占用（疑似持锁清理/拨号，PRT-19 回归）")
	}
	select {
	case <-rebuilt:
	case <-time.After(15 * time.Second):
		t.Fatal("黑洞端点下 rebuild 未在预算内返回（>15s）")
	}
	m.mu.Lock()
	nilClient := m.client == nil
	m.mu.Unlock()
	if !nilClient {
		t.Fatal("重建失败（黑洞）后 m.client 应为 nil")
	}
}

// TestStopDuringSubscriptionLoopNoPanic（PRT-20）：
// 订阅循环活跃期间调用 Stop（置 nil client）不得触发 nil 解引用 panic
// （旧实现循环体不持锁读 m.client 调 PubAck，与置 nil 竞态）；
// Stop 后状态收敛，后续 Collect 不悬挂。-race 下运行可捕获数据竞争。
func TestStopDuringSubscriptionLoopNoPanic(t *testing.T) {
	m, _ := startSubscribedMapper(t)

	stopDone := make(chan error, 1)
	go func() { stopDone <- m.Stop() }()

	// Stop 进行中并发触发一次 Collect：旧实现持锁 Close 最坏 5s 且与
	// 置 nil 竞态；新实现 Close 在锁外，Collect 等锁时间有界。
	collectDone := make(chan struct{})
	go func() {
		_, _ = m.Collect()
		close(collectDone)
	}()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 3s 未完成")
	}
	select {
	case <-collectDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 期间并发 Collect 3s 未完成（疑似锁内 Close 阻塞）")
	}

	m.mu.Lock()
	stopped := !m.started && !m.subOn
	m.mu.Unlock()
	// 注：不在此断言 m.client==nil——并发 Collect 的自动重连（既有语义）
	// 会合法回填 client；Stop 自身的置 nil 行为由状态断言与 -race 覆盖。
	if !stopped {
		t.Fatal("Stop 后 started/subOn 应为 false")
	}
	// 循环体若在 Stop 后仍引用旧 client，PubAck 对已关连接报错并被吞掉，
	// 不应 panic（panic 会使整个测试进程崩溃，即为本测试失败）。
}

// TestSubscriptionLoopPubAckSnapshotClosedClient（PRT-20 单元级）：
// 循环体 PubAck 必须经锁内快照取 client——client 为已关闭连接时
// PubAck 报错被吞、循环正常退出；client 为 nil 时直接跳过，均不 panic。
func TestSubscriptionLoopPubAckSnapshotClosedClient(t *testing.T) {
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithStep(20*time.Millisecond), opcuasim.WithSeed(7))
	if err := sim.Start(); err != nil {
		t.Fatalf("模拟器启动失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })

	t.Run("closedClient", func(t *testing.T) {
		c, err := opcuapkg.Open("opc.tcp://"+sim.Addr(), time.Second)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		m := &OPCUAMapper{deviceName: "prt20-closed", subValues: make(map[string]float64), client: c}
		ch := make(chan opcuapkg.PublishResult, 1)
		ch <- opcuapkg.PublishResult{KeepAlive: true}
		close(ch) // 触发一次 PubAck 快照路径后经 range 关闭退出
		done := make(chan struct{})
		go func() { m.subscriptionLoop(ch); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("subscriptionLoop 未退出")
		}
	})
	t.Run("nilClient", func(t *testing.T) {
		m := &OPCUAMapper{deviceName: "prt20-nil", subValues: make(map[string]float64)}
		ch := make(chan opcuapkg.PublishResult, 1)
		ch <- opcuapkg.PublishResult{KeepAlive: true}
		close(ch)
		done := make(chan struct{})
		go func() { m.subscriptionLoop(ch); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("subscriptionLoop 未退出")
		}
	})
}

// TestStopNilClientCollectErrors（PRT-20 行为明示）：
// Stop 后轮询路径不再自动重连——旧实现同样置 nil，但重连只发生在
// withClient；此测试钉住"Stop 后 Collect 快速报错"语义（不悬挂）。
func TestStopNilClientCollectErrors(t *testing.T) {
	m, sim := startSubscribedMapper(t)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_ = sim.Stop() // 设备也下线：排除"Stop 后 Collect 自动重连成功"的既有语义
	done := make(chan error, 1)
	go func() {
		_, err := m.Collect()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Stop 且设备下线后 Collect 应报错")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 后 Collect 悬挂")
	}
}
