// Package mqtt 提供 MQTT 设备 Mapper（v0.24.0，F-2 适配器）。
//
// 与 Modbus/OPC-UA 的轮询型采集不同，MQTT Mapper 是订阅型：设备把属性
// 上报到 broker，本 Mapper 经 pkg/mqtt（自研 MQTT 3.1.1 客户端，零第三方
// 依赖）订阅数据主题，把 payload（JSON 数字字段或 "k=v" 文本，容错解析）
// 合并进本地属性快照；Collect() 返回快照副本；HandleCommand() 把云端指令
// 按 DeviceCommand 云边契约 JSON 序列化后发布到命令主题（设备异步执行，
// 无同步回读——这是订阅型设备与寄存器型设备的本质差异）。
//
// 实现 edge/pkg/mapper.DeviceMapper + DeviceNameResolver +
// DeviceNamespaceResolver（路由索引与 modbus/opcua 同款，namespace/
// deviceName 双键路由）。
//
// 连接管理（pkg/mqtt.Client 无自动重连——断开后 Publish/Subscribe 返回
// ErrClientClosed，重连由本 Mapper 的监管 goroutine 负责）：
//   - Start（幂等）：Dial（ClientID "edgeflow-mqtt-mapper-"+deviceName，
//     KeepAlive 30s，CleanSession true）→ 逐 filter Subscribe(qos0) →
//     启动监管 goroutine；
//   - 监管循环：以探针 Publish（空 payload → HealthTopic，qos0）周期探测
//     连接存活。不用 Subscribe 做探针：client 对同一 filter 是 append
//     语义（handlers[filter] 追加 handler），反复重订会使 handler 无限
//     累积；Publish 探针零状态累积，且空消息落在保留主题 edgeflow/mqtt/
//     health 上，无人订阅、运维亦可借它观测 Mapper 存活；
//   - 探针失败（ErrClientClosed 或写失败）→ 关闭旧 client → 2s 后重
//     Dial + 重订阅（全新 client，handlers 干净）；context 取消即退；
//   - Stop（幂等）：取消监管 → client.Close()；快照与订阅绑定，停止即
//     清空（与 opcua 订阅缓存复位同款语义）。
//
// 数据面节奏：订阅回调在 client 读泵 goroutine 上执行（pkg/mqtt 约定），
// 回调只做解析 + 快照合并 + 台账记录（均为本地快速操作），不阻塞泵。
package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
	"edgeflow/pkg/mqtt"
)

// 常量与默认值（与 docs/MAPPER-GUIDE.md MQTT 章节一致）。
const (
	// DefaultName 是 Mapper 注册名（注册表唯一键）。
	DefaultName = "mqtt"
	// DefaultDeviceName 是默认设备名。
	DefaultDeviceName = "mqtt-device-01"
	// DefaultNamespace 是设备默认命名空间。
	DefaultNamespace = "default"

	// EnvBroker 是 broker 地址环境变量（host:port）；非空即装配层注册
	// MQTT Mapper 的开关（与 EDGEFLOW_MODBUS_ADDR / EDGEFLOW_OPCUA_ENDPOINT
	// 的显式 opt-in 惯例一致）。
	EnvBroker = "EDGEFLOW_MQTT_BROKER"
	// EnvTopics 是订阅 filter 环境变量（逗号分隔，如 "demo/+/state,demo/#"）。
	EnvTopics = "EDGEFLOW_MQTT_TOPICS"
	// EnvDeviceName 是覆盖设备名的环境变量。
	EnvDeviceName = "EDGEFLOW_MQTT_DEVICE_NAME"
	// EnvNamespace 是覆盖设备命名空间的环境变量。
	EnvNamespace = "EDGEFLOW_MQTT_NAMESPACE"
	// EnvCmdTopic 是指令发布主题环境变量（空则按首个订阅 filter 推导）。
	EnvCmdTopic = "EDGEFLOW_MQTT_CMD_TOPIC"

	// DefaultKeepAlive 是 MQTT KeepAlive（30s，PINGREQ 周期）。
	DefaultKeepAlive = 30 * time.Second
	// DefaultConnectTimeout 是单次 Dial 超时（与 modbus/opcua 的 5s 对齐）。
	DefaultConnectTimeout = 5 * time.Second
	// ReconnectInterval 是连接断开后的重试间隔。
	ReconnectInterval = 2 * time.Second
	// ProbeInterval 是存活探针周期（监管循环在连接存活期间使用）。
	ProbeInterval = 2 * time.Second

	// HealthTopic 是存活探针发布主题（空 payload、qos0；无人订阅，
	// 仅作连接活性探测与运维观测）。
	HealthTopic = "edgeflow/mqtt/health"
	// DefaultCmdTopic 是命令主题兜底值（首个 filter 无字面前缀段时使用，
	// 如 filter 仅有 "+"/"#" 通配段）。
	DefaultCmdTopic = "edgeflow/mqtt/cmd"

	// clientIDPrefix 是 MQTT ClientID 前缀（完整 ID = 前缀 + 设备名）。
	clientIDPrefix = "edgeflow-mqtt-mapper-"
)

// DefaultTopics 是 EDGEFLOW_MQTT_TOPICS 未设置时的默认订阅 filter 组。
var DefaultTopics = []string{"devices/+/state"}

// OpLedger 是操作台账的最小接口（metamanager.Ledger 实现；测试可注入
// fake，不强制依赖 SQLite。与 modbus.OpLedger / opcua.OpLedger 同形，
// 各 Mapper 独立声明以保持包间解耦）。
type OpLedger interface {
	SaveOp(rec metamanager.OpRecord) error
}

// Option 是 MQTTMapper 的可选配置项（函数式选项）。
type Option func(*MQTTMapper)

// WithBroker 设置 broker 地址（默认空——New 回退 EnvBroker）。
func WithBroker(addr string) Option {
	return func(m *MQTTMapper) { m.broker = addr }
}

// WithTopics 设置订阅 filter 组（默认空——New 回退 EnvTopics，仍未设置
// 则用 DefaultTopics）。
func WithTopics(topics []string) Option {
	return func(m *MQTTMapper) { m.topics = append([]string(nil), topics...) }
}

// WithDeviceName 设置设备名（默认 mqtt-device-01）。
func WithDeviceName(name string) Option {
	return func(m *MQTTMapper) { m.deviceName = name }
}

// WithNamespace 设置设备命名空间（默认 default）。
func WithNamespace(ns string) Option {
	return func(m *MQTTMapper) { m.namespace = ns }
}

// WithKeepAlive 设置 MQTT KeepAlive（默认 30s）。
func WithKeepAlive(d time.Duration) Option {
	return func(m *MQTTMapper) { m.keepAlive = d }
}

// WithLedger 设置操作台账（nil = 不记录，测试可注入 fake）。
func WithLedger(l OpLedger) Option {
	return func(m *MQTTMapper) { m.ledger = l }
}

// 编译期断言：MQTTMapper 实现 Mapper 框架接口（DeviceNameResolver 声明
// 设备名、DeviceNamespaceResolver 声明命名空间，注册表据此建立
// 「namespace/deviceName」路由索引，与 modbus/opcua 同款）。
var (
	_ mapper.DeviceMapper            = (*MQTTMapper)(nil)
	_ mapper.DeviceNameResolver      = (*MQTTMapper)(nil)
	_ mapper.DeviceNamespaceResolver = (*MQTTMapper)(nil)
)

// MQTTMapper 是 MQTT 订阅型设备 Mapper。
//
// 并发模型：m.mu 保护 props 快照、client 指针与 started 生命周期位；
// 监管 goroutine 独占连接的建立/更换/关闭（经 mu 串行化对外可见状态），
// 订阅回调与 Collect/HandleCommand 只取 client 快照后调用（pkg/mqtt
// Client 自身并发安全：写路径 writeMu 串行化）。
type MQTTMapper struct {
	mu         sync.Mutex
	broker     string
	topics     []string
	cmdTopic   string
	deviceName string
	namespace  string
	keepAlive  time.Duration
	ledger     OpLedger

	props  map[string]float64 // 属性快照（订阅推送合并；Stop 时清空）
	client *mqtt.Client       // 当前连接（nil = 未连接）

	started bool
	cancel  context.CancelFunc // 监管 goroutine 的取消函数（Start 时创建）
	done    chan struct{}      // 监管 goroutine 退出信号（Start 时创建）
}

// New 创建 MQTT Mapper（未启动）。broker 为空时回退 EnvBroker；
// topics/设备名/命名空间/命令主题的解析优先级均为：With 选项 >
// 环境变量 > 默认值（命令主题 EnvCmdTopic 优先，其次按首个订阅 filter
// 推导，见 CmdTopicFromFilter）。broker 允许为空串构造（注册门控由
// 装配层负责；空 broker 只会让监管循环持续重试告警）。
func New(broker string, opts ...Option) *MQTTMapper {
	if broker == "" {
		broker = os.Getenv(EnvBroker)
	}
	m := &MQTTMapper{
		broker:     broker,
		keepAlive:  DefaultKeepAlive,
		deviceName: DefaultDeviceName,
		namespace:  DefaultNamespace,
		props:      make(map[string]float64),
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
	if m.deviceName == "" {
		m.deviceName = os.Getenv(EnvDeviceName)
	}
	if m.deviceName == "" {
		m.deviceName = DefaultDeviceName
	}
	if len(m.topics) == 0 {
		m.topics = ParseTopics(os.Getenv(EnvTopics))
	}
	if len(m.topics) == 0 {
		m.topics = append([]string(nil), DefaultTopics...)
	}
	if m.keepAlive <= 0 {
		m.keepAlive = DefaultKeepAlive
	}
	if m.cmdTopic == "" {
		m.cmdTopic = os.Getenv(EnvCmdTopic)
	}
	if m.cmdTopic == "" {
		m.cmdTopic = CmdTopicFromFilter(m.topics[0])
	}
	return m
}

// Name 返回注册名（注册表唯一键）。
func (m *MQTTMapper) Name() string { return DefaultName }

// DeviceNames 声明本 Mapper 管理的设备名（单设备：一个订阅组对应一台
// 逻辑设备，多台物理设备经通配 filter 汇入同一快照）。
func (m *MQTTMapper) DeviceNames() []string { return []string{m.deviceName} }

// DeviceNamespace 返回设备所属命名空间（New 已保证恒非空，与注册表
// 索引键、DeviceReport.Namespace 同源一致）。
func (m *MQTTMapper) DeviceNamespace() string { return m.namespace }

// Broker 返回 broker 地址（诊断/测试用）。
func (m *MQTTMapper) Broker() string { return m.broker }

// Topics 返回订阅 filter 副本（诊断/测试用）。
func (m *MQTTMapper) Topics() []string { return append([]string(nil), m.topics...) }

// CommandTopic 返回指令发布主题（诊断/装配日志用）。
func (m *MQTTMapper) CommandTopic() string { return m.cmdTopic }

// ParseTopics 解析 EDGEFLOW_MQTT_TOPICS：逗号分隔的订阅 filter，
// 逐项去首尾空白，空条目丢弃；全空返回 nil（调用方回退默认值）。
func ParseTopics(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, item)
	}
	return out
}

// CmdTopicFromFilter 从订阅 filter 推导默认命令主题：按 '/' 分段、
// 去掉通配段（"+"、"#"）与空段，剩余字面段用 '/' 拼接为前缀后追加
// "/cmd"；无字面段（如 "+"、"#"、空串）回退 DefaultCmdTopic。
// 例："demo/+/state" → "demo/state/cmd"，"demo/#" → "demo/cmd"。
func CmdTopicFromFilter(filter string) string {
	lit := make([]string, 0, 4)
	for _, seg := range strings.Split(strings.TrimSpace(filter), "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "+" || seg == "#" {
			continue
		}
		lit = append(lit, seg)
	}
	if len(lit) == 0 {
		return DefaultCmdTopic
	}
	return strings.Join(lit, "/") + "/cmd"
}

// Start 启动 Mapper（幂等）：拨号 + 订阅由监管 goroutine 全权负责，
// Start 本身立即返回——broker 未就绪（晚于 edgecore 启动）不阻塞装配，
// 监管循环会以 ReconnectInterval 周期重试直至连上。context 取消同时
// 终止监管循环（框架 StopAll 之外的兜底退出路径）。
func (m *MQTTMapper) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	supCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	m.started = true
	m.mu.Unlock()

	log.Infof("MQTTMapper %s 启动（broker=%s，订阅 [%s]，cmd=%s，keepAlive=%s）",
		m.deviceName, m.broker, strings.Join(m.topics, ","), m.cmdTopic, m.keepAlive)
	go m.supervise(supCtx)
	return nil
}

// Stop 停止 Mapper（幂等）：取消监管 goroutine 并等待其退出（最坏阻塞
// 一个 ConnectTimeout，用于 shutdown 可接受），随后兜底关闭残留连接。
// 快照与订阅绑定：停止即清空（与 opcua 订阅缓存复位同款语义），重启后
// 由设备上报重新填充。
func (m *MQTTMapper) Stop() error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = false
	cancel, done := m.cancel, m.done
	m.props = make(map[string]float64)
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	m.closeCurrent() // 监管退出路径已清理，此处幂等兜底
	log.Infof("MQTTMapper %s 已停止", m.deviceName)
	return nil
}

// supervise 是监管循环：Dial + 重订阅 → 存活探测 → 断开重连，直至
// context 取消。所有退出路径保证关闭已建立的连接（done 关闭前完成）。
func (m *MQTTMapper) supervise(ctx context.Context) {
	defer close(m.done)
	for {
		if ctx.Err() != nil {
			m.closeCurrent()
			return
		}
		cl, err := m.connect()
		if err != nil {
			if ctx.Err() != nil {
				return // connect 内部已关闭半建立连接
			}
			log.Warnf("MQTTMapper %s: 连接 broker %s 失败（%v），%s 后重试",
				m.deviceName, m.broker, err, ReconnectInterval)
			if !sleepCtx(ctx, ReconnectInterval) {
				return
			}
			continue
		}
		m.setClient(cl)
		log.Infof("MQTTMapper %s 已连接 broker %s（订阅 %d 个 filter）",
			m.deviceName, m.broker, len(m.topics))

		dead := m.monitor(ctx, cl)
		m.clearClient(cl) // 断开与 ctx 取消两路都关闭换出
		if !dead {
			return // context 取消
		}
		log.Warnf("MQTTMapper %s: 与 broker %s 的连接断开，%s 后重连",
			m.deviceName, m.broker, ReconnectInterval)
		if !sleepCtx(ctx, ReconnectInterval) {
			return
		}
	}
}

// connect 建立新连接并逐 filter 重订阅（全新 client，handlers 干净，
// 无重订阅累积问题）。任一 filter 订阅失败即关闭半建立连接并报错。
func (m *MQTTMapper) connect() (*mqtt.Client, error) {
	opts := mqtt.Options{
		ClientID:       clientIDPrefix + m.deviceName,
		KeepAlive:      m.keepAlive,
		CleanSession:   true,
		ConnectTimeout: DefaultConnectTimeout,
	}
	cl, err := mqtt.Dial(m.broker, opts)
	if err != nil {
		return nil, fmt.Errorf("拨号 %s 失败: %w", m.broker, err)
	}
	for _, f := range m.topics {
		if err := cl.Subscribe(f, 0, m.onMessage); err != nil {
			_ = cl.Close()
			return nil, fmt.Errorf("订阅 %s 失败: %w", f, err)
		}
	}
	return cl, nil
}

// monitor 在连接存活期间周期探测：向 HealthTopic 发布空消息（qos0），
// 失败（ErrClientClosed——读泵已因断开/关闭退出，或写失败）即判定
// 断开。返回 true = 断开重连，false = context 取消。不用 Subscribe
// 探测的原因见包注释（handlers append 语义会无限累积）。
func (m *MQTTMapper) monitor(ctx context.Context, cl *mqtt.Client) bool {
	timer := time.NewTimer(ProbeInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
		if err := cl.Publish(HealthTopic, 0, nil); err != nil {
			return true
		}
		timer.Reset(ProbeInterval)
	}
}

// sleepCtx 睡眠 d 或 ctx 取消；返回 false 表示 ctx 已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// setClient 装填新连接（仅监管 goroutine 调用）。
func (m *MQTTMapper) setClient(cl *mqtt.Client) {
	m.mu.Lock()
	m.client = cl
	m.mu.Unlock()
}

// clearClient 关闭 cl 并在仍是当前连接时换出（身份校验：避免误关
// 监管循环刚换上的新连接）。Close 幂等（closeOnce）。
func (m *MQTTMapper) clearClient(cl *mqtt.Client) {
	m.mu.Lock()
	if m.client == cl {
		m.client = nil
	}
	m.mu.Unlock()
	_ = cl.Close()
}

// closeCurrent 关闭并卸载当前连接（Stop 兜底 / 监管退出清理用）。
func (m *MQTTMapper) closeCurrent() {
	m.mu.Lock()
	cl := m.client
	m.client = nil
	m.mu.Unlock()
	if cl != nil {
		_ = cl.Close()
	}
}

// clientSnapshot 返回当前连接快照（nil = 未连接）。
func (m *MQTTMapper) clientSnapshot() *mqtt.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.client
}

// onMessage 是订阅回调（client 读泵 goroutine 上执行）：容错解析
// payload → 合并进属性快照 → 记台账（方向 up）。解析失败整条跳过
// （记 error 台账，快照不变）；回调只做本地快速操作，不阻塞读泵。
func (m *MQTTMapper) onMessage(topic string, payload []byte) {
	props, err := parsePayload(payload)
	if err != nil {
		log.Warnf("MQTTMapper %s: %s payload 解析失败，跳过（%v）", m.deviceName, topic, err)
		m.saveOp(metamanager.DirUp, topic, truncateForLog(payload), "error", err.Error())
		return
	}
	m.mu.Lock()
	for k, v := range props {
		m.props[k] = v
	}
	m.mu.Unlock()
	m.saveOp(metamanager.DirUp, topic, formatProps(props), "ok",
		fmt.Sprintf("收到上报 %d 个属性: %s", len(props), formatProps(props)))
}

// Collect 返回属性快照副本（订阅型采集：设备推送 → 快照，本方法只读
// 缓存，恒成功——空快照是"尚未收到上报"的合法状态）。
func (m *MQTTMapper) Collect() (map[string]float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(m.props))
	for k, v := range m.props {
		out[k] = v
	}
	return out, nil
}

// HandleCommand 处理云端下发的指令：把 DeviceCommand 按云边契约 JSON
// （deviceName/namespace/property/value，mapper.DeviceCommand 的原生
// 序列化，与指令下发链路的字段一致）发布到 cmdTopic（qos0），设备
// 异步执行后把新状态上报数据主题即完成闭环；本方法随即返回当前快照
// （订阅型设备无同步回读，这是与 modbus/opcua 的语义差异）。
// 设备名不符、property 缺失、未连接 broker 均返回错误（云端 502）。
func (m *MQTTMapper) HandleCommand(cmd mapper.DeviceCommand) (mapper.DeviceReport, error) {
	if cmd.DeviceName != "" && cmd.DeviceName != m.deviceName {
		return mapper.DeviceReport{}, fmt.Errorf("指令设备名 %q 与本 Mapper 设备 %q 不符",
			cmd.DeviceName, m.deviceName)
	}
	if cmd.Property == "" {
		return mapper.DeviceReport{}, errors.New("指令缺少 property 字段")
	}
	cl := m.clientSnapshot()
	if cl == nil {
		return mapper.DeviceReport{}, fmt.Errorf("MQTT Mapper %s 未连接 broker，指令暂不可下发", m.deviceName)
	}
	payload, err := json.Marshal(cmd)
	if err != nil {
		return mapper.DeviceReport{}, fmt.Errorf("指令序列化失败: %w", err)
	}
	if err := cl.Publish(m.cmdTopic, 0, payload); err != nil {
		m.saveOp(metamanager.DirDown, m.cmdTopic, string(payload), "error", err.Error())
		return mapper.DeviceReport{}, fmt.Errorf("发布指令到 %s 失败: %w", m.cmdTopic, err)
	}
	m.saveOp(metamanager.DirDown, m.cmdTopic, string(payload), "ok",
		fmt.Sprintf("指令已发布（%s=%v）", cmd.Property, cmd.Value))
	return m.snapshot(), nil
}

// snapshot 返回当前状态快照（DeviceReport，reportedAt 取当前毫秒时间戳）。
// Collect 恒成功，此处忽略 error。
func (m *MQTTMapper) snapshot() mapper.DeviceReport {
	props, _ := m.Collect()
	return mapper.DeviceReport{
		DeviceName: m.deviceName,
		Namespace:  m.namespace,
		Properties: props,
		ReportedAt: time.Now().UnixMilli(),
	}
}

// saveOp 记录一条台账（方向/主题/值/结果/消息）。未装配台账时静默跳过；
// 台账写入失败只告警（不影响主链路）。与 modbus/opcua 同款。
func (m *MQTTMapper) saveOp(direction, regAddr, value, result, msg string) {
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
		log.Warnf("MQTTMapper %s: 台账记录失败: %v", m.deviceName, serr)
	}
}

// ---------------------------------------------------------------------
// payload 容错解析：设备固件五花八门，上报格式不强制——优先 JSON 对象
// （顶层可转数值字段），退回 "k=v" 文本；两种形式都解析不出属性则整条
// 跳过（上层记 error 台账，快照不变）。
// ---------------------------------------------------------------------

// parsePayload 解析设备上报 payload：
//   - JSON 对象：仅取顶层可转数值的字段（Number 直接转；字符串尝试
//     ParseFloat；布尔/嵌套对象/数组/null 等不可转字段跳过）；
//   - "k=v" 文本：按逗号/分号/空白/换行分词，含 '=' 的词解析键值
//     （值需可 ParseFloat），其余词跳过。
//
// 任一形式解析出 ≥1 个属性即成功；否则返回错误（调用方跳过该条消息）。
func parsePayload(payload []byte) (map[string]float64, error) {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return nil, errors.New("空 payload")
	}
	if strings.HasPrefix(trimmed, "{") {
		if props, ok := parseJSONProps(trimmed); ok {
			return props, nil
		}
	}
	if props, ok := parseKVProps(trimmed); ok {
		return props, nil
	}
	return nil, fmt.Errorf("无法从 payload 解析出任何属性: %s", truncateForLog(payload))
}

// parseJSONProps 解析 JSON 对象的顶层可转数值字段（UseNumber 保留数值
// 精度，避免 float64 二次转换损失）。解析不出任何属性返回 false。
func parseJSONProps(s string) (map[string]float64, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	raw := make(map[string]any)
	if err := dec.Decode(&raw); err != nil {
		return nil, false
	}
	props := make(map[string]float64, len(raw))
	for k, v := range raw {
		if f, ok := jsonToFloat(v); ok {
			props[k] = f
		}
	}
	if len(props) == 0 {
		return nil, false
	}
	return props, true
}

// jsonToFloat 把 JSON 字段值转换为 float64：Number 直接转；字符串尝试
// ParseFloat（固件把数字序列化成字符串的容错）；其余类型不支持。
func jsonToFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// parseKVProps 解析 "k=v" 文本：分隔符支持逗号/分号/空白/换行（固件
// 常见 "k1=v1,k2=v2" 与 "k1=v1 k2=v2" 两种）。键为空、无 '='、值非
// 数值的词跳过；解析不出任何属性返回 false。
func parseKVProps(s string) (map[string]float64, bool) {
	props := make(map[string]float64)
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	}) {
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 { // 无 '=' 或键为空（"=v"）都跳过
			continue
		}
		k := strings.TrimSpace(tok[:eq])
		f, err := strconv.ParseFloat(strings.TrimSpace(tok[eq+1:]), 64)
		if err != nil || k == "" {
			continue
		}
		props[k] = f
	}
	if len(props) == 0 {
		return nil, false
	}
	return props, true
}

// formatProps 把属性 map 格式化为稳定排序的 "k=v" 串（台账/日志用，
// 保证同一快照的消息内容确定）。
func formatProps(props map[string]float64) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, props[k]))
	}
	return strings.Join(parts, ",")
}

// truncateForLog 截断 payload 用于错误消息（防超长 payload 刷爆日志）。
func truncateForLog(b []byte) string {
	const max = 64
	s := string(b)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
