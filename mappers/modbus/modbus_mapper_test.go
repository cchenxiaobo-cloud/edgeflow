// Modbus Mapper 集成测试（WBS 5.2 验收：双向读写验证 + 台账记录）。
//
// 起真实模拟器（pkg/modbussim，自实现协议帧）→ 用 Mapper（goburrow 客户端）
// 读写 → 验证：
//   - Collect 读温度/湿度（寄存器 0x0000-0x0001）；
//   - HandleCommand 写目标温度（0x0010）/ 写线圈（0x0020-0x0023），
//     写后回读验证写入生效（双向验证）；
//   - 每次操作落台账（真实 SQLite），可查可统计；
//   - 断线重连（设备重启后操作自动恢复）。
package modbus

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goburrow/modbus"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/modbussim"
)

// fakeLedger 是 OpLedger 的内存实现（单测用，避免依赖 SQLite）。
type fakeLedger struct {
	ops []metamanager.OpRecord
}

func (f *fakeLedger) SaveOp(rec metamanager.OpRecord) error {
	f.ops = append(f.ops, rec)
	return nil
}

// newTestEnv 起模拟器（短波动周期）并构造装配了台账的 Mapper。
func newTestEnv(t *testing.T) (*modbussim.Simulator, *ModbusMapper, *metamanager.Ledger, *metamanager.Store) {
	t.Helper()
	sim := modbussim.New("127.0.0.1:0", modbussim.WithSeed(42), modbussim.WithStep(30*time.Millisecond))
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })

	store, err := metamanager.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("打开台账 DB 失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ledger, err := metamanager.NewLedger(store)
	if err != nil {
		t.Fatalf("NewLedger 失败: %v", err)
	}

	m := New(sim.Addr(), WithLedger(ledger), WithTimeout(2*time.Second))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	return sim, m, ledger, store
}

// waitUntil 轮询等待 cond 成立，超时返回 false。
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestCollectReadsTemperatureHumidity Collect 读温度/湿度（0x0000-0x0001），
// 值域合法且随模拟器波动；台账记录方向 up。
func TestCollectReadsTemperatureHumidity(t *testing.T) {
	_, m, ledger, _ := newTestEnv(t)

	props, err := m.Collect()
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if len(props) != 2 {
		t.Fatalf("属性数 = %d，期望 2: %v", len(props), props)
	}
	if props["temperature"] < 0 || props["temperature"] > 100 {
		t.Errorf("温度 = %.1f，期望 [0,100]", props["temperature"])
	}
	if props["humidity"] < 0 || props["humidity"] > 100 {
		t.Errorf("湿度 = %.1f，期望 [0,100]", props["humidity"])
	}

	// 台账：1 条 up 记录，地址 0x0000-0x0001，结果 ok
	ops, err := ledger.ListOps(metamanager.OpFilter{})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("台账记录数 = %d，期望 1", len(ops))
	}
	op := ops[0]
	if op.Direction != metamanager.DirUp || op.RegAddr != "0x0000-0x0001" || op.Result != "ok" {
		t.Errorf("台账记录不符: %+v", op)
	}
	if !strings.Contains(op.Value, "/") {
		t.Errorf("台账值应为 原始温度/原始湿度 格式: %q", op.Value)
	}
}

// TestHandleCommandTargetTempWriteReadback 写目标温度（0x0010）：
// 回读验证寄存器值 == 写入值（双向验证）；模拟器温度随后向新目标收敛；
// 台账记录方向 down 且含回读验证消息。
func TestHandleCommandTargetTempWriteReadback(t *testing.T) {
	sim, m, ledger, _ := newTestEnv(t)

	report, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Namespace: "default",
		Property: "targetTemp", Value: 30.5,
	})
	if err != nil {
		t.Fatalf("写目标温度指令失败: %v", err)
	}
	if report.DeviceName != "mb-sensor-01" || len(report.Properties) != 2 {
		t.Errorf("快照不符: %+v", report)
	}

	// 回读验证：寄存器 0x0010 原始值 = 305（30.5°C）
	raw := readReg(t, sim, 0x0010)
	if raw != 305 {
		t.Errorf("寄存器 0x0010 = %d，期望 305（30.5°C）", raw)
	}

	// 温度向 30.5°C 收敛（波动周期 30ms）
	if !waitUntil(t, 5*time.Second, func() bool {
		return sim.Temp() > 29.5 && sim.Temp() <= 31.5
	}) {
		t.Fatalf("温度未收敛到 30.5 附近，当前 %.2f", sim.Temp())
	}

	// 台账：down 记录，地址 0x0010，值 305，结果 ok，含回读验证
	downs, err := ledger.ListOps(metamanager.OpFilter{Direction: metamanager.DirDown})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(downs) != 1 {
		t.Fatalf("down 记录数 = %d，期望 1", len(downs))
	}
	op := downs[0]
	if op.RegAddr != "0x0010" || op.Value != "305" || op.Result != "ok" {
		t.Errorf("台账 down 记录不符: %+v", op)
	}
	if !strings.Contains(op.Message, "回读验证一致") {
		t.Errorf("台账消息缺少回读验证说明: %q", op.Message)
	}
}

// TestHandleCommandCoilWriteReadback 写线圈（coil2 → 0x0022）：
// 回读验证线圈状态翻转（ON/OFF 双向）；台账含 coil 地址。
func TestHandleCommandCoilWriteReadback(t *testing.T) {
	sim, m, ledger, _ := newTestEnv(t)

	// coil2 = 1（ON）
	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Property: "coil2", Value: 1,
	}); err != nil {
		t.Fatalf("写 coil2=1 失败: %v", err)
	}
	// 回读验证：线圈 0x0022 为 ON（位图 bit2）
	rb := readCoils(t, sim, 0x0020, 4)
	if rb&0x04 == 0 {
		t.Errorf("线圈位图 = %#x，期望 bit2 置位（coil2 ON）", rb)
	}
	// coil2 = 0（OFF）
	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Property: "coil2", Value: 0,
	}); err != nil {
		t.Fatalf("写 coil2=0 失败: %v", err)
	}
	rb = readCoils(t, sim, 0x0020, 4)
	if rb&0x04 != 0 {
		t.Errorf("线圈位图 = %#x，期望 bit2 清零（coil2 OFF）", rb)
	}

	// 台账：2 条 down 记录，地址 coil:0x0022
	downs, err := ledger.ListOps(metamanager.OpFilter{Direction: metamanager.DirDown})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(downs) != 2 {
		t.Fatalf("down 记录数 = %d，期望 2", len(downs))
	}
	for _, op := range downs {
		if op.RegAddr != "coil:0x0022" || op.Result != "ok" {
			t.Errorf("台账线圈记录不符: %+v", op)
		}
	}
}

// TestHandleCommandInvalidTargetTemp 越界目标温度被拒绝且台账记 error。
func TestHandleCommandInvalidTargetTemp(t *testing.T) {
	_, m, ledger, _ := newTestEnv(t)

	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Property: "targetTemp", Value: 200,
	}); err == nil {
		t.Fatal("targetTemp=200 应返回错误")
	}
	ops, err := ledger.ListOps(metamanager.OpFilter{Direction: metamanager.DirDown})
	if err != nil {
		t.Fatalf("ListOps 失败: %v", err)
	}
	if len(ops) != 1 || ops[0].Result != "error" {
		t.Errorf("越界指令应记录 error 台账: %+v", ops)
	}
}

// TestHandleCommandUnknownProperty 未知属性/设备名不符返回错误。
func TestHandleCommandUnknownProperty(t *testing.T) {
	_, m, _, _ := newTestEnv(t)

	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Property: "fanSpeed", Value: 1,
	}); err == nil {
		t.Error("未知属性应返回错误")
	}
	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "other-device", Property: "targetTemp", Value: 25,
	}); err == nil {
		t.Error("设备名不符应返回错误")
	}
	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Property: "coil9", Value: 1,
	}); err == nil {
		t.Error("越界线圈应返回错误")
	}
}

// TestReconnectAfterDeviceRestart 设备重启（模拟器 Stop 再在同端口重启）后，
// 操作自动恢复（断线重连：Collect 失败 → 重连 → 成功）。
func TestReconnectAfterDeviceRestart(t *testing.T) {
	sim := modbussim.New("127.0.0.1:0", modbussim.WithSeed(7), modbussim.WithStep(50*time.Millisecond))
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	addr := sim.Addr()
	fake := &fakeLedger{}
	m := New(addr, WithLedger(fake), WithTimeout(1*time.Second))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = m.Stop() }()

	// 第一轮采集成功
	if _, err := m.Collect(); err != nil {
		t.Fatalf("首轮 Collect 失败: %v", err)
	}
	// 模拟设备重启：停止监听，再在同一地址重启
	if err := sim.Stop(); err != nil {
		t.Fatalf("Stop 模拟器失败: %v", err)
	}
	sim2 := modbussim.New(addr, modbussim.WithSeed(8), modbussim.WithStep(50*time.Millisecond))
	if err := sim2.Start(); err != nil {
		t.Fatalf("重启模拟器失败: %v", err)
	}
	defer func() { _ = sim2.Stop() }()

	// 等设备端口恢复监听后重试：最终应成功（重连路径）
	if !waitUntil(t, 5*time.Second, func() bool {
		_, err := m.Collect()
		return err == nil
	}) {
		t.Fatal("设备重启后 Collect 未能自动恢复")
	}
	// 写入也应恢复（目标温度 0x0010）
	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Property: "targetTemp", Value: 26.0,
	}); err != nil {
		t.Fatalf("重启后写指令失败: %v", err)
	}
	if got := readReg(t, sim2, 0x0010); got != 260 {
		t.Errorf("重启后写 0x0010 = %d，期望 260", got)
	}
}

// TestCollectConnectionRefused 设备不可达：Collect 返回明确错误且台账记 error。
func TestCollectConnectionRefused(t *testing.T) {
	fake := &fakeLedger{}
	// 用本机一个必然无人监听的端口（先占一个端口再释放）
	ln, err := listenEphemeral()
	if err != nil {
		t.Fatalf("获取空闲端口失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 立即释放 → 端口无监听

	m := New(addr, WithLedger(fake), WithTimeout(1*time.Second))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = m.Stop() }()

	_, err = m.Collect()
	if err == nil {
		t.Fatal("设备不可达时 Collect 应返回错误")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("错误应包含设备地址: %v", err)
	}
	if len(fake.ops) != 1 || fake.ops[0].Result != "error" || fake.ops[0].Direction != metamanager.DirUp {
		t.Errorf("失败操作应记录 error 台账: %+v", fake.ops)
	}
}

// TestNoLedgerNoPanic 未装配台账时读写照常（nil ledger 安全）。
func TestNoLedgerNoPanic(t *testing.T) {
	sim := modbussim.New("127.0.0.1:0", modbussim.WithSeed(1))
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	defer func() { _ = sim.Stop() }()
	m := New(sim.Addr()) // 无 WithLedger
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = m.Stop() }()
	if _, err := m.Collect(); err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Property: "targetTemp", Value: 25,
	}); err != nil {
		t.Fatalf("HandleCommand 失败: %v", err)
	}
}

// readReg 直连模拟器读单寄存器原始值（测试辅助，不经 Mapper）。
func readReg(t *testing.T, sim *modbussim.Simulator, addr uint16) uint16 {
	t.Helper()
	results, err := modbusRawRead(sim.Addr(), addr, 1)
	if err != nil {
		t.Fatalf("读寄存器 0x%04X 失败: %v", addr, err)
	}
	return uint16(results[0])<<8 | uint16(results[1])
}

// readCoils 直连模拟器读线圈位图（测试辅助，不经 Mapper）。
func readCoils(t *testing.T, sim *modbussim.Simulator, start, qty uint16) byte {
	t.Helper()
	results, err := modbusRawCoils(sim.Addr(), start, qty)
	if err != nil {
		t.Fatalf("读线圈失败: %v", err)
	}
	return results[0]
}

// modbusRawRead 用 goburrow 客户端直读保持寄存器（测试辅助）。
func modbusRawRead(addr string, reg, qty uint16) ([]byte, error) {
	handler := modbus.NewTCPClientHandler(addr)
	handler.Timeout = 2 * time.Second
	defer func() { _ = handler.Close() }()
	return modbus.NewClient(handler).ReadHoldingRegisters(reg, qty)
}

// modbusRawCoils 用 goburrow 客户端直读线圈（测试辅助）。
func modbusRawCoils(addr string, start, qty uint16) ([]byte, error) {
	handler := modbus.NewTCPClientHandler(addr)
	handler.Timeout = 2 * time.Second
	defer func() { _ = handler.Close() }()
	return modbus.NewClient(handler).ReadCoils(start, qty)
}

// listenEphemeral 占一个随机端口并立即释放（构造"无监听端口"）。
func listenEphemeral() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}
