// Package mqttsim provides a minimal in-process MQTT 3.1.1 test broker for
// EdgeFlow integration tests. It builds on pkg/mqtt's exported packet types
// and, since the v0.25.0 R-6 consolidation, on pkg/mqtt's exported wire
// codec (EncodePacket/DecodePacket) and matcher (MatchTopic) through thin
// same-name shims at the bottom of this file — the shims exist only so the
// frozen v0240_sim_test.go keeps compiling unchanged.
//
// Boundary: QoS 0/1 subset, 127.0.0.1 ephemeral listener, best-effort
// outbound queues, no authentication beyond refusing an empty ClientID.
package mqttsim

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"edgeflow/pkg/mqtt"
)

// outQueueSize bounds each client's outbound queue. A send that would exceed
// it is dropped and counted in Broker.dropCount (best-effort test broker).
const outQueueSize = 32

// subFailureCode is the SUBACK code for a rejected topic filter.
const subFailureCode = 0x80

// Broker is a minimal MQTT test broker listening on 127.0.0.1 on an
// ephemeral port. Zero dependency beyond the pkg/mqtt codec.
type Broker struct {
	ln        net.Listener
	mu        sync.Mutex
	clients   map[*simClient]struct{}
	received  []mqtt.Publish
	pingCount int
	dropCount int

	// MQTT 5.0（v0.30.0 阶段一）：receiveMax>0 时 CONNACK 携带 Receive
	// Maximum 且对上行 QoS2 暂存深度做流控强制（超限 DISCONNECT 0x93）；
	// username 非空时启用鉴权（v5 失败码 0x86 / 3.1.1 失败码 0x04）。
	receiveMax uint16
	username   string
	password   string

	// pendingQoS2 parks upstream QoS2 PUBLISH packets until the sender's
	// PUBREL arrives; delivery happens only after the release leg (v0.26.0).
	// Keyed per connection (*simClient) then per PacketID: MQTT packet
	// identifiers are only unique within a single connection, so a global
	// PacketID map would let concurrent clients clobber each other's
	// in-flight exchanges.
	pendingQoS2 map[*simClient]map[uint16]*mqtt.Publish

	// persistDir is the broker-side QoS2 record directory (v0.27.0).
	// Empty (the default) = disabled, v0.26.0 behavior unchanged. Set via
	// NewBrokerWithOptions before the first connection.
	persistDir string

	// orphanQoS2 holds records loaded from persistDir at startup (v0.27.0):
	// parked messages whose sender connection died with the previous broker
	// process. A later release leg for the same packet id (from any
	// connection) completes the delivery exactly once and drops the
	// orphan. Per-connection parks always win over orphans.
	orphanQoS2 map[uint16]*mqtt.Publish

	// ---- 会话解耦 + 共享订阅（v0.32.0 阶段二）----
	// sessions 按 ClientID 保留断连后的会话（订阅表 + 离线 QoS1 队列）。
	// expirySec=0 的会话断连即毁（现状行为），不入此表。
	sessions map[string]*simSession
	// shareCursor 是共享订阅组 (group,inner) 的 round-robin 游标。
	shareCursor map[string]uint64
	// pktID 分配离线 QoS1 恢复下行的报文标识（broker 侧自增循环）。
	pktID uint32

	closeOnce sync.Once
	closed    bool
}

// shareSub 是一条共享订阅（v0.32.0）：原始 filter 串 → 组名 + 内层 filter。
type shareSub struct {
	group string
	inner string
}

// simSession 是断连后按 ClientID 保留的会话（v0.32.0 阶段二）：订阅表
// 快照 + 离线 QoS1 暂存队列。conn 指向当前在线连接（nil = 离线）。
type simSession struct {
	clientID   string
	conn       *simClient      // 非 nil = 在线（订阅表以连接上的为准）
	expirySec  uint32          // 0 = 断连即毁；0xFFFFFFFF = 3.1.1 永久/极长 v5 值
	expiresAt  time.Time       // 断连时刻 + Session Expiry；在线期为零值
	filters    map[string]byte // 订阅表快照（filter -> granted QoS；v0330 由 struct{} 改为 granted）
	subOpts    map[string]byte // 订阅选项快照（filter -> 选项位 0x04 NoLocal|0x08 RAP；v0330）
	shared     map[string]shareSub
	offline    []*mqtt.Publish // 离线 QoS1 暂存（≤64，超限丢最旧；头部 dispatched 条已入连接队列在途）
	offlineDr  int             // 超限丢弃计数
	dispatched int             // 已入连接队列待 PUBACK 的条数（恢复下发窗口，v0.32.0）
	mu         sync.Mutex      // 保护 offline/dispatched（确认出队 vs 快照下发）

	// aliases 是入站 Topic Alias 映射（v0.33.0 阶段三，≤16）：alias → 主题。
	// ⚠️ 由 sess.mu 保护（复核 P0-1：接管时 shutdown 只等 pumpDone、不等
	// 旧 serve 退出，新旧 serve 可短暂并发——confined 论证不成立）；
	// 随会话保留/销毁语义走（断连保留、Clean Start 重建即清）。
	aliases map[uint16]string
}

// recoverWindow 是会话恢复下发的在途窗口（≤ outQueueSize/2，防连接队列
// 溢出丢帧——v0320 门禁发现的真缺陷修复：一次性灌 64 条超 out 队列容量）。
const recoverWindow = outQueueSize / 2

// nextPktID 分配 broker 侧下行报文标识（循环，跳过 0）。
func (b *Broker) nextPktID() uint16 {
	for {
		v := uint16(atomic.AddUint32(&b.pktID, 1) & 0xFFFF)
		if v != 0 {
			return v
		}
	}
}

// expiryZero 报告会话是否已过期（在线会话永不过期）。
func (s *simSession) expiryZero() bool {
	return s.conn == nil && !s.expiresAt.IsZero() && time.Now().After(s.expiresAt)
}

// offlineCap 是会话离线 QoS1 暂存上限。
const offlineCap = 64

// matchesNormal 报告会话普通订阅是否匹配 topic（离线暂存判定用）。
func (s *simSession) matchesNormal(topic string) bool {
	for f := range s.filters {
		if simMatchTopic(f, topic) {
			return true
		}
	}
	return false
}

// simClient is one accepted connection: its subscription filters and its
// outbound byte queue drained by a dedicated pump goroutine.
type simClient struct {
	br        *Broker
	conn      net.Conn
	mu        sync.Mutex
	filters   map[string]byte     // 普通订阅 filter -> granted QoS（v0.33.0：struct{}→byte；v3 路径行为不变）
	subOpts   map[string]byte     // 普通订阅 filter -> v5 选项位（0x04 NoLocal | 0x08 RAP；v0.33.0）
	shared    map[string]shareSub // 共享订阅（原始 filter -> 组/内层，v0.32.0）
	clientID  string              // CONNECT 后填充（会话归属/接管判定）
	sess      *simSession         // 绑定的会话（v0.32.0；nil = 恒 clean 老路径）
	out       chan []byte
	done      chan struct{}
	doneOnce  sync.Once
	pumpDone  chan struct{} // v0.30.0：泵退出信号（关停等队列清空后再关连接）
	closeOnce sync.Once
	connV5    bool // 该连接经 v5 CONNECT 协商（v0.30.0）
}

// NewBroker starts a plaintext listener and the accept loop.
func NewBroker() (*Broker, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return newBrokerFromListener(ln), nil
}

// NewBrokerTLS starts a TLS listener and the accept loop (v0.25.0). tlsCfg
// must be a non-nil server-side config (typically Certificates set). TLS
// terminates at the listener: serve()/pump() still see a plain net.Conn,
// so the rest of the broker is byte-for-byte identical to the plaintext
// path.
func NewBrokerTLS(tlsCfg *tls.Config) (*Broker, error) {
	if tlsCfg == nil {
		return nil, errors.New("mqttsim: NewBrokerTLS requires a non-nil *tls.Config")
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return newBrokerFromListener(tls.NewListener(raw, tlsCfg)), nil
}

// NewBrokerWithOptions starts a plaintext listener with v0.27.0 options
// (persistDir: QoS2 parked-message record directory; "" = disabled, the
// v0.26.0 behavior). Existing NewBroker/NewBrokerTLS keep their signatures
// and semantics.
func NewBrokerWithOptions(persistDir string) (*Broker, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := newBrokerFromListener(ln)
	b.brokerSetPersistDir(persistDir)
	return b, nil
}

// BrokerConfig 是 NewBrokerWithConfig 的配置（v0.30.0）：零值 = NewBroker
// 行为。ReceiveMax>0 时向 v5 客户端下发并在服务端强制（上行 QoS2 暂存
// 深度超限 → DISCONNECT 0x93）；Username 非空时启用鉴权。
type BrokerConfig struct {
	PersistDir string
	ReceiveMax uint16
	Username   string
	Password   string
}

// NewBrokerWithConfig 以扩展配置启动 broker（v0.30.0）。既有构造函数
// 语义不变。
func NewBrokerWithConfig(cfg BrokerConfig) (*Broker, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := newBrokerFromListener(ln)
	b.brokerSetPersistDir(cfg.PersistDir)
	b.receiveMax = cfg.ReceiveMax
	b.username = cfg.Username
	b.password = cfg.Password
	return b, nil
}

// newBrokerFromListener wires up the broker around an already-created
// listener and starts the accept loop.
func newBrokerFromListener(ln net.Listener) *Broker {
	b := &Broker{
		ln:          ln,
		clients:     make(map[*simClient]struct{}),
		pendingQoS2: make(map[*simClient]map[uint16]*mqtt.Publish),
		sessions:    make(map[string]*simSession),
		shareCursor: make(map[string]uint64),
	}
	go b.acceptLoop()
	return b
}

// Addr returns the listener address string; it stays readable after Close.
func (b *Broker) Addr() string { return b.ln.Addr().String() }

// Close shuts down the listener and every client connection. Idempotent.
func (b *Broker) Close() error {
	var err error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		snap := make([]*simClient, 0, len(b.clients))
		for c := range b.clients {
			snap = append(snap, c)
		}
		b.mu.Unlock()
		err = b.ln.Close()
		for _, c := range snap {
			c.shutdown() // closes conn; reader/pump goroutines exit
		}
	})
	return err
}

func (b *Broker) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return // listener closed (or fatal accept error)
		}
		c := &simClient{
			br:       b,
			conn:     conn,
			filters:  make(map[string]byte),
			subOpts:  make(map[string]byte),
			shared:   make(map[string]shareSub),
			out:      make(chan []byte, outQueueSize),
			done:     make(chan struct{}),
			pumpDone: make(chan struct{}),
		}
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			conn.Close()
			return
		}
		b.clients[c] = struct{}{}
		b.mu.Unlock()
		go c.pump()
		go c.serve()
	}
}

func (b *Broker) unregister(c *simClient) {
	b.mu.Lock()
	delete(b.clients, c)
	delete(b.pendingQoS2, c) // drop any QoS2 exchanges parked by this connection
	// 会话解耦（v0.32.0）：本连接仍持有会话时（未被新连接接管），
	// 按 Session Expiry 决定保留或销毁；保留的会话接管订阅表快照。
	if c.sess != nil && c.sess.conn == c {
		c.sess.conn = nil
		if c.sess.expirySec == 0 {
			delete(b.sessions, c.sess.clientID) // 断连即毁（恒 clean 现状行为）
		} else {
			c.sess.expiresAt = time.Now().Add(time.Duration(c.sess.expirySec) * time.Second)
			c.mu.Lock()
			c.sess.filters = make(map[string]byte, len(c.filters))
			for f, g := range c.filters {
				c.sess.filters[f] = g
			}
			c.sess.subOpts = make(map[string]byte, len(c.subOpts))
			for f, o := range c.subOpts {
				c.sess.subOpts[f] = o
			}
			c.sess.shared = make(map[string]shareSub, len(c.shared))
			for f, sh := range c.shared {
				c.sess.shared[f] = sh
			}
			c.mu.Unlock()
		}
	}
	b.mu.Unlock()
}

// enqueue encodes pkt and puts it on the client's outbound queue without
// blocking; if the queue is full the packet is dropped and counted.
func (b *Broker) enqueue(c *simClient, pkt mqtt.Packet) {
	var buf bytes.Buffer
	if err := encodePacket(&buf, pkt); err != nil {
		return // unrecoverable for a test broker; drop silently
	}
	select {
	case c.out <- buf.Bytes():
	default:
		b.mu.Lock()
		b.dropCount++
		b.mu.Unlock()
	}
}

// Publish pushes a server-originated message (QoS 0) to every client whose
// subscription matches topic. Having no subscriber is not an error.
func (b *Broker) Publish(topic string, payload []byte) error {
	b.fanoutBytes(nil, topic, payload, 0)
	return nil
}

// hasSubscriber 报告是否有任何客户端订阅（含共享订阅，v0.32.0）匹配
// topic（v0.30.0：PUBACK 0x10 判定用）。
func (b *Broker) hasSubscriber(topic string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.clients {
		if c.matches(topic) || len(c.sharedMatches(topic)) > 0 {
			return true
		}
	}
	return false
}

// hasOfflineInterest 报告是否有保留中的离线会话订阅匹配 topic（v0.32.0）。
func (b *Broker) hasOfflineInterest(topic string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for cid, sess := range b.sessions {
		if sess.conn != nil {
			continue
		}
		if !sess.expiresAt.IsZero() && time.Now().After(sess.expiresAt) {
			delete(b.sessions, cid)
			continue
		}
		if sess.matchesNormal(topic) {
			return true
		}
	}
	return false
}

// fanout distributes a client-originated publish to all matching subscribers
// (the sender included, if subscribed). Always re-encoded as QoS 0.
// v0.30.0：按连接编码——v5 客户端的 PUBLISH 需携带属性长度字节（v5 帧形态）。
func (b *Broker) fanout(topic string, payload []byte) {
	b.fanoutBytes(nil, topic, payload, 0)
}

func (b *Broker) fanoutBytes(publisher *simClient, topic string, data []byte, qos byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// 快照在线成员，按 ClientID 排序保证轮转确定性。
	type member struct {
		c      *simClient
		cid    string
		h      normalHit // 普通订阅命中信息（granted/NoLocal/RAP，v0.33.0）
		normal bool
		shares []shareSub
	}
	var members []member
	for c := range b.clients {
		m := member{c: c, cid: c.clientID}
		m.h = c.matchesInfo(topic)
		m.normal = m.h.hit
		m.shares = c.sharedMatches(topic)
		if m.normal || len(m.shares) > 0 {
			members = append(members, m)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].cid < members[j].cid })

	// 入队辅助：持 b.mu 路径专用（复核 P0-1——enqueueQoS 的队列满分支
	// 会 Lock b.mu，持锁调用 = 非重入自死锁；此处内联 select 恢复旧行为）。
	inlineEnqueue := func(c *simClient, pkt *mqtt.Publish) {
		var buf bytes.Buffer
		if err := encodePacket(&buf, pkt); err != nil {
			return
		}
		select {
		case c.out <- buf.Bytes():
		default:
			b.dropCount++
		}
	}
	for _, m := range members {
		// 普通订阅：每连接至多一份（既有语义）。
		if !m.normal {
			continue
		}
		// v0.33.0 NoLocal：全部命中条目均 NoLocal 且发布者 = 自己 → 跳过。
		if publisher != nil && m.c == publisher && m.h.noLocalAll {
			continue
		}
		// v0.33.0：v5 订阅 granted≥1 → QoS1 下行（在途窗口/会话队列）；
		// v3.1.1 与 granted=0 维持 QoS0 best-effort 现状（冻结）。
		// 共享订阅不受影响（维持 QoS0，spec 0006 边界登记）。
		if m.c.connV5 && m.h.granted >= 1 && qos >= 1 {
			pktID := b.nextPktID()
			sess := m.c.sess
			direct := false
			sess.mu.Lock()
			if len(sess.offline) >= offlineCap {
				sess.offline = sess.offline[1:]
				sess.offlineDr++
			}
			sess.offline = append(sess.offline, &mqtt.Publish{QoS: 1, Topic: topic, Payload: append([]byte(nil), data...), PacketID: pktID})
			if sess.dispatched < recoverWindow {
				sess.dispatched++
				direct = true
			}
			sess.mu.Unlock()
			if direct {
				inlineEnqueue(m.c, &mqtt.Publish{Topic: topic, Payload: data, QoS: 1, PacketID: pktID, V5: true})
			}
			continue
		}
		inlineEnqueue(m.c, &mqtt.Publish{Topic: topic, Payload: data, V5: m.c.connV5})
	}
	// 共享订阅：按组键 (group,inner) 分组，组内 round-robin 选一在线成员。
	groupHits := make(map[shareSub][]*simClient)
	for i := range members {
		for _, sh := range members[i].shares {
			groupHits[sh] = append(groupHits[sh], members[i].c)
		}
	}
	picked := make(map[*simClient]map[shareSub]struct{})
	for sh, cands := range groupHits {
		key := sh.group + "\x00" + sh.inner
		idx := b.shareCursor[key] % uint64(len(cands))
		b.shareCursor[key]++
		target := cands[idx]
		if picked[target] == nil {
			picked[target] = make(map[shareSub]struct{})
		}
		picked[target][sh] = struct{}{}
	}
	for c, subs := range picked {
		for range subs { // 每个命中组一份（同连接同组多 filter 已去重）
			inlineEnqueue(c, &mqtt.Publish{Topic: topic, Payload: data, V5: c.connV5})
		}
	}

	// 离线会话暂存（v0.32.0）：仅 QoS1、普通订阅；共享订阅不暂存（spec
	// 0005 US-5）。惰性清理过期会话。
	for cid, sess := range b.sessions {
		if sess.conn != nil {
			continue // 在线会话由上方 members 覆盖
		}
		if !sess.expiresAt.IsZero() && time.Now().After(sess.expiresAt) {
			delete(b.sessions, cid)
			continue
		}
		if qos != 1 || !sess.matchesNormal(topic) {
			continue
		}
		sess.mu.Lock()
		if len(sess.offline) >= offlineCap {
			sess.offline = sess.offline[1:]
			sess.offlineDr++
		}
		sess.offline = append(sess.offline, &mqtt.Publish{QoS: 1, Topic: topic, Payload: append([]byte(nil), data...)})
		sess.mu.Unlock()
	}
}

// enqueueQoS 以指定 QoS 编码并入队（pktID 由调用方分配——恢复下发路径
// 由在途窗口管理；qos=0 即既有 best-effort 语义）。⚠️ 队列满分支会锁
// b.mu——仅限不持有 b.mu 的调用方（恢复下发/PUBACK 补发）；fanoutBytes
// 持锁路径必须内联（复核 P0-1 死锁修复）。
func (b *Broker) enqueueQoS(c *simClient, topic string, data []byte, qos byte, pktID uint16, dup byte) {
	var buf bytes.Buffer
	if err := encodePacket(&buf, &mqtt.Publish{Topic: topic, Payload: data, QoS: qos, PacketID: pktID, Dup: dup, V5: c.connV5}); err != nil {
		return
	}
	select {
	case c.out <- buf.Bytes():
	default:
		b.mu.Lock()
		b.dropCount++
		b.mu.Unlock()
	}
}

// recordPublish stores a deep copy of a client PUBLISH for test assertions.
func (b *Broker) recordPublish(p *mqtt.Publish) {
	b.mu.Lock()
	b.received = append(b.received, mqtt.Publish{
		Dup:      p.Dup,
		QoS:      p.QoS,
		Retain:   p.Retain,
		Topic:    p.Topic,
		PacketID: p.PacketID,
		Payload:  append([]byte(nil), p.Payload...),
	})
	b.mu.Unlock()
}

// Received returns a deep copy of every PUBLISH received from clients.
func (b *Broker) Received() []mqtt.Publish {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]mqtt.Publish, len(b.received))
	for i, p := range b.received {
		out[i] = p
		out[i].Payload = append([]byte(nil), p.Payload...)
	}
	return out
}

// PingCount reports how many PINGREQ packets have been processed.
func (b *Broker) PingCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pingCount
}

// parseShareFilter 拆解共享订阅 filter（v0.32.0）：
//   - "$share/{group}/{inner}" → (group, inner, true)；group/inner 任一为空 → !ok
//   - "$queue/..." → 不支持，!ok
//   - 其余 → (""，原文，true) 普通订阅
func parseShareFilter(f string) (group, inner string, ok bool) {
	if strings.HasPrefix(f, "$queue/") {
		return "", "", false
	}
	if !strings.HasPrefix(f, "$share/") {
		return "", f, true
	}
	rest := f[len("$share/"):]
	i := strings.Index(rest, "/")
	if i <= 0 || i == len(rest)-1 { // 无组/空组；无内层/空内层
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// sharedMatches 返回该连接命中的共享订阅组键去重集合（caller 持有 c.mu
// 或保证独占；与 matches 同样的锁约定）。
func (c *simClient) sharedMatches(topic string) []shareSub {
	seen := make(map[shareSub]struct{})
	var out []shareSub
	for _, sh := range c.shared {
		if simMatchTopic(sh.inner, topic) {
			if _, dup := seen[sh]; !dup {
				seen[sh] = struct{}{}
				out = append(out, sh)
			}
		}
	}
	return out
}

// matches reports whether any of the client's filters match topic.
// Caller may hold b.mu; c.mu guarding keeps it race-free either way.
func (c *simClient) matches(topic string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for f := range c.filters {
		if simMatchTopic(f, topic) {
			return true
		}
	}
	return false
}

// normalHit 汇总普通订阅命中信息（v0.33.0：granted QoS + NoLocal/RAP 合并）。
type normalHit struct {
	hit        bool
	granted    byte // 命中条目的最大 granted QoS
	noLocalAll bool // 所有命中条目均 NoLocal（发布者自身 → 跳过）
	rap        bool // 任一命中条目 RAP（保留原 QoS；granted cap 1 下与按 granted 同值，存储登记）
}

// matchesInfo 报告普通订阅命中并汇总选项（v0.33.0；逐条目 NoLocal 语义：
// 仅当全部命中条目都 NoLocal 且发布者为自己时才跳过）。
func (c *simClient) matchesInfo(topic string) normalHit {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := normalHit{noLocalAll: true}
	for f, g := range c.filters {
		if simMatchTopic(f, topic) {
			h.hit = true
			if g > h.granted {
				h.granted = g
			}
			if c.subOpts[f]&0x04 == 0 {
				h.noLocalAll = false // 任一非 NoLocal 命中 → 不跳过
			}
			if c.subOpts[f]&0x08 != 0 {
				h.rap = true
			}
		}
	}
	return h
}

// pump drains the outbound queue onto the connection. It exits when the
// client is shut down or a write fails.
// v0.30.0：关停（done）后先清空既有队列再退出——恢复“单写者先到先发”
// 语义，鉴权 CONNACK / 流控 DISCONNECT 等关停前最后一报不再被丢；
// shutdown 等待泵退出后才关连接。
func (c *simClient) pump() {
	defer close(c.pumpDone)
	for {
		select {
		case buf := <-c.out:
			// v0320（复核 P0-1 同面补充修复）：写截止 5s——死消费者
			// （对端不读、内核缓冲满）会让阻塞写永挂，close(done) 无法
			// 中断进行中的 write；超时判连接死，放弃退出。正常客户端
			// （小帧、正常读）5s 绰绰有余，语义不变。
			_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.conn.Write(buf); err != nil {
				c.doneOnce.Do(func() { close(c.done) })
				return
			}
		case <-c.done:
			// v0.30.0 语义：关停后先清空既有队列再退出（单写者时序）。
			// v0320 补充（复核 P0-1 同面发现）：死消费者场景 write 可能
			// 永久阻塞（内核缓冲 + 4MB 积压、对端不读）→ drain 设写截止
			// 1s，超时放弃退出（正常关停小帧 <1s，语义不变）。
			_ = c.conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
			for {
				select {
				case buf := <-c.out:
					if _, err := c.conn.Write(buf); err != nil {
						return
					}
				default:
					return
				}
			}
		}
	}
}

// shutdown closes the connection exactly once and unregisters the client.
// v0.30.0：先等泵清空队列退出，再关连接（关停报文时序确定性）。
func (c *simClient) shutdown() {
	c.closeOnce.Do(func() {
		c.doneOnce.Do(func() { close(c.done) })
		<-c.pumpDone
		c.conn.Close()
	})
	c.br.unregister(c)
}

// serve is the per-connection read loop. The first packet must be CONNECT;
// afterwards CONNECT/SUBSCRIBE/PUBLISH/PINGREQ/DISCONNECT are handled and
// any decode or read error tears the connection down.
// v0.30.0：CONNECT 按级别字节分派（4=既有 3.1.1 路径逐字不变，5=v5：
// CONNACK v5 形态 + RM 下发 + 鉴权失败码 0x86）；v5 客户端上行 QoS2 暂存
// 深度超 server RM → DISCONNECT 0x93 断连。
func (c *simClient) serve() {
	defer c.shutdown()
	authed := false
	for {
		hint := true // CONNECT 前接受级别 4/5（首包自动分派）
		if authed {
			hint = c.connV5
		}
		pkt, err := decodePacketNegotiated(c.conn, hint)
		if err != nil {
			return // read error, ErrMalformed*, or closed conn
		}
		if !authed {
			con, ok := pkt.(*mqtt.Connect)
			if !ok {
				return // first packet must be CONNECT
			}
			if con.ClientID == "" {
				return // empty ClientID: refuse and close
			}
			if con.V5 {
				c.connV5 = true
			}
			c.clientID = con.ClientID
			if c.br.username != "" && (con.Username != c.br.username || con.Password != c.br.password) {
				// 鉴权失败：v5 回原因码 0x86，3.1.1 回 returnCode 4
				//（bad user name or password）。经队列回送 + 关停清空，
				// 保证报文上线后连接才关（单写者时序）。
				if c.connV5 {
					c.br.enqueue(c, &mqtt.Connack{V5: true, ReturnCode: mqtt.MQTTV5BadUserpass})
				} else {
					c.br.enqueue(c, &mqtt.Connack{ReturnCode: 4})
				}
				return
			}

			// ---- 会话解耦（v0.32.0 阶段二，仅 v5 生效）----
			// 3.1.1 连接整体保持 v0.24.0 以来的现状路径：无会话恢复、
			// 无接管仲裁（CleanSession=false 的持久意图不支持，登记
			// spec 0005 as-built）。v5：Clean Start 位 + Session Expiry。
			cleanStart := con.CleanSession // v5 位语义 = Clean Start
			var expirySec uint32
			if con.V5 {
				expirySec = con.SessionExpiry
			} else {
				cleanStart = true // 3.1.1：现状恒 clean 语义
			}
			present := false
			var restored *simSession
			c.br.mu.Lock()
			if old := c.br.sessions[con.ClientID]; old != nil {
				if cleanStart || old.expiryZero() {
					delete(c.br.sessions, con.ClientID) // Clean Start=1 或已过期 → 重建
				} else {
					present = true
					restored = old
				}
			}
			if restored != nil {
				// 恢复：订阅表与离线队列移交本连接。
				c.mu.Lock()
				for f, g := range restored.filters {
					c.filters[f] = g
				}
				for f, o := range restored.subOpts {
					c.subOpts[f] = o
				}
				for f, sh := range restored.shared {
					c.shared[f] = sh
				}
				c.mu.Unlock()
				restored.conn = c
				restored.expiresAt = time.Time{}
				restored.expirySec = expirySec // v5 DISCONNECT/重连可更新（简化：以新连接值为准）
				c.sess = restored
			} else {
				c.sess = &simSession{
					clientID:  con.ClientID,
					conn:      c,
					expirySec: expirySec,
					filters:   make(map[string]byte),
					shared:    make(map[string]shareSub),
					aliases:   make(map[uint16]string),
					expiresAt: time.Time{},
				}
				c.br.sessions[con.ClientID] = c.sess
			}
			// 接管（v0.32.0）：同 ClientID 冲突仲裁——仅当任一方持有
			// 持久会话意图（新连接 CleanStart=0/SE>0，或旧连接持久）时
			// 踢旧连接；双方均为 clean 时保持 v0.24.0 以来并存行为
			//（冻结兼容；3.1.1 恒 clean → 恒不踢，spec 0005 as-built）。
			var kicked []*simClient
			takeover := !cleanStart || expirySec > 0
			for oc := range c.br.clients {
				if oc != c && oc.clientID == con.ClientID && (takeover || (oc.sess != nil && oc.sess.expirySec > 0)) {
					kicked = append(kicked, oc)
				}
			}
			c.br.mu.Unlock()
			for _, oc := range kicked {
				oc.shutdown() // 旧连接断开；其 disconnect 看到会话已转属（sess.conn != oc），不会清订阅
			}

			if c.connV5 {
				c.br.enqueue(c, &mqtt.Connack{V5: true, ReturnCode: 0, ReceiveMax: c.br.receiveMax, SessionPresent: present, SessionExpiry: expirySec})
			} else {
				c.br.enqueue(c, &mqtt.Connack{ReturnCode: 0, SessionPresent: present})
			}
			// 离线 QoS1 恢复下发（恢复会话且队列非空）：CONNACK 先行，
			// 离线消息随后入同一 FIFO 队列（dup=0，PUBACK 到达出队；
			// 无重发——spec 0005 as-built）。
			if restored != nil {
				restored.mu.Lock()
				w := len(restored.offline)
				if w > recoverWindow {
					w = recoverWindow
				}
				// v0.33.0（复核 P1-1 修正）：前 dispatched 条（断连时刻真实
				// 在途）DUP=1 重发；其后的离线暂存条目（append 不动
				// dispatched，从未下发）DUP=0 首次下发。注意 dispatched 计
				// 数的是"曾入连接队列"条目，与 offline 头部对齐仅在
				// PUBACK 滑动场景成立；离线暂存追加在尾部。
				inflight := restored.dispatched
				if inflight > w {
					inflight = w
				}
				var batch []*mqtt.Publish
				for i := 0; i < w; i++ {
					m := restored.offline[i]
					m.PacketID = c.br.nextPktID()
					if i < inflight {
						m.Dup = 1
					}
					batch = append(batch, m)
				}
				restored.dispatched = w
				restored.mu.Unlock()
				// 锁外入队（锁序纪律：sess.mu 内不做 enqueueQoS）。
				// DUP 按条目标注：断连前在途 → 1；离线暂存首传 → 0。
				for _, m := range batch {
					c.br.enqueueQoS(c, m.Topic, m.Payload, 1, m.PacketID, m.Dup)
				}
			}
			authed = true
			continue
		}
		switch p := pkt.(type) {
		case *mqtt.Connect:
			return // duplicate CONNECT is a protocol violation
		case *mqtt.Subscribe:
			codes := make([]byte, len(p.Topics))
			c.mu.Lock()
			for i, tf := range p.Topics {
				group, inner, ok := parseShareFilter(tf.Topic)
				if !ok || mqtt.ValidateTopicFilter(inner) != nil {
					codes[i] = subFailureCode
					continue
				}
				if group != "" && tf.NoLocal {
					// 共享订阅携带 NoLocal = 协议错误（MQTT-3.8.3-4，
					// 复核 P2）→ 0x80 拒绝该订阅。
					codes[i] = subFailureCode
					continue
				}
				if group == "" {
					if c.connV5 {
						// v0.33.0：v5 授予 = min(req,1)（请求 2 → 授予 1，
						// spec 0006 边界登记不回 0x9B）；选项位存储
						//（NoLocal/RAP 生效，RH 仅校验+登记——sim 无 retain 面）。
						g := tf.QoS
						if g > 1 {
							g = 1
						}
						c.filters[inner] = g
						var ob byte
						if tf.NoLocal {
							ob |= 0x04
						}
						if tf.RetainAsPublished {
							ob |= 0x08
						}
						if ob != 0 {
							c.subOpts[inner] = ob
						} else {
							delete(c.subOpts, inner)
						}
						codes[i] = g
					} else {
						// v3.1.1 现状冻结：codes = 请求 QoS；下行恒 QoS0
						//（granted 仅存档不改变下行行为）。
						c.filters[inner] = tf.QoS
						codes[i] = tf.QoS
					}
				} else {
					// 共享订阅下行维持 QoS0（v0.33.0 边界：granted-QoS1 仅普通订阅）。
					c.shared[tf.Topic] = shareSub{group: group, inner: inner}
					codes[i] = tf.QoS
				}
			}
			c.mu.Unlock()
			c.br.enqueue(c, &mqtt.Suback{PacketID: p.PacketID, Codes: codes, V5: c.connV5})
		case *mqtt.Publish:
			// v0.33.0：入站 Topic Alias 解映射（仅 v5）。alias=0 视为未
			// 携带（codec 无法区分携带 0 与未携带——spec 0006 边界登记）。
			// aliases 由 sess.mu 保护（复核 P0-1：接管时新旧 serve 短暂
			// 并发）；DISCONNECT 入队在 sess.mu 锁外（锁序纪律）。
			if p.V5 && p.TopicAlias != 0 && c.sess != nil {
				var aliasRc byte // 0 = 正常；非 0 = 待断开原因码
				c.sess.mu.Lock()
				if p.Topic == "" {
					// alias-only：解映射；未建立 → 0x94。
					if t, ok := c.sess.aliases[p.TopicAlias]; ok {
						p.Topic = t
					} else {
						aliasRc = mqtt.MQTTV5TopicAliasInvalid
					}
				} else if _, exists := c.sess.aliases[p.TopicAlias]; !exists && len(c.sess.aliases) >= 16 {
					// 新键且映射表满（≤16）→ 0x94。
					aliasRc = mqtt.MQTTV5TopicAliasInvalid
				} else {
					c.sess.aliases[p.TopicAlias] = p.Topic
				}
				c.sess.mu.Unlock()
				if aliasRc != 0 {
					c.br.enqueue(c, &mqtt.Disconnect{V5: true, ReasonCode: aliasRc})
					return
				}
			}
			if p.QoS == 2 {
				// QoS2 upstream (v0.26.0): park the PUBLISH, ack with
				// PUBREC; delivery waits for the sender's PUBREL. Parked
				// per connection so concurrent clients may reuse the same
				// packet id without interference.
				c.br.mu.Lock()
				persistDir := c.br.persistDir
				perConn := c.br.pendingQoS2[c]
				if perConn == nil {
					perConn = make(map[uint16]*mqtt.Publish)
					c.br.pendingQoS2[c] = perConn
				}
				// v0.30.0：服务端流控强制——v5 连接的暂存深度达 server RM
				// 时拒绝入站（规范 DISCONNECT 0x93）。
				if c.connV5 && c.br.receiveMax > 0 && len(perConn) >= int(c.br.receiveMax) {
					c.br.mu.Unlock()
					// 经队列回送 DISCONNECT 0x93：落在已入队 PUBREC 之后
					//（单写者 FIFO），关停清空保证上线。
					c.br.enqueue(c, &mqtt.Disconnect{V5: true, ReasonCode: mqtt.MQTTV5ReceiveMaxExceeded})
					return
				}
				perConn[p.PacketID] = &mqtt.Publish{Dup: p.Dup, QoS: 2, PacketID: p.PacketID, Topic: p.Topic, Payload: append([]byte(nil), p.Payload...)}
				c.br.mu.Unlock()
				// v0.27.0: record the parked message (soft-fail; the
				// protocol reply below must not depend on disk health).
				_ = brokerQoS2Save(persistDir, p.PacketID, p)
				c.br.enqueue(c, &mqtt.Pubrec{PacketID: p.PacketID, V5: c.connV5})
				continue
			}
			c.br.recordPublish(p)
			if p.QoS == 1 {
				// v0.30.0：v5 下无匹配订阅者的 QoS1 回 PUBACK 原因码 0x10
				//（警告级：消息已确认但无人消费）。v0.32.0：保留中的离线
				// 会话也计为订阅者（消息将暂存待其重连消费）。
				rc := byte(0)
				if c.connV5 && !c.br.hasSubscriber(p.Topic) && !c.br.hasOfflineInterest(p.Topic) {
					rc = mqtt.MQTTV5NoMatchingSubscribers
				}
				c.br.enqueue(c, &mqtt.Puback{PacketID: p.PacketID, V5: c.connV5, ReasonCode: rc})
			}
			c.br.fanoutBytes(c, p.Topic, p.Payload, p.QoS)
		case *mqtt.Pubrel:
			// Release leg: deliver exactly once, then PUBCOMP. Only this
			// connection's parked exchange can complete here; if no
			// per-connection park matches (e.g. the sender reconnected to a
			// restarted broker), fall back to the startup-loaded orphan
			// table (v0.27.0 restart recovery).
			c.br.mu.Lock()
			persistDir := c.br.persistDir
			perConn := c.br.pendingQoS2[c]
			parked := perConn[p.PacketID]
			delete(perConn, p.PacketID)
			if parked == nil && c.br.orphanQoS2 != nil {
				parked = c.br.orphanQoS2[p.PacketID]
				delete(c.br.orphanQoS2, p.PacketID)
			}
			c.br.mu.Unlock()
			if parked != nil {
				c.br.recordPublish(parked)
				c.br.fanoutBytes(c, parked.Topic, parked.Payload, parked.QoS)
			}
			c.br.enqueue(c, &mqtt.Pubcomp{PacketID: p.PacketID})
			// Exchange complete: drop the record (no-op when disabled).
			_ = brokerQoS2Remove(persistDir, p.PacketID)
		case *mqtt.Puback:
			// v0.32.0：下行 QoS1 确认——从本连接会话的恢复下发记录中
			// 移除（简化语义：无重发定时器，确认前不重推）；确认释放
			// 在途窗口后补发下一条（窗口 = recoverWindow，防 out 队列溢出）。
			if c.sess != nil {
				var nxt *mqtt.Publish
				found := false
				c.sess.mu.Lock()
				for i, m := range c.sess.offline {
					if m.PacketID == p.PacketID {
						c.sess.offline = append(c.sess.offline[:i], c.sess.offline[i+1:]...)
						found = true
						break
					}
				}
				// v0.33.0 防御：在途条可能被 offlineCap 丢最旧挤掉（未命中），
				// 窗口仍需释放，否则 dispatched 单调涨满后 QoS1 下行全积压。
				if c.sess.dispatched > 0 {
					c.sess.dispatched--
				}
				if c.sess.dispatched < len(c.sess.offline) {
					nxt = c.sess.offline[c.sess.dispatched]
					nxt.PacketID = c.br.nextPktID()
					c.sess.dispatched++
				}
				c.sess.mu.Unlock()
				_ = found
				if nxt != nil {
					c.br.enqueueQoS(c, nxt.Topic, nxt.Payload, 1, nxt.PacketID, 0)
				}
			}
		case *mqtt.Pingreq:
			c.br.mu.Lock()
			c.br.pingCount++
			c.br.mu.Unlock()
			c.br.enqueue(c, &mqtt.Pingresp{})
		case *mqtt.Disconnect:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// R-6 consolidation shims (v0.25.0).
//
// The local wire codec and matcher were removed; client, broker and tests
// now share pkg/mqtt's single implementation. One scoped exception remains
// for bad-client simulation: the frozen negative test
// (TestSubscribeSubackEchoAndInvalidFilter) puts a malformed SUBSCRIBE
// filter ("a/#/b") on the wire and expects the broker to parse it and
// reject per-filter with SUBACK 0x80 while keeping the connection open.
// The client-grade encoder/decoder refuses malformed filters by design
// (correct for real clients), so SUBSCRIBE packets in that scenario take
// a minimal permissive path here. simMatchTopic stays a pure forwarder
// (v0240_sim_test.go references it directly).
// ---------------------------------------------------------------------------

// simMatchTopic forwards to pkg/mqtt.MatchTopic (R-6: single matcher).
func simMatchTopic(filter, topic string) bool { return mqtt.MatchTopic(filter, topic) }

// encodePacket forwards to pkg/mqtt.EncodePacket (R-6: single wire codec),
// except for a SUBSCRIBE carrying a deliberately-invalid filter, which the
// client-grade encoder refuses by design; such packets take the permissive
// encoder so bad-client tests can still put them on the wire.
func encodePacket(w io.Writer, p mqtt.Packet) error {
	if s, ok := p.(*mqtt.Subscribe); ok && subscribeHasInvalidFilter(s) {
		return encodePermissiveSubscribe(w, s)
	}
	return mqtt.EncodePacket(w, p)
}

// decodePacket defers to pkg/mqtt.DecodePacket (R-6: single wire codec),
// except for SUBSCRIBE, which parses permissively so the broker can reject
// malformed filters per-filter (SUBACK 0x80) instead of tearing the
// connection down. The fixed-header byte is read first to pick the path;
// every non-SUBSCRIBE type is re-fed to the shared decoder verbatim via
// MultiReader, so its strictness is untouched.
func decodePacket(r io.Reader) (mqtt.Packet, error) {
	return decodePacketNegotiated(r, false)
}

// decodePacketNegotiated 是 v5 感知的 shim（v0.30.0）：非 SUBSCRIBE 报文
// 直通 mqtt.DecodePacketV（协商提示）；SUBSCRIBE 走宽容解析（v5 需跳过
// 属性长度字节）。
func decodePacketNegotiated(r io.Reader, v5 bool) (mqtt.Packet, error) {
	var h [1]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	if h[0]>>4 != mqtt.PacketTypeSUBSCRIBE {
		return mqtt.DecodePacketV(io.MultiReader(bytes.NewReader(h[:]), r), v5)
	}
	rl, err := readVarintBytes(r)
	if err != nil {
		return nil, err
	}
	body := make([]byte, rl)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return decodePermissiveSubscribe(body, v5)
}

// subscribeHasInvalidFilter reports whether any filter in s would be
// rejected by pkg/mqtt.ValidateTopicFilter.
func subscribeHasInvalidFilter(s *mqtt.Subscribe) bool {
	for _, tf := range s.Topics {
		if mqtt.ValidateTopicFilter(tf.Topic) != nil {
			return true
		}
	}
	return false
}

// encodePermissiveSubscribe serialises a SUBSCRIBE without filter
// validation (bad-client path only; wire format identical).
func encodePermissiveSubscribe(w io.Writer, s *mqtt.Subscribe) error {
	var body []byte
	body = append(body, byte(s.PacketID>>8), byte(s.PacketID))
	for _, tf := range s.Topics {
		body = append(body, byte(len(tf.Topic)>>8), byte(len(tf.Topic)))
		body = append(body, tf.Topic...)
		body = append(body, tf.QoS)
	}
	out := append([]byte{0x82}, appendVarintBytes(uint32(len(body)))...)
	out = append(out, body...)
	_, err := w.Write(out)
	return err
}

// decodePermissiveSubscribe parses a SUBSCRIBE body without filter
// validation (bad-client path only). QoS range and the at-least-one-filter
// rule are still enforced, mirroring the previous local codec.
// v0.30.0：v5=true 时跳过 packetID 后的属性区（阶段一：属性长度字节 +
// 跳过对应字节数，不解析属性语义）。
func decodePermissiveSubscribe(body []byte, v5 bool) (mqtt.Packet, error) {
	if len(body) < 3 {
		return nil, mqtt.ErrMalformed
	}
	s := &mqtt.Subscribe{PacketID: uint16(body[0])<<8 | uint16(body[1])}
	i := 2
	if v5 {
		if i >= len(body) {
			return nil, mqtt.ErrMalformed
		}
		propLen := int(body[i]) // 阶段一：VBI 首字节即总长（0 或 3，宽容路径不深解析）
		i++
		if i+propLen > len(body) {
			return nil, mqtt.ErrMalformed
		}
		i += propLen
	}
	for i < len(body) {
		if i+2 > len(body) {
			return nil, mqtt.ErrMalformed
		}
		n := int(uint16(body[i])<<8 | uint16(body[i+1]))
		i += 2
		if i+n > len(body) {
			return nil, mqtt.ErrMalformed
		}
		topic := string(body[i : i+n])
		i += n
		if i >= len(body) {
			return nil, mqtt.ErrMalformed
		}
		qos := body[i]
		i++
		noLocal, rap := false, false
		if v5 {
			// v0330：v5 订阅选项字节拆解（QoS 低 2 位；NoLocal/RAP 传入
			// TopicFilter；保留位/RH 超界拒绝）。v3.1.1 路径字节不变。
			if qos&0xC0 != 0 || qos>>4 > 2 {
				return nil, mqtt.ErrMalformed
			}
			noLocal = qos&0x04 != 0
			rap = qos&0x08 != 0
			qos &= 0x03
		}
		if qos > 2 {
			return nil, mqtt.ErrMalformed
		}
		s.Topics = append(s.Topics, mqtt.TopicFilter{Topic: topic, QoS: qos, NoLocal: noLocal, RetainAsPublished: rap})
	}
	return s, nil
}

// appendVarintBytes encodes an MQTT remaining-length varint (bad-client
// framing path only).
func appendVarintBytes(v uint32) []byte {
	var out []byte
	for {
		x := byte(v & 0x7F)
		v >>= 7
		if v > 0 {
			x |= 0x80
		}
		out = append(out, x)
		if v == 0 {
			return out
		}
	}
}

// readVarintBytes decodes an MQTT remaining-length varint (bad-client
// framing path only).
func readVarintBytes(r io.Reader) (uint32, error) {
	var v, mul uint32
	var b [1]byte
	for i := 0; i < 4; i++ {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		v |= uint32(b[0]&0x7F) << mul
		if b[0]&0x80 == 0 {
			return v, nil
		}
		mul += 7
	}
	return 0, mqtt.ErrMalformed
}
