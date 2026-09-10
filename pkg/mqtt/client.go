package mqtt

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClientClosed is returned by operations attempted after the client has
// been closed, or after the underlying connection has dropped.
var ErrClientClosed = errors.New("mqtt: client closed or disconnected")

// ackTimeout bounds how long Publish (QoS1/QoS2) and Subscribe wait for the
// matching ack from the broker. It is a var so tests can shorten it; the
// production default stays 10s.
var ackTimeout = ackTimeoutDefault

const (
	ackTimeoutDefault = 10 * time.Second
)

// Options configures Dial.
type Options struct {
	ClientID       string
	KeepAlive      time.Duration
	CleanSession   bool // reserved: v0.24.0 always requests a clean session
	Username       string
	Password       string
	ConnectTimeout time.Duration

	// TLSConfig enables TLS when non-nil (nil = plaintext TCP, the
	// v0.24.0 behavior). When ServerName is empty, Dial fills it from
	// the host part of the dial address.
	TLSConfig *tls.Config

	// EnableQoS2 opts in to MQTT QoS 2 (EXACTLY ONCE) support (v0.26.0).
	// When false (the default), Publish rejects QoS 2 before the wire —
	// byte-for-byte the v0.24.0/v0.25.0 behavior.
	EnableQoS2 bool

	// PersistenceDir enables QoS2 in-flight persistence (v0.27.0, opt-in).
	// Empty (the default) keeps the v0.26.0 in-memory behavior byte-for-
	// byte. When set, unfinished QoS2 exchanges (upstream awaiting
	// PUBREC/PUBCOMP, downstream parked awaiting PUBREL) are recorded as
	// JSON files under this directory; ResumePending replays them after a
	// reconnect. Records are removed as soon as the exchange completes
	// (PUBCOMP received / PUBREL delivered).
	PersistenceDir string

	// ProtocolVersion5 opts in to MQTT 5.0 (v0.30.0, 阶段一)。默认 false
	// = 逐字节 3.1.1 行为（冻结）。true 时 CONNECT 以级别 0x05 编码，
	// CONNACK/确认报文按 v5 形态解析（原因码/属性区），并启用 Receive
	// Maximum 流控（见 ReceiveMax）。
	ProtocolVersion5 bool

	// ReceiveMax 是客户端在 CONNECT 中向服务器宣告的自身接收上限
	//（v5 专用；0 = 不携带属性，语义等同 65535 无限制）。出站方向
	// 以服务器 CONNACK 下发的 Receive Maximum 为节流窗口。
	ReceiveMax uint16

	// PersistentSession 开启持久会话（v0.32.0 阶段二，opt-in；默认
	// false = 现状 CleanSession 恒 true 的逐字节行为）：v5 连接发
	// CleanStart=0 + Session Expiry 属性；3.1.1 连接发 CleanSession=0。
	// 会话由服务端按 ClientID 保留（订阅表 + 离线 QoS1 下行，视服务端
	// 能力），重连（同 ClientID 且 PersistentSession）时 CONNACK 的
	// Session Present=1，经 SessionPresent() 读取。
	PersistentSession bool

	// SessionExpiryMs 是 v5 会话过期间隔（毫秒，换算为秒向上取整）：
	// 仅 ProtocolVersion5 且 PersistentSession 时编码进 CONNECT 属性区
	//（0 = 服务端默认策略；断连即毁的纯订阅保留也属合法值）。3.1.1
	// 持久会话无过期概念（服务端保留至重连）。
	SessionExpiryMs uint64
}

// Handler is invoked for every inbound PUBLISH whose topic matches one of the
// filters registered via Subscribe. Handlers run sequentially on the read
// pump goroutine; long-running work should be handed off to another
// goroutine by the handler itself.
type Handler func(topic string, payload []byte)

// Client is a minimal MQTT 3.1.1 client over a single TCP connection.
//
// It does NOT auto-reconnect: when the connection drops (or Close is called)
// the client becomes unusable and further operations return
// ErrClientClosed. Reconnection and session re-establishment are the
// responsibility of the upper layer (the EdgeFlow Mapper).
type Client struct {
	conn    net.Conn
	writeMu sync.Mutex // serializes all packet writes on conn

	packetID uint32 // atomic counter feeding 16-bit packet identifiers

	handlersMu sync.RWMutex
	handlers   map[string][]Handler // filter string -> handlers via Subscribe

	pendingMu   sync.Mutex
	pendingAcks map[uint16]chan Packet // packet id -> PUBACK/SUBACK waiter

	pendingDownQoS2 map[uint16]*Publish // inbound QoS2 parked awaiting PUBREL (v0.26.0)

	persistDir string // QoS2 record directory ("" = disabled, v0.26.0 behavior)

	enableQoS2 bool // gate for the QoS2 code paths (Options.EnableQoS2)

	// ---- MQTT 5.0（v0.30.0，阶段一）----
	v5        bool          // 协商结果：CONNECT 以 v5 发出
	flowSlots chan struct{} // 出站 QoS1/2 在途窗口（容量=server RM）；nil=无限制

	sessionPresent bool // CONNACK Session Present（v0.32.0 阶段二：服务端恢复了持久会话）

	// pendingRecovered 缓冲恢复会话场景下"先于 handler 注册到达"的下行
	// QoS1/0 PUBLISH（SessionPresent 时启用；容量 32，超限丢最旧）。
	// Subscribe 注册 handler 时按 filter 补投一次（QoS1 已向 broker 确认，
	// 缓冲仅为 handler 补投，语义 = 至多一次）。
	pendMu           sync.Mutex
	pendingRecovered []*Publish

	closeOnce sync.Once
	done      chan struct{} // closed by the read pump on exit (disconnected)
}

// Dial connects to addr, performs the CONNECT/CONNACK handshake and starts
// the client's read pump (and keep-alive pinger when KeepAlive > 0).
func Dial(addr string, opts Options) (*Client, error) {
	if opts.ClientID == "" {
		return nil, errors.New("mqtt: ClientID is required")
	}
	timeout := opts.ConnectTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	var (
		conn net.Conn
		err  error
	)
	if opts.TLSConfig != nil {
		// TLS path (v0.25.0): clone so the caller's config is never
		// mutated, and fill ServerName from the dial address host when
		// unset (crypto/tls would do the same, but be explicit).
		cfg := opts.TLSConfig.Clone()
		if cfg.ServerName == "" {
			if host, _, splitErr := net.SplitHostPort(addr); splitErr == nil && host != "" {
				cfg.ServerName = host
			}
		}
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", addr, cfg)
	} else {
		conn, err = net.DialTimeout("tcp", addr, timeout)
	}
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:            conn,
		handlers:        make(map[string][]Handler),
		pendingAcks:     make(map[uint16]chan Packet),
		pendingDownQoS2: make(map[uint16]*Publish),
		persistDir:      opts.PersistenceDir,
		enableQoS2:      opts.EnableQoS2,
		done:            make(chan struct{}),
	}

	keepSecs := int64(opts.KeepAlive / time.Second)
	if keepSecs > 65535 {
		keepSecs = 65535
	}
	// v0.32.0 阶段二：持久会话 opt-in（默认 false = 恒 clean，历史行为
	// 逐字节保留）。Session Expiry 毫秒→秒向上取整（60000ms→60s）。
	ck := &Connect{
		ClientID:     opts.ClientID,
		KeepAlive:    uint16(keepSecs),
		CleanSession: true, // 默认：v0.24.0 以来的恒 clean 行为
		Username:     opts.Username,
		Password:     opts.Password,
		// Will is intentionally not set.
		V5:         opts.ProtocolVersion5,
		ReceiveMax: opts.ReceiveMax, // v5：>0 时携带 RM 属性
	}
	if opts.PersistentSession {
		ck.CleanSession = false
		if opts.ProtocolVersion5 {
			secs := (opts.SessionExpiryMs + 999) / 1000 // 向上取整
			if secs > 0xFFFFFFFF {
				secs = 0xFFFFFFFF
			}
			ck.SessionExpiry = uint32(secs)
		}
	}
	if err := c.write(ck); err != nil {
		conn.Close()
		return nil, fmt.Errorf("mqtt: send CONNECT: %w", err)
	}
	c.v5 = opts.ProtocolVersion5
	p, err := decodePacketV(conn, opts.ProtocolVersion5)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mqtt: read CONNACK: %w", err)
	}
	ca, ok := p.(*Connack)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("mqtt: expected CONNACK, got packet type 0x%02X", p.Type())
	}
	if ca.ReturnCode != 0 {
		conn.Close()
		if ca.V5 {
			return nil, fmt.Errorf("mqtt: connect refused, reason code 0x%02X (%s)", ca.ReturnCode, v5ReasonText(ca.ReturnCode))
		}
		return nil, fmt.Errorf("mqtt: connect refused, return code %d (0x%02X)", ca.ReturnCode, ca.ReturnCode)
	}
	// v5 流控：服务器 CONNACK 下发 Receive Maximum 时启用出站在途窗口
	//（0/65535 = 未限制，不建槽）。
	if opts.ProtocolVersion5 && ca.ReceiveMax > 0 && ca.ReceiveMax < 65535 {
		c.flowSlots = make(chan struct{}, ca.ReceiveMax)
	}
	if ca.SessionPresent {
		c.sessionPresent = true
	}

	go c.readPump()
	if opts.KeepAlive > 0 {
		go c.pingLoop(opts.KeepAlive)
	}
	return c, nil
}

// write sends one packet; all writes share writeMu so goroutines (read pump
// acks, pinger, caller threads) never interleave packet bytes.
func (c *Client) write(p Packet) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return encodePacket(c.conn, p)
}

// nextID returns the next non-zero 16-bit packet identifier.
func (c *Client) nextID() uint16 {
	for {
		v := uint16(atomic.AddUint32(&c.packetID, 1))
		if v != 0 {
			return v
		}
	}
}

// ensureOpen reports ErrClientClosed once the read pump has exited (Close or
// dropped connection).
func (c *Client) ensureOpen() error {
	select {
	case <-c.done:
		return ErrClientClosed
	default:
		return nil
	}
}

// SessionPresent reports whether the server restored a previously stored
// session for this connection（CONNACK Session Present，v0.32.0 阶段二）。
// 仅在 Dial 成功后有意义的只读快照；与 Options.PersistentSession 搭配使用。
func (c *Client) SessionPresent() bool { return c.sessionPresent }

// Subscribe sends a SUBSCRIBE for a single filter and waits for the matching
// SUBACK. On a granted code (< 0x80) the handler is registered under the
// exact filter string; on rejection the error carries the SUBACK code.
// 共享订阅（$share/{group}/{filter}）的 handler 以内层 filter 注册。
func (c *Client) Subscribe(topic string, qos byte, h Handler) error {
	if h == nil {
		return errors.New("mqtt: nil handler")
	}
	if err := validateTopicFilter(topic); err != nil {
		return err
	}
	if err := c.ensureOpen(); err != nil {
		return err
	}
	pk := &Subscribe{
		PacketID: c.nextID(),
		Topics:   []TopicFilter{{Topic: topic, QoS: qos}},
		V5:       c.v5,
	}
	ch := c.registerAck(pk.PacketID)
	defer c.unregisterAck(pk.PacketID)
	if err := c.write(pk); err != nil {
		return err
	}
	ack, err := c.waitAck(ch)
	if err != nil {
		return err
	}
	sa, ok := ack.(*Suback)
	if !ok {
		return fmt.Errorf("mqtt: expected SUBACK for packet id %d", pk.PacketID)
	}
	if len(sa.Codes) == 0 {
		return errors.New("mqtt: empty SUBACK")
	}
	if sa.Codes[0] >= 0x80 {
		return fmt.Errorf("mqtt: subscribe rejected, code 0x%02X", sa.Codes[0])
	}
	// v0.32.0：共享订阅 handler 以内层 filter 注册（到达 PUBLISH 的
	// topic 已剥去 $share/{group}/ 前缀，须按内层匹配）。
	regKey := topic
	if group, inner, ok := parseShareFilterClient(topic); ok && group != "" {
		regKey = inner
	}
	c.handlersMu.Lock()
	c.handlers[regKey] = append(c.handlers[regKey], h)
	c.handlersMu.Unlock()
	if c.sessionPresent {
		c.flushRecovered(regKey)
	}
	return nil
}

// matchHandlersHit 报告是否有已注册 handler 匹配 topic（恢复会话缓冲判定）。
func (c *Client) matchHandlersHit(topic string) bool {
	c.handlersMu.RLock()
	defer c.handlersMu.RUnlock()
	for filter := range c.handlers {
		if MatchTopic(filter, topic) {
			return true
		}
	}
	return false
}

// flushRecovered 把缓冲的恢复期消息按新注册 filter 补投（Subscribe 调用）。
func (c *Client) flushRecovered(filter string) {
	c.pendMu.Lock()
	kept := c.pendingRecovered[:0]
	var deliver []*Publish
	for _, pv := range c.pendingRecovered {
		if MatchTopic(filter, pv.Topic) {
			deliver = append(deliver, pv)
		} else {
			kept = append(kept, pv)
		}
	}
	c.pendingRecovered = kept
	c.pendMu.Unlock()
	for _, pv := range deliver {
		for _, h := range c.matchHandlers(pv.Topic) {
			h(pv.Topic, pv.Payload)
		}
	}
}

// parseShareFilterClient 拆解 $share/{group}/{filter}（client 侧 handler
// 注册键解析；与 broker 侧语义一致）。普通 filter 返回 (""，原文, true)。
func parseShareFilterClient(f string) (group, inner string, ok bool) {
	if !strings.HasPrefix(f, "$share/") {
		return "", f, true
	}
	rest := f[len("$share/"):]
	i := strings.Index(rest, "/")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// Publish sends a PUBLISH. QoS 0 is fire-and-forget; QoS 1 waits up to
// ackTimeout for the broker's PUBACK; QoS 2 (v0.26.0) runs the full
// PUBLISH→PUBREC→PUBREL→PUBCOMP handshake and waits for each leg, but only
// when the client was dialed with Options.EnableQoS2 — otherwise QoS 2 is
// rejected before it ever reaches the wire (the v0.24.0 contract).
func (c *Client) Publish(topic string, qos byte, payload []byte) error {
	if err := validateTopicName(topic); err != nil {
		return err
	}
	if qos > 2 || (qos == 2 && !c.enableQoS2) {
		return fmt.Errorf("mqtt: unsupported QoS %d", qos)
	}
	if err := c.ensureOpen(); err != nil {
		return err
	}
	pk := &Publish{QoS: qos, Topic: topic, Payload: payload, V5: c.v5}
	if qos == 0 {
		return c.write(pk)
	}
	pk.PacketID = c.nextID()
	// v5 流控（v0.30.0）：QoS1/2 出站受服务器 Receive Maximum 节流。
	// QoS0 不占槽（规范语义：流控仅约束 QoS>0）。获取槽前阻塞等待；
	// defer 在全部退出路径（成功/超时/断连）释放。
	if c.flowSlots != nil {
		select {
		case c.flowSlots <- struct{}{}:
		case <-c.done:
			return ErrClientClosed
		}
		defer func() { <-c.flowSlots }()
	}
	ch := c.registerAck(pk.PacketID)
	defer c.unregisterAck(pk.PacketID)
	if err := c.write(pk); err != nil {
		return err
	}
	if qos == 1 {
		ack, err := c.waitAck(ch)
		if err != nil {
			return err
		}
		pa, ok := ack.(*Puback)
		if !ok {
			return fmt.Errorf("mqtt: expected PUBACK for packet id %d", pk.PacketID)
		}
		// v5：0x10（无匹配订阅者）为警告级成功；其他非零码为失败。
		if pa.ReasonCode != 0 && pa.ReasonCode != MQTTV5NoMatchingSubscribers {
			return fmt.Errorf("mqtt: publish rejected, reason code 0x%02X (%s)", pa.ReasonCode, v5ReasonText(pa.ReasonCode))
		}
		return nil
	}
	// QoS 2 (v0.27.0): record the outbound exchange before waiting, so a
	// crash/disconnect between legs leaves a replayable record. Removed on
	// PUBCOMP. Persistence disabled (dir "") = no-op.
	if err := qos2Save(c.persistDir, qos2Record{Kind: 'o', Phase: 1, PktID: pk.PacketID, Topic: topic, Payload: payload}); err != nil {
		return fmt.Errorf("mqtt: persist qos2 outbound: %w", err)
	}
	// QoS 2: PUBLISH→PUBREC→PUBREL→PUBCOMP. Both legs wait on the same
	// buffered channel (registered once for this packet id): the waiter
	// drains the PUBREC delivery before blocking again, so the PUBCOMP
	// from resolveAck lands in the same slot.
	rec, err := c.waitAck(ch)
	if err != nil {
		return err
	}
	pr, ok := rec.(*Pubrec)
	if !ok {
		return fmt.Errorf("mqtt: expected PUBREC for packet id %d", pk.PacketID)
	}
	if pr.ReasonCode != 0 && pr.ReasonCode != MQTTV5NoMatchingSubscribers {
		return fmt.Errorf("mqtt: qos2 rejected at PUBREC, reason code 0x%02X (%s)", pr.ReasonCode, v5ReasonText(pr.ReasonCode))
	}
	// Phase 2: advance the record before sending PUBREL.
	if err := qos2Save(c.persistDir, qos2Record{Kind: 'o', Phase: 2, PktID: pk.PacketID, Topic: topic, Payload: payload}); err != nil {
		return fmt.Errorf("mqtt: persist qos2 outbound phase 2: %w", err)
	}
	rel := &Pubrel{PacketID: pk.PacketID, V5: c.v5}
	if err := c.write(rel); err != nil {
		return err
	}
	comp, err := c.waitAck(ch)
	if err != nil {
		return err
	}
	if _, ok := comp.(*Pubcomp); !ok {
		return fmt.Errorf("mqtt: expected PUBCOMP for packet id %d", pk.PacketID)
	}
	// Exchange complete: drop the record (no-op when disabled).
	if err := qos2Remove(c.persistDir, pk.PacketID); err != nil {
		return fmt.Errorf("mqtt: clear qos2 record: %w", err)
	}
	return nil
}

// Close is idempotent: it sends DISCONNECT (best effort), closes the
// connection and waits for the read pump to exit.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		_ = c.write(&Disconnect{}) // best effort; conn may already be gone
		_ = c.conn.Close()
		<-c.done // wait until the read pump observes the closed conn
	})
	return nil
}

// readPump decodes inbound packets until the connection fails or is closed;
// on exit it closes c.done, unblocking ack waiters and stopping the pinger.
// It intentionally does NOT reconnect — dial-back and session recovery are
// owned by the upper Mapper layer.
func (c *Client) readPump() {
	defer close(c.done)
	for {
		p, err := decodePacketV(c.conn, c.v5)
		if err != nil {
			return
		}
		switch pv := p.(type) {
		case *Publish:
			if validateTopicName(pv.Topic) != nil {
				continue // malformed topic: skip the packet, keep the connection
			}
			if pv.QoS == 1 {
				// QoS1 inbound must be acknowledged so the broker does not resend.
				_ = c.write(&Puback{V5: c.v5, PacketID: pv.PacketID})
			}
			// 恢复会话缓冲（复核 P1-1 修复：仅 QoS<2；QoS2 有独立 park/
			// PUBREC 状态机，不得被缓冲拦截）：
			if c.sessionPresent && pv.QoS < 2 && !c.matchHandlersHit(pv.Topic) {
				// 消息先于 handler 注册到达（broker 在 CONNACK 后立即
				// 下发离线队列）→ 缓冲待 Subscribe 补投。
				c.pendMu.Lock()
				if len(c.pendingRecovered) >= 32 {
					c.pendingRecovered = c.pendingRecovered[1:]
				}
				c.pendingRecovered = append(c.pendingRecovered, pv)
				c.pendMu.Unlock()
				continue
			}
			if pv.QoS == 2 {
				// QoS2 inbound (v0.26.0): ack PUBLISH with PUBREC and park the
				// message until the broker's PUBREL; delivery happens only
				// after the release leg, exactly once. The broker must send
				// PUBREL for the exchange to complete (MQTT 3.1.1 §4.3.3).
				// v0.27.0: park is also persisted so a broker PUBREL on a
				// later connection can still complete the delivery.
				c.pendingMu.Lock()
				c.pendingDownQoS2[pv.PacketID] = pv
				c.pendingMu.Unlock()
				if err := qos2Save(c.persistDir, qos2Record{Kind: 'i', Phase: 1, PktID: pv.PacketID, Topic: pv.Topic, Payload: pv.Payload}); err != nil {
					// Persist failure must not block the protocol reply: the
					// exchange continues in memory; the record gap is logged
					// by the upper layer via ResumePending absence.
					// (Deliberate soft-fail: delivery semantics stay intact.)
				}
				_ = c.write(&Pubrec{V5: c.v5, PacketID: pv.PacketID})
				continue
			}
			for _, h := range c.matchHandlers(pv.Topic) {
				h(pv.Topic, pv.Payload)
			}
		case *Pubrel:
			// Release leg of an inbound QoS2 exchange: deliver the parked
			// PUBLISH exactly once, then acknowledge with PUBCOMP.
			c.pendingMu.Lock()
			parked := c.pendingDownQoS2[pv.PacketID]
			delete(c.pendingDownQoS2, pv.PacketID)
			c.pendingMu.Unlock()
			if parked != nil {
				for _, h := range c.matchHandlers(parked.Topic) {
					h(parked.Topic, parked.Payload)
				}
			}
			_ = c.write(&Pubcomp{PacketID: pv.PacketID})
			// Exchange complete: drop the record (no-op when disabled).
			_ = qos2Remove(c.persistDir, pv.PacketID)
		case *Puback:
			c.resolveAck(pv.PacketID, pv)
		case *Pubrec:
			c.resolveAck(pv.PacketID, pv)
		case *Pubcomp:
			c.resolveAck(pv.PacketID, pv)
		case *Suback:
			c.resolveAck(pv.PacketID, pv)
		case *Disconnect:
			// 服务器主动断连（v5 流控违规 0x93 等）：会话终止，读泵退出
			// 并关闭 done，waiters 经 waitAck 的 done 分支报错返回。
			return
		default:
			// PINGRESP and any other packet: ignore.
		}
	}
}

// pingLoop sends PINGREQ every keepAlive interval until the client is done.
func (c *Client) pingLoop(keepAlive time.Duration) {
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := c.write(&Pingreq{}); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

// matchHandlers snapshots the handlers of every registered filter matching
// topic (read lock held only for the snapshot; handlers run unlocked).
func (c *Client) matchHandlers(topic string) []Handler {
	c.handlersMu.RLock()
	defer c.handlersMu.RUnlock()
	var hs []Handler
	for filter, list := range c.handlers {
		if MatchTopic(filter, topic) {
			hs = append(hs, list...)
		}
	}
	return hs
}

// registerAck installs a waiter channel for the given packet identifier.
// The channel is buffered so a late ack can never block the read pump.
func (c *Client) registerAck(id uint16) chan Packet {
	ch := make(chan Packet, 1)
	c.pendingMu.Lock()
	c.pendingAcks[id] = ch
	c.pendingMu.Unlock()
	return ch
}

// unregisterAck removes the waiter; called via defer so timed-out waiters do
// not leak map entries.
func (c *Client) unregisterAck(id uint16) {
	c.pendingMu.Lock()
	delete(c.pendingAcks, id)
	c.pendingMu.Unlock()
}

// resolveAck delivers an ack packet to the waiter, if any.
func (c *Client) resolveAck(id uint16, p Packet) {
	c.pendingMu.Lock()
	ch, ok := c.pendingAcks[id]
	c.pendingMu.Unlock()
	if !ok {
		return // late/unsolicited ack: drop
	}
	select {
	case ch <- p:
	default:
	}
}

// waitAck blocks for the ack, the ack timeout, or client shutdown.
func (c *Client) waitAck(ch chan Packet) (Packet, error) {
	select {
	case p := <-ch:
		return p, nil
	case <-time.After(ackTimeout):
		return nil, fmt.Errorf("mqtt: ack timeout after %s", ackTimeout)
	case <-c.done:
		return nil, ErrClientClosed
	}
}
