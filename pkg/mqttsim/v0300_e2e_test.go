package mqttsim

// v0.30.0 MQTT 5.0 阶段一 e2e：v5 全链路（RM 协商 / QoS0-2 / 原因码
// 0x10 容忍 / 下行 v5 形态）、流控违规 0x93 强制断连、鉴权失败码
//（v5 0x86 / 3.1.1 returnCode 4）与向下兼容。

import (
	"net"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
)

// TestV0300EndToEndV5：v5 client ↔ v5 sim 全链路（spec 0003 US-5）。
func TestV0300EndToEndV5(t *testing.T) {
	sim, err := NewBrokerWithConfig(BrokerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()

	received := make(chan mqtt.Publish, 16)
	c, err := mqtt.Dial(sim.Addr(), mqtt.Options{
		ClientID:         "v5e2e",
		ProtocolVersion5: true,
		ReceiveMax:       100,
		EnableQoS2:       true,
	})
	if err != nil {
		t.Fatalf("v5 Dial: %v", err)
	}
	defer c.Close()

	if err := c.Subscribe("demo/#", 1, func(topic string, payload []byte) {
		received <- mqtt.Publish{Topic: topic, Payload: payload}
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// QoS0 / QoS1 / QoS2 上行
	if err := c.Publish("demo/a", 0, []byte("p0")); err != nil {
		t.Fatalf("QoS0: %v", err)
	}
	if err := c.Publish("demo/b", 1, []byte("p1")); err != nil {
		t.Fatalf("QoS1: %v", err)
	}
	if err := c.Publish("demo/c", 2, []byte("p2")); err != nil {
		t.Fatalf("QoS2: %v", err)
	}
	for i, want := range []string{"demo/a", "demo/b", "demo/c"} {
		select {
		case p := <-received:
			if p.Topic != want {
				t.Fatalf("第 %d 条期望 %s，得 %s", i, want, p.Topic)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("第 %d 条（%s）未送达", i, want)
		}
	}

	// QoS1 无匹配订阅者：PUBACK 0x10 警告级——Publish 仍返回 nil
	if err := c.Publish("lonely/topic", 1, []byte("x")); err != nil {
		t.Fatalf("0x10 警告级应容忍: %v", err)
	}

	// 下行（broker 主动 QoS0）：v5 客户端需正确解析 v5 帧形态
	if err := sim.Publish("demo/down", []byte("d1")); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-received:
		if p.Topic != "demo/down" {
			t.Fatalf("下行主题失配: %s", p.Topic)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("下行未送达")
	}
}

// TestV0300FlowControlViolation93：上行 QoS2 暂存深度超 server RM →
// DISCONNECT 0x93 断连（spec 0003 US-4 服务端强制）。
func TestV0300FlowControlViolation93(t *testing.T) {
	sim, err := NewBrokerWithConfig(BrokerConfig{ReceiveMax: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()

	conn, err := net.Dial("tcp", sim.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := mqtt.EncodePacket(conn, &mqtt.Connect{ClientID: "raw", V5: true}); err != nil {
		t.Fatal(err)
	}
	p, err := mqtt.DecodePacketV(conn, true)
	if err != nil {
		t.Fatal(err)
	}
	ca, ok := p.(*mqtt.Connack)
	if !ok || !ca.V5 || ca.ReturnCode != 0 || ca.ReceiveMax != 2 {
		t.Fatalf("CONNACK 应为 v5 + RM=2: %+v", ca)
	}
	// 3 条 QoS2 PUBLISH 不等回执（第三条触发 0x93）。前两条正常回
	// PUBREC，0x93 在其后——清流至 DISCONNECT（协议顺序正确性验证）。
	for pid := uint16(1); pid <= 3; pid++ {
		if err := mqtt.EncodePacket(conn, &mqtt.Publish{Topic: "burst", QoS: 2, PacketID: pid, V5: true}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	var dc *mqtt.Disconnect
	var gotPubrec int
	for dc == nil {
		if time.Now().After(deadline) {
			t.Fatalf("3s 内未收到 DISCONNECT 0x93（已收 PUBREC %d 条）", gotPubrec)
		}
		p, err = mqtt.DecodePacketV(conn, true)
		if err != nil {
			t.Fatalf("期望 DISCONNECT 0x93，得错误: %v（已收 PUBREC %d 条）", err, gotPubrec)
		}
		switch pv := p.(type) {
		case *mqtt.Pubrec:
			gotPubrec++ // 前两条正常接受
		case *mqtt.Disconnect:
			dc = pv
		default:
			t.Fatalf("意外报文 %#v", p)
		}
	}
	if gotPubrec != 2 {
		t.Fatalf("0x93 前应恰有 2 条 PUBREC，得 %d", gotPubrec)
	}
	if !dc.V5 || dc.ReasonCode != mqtt.MQTTV5ReceiveMaxExceeded {
		t.Fatalf("期望 DISCONNECT 0x93，得 %#v", dc)
	}
	// 随后连接关闭
	if _, err := mqtt.DecodePacketV(conn, true); err == nil {
		t.Fatal("0x93 后连接应关闭")
	}
}

// TestV0300AuthAndDowngrade：鉴权失败码（v5 0x86 / 3.1.1 returnCode 4）
// 与 3.1.1 向下兼容（spec 0003 US-3/US-5）。
func TestV0300AuthAndDowngrade(t *testing.T) {
	sim, err := NewBrokerWithConfig(BrokerConfig{Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()

	// v5 错误凭证 → 0x86 语义透出
	_, err = mqtt.Dial(sim.Addr(), mqtt.Options{ClientID: "v5bad", ProtocolVersion5: true, Username: "u", Password: "x"})
	if err == nil || !strings.Contains(err.Error(), "0x86") {
		t.Fatalf("v5 鉴权失败应透出 0x86: %v", err)
	}
	// 3.1.1 错误凭证 → returnCode 4（既有语义）
	_, err = mqtt.Dial(sim.Addr(), mqtt.Options{ClientID: "v3bad", Username: "u", Password: "x"})
	if err == nil || !strings.Contains(err.Error(), "return code 4") {
		t.Fatalf("3.1.1 鉴权失败应透出 return code 4: %v", err)
	}
	// 正确凭证：v5 与 3.1.1（冻结路径）均成功
	c5, err := mqtt.Dial(sim.Addr(), mqtt.Options{ClientID: "v5ok", ProtocolVersion5: true, Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("v5 正确凭证: %v", err)
	}
	_ = c5.Close()
	c3, err := mqtt.Dial(sim.Addr(), mqtt.Options{ClientID: "v3ok", Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("3.1.1 正确凭证（冻结路径）: %v", err)
	}
	_ = c3.Close()
}
