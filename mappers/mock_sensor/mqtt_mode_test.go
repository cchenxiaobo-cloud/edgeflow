// MockSensor MQTT 数据面模式（WBS 3.6 集成）的测试。
//
// 与默认（纯本地）模式的行为差异：
//   - MQTT 模式：采集循环每次采样后向 TelemetryTopic 发布遥测（QoS1）；
//     订阅 CommandTopic，收到 JSON 指令修改目标温度；
//   - 默认模式：不订阅、不发布（其余行为完全一致，见 mock_sensor_test.go）。
//
// 集成用例依赖本机 mosquitto broker（brew install mosquitto）：每个用例
// 启动一个临时 mosquitto 实例（随机空闲端口），结束后杀掉进程；本机找不到
// mosquitto 二进制时相关用例自动 t.Skip（与 edge/pkg/eventbus 的测试约定一致）。
package mocksensor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"edgeflow/edge/pkg/eventbus"
	"edgeflow/edge/pkg/mapper"
)

// --- 测试基础设施（临时 mosquitto broker，与 eventbus 测试同构） ---

// findMosquitto 在 PATH 与常见安装路径中查找 mosquitto 二进制，
// 找不到返回 ""（测试将 Skip）。macOS brew 安装路径 /opt/homebrew/sbin
// 通常不在 shell PATH 中，这里显式兜底。
func findMosquitto() string {
	candidates := []string{
		"mosquitto",
		"/opt/homebrew/sbin/mosquitto",
		"/usr/local/sbin/mosquitto",
		"/usr/local/opt/mosquitto/sbin/mosquitto",
		"/usr/sbin/mosquitto",
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil && p != "" {
			return p
		}
		if p, err := filepath.Abs(c); err == nil {
			if _, err := exec.LookPath(p); err == nil {
				return p
			}
			if _, err := filepath.Glob(p); err == nil {
				if fi, err := exec.Command(c, "-h").Output(); err == nil && len(fi) > 0 {
					return c
				}
			}
		}
	}
	if out, err := exec.Command("brew", "--prefix", "mosquitto").Output(); err == nil {
		p := filepath.Join(string(out), "sbin", "mosquitto")
		if _, err := exec.Command(p, "-h").Output(); err == nil {
			return p
		}
	}
	return ""
}

// requireMosquitto 返回 mosquitto 路径，找不到则跳过测试。
func requireMosquitto(t *testing.T) string {
	t.Helper()
	p := findMosquitto()
	if p == "" {
		t.Skip("未找到 mosquitto 二进制（brew install mosquitto 后重跑；" +
			"macOS 路径通常为 /opt/homebrew/sbin/mosquitto）")
	}
	return p
}

// freePort 申请一个空闲 TCP 端口（监听 :0 后关闭，端口让给后续 broker）。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请空闲端口失败: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// startBroker 在指定端口启动临时 mosquitto，等待就绪后返回进程句柄；
// 测试结束（或 t.Cleanup）时杀掉进程。
func startBroker(t *testing.T, port int) *exec.Cmd {
	t.Helper()
	mosq := requireMosquitto(t)
	const maxAttempts = 3
	for attempt := 1; ; attempt++ {
		cmd := exec.Command(mosq, "-p", fmt.Sprintf("%d", port))
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动 mosquitto(端口 %d) 失败: %v", port, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				ready = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if ready {
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			})
			return cmd
		}
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if attempt >= maxAttempts {
			t.Fatalf("mosquitto(端口 %d) %d 次尝试均未就绪", port, maxAttempts)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// newTestBus 创建指向指定端口的测试 EventBus（唯一客户端 ID，避免并行冲突）。
func newTestBus(t *testing.T, port int, name string) *eventbus.EventBus {
	t.Helper()
	return eventbus.New(fmt.Sprintf("tcp://127.0.0.1:%d", port),
		eventbus.WithClientID(fmt.Sprintf("sensor-test-%s-%d", name, time.Now().UnixNano())),
		eventbus.WithConnectTimeout(2*time.Second),
	)
}

// telemetrySample 是订阅端收到的遥测样本。
type telemetrySample struct {
	topic string
	msg   mqttTelemetryPayload
}

// startTelemetrySubscriber 订阅遥测主题，返回样本通道（缓冲 64，防发布端阻塞）。
func startTelemetrySubscriber(t *testing.T, bus *eventbus.EventBus, topic string) chan telemetrySample {
	t.Helper()
	ch := make(chan telemetrySample, 64)
	if err := bus.Subscribe(topic, func(topic string, payload []byte) {
		var msg mqttTelemetryPayload
		if err := json.Unmarshal(payload, &msg); err != nil {
			t.Errorf("订阅端解析遥测失败: %v（payload=%s）", err, string(payload))
			return
		}
		ch <- telemetrySample{topic: topic, msg: msg}
	}); err != nil {
		t.Fatalf("订阅遥测主题失败: %v", err)
	}
	return ch
}

// newMqttModeSensor 创建 MQTT 模式的测试传感器（固定种子 + 短周期）。
func newMqttModeSensor(t *testing.T, deviceName string, interval time.Duration, bus *eventbus.EventBus) *MockSensor {
	t.Helper()
	return New(deviceName, WithSeed(42), WithInterval(interval), WithEventBus(bus))
}

// assertSampleValid 断言遥测样本合法：主题完整、温度/湿度在合法范围、时间戳非零。
func assertSampleValid(t *testing.T, s telemetrySample) {
	t.Helper()
	if s.topic != "devices/default/sensor-01/telemetry" {
		t.Errorf("遥测主题 = %q，期望 devices/default/sensor-01/telemetry", s.topic)
	}
	assertInRange(t, "temperature", s.msg.Temperature, minTemp, maxTemp)
	assertInRange(t, "humidity", s.msg.Humidity, minHumidity, maxHumidity)
	if s.msg.TS <= 0 {
		t.Errorf("遥测 ts = %d，期望非零毫秒时间戳", s.msg.TS)
	}
}

// TestMqttModePublishesTelemetry MQTT 模式：订阅端能收到传感器发布的
// 遥测数据流（主题完整、值在合法范围、ts 非零）；停止后不再发布。
func TestMqttModePublishesTelemetry(t *testing.T) {
	port := freePort(t)
	startBroker(t, port)

	ctx := context.Background()
	bus := newTestBus(t, port, "pub")
	if err := bus.Connect(ctx); err != nil {
		t.Fatalf("总线 Connect 失败: %v", err)
	}
	defer bus.Disconnect()

	s := newMqttModeSensor(t, "sensor-01", 20*time.Millisecond, bus)
	if !s.MqttEnabled() {
		t.Fatal("WithEventBus 后应处于 MQTT 模式")
	}
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	// 订阅端先订阅再等数据（先于传感器启动订阅，避免丢消息）
	ch := startTelemetrySubscriber(t, bus, "devices/default/sensor-01/telemetry")

	// 收集至少 3 条遥测样本
	got := 0
	deadline := time.Now().Add(5 * time.Second)
	for got < 3 && time.Now().Before(deadline) {
		select {
		case sample := <-ch:
			assertSampleValid(t, sample)
			got++
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("5s 内仅收到 %d 条遥测（期望 ≥3）", got)
		}
	}

	// 停止后不再发布（采样间隔内无新样本）
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	select {
	case sample := <-ch:
		t.Fatalf("Stop 后仍收到遥测: %+v", sample)
	case <-time.After(120 * time.Millisecond):
	}
}

// TestMqttCommandChangesTargetTemp 数据面指令：向 command 主题发布
// {"property":"targetTemp","value":X} → 目标温度改变，且线上遥测向新目标收敛
// （先高温端后低温端，双向验证，与本地模式收敛测试同构）。
func TestMqttCommandChangesTargetTemp(t *testing.T) {
	port := freePort(t)
	startBroker(t, port)

	ctx := context.Background()
	bus := newTestBus(t, port, "cmd")
	if err := bus.Connect(ctx); err != nil {
		t.Fatalf("总线 Connect 失败: %v", err)
	}
	defer bus.Disconnect()

	s := newMqttModeSensor(t, "sensor-01", 20*time.Millisecond, bus)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = s.Stop() }()
	ch := startTelemetrySubscriber(t, bus, "devices/default/sensor-01/telemetry")

	// 阶段一：MQTT 指令 targetTemp=35（高温端）
	if err := bus.Publish("devices/default/sensor-01/command",
		[]byte(`{"property":"targetTemp","value":35}`)); err != nil {
		t.Fatalf("发布数据面指令失败: %v", err)
	}
	if !waitUntil(t, 3*time.Second, func() bool { return s.TargetTemp() == maxTemp }) {
		t.Fatalf("数据面指令后 TargetTemp 未变为 %v（当前 %v）", maxTemp, s.TargetTemp())
	}
	// 线上遥测温度向 35 收敛（>32）
	if !waitUntil(t, 3*time.Second, func() bool {
		select {
		case sample := <-ch:
			assertSampleValid(t, sample)
			return sample.msg.Temperature > 32
		default:
			return false
		}
	}) {
		t.Fatal("设置 targetTemp=35 后线上遥测未收敛到 32 以上")
	}

	// 阶段二：MQTT 指令 targetTemp=20（低温端）
	if err := bus.Publish("devices/default/sensor-01/command",
		[]byte(`{"property":"targetTemp","value":20}`)); err != nil {
		t.Fatalf("发布数据面指令失败: %v", err)
	}
	if !waitUntil(t, 3*time.Second, func() bool { return s.TargetTemp() == minTemp }) {
		t.Fatalf("数据面指令后 TargetTemp 未变为 %v", minTemp)
	}
	if !waitUntil(t, 3*time.Second, func() bool {
		select {
		case sample := <-ch:
			assertSampleValid(t, sample)
			return sample.msg.Temperature < 23
		default:
			return false
		}
	}) {
		t.Fatal("设置 targetTemp=20 后线上遥测未收敛到 23 以下")
	}
}

// TestMqttCommandRejectsInvalid 数据面非法指令被忽略且不 panic：
// 越界值 / 非法 JSON / 未知属性 / 缺少 property 均不改目标温度。
func TestMqttCommandRejectsInvalid(t *testing.T) {
	port := freePort(t)
	startBroker(t, port)

	ctx := context.Background()
	bus := newTestBus(t, port, "bad")
	if err := bus.Connect(ctx); err != nil {
		t.Fatalf("总线 Connect 失败: %v", err)
	}
	defer bus.Disconnect()

	s := newMqttModeSensor(t, "sensor-01", time.Second, bus)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = s.Stop() }()

	commandTopic := "devices/default/sensor-01/command"
	invalid := []string{
		`{"property":"targetTemp","value":99}`, // 越界
		`not-json`,                             // 非法 JSON
		`{"property":"bogus","value":1}`,       // 未知属性
		`{"value":25}`,                         // 缺 property
	}
	for _, payload := range invalid {
		if err := bus.Publish(commandTopic, []byte(payload)); err != nil {
			t.Fatalf("发布非法指令 %s 失败: %v", payload, err)
		}
	}
	// 等待消息送达（异步分发）
	time.Sleep(200 * time.Millisecond)
	if s.TargetTemp() != DefaultTargetTemp {
		t.Errorf("非法指令后 TargetTemp = %v，期望保持默认 %v", s.TargetTemp(), DefaultTargetTemp)
	}

	// reset 数据面指令：目标温度回默认，状态仍合法
	if err := bus.Publish(commandTopic, []byte(`{"property":"reset"}`)); err != nil {
		t.Fatalf("发布 reset 指令失败: %v", err)
	}
	if !waitUntil(t, 3*time.Second, func() bool { return s.TargetTemp() == DefaultTargetTemp }) {
		t.Errorf("reset 后 TargetTemp = %v，期望默认 %v", s.TargetTemp(), DefaultTargetTemp)
	}
	props, err := s.Collect()
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	assertPropsValid(t, props)
}

// TestMqttDualChannelLastWriteWins 双通道语义：MQTT 数据面指令与云边通道
// HandleCommand 写同一份目标温度，后到者生效（last-write-wins）。
func TestMqttDualChannelLastWriteWins(t *testing.T) {
	port := freePort(t)
	startBroker(t, port)

	ctx := context.Background()
	bus := newTestBus(t, port, "dual")
	if err := bus.Connect(ctx); err != nil {
		t.Fatalf("总线 Connect 失败: %v", err)
	}
	defer bus.Disconnect()

	s := newMqttModeSensor(t, "sensor-01", time.Second, bus)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = s.Stop() }()

	// 1) 数据面指令 30 → 生效
	if err := bus.Publish("devices/default/sensor-01/command", []byte(`{"property":"targetTemp","value":30}`)); err != nil {
		t.Fatalf("发布数据面指令失败: %v", err)
	}
	if !waitUntil(t, 3*time.Second, func() bool { return s.TargetTemp() == 30 }) {
		t.Fatalf("数据面指令未生效: TargetTemp=%v", s.TargetTemp())
	}
	// 2) 云边通道（HandleCommand）指令 25 → 覆盖
	if _, err := s.HandleCommand(mapper.DeviceCommand{
		DeviceName: "sensor-01", Property: "targetTemp", Value: 25,
	}); err != nil {
		t.Fatalf("云边通道指令失败: %v", err)
	}
	if s.TargetTemp() != 25 {
		t.Fatalf("云边通道未覆盖: TargetTemp=%v", s.TargetTemp())
	}
	// 3) 数据面指令 32 → 再次覆盖（双向都可写）
	if err := bus.Publish("devices/default/sensor-01/command", []byte(`{"property":"targetTemp","value":32}`)); err != nil {
		t.Fatalf("发布数据面指令失败: %v", err)
	}
	if !waitUntil(t, 3*time.Second, func() bool { return s.TargetTemp() == 32 }) {
		t.Fatalf("数据面指令未再次覆盖: TargetTemp=%v", s.TargetTemp())
	}
}

// TestMqttDisconnectNoPanic 断开 EventBus 不 panic：运行中断线 → 采集照常
// （发布跳过）；停止后总线已断开 → 取消订阅失败仅告警；Collect 仍可用；
// 重新 Start 在未连接总线上 → 订阅失败仅告警，回退为本地波动。
func TestMqttDisconnectNoPanic(t *testing.T) {
	port := freePort(t)
	startBroker(t, port)

	ctx := context.Background()
	bus := newTestBus(t, port, "disc")
	if err := bus.Connect(ctx); err != nil {
		t.Fatalf("总线 Connect 失败: %v", err)
	}

	s := newMqttModeSensor(t, "sensor-01", 20*time.Millisecond, bus)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	// 断线：采集循环继续波动（不 panic），本地状态仍合法
	bus.Disconnect()
	time.Sleep(150 * time.Millisecond)
	props, err := s.Collect()
	if err != nil {
		t.Fatalf("断线后 Collect 失败: %v", err)
	}
	assertPropsValid(t, props)

	// 停止：总线已断开，取消订阅失败 → 仅告警，不 panic
	if err := s.Stop(); err != nil {
		t.Fatalf("断线后 Stop 失败: %v", err)
	}
	// Stop 幂等
	if err := s.Stop(); err != nil {
		t.Errorf("重复 Stop 不应报错: %v", err)
	}

	// 重新 Start（总线仍未连接）：订阅失败仅告警，回退为本地波动，不 panic
	if err := s.Start(ctx); err != nil {
		t.Fatalf("未连接总线上重启 Start 失败: %v", err)
	}
	defer func() { _ = s.Stop() }()
	time.Sleep(60 * time.Millisecond)
	props, err = s.Collect()
	if err != nil {
		t.Fatalf("重启后 Collect 失败: %v", err)
	}
	assertPropsValid(t, props)
}

// TestMqttModeOffByDefault 向后兼容：不传 WithEventBus 时默认纯本地模式
// （不订阅不发布），MqttEnabled 为 false。
func TestMqttModeOffByDefault(t *testing.T) {
	s := New("sensor-01", WithSeed(1), WithInterval(time.Second))
	if s.MqttEnabled() {
		t.Error("未传 WithEventBus 时不应处于 MQTT 模式")
	}
	// 显式传 nil 等价于本地模式
	if s2 := New("sensor-02", WithEventBus(nil)); s2.MqttEnabled() {
		t.Error("WithEventBus(nil) 不应进入 MQTT 模式")
	}
}
