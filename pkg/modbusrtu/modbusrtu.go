// Package modbusrtu 提供 Modbus RTU 帧编解码与客户端（v0.41.0，spec 0014 US-1）。
//
// RTU 帧：`[从站地址(1)][PDU][CRC16(2, 低字节在前)]`——与 MBAP（TCP）封装的
// 差异只在帧头/帧尾，PDU（功能码+数据）完全一致。本包零依赖（标准库）：
//   - CRC16：Modbus 规范多项式 0xA001（反射），查表法实现；
//   - 帧编解码：AppendFrame（addr+PDU+CRC）/ ParseFrame（长度/CRC/最小长度校验）；
//   - 客户端：面向 io.ReadWriteCloser 字节流（RTU-over-TCP 拨号助手 DialTCP；
//     真串口 transport 登记为后续候选——spec 0014/KI §42），支持 FC 0x01/0x03/
//     0x05/0x06，响应截止超时、从站地址/功能码/CRC 校验、Modbus 异常码帧。
//
// 与 goburrow/modbus（TCP 路径既有依赖）的关系：TCP 路径继续用 goburrow
// （mappers/modbus 零回归）；RTU-over-TCP（串口-网关主流形态）与后续串口
// transport 使用本包——模拟器 pkg/modbussim 提供 RTU 模式联调。
package modbusrtu

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// 功能码常量（本客户端支持集）。
const (
	FuncReadCoils            byte = 0x01
	FuncReadHoldingRegisters byte = 0x03
	FuncWriteSingleCoil      byte = 0x05
	FuncWriteSingleRegister  byte = 0x06
)

// 帧长度约束（PDU 最长 253，帧最长 256；本客户端请求/应答均有界）。
const (
	maxPDUSize    = 253
	maxFrameSize  = 256
	minFrameSize  = 4 // addr(1) + fc(1) + crc(2)——异常帧最小长度
	respTailCRC   = 2
	respHeadLen   = 2 // addr + fc
	exceptionMask = 0x80
)

// CRC16 计算 Modbus CRC16（多项式 0xA001 反射，初值 0xFFFF，低字节在前传输）。
// 标准校验向量：ASCII "123456789" → 0x4B37。
func CRC16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// AppendFrame 组装 RTU 帧：addr + pdu + CRC16（低字节在前）。pdu 超长（>253）
// 返回 nil——调用方输入有界，防御性保护。
func AppendFrame(addr byte, pdu []byte) []byte {
	if len(pdu) > maxPDUSize {
		return nil
	}
	frame := make([]byte, 0, len(pdu)+3)
	frame = append(frame, addr)
	frame = append(frame, pdu...)
	crc := CRC16(frame)
	frame = append(frame, byte(crc), byte(crc>>8)) // CRC 低字节在前（Modbus 规范）
	return frame
}

// ParseFrame 解析 RTU 帧：校验最小长度与 CRC，返回从站地址与 PDU。
// CRC 不符返回 ErrBadCRC（调用方按传输错误处理）。
func ParseFrame(frame []byte) (byte, []byte, error) {
	if len(frame) < minFrameSize {
		return 0, nil, fmt.Errorf("RTU 帧长度 %d 不足（最小 %d）", len(frame), minFrameSize)
	}
	if len(frame) > maxFrameSize {
		return 0, nil, fmt.Errorf("RTU 帧长度 %d 超限（最大 %d）", len(frame), maxFrameSize)
	}
	pduLen := len(frame) - 3 // addr(1) + pdu(N) + crc(2)
	crc := uint16(frame[len(frame)-2]) | uint16(frame[len(frame)-1])<<8
	if CRC16(frame[:1+pduLen]) != crc {
		return 0, nil, ErrBadCRC
	}
	return frame[0], frame[1 : 1+pduLen], nil
}

// 错误哨兵（传输/协议层；调用方以 errors.Is 区分）。
var (
	// ErrBadCRC 表示应答帧 CRC 校验失败（链路误码/错位）。
	ErrBadCRC = errors.New("modbusrtu: CRC 校验失败")
	// ErrBadSlave 表示应答从站地址与请求不符。
	ErrBadSlave = errors.New("modbusrtu: 应答从站地址不符")
	// ErrBadFunc 表示应答功能码与请求不符。
	ErrBadFunc = errors.New("modbusrtu: 应答功能码不符")
	// ErrShortResp 表示应答长度不足。
	ErrShortResp = errors.New("modbusrtu: 应答长度不足")
	// ErrBadPDU 表示 PDU 结构非法（长度/字段缺失）。
	ErrBadPDU = errors.New("modbusrtu: PDU 非法")
)

// ModbusError 是 Modbus 异常应答（fc|0x80 + 异常码）——设备已应答的业务错误
// （非法地址/值等），调用方语义上不应重试（与 goburrow modbus.ModbusError 对齐）。
type ModbusError struct {
	Function byte // 原功能码（去 0x80 位）
	Code     byte // 异常码（0x01 非法功能 / 0x02 非法地址 / 0x03 非法值 / ...）
}

// Error 实现 error 接口。
func (e *ModbusError) Error() string {
	return fmt.Sprintf("modbus 异常应答: function=0x%02x code=0x%02x", e.Function, e.Code)
}

// Client 是 RTU 客户端：面向已建立的字节流连接（RTU-over-TCP 由 DialTCP 建立；
// 真串口由调用方提供 termios 配置后的 io.ReadWriteCloser——后续候选）。
// 并发约束：单连接串行请求-应答，调用方自行串行化（mapper 持锁）。
type Client struct {
	conn    net.Conn // 读写与截止超时需要 net.Conn（DialTCP 保证）
	slaveID byte
	timeout time.Duration
}

// DialTCP 建立 RTU-over-TCP 客户端（RTU 帧封装 over TCP 字节流——串口-网关
// 主流形态；模拟器 pkg/modbussim RTU 模式联调用）。
func DialTCP(addr string, slaveID byte, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("拨号 RTU-over-TCP %s 失败: %w", addr, err)
	}
	return &Client{conn: conn, slaveID: slaveID, timeout: timeout}, nil
}

// NewClient 面向已建立连接构造客户端（真串口 transport 后续候选）。
func NewClient(conn net.Conn, slaveID byte, timeout time.Duration) *Client {
	return &Client{conn: conn, slaveID: slaveID, timeout: timeout}
}

// Close 关闭底层连接（幂等）。
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// request 发送请求帧并读取应答帧：组帧 → 写 → 按功能码确定应答长度读取 →
// 校验（从站/功能码/CRC）→ 返回应答 PDU 数据段（不含 addr/fc）。
func (c *Client) request(fc byte, data []byte) ([]byte, error) {
	if c.conn == nil {
		return nil, errors.New("modbusrtu: 连接未建立")
	}
	req := AppendFrame(c.slaveID, append([]byte{fc}, data...))
	if req == nil {
		return nil, fmt.Errorf("%w: 请求 PDU 超长", ErrBadPDU)
	}
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := c.conn.Write(req); err != nil {
		return nil, fmt.Errorf("写 RTU 帧失败: %w", err)
	}
	pdu, err := c.readResponse(fc)
	if err != nil && (errors.Is(err, ErrBadCRC) || errors.Is(err, ErrShortResp) || errors.Is(err, ErrBadPDU)) {
		// 失步/帧错：RTU 无帧边界重同步语义，主动关闭连接（调用方重连恢复；
		// 直连用户不会读到错位帧）——spec 0014 US-1 边界，KI §42。
		_ = c.conn.Close()
	}
	return pdu, err
}

// readResponse 读取并校验一帧应答（长度按功能码判定；异常帧按 0x80 位识别）。
func (c *Client) readResponse(reqFC byte) ([]byte, error) {
	head := make([]byte, respHeadLen)
	if err := c.readFull(head); err != nil {
		return nil, err
	}
	addr, respFC := head[0], head[1]
	if addr != c.slaveID {
		return nil, fmt.Errorf("%w: 应答 addr=0x%02x 请求 slaveID=0x%02x", ErrBadSlave, addr, c.slaveID)
	}
	if respFC == reqFC^exceptionMask {
		tail := make([]byte, 1+respTailCRC) // 异常码(1) + CRC(2)
		if err := c.readFull(tail); err != nil {
			return nil, err
		}
		frame := append(head, tail...)
		if _, _, err := ParseFrame(frame); err != nil {
			return nil, err
		}
		return nil, &ModbusError{Function: reqFC, Code: tail[0]}
	}
	if respFC != reqFC {
		return nil, fmt.Errorf("%w: 应答 fc=0x%02x 请求 fc=0x%02x", ErrBadFunc, respFC, reqFC)
	}
	var dataLen int
	switch reqFC {
	case FuncReadCoils, FuncReadHoldingRegisters:
		bc := make([]byte, 1)
		if err := c.readFull(bc); err != nil {
			return nil, err
		}
		dataLen = int(bc[0]) + 1 + respTailCRC // byteCount 字节 + byteCount 本身 + CRC
		buf := append(bc, make([]byte, dataLen-1)...)
		if err := c.readFull(buf[1:]); err != nil {
			return nil, err
		}
		frame := append(head, buf...)
		_, pdu, err := ParseFrame(frame)
		if err != nil {
			return nil, err
		}
		return pdu, nil // pdu = [byteCount][data...]（fc 之后）
	case FuncWriteSingleCoil, FuncWriteSingleRegister:
		echo := make([]byte, 4+respTailCRC) // addr(2) + value(2) + CRC(2)
		if err := c.readFull(echo); err != nil {
			return nil, err
		}
		frame := append(head, echo...)
		_, pdu, err := ParseFrame(frame)
		if err != nil {
			return nil, err
		}
		return pdu, nil // pdu = [addr(2)][value(2)]
	default:
		return nil, fmt.Errorf("%w: fc=0x%02x", ErrBadFunc, reqFC)
	}
}

// readFull 恰好读满 len(buf) 字节（不足=对端关闭/超时 → ErrShortResp 语义）。
func (c *Client) readFull(buf []byte) error {
	got := 0
	for got < len(buf) {
		n, err := c.conn.Read(buf[got:])
		if err != nil {
			return fmt.Errorf("%w: 读应答失败（%d/%d）: %v", ErrShortResp, got, len(buf), err)
		}
		if n == 0 {
			return fmt.Errorf("%w: 对端关闭", ErrShortResp)
		}
		got += n
	}
	return nil
}

// ReadRequestFrame 服务端助手：从字节流读取一帧完整 RTU 请求（addr+fc 之后
// 按功能码判定数据段长度：01/03/04/05/06 为定长 4 字节；其余功能码返回错误
// ——本客户端/模拟器面仅支持上述功能码）。返回原始帧（含 CRC），由调用方
// ParseFrame 校验。模拡器 pkg/modbussim RTU 模式使用。
func ReadRequestFrame(conn net.Conn) ([]byte, error) {
	head := make([]byte, 2) // addr + fc
	if err := readAll(conn, head); err != nil {
		return nil, err
	}
	switch head[1] {
	case FuncReadCoils, FuncReadHoldingRegisters, FuncReadInput, FuncWriteSingleCoil, FuncWriteSingleRegister:
		rest := make([]byte, 4+respTailCRC) // 数据 4 字节 + CRC 2
		if err := readAll(conn, rest); err != nil {
			return nil, err
		}
		return append(head, rest...), nil
	default:
		return nil, fmt.Errorf("%w: 请求 fc=0x%02x 不支持", ErrBadFunc, head[1])
	}
}

// FuncReadInput 是读输入寄存器（FC 0x04）——模拟器 RTU 模式支持面内。
const FuncReadInput byte = 0x04

// readAll 面向 net.Conn 的 readFull（服务端助手复用）。
func readAll(conn net.Conn, buf []byte) error {
	got := 0
	for got < len(buf) {
		n, err := conn.Read(buf[got:])
		if err != nil {
			return fmt.Errorf("%w: 读请求失败（%d/%d）: %v", ErrShortResp, got, len(buf), err)
		}
		if n == 0 {
			return fmt.Errorf("%w: 对端关闭", ErrShortResp)
		}
		got += n
	}
	return nil
}

// ReadCoils 读线圈（FC 0x01）：返回打包位字节（每字节 LSB 在前，Modbus 规范）。
func (c *Client) ReadCoils(address, quantity uint16) ([]byte, error) {
	if quantity == 0 || quantity > 2000 {
		return nil, fmt.Errorf("%w: 读线圈数量 %d 非法（1..2000）", ErrBadPDU, quantity)
	}
	data := []byte{byte(address >> 8), byte(address), byte(quantity >> 8), byte(quantity)}
	pdu, err := c.request(FuncReadCoils, data)
	if err != nil {
		return nil, err
	}
	if len(pdu) < 3 {
		return nil, fmt.Errorf("%w: 读线圈应答过短", ErrBadPDU)
	}
	wantBC := int((quantity + 7) / 8) // 打包位：每字节 8 位
	if int(pdu[1]) != wantBC || len(pdu)-2 != wantBC {
		return nil, fmt.Errorf("%w: 读线圈应答 byteCount=%d（期望 %d）", ErrBadPDU, pdu[1], wantBC)
	}
	return pdu[2:], nil // 去 fc + byteCount（打包位数据）
}

// ReadHoldingRegisters 读保持寄存器（FC 0x03）：返回寄存器原始字节（大端）。
func (c *Client) ReadHoldingRegisters(address, quantity uint16) ([]byte, error) {
	if quantity == 0 || quantity > 125 {
		return nil, fmt.Errorf("%w: 读保持寄存器数量 %d 非法（1..125）", ErrBadPDU, quantity)
	}
	data := []byte{byte(address >> 8), byte(address), byte(quantity >> 8), byte(quantity)}
	pdu, err := c.request(FuncReadHoldingRegisters, data)
	if err != nil {
		return nil, err
	}
	if len(pdu) < 3 {
		return nil, fmt.Errorf("%w: 读保持寄存器应答过短", ErrBadPDU)
	}
	wantBC := int(quantity) * 2 // 每寄存器 2 字节
	if int(pdu[1]) != wantBC || len(pdu)-2 != wantBC {
		return nil, fmt.Errorf("%w: 读保持寄存器应答 byteCount=%d（期望 %d）", ErrBadPDU, pdu[1], wantBC)
	}
	return pdu[2:], nil // 去 fc + byteCount（寄存器原始字节，大端）
}

// WriteSingleRegister 写单寄存器（FC 0x06）：应答为请求回显。
func (c *Client) WriteSingleRegister(address, value uint16) ([]byte, error) {
	data := []byte{byte(address >> 8), byte(address), byte(value >> 8), byte(value)}
	pdu, err := c.request(FuncWriteSingleRegister, data)
	if err != nil {
		return nil, err
	}
	if len(pdu) < 4 {
		return nil, fmt.Errorf("%w: 写单寄存器应答过短", ErrBadPDU)
	}
	return pdu[1:], nil // 去 fc（回显 addr(2)+value(2)）
}

// WriteSingleCoil 写单线圈（FC 0x05）：value 非 0 = ON（0xFF00），0 = OFF（0x0000）。
func (c *Client) WriteSingleCoil(address uint16, value uint16) ([]byte, error) {
	coilVal := uint16(0x0000)
	if value != 0 {
		coilVal = 0xFF00
	}
	data := []byte{byte(address >> 8), byte(address), byte(coilVal >> 8), byte(coilVal)}
	pdu, err := c.request(FuncWriteSingleCoil, data)
	if err != nil {
		return nil, err
	}
	if len(pdu) < 4 {
		return nil, fmt.Errorf("%w: 写单线圈应答过短", ErrBadPDU)
	}
	return pdu[1:], nil // 去 fc（回显 addr(2)+coilVal(2)）
}
