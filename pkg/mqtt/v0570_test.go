// v0570_test.go：QoS2 跨相 drain 单测（v0.57.0，spec 0030，R2 P3-1 处置）。
package mqtt

import (
	"net"
	"sync"
	"testing"
	"time"
)

// drainBroker：PUBREC 首帧延迟回（> 客户端重发间隔 → 触发一次重发），每次
// PUBLISH 各回一帧 PUBREC（制造迟到重复帧）；PUBREL 首帧即回 PUBCOMP。
type drainBroker struct {
	ln      net.Listener
	mu      sync.Mutex
	pubs    int
	pubrels int
}

func startDrainBroker(t *testing.T) *drainBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	b := &drainBroker{ln: ln}
	go b.acceptLoop()
	return b
}

func (b *drainBroker) addr() string { return b.ln.Addr().String() }

func (b *drainBroker) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.serve(conn)
	}
}

func (b *drainBroker) serve(conn net.Conn) {
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
			b.pubs++
			first := b.pubs == 1
			b.mu.Unlock()
			if first {
				time.Sleep(60 * time.Millisecond) // 超过客户端重发间隔 → 触发一次重发
			}
			_ = encodePacket(conn, &Pubrec{PacketID: pv.PacketID})
		case *Pubrel:
			b.mu.Lock()
			b.pubrels++
			b.mu.Unlock()
			_ = encodePacket(conn, &Pubcomp{PacketID: pv.PacketID})
		}
	}
}

func (b *drainBroker) counts() (pubs, pubrels int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pubs, b.pubrels
}

// US-1 跨相 drain：PUBREC 延迟触发一次重发 → 迟到重复 PUBREC 滞留通道，
// phase2 前 drain 清空 → PUBCOMP 正常闭环（无 drain 时 phase2 会误取
// 重复 PUBREC 报 "expected PUBCOMP"）。
func TestV0570QoS2StalePubrecDrain(t *testing.T) {
	b := startDrainBroker(t)
	c, err := Dial(b.addr(), Options{ClientID: "c-v0570", EnableQoS2: true, ResendInterval: 40 * time.Millisecond})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Publish("drain/1", 2, []byte("x")); err != nil {
		t.Fatalf("publish 应在 drain 保护下成功: %v", err)
	}
	pubs, rels := b.counts()
	if pubs != 2 {
		t.Fatalf("PUBLISH 应 2 帧（首发+一次重发）: %d", pubs)
	}
	if rels != 1 {
		t.Fatalf("PUBREL 应 1 帧（phase2 无重发）: %d", rels)
	}
}
