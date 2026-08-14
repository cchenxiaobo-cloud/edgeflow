// Modbus TCP 模拟器协议测试（WBS 5.2）。
//
// 用裸 TCP 客户端（自组 MBAP+PDU 帧）验证协议正确性，不经过第三方客户端库：
//   - 读线圈/读保持寄存器/写单线圈/写单寄存器/写多寄存器的请求-应答帧；
//   - 异常应答：0x01 非法功能码、0x02 非法数据地址、0x03 非法数据值。
package modbussim

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// rawResp 是一次裸 Modbus TCP 请求的应答（事务 ID / unit ID / 功能码 / 数据）。
type rawResp struct {
	txID  uint16
	unit  byte
	fc    byte
	data  []byte
	frame []byte // 完整 ADU（错误码检查用）
}

// rawRequest 发送一个自组 Modbus TCP 帧并读取应答。
func rawRequest(t *testing.T, addr string, unit byte, fc byte, data []byte) rawResp {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接模拟器失败: %v", err)
	}
	defer func() { _ = conn.Close() }()

	txID := uint16(0x1234)
	adu := make([]byte, 0, 7+1+len(data))
	adu = append(adu, byte(txID>>8), byte(txID), 0, 0, 0, byte(2+len(data)), unit, fc)
	adu = append(adu, data...)
	if _, err := conn.Write(adu); err != nil {
		t.Fatalf("发送请求失败: %v", err)
	}

	frame := make([]byte, 7) // MBAP 头：事务 ID(2) + 协议 ID(2) + 长度(2) + unit ID(1)
	if _, err := io.ReadFull(conn, frame); err != nil {
		t.Fatalf("读取应答失败: %v", err)
	}
	length := int(binary.BigEndian.Uint16(frame[4:6]))
	rest := make([]byte, length-1) // 剩余 = length - unit(1)
	if _, err := io.ReadFull(conn, rest); err != nil {
		t.Fatalf("读取应答数据失败: %v", err)
	}
	full := append(frame, rest...)
	return rawResp{
		txID:  binary.BigEndian.Uint16(frame[0:2]),
		unit:  frame[6],
		fc:    rest[0],
		data:  rest[1:],
		frame: full,
	}
}

// startTestSim 启动一个随机端口模拟器（短波动周期加速收敛验证）并注册清理。
func startTestSim(t *testing.T) *Simulator {
	t.Helper()
	sim := New("127.0.0.1:0", WithSeed(42), WithStep(20*time.Millisecond))
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	return sim
}

// TestReadHoldingRegisters 读保持寄存器 0x0000-0x0001（温度/湿度，10 倍缩放）。
func TestReadHoldingRegisters(t *testing.T) {
	sim := startTestSim(t)
	resp := rawRequest(t, sim.Addr(), 1, 0x03, []byte{0x00, 0x00, 0x00, 0x02})

	if resp.txID != 0x1234 {
		t.Errorf("事务 ID = %#x，期望回显 %#x", resp.txID, 0x1234)
	}
	if resp.unit != 1 {
		t.Errorf("unit ID = %d，期望 1", resp.unit)
	}
	if resp.fc != 0x03 {
		t.Fatalf("功能码 = %#x，期望 0x03", resp.fc)
	}
	// 应答：字节数(1) + 4 字节寄存器值
	if len(resp.data) != 5 {
		t.Fatalf("应答数据长度 = %d，期望 5（字节数+4）", len(resp.data))
	}
	if resp.data[0] != 4 {
		t.Errorf("字节数 = %d，期望 4", resp.data[0])
	}
	temp := float64(binary.BigEndian.Uint16(resp.data[1:3])) / scaleFactor
	hum := float64(binary.BigEndian.Uint16(resp.data[3:5])) / scaleFactor
	// 初始范围：温度 [20,30]，湿度 [40,70]
	if temp < 20 || temp > 30 {
		t.Errorf("温度 = %.1f°C，期望初始范围 [20, 30]", temp)
	}
	if hum < 40 || hum > 70 {
		t.Errorf("湿度 = %.1f%%，期望初始范围 [40, 70]", hum)
	}
}

// TestReadCoils 读线圈 0x0020 起 4 个（初始全 OFF，位打包 LSB 在前）。
func TestReadCoils(t *testing.T) {
	sim := startTestSim(t)
	resp := rawRequest(t, sim.Addr(), 1, 0x01, []byte{0x00, 0x20, 0x00, 0x04})

	if resp.fc != 0x01 {
		t.Fatalf("功能码 = %#x，期望 0x01", resp.fc)
	}
	if len(resp.data) != 2 {
		t.Fatalf("应答数据长度 = %d，期望 2（字节数+1）", len(resp.data))
	}
	if resp.data[0] != 1 {
		t.Errorf("字节数 = %d，期望 1（4 线圈 = 1 字节）", resp.data[0])
	}
	if resp.data[1] != 0x00 {
		t.Errorf("线圈位图 = %#x，期望 0x00（初始全 OFF）", resp.data[1])
	}
}

// TestWriteSingleRegister 写单寄存器 0x0010（目标温度）：
// 应答回显 + 读回验证 + 温度向新目标收敛（双向验证的核心）。
func TestWriteSingleRegister(t *testing.T) {
	sim := startTestSim(t)

	// 写目标温度 30.0°C（原始值 300）
	resp := rawRequest(t, sim.Addr(), 1, 0x06, []byte{0x00, 0x10, 0x01, 0x2C})
	if resp.fc != 0x06 {
		t.Fatalf("功能码 = %#x，期望 0x06（正常应答）", resp.fc)
	}
	// 应答回显请求：地址 + 值
	if len(resp.data) != 4 || resp.data[0] != 0x00 || resp.data[1] != 0x10 ||
		resp.data[2] != 0x01 || resp.data[3] != 0x2C {
		t.Errorf("应答未回显请求: % x", resp.data)
	}
	if sim.TargetTemp() != 30.0 {
		t.Errorf("目标温度 = %.1f，期望 30.0", sim.TargetTemp())
	}

	// 读回 0x0010 验证写入生效
	rb := rawRequest(t, sim.Addr(), 1, 0x03, []byte{0x00, 0x10, 0x00, 0x01})
	if got := binary.BigEndian.Uint16(rb.data[1:3]); got != 300 {
		t.Errorf("读回 0x0010 = %d，期望 300", got)
	}

	// 温度向新目标收敛（波动周期 20ms，等 3s 足够）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v := sim.Temp(); v > 28.5 && v <= 30.5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v := sim.Temp(); v <= 28.5 || v > 30.5 {
		t.Errorf("温度 = %.2f，未收敛到 30.0 附近（期望 (28.5, 30.5]）", v)
	}
}

// TestWriteSingleCoil 写单线圈（0xFF00 ON / 0x0000 OFF）+ 读回验证位图。
func TestWriteSingleCoil(t *testing.T) {
	sim := startTestSim(t)

	// 写线圈 0x0021（coil1）ON
	resp := rawRequest(t, sim.Addr(), 1, 0x05, []byte{0x00, 0x21, 0xFF, 0x00})
	if resp.fc != 0x05 {
		t.Fatalf("功能码 = %#x，期望 0x05", resp.fc)
	}
	if len(resp.data) != 4 || resp.data[2] != 0xFF || resp.data[3] != 0x00 {
		t.Errorf("应答未回显 0xFF00: % x", resp.data)
	}
	// 读回：coil1 应为 ON（位图 bit1）
	rb := rawRequest(t, sim.Addr(), 1, 0x01, []byte{0x00, 0x20, 0x00, 0x04})
	if rb.data[1] != 0x02 {
		t.Errorf("线圈位图 = %#x，期望 0x02（仅 coil1 ON）", rb.data[1])
	}

	// 写回 OFF
	resp = rawRequest(t, sim.Addr(), 1, 0x05, []byte{0x00, 0x21, 0x00, 0x00})
	if resp.fc != 0x05 {
		t.Fatalf("写 OFF 功能码 = %#x，期望 0x05", resp.fc)
	}
	rb = rawRequest(t, sim.Addr(), 1, 0x01, []byte{0x00, 0x20, 0x00, 0x04})
	if rb.data[1] != 0x00 {
		t.Errorf("线圈位图 = %#x，期望 0x00（coil1 已 OFF）", rb.data[1])
	}
}

// TestWriteMultipleRegisters 写多寄存器 0x10（0x0010 单寄存器）+ 读回验证。
func TestWriteMultipleRegisters(t *testing.T) {
	sim := startTestSim(t)

	// 0x10 写 0x0010 数量 1，值 250（25.0°C）：数据区 = 字节数(1)+值(2)
	resp := rawRequest(t, sim.Addr(), 1, 0x10, []byte{0x00, 0x10, 0x00, 0x01, 0x02, 0x00, 0xFA})
	if resp.fc != 0x10 {
		t.Fatalf("功能码 = %#x，期望 0x10", resp.fc)
	}
	// 应答 = 起始地址 + 数量
	if len(resp.data) != 4 || resp.data[0] != 0x00 || resp.data[1] != 0x10 ||
		resp.data[2] != 0x00 || resp.data[3] != 0x01 {
		t.Errorf("应答 = % x，期望 [00 10 00 01]", resp.data)
	}
	rb := rawRequest(t, sim.Addr(), 1, 0x03, []byte{0x00, 0x10, 0x00, 0x01})
	if got := binary.BigEndian.Uint16(rb.data[1:3]); got != 250 {
		t.Errorf("读回 0x0010 = %d，期望 250", got)
	}
}

// TestErrorIllegalFunction 不支持的函数码（0x04 读输入寄存器）→ 异常 0x01。
func TestErrorIllegalFunction(t *testing.T) {
	sim := startTestSim(t)
	resp := rawRequest(t, sim.Addr(), 1, 0x04, []byte{0x00, 0x00, 0x00, 0x02})
	if resp.fc != 0x84 {
		t.Fatalf("功能码 = %#x，期望 0x84（0x04 | 0x80）", resp.fc)
	}
	if len(resp.data) != 1 || resp.data[0] != excIllegalFunction {
		t.Errorf("异常码 = %#x，期望 0x01（非法功能码）", resp.data[0])
	}
}

// TestErrorIllegalAddress 越界地址 / 写只读寄存器 → 异常 0x02。
func TestErrorIllegalAddress(t *testing.T) {
	sim := startTestSim(t)

	// 读未定义保持寄存器 0x0100 → 0x02
	resp := rawRequest(t, sim.Addr(), 1, 0x03, []byte{0x01, 0x00, 0x00, 0x01})
	if resp.fc != 0x83 || resp.data[0] != excIllegalAddress {
		t.Errorf("越界读 = fc %#x exc %#x，期望 0x83/0x02", resp.fc, resp.data[0])
	}
	// 读跨定义域（0x0001 起 2 个，含未定义的 0x0002）→ 0x02
	resp = rawRequest(t, sim.Addr(), 1, 0x03, []byte{0x00, 0x01, 0x00, 0x02})
	if resp.fc != 0x83 || resp.data[0] != excIllegalAddress {
		t.Errorf("跨域读 = fc %#x exc %#x，期望 0x83/0x02", resp.fc, resp.data[0])
	}
	// 写只读寄存器 0x0000（温度）→ 0x02
	resp = rawRequest(t, sim.Addr(), 1, 0x06, []byte{0x00, 0x00, 0x01, 0x2C})
	if resp.fc != 0x86 || resp.data[0] != excIllegalAddress {
		t.Errorf("写只读 = fc %#x exc %#x，期望 0x86/0x02", resp.fc, resp.data[0])
	}
	// 写越界线圈 0x0024 → 0x02
	resp = rawRequest(t, sim.Addr(), 1, 0x05, []byte{0x00, 0x24, 0xFF, 0x00})
	if resp.fc != 0x85 || resp.data[0] != excIllegalAddress {
		t.Errorf("写越界线圈 = fc %#x exc %#x，期望 0x85/0x02", resp.fc, resp.data[0])
	}
}

// TestErrorIllegalValue 非法数据值 → 异常 0x03。
func TestErrorIllegalValue(t *testing.T) {
	sim := startTestSim(t)

	// 线圈值非 0xFF00/0x0000 → 0x03
	resp := rawRequest(t, sim.Addr(), 1, 0x05, []byte{0x00, 0x20, 0x12, 0x34})
	if resp.fc != 0x85 || resp.data[0] != excIllegalValue {
		t.Errorf("非法线圈值 = fc %#x exc %#x，期望 0x85/0x03", resp.fc, resp.data[0])
	}
	// 目标温度越界（>1000）→ 0x03
	resp = rawRequest(t, sim.Addr(), 1, 0x06, []byte{0x00, 0x10, 0x07, 0xD0})
	if resp.fc != 0x86 || resp.data[0] != excIllegalValue {
		t.Errorf("目标温度越界 = fc %#x exc %#x，期望 0x86/0x03", resp.fc, resp.data[0])
	}
	// 读数量为 0 → 0x03
	resp = rawRequest(t, sim.Addr(), 1, 0x03, []byte{0x00, 0x00, 0x00, 0x00})
	if resp.fc != 0x83 || resp.data[0] != excIllegalValue {
		t.Errorf("数量为 0 = fc %#x exc %#x，期望 0x83/0x03", resp.fc, resp.data[0])
	}
	// 写多寄存器字节数不匹配（qty=1 但 byteCount=4）→ 0x03
	resp = rawRequest(t, sim.Addr(), 1, 0x10, []byte{0x00, 0x10, 0x00, 0x01, 0x04, 0x00, 0xFA, 0x00, 0x00})
	if resp.fc != 0x90 || resp.data[0] != excIllegalValue {
		t.Errorf("字节数不匹配 = fc %#x exc %#x，期望 0x90/0x03", resp.fc, resp.data[0])
	}
}

// TestUnitIDPassthrough 合法 unit ID（1-247）均应答并回显（模拟网关后设备的语义）。
func TestUnitIDPassthrough(t *testing.T) {
	sim := startTestSim(t)
	resp := rawRequest(t, sim.Addr(), 17, 0x03, []byte{0x00, 0x00, 0x00, 0x01})
	if resp.fc != 0x03 {
		t.Fatalf("功能码 = %#x，期望 0x03", resp.fc)
	}
	if resp.unit != 17 {
		t.Errorf("unit ID = %d，期望回显 17", resp.unit)
	}
}

// TestUnitIDOutOfRangeRejected 越界 unit ID（0 广播 / 248-255 保留段）
// 按规范应答异常码 0x0B（M4C P2-③ 修复：不再任意回显）。
func TestUnitIDOutOfRangeRejected(t *testing.T) {
	sim := startTestSim(t)
	for _, unit := range []byte{0, 248, 255} {
		resp := rawRequest(t, sim.Addr(), unit, 0x03, []byte{0x00, 0x00, 0x00, 0x01})
		if resp.fc != 0x83 {
			t.Errorf("unit=%d: 功能码 = %#x，期望异常 0x83", unit, resp.fc)
		}
		if len(resp.data) != 1 || resp.data[0] != excGatewayTarget {
			t.Errorf("unit=%d: 异常码 = %v，期望 0x0B", unit, resp.data)
		}
		if resp.unit != unit {
			t.Errorf("unit=%d: 应答 unit ID = %d，期望回显 %d", unit, resp.unit, unit)
		}
	}
}

// TestMaxConnsRejectsExcess 并发连接数超过上限后新连接被服务端直接关闭（读侧 EOF），
// 存量连接不受影响仍可正常收发（M4C P2-③ 修复）。
func TestMaxConnsRejectsExcess(t *testing.T) {
	sim := New("127.0.0.1:0", WithMaxConns(2), WithSeed(42), WithStep(20*time.Millisecond))
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })

	dial := func() net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("tcp", sim.Addr(), 2*time.Second)
		if err != nil {
			t.Fatalf("连接模拟器失败: %v", err)
		}
		return conn
	}
	c1 := dial()
	defer func() { _ = c1.Close() }()
	c2 := dial()
	defer func() { _ = c2.Close() }()

	// 第 3 条连接：超出上限，服务端应直接关闭（读侧收到 EOF/错误）
	c3 := dial()
	defer func() { _ = c3.Close() }()
	_ = c3.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := c3.Read(buf); err == nil {
		t.Errorf("超限连接应被服务端关闭（期望 EOF/错误），实际读到数据")
	}

	// 存量连接仍可正常收发（发读请求 → 正常应答）
	req := []byte{0x12, 0x34, 0, 0, 0, 6, 1, 0x03, 0x00, 0x00, 0x00, 0x01}
	if _, err := c1.Write(req); err != nil {
		t.Fatalf("存量连接发送失败: %v", err)
	}
	_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	head := make([]byte, 7)
	if _, err := io.ReadFull(c1, head); err != nil {
		t.Fatalf("存量连接读取应答失败: %v", err)
	}
	if head[6] != 1 {
		t.Errorf("应答 unit ID = %d，期望 1", head[6])
	}
}

// TestRegisterTable 寄存器表说明包含全部关键地址。
func TestRegisterTable(t *testing.T) {
	sim := startTestSim(t)
	table := sim.RegisterTable()
	for _, want := range []string{"0x0000", "0x0001", "0x0010", "0x0020", "0x0021", "0x0022", "0x0023"} {
		if !contains(table, want) {
			t.Errorf("寄存器表缺少地址 %s", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
