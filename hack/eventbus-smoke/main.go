// eventbus-smoke 是 EventBus 的真实环境冒烟工具（开发用）。
//
// 场景：起临时 mosquitto（端口 18883）→ 发布/订阅收发 → 停 broker →
// 重启 → 验证自动重连与订阅恢复 → 清理。
// 用法：go run ./hack/eventbus-smoke
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"edgeflow/edge/pkg/eventbus"
)

const (
	port = 18883 // 冒烟固定端口（与生产 1883 区分，避免误连）
	addr = "tcp://127.0.0.1:18883"
)

func main() {
	mosq, err := exec.LookPath("mosquitto")
	if err != nil {
		// macOS brew 的 broker 在 sbin，通常不在 PATH
		for _, p := range []string{"/opt/homebrew/sbin/mosquitto", "/usr/local/sbin/mosquitto"} {
			if _, err := os.Stat(p); err == nil {
				mosq = p
				break
			}
		}
	}
	if mosq == "" {
		fmt.Println("FAIL: 找不到 mosquitto（brew install mosquitto）")
		os.Exit(1)
	}

	step := func(n, desc string) { fmt.Printf("\n== %s. %s ==\n", n, desc) }
	ok := func(desc string) { fmt.Println("ok:", desc) }

	// --- 1. 启动临时 broker ---
	step("1", fmt.Sprintf("启动临时 mosquitto（端口 %d）", port))
	broker := startBroker(mosq, port)
	defer stopBroker(broker)
	ok("broker 就绪")

	// --- 2. 连接并收发 ---
	step("2", "连接 EventBus，验证发布/订阅收发（QoS 1）")
	ctx := context.Background()
	sub := newBus("smoke-sub")
	pub := newBus("smoke-pub")
	must(sub.Connect(ctx), "订阅端 Connect")
	must(pub.Connect(ctx), "发布端 Connect")
	defer sub.Disconnect()
	defer pub.Disconnect()

	topic, _ := eventbus.TelemetryTopic("default", "smoke-sensor")
	received := make(chan string, 8)
	must(sub.Subscribe(topic, func(t string, payload []byte) {
		received <- fmt.Sprintf("%s|%s", t, payload)
	}), "Subscribe")

	must(pub.Publish(topic, []byte(`{"temperature":25.5,"seq":1}`)), "Publish #1")
	must(pub.Publish(topic, []byte(`{"temperature":25.8,"seq":2}`)), "Publish #2")
	for i := 1; i <= 2; i++ {
		select {
		case msg := <-received:
			fmt.Println("  收到:", msg)
		case <-time.After(5 * time.Second):
			fail("第 %d 条消息超时未到", i)
		}
	}
	ok("收发正常（QoS 1）")

	// --- 3. 停 broker，验证断线感知 ---
	step("3", "停 broker，等待客户端检测到断线")
	stopBroker(broker)
	waitUntil(func() bool { return !sub.IsOnline() && !pub.IsOnline() }, 10*time.Second)
	ok("两端已感知断线（IsOnline=false，自动重连中）")

	// --- 4. 重启 broker，验证自动重连 + 订阅恢复 ---
	step("4", "重启 broker，验证自动重连与订阅恢复")
	broker = startBroker(mosq, port)
	waitUntil(func() bool { return sub.IsOnline() && pub.IsOnline() }, 15*time.Second)
	ok("两端自动重连成功（IsOnline=true）")

	must(pub.Publish(topic, []byte(`{"temperature":26.1,"seq":3}`)), "重连后 Publish")
	select {
	case msg := <-received:
		fmt.Println("  重连后收到:", msg)
		if msg != topic+"|"+`{"temperature":26.1,"seq":3}` {
			fail("重连后消息内容不符: %q", msg)
		}
	case <-time.After(5 * time.Second):
		fail("重连后消息超时未到（订阅未自动恢复？）")
	}
	ok("重连后订阅自动恢复，收发正常")

	// --- 5. 清理 ---
	step("5", "清理")
	sub.Disconnect()
	pub.Disconnect()
	stopBroker(broker)
	ok("客户端已断开、broker 已停止")
	fmt.Println("\nSMOKE PASS")
}

// --- 工具函数 ---

func startBroker(mosq string, port int) *exec.Cmd {
	cmd := exec.Command(mosq, "-p", fmt.Sprintf("%d", port))
	if err := cmd.Start(); err != nil {
		fail("启动 mosquitto 失败: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return cmd
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	fail("mosquitto 5s 内未就绪")
	return nil
}

func stopBroker(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func newBus(id string) *eventbus.EventBus {
	return eventbus.New(addr,
		eventbus.WithClientID(id),
		eventbus.WithKeepAlive(1*time.Second),
		eventbus.WithConnectTimeout(2*time.Second),
		eventbus.WithMaxReconnectInterval(1*time.Second),
	)
}

func must(err error, desc string) {
	if err != nil {
		fail("%s 失败: %v", desc, err)
	}
	fmt.Println("ok:", desc)
}

func waitUntil(cond func() bool, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func fail(format string, args ...any) {
	fmt.Printf("FAIL: "+format+"\n", args...)
	os.Exit(1)
}
