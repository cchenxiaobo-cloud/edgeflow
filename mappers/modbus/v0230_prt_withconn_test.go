// v0.23.0 P2 回归测试：PRT-21（withConn 无整体时间预算）。
//
// 只新增本文件，不改任何既有测试（modbus_mapper_test.go / modbus_namespace_test.go）。
package modbus

import (
	"net"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/modbussim"
)

// newBudgetMapper 构造未启动的 Mapper（不走 Start 预连接），小超时便于
// 快速验证预算行为。
func newBudgetMapper(t *testing.T, addr string, timeout time.Duration) *ModbusMapper {
	t.Helper()
	m := New(addr, WithTimeout(timeout))
	return m
}

// silentDropListener 返回一个"接受连接但永不响应"的 TCP 监听器：
// 客户端写出请求后读应答将一直等到单步超时（goburrow handler.Timeout）。
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
				<-done // 吞连接不回包：对端读应答超时
				_ = c.Close()
			}(c)
		}
	}()
	t.Cleanup(func() {
		close(done)
		_ = ln.Close()
	})
	return ln.Addr().String()
}

// TestWithConnBudgetBlackHole（PRT-21 主断言）：
// 黑洞端点（连接成功但不回包）时，op 单步超时（1×timeout）失败进入
// 重试分支，重试后再次失败——整体耗时被钉在预算 2×timeout 附近
// （重试中单步由 goburrow handler.Timeout 兑现剩余预算）。错误可观测。
// "预算耗尽放弃重试"分支由 TestWithConnBudgetGateExercised 直测。
func TestWithConnBudgetBlackHole(t *testing.T) {
	addr := silentDropListener(t)
	m := newBudgetMapper(t, addr, 300*time.Millisecond)

	start := time.Now()
	err := m.withConn(func() error {
		_, err := m.client.ReadHoldingRegisters(0x0000, 1)
		return err
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("黑洞端点下 withConn 应返回错误")
	}
	// 预算上限 2×timeout=600ms；单步上限 300ms×2 步=600ms 一致。
	// 旧实现无预算门（但本场景恰好被单步上限兜住 ≈600ms）；本测试
	// 钉住整体有界 + 错误可观测（含预算耗尽说明）。
	if elapsed > 2*time.Second {
		t.Fatalf("withConn 耗时 %v（预算 2×timeout=600ms + ε，严重超界）", elapsed)
	}
}

// TestWithConnBudgetRetryBlockedConnRefused（PRT-21 快速失败分支）：
// 首步即连接拒绝（RST 快速失败）→ 重试阶段再次拒绝：预算门不改变
// "重试一次"语义，错误仍须上抛且包含地址。
func TestWithConnBudgetRetryBlockedConnRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("获取空闲端口失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 立即释放 → 无监听

	m := newBudgetMapper(t, addr, 500*time.Millisecond)
	err = m.withConn(func() error {
		_, err := m.client.ReadHoldingRegisters(0x0000, 1)
		return err
	})
	if err == nil {
		t.Fatal("无监听端点 withConn 应返回错误")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Fatalf("错误应包含设备地址: %v", err)
	}
}

// TestWithConnBudgetHappyPathUnaffected（PRT-21 行为保持）：
// 正常设备路径下预算门不改变结果——采集照常成功且台账照常记录。
func TestWithConnBudgetHappyPathUnaffected(t *testing.T) {
	sim := modbussim.New("127.0.0.1:0", modbussim.WithSeed(42), modbussim.WithStep(30*time.Millisecond))
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })

	m := newBudgetMapper(t, sim.Addr(), 2*time.Second)
	props, err := m.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if props["temperature"] == 0 && props["humidity"] == 0 {
		t.Fatalf("采集值异常: %v", props)
	}
}

// TestWithConnBudgetGateExercised（PRT-21 门控行为直测）：
// 注入一个恒失败的 op（模拟"重试阶段开始时预算已耗尽"）：第一次调用
// 即返回传输错误且耗时≥预算——门控必须在重试前放弃。用极小 timeout
// 与 sleep 保证 deadline 先于重试判定过期，断言错误含"放弃重试"且
// 不发生第二次 op 调用（重试被跳过）。
func TestWithConnBudgetGateExercised(t *testing.T) {
	sim := modbussim.New("127.0.0.1:0", modbussim.WithSeed(42), modbussim.WithStep(30*time.Millisecond))
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })

	m := newBudgetMapper(t, sim.Addr(), 30*time.Millisecond) // 预算 60ms
	calls := 0
	start := time.Now()
	err := m.withConn(func() error {
		calls++
		time.Sleep(120 * time.Millisecond) // 单次即耗尽 60ms 预算
		return net.ErrClosed               // 传输类错误（非 ModbusError）→ 触发重试判定
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("恒失败 op 应返回错误")
	}
	if !strings.Contains(err.Error(), "放弃重试") {
		t.Fatalf("预算耗尽应放弃重试并明示: %v", err)
	}
	if calls != 1 {
		t.Fatalf("预算耗尽后不得进入重试（op 调用 %d 次）", calls)
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("op 未实际执行完（耗时 %v）", elapsed)
	}
}
