// Package opcua 提供 OPC-UA 设备 Mapper（WBS 5.2 第二阶段，v0.14.0）。
//
// 实现 edge/pkg/mapper.DeviceMapper 接口，管理 1 台 OPC-UA 设备
// （默认 opcua-device-01）：Collect 批量读点位并转换为 map[string]float64，
// HandleCommand 写点位（setpoint 等可写节点）+ 回读验证。每次操作写入
// 操作台账（edge/pkg/metamanager.Ledger，SQLite 持久化，保留 30 天）。
//
// 依赖：全标准库（pkg/opcua 自研 UA Binary 协议栈 + Client，零第三方
// 依赖规则保持；与 Modbus Mapper 依赖 goburrow/modbus 不同——OPC-UA
// 客户端是自实现的）。
//
// 连接管理：
//   - TCP 长连接，端点 env EDGEFLOW_OPCUA_ENDPOINT（opc.tcp://host:port），
//     操作超时 5s；
//   - 每次操作前确保连接（opcua.Open 幂等，未连接才拨号；断连自动重连）；
//   - 传输层错误 → 断开旧客户端重连并重试一次；
//     服务端拒绝（Results 非 Good，如 BadNodeIdUnknown / BadNotWritable）
//     是设备已应答的业务错误，不重试，直接返回。
package opcua

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

	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
	opcuapkg "edgeflow/pkg/opcua"
)

// 常量与默认值（与 docs/OPCUA-GUIDE.md 一致）。
const (
	// DefaultName 是 Mapper 注册名（注册表唯一键）。
	DefaultName = "opcua-mapper"
	// DefaultDeviceName 是默认设备名。
	DefaultDeviceName = "opcua-device-01"
	// DefaultNamespace 是设备默认命名空间。
	DefaultNamespace = "default"
	// EnvEndpoint 是覆盖设备端点 URL 的环境变量名（非空即注册 OPC-UA Mapper）。
	EnvEndpoint = "EDGEFLOW_OPCUA_ENDPOINT"
	// EnvNodes 是点位表环境变量（逗号分隔 name=nodeId）。
	EnvNodes = "EDGEFLOW_OPCUA_NODES"
	// EnvDeviceName 是覆盖设备名的环境变量。
	EnvDeviceName = "EDGEFLOW_OPCUA_DEVICE_NAME"
	// EnvNamespace 是覆盖设备命名空间的环境变量。
	EnvNamespace = "EDGEFLOW_OPCUA_NAMESPACE"
	// DefaultTimeout 是单次操作超时（连接 + 握手 + 读写）。
	DefaultTimeout = 5 * time.Second
)

// OpLedger 是操作台账的最小接口（metamanager.Ledger 实现；
// 测试可注入 fake，与 modbus.OpLedger 同款）。
type OpLedger interface {
	SaveOp(rec metamanager.OpRecord) error
}

// PointDef 是点位定义：上报属性名 + 读/写目标节点。
type PointDef struct {
	Name   string          // 上报属性名（DeviceReport.Properties 键）
	NodeID opcuapkg.NodeId // 读/写目标节点
}

// Option 是 OPCUAMapper 的可选配置项（函数式选项）。
type Option func(*OPCUAMapper)

// WithPoints 设置点位表（默认空——装配时从 EDGEFLOW_OPCUA_NODES 解析）。
func WithPoints(points []PointDef) Option {
	return func(m *OPCUAMapper) { m.points = points }
}

// WithDeviceName 设置设备名（默认 opcua-device-01）。
func WithDeviceName(name string) Option {
	return func(m *OPCUAMapper) { m.deviceName = name }
}

// WithNamespace 设置设备命名空间（默认 default）。
func WithNamespace(ns string) Option {
	return func(m *OPCUAMapper) { m.namespace = ns }
}

// WithTimeout 设置单次操作超时（默认 5s）。
func WithTimeout(d time.Duration) Option {
	return func(m *OPCUAMapper) { m.timeout = d }
}

// WithLedger 设置操作台账（nil = 不记录，测试可注入 fake）。
func WithLedger(l OpLedger) Option {
	return func(m *OPCUAMapper) { m.ledger = l }
}

// 编译期断言：OPCUAMapper 实现 Mapper 框架接口。
var (
	_ mapper.DeviceMapper            = (*OPCUAMapper)(nil)
	_ mapper.DeviceNameResolver      = (*OPCUAMapper)(nil)
	_ mapper.DeviceNamespaceResolver = (*OPCUAMapper)(nil)
)

// OPCUAMapper 是 OPC-UA 设备 Mapper。
type OPCUAMapper struct {
	mu         sync.Mutex // 串行化连接状态与读写（重连逻辑需要独占连接）
	endpoint   string
	points     []PointDef
	timeout    time.Duration
	deviceName string
	namespace  string
	ledger     OpLedger

	client  *opcuapkg.Client
	started bool
}

// New 创建 OPC-UA Mapper（未启动）。endpoint 为空时从环境变量
// EDGEFLOW_OPCUA_ENDPOINT 读取；仍未设置则按错误处理（缺省不启用，
// 由装配层门控）。
func New(endpoint string, opts ...Option) (*OPCUAMapper, error) {
	if endpoint == "" {
		endpoint = os.Getenv(EnvEndpoint)
	}
	if endpoint == "" {
		return nil, errors.New("OPC-UA Mapper 缺少端点（EDGEFLOW_OPCUA_ENDPOINT 未设置）")
	}
	m := &OPCUAMapper{
		endpoint:   endpoint,
		timeout:    DefaultTimeout,
		deviceName: DefaultDeviceName,
		namespace:  DefaultNamespace,
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
	if dn := os.Getenv(EnvDeviceName); dn != "" {
		m.deviceName = dn
	}
	return m, nil
}

// Name 返回注册名（注册表唯一键）。
func (m *OPCUAMapper) Name() string { return DefaultName }

// DeviceNames 声明本 Mapper 管理的设备名。
func (m *OPCUAMapper) DeviceNames() []string { return []string{m.deviceName} }

// DeviceNamespace 返回设备所属命名空间。
func (m *OPCUAMapper) DeviceNamespace() string { return m.namespace }

// Start 启动 Mapper（幂等）：做一次尽力而为的预连接。
// 设备未就绪只告警不报错——操作时按需重连。
func (m *OPCUAMapper) Start(_ context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	m.mu.Unlock()

	log.Infof("OPCUAMapper %s 启动（endpoint=%s, timeout=%s, 点位 %d 个）",
		m.deviceName, m.endpoint, m.timeout, len(m.points))
	if err := m.ensureClient(); err != nil {
		log.Warnf("OPCUAMapper %s: 预连接 %s 失败（%v），操作时将自动重连",
			m.deviceName, m.endpoint, err)
	}
	return nil
}

// Stop 停止 Mapper（幂等）：关闭客户端连接。
func (m *OPCUAMapper) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return nil
	}
	m.started = false
	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
	}
	log.Infof("OPCUAMapper %s 已停止", m.deviceName)
	return nil
}

// ensureClient 保证客户端已连接（未连接才 opcua.Open）。
// 调用方需持有 m.mu。
func (m *OPCUAMapper) ensureClient() error {
	if m.client != nil {
		return nil
	}
	c, err := opcuapkg.Open(m.endpoint, m.timeout)
	if err != nil {
		return err
	}
	m.client = c
	return nil
}

// withClient 保证连接就绪后执行 op；传输层失败时断开重连并重试一次。
// 调用方需保证 op 不再次加锁（本方法持有 m.mu）。
func (m *OPCUAMapper) withClient(op func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureClient(); err != nil {
		return fmt.Errorf("连接 OPC-UA 设备 %s 失败: %w", m.endpoint, err)
	}
	if err := op(); err != nil {
		// 传输层错误：连接可能已失效（设备重启/断网），断开后重连重试一次
		_ = m.client.Close()
		m.client = nil
		if cerr := m.ensureClient(); cerr != nil {
			return fmt.Errorf("重连 OPC-UA 设备 %s 失败: %w（原始错误: %v）", m.endpoint, cerr, err)
		}
		if err2 := op(); err2 != nil {
			return fmt.Errorf("重试后仍失败: %w", err2)
		}
	}
	return nil
}

// Collect 采集设备当前属性值：批量读全部点位并转换。成功后记录台账
// （方向 up，结果 ok）。
func (m *OPCUAMapper) Collect() (map[string]float64, error) {
	if len(m.points) == 0 {
		return map[string]float64{}, nil
	}
	nodes := make([]opcuapkg.NodeId, 0, len(m.points))
	for _, p := range m.points {
		nodes = append(nodes, p.NodeID)
	}
	var vals []opcuapkg.DataValue
	err := m.withClient(func() error {
		var err error
		vals, err = m.client.Read(nodes)
		return err
	})
	if err != nil {
		m.saveOp(metamanager.DirUp, nodeIDs(m.points), "", "error", err.Error())
		return nil, fmt.Errorf("采集 %s 失败: %w", m.deviceName, err)
	}
	props := make(map[string]float64, len(m.points))
	bad := 0
	for i, p := range m.points {
		if i >= len(vals) {
			break
		}
		dv := vals[i]
		if dv.Status != nil && !dv.Status.IsGood() {
			log.Warnf("OPCUAMapper %s: 点位 %s 状态非 Good（%s），跳过",
				m.deviceName, p.Name, *dv.Status)
			bad++
			continue
		}
		if dv.Value == nil {
			log.Warnf("OPCUAMapper %s: 点位 %s 无值，跳过", m.deviceName, p.Name)
			bad++
			continue
		}
		f, ok := variantToFloat(dv.Value.Value)
		if !ok {
			log.Warnf("OPCUAMapper %s: 点位 %s 类型 %T 不支持转换，跳过", m.deviceName, p.Name, dv.Value.Value)
			bad++
			continue
		}
		props[p.Name] = f
	}
	if bad > 0 {
		m.saveOp(metamanager.DirUp, nodeIDs(m.points), "", "ok",
			fmt.Sprintf("采集 %d/%d 个点位（%d 个跳过）", len(props), len(m.points), bad))
	} else {
		m.saveOp(metamanager.DirUp, nodeIDs(m.points), "", "ok",
			fmt.Sprintf("采集 %d 个点位", len(props)))
	}
	return props, nil
}

// HandleCommand 处理云端下发的指令：
//   - property 命中点位名：写该节点（Double 值）+ 回读验证（容差 1e-6）；
//   - 点位不存在 / 服务端拒绝（Results 非 Good）→ 错误（云端 502）；
//   - 未匹配任何点位名 → 错误 "不支持的指令属性"。
//
// 返回处理后的最新状态快照（DeviceReport）。每次写操作记录台账（方向 down）。
func (m *OPCUAMapper) HandleCommand(cmd mapper.DeviceCommand) (mapper.DeviceReport, error) {
	if cmd.DeviceName != "" && cmd.DeviceName != m.deviceName {
		return mapper.DeviceReport{}, fmt.Errorf("指令设备名 %q 与本 Mapper 设备 %q 不符",
			cmd.DeviceName, m.deviceName)
	}
	var pt *PointDef
	for i := range m.points {
		if m.points[i].Name == cmd.Property {
			pt = &m.points[i]
			break
		}
	}
	if pt == nil {
		return mapper.DeviceReport{}, fmt.Errorf("不支持的指令属性 %q（未配置该点位）", cmd.Property)
	}
	v, err := opcuapkg.NewVariant(cmd.Value)
	if err != nil {
		return mapper.DeviceReport{}, fmt.Errorf("构造写值失败: %w", err)
	}
	var st opcuapkg.StatusCode
	err = m.withClient(func() error {
		var err error
		st, err = m.client.Write(pt.NodeID, v)
		return err
	})
	if err != nil {
		m.saveOp(metamanager.DirDown, pt.NodeID.String(), fmt.Sprintf("%v", cmd.Value), "error", err.Error())
		return mapper.DeviceReport{}, fmt.Errorf("写点位 %s 失败: %w", pt.Name, err)
	}
	if !st.IsGood() {
		err := fmt.Errorf("写点位 %s 被服务端拒绝: %s", pt.Name, st)
		m.saveOp(metamanager.DirDown, pt.NodeID.String(), fmt.Sprintf("%v", cmd.Value), "error", err.Error())
		return mapper.DeviceReport{}, err
	}
	// 回读验证写入生效
	if err := m.verifyWrite(*pt, cmd.Value); err != nil {
		m.saveOp(metamanager.DirDown, pt.NodeID.String(), fmt.Sprintf("%v", cmd.Value), "error", err.Error())
		return mapper.DeviceReport{}, err
	}
	m.saveOp(metamanager.DirDown, pt.NodeID.String(), fmt.Sprintf("%v", cmd.Value), "ok",
		fmt.Sprintf("写点位 %s，回读验证一致", pt.Name))
	return m.snapshot()
}

// verifyWrite 回读节点并验证与写入值一致（容差 1e-6）。
func (m *OPCUAMapper) verifyWrite(pt PointDef, want float64) error {
	var dv opcuapkg.DataValue
	err := m.withClient(func() error {
		vals, err := m.client.Read([]opcuapkg.NodeId{pt.NodeID})
		if err != nil {
			return err
		}
		if len(vals) != 1 || vals[0].Value == nil {
			return errors.New("回读结果异常（空值）")
		}
		dv = vals[0]
		return nil
	})
	if err != nil {
		return fmt.Errorf("回读点位 %s 失败: %w", pt.Name, err)
	}
	f, ok := variantToFloat(dv.Value.Value)
	if !ok {
		return fmt.Errorf("回读点位 %s 类型不支持转换（%T）", pt.Name, dv.Value.Value)
	}
	if math.Abs(f-want) > 1e-6 {
		return fmt.Errorf("回读验证失败：写 %v，回读 %v", want, f)
	}
	return nil
}

// snapshot 返回当前状态快照（DeviceReport，reportedAt 取当前毫秒时间戳）。
func (m *OPCUAMapper) snapshot() (mapper.DeviceReport, error) {
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

// saveOp 记录一条台账。未装配台账时静默跳过；写入失败只告警。
func (m *OPCUAMapper) saveOp(direction, regAddr, value, result, msg string) {
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
		log.Warnf("OPCUAMapper %s: 台账记录失败: %v", m.deviceName, serr)
	}
}

// nodeIDs 拼接点位节点标识（台账记录用）。
func nodeIDs(points []PointDef) string {
	ids := make([]string, 0, len(points))
	for _, p := range points {
		ids = append(ids, p.NodeID.String())
	}
	return strings.Join(ids, ",")
}

// variantToFloat 把 UA Variant 值转换为 float64（采集转换的契约）。
// 支持数值类型与 Boolean（true→1/false→0）；String 尝试 ParseFloat；
// 其余类型（DateTime/Guid/ByteString/NodeId/StatusCode/QualifiedName/
// LocalizedText/ExtensionObject/DataValue/数组/Null）不支持 → 返回 false。
func variantToFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// ParseNodes 解析点位表配置（EDGEFLOW_OPCUA_NODES）：
// 逗号分隔 "name=nodeId"（nodeId 内部用 ';'，与逗号不冲突）；
// name 缺省退化为 nodeId 字符串。非法条目整体报错（拒绝注册）。
func ParseNodes(s string) ([]PointDef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []PointDef
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		// 以 "ns=" 开头的条目是退化 nodeId 形式（name=nodeId 字符串）；
		// 否则按第一个 '=' 拆分为 name=nodeId（nodeId 内部含 ';'）。
		name, nodeStr := item, item
		if !strings.HasPrefix(item, "ns=") {
			if eq := strings.IndexByte(item, '='); eq >= 0 {
				name = strings.TrimSpace(item[:eq])
				nodeStr = strings.TrimSpace(item[eq+1:])
			}
		}
		if name == "" || nodeStr == "" {
			return nil, fmt.Errorf("非法点位条目 %q（期望 name=nodeId）", item)
		}
		nid, err := opcuapkg.ParseNodeID(nodeStr)
		if err != nil {
			return nil, fmt.Errorf("点位 %q 节点解析失败: %w", item, err)
		}
		out = append(out, PointDef{Name: name, NodeID: nid})
	}
	return out, nil
}
