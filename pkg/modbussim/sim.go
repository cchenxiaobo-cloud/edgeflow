// Package modbussim 提供高保真 Modbus TCP 模拟器（WBS 5.2）。
//
// 设计目标：在无真实设备时验证 Modbus Mapper 的完整读写链路与操作台账。
// 协议帧（MBAP 头 + PDU）完全自实现，不依赖任何第三方 Modbus 库——
// 客户端侧用社区标准库（goburrow/modbus）反而能交叉验证帧格式正确性。
//
// 支持的 Modbus 功能码（与任务验收一致）：
//   - 0x01 读线圈（Read Coils）
//   - 0x03 读保持寄存器（Read Holding Registers）
//   - 0x05 写单线圈（Write Single Coil）
//   - 0x06 写单寄存器（Write Single Register）
//   - 0x10 写多寄存器（Write Multiple Registers）
//
// 异常应答（功能码 | 0x80 + 错误码）：
//   - 0x01 非法功能码（不支持的 FC，如 0x02/0x04/0x0F）
//   - 0x02 非法数据地址（越界地址 / 写只读寄存器）
//   - 0x03 非法数据值（线圈值非 0xFF00/0x0000、目标温度越界、数量为 0 等）
//
// 设备模型（1 台模拟设备，unit ID 1）：
//   - 保持寄存器 0x0000 温度（原始值 = 物理值×10，如 250 → 25.0°C）
//   - 保持寄存器 0x0001 湿度（同 10 倍缩放）
//   - 保持寄存器 0x0010 目标温度（可写；0x0000/0x0001 只读）
//   - 线圈 0x0020-0x0023 共 4 个（coil0..coil3，可写）
//
// 动态行为（仿 mock_sensor）：温度按比例向目标温度收敛并叠加小随机扰动，
// 湿度随机游走，均钳制在 [0, 100]°C / %；寄存器表由波动 goroutine 持续刷新。
package modbussim

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"
)

// 寄存器与线圈地址（模拟设备地址映射表，与 docs/MODBUS-GUIDE.md 一致）。
const (
	// RegTemperature 是温度保持寄存器地址（原始值 = 物理值×10）。
	RegTemperature = 0x0000
	// RegHumidity 是湿度保持寄存器地址（原始值 = 物理值×10）。
	RegHumidity = 0x0001
	// RegTargetTemp 是目标温度保持寄存器地址（可写，值域 [0,1000] 即 [0,100]°C）。
	RegTargetTemp = 0x0010
	// CoilBase 是线圈起始地址（共 CoilCount 个：0x0020-0x0023）。
	CoilBase = 0x0020
	// CoilCount 是线圈数量。
	CoilCount = 4

	// DefaultPort 是模拟器默认监听端口（环境变量 MODBUS_SIM_PORT 可覆盖）。
	DefaultPort = 15020
	// EnvPort 是覆盖监听端口的环境变量名。
	EnvPort = "MODBUS_SIM_PORT"

	// scaleFactor 是寄存器原始值与物理值的换算倍数（原始值 = 物理值×10）。
	scaleFactor = 10.0
	// minPhys/maxPhys 是温度/湿度/目标温度的物理值范围（°C / %RH）。
	minPhys = 0.0
	maxPhys = 100.0
	// convergeFactor 每波动周期温度向目标收敛的比例（仿 mock_sensor）。
	convergeFactor = 0.2
	// tempNoise/humNoise 每周期随机扰动上限（物理值绝对变化量）。
	tempNoise = 0.5
	humNoise  = 0.8

	// defaultStep 是默认波动周期。
	defaultStep = 500 * time.Millisecond
	// connIdleTimeout 是单连接空闲超时（超时后服务端主动断开）。
	connIdleTimeout = 60 * time.Second

	// Modbus 功能码。
	fcReadCoils         = 0x01
	fcReadHoldingRegs   = 0x03
	fcWriteSingleCoil   = 0x05
	fcWriteSingleReg    = 0x06
	fcWriteMultipleRegs = 0x10

	// Modbus 异常码。
	excIllegalFunction = 0x01
	excIllegalAddress  = 0x02
	excIllegalValue    = 0x03
)

// Option 是 Simulator 的可选配置项（函数式选项）。
type Option func(*Simulator)

// WithStep 设置寄存器波动周期（默认 500ms；测试可调小加速收敛验证）。
func WithStep(d time.Duration) Option {
	return func(s *Simulator) { s.step = d }
}

// WithSeed 设置随机数种子（默认随机；测试传固定种子保证确定性）。
func WithSeed(seed int64) Option {
	return func(s *Simulator) { s.rng = rand.New(rand.NewSource(seed)) }
}

// Simulator 是一台 Modbus TCP 模拟设备（unit ID 1）。
type Simulator struct {
	mu   sync.Mutex // 保护寄存器表与线圈（请求处理与波动 goroutine 并发访问）
	regs map[uint16]uint16
	coil [CoilCount]bool

	temp   float64 // 当前温度（物理值）
	hum    float64 // 当前湿度（物理值）
	target float64 // 目标温度（物理值，寄存器 0x0010 的语义）

	rng  *rand.Rand
	step time.Duration

	listenAddr string // 监听地址（":15020" 或测试用 "127.0.0.1:0"）
	ln         net.Listener
	conns      map[net.Conn]struct{} // 活跃连接（Stop 时全部断开，模拟服务端下电）
	done       chan struct{}
	wg         sync.WaitGroup
	started    bool
}

// New 创建模拟器（未启动）。listenAddr 为空时默认 ":15020"。
func New(listenAddr string, opts ...Option) *Simulator {
	if listenAddr == "" {
		listenAddr = fmt.Sprintf(":%d", DefaultPort)
	}
	s := &Simulator{
		regs: map[uint16]uint16{
			RegTemperature: 0,
			RegHumidity:    0,
			RegTargetTemp:  0,
		},
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
		step:       defaultStep,
		listenAddr: listenAddr,
		conns:      make(map[net.Conn]struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	// 初始值：温度/湿度随机，目标温度默认 25.0°C（与 mock_sensor 的默认目标对齐）
	s.temp = s.randRange(20, 30)
	s.hum = s.randRange(40, 70)
	s.target = 25.0
	s.syncRegsLocked()
	return s
}

// randRange 返回 [lo, hi] 内的随机浮点数（调用方需持有锁）。
func (s *Simulator) randRange(lo, hi float64) float64 {
	return lo + s.rng.Float64()*(hi-lo)
}

// randSigned 返回 [-max, max] 内的随机数（调用方需持有锁）。
func (s *Simulator) randSigned(max float64) float64 {
	return (s.rng.Float64()*2 - 1) * max
}

// clamp 把 v 限制在 [lo, hi] 内。
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// syncRegsLocked 把当前物理值同步进寄存器表（调用方需持有锁）。
func (s *Simulator) syncRegsLocked() {
	s.regs[RegTemperature] = uint16(s.temp * scaleFactor)
	s.regs[RegHumidity] = uint16(s.hum * scaleFactor)
	s.regs[RegTargetTemp] = uint16(s.target * scaleFactor)
}

// Addr 返回实际监听地址（测试用 127.0.0.1:0 随机端口时取真实端口）。
// 未启动时返回空串。
func (s *Simulator) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Start 启动模拟器：监听 TCP + 启动寄存器波动 goroutine。幂等。
func (s *Simulator) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("modbus 模拟器监听 %s 失败: %w", s.listenAddr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.conns = make(map[net.Conn]struct{})
	s.started = true
	s.done = make(chan struct{})
	s.mu.Unlock()

	s.wg.Add(1)
	go s.acceptLoop(ln)
	s.wg.Add(1)
	go s.fluctuateLoop()
	return nil
}

// Stop 停止模拟器：关闭监听、断开全部连接、停止波动。幂等。
func (s *Simulator) Stop() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	ln := s.ln
	done := s.done
	s.ln = nil
	// 断开全部活跃连接（客户端随即收到 EOF/RST，触发其重连路径）
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()

	_ = ln.Close()
	close(done)
	s.wg.Wait()
	return nil
}

// acceptLoop 接受连接并逐个处理（每个连接独立 goroutine）。
func (s *Simulator) acceptLoop(ln net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			// 监听已关闭（Stop）：正常退出
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.dropConn(conn)
			s.handleConn(conn)
		}()
	}
}

// dropConn 从活跃连接表移除一个连接（连接退出时调用）。
func (s *Simulator) dropConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// fluctuateLoop 周期刷新寄存器表：温度向目标收敛 + 随机扰动，湿度随机游走。
func (s *Simulator) fluctuateLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.step)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.tick()
		case <-s.done:
			return
		}
	}
}

// tick 执行一个波动周期（仿 mock_sensor 行为：温度向目标收敛）。
func (s *Simulator) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.temp = clamp(s.temp+(s.target-s.temp)*convergeFactor+s.randSigned(tempNoise), minPhys, maxPhys)
	s.hum = clamp(s.hum+s.randSigned(humNoise), minPhys, maxPhys)
	s.syncRegsLocked()
}

// handleConn 处理一个 Modbus TCP 连接：循环读请求帧、应答、直到出错或空闲超时。
func (s *Simulator) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	header := make([]byte, 7) // MBAP 头：事务 ID(2) + 协议 ID(2) + 长度(2) + unit ID(1)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(connIdleTimeout)); err != nil {
			return
		}
		// 读 MBAP 头
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		// 协议 ID 必须为 0（Modbus 协议）；否则视为非 Modbus 流量，断开
		if protoID := binary.BigEndian.Uint16(header[2:4]); protoID != 0 {
			return
		}
		length := binary.BigEndian.Uint16(header[4:6])
		// 长度 = unit ID(1) + PDU；最小 2（FC + 空数据），最大 254
		if length < 2 || length > 254 {
			return
		}
		// 读 PDU（长度 - unit ID 1 字节）
		pdu := make([]byte, length-1)
		if _, err := io.ReadFull(conn, pdu); err != nil {
			return
		}
		unitID := header[6]
		fc := pdu[0]

		// 处理请求：respData 为正常应答数据；exc != 0 表示异常应答
		respData, exc := s.handlePDU(fc, pdu[1:])

		// 组应答帧：事务 ID/协议 ID 回显，长度重算
		var respPDU []byte
		if exc != 0 {
			respPDU = []byte{fc | 0x80, exc}
		} else {
			respPDU = make([]byte, 0, 1+len(respData))
			respPDU = append(respPDU, fc)
			respPDU = append(respPDU, respData...)
		}
		adu := make([]byte, 0, 7+len(respPDU))
		adu = append(adu, header[0:4]...) // 回显事务 ID + 协议 ID
		adu = append(adu, byte(0), byte(1+len(respPDU)))
		adu = append(adu, unitID)
		adu = append(adu, respPDU...)
		if _, err := conn.Write(adu); err != nil {
			return
		}
	}
}

// handlePDU 分发 PDU 到具体功能码处理，返回 (应答数据, 异常码)。
// exc 非 0 时忽略应答数据，组装成 功能码|0x80 + 异常码 应答。
func (s *Simulator) handlePDU(fc byte, data []byte) ([]byte, byte) {
	switch fc {
	case fcReadCoils:
		return s.readCoils(data)
	case fcReadHoldingRegs:
		return s.readHoldingRegs(data)
	case fcWriteSingleCoil:
		return s.writeSingleCoil(data)
	case fcWriteSingleReg:
		return s.writeSingleReg(data)
	case fcWriteMultipleRegs:
		return s.writeMultipleRegs(data)
	default:
		// 不支持的函数码（如 0x02/0x04/0x0F）：异常码 0x01
		return nil, excIllegalFunction
	}
}

// parseAddrQty 解析 (起始地址, 数量) 请求参数；data 长度不足返回错误。
func parseAddrQty(data []byte) (addr, qty uint16, err error) {
	if len(data) < 4 {
		return 0, 0, errors.New("PDU 长度不足（需 4 字节）")
	}
	return binary.BigEndian.Uint16(data[0:2]), binary.BigEndian.Uint16(data[2:4]), nil
}

// readCoils 处理 0x01 读线圈：应答 = 字节数 + 位打包线圈状态（LSB 在前）。
func (s *Simulator) readCoils(data []byte) ([]byte, byte) {
	addr, qty, err := parseAddrQty(data)
	if err != nil {
		return nil, excIllegalValue
	}
	if qty < 1 || qty > 2000 {
		return nil, excIllegalValue
	}
	// 地址范围检查：必须完全落在 [CoilBase, CoilBase+CoilCount) 内
	if addr < CoilBase || uint32(addr)+uint32(qty) > uint32(CoilBase+CoilCount) {
		return nil, excIllegalAddress
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nbytes := int((qty + 7) / 8)
	resp := make([]byte, 1+nbytes)
	resp[0] = byte(nbytes)
	for i := uint16(0); i < qty; i++ {
		if s.coil[addr-CoilBase+uint16(i)] {
			resp[1+i/8] |= 1 << (i % 8) // 位打包：bit0 = 起始地址线圈
		}
	}
	return resp, 0
}

// readHoldingRegs 处理 0x03 读保持寄存器：应答 = 字节数 + 寄存器值（大端）。
func (s *Simulator) readHoldingRegs(data []byte) ([]byte, byte) {
	addr, qty, err := parseAddrQty(data)
	if err != nil {
		return nil, excIllegalValue
	}
	if qty < 1 || qty > 125 {
		return nil, excIllegalValue
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 逐地址校验存在性：任一地址未定义 → 0x02（非法数据地址）
	resp := make([]byte, 1+qty*2)
	resp[0] = byte(qty * 2)
	for i := uint16(0); i < qty; i++ {
		v, ok := s.regs[addr+i]
		if !ok {
			return nil, excIllegalAddress
		}
		binary.BigEndian.PutUint16(resp[1+i*2:], v)
	}
	return resp, 0
}

// writeSingleCoil 处理 0x05 写单线圈：值必须为 0xFF00(ON)/0x0000(OFF)，应答回显请求。
func (s *Simulator) writeSingleCoil(data []byte) ([]byte, byte) {
	addr, val, err := parseAddrQty(data)
	if err != nil {
		return nil, excIllegalValue
	}
	if addr < CoilBase || addr >= CoilBase+CoilCount {
		return nil, excIllegalAddress
	}
	switch val {
	case 0xFF00: // ON
	case 0x0000: // OFF
	default:
		return nil, excIllegalValue
	}
	s.mu.Lock()
	s.coil[addr-CoilBase] = val == 0xFF00
	s.mu.Unlock()
	// 应答 = 原样回显请求数据（地址 + 值）
	return data[:4], 0
}

// writeSingleReg 处理 0x06 写单寄存器：仅 0x0010 目标温度可写，
// 0x0000/0x0001 为只读（读侧模拟器维护）→ 0x02；值域 [0,1000] 外 → 0x03。
func (s *Simulator) writeSingleReg(data []byte) ([]byte, byte) {
	addr, val, err := parseAddrQty(data)
	if err != nil {
		return nil, excIllegalValue
	}
	if addr != RegTargetTemp {
		return nil, excIllegalAddress
	}
	if val > uint16(maxPhys*scaleFactor) {
		return nil, excIllegalValue
	}
	s.mu.Lock()
	s.target = float64(val) / scaleFactor
	s.syncRegsLocked()
	s.mu.Unlock()
	return data[:4], 0
}

// writeMultipleRegs 处理 0x10 写多寄存器：仅支持写 0x0010 单寄存器
// （可写集合只有目标温度；其余地址 → 0x02）。
func (s *Simulator) writeMultipleRegs(data []byte) ([]byte, byte) {
	addr, qty, err := parseAddrQty(data)
	if err != nil {
		return nil, excIllegalValue
	}
	if qty < 1 || qty > 123 {
		return nil, excIllegalValue
	}
	// 数据区 = 字节数(1) + 寄存器值(qty*2)
	if len(data) < 5 {
		return nil, excIllegalValue
	}
	if int(data[4]) != int(qty)*2 {
		return nil, excIllegalValue
	}
	if len(data) < 5+int(qty)*2 {
		return nil, excIllegalValue
	}
	// 可写集合校验：必须完全落在 {RegTargetTemp} 内
	if addr != RegTargetTemp || qty != 1 {
		return nil, excIllegalAddress
	}
	val := binary.BigEndian.Uint16(data[5:7])
	if val > uint16(maxPhys*scaleFactor) {
		return nil, excIllegalValue
	}
	s.mu.Lock()
	s.target = float64(val) / scaleFactor
	s.syncRegsLocked()
	s.mu.Unlock()
	// 应答 = 起始地址 + 数量
	return data[:4], 0
}

// RegisterTable 返回寄存器表说明（启动时打印 / 文档引用）。
func (s *Simulator) RegisterTable() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf(`模拟设备寄存器表（unit ID 1）：
  保持寄存器  0x0000  温度      （只读，原始值/10 = °C，当前 %d → %.1f°C）
  保持寄存器  0x0001  湿度      （只读，原始值/10 = %%，当前 %d → %.1f%%）
  保持寄存器  0x0010  目标温度  （可写，原始值/10 = °C，当前 %d → %.1f°C，温度向此收敛）
  线圈        0x0020  线圈0     （可写，当前 %v）
  线圈        0x0021  线圈1     （可写，当前 %v）
  线圈        0x0022  线圈2     （可写，当前 %v）
  线圈        0x0023  线圈3     （可写，当前 %v）`,
		s.regs[RegTemperature], s.temp,
		s.regs[RegHumidity], s.hum,
		s.regs[RegTargetTemp], s.target,
		s.coil[0], s.coil[1], s.coil[2], s.coil[3])
}

// Temp 返回当前温度物理值（测试/诊断用）。
func (s *Simulator) Temp() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.temp
}

// TargetTemp 返回当前目标温度物理值（测试/诊断用）。
func (s *Simulator) TargetTemp() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target
}
