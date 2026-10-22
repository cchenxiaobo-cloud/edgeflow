// v0550_test.go：QoS>0 出站重发单测（v0.55.0，spec 0028 B1/B2）。
package mqtt

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptBroker 脚本化 broker：按钩子决定确认时机，锁内记录帧序。
type scriptBroker struct {
	ln net.Listener

	mu        sync.Mutex
	pubs      []*Publish
	dups      []bool // 与 pubs 对齐的 DUP 位记录
	pubrelCnt int

	// onPublish 返回 true = 回 PUBACK；onPubrel 返回 true = 回 PUBCOMP。
	onPublish func(n int) bool
	onPubrel  func(n int) bool
}

func startScriptBroker(t *testing.T, onPublish func(int) bool, onPubrel func(int) bool) *scriptBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	b := &scriptBroker{ln: ln, onPublish: onPublish, onPubrel: onPubrel}
	go b.acceptLoop()
	return b
}

func (b *scriptBroker) addr() string { return b.ln.Addr().String() }

// snapshot 返回 (PUBLISH 数, DUP 位序列, PUBREL 数)（锁内拷贝，-race 安全）。
func (b *scriptBroker) snapshot() (int, []bool, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	dups := append([]bool(nil), b.dups...)
	return len(b.pubs), dups, b.pubrelCnt
}

func (b *scriptBroker) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.serve(conn)
	}
}

func (b *scriptBroker) serve(conn net.Conn) {
	defer conn.Close()
	for {
		p, err := decodePacket(conn)
		if err != nil {
			return
		}
		switch pv := p.(type) {
		case *Connect:
			_ = encodePacket(conn, &Connack{ReturnCode: 0})
		case *Publish:
			b.mu.Lock()
			b.pubs = append(b.pubs, pv)
			b.dups = append(b.dups, pv.Dup != 0)
			n := len(b.pubs)
			b.mu.Unlock()
			switch pv.QoS {
			case 1:
				if b.onPublish != nil && b.onPublish(n) {
					_ = encodePacket(conn, &Puback{PacketID: pv.PacketID})
				}
			case 2:
				_ = encodePacket(conn, &Pubrec{PacketID: pv.PacketID})
			}
		case *Pubrel:
			b.mu.Lock()
			b.pubrelCnt++
			n := b.pubrelCnt
			b.mu.Unlock()
			if b.onPubrel != nil && b.onPubrel(n) {
				_ = encodePacket(conn, &Pubcomp{PacketID: pv.PacketID})
			}
		}
	}
}

// US-1 QoS1 重发至确认：broker 第 2 次才 PUBACK；首发 DUP=0、重发 DUP=1。
func TestV0550QoS1ResendUntilAck(t *testing.T) {
	b := startScriptBroker(t, func(n int) bool { return n >= 2 }, nil)
	c, err := Dial(b.addr(), Options{ClientID: "c-v0550", ResendInterval: 50 * time.Millisecond, MaxResends: 3})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Publish("resend/1", 1, []byte("x")); err != nil {
		t.Fatalf("publish 应在第 2 次投递后成功: %v", err)
	}
	count, dups, _ := b.snapshot()
	if count != 2 || len(dups) != 2 || dups[0] || !dups[1] {
		t.Fatalf("帧序不符（首发 DUP=0、重发 DUP=1）: count=%d dups=%v", count, dups)
	}
}

// US-2 额度耗尽：broker 永不确认 → 3 帧（1+2 重发）后返回 ack timeout。
func TestV0550QoS1ResendExhausted(t *testing.T) {
	b := startScriptBroker(t, func(n int) bool { return false }, nil)
	c, err := Dial(b.addr(), Options{ClientID: "c-v0550", ResendInterval: 30 * time.Millisecond, MaxResends: 2})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	pubErr := c.Publish("resend/2", 1, []byte("x"))
	if pubErr == nil || !strings.Contains(pubErr.Error(), "ack timeout") {
		t.Fatalf("额度耗尽应返回 ack timeout: %v", pubErr)
	}
	count, dups, _ := b.snapshot()
	if count != 3 || len(dups) != 3 || dups[0] || !dups[1] || !dups[2] {
		t.Fatalf("帧数/DUP 序列不符: count=%d dups=%v", count, dups)
	}
}

// US-3 默认禁用：逐字节既有行为——单轮 ackTimeout，仅首发 1 帧。
func TestV0550ResendDisabledSingleAttempt(t *testing.T) {
	b := startScriptBroker(t, func(n int) bool { return false }, nil)
	old := ackTimeout
	ackTimeout = 100 * time.Millisecond
	defer func() { ackTimeout = old }()
	c, err := Dial(b.addr(), Options{ClientID: "c-v0550"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	pubErr := c.Publish("resend/3", 1, []byte("x"))
	if pubErr == nil || !strings.Contains(pubErr.Error(), "ack timeout") {
		t.Fatalf("禁用重发应单轮超时: %v", pubErr)
	}
	if count, _, _ := b.snapshot(); count != 1 {
		t.Fatalf("禁用重发应仅首发 1 帧: %d", count)
	}
}

// US-4 QoS2 phase2 重发：broker 恒回 PUBREC，但第 2 次 PUBREL 才回 PUBCOMP；
// 客户端重发 PUBREL 后闭环（R2 P3-3 处置：补 PUBLISH 帧数断言——phase1 无超时
// 即无重发，PUBLISH 应恰 1 帧）。
func TestV0550QoS2PubrelResend(t *testing.T) {
	b := startScriptBroker(t, nil, func(n int) bool { return n >= 2 })
	c, err := Dial(b.addr(), Options{ClientID: "c-v0550", EnableQoS2: true, ResendInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Publish("resend/4", 2, []byte("x")); err != nil {
		t.Fatalf("publish 应在 PUBREL 重发后成功: %v", err)
	}
	if count, _, rels := b.snapshot(); count != 1 || rels != 2 {
		t.Fatalf("帧数不符（PUBLISH=1 无 phase1 重发；PUBREL=2 首发+重发）: pubs=%d rels=%d", count, rels)
	}
}
