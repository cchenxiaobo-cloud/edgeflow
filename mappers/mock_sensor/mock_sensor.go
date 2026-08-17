// Package mocksensor 提供示例模拟设备 Mapper（WBS 5.5）：一个虚拟的
// 温湿度传感器。它实现 edge/pkg/mapper.DeviceMapper 接口，属性随机波动、
// 支持 targetTemp / reset 指令，用于在真实设备接入（Modbus / MQTT）
// 之前打通"云→边指令、边→云上报"的完整链路。
package mocksensor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"edgeflow/edge/pkg/eventbus"
	"edgeflow/edge/pkg/mapper"
	"edgeflow/pkg/log"
)

// 默认参数（对外暴露，便于测试与文档引用）。
const (
	// DefaultName 是 Mapper 的注册名。
	DefaultName = "mock-sensor"
	// DefaultNamespace 是设备默认命名空间。
	DefaultNamespace = "default"
	// DefaultInterval 是默认采集/波动周期。
	DefaultInterval = 2 * time.Second
	// DefaultTargetTemp 是默认目标温度（未下发指令时的收敛目标）。
	DefaultTargetTemp = 28.0

	// 温度合法范围与波动步长上限。
	minTemp = 20.0
	maxTemp = 35.0
	// 湿度合法范围与波动步长上限。
	minHumidity = 40.0
	maxHumidity = 70.0
	// convergeFactor 每周期向目标温度收敛的比例（0~1）。
	convergeFactor = 0.3
	// maxStep 每周期随机扰动上限（温度/湿度的绝对变化量）。
	maxStep = 0.8
)

// Option 是 MockSensor 的可选配置项（函数式选项）。
type Option func(*MockSensor)

// WithNamespace 设置设备命名空间（默认 "default"）。
func WithNamespace(ns string) Option {
	return func(m *MockSensor) { m.namespace = ns }
}

// WithInterval 设置波动周期（默认 2s；测试可调小以加速收敛验证）。
func WithInterval(d time.Duration) Option {
	return func(m *MockSensor) { m.interval = d }
}

// WithSeed 设置随机数种子（默认随机；测试传入固定种子保证确定性）。
func WithSeed(seed int64) Option {
	return func(m *MockSensor) { m.rng = rand.New(rand.NewSource(seed)) }
}

// WithEventBus 设置 MQTT 数据面（WBS 3.6）：传入装配层已连接的 EventBus，
// 传感器进入 MQTT 模式——
//   - 采集循环每次采样后把遥测发布到 devices/<ns>/<deviceName>/telemetry（QoS1）；
//   - Start 时订阅 devices/<ns>/<deviceName>/command（QoS1），收到
//     {"property":"targetTemp","value":25} 格式 JSON 指令后设置目标温度
//     （复用与云边通道 HandleCommand 相同的 targetTemp/reset 逻辑）；
//   - 总线断开/未连接时采集循环照常运行（本地波动），发布自动跳过并告警，不 panic。
//
// 不设置（或传 nil）则保持纯本地模式（默认，向后兼容）：不订阅、不发布，
// 仅由 HandleCommand/Collect 与云边通道交互。
// 注意：EventBus 的 Connect 由装配层负责（MockSensor 不负责建连），
// 使用方须保证 Start 前总线已连接（订阅才能成功）。
func WithEventBus(bus *eventbus.EventBus) Option {
	return func(m *MockSensor) { m.bus = bus }
}

// MockSensor 是模拟温湿度传感器 Mapper。
type MockSensor struct {
	mu sync.RWMutex

	deviceName string
	namespace  string
	interval   time.Duration
	rng        *rand.Rand

	temperature float64 // 当前温度（采集值）
	humidity    float64 // 当前湿度（采集值）
	targetTemp  float64 // 目标温度：采集向该值收敛

	started bool          // Start 是否已调用（幂等标记）
	stopCh  chan struct{} // 停止信号
	doneCh  chan struct{} // 采集循环退出信号

	// MQTT 数据面（WBS 3.6）：nil = 纯本地模式（默认）。
	// 主题在 New 时按 namespace/deviceName 构造（见 initMqttTopics）。
	bus            *eventbus.EventBus
	telemetryTopic string
	commandTopic   string
}

// mqttCommandPayload 是 MQTT 数据面指令负载：字段与云边通道的
// DeviceCommand（property/value）对齐，语义同义复用。
type mqttCommandPayload struct {
	Property string  `json:"property"` // 目标属性名，如 targetTemp / reset
	Value    float64 `json:"value"`    // 属性目标值（reset 可省略）
}

// mqttTelemetryPayload 是 MQTT 数据面遥测负载：温度/湿度 + 毫秒时间戳。
type mqttTelemetryPayload struct {
	Temperature float64 `json:"temperature"`
	Humidity    float64 `json:"humidity"`
	TS          int64   `json:"ts"` // 采集时刻（Unix 毫秒）
}

// New 创建模拟传感器 Mapper。deviceName 是设备名（注册表路由键）。
// 初始温度/湿度在合法范围内随机生成，目标温度取默认值。
//
// 单设备设计（M3A P2-2 结论）：一个 MockSensor 实例只管理一台设备
// （deviceName 在 New 时固定，Name() 返回常量 "mock-sensor"——同一注册表
// 无法注册第二个实例）。多设备场景需注册多个 Mapper 实例（各配不同
// deviceName 与注册名），或扩展 DeviceNames() 支持多设备；当前单设备
// 场景够用，暂不扩展。
func New(deviceName string, opts ...Option) *MockSensor {
	m := &MockSensor{
		deviceName: deviceName,
		namespace:  DefaultNamespace,
		interval:   DefaultInterval,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	for _, o := range opts {
		o(m)
	}
	m.initMqttTopics()
	m.resetLocked()
	return m
}

// initMqttTopics 按命名空间/设备名构造 MQTT 主题；
// 段值非法（含 / + # 等）时记警告并回退为纯本地模式（主题无法安全构造）。
// 与 EventBus 主题约定一致（见 edge/pkg/eventbus.TelemetryTopic）。
func (m *MockSensor) initMqttTopics() {
	if m.bus == nil {
		return
	}
	tel, err := eventbus.TelemetryTopic(m.namespace, m.deviceName)
	if err != nil {
		log.Warnf("MockSensor %s: 构造遥测主题失败（%v），回退为纯本地模式", m.deviceName, err)
		m.bus = nil
		return
	}
	cmd, err := eventbus.CommandTopic(m.namespace, m.deviceName)
	if err != nil {
		log.Warnf("MockSensor %s: 构造指令主题失败（%v），回退为纯本地模式", m.deviceName, err)
		m.bus = nil
		return
	}
	m.telemetryTopic = tel
	m.commandTopic = cmd
}

// MqttEnabled 返回是否处于 MQTT 模式（装配了 EventBus）。
// 供装配层/测试确认数据面配置生效。
func (m *MockSensor) MqttEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bus != nil
}

// Name 返回注册名（注册表唯一键）。
func (m *MockSensor) Name() string { return DefaultName }

// DeviceNames 声明本 Mapper 管理的设备名（注册表据此建立路由索引）。
func (m *MockSensor) DeviceNames() []string {
	return []string{m.deviceName}
}

// DeviceNamespace 返回设备所属命名空间（注册表据此建立 namespace/deviceName
// 路由索引，M3A P2-1；默认 "default"，可通过 WithNamespace 覆盖）。
func (m *MockSensor) DeviceNamespace() string { return m.namespace }

// TargetTemp 返回当前目标温度（供 DeviceTwin / 测试观测指令是否生效）。
func (m *MockSensor) TargetTemp() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.targetTemp
}

// resetLocked 重置设备状态（调用方需持有写锁）：
// 温度/湿度在合法范围内重新随机，目标温度回到默认值。
func (m *MockSensor) resetLocked() {
	m.temperature = m.randRange(minTemp, maxTemp)
	m.humidity = m.randRange(minHumidity, maxHumidity)
	m.targetTemp = DefaultTargetTemp
}

// randRange 返回 [lo, hi] 内的随机浮点数（调用方需持有锁）。
func (m *MockSensor) randRange(lo, hi float64) float64 {
	return lo + m.rng.Float64()*(hi-lo)
}

// randSigned 返回 [-max, max] 内的随机数（调用方需持有锁）。
func (m *MockSensor) randSigned(max float64) float64 {
	return (m.rng.Float64()*2 - 1) * max
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

// Start 启动采集循环（幂等）：按 interval 周期波动属性值——温度向
// targetTemp 收敛并叠加小扰动，湿度随机游走。ctx 取消或 Stop() 均会退出。
func (m *MockSensor) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil // 已启动：幂等
	}
	m.started = true
	m.stopCh = make(chan struct{})
	m.doneCh = make(chan struct{})
	interval := m.interval
	stopCh := m.stopCh
	doneCh := m.doneCh
	m.mu.Unlock()

	log.Infof("MockSensor %s 启动（interval=%s）", m.deviceName, interval)
	// MQTT 模式：先订阅指令主题（QoS1），再启动采集循环。
	// 订阅失败只告警不阻断：采集照常进行（遥测发布依赖总线在线状态）。
	// 总线断线重连后的订阅恢复由 EventBus 自动完成，无需在此处理。
	if m.bus != nil {
		if err := m.bus.Subscribe(m.commandTopic, m.handleCommandMsg); err != nil {
			log.Warnf("MockSensor %s: 订阅指令主题 %s 失败（%v），数据面指令不可达，遥测发布不受影响",
				m.deviceName, m.commandTopic, err)
		} else {
			log.Infof("MockSensor %s: 已订阅数据面指令主题 %s（MQTT 模式）", m.deviceName, m.commandTopic)
		}
	}
	go m.run(ctx, interval, stopCh, doneCh)
	return nil
}

// run 是采集循环：每个周期做一次波动（温度收敛 + 随机扰动）。
func (m *MockSensor) run(ctx context.Context, interval time.Duration, stopCh, doneCh chan struct{}) {
	defer close(doneCh)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.tick()
			m.publishTelemetry() // MQTT 模式：每次采样后发布遥测（纯本地模式为 no-op）
		case <-stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// publishTelemetry 把最新采样发布到遥测主题（QoS1，payload 见 mqttTelemetryPayload）。
// 纯本地模式（bus 为 nil）或总线不在线时直接跳过；发布失败只告警不重试
// （下一采样周期会再发布，业务侧按最新值消费即可）。
// 快照在锁外构建，避免网络阻塞采集循环。
func (m *MockSensor) publishTelemetry() {
	if m.bus == nil {
		return
	}
	m.mu.RLock()
	payload := mqttTelemetryPayload{
		Temperature: m.temperature,
		Humidity:    m.humidity,
		TS:          time.Now().UnixMilli(),
	}
	m.mu.RUnlock()

	// 总线断开（断线窗口/已 Disconnect）：跳过本轮，不 panic；重连后自动恢复
	if !m.bus.IsOnline() {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		log.Warnf("MockSensor %s: 遥测序列化失败: %v", m.deviceName, err)
		return
	}
	if err := m.bus.Publish(m.telemetryTopic, data); err != nil {
		log.Warnf("MockSensor %s: 遥测发布到 %s 失败: %v", m.deviceName, m.telemetryTopic, err)
	}
}

// handleCommandMsg 处理 MQTT 数据面指令（EventBus 消息分发 goroutine 中调用，
// 只做加锁状态更新，不做阻塞操作）：
//   - property=targetTemp：设置目标温度（范围校验与云边通道一致）；
//   - property=reset：恢复出厂状态。
//
// 与云边通道（HandleCommand）构成双通道：二者写同一份目标状态，
// 后到者生效（last-write-wins），语义见 docs/MAPPER-GUIDE.md。
func (m *MockSensor) handleCommandMsg(topic string, payload []byte) {
	var cmd mqttCommandPayload
	if err := json.Unmarshal(payload, &cmd); err != nil {
		log.Warnf("MockSensor %s: 数据面指令 JSON 解析失败（%v）: %s", m.deviceName, err, string(payload))
		return
	}
	switch cmd.Property {
	case "targetTemp":
		m.mu.Lock()
		if cmd.Value < minTemp || cmd.Value > maxTemp {
			m.mu.Unlock()
			log.Warnf("MockSensor %s: 数据面指令 targetTemp=%v 超出合法范围 [%v, %v]，已忽略",
				m.deviceName, cmd.Value, minTemp, maxTemp)
			return
		}
		m.targetTemp = cmd.Value
		m.mu.Unlock()
		log.Infof("MockSensor %s: 数据面指令生效 targetTemp=%v（topic=%s）", m.deviceName, cmd.Value, topic)
	case "reset":
		m.mu.Lock()
		m.resetLocked()
		m.mu.Unlock()
		log.Infof("MockSensor %s: 数据面指令生效 reset（topic=%s）", m.deviceName, topic)
	case "":
		log.Warnf("MockSensor %s: 数据面指令缺少 property 字段: %s", m.deviceName, string(payload))
	default:
		log.Warnf("MockSensor %s: 不支持的指令属性 %q（数据面）", m.deviceName, cmd.Property)
	}
}

// tick 执行一个波动周期：温度向目标值收敛并叠加小随机扰动，
// 湿度随机游走；结果均钳制在合法范围内。
func (m *MockSensor) tick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 温度：按比例向目标收敛 + 小随机扰动，钳制在 [minTemp, maxTemp]
	drift := (m.targetTemp - m.temperature) * convergeFactor
	m.temperature = clamp(m.temperature+drift+m.randSigned(maxStep), minTemp, maxTemp)
	// 湿度：随机游走，钳制在 [minHumidity, maxHumidity]
	m.humidity = clamp(m.humidity+m.randSigned(maxStep), minHumidity, maxHumidity)
}

// Stop 停止采集循环（幂等）：等待循环退出后状态冻结，不再变化。
func (m *MockSensor) Stop() error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil // 未启动：无需停止
	}
	m.started = false
	stopCh := m.stopCh
	doneCh := m.doneCh
	m.mu.Unlock()

	close(stopCh)
	<-doneCh // 等待采集循环退出，保证停止后状态不再变化
	// MQTT 模式：取消指令订阅（总线已断开时失败仅告警——连接断开后订阅
	// 本就不复存在，EventBus 重连恢复只针对存活订阅表）。
	if m.bus != nil {
		if err := m.bus.Unsubscribe(m.commandTopic); err != nil {
			log.Warnf("MockSensor %s: 取消订阅 %s 失败（%v），忽略", m.deviceName, m.commandTopic, err)
		}
	}
	log.Infof("MockSensor %s 已停止", m.deviceName)
	return nil
}

// Collect 采集当前属性快照（温度/湿度，均为合法范围内的值）。
func (m *MockSensor) Collect() (map[string]float64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return map[string]float64{
		"temperature": m.temperature,
		"humidity":    m.humidity,
	}, nil
}

// HandleCommand 处理云端下发的指令：
//   - property=targetTemp：把目标温度设为 value（范围 [minTemp, maxTemp]，
//     越界返回错误；后续采集将向新目标收敛）；
//   - property=reset：恢复出厂状态（温度/湿度重新随机，目标温度回默认值）。
//
// 返回处理后的最新状态快照（DeviceReport）。指令设备名与自身不符、
// property 缺失或未知属性均返回错误。
//
// 空 deviceName 语义（M3A P2-3 结论）：cmd.DeviceName 为空时视为匹配本设备
// ——依赖 Dispatch 按名路由的隐式约定（空名在 Route 阶段即被拒绝，到不了
// HandleCommand），此处宽松校验仅为防御直接调用。收紧时在 Dispatch 入口
// 加显式空名校验即可。
func (m *MockSensor) HandleCommand(cmd mapper.DeviceCommand) (mapper.DeviceReport, error) {
	if cmd.DeviceName != "" && cmd.DeviceName != m.deviceName {
		return mapper.DeviceReport{}, fmt.Errorf("指令设备名 %q 与本 Mapper 设备 %q 不符",
			cmd.DeviceName, m.deviceName)
	}
	m.mu.Lock()
	switch cmd.Property {
	case "targetTemp":
		if cmd.Value < minTemp || cmd.Value > maxTemp {
			m.mu.Unlock()
			return mapper.DeviceReport{}, fmt.Errorf("targetTemp %v 超出合法范围 [%v, %v]",
				cmd.Value, minTemp, maxTemp)
		}
		m.targetTemp = cmd.Value
	case "reset":
		m.resetLocked()
	case "":
		m.mu.Unlock()
		return mapper.DeviceReport{}, errors.New("指令缺少 property 字段")
	default:
		m.mu.Unlock()
		return mapper.DeviceReport{}, fmt.Errorf("不支持的指令属性 %q", cmd.Property)
	}
	m.mu.Unlock()
	return m.snapshot(), nil
}

// snapshot 返回当前状态快照（DeviceReport，reportedAt 取当前毫秒时间戳）。
func (m *MockSensor) snapshot() mapper.DeviceReport {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return mapper.DeviceReport{
		DeviceName: m.deviceName,
		Namespace:  m.namespace,
		Properties: map[string]float64{
			"temperature": m.temperature,
			"humidity":    m.humidity,
		},
		ReportedAt: time.Now().UnixMilli(),
	}
}
