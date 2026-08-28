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
	"sort"
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
	// EnvSubscription 是订阅模式开关（v0.15.0）：取值 on/off，缺省 off
	// （轮询模式，行为与 v0.14.0 逐字节一致）。on 时经 OPC-UA Subscription
	// 接收数据变更推送，Collect() 返回最近通知缓存快照。
	EnvSubscription = "EDGEFLOW_OPCUA_SUBSCRIPTION"
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

	// 订阅模式（v0.15.0，EnvSubscription=on）
	subOn     bool
	subValues map[string]float64
	subDirty  bool
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
	m.subValues = make(map[string]float64)
	return m, nil
}

// SubscriptionEnabled 读取订阅开关（env on/off；缺省 off）。
func SubscriptionEnabled() bool {
	switch strings.ToLower(os.Getenv(EnvSubscription)) {
	case "on", "true", "1":
		return true
	}
	return false
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
	if SubscriptionEnabled() {
		if err := m.StartSubscription(); err != nil {
			log.Warnf("OPCUAMapper %s: 订阅模式启用失败（%v），降级为轮询采集",
				m.deviceName, err)
		}
	}
	return nil
}

// Stop 停止 Mapper（幂等）：关闭客户端连接。
// PRT-20：锁内仅做状态翻转与 client 指针快照+置空，Close 移到锁外——
// 旧实现持锁 Close（最坏 5s），且订阅循环的 PubAck 不持锁读 m.client，
// 与"置 nil"存在 nil 解引用竞态；先翻转 started/subOn 并置 nil 再 Close，
// 循环体经锁内快照拿到旧 client（PubAck 对已关连接返回错误，安全吞掉）
// 或拿到 nil（跳过），不再 panic。
func (m *OPCUAMapper) Stop() error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = false
	m.subOn = false
	m.subValues = make(map[string]float64)
	cl := m.client
	m.client = nil
	m.mu.Unlock()
	if cl != nil {
		_ = cl.Close() // Close 内含 DeleteSubscriptions=true
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
// 调用方需保证 op 不再次加锁（本方法持有 m.mu）。重试与连接管理可能
// 阻塞至多一个超时周期，参见 rebuildSubscription（PRT-19）与
// ModbusMapper.withConn（PRT-21）的预算约束说明。
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
	if props, ok := m.collectFromCache(); ok {
		return props, nil // 订阅模式：推送缓存快照
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
	// 订阅模式下同步刷新推送缓存：写点立即生效，不必等服务端下一拍
	// 数据通知（可写节点本轮变化也可能被 KeepAlive 间隔吞掉首拍）。
	m.mu.Lock()
	if m.subOn {
		m.subValues[pt.Name] = f
		m.subDirty = true
	}
	m.mu.Unlock()
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

// ---------------------------------------------------------------------
// 订阅模式（v0.15.0）：EDGEFLOW_OPCUA_SUBSCRIPTION=on 时启用。
// 通知经 OPC-UA Subscription 推送至缓存；Collect() 返回缓存快照；
// 轮询路径（off，缺省）不经过以下任何代码。
// ---------------------------------------------------------------------

// StartSubscription 启用订阅采集：建立订阅并把全部点位登记为监测项，
// 后台 goroutine 消费通知刷新缓存。幂等（重复调用返回 nil）。
func (m *OPCUAMapper) StartSubscription() error {
	m.mu.Lock()
	if m.subOn {
		m.mu.Unlock()
		return nil
	}
	// 借用 withClient 的连接（先确保在线）
	if err := m.ensureClient(); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("订阅前连接失败: %w", err)
	}
	nodes := make([]opcuapkg.NodeId, 0, len(m.points))
	for _, p := range m.points {
		nodes = append(nodes, p.NodeID)
	}
	ch, err := m.client.Subscribe(nodes, 0)
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("创建订阅失败: %w", err)
	}
	m.subOn = true
	m.mu.Unlock()

	go m.subscriptionLoop(ch)
	log.Infof("OPCUAMapper %s 订阅模式已启用（点位 %d 个）", m.deviceName, len(m.points))
	return nil
}

// subscriptionLoop 消费推送通知：数据变更→缓存+台账；状态变更/gap→重建。
// PRT-18：client 泵异常退出/Close 时会关闭 pubCh（v0.21.0 起），
// 本循环可经 range 感知关闭退出，不再永久阻塞；退出后按既有
// rebuildSubscription 节奏重建订阅（改动最小方案）。
func (m *OPCUAMapper) subscriptionLoop(ch <-chan opcuapkg.PublishResult) {
	for pr := range ch {
		if pr.IsStatusChange {
			log.Warnf("OPCUAMapper %s: 订阅状态变更（%s），重建订阅", m.deviceName, pr.StatusChange)
			m.rebuildSubscription()
			return
		}
		if pr.KeepAlive {
			continue
		}
		m.mu.Lock()
		for _, item := range pr.DataChange {
			// clientHandle → 点位名（handle 从 1 起，顺序即 points 序）
			idx := int(item.ClientHandle) - 1
			if idx < 0 || idx >= len(m.points) {
				continue
			}
			name := m.points[idx].Name
			dv := item.Value
			if dv.Status != nil && !dv.Status.IsGood() {
				continue // Bad 节点跳过（与轮询口径一致）
			}
			if dv.Value == nil {
				continue
			}
			if f, ok := variantToFloat(dv.Value.Value); ok {
				m.subValues[name] = f
				m.subDirty = true
			}
		}
		dirty := m.subDirty
		points := nodeIDs(m.points)
		vals := make([]string, 0, len(m.subValues))
		for k, v := range m.subValues {
			vals = append(vals, fmt.Sprintf("%s=%.3f", k, v))
		}
		sort.Strings(vals)
		m.mu.Unlock()
		if dirty {
			m.saveOp(metamanager.DirUp, points, "", "ok",
				fmt.Sprintf("订阅推送 %d 个点位: %s", len(vals), strings.Join(vals, ",")))
			m.mu.Lock()
			m.subDirty = false
			m.mu.Unlock()
		}
		m.mu.Lock()
		cl := m.client
		m.mu.Unlock()
		if cl != nil {
			_ = cl.PubAck() // 补挂下一条 Publish（维持发布窗口）
		}
	}
	// 通道关闭：泵异常退出或客户端已 Close。订阅模式仍在开启（subOn）
	// 时按既有节奏重连重建；Stop 已关订阅则静默收尾。
	m.mu.Lock()
	on := m.subOn
	m.mu.Unlock()
	if on {
		log.Warnf("OPCUAMapper %s: 订阅通道关闭（泵退出/连接断开），重建订阅", m.deviceName)
		m.rebuildSubscription()
		return
	}
	log.Infof("OPCUAMapper %s: 订阅通道关闭，订阅已停止", m.deviceName)
}

// rebuildSubscription 断线/状态变更后重建：换出死连接→重连→重新 Subscribe。
// PRT-19：旧实现全程持 m.mu——先在锁内对死连接逐协议清理（每步至多
// m.timeout，叠加可达 15-20s），再持锁拨号/订阅（黑洞端点再挂 10s），
// 期间并发 Collect/HandleCommand 全部被阻塞。现在锁内只做缓存复位与
// client 指针换出；死连接的协议级清理（DeleteSubscription+Close，死 TCP
// 上快速失败）、重连拨号、重订阅全部在锁外执行，仅最后装填新 client
// 时短暂持锁。装填时校验 Mapper 仍启用（Stop 已发生则丢弃新连接）且
// 无人抢先装填（并发 Start/重建 winner check），防泄漏。清理失败只影响
// 服务端订阅随 lifetimeCount 自然过期，可观测性由探针兜底。
func (m *OPCUAMapper) rebuildSubscription() {
	m.mu.Lock()
	m.subValues = make(map[string]float64)
	m.subDirty = false
	dead := m.client
	m.client = nil
	m.mu.Unlock()
	if dead != nil {
		go func(cl *opcuapkg.Client) {
			_ = cl.DeleteSubscription()
			_ = cl.Close()
		}(dead)
	}
	nodes := make([]opcuapkg.NodeId, 0, len(m.points))
	for _, p := range m.points {
		nodes = append(nodes, p.NodeID)
	}
	// 锁外重连（PRT-19）：拨号受传输层超时约束，不再占用 m.mu。
	c, err := opcuapkg.Open(m.endpoint, m.timeout)
	if err != nil {
		log.Warnf("OPCUAMapper %s: 订阅重建连接失败（%v），将在操作时重试", m.deviceName, err)
		return
	}
	ch, err := c.Subscribe(nodes, 0)
	if err != nil {
		_ = c.Close()
		log.Warnf("OPCUAMapper %s: 订阅重建失败（%v）", m.deviceName, err)
		return
	}
	m.mu.Lock()
	if !m.started || m.client != nil {
		// 重建期间发生了 Stop（或并发路径抢先装填）：丢弃新连接防泄漏。
		m.mu.Unlock()
		_ = c.Close()
		return
	}
	m.client = c
	m.mu.Unlock()
	go m.subscriptionLoop(ch)
	log.Infof("OPCUAMapper %s 订阅已重建", m.deviceName)
}

// collectFromCache 返回订阅缓存快照（Collect 在订阅模式下的短路路径）。
func (m *OPCUAMapper) collectFromCache() (map[string]float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.subOn {
		return nil, false
	}
	out := make(map[string]float64, len(m.subValues))
	for k, v := range m.subValues {
		out[k] = v
	}
	return out, true
}
