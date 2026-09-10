package mqtt

// v0.30.0 MQTT 5.0 阶段一测试：编解码（属性区/原因码/v5 帧形态）、
// 冻结锚（3.1.1 默认路径逐字节）、Receive Maximum 出站流控。
// 本文件与 v0240–v0270 冻结测试物理隔离（spec 0003 US-1 冻结声明）。

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// TestV0300PropertyLengthVBI：属性长度 VBI 编解码往返与边界。
func TestV0300PropertyLengthVBI(t *testing.T) {
	cases := []struct {
		v    uint32
		wire []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7F}},
		{128, []byte{0x80, 0x01}},
		{16383, []byte{0xFF, 0x7F}},
		{16384, []byte{0x80, 0x80, 0x01}},
		{2097151, []byte{0xFF, 0xFF, 0x7F}},
		{2097152, []byte{0x80, 0x80, 0x80, 0x01}},
	}
	for _, c := range cases {
		var d decoder
		d.b = c.wire
		got, err := d.readVBI()
		if err != nil || got != c.v {
			t.Fatalf("readVBI(% x) = %d, %v；期望 %d", c.wire, got, err, c.v)
		}
	}
	// 5 字节溢出拒绝
	d := decoder{b: []byte{0x80, 0x80, 0x80, 0x80, 0x00}}
	if _, err := d.readVBI(); err == nil {
		t.Fatal("5 字节 VBI 应拒绝")
	}
}

// TestV0300FrozenConnectBytes：3.1.1 默认路径冻结锚——V5=false 的
// CONNECT 编码逐字节等于历史版本（spec 0003 US-1 验收）。
func TestV0300FrozenConnectBytes(t *testing.T) {
	var buf bytes.Buffer
	if err := encodePacket(&buf, &Connect{ClientID: "c", KeepAlive: 30, CleanSession: true}); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x10, 0x0D, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04, 0x02, 0x00, 0x1E, 0x00, 0x01, 'c'}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("冻结锚失配：got % x want % x", buf.Bytes(), want)
	}
}

// TestV0300ConnectV5Codec：v5 CONNECT 编码（级别 5 + RM 属性）与往返。
func TestV0300ConnectV5Codec(t *testing.T) {
	var buf bytes.Buffer
	if err := encodePacket(&buf, &Connect{ClientID: "c", KeepAlive: 30, CleanSession: true, V5: true, ReceiveMax: 10}); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x10, 0x11, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x05, 0x02, 0x00, 0x1E, 0x03, 0x21, 0x00, 0x0A, 0x00, 0x01, 'c'}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("v5 CONNECT 字节失配：got % x want % x", buf.Bytes(), want)
	}
	p, err := decodePacketV(bytes.NewReader(buf.Bytes()), true)
	if err != nil {
		t.Fatal(err)
	}
	con := p.(*Connect)
	if !con.V5 || con.ReceiveMax != 10 {
		t.Fatalf("v5 CONNECT 往返失配: %+v", con)
	}
	// 级别 4 在 v5 提示下仍接受（向下兼容协商），V5=false
	var b2 bytes.Buffer
	if err := encodePacket(&b2, &Connect{ClientID: "c", CleanSession: true}); err != nil {
		t.Fatal(err)
	}
	p2, err := decodePacketV(bytes.NewReader(b2.Bytes()), true)
	if err != nil {
		t.Fatal(err)
	}
	if p2.(*Connect).V5 {
		t.Fatal("级别 4 CONNECT 不应标记 V5")
	}
	// 冻结路径（DecodePacket=decodePacketV(false)）对级别 5 仍拒绝
	if _, err := decodePacket(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("冻结 decodePacket 应拒绝级别 5 CONNECT")
	}
}

// TestV0300ConnackV5Codec：v5 CONNACK（RM 属性 + 失败原因码）。
func TestV0300ConnackV5Codec(t *testing.T) {
	var buf bytes.Buffer
	if err := encodePacket(&buf, &Connack{V5: true, ReturnCode: 0, ReceiveMax: 5}); err != nil {
		t.Fatal(err)
	}
	p, err := decodePacketV(bytes.NewReader(buf.Bytes()), true)
	if err != nil {
		t.Fatal(err)
	}
	ca := p.(*Connack)
	if !ca.V5 || ca.ReceiveMax != 5 || ca.ReturnCode != 0 {
		t.Fatalf("v5 CONNACK 往返失配: %+v", ca)
	}
	// 失败码 0x84 往返（无属性区）
	var b2 bytes.Buffer
	if err := encodePacket(&b2, &Connack{V5: true, ReturnCode: MQTTV5UnsupportedProtocolVer}); err != nil {
		t.Fatal(err)
	}
	p2, err := decodePacketV(bytes.NewReader(b2.Bytes()), true)
	if err != nil {
		t.Fatal(err)
	}
	if got := p2.(*Connack).ReturnCode; got != MQTTV5UnsupportedProtocolVer {
		t.Fatalf("0x84 往返失配: 0x%02X", got)
	}
	if v5ReasonText(MQTTV5UnsupportedProtocolVer) == "" {
		t.Fatal("0x84 应有可读语义")
	}
}

// TestV0300AckReasonCodeCodec：PUBACK/PUBREL/SUBACK/DISCONNECT v5 形态。
func TestV0300AckReasonCodeCodec(t *testing.T) {
	roundtrip := func(p Packet) Packet {
		t.Helper()
		var buf bytes.Buffer
		if err := encodePacket(&buf, p); err != nil {
			t.Fatal(err)
		}
		got, err := decodePacketV(bytes.NewReader(buf.Bytes()), true)
		if err != nil {
			t.Fatalf("%T 往返: %v", p, err)
		}
		return got
	}
	pa := roundtrip(&Puback{PacketID: 7, V5: true, ReasonCode: MQTTV5NoMatchingSubscribers}).(*Puback)
	if pa.PacketID != 7 || pa.ReasonCode != MQTTV5NoMatchingSubscribers {
		t.Fatalf("PUBACK v5 往返失配: %+v", pa)
	}
	// 2B（无 rc）容忍：rc 视为 0x00
	var b2 bytes.Buffer
	b2.Write([]byte{0x40, 0x02, 0x00, 0x07})
	pa2, err := decodePacketV(bytes.NewReader(b2.Bytes()), true)
	if err != nil {
		t.Fatal(err)
	}
	if pa2.(*Puback).ReasonCode != 0 {
		t.Fatalf("2B PUBACK rc 应视为 0: %+v", pa2)
	}
	pr := roundtrip(&Pubrel{PacketID: 9, V5: true, ReasonCode: 0}).(*Pubrel)
	if pr.PacketID != 9 || !pr.V5 {
		t.Fatalf("PUBREL v5 往返失配: %+v", pr)
	}
	sa := roundtrip(&Suback{PacketID: 3, V5: true, Codes: []byte{MQTTV5GrantedQoS1, MQTTV5TopicFilterInvalid}}).(*Suback)
	if len(sa.Codes) != 2 || sa.Codes[1] != MQTTV5TopicFilterInvalid {
		t.Fatalf("SUBACK v5 往返失配: %+v", sa)
	}
	dc := roundtrip(&Disconnect{V5: true, ReasonCode: MQTTV5ReceiveMaxExceeded}).(*Disconnect)
	if dc.ReasonCode != MQTTV5ReceiveMaxExceeded {
		t.Fatalf("DISCONNECT 0x93 往返失配: %+v", dc)
	}
	// v5 PUBLISH：属性长度 0 往返
	pub := roundtrip(&Publish{Topic: "a/b", Payload: []byte("xyz"), V5: true, QoS: 1, PacketID: 11}).(*Publish)
	if pub.Topic != "a/b" || string(pub.Payload) != "xyz" || pub.PacketID != 11 {
		t.Fatalf("v5 PUBLISH 往返失配: %+v", pub)
	}
}

// TestV0300ClientFlowControlThrottle：RM=2 时第三条 QoS1 阻塞，回执后放行
// （spec 0003 US-4 客户端出站节流）。
func TestV0300ClientFlowControlThrottle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type pubMsg struct {
		topic string
		id    uint16
	}
	got := make(chan pubMsg, 8)
	doAck := make(chan uint16, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		p, err := DecodePacketV(conn, true)
		if err != nil {
			return
		}
		con, ok := p.(*Connect)
		if !ok || !con.V5 {
			return
		}
		if err := EncodePacket(conn, &Connack{V5: true, ReturnCode: 0, ReceiveMax: 2}); err != nil {
			return
		}
		writeDone := make(chan struct{})
		go func() {
			defer close(writeDone)
			for {
				select {
				case id := <-doAck:
					if err := EncodePacket(conn, &Puback{PacketID: id, V5: true}); err != nil {
						return
					}
				case <-done:
					return
				}
			}
		}()
		for {
			pk, err := DecodePacketV(conn, true)
			if err != nil {
				return
			}
			switch pv := pk.(type) {
			case *Publish:
				got <- pubMsg{topic: pv.Topic, id: pv.PacketID}
			case *Disconnect:
				return
			}
		}
	}()
	c, err := Dial(ln.Addr().String(), Options{ClientID: "fc", ProtocolVersion5: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = c.Publish(fmt.Sprintf("t/%d", i), 1, []byte("x"))
		}(i)
	}
	// RM=2：恰好两条到达
	m1 := <-got
	m2 := <-got
	select {
	case m := <-got:
		t.Fatalf("第三条在无回执时上线（RM=2 失效）: %+v", m)
	case <-time.After(400 * time.Millisecond):
	}
	// 回执一条 → 第三条放行
	doAck <- m1.id
	m3 := <-got
	if m3.topic == m1.topic || m3.topic == m2.topic {
		// 允许任意顺序，但不能与已收重复（第三条必须是新的）
		t.Fatalf("重复收到已确认报文: %+v", m3)
	}
	// 两条剩余回执，让 goroutine 收尾
	doAck <- m2.id
	doAck <- m3.id
	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()
	select {
	case <-wgDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish goroutine 未收尾")
	}
}
