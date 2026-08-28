// CHN-13（v0.23.0）：Publish/Subscribe/Unsubscribe 确认等待有界化测试。
//
// 用原生 TCP mock broker 模拟「半死 broker」：TCP 三次握手成功、CONNACK
// 正常应答（建连可成功），但对 PUBLISH/SUBSCRIBE/UNSUBSCRIBE 永不回
// PUBACK/SUBACK/UNSUBACK —— 复现 QoS1 半死场景，验证调用方在
// waitTimeout 内拿到超时错误而非无界阻塞。
// 另附 mosquitto 快乐路径护栏（确认等待有界化不影响正常确认路径）。
package eventbus

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// silentBroker 是「应答建连、沉默业务报文」的最小 MQTT broker mock：
//   - CONNECT → CONNACK（会话建立成功，b.connected/online 置位）
//   - PINGREQ → PINGRESP（维持 keepalive，连接不被 paho 判死）
//   - 其余报文（PUBLISH/SUBSCRIBE/UNSUBSCRIBE...）→ 沉默不回，保持连接
type silentBroker struct {
	ln net.Listener

	mu      sync.Mutex
	closing bool
}

func startSilentBroker(t *testing.T) *silentBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mock broker 监听失败: %v", err)
	}
	b := &silentBroker{ln: ln}
	go b.acceptLoop()
	t.Cleanup(b.stop)
	return b
}

func (b *silentBroker) addr() string { return "tcp://" + b.ln.Addr().String() }

func (b *silentBroker) stop() {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	b.closing = true
	b.mu.Unlock()
	_ = b.ln.Close()
}

func (b *silentBroker) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.handle(conn)
	}
}

func (b *silentBroker) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		// MQTT 固定头：1 字节类型标志 + 1~4 字节剩余长度 varint。
		hb, err := r.ReadByte()
		if err != nil {
			return // 连接关闭/错误 → 结束本连接
		}
		remaining := 0
		multiplier := 1
		for {
			rb, err := r.ReadByte()
			if err != nil {
				return
			}
			remaining += int(rb&0x7f) * multiplier
			if rb&0x80 == 0 {
				break
			}
			multiplier *= 128
			if multiplier > 128*128*128 {
				return // 协议畸形，放弃
			}
		}
		if _, err := io.CopyN(io.Discard, r, int64(remaining)); err != nil {
			return
		}
		switch hb >> 4 {
		case 1: // CONNECT → CONNACK（0x20 0x02 0x00 0x00）
			if _, err := conn.Write([]byte{0x20, 0x02, 0x00, 0x00}); err != nil {
				return
			}
		case 14: // PINGREQ → PINGRESP（0xD0 0x00）
			if _, err := conn.Write([]byte{0xd0, 0x00}); err != nil {
				return
			}
		default:
			// 业务报文沉默：模拟半死 broker 的核心行为。
		}
	}
}

// withShortWait 注入短确认超时（生产路径恒为 DefaultWaitTimeout，仅测试用）。
func withShortWait(d time.Duration) func() {
	old := waitTimeout
	waitTimeout = d
	return func() { waitTimeout = old }
}

// TestQoS1AckTimeoutOnHalfDeadBroker：半死 broker 下三个原语均应在
// 注入的短超时内返回「确认超时」错误，而非无界阻塞（CHN-13 验收）。
func TestQoS1AckTimeoutOnHalfDeadBroker(t *testing.T) {
	restore := withShortWait(300 * time.Millisecond)
	defer restore()

	b := startSilentBroker(t)
	bus := newTestBus(t, portOf(b.addr()), "halfdead")
	if err := bus.Connect(t.Context()); err != nil {
		t.Fatalf("Connect 应成功（mock 会应答 CONNACK）: %v", err)
	}
	t.Cleanup(func() { bus.Disconnect() })

	// Publish：300ms 超时报错，总耗时有界（<3s，远小于历史无界 10~20s）。
	start := time.Now()
	err := bus.Publish(telTopic(t, "ns1", "dev1"), []byte(`{"t":1}`))
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "确认超时") {
		t.Fatalf("Publish 应返回确认超时错误，got: %v（耗时 %v）", err, elapsed)
	}
	if elapsed < 300*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("Publish 超时耗时应落在 [300ms, 3s]，实际 %v", elapsed)
	}

	// Subscribe：同口径超时（SUBACK 沉默）。
	start = time.Now()
	err = bus.Subscribe(cmdTopic(t, "ns1", "dev1"), func(string, []byte) {})
	elapsed = time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "确认超时") {
		t.Fatalf("Subscribe 应返回确认超时错误，got: %v（耗时 %v）", err, elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Subscribe 超时耗时应 <3s，实际 %v", elapsed)
	}

	// Unsubscribe：Subscribe 超时前订阅表已登记（先登记后确认的既有顺序），
	// 可通过「未订阅」检查走到 UNSUBSCRIBE，同样超时报错。
	start = time.Now()
	err = bus.Unsubscribe(cmdTopic(t, "ns1", "dev1"))
	elapsed = time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "确认超时") {
		t.Fatalf("Unsubscribe 应返回确认超时错误，got: %v（耗时 %v）", err, elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Unsubscribe 超时耗时应 <3s，实际 %v", elapsed)
	}
}

// TestQoS1AckHappyPathWithBroker：mosquitto 快乐路径护栏——确认等待有界化
// 不改变正常确认行为（真实 PUBACK/SUBACK 毫秒级到达，远快于超时值）。
func TestQoS1AckHappyPathWithBroker(t *testing.T) {
	mosq := requireMosquitto(t)
	_ = mosq
	restore := withShortWait(2 * time.Second)
	defer restore()

	port := freePort(t)
	cmd := startBroker(t, port)
	_ = cmd
	bus := newTestBus(t, port, "qoshappy")
	if err := bus.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { bus.Disconnect() })

	got := make(chan [2]string, 4)
	if err := bus.Subscribe(cmdTopic(t, "ns2", "dev2"), func(topic string, payload []byte) {
		got <- [2]string{topic, string(payload)}
	}); err != nil {
		t.Fatalf("Subscribe 应在正常 broker 下成功: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // 订阅生效窗口
	if err := bus.Publish(cmdTopic(t, "ns2", "dev2"), []byte(`{"on":true}`)); err != nil {
		t.Fatalf("Publish 应成功（PUBACK 正常到达）: %v", err)
	}
	select {
	case m := <-got:
		if m[1] != `{"on":true}` {
			t.Fatalf("收到意外负载: %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到订阅消息")
	}
	if err := bus.Unsubscribe(cmdTopic(t, "ns2", "dev2")); err != nil {
		t.Fatalf("Unsubscribe 应成功: %v", err)
	}
}

// telTopic/cmdTopic 包装主题构造：非法段直接 Fatal。
func telTopic(t *testing.T, ns, dev string) string {
	t.Helper()
	topic, err := TelemetryTopic(ns, dev)
	if err != nil {
		t.Fatalf("构造遥测主题失败: %v", err)
	}
	return topic
}

func cmdTopic(t *testing.T, ns, dev string) string {
	t.Helper()
	topic, err := CommandTopic(ns, dev)
	if err != nil {
		t.Fatalf("构造指令主题失败: %v", err)
	}
	return topic
}

// portOf 从 tcp://host:port 地址解析端口。
func portOf(addr string) int {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(addr, "tcp://"))
	n := 0
	for _, c := range port {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
