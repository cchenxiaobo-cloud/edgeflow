// Package eventbus 实现边缘内部设备通信总线（WBS 3.6），对标 KubeEdge 的 EventBus。
//
// 定位：EventBus 是"边缘内部设备通信总线"，不是云边通道：
//   - 真实设备（MQTT 传感器 / Modbus 网关等）通过 MQTT 接入边缘，
//     设备数据在"设备 ↔ Mapper"之间走 EventBus；
//   - Mapper 不再直接轮询设备，而是作为 EventBus 客户端订阅设备遥测、
//     向设备下发指令；
//   - EventBus 同时预留"边缘内部模块间事件"通道（edgeflow/<module>/<event>），
//     供 EdgeCore 内部模块解耦通信。
//
// 与云边 WebSocket 通道（edgehub，WBS 3.1）的分工：
//   - WebSocket = 云边管理面：节点注册、心跳、DeviceCommand/DeviceReport 等
//     管理消息（设备契约消息仍走 WebSocket 直达云端）；
//   - MQTT EventBus = 边缘设备数据面：设备接入点 + 模块间事件，不出边缘。
//
// 主题约定（详见 docs/EVENTBUS-GUIDE.md）：
//   - devices/<namespace>/<deviceName>/telemetry   设备 → 边缘（遥测上报）
//   - devices/<namespace>/<deviceName>/command     边缘 → 设备（指令下发）
//   - edgeflow/<module>/<event>                    模块间事件（预留）
//
// 可靠性语义：Publish/Subscribe 默认 QoS 1（至少一次投递，不保证去重，
// 业务侧需幂等）；客户端启用 paho 自动重连（指数退避），断线期间
// 订阅关系由本包在重连成功后自动恢复（不依赖 broker 会话持久化）。
package eventbus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"edgeflow/pkg/log"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// 环境变量与默认值。
const (
	// EnvMQTTAddr 指定边缘 MQTT broker 地址的环境变量
	// （等价于 KubeEdge 的 edgecore.conf MQTT 配置项）。
	EnvMQTTAddr = "EDGEFLOW_EDGECORE_MQTT_ADDR"

	// DefaultBrokerAddr 是默认 broker 地址（本机开发/单机部署）。
	DefaultBrokerAddr = "tcp://127.0.0.1:1883"
	// DefaultClientID 是默认客户端 ID（多实例部署时须用 WithClientID 覆盖）。
	DefaultClientID = "edgeflow-edgecore"

	// DefaultConnectTimeout 是单次建连等待超时。
	DefaultConnectTimeout = 5 * time.Second
	// DefaultKeepAlive 是 MQTT 心跳间隔（决定死连接发现速度）。
	DefaultKeepAlive = 30 * time.Second
	// DefaultMaxReconnectInterval 是重连指数退避的上限。
	DefaultMaxReconnectInterval = 10 * time.Second
	// DefaultConnectRetryInterval 是首次建连失败后的重试间隔
	// （broker 可能晚于 EdgeCore 启动，按 1s 重试可快速恢复）。
	DefaultConnectRetryInterval = 1 * time.Second
	// disconnectQuiesce 是 Disconnect 时等待在途消息冲刷的时间（毫秒）。
	disconnectQuiesce = 250
)

// 主题前缀与层级（写入主题的命名空间/设备名/模块名不得包含
// '/'、'+'、'#'，防止主题注入与跨设备串扰，见 validateSegment）。
const (
	// TopicPrefixDevices 是设备主题前缀：devices/<namespace>/<deviceName>/<kind>。
	TopicPrefixDevices = "devices"
	// TopicSuffixTelemetry 是遥测主题后缀（设备 → 边缘）。
	TopicSuffixTelemetry = "telemetry"
	// TopicSuffixCommand 是指令主题后缀（边缘 → 设备）。
	TopicSuffixCommand = "command"
	// TopicPrefixEvent 是模块间事件主题前缀：edgeflow/<module>/<event>。
	TopicPrefixEvent = "edgeflow"
)

// EventBus 是 MQTT 设备通信总线的封装：内部持有 paho 客户端，
// 对外提供 Connect/Publish/Subscribe/Disconnect 等原语。
// 所有方法线程安全；Publish/Subscribe 需在 Connect 成功后调用。
type EventBus struct {
	mu sync.RWMutex

	addr     string // broker 地址（如 tcp://127.0.0.1:1883）
	clientID string
	opts     *mqtt.ClientOptions

	client    mqtt.Client
	connected bool // Connect 是否已成功（Disconnect 后复位）
	online    bool // 当前是否有"活的"连接（onConnect/onConnectionLost 维护）

	// subs：topic → handler，Connect 成功后按需恢复订阅
	// （重连成功后由 OnConnect 回调重新订阅，不依赖 broker 会话）。
	subs map[string]mqtt.MessageHandler
}

// Option 是 EventBus 的可选配置项（函数式选项）。
type Option func(*EventBus)

// WithClientID 设置 MQTT 客户端 ID（默认 DefaultClientID）。
// 同一 broker 上客户端 ID 必须唯一，多实例部署务必覆盖。
func WithClientID(id string) Option {
	return func(b *EventBus) { b.clientID = id }
}

// WithCredentials 设置 MQTT 用户名/密码（broker 开启认证时使用）。
func WithCredentials(username, password string) Option {
	return func(b *EventBus) {
		b.opts.SetUsername(username)
		b.opts.SetPassword(password)
	}
}

// WithConnectTimeout 设置单次建连等待超时（默认 5s）。
func WithConnectTimeout(d time.Duration) Option {
	return func(b *EventBus) { b.opts.SetConnectTimeout(d) }
}

// WithKeepAlive 设置 MQTT 心跳间隔（默认 30s，测试可调小以加速断线发现）。
func WithKeepAlive(d time.Duration) Option {
	return func(b *EventBus) { b.opts.SetKeepAlive(d) }
}

// WithMaxReconnectInterval 设置重连指数退避上限（默认 10s）。
func WithMaxReconnectInterval(d time.Duration) Option {
	return func(b *EventBus) { b.opts.SetMaxReconnectInterval(d) }
}

// DefaultBrokerAddrFromEnv 解析 broker 地址：环境变量
// EDGEFLOW_EDGECORE_MQTT_ADDR 优先，缺省 tcp://127.0.0.1:1883。
func DefaultBrokerAddrFromEnv() string {
	if v := os.Getenv(EnvMQTTAddr); v != "" {
		return v
	}
	return DefaultBrokerAddr
}

// New 创建 EventBus。brokerAddr 形如 tcp://127.0.0.1:1883
// （传空字符串时使用环境变量/默认地址）。
func New(brokerAddr string, opts ...Option) *EventBus {
	if brokerAddr == "" {
		brokerAddr = DefaultBrokerAddrFromEnv()
	}
	b := &EventBus{
		addr:     brokerAddr,
		clientID: DefaultClientID,
		subs:     make(map[string]mqtt.MessageHandler),
	}
	b.opts = mqtt.NewClientOptions().
		AddBroker(brokerAddr).
		SetClientID(b.clientID).
		// 自动重连：断线后由 paho 后台重试（指数退避），
		// 重连成功后触发 OnConnect 回调恢复订阅。
		SetAutoReconnect(true).
		SetConnectRetry(true). // 首次建连失败也自动重试（配合 Connect(ctx) 等待）
		SetCleanSession(true). // 不依赖 broker 会话持久化，订阅由本包恢复
		SetConnectTimeout(DefaultConnectTimeout).
		SetKeepAlive(DefaultKeepAlive).
		SetMaxReconnectInterval(DefaultMaxReconnectInterval).
		SetConnectRetryInterval(DefaultConnectRetryInterval).
		SetOnConnectHandler(b.onConnect).
		SetConnectionLostHandler(b.onConnectionLost).
		SetReconnectingHandler(b.onReconnecting)
	for _, o := range opts {
		o(b)
	}
	// 选项可能在 With* 里被改写（如客户端 ID），确保最终一致
	b.opts.SetClientID(b.clientID)
	b.client = mqtt.NewClient(b.opts)
	return b
}

// onConnect 是 paho 的（重）连接成功回调：日志 + 恢复全部订阅。
// 注意：该回调运行在 paho 内部 goroutine，且每次重连成功都会触发；
// 恢复订阅失败只记日志（下次重连会再尝试），不阻塞连接。
func (b *EventBus) onConnect(_ mqtt.Client) {
	log.Infof("eventbus: 已连接 broker %s（clientID=%s）", b.addr, b.clientID)
	b.mu.Lock()
	b.online = true
	b.mu.Unlock()
	b.mu.RLock()
	subs := make([]string, 0, len(b.subs))
	for topic := range b.subs {
		subs = append(subs, topic)
	}
	b.mu.RUnlock()
	for _, topic := range subs {
		if err := b.subscribeLocked(topic); err != nil {
			log.Warnf("eventbus: 重连后恢复订阅 %q 失败: %v", topic, err)
		}
	}
}

// onConnectionLost 是断线回调（仅日志；自动重连由 paho 负责）。
func (b *EventBus) onConnectionLost(_ mqtt.Client, err error) {
	log.Warnf("eventbus: 与 broker %s 连接断开: %v（自动重连中）", b.addr, err)
	b.mu.Lock()
	b.online = false
	b.mu.Unlock()
}

// onReconnecting 是 paho 开始重连尝试的回调（仅日志）。
func (b *EventBus) onReconnecting(_ mqtt.Client, _ *mqtt.ClientOptions) {
	log.Warnf("eventbus: 正在重连 broker %s ...", b.addr)
}

// BrokerAddr 返回 broker 地址。
func (b *EventBus) BrokerAddr() string { return b.addr }

// IsConnected 返回是否"可用"：当前已连接，或虽断线但自动重连
// 已在进行中（paho AutoReconnect 语义：连接终将恢复）。
// 业务侧判断"当前这一刻是否在线"请用 IsOnline。
func (b *EventBus) IsConnected() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.connected && b.client.IsConnected()
}

// IsOnline 返回当前是否处于"活的"连接状态（与 broker 的 TCP 连接存在、
// 尚未触发断线回调）。断线瞬间为 false，重连成功后恢复 true。
func (b *EventBus) IsOnline() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.connected && b.online
}

// Connect 建立与 broker 的连接，成功或 ctx 取消时返回。
// 已启用 ConnectRetry：broker 暂不可用时会在后台持续重试，
// 本方法阻塞直到连接成功（ctx 取消则返回错误）。
// 幂等：重复调用返回 nil。
func (b *EventBus) Connect(ctx context.Context) error {
	b.mu.Lock()
	if b.connected {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	token := b.client.Connect()
	for {
		if token.WaitTimeout(200 * time.Millisecond) {
			if err := token.Error(); err != nil {
				return fmt.Errorf("eventbus: 连接 broker %s 失败: %w", b.addr, err)
			}
			b.mu.Lock()
			b.connected = true
			b.mu.Unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("eventbus: 等待连接 broker %s 被取消: %w", b.addr, ctx.Err())
		default:
		}
	}
}

// Disconnect 断开与 broker 的连接（等待在途消息冲刷后关闭）。
// 幂等；断开后可再次 Connect。
func (b *EventBus) Disconnect() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.connected {
		return
	}
	b.client.Disconnect(disconnectQuiesce)
	b.connected = false
	b.online = false
	log.Infof("eventbus: 已断开 broker %s", b.addr)
}

// Publish 向 topic 发布一条 QoS 1 消息（至少一次投递，调用方需幂等消费）。
// 未连接时返回错误。
func (b *EventBus) Publish(topic string, payload []byte) error {
	b.mu.RLock()
	connected := b.connected && b.online
	b.mu.RUnlock()
	if !connected {
		return fmt.Errorf("eventbus: 未连接，无法发布到 %q", topic)
	}
	token := b.client.Publish(topic, 1, false, payload)
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("eventbus: 发布到 %q 失败: %w", topic, err)
	}
	return nil
}

// Subscribe 订阅 topic（QoS 1），收到消息时以
// handler(topic string, payload []byte) 回调（paho 消息分发 goroutine 中调用，
// 回调内勿做长时间阻塞；如需串行处理请自行加锁/队列）。
// 同一 topic 重复订阅会替换旧 handler。未连接时返回错误。
func (b *EventBus) Subscribe(topic string, handler func(topic string, payload []byte)) error {
	if handler == nil {
		return errors.New("eventbus: handler 不能为 nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.connected {
		return fmt.Errorf("eventbus: 未连接，无法订阅 %q", topic)
	}
	b.subs[topic] = wrapHandler(handler)
	return b.subscribeLocked(topic)
}

// Unsubscribe 取消订阅 topic。未连接时返回错误。
func (b *EventBus) Unsubscribe(topic string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.connected {
		return fmt.Errorf("eventbus: 未连接，无法取消订阅 %q", topic)
	}
	if _, ok := b.subs[topic]; !ok {
		return fmt.Errorf("eventbus: 未订阅 %q", topic)
	}
	token := b.client.Unsubscribe(topic)
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("eventbus: 取消订阅 %q 失败: %w", topic, err)
	}
	delete(b.subs, topic)
	return nil
}

// subscribeLocked 执行真正的 MQTT SUBSCRIBE（调用方须持有 b.mu）。
func (b *EventBus) subscribeLocked(topic string) error {
	handler, ok := b.subs[topic]
	if !ok {
		return fmt.Errorf("eventbus: 订阅表缺少 %q", topic)
	}
	token := b.client.Subscribe(topic, 1, handler)
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("eventbus: 订阅 %q 失败: %w", topic, err)
	}
	log.Infof("eventbus: 已订阅 %s (QoS 1)", topic)
	return nil
}

// wrapHandler 把业务 handler 适配为 paho MessageHandler，
// 并补全 topic 字段（订阅通配符时 paho 的 msg.Topic() 才是实际主题）。
func wrapHandler(fn func(topic string, payload []byte)) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		fn(msg.Topic(), msg.Payload())
	}
}

// --- 主题构建与校验 ---

// validateSegment 校验主题中的命名空间/设备名/模块名等单段值：
// 非空、不含 '/'（层级分隔）、'+'（单层通配）、'#'（多层通配）。
func validateSegment(kind, v string) error {
	if v == "" {
		return fmt.Errorf("eventbus: %s 不能为空", kind)
	}
	if strings.ContainsAny(v, "/+#") {
		return fmt.Errorf("eventbus: %s %q 包含非法字符（不允许 / + #）", kind, v)
	}
	return nil
}

// TelemetryTopic 构造遥测主题：devices/<namespace>/<deviceName>/telemetry
// （设备 → 边缘：设备遥测数据上报）。
func TelemetryTopic(namespace, deviceName string) (string, error) {
	if err := validateSegment("namespace", namespace); err != nil {
		return "", err
	}
	if err := validateSegment("deviceName", deviceName); err != nil {
		return "", err
	}
	return TopicPrefixDevices + "/" + namespace + "/" + deviceName + "/" + TopicSuffixTelemetry, nil
}

// CommandTopic 构造指令主题：devices/<namespace>/<deviceName>/command
// （边缘 → 设备：边缘向设备下发指令）。
func CommandTopic(namespace, deviceName string) (string, error) {
	if err := validateSegment("namespace", namespace); err != nil {
		return "", err
	}
	if err := validateSegment("deviceName", deviceName); err != nil {
		return "", err
	}
	return TopicPrefixDevices + "/" + namespace + "/" + deviceName + "/" + TopicSuffixCommand, nil
}

// EventTopic 构造模块间事件主题：edgeflow/<module>/<event>（预留通道）。
func EventTopic(module, event string) (string, error) {
	if err := validateSegment("module", module); err != nil {
		return "", err
	}
	if err := validateSegment("event", event); err != nil {
		return "", err
	}
	return TopicPrefixEvent + "/" + module + "/" + event, nil
}
