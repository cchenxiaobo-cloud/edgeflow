// Package mocksensor 提供示例模拟设备 Mapper（WBS 5.5）：一个虚拟的
// 温湿度传感器。它实现 edge/pkg/mapper.DeviceMapper 接口，属性随机波动、
// 支持 targetTemp / reset 指令，用于在真实设备接入（Modbus / MQTT）
// 之前打通"云→边指令、边→云上报"的完整链路。
package mocksensor

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

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
}

// New 创建模拟传感器 Mapper。deviceName 是设备名（注册表路由键）。
// 初始温度/湿度在合法范围内随机生成，目标温度取默认值。
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
	m.resetLocked()
	return m
}

// Name 返回注册名（注册表唯一键）。
func (m *MockSensor) Name() string { return DefaultName }

// DeviceNames 声明本 Mapper 管理的设备名（注册表据此建立路由索引）。
func (m *MockSensor) DeviceNames() []string {
	return []string{m.deviceName}
}

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
		case <-stopCh:
			return
		case <-ctx.Done():
			return
		}
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
