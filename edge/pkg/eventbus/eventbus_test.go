// Package eventbus 的测试。
//
// 集成测试依赖本机 mosquitto broker（brew install mosquitto）：
// 每个用例启动一个临时 mosquitto 实例（随机空闲端口），结束后杀掉进程；
// 若本机找不到 mosquitto 二进制，相关用例自动 t.Skip 并提示安装方法。
package eventbus

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- 测试基础设施 ---

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
				// 兜底：绝对路径直接检查可执行性
				if fi, err := exec.Command(c, "-h").Output(); err == nil && len(fi) > 0 {
					return c
				}
			}
		}
	}
	// 最后尝试 brew --prefix mosquitto
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
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startBroker 在指定端口启动临时 mosquitto，等待就绪后返回进程句柄；
// 测试结束（或 t.Cleanup）时杀掉进程。若端口因上一进程残留未及时释放
// （TIME_WAIT 等），自动重试最多 3 次。
func startBroker(t *testing.T, port int) *exec.Cmd {
	t.Helper()
	mosq := requireMosquitto(t)
	const maxAttempts = 3
	for attempt := 1; ; attempt++ {
		cmd := exec.Command(mosq, "-p", fmt.Sprintf("%d", port))
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动 mosquitto(端口 %d) 失败: %v", port, err)
		}
		// 轮询 TCP 端口直到 broker 可接受连接
		deadline := time.Now().Add(5 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
			if err == nil {
				conn.Close()
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
		// 端口未就绪：杀掉本次进程，稍候重试
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if attempt >= maxAttempts {
			t.Fatalf("mosquitto(端口 %d) %d 次尝试均未就绪", port, maxAttempts)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// stopBroker 立即杀掉 broker（用于重连测试中的"宕机"阶段）。
func stopBroker(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

// newTestBus 创建指向指定端口的测试 EventBus：
// 短 keepalive / 短重连退避 / 唯一客户端 ID（避免与并行用例冲突）。
func newTestBus(t *testing.T, port int, name string) *EventBus {
	t.Helper()
	return New(fmt.Sprintf("tcp://127.0.0.1:%d", port),
		WithClientID(fmt.Sprintf("test-%s-%d", name, time.Now().UnixNano())),
		WithKeepAlive(1*time.Second),
		WithConnectTimeout(2*time.Second),
		WithMaxReconnectInterval(1*time.Second),
	)
}

// waitUntil 轮询等待 cond 成立（默认 10s 超时）。
func waitUntil(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// --- 单元测试（不依赖 broker） ---

// TestTopicBuilders 验证三类主题的构造与非法段校验。
func TestTopicBuilders(t *testing.T) {
	cases := []struct {
		name    string
		build   func() (string, error)
		want    string
		wantErr bool
	}{
		{"telemetry", func() (string, error) { return TelemetryTopic("default", "sensor-01") },
			"devices/default/sensor-01/telemetry", false},
		{"command", func() (string, error) { return CommandTopic("default", "sensor-01") },
			"devices/default/sensor-01/command", false},
		{"event", func() (string, error) { return EventTopic("metamanager", "pod-updated") },
			"edgeflow/metamanager/pod-updated", false},
		{"空 namespace", func() (string, error) { return TelemetryTopic("", "sensor-01") }, "", true},
		{"空 deviceName", func() (string, error) { return CommandTopic("default", "") }, "", true},
		{"deviceName 含斜杠", func() (string, error) { return TelemetryTopic("default", "a/b") }, "", true},
		{"namespace 含通配符", func() (string, error) { return TelemetryTopic("de+#fault", "s") }, "", true},
		{"空 module", func() (string, error) { return EventTopic("", "evt") }, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.build()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望错误，实际得到 topic=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("构造主题失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("topic = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultBrokerAddrFromEnv 验证环境变量覆盖默认地址。
func TestDefaultBrokerAddrFromEnv(t *testing.T) {
	t.Setenv(EnvMQTTAddr, "tcp://192.168.1.10:1883")
	if got := DefaultBrokerAddrFromEnv(); got != "tcp://192.168.1.10:1883" {
		t.Errorf("DefaultBrokerAddrFromEnv = %q, want 环境变量值", got)
	}
	t.Setenv(EnvMQTTAddr, "")
	if got := DefaultBrokerAddrFromEnv(); got != DefaultBrokerAddr {
		t.Errorf("DefaultBrokerAddrFromEnv = %q, want 默认 %q", got, DefaultBrokerAddr)
	}
}

// TestConnectFailsWithoutBroker Connect 在 broker 不可达且 ctx 超时后返回错误。
func TestConnectFailsWithoutBroker(t *testing.T) {
	port := freePort(t) // 该端口无 broker 监听
	b := newTestBus(t, port, "nobroker")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := b.Connect(ctx); err == nil {
		t.Fatal("期望 Connect 在超时后返回错误，实际返回 nil")
	}
}

// --- 集成测试（依赖 mosquitto） ---

// TestPublishSubscribeRoundTrip 两个客户端互发：订阅端收到发布端全部消息，
// Unsubscribe 后不再收到。
func TestPublishSubscribeRoundTrip(t *testing.T) {
	port := freePort(t)
	startBroker(t, port)

	ctx := context.Background()
	subBus := newTestBus(t, port, "sub")
	pubBus := newTestBus(t, port, "pub")
	if err := subBus.Connect(ctx); err != nil {
		t.Fatalf("订阅端 Connect 失败: %v", err)
	}
	defer subBus.Disconnect()
	if err := pubBus.Connect(ctx); err != nil {
		t.Fatalf("发布端 Connect 失败: %v", err)
	}
	defer pubBus.Disconnect()

	topic, err := TelemetryTopic("default", "sensor-01")
	if err != nil {
		t.Fatalf("构造主题失败: %v", err)
	}

	// 订阅端先订阅，再发布（避免丢消息）
	received := make(chan mqttMessage, 16)
	if err := subBus.Subscribe(topic, func(t string, payload []byte) {
		received <- mqttMessage{t, string(payload)}
	}); err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}

	const n = 5
	for i := 0; i < n; i++ {
		payload := fmt.Sprintf("temp=%.1f seq=%d", 20.0+float64(i), i)
		if err := pubBus.Publish(topic, []byte(payload)); err != nil {
			t.Fatalf("Publish #%d 失败: %v", i, err)
		}
	}

	// 逐条校验：topic 完整、payload 与发布一致（QoS 1 至少一次）
	for i := 0; i < n; i++ {
		select {
		case msg := <-received:
			if msg.topic != topic {
				t.Errorf("收到主题 %q, want %q", msg.topic, topic)
			}
			want := fmt.Sprintf("temp=%.1f seq=%d", 20.0+float64(i), i)
			if msg.payload != want {
				t.Errorf("payload = %q, want %q", msg.payload, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("等待第 %d 条消息超时（共收到 %d/%d）", i, i, n)
		}
	}

	// Unsubscribe 后不再收到消息
	if err := subBus.Unsubscribe(topic); err != nil {
		t.Fatalf("Unsubscribe 失败: %v", err)
	}
	if err := pubBus.Publish(topic, []byte("should-not-arrive")); err != nil {
		t.Fatalf("Publish 失败: %v", err)
	}
	select {
	case msg := <-received:
		t.Fatalf("Unsubscribe 后仍收到消息: %q", msg.payload)
	case <-time.After(500 * time.Millisecond):
		// 期望路径：静默
	}
}

// mqttMessage 是测试用消息载体。
type mqttMessage struct {
	topic   string
	payload string
}

// TestQoS1Delivery 验证 QoS 1 消息全部可达（至少一次投递）。
func TestQoS1Delivery(t *testing.T) {
	port := freePort(t)
	startBroker(t, port)

	ctx := context.Background()
	subBus := newTestBus(t, port, "qos1-sub")
	pubBus := newTestBus(t, port, "qos1-pub")
	if err := subBus.Connect(ctx); err != nil {
		t.Fatalf("订阅端 Connect 失败: %v", err)
	}
	defer subBus.Disconnect()
	if err := pubBus.Connect(ctx); err != nil {
		t.Fatalf("发布端 Connect 失败: %v", err)
	}
	defer pubBus.Disconnect()

	topic, _ := CommandTopic("default", "actuator-01")
	received := make(chan string, 64)
	if err := subBus.Subscribe(topic, func(_ string, payload []byte) {
		received <- string(payload)
	}); err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}

	const n = 10
	for i := 0; i < n; i++ {
		if err := pubBus.Publish(topic, []byte(fmt.Sprintf("cmd-%d", i))); err != nil {
			t.Fatalf("Publish #%d 失败: %v", i, err)
		}
	}

	seen := make(map[string]bool)
	for i := 0; i < n; i++ {
		select {
		case p := <-received:
			seen[p] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("QoS 1 消息缺失：已收 %d/%d（%v）", len(seen), n, seen)
		}
	}
	for i := 0; i < n; i++ {
		if !seen[fmt.Sprintf("cmd-%d", i)] {
			t.Errorf("QoS 1 消息 cmd-%d 未到达", i)
		}
	}
}

// TestReconnectAutoRestore 断线重连：停 broker → 客户端自动重连 →
// 重启 broker → 自动恢复订阅与收发（无需手动重新 Subscribe）。
func TestReconnectAutoRestore(t *testing.T) {
	port := freePort(t)
	broker := startBroker(t, port)

	ctx := context.Background()
	subBus := newTestBus(t, port, "rc-sub")
	pubBus := newTestBus(t, port, "rc-pub")
	if err := subBus.Connect(ctx); err != nil {
		t.Fatalf("订阅端 Connect 失败: %v", err)
	}
	defer subBus.Disconnect()
	if err := pubBus.Connect(ctx); err != nil {
		t.Fatalf("发布端 Connect 失败: %v", err)
	}
	defer pubBus.Disconnect()

	topic, _ := TelemetryTopic("default", "sensor-rc")
	received := make(chan string, 64)
	if err := subBus.Subscribe(topic, func(_ string, payload []byte) {
		received <- string(payload)
	}); err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}

	// 阶段一：基线收发正常
	if err := pubBus.Publish(topic, []byte("before-outage")); err != nil {
		t.Fatalf("Publish 失败: %v", err)
	}
	select {
	case p := <-received:
		if p != "before-outage" {
			t.Fatalf("基线消息 payload = %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("基线消息未收到")
	}

	// 阶段二：停 broker，等待两端检测到断线
	stopBroker(t, broker)
	if !waitUntil(t, func() bool {
		return !subBus.IsOnline() && !pubBus.IsOnline()
	}) {
		t.Fatal("停 broker 后客户端未检测到断线（IsOnline 仍为 true）")
	}

	// 阶段三：同端口重启 broker，等待自动重连
	startBroker(t, port)
	if !waitUntil(t, func() bool { return subBus.IsOnline() && pubBus.IsOnline() }) {
		t.Fatal("broker 重启后客户端未自动重连")
	}

	// 阶段四：重连后无需重新订阅即可收发（OnConnect 自动恢复订阅）
	if err := pubBus.Publish(topic, []byte("after-reconnect")); err != nil {
		t.Fatalf("重连后 Publish 失败: %v", err)
	}
	select {
	case p := <-received:
		if p != "after-reconnect" {
			t.Fatalf("重连后消息 payload = %q, want after-reconnect", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("重连后消息未收到（订阅未自动恢复？）")
	}
}

// TestConnectWaitsForBroker Connect 在 broker 尚未启动时阻塞等待，
// broker 起来后自动建连成功（验证 ConnectRetry 语义）。
func TestConnectWaitsForBroker(t *testing.T) {
	port := freePort(t)
	b := newTestBus(t, port, "late-broker")

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		done <- b.Connect(ctx)
	}()

	// 确认 Connect 处于等待中（尚未返回），再启动 broker
	select {
	case err := <-done:
		t.Fatalf("broker 未启动时 Connect 就返回了: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	startBroker(t, port)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("broker 启动后 Connect 仍失败: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("broker 启动后 Connect 未返回")
	}
	b.Disconnect()
}

// TestConcurrentPublishSubscribe 并发发布/订阅/取消订阅不崩溃、不丢数据
// （配合 -race 使用）。
func TestConcurrentPublishSubscribe(t *testing.T) {
	port := freePort(t)
	startBroker(t, port)

	ctx := context.Background()
	subBus := newTestBus(t, port, "cc-sub")
	pubBus := newTestBus(t, port, "cc-pub")
	if err := subBus.Connect(ctx); err != nil {
		t.Fatalf("订阅端 Connect 失败: %v", err)
	}
	defer subBus.Disconnect()
	if err := pubBus.Connect(ctx); err != nil {
		t.Fatalf("发布端 Connect 失败: %v", err)
	}
	defer pubBus.Disconnect()

	topic, _ := EventTopic("edged", "pod-updated")
	var mu sync.Mutex
	count := 0
	if err := subBus.Subscribe(topic, func(_ string, _ []byte) {
		mu.Lock()
		count++
		mu.Unlock()
	}); err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}

	const workers, perWorker = 4, 5
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if err := pubBus.Publish(topic, []byte(fmt.Sprintf("w%d-%d", w, i))); err != nil {
					t.Errorf("并发 Publish 失败: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// 等待全部消息到达（QoS 1 至少一次，只要求数量达标不丢）
	if !waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return count >= workers*perWorker
	}) {
		mu.Lock()
		t.Fatalf("并发消息未收齐：count=%d, want %d", count, workers*perWorker)
		mu.Unlock()
	}
}
