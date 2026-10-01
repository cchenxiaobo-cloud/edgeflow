// v0410_test.go：Modbus RTU 协议与联调单测（spec 0014 US-1）。
package modbusrtu

import (
	"errors"
	"net"
	"testing"
	"time"
)

// TestCRC16Vectors 覆盖标准校验向量（Modbus CRC16 多项式 0xA001）。
func TestCRC16Vectors(t *testing.T) {
	// 标准向量：ASCII "123456789" → 0x4B37（Modbus CRC16 规范值）。
	if got := CRC16([]byte("123456789")); got != 0x4B37 {
		t.Fatalf("CRC16(123456789) = 0x%04X, want 0x4B37", got)
	}
	// 空输入：初值 0xFFFF（未参与任何折叠）。
	if got := CRC16(nil); got != 0xFFFF {
		t.Fatalf("CRC16(nil) = 0x%04X, want 0xFFFF", got)
	}
	// 单字节确定性。
	if CRC16([]byte{0x01}) != CRC16([]byte{0x01}) {
		t.Fatalf("CRC16 应确定性")
	}
}

// TestFrameRoundTrip 覆盖组帧/解析往返与篡改拒绝。
func TestFrameRoundTrip(t *testing.T) {
	pdu := []byte{0x03, 0x02, 0x01, 0xF4} // 读保持寄存器应答示例
	frame := AppendFrame(0x01, pdu)
	if len(frame) != len(pdu)+3 {
		t.Fatalf("帧长 = %d, want %d", len(frame), len(pdu)+3)
	}
	addr, got, err := ParseFrame(frame)
	if err != nil || addr != 0x01 {
		t.Fatalf("解析失败: %v, addr=%d", err, addr)
	}
	if string(got) != string(pdu) {
		t.Fatalf("PDU 往返不一致: % X vs % X", got, pdu)
	}
	// 篡改 PDU 一字节 → CRC 拒绝。
	bad := append([]byte(nil), frame...)
	bad[2] ^= 0xFF
	if _, _, err := ParseFrame(bad); !errors.Is(err, ErrBadCRC) {
		t.Fatalf("篡改帧应 ErrBadCRC: %v", err)
	}
	// 残帧拒绝。
	if _, _, err := ParseFrame(frame[:2]); err == nil {
		t.Fatalf("残帧应拒绝")
	}
	// 超长 PDU 拒绝（组帧返回 nil）。
	if AppendFrame(0x01, make([]byte, 254)) != nil {
		t.Fatalf("超长 PDU 组帧应返回 nil")
	}
}

// startRTUSim 启动 modbussim RTU 模式并返回地址（modbussim 联调用）。
// 为避免包依赖方向问题（modbussim → modbusrtu 单向），联调测试由
// mappers/modbus 包承担（mapper rtutcp:// ↔ sim.StartRTU 全链），本包内
// 仅用 net.Pipe 式回环验证客户端读帧逻辑——以本地 TCP 回环服务端实现。
func startEchoServer(t *testing.T, resp func(req []byte) []byte) net.Addr {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				for {
					if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
						return
					}
					req, err := ReadRequestFrame(c)
					if err != nil {
						return
					}
					addr, pdu, perr := ParseFrame(req)
					if perr != nil {
						return
					}
					if _, err := c.Write(AppendFrame(addr, resp(pdu))); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr()
}

// TestClientReadHoldingRegisters 覆盖 FC 0x03/0x06 全链（客户端↔回环服务端）。
func TestClientReadHoldingRegisters(t *testing.T) {
	addr := startEchoServer(t, func(pdu []byte) []byte {
		switch pdu[0] {
		case FuncReadHoldingRegisters:
			return []byte{0x03, 0x02, 0x00, 0x64} // byteCount=2, 值 100
		case FuncWriteSingleRegister:
			return pdu // 回显完整 PDU（fc + addr(2) + value(2)）
		default:
			return []byte{pdu[0] | 0x80, 0x01} // 非法功能
		}
	})
	c, err := DialTCP(addr.String(), 0x01, time.Second)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 正常读（FC 0x03）：应答数据段去掉 byteCount 后为 2 字节大端寄存器。
	raw, err := c.ReadHoldingRegisters(0x0000, 1)
	if err != nil {
		t.Fatalf("读保持寄存器失败: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("应答数据长 = %d, want 2", len(raw))
	}

	// 写单寄存器（FC 0x06）：回显 4 字节。
	echo, err := c.WriteSingleRegister(0x0010, 250)
	if err != nil {
		t.Fatalf("写单寄存器失败: %v", err)
	}
	if len(echo) != 4 {
		t.Fatalf("写应答长 = %d, want 4", len(echo))
	}
}

// TestClientExceptionFrame 覆盖异常应答帧（fc|0x80 + 异常码）→ ModbusError。
func TestClientExceptionFrame(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			req, err := ReadRequestFrame(conn)
			if err != nil {
				return
			}
			addr, pdu, perr := ParseFrame(req)
			if perr != nil {
				return
			}
			// 读保持寄存器一律回异常码 0x02（非法地址）。
			if pdu[0] == FuncReadHoldingRegisters {
				if _, err := conn.Write(AppendFrame(addr, []byte{FuncReadHoldingRegisters | 0x80, 0x02})); err != nil {
					return
				}
			}
		}
	}()
	c, err := DialTCP(ln.Addr().String(), 0x01, time.Second)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	_, err = c.ReadHoldingRegisters(0xFFFF, 1)
	var mbErr *ModbusError
	if !errors.As(err, &mbErr) || mbErr.Code != 0x02 {
		t.Fatalf("应得 ModbusError(0x02): %v", err)
	}
}
