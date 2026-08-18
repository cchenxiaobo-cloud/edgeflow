// Package modbus 提供 Modbus TCP 设备 Mapper（WBS 5.2）。
//
// 实现 edge/pkg/mapper.DeviceMapper 接口，管理 1 台 Modbus TCP 设备
// （mb-sensor-01，unit ID 1）：Collect 读温度/湿度保持寄存器，
// HandleCommand 写目标温度寄存器 / 写线圈。每次操作写入操作台账
// （edge/pkg/metamanager.Ledger，SQLite 持久化，保留 30 天）。
//
// 依赖说明（唯一新增依赖 github.com/goburrow/modbus）：
//   - 社区标准 Modbus 客户端，功能码/异常码/事务一致性校验完整，
//     无需自研协议栈（模拟器 pkg/modbussim 反而是自实现，用于交叉验证）；
//   - 纯 Go 无 CGO，与项目交叉编译约束一致（linux/amd64、linux/arm64）；
//   - 依赖克制：除 goburrow/modbus（及其串口子依赖）外不新增任何包。
//
// 连接管理：
//   - TCP 长连接，地址 env EDGEFLOW_MODBUS_ADDR（默认 127.0.0.1:15020），
//     操作超时 5s；
//   - 每次操作前确保连接（goburrow Connect 幂等，未连接才拨号）；
//   - 传输层错误（设备重启/断网）→ 断开旧连接重连并重试一次；
//     Modbus 异常应答（*modbus.ModbusError，如非法地址/值）是设备已应答的
//     业务错误，不重试，直接返回。
package modbus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goburrow/modbus"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
)

// 寄存器/线圈地址（与模拟器 pkg/modbussim 及 docs/MODBUS-GUIDE.md 一致）。
const (
	// RegTemperature 是温度保持寄存器地址（原始值 = 物理值×10）。
	RegTemperature = 0x0000
	// RegHumidity 是湿度保持寄存器地址。
	RegHumidity = 0x0001
	// RegTargetTemp 是目标温度保持寄存器地址（可写，值域 [0,1000] 即 [0,100]°C）。
	RegTargetTemp = 0x0010
	// CoilBase 是线圈起始地址（coil0..coil3 → 0x0020-0x0023）。
	CoilBase = 0x0020
	// CoilCount 是线圈数量。
	CoilCount = 4

	// DefaultName 是 Mapper 注册名（注册表唯一键）。
	DefaultName = "modbus-mapper"
	// DefaultDeviceName 是默认设备名。
	DefaultDeviceName = "mb-sensor-01"
	// DefaultNamespace 是设备默认命名空间。
	DefaultNamespace = "default"
	// DefaultAddr 是默认设备地址（TCP，env EDGEFLOW_MODBUS_ADDR 可覆盖）。
	DefaultAddr = "127.0.0.1:15020"
	// EnvAddr 是覆盖设备地址的环境变量名。
	EnvAddr = "EDGEFLOW_MODBUS_ADDR"
	// EnvNamespace 是覆盖设备命名空间的环境变量名（与 EnvAddr 约定一致；
	// 优先级：WithNamespace 选项 > 本环境变量 > DefaultNamespace）。
	EnvNamespace = "EDGEFLOW_MODBUS_NAMESPACE"
	// DefaultTimeout 是单次操作超时（连接 + 读写）。
	DefaultTimeout = 5 * time.Second
	// DefaultSlaveID 是默认 Modbus 从站地址（unit ID）。
	DefaultSlaveID = 1

	// scaleFactor 是寄存器原始值与物理值的换算倍数（原始值 = 物理值×10）。
	scaleFactor = 10.0
	// minTemp/maxTemp 是目标温度物理值范围（°C）。
	minTemp = 0.0
	maxTemp = 100.0
)

// OpLedger 是操作台账的最小接口（metamanager.Ledger 实现；
// 测试可注入 fake，不强制依赖 SQLite）。
type OpLedger interface {
	SaveOp(rec metamanager.OpRecord) error
}

// Option 是 ModbusMapper 的可选配置项（函数式选项）。
type Option func(*ModbusMapper)

// WithAddr 设置设备 TCP 地址（默认 127.0.0.1:15020）。
func WithAddr(addr string) Option {
	return func(m *ModbusMapper) { m.addr = addr }
}

// WithTimeout 设置单次操作超时（默认 5s）。
func WithTimeout(d time.Duration) Option {
	return func(m *ModbusMapper) { m.timeout = d }
}

// WithSlaveID 设置从站地址（默认 1）。
func WithSlaveID(id byte) Option {
	return func(m *ModbusMapper) { m.slaveID = id }
}

// WithDeviceName 设置设备名（默认 mb-sensor-01）。
func WithDeviceName(name string) Option {
	return func(m *ModbusMapper) { m.deviceName = name }
}

// WithNamespace 设置设备命名空间（默认 default）。
func WithNamespace(ns string) Option {
	return func(m *ModbusMapper) { m.namespace = ns }
}

// WithLedger 设置操作台账（nil = 不记录，测试可注入 fake）。
func WithLedger(l OpLedger) Option {
	return func(m *ModbusMapper) { m.ledger = l }
}

// 编译期断言：ModbusMapper 实现 Mapper 框架接口。DeviceNameResolver 声明
// 设备名、DeviceNamespaceResolver 声明设备命名空间——注册表据此建立
// 「namespace/deviceName」路由索引（M3A P2-1），非 default 命名空间场景
// 下路由与影子键才能正确联动。
var (
	_ mapper.DeviceMapper            = (*ModbusMapper)(nil)
	_ mapper.DeviceNameResolver      = (*ModbusMapper)(nil)
	_ mapper.DeviceNamespaceResolver = (*ModbusMapper)(nil)
)

// ModbusMapper 是 Modbus TCP 设备 Mapper。
type ModbusMapper struct {
	mu         sync.Mutex // 串行化连接状态与读写（重连逻辑需要独占连接）
	addr       string
	timeout    time.Duration
	slaveID    byte
	deviceName string
	namespace  string
	ledger     OpLedger

	handler *modbus.TCPClientHandler
	client  modbus.Client
	started bool
}

// New 创建 Modbus Mapper（未启动）。addr 为空时从环境变量
// EDGEFLOW_MODBUS_ADDR 读取，仍未设置则用默认 127.0.0.1:15020。
func New(addr string, opts ...Option) *ModbusMapper {
	if addr == "" {
		addr = os.Getenv(EnvAddr)
	}
	if addr == "" {
		addr = DefaultAddr
	}
	// namespace 解析优先级（与 addr 的 env 约定一致）：
	//   1. WithNamespace 选项（显式配置最高优先）；
	//   2. 环境变量 EDGEFLOW_MODBUS_NAMESPACE（非 default 命名空间部署
	//      无需改装配代码，edgecore 的 buildMapperRegistry 直接生效）；
	//   3. 默认 default（未配置/显式空串统一回退，保证 DeviceNamespace()
	//      与 DeviceReport.Namespace 恒非空，与注册表索引键一致）。
	m := &ModbusMapper{
		addr:       addr,
		timeout:    DefaultTimeout,
		slaveID:    DefaultSlaveID,
		deviceName: DefaultDeviceName,
	}
	for _, o := range opts {
		o(m)
	}
	if m.namespace == "" {
		m.namespace = os.Getenv(EnvNamespace)
	}
	if m.namespace == "" {
		m.namespace = DefaultNamespace
	}
	handler := modbus.NewTCPClientHandler(m.addr)
	handler.Timeout = m.timeout
	handler.SlaveId = m.slaveID
	m.handler = handler
	m.client = modbus.NewClient(handler)
	return m
}

// Addr 返回设备地址（诊断/测试用）。
func (m *ModbusMapper) Addr() string { return m.addr }

// Name 返回注册名（注册表唯一键）。
func (m *ModbusMapper) Name() string { return DefaultName }

// DeviceNames 声明本 Mapper 管理的设备名（注册表据此建立路由索引）。
func (m *ModbusMapper) DeviceNames() []string {
	return []string{m.deviceName}
}

// DeviceNamespace 返回设备所属命名空间（注册表据此建立 namespace/deviceName
// 路由索引，M3A P2-1；与 mock_sensor.DeviceNamespace 同签名）。
// New 已保证恒非空（option > env EDGEFLOW_MODBUS_NAMESPACE > default），
// 因此注册表索引键与 DeviceReport.Namespace 总是同源一致。
func (m *ModbusMapper) DeviceNamespace() string { return m.namespace }

// Start 启动 Mapper（幂等）：做一次尽力而为的预连接。
// 设备未就绪（模拟器/真实设备晚于 edgecore 启动）只告警不报错——
// 采集/指令操作时按需重连（见 withConn）。
func (m *ModbusMapper) Start(_ context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	m.mu.Unlock()

	log.Infof("ModbusMapper %s 启动（addr=%s, slaveID=%d, timeout=%s）",
		m.deviceName, m.addr, m.slaveID, m.timeout)
	if err := m.handler.Connect(); err != nil {
		log.Warnf("ModbusMapper %s: 预连接 %s 失败（%v），操作时将自动重连",
			m.deviceName, m.addr, err)
	}
	return nil
}

// Stop 停止 Mapper（幂等）：断开与设备的连接，后续操作将返回错误。
func (m *ModbusMapper) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return nil
	}
	m.started = false
	if err := m.handler.Close(); err != nil {
		log.Warnf("ModbusMapper %s: 关闭连接失败（%v），忽略", m.deviceName, err)
	}
	log.Infof("ModbusMapper %s 已停止", m.deviceName)
	return nil
}

// withConn 保证连接就绪后执行 op；传输层失败时断开重连并重试一次。
// Modbus 异常应答（*modbus.ModbusError）是设备已应答的业务错误，不重试。
// 调用方需保证 op 不再次加锁（本方法持有 m.mu）。
func (m *ModbusMapper) withConn(op func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.handler.Connect(); err != nil {
		return fmt.Errorf("连接 Modbus 设备 %s 失败: %w", m.addr, err)
	}
	if err := op(); err != nil {
		var mbErr *modbus.ModbusError
		if errors.As(err, &mbErr) {
			return err // 设备异常应答（非法地址/值等）：语义错误，无需重连
		}
		// 传输层错误：连接可能已失效（设备重启/断网），断开后重连重试一次
		_ = m.handler.Close()
		if cerr := m.handler.Connect(); cerr != nil {
			return fmt.Errorf("重连 Modbus 设备 %s 失败: %w（原始错误: %v）", m.addr, cerr, err)
		}
		if err2 := op(); err2 != nil {
			return fmt.Errorf("重试后仍失败: %w", err2)
		}
	}
	return nil
}

// Collect 采集设备当前属性值：一次读 0x0000-0x0001（温度/湿度保持寄存器），
// 原始值除以 10 换算为物理值。成功后记录台账（方向 up，结果 ok）。
func (m *ModbusMapper) Collect() (map[string]float64, error) {
	var tempRaw, humRaw uint16
	err := m.withConn(func() error {
		results, err := m.client.ReadHoldingRegisters(RegTemperature, 2)
		if err != nil {
			return err
		}
		// 应答：字节数(1) + 寄存器值(大端，每 2 字节一个)
		if len(results) < 4 {
			return fmt.Errorf("读保持寄存器应答长度 %d 不足（期望 ≥4）", len(results))
		}
		tempRaw = uint16(results[0])<<8 | uint16(results[1])
		humRaw = uint16(results[2])<<8 | uint16(results[3])
		return nil
	})
	if err != nil {
		m.saveOp(metamanager.DirUp, "0x0000-0x0001", "", "error", err.Error())
		return nil, fmt.Errorf("采集 %s 温度/湿度失败: %w", m.deviceName, err)
	}
	m.saveOp(metamanager.DirUp, "0x0000-0x0001",
		fmt.Sprintf("%d/%d", tempRaw, humRaw), "ok",
		fmt.Sprintf("读温度+湿度（%.1f°C / %.1f%%）", float64(tempRaw)/scaleFactor, float64(humRaw)/scaleFactor))
	return map[string]float64{
		"temperature": float64(tempRaw) / scaleFactor,
		"humidity":    float64(humRaw) / scaleFactor,
	}, nil
}

// HandleCommand 处理云端下发的指令：
//   - property=targetTemp：写保持寄存器 0x0010（值域 [0,100]°C，越界报错），
//     写后回读验证一致性；
//   - property=coil0..coil3：写线圈 0x0020-0x0023（value 非 0 = ON，0 = OFF），
//     写后回读验证一致性。
//
// 返回处理后的最新状态快照（DeviceReport）。指令设备名与自身不符、
// property 缺失或未知属性均返回错误。每次写操作记录台账（方向 down）。
func (m *ModbusMapper) HandleCommand(cmd mapper.DeviceCommand) (mapper.DeviceReport, error) {
	if cmd.DeviceName != "" && cmd.DeviceName != m.deviceName {
		return mapper.DeviceReport{}, fmt.Errorf("指令设备名 %q 与本 Mapper 设备 %q 不符",
			cmd.DeviceName, m.deviceName)
	}
	switch {
	case cmd.Property == "targetTemp":
		return m.handleTargetTemp(cmd.Value)
	case strings.HasPrefix(cmd.Property, "coil"):
		return m.handleCoil(cmd.Property, cmd.Value)
	case cmd.Property == "":
		return mapper.DeviceReport{}, errors.New("指令缺少 property 字段")
	default:
		return mapper.DeviceReport{}, fmt.Errorf("不支持的指令属性 %q", cmd.Property)
	}
}

// handleTargetTemp 写目标温度寄存器（0x0010）并回读验证。
func (m *ModbusMapper) handleTargetTemp(value float64) (mapper.DeviceReport, error) {
	if value < minTemp || value > maxTemp {
		err := fmt.Errorf("targetTemp %v 超出合法范围 [%v, %v]", value, minTemp, maxTemp)
		m.saveOp(metamanager.DirDown, fmt.Sprintf("0x%04X", RegTargetTemp),
			fmt.Sprintf("%v", value), "error", err.Error())
		return mapper.DeviceReport{}, err
	}
	// M4C P2-⑤ 修复（float 精度边界）：原始值 = round(物理值×10)，四舍五入到
	// 最近的 0.1°C 粒度，而非直接截断（如 25.55°C → 原始值 256 而非 255）。
	// 值域校验 [0,100] 已在前置完成，舍入后原始值仍在 [0,1000]，不会越界；
	// 寄存器原始值精度为 0.1，写入精度边界以 0.1°C 为准。
	raw := uint16(math.Round(value * scaleFactor))
	var readBack uint16
	err := m.withConn(func() error {
		if _, err := m.client.WriteSingleRegister(RegTargetTemp, raw); err != nil {
			return err
		}
		// 回读验证写入生效（真实设备链路的一致性检查）
		results, err := m.client.ReadHoldingRegisters(RegTargetTemp, 1)
		if err != nil {
			return err
		}
		if len(results) < 2 {
			return fmt.Errorf("回读应答长度 %d 不足", len(results))
		}
		readBack = uint16(results[0])<<8 | uint16(results[1])
		return nil
	})
	if err != nil {
		m.saveOp(metamanager.DirDown, fmt.Sprintf("0x%04X", RegTargetTemp),
			fmt.Sprintf("%d", raw), "error", err.Error())
		return mapper.DeviceReport{}, fmt.Errorf("写目标温度 %s 失败: %w", m.deviceName, err)
	}
	if readBack != raw {
		err := fmt.Errorf("回读验证失败：写 0x%04X=%d，回读 %d", RegTargetTemp, raw, readBack)
		m.saveOp(metamanager.DirDown, fmt.Sprintf("0x%04X", RegTargetTemp),
			fmt.Sprintf("%d", raw), "error", err.Error())
		return mapper.DeviceReport{}, err
	}
	m.saveOp(metamanager.DirDown, fmt.Sprintf("0x%04X", RegTargetTemp),
		fmt.Sprintf("%d", raw), "ok",
		fmt.Sprintf("写目标温度 %.1f°C，回读验证一致", value))
	return m.snapshot()
}

// handleCoil 写线圈（coil0..coil3 → 0x0020-0x0023）并回读验证。
func (m *ModbusMapper) handleCoil(property string, value float64) (mapper.DeviceReport, error) {
	// M4C P2-⑥：严格精确匹配 coil0..coil3（Sscanf 会误接受 "coil2x"）
	if !strings.HasPrefix(property, "coil") {
		err := fmt.Errorf("不支持的线圈属性 %q（支持 coil0..coil%d）", property, CoilCount-1)
		m.saveOp(metamanager.DirDown, property, "", "error", err.Error())
		return mapper.DeviceReport{}, err
	}
	idxStr := strings.TrimPrefix(property, "coil")
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= CoilCount {
		err := fmt.Errorf("不支持的线圈属性 %q（支持 coil0..coil%d）", property, CoilCount-1)
		m.saveOp(metamanager.DirDown, property, "", "error", err.Error())
		return mapper.DeviceReport{}, err
	}
	addr := CoilBase + uint16(idx)
	on := value != 0 // 非 0 = ON，0 = OFF
	var coilVal uint16
	if on {
		coilVal = 0xFF00
	}
	var readBack bool
	err = m.withConn(func() error {
		if _, err := m.client.WriteSingleCoil(addr, coilVal); err != nil {
			return err
		}
		// 回读验证写入生效
		results, err := m.client.ReadCoils(addr, 1)
		if err != nil {
			return err
		}
		if len(results) < 1 {
			return fmt.Errorf("回读应答为空")
		}
		readBack = results[0]&0x01 != 0
		return nil
	})
	if err != nil {
		m.saveOp(metamanager.DirDown, fmt.Sprintf("coil:0x%04X", addr), "", "error", err.Error())
		return mapper.DeviceReport{}, fmt.Errorf("写线圈 %s 失败: %w", m.deviceName, err)
	}
	if readBack != on {
		err := fmt.Errorf("回读验证失败：写线圈 0x%04X=%v，回读 %v", addr, on, readBack)
		m.saveOp(metamanager.DirDown, fmt.Sprintf("coil:0x%04X", addr),
			fmt.Sprintf("%v", on), "error", err.Error())
		return mapper.DeviceReport{}, err
	}
	m.saveOp(metamanager.DirDown, fmt.Sprintf("coil:0x%04X", addr),
		fmt.Sprintf("%v", on), "ok", "写线圈，回读验证一致")
	return m.snapshot()
}

// snapshot 返回当前状态快照（DeviceReport，reportedAt 取当前毫秒时间戳）。
// 先采集最新属性值；设备不可达时返回错误（写指令成功但快照失败仍应让
// 云端感知设备异常）。
func (m *ModbusMapper) snapshot() (mapper.DeviceReport, error) {
	props, err := m.Collect()
	if err != nil {
		return mapper.DeviceReport{}, err
	}
	return mapper.DeviceReport{
		DeviceName: m.deviceName,
		Namespace:  m.namespace,
		Properties: props,
		ReportedAt: time.Now().UnixMilli(),
	}, nil
}

// saveOp 记录一条台账（方向/寄存器地址/值/结果/消息）。未装配台账时静默跳过；
// 台账写入失败只告警（不影响主链路）。
func (m *ModbusMapper) saveOp(direction, regAddr, value, result, msg string) {
	if m.ledger == nil {
		return
	}
	rec := metamanager.OpRecord{
		Ts:        time.Now().UnixMilli(),
		DeviceID:  m.deviceName,
		Direction: direction,
		RegAddr:   regAddr,
		Value:     value,
		Result:    result,
		Message:   msg,
	}
	if serr := m.ledger.SaveOp(rec); serr != nil {
		log.Warnf("ModbusMapper %s: 台账记录失败: %v", m.deviceName, serr)
	}
}
