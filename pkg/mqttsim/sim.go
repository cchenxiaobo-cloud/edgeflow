// Package mqttsim provides a minimal in-process MQTT 3.1.1 test broker for
// EdgeFlow integration tests. It builds on pkg/mqtt's exported packet TYPES
// and implements its own small wire codec and topic matcher.
//
// Boundary note: pkg/mqtt's codec functions (encodePacket/decodePacket/
// validateTopicFilter) landed unexported, so they are unreachable from
// another package, and this task's file scope forbids modifying pkg/mqtt/**.
// mqttsim therefore carries a minimal local codec (same wire format, built
// on mqtt's exported structs) plus ~30 lines of duplicated matching logic
// (simMatchTopic).
//
// It deliberately does NOT depend on the M2a worker pieces (pkg/mqtt
// match.go / client.go): those are developed in parallel and the build order
// between subagents cannot be relied upon.
package mqttsim

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"sync"

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
	closeOnce sync.Once
	closed    bool
}

// simClient is one accepted connection: its subscription filters and its
// outbound byte queue drained by a dedicated pump goroutine.
type simClient struct {
	br        *Broker
	conn      net.Conn
	mu        sync.Mutex
	filters   map[string]struct{}
	out       chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

// NewBroker starts the listener and the accept loop.
func NewBroker() (*Broker, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &Broker{
		ln:      ln,
		clients: make(map[*simClient]struct{}),
	}
	go b.acceptLoop()
	return b, nil
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
			br:      b,
			conn:    conn,
			filters: make(map[string]struct{}),
			out:     make(chan []byte, outQueueSize),
			done:    make(chan struct{}),
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
	var buf bytes.Buffer
	if err := encodePacket(&buf, &mqtt.Publish{Topic: topic, Payload: payload}); err != nil {
		return err
	}
	b.fanoutBytes(topic, buf.Bytes())
	return nil
}

// fanout distributes a client-originated publish to all matching subscribers
// (the sender included, if subscribed). Always re-encoded as QoS 0.
func (b *Broker) fanout(topic string, payload []byte) {
	var buf bytes.Buffer
	if err := encodePacket(&buf, &mqtt.Publish{Topic: topic, Payload: payload}); err != nil {
		return
	}
	b.fanoutBytes(topic, buf.Bytes())
}

func (b *Broker) fanoutBytes(topic string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.clients {
		if !c.matches(topic) {
			continue
		}
		select {
		case c.out <- data:
		default:
			b.dropCount++
		}
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

// pump drains the outbound queue onto the connection. It exits when the
// client is shut down or a write fails.
func (c *simClient) pump() {
	for {
		select {
		case buf := <-c.out:
			if _, err := c.conn.Write(buf); err != nil {
				c.shutdown()
				return
			}
		case <-c.done:
			return
		}
	}
}

// shutdown closes the connection exactly once and unregisters the client.
func (c *simClient) shutdown() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.conn.Close()
	})
	c.br.unregister(c)
}

// serve is the per-connection read loop. The first packet must be CONNECT;
// afterwards CONNECT/SUBSCRIBE/PUBLISH/PINGREQ/DISCONNECT are handled and
// any decode or read error tears the connection down.
func (c *simClient) serve() {
	defer c.shutdown()
	authed := false
	for {
		pkt, err := decodePacket(c.conn)
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
			c.br.enqueue(c, &mqtt.Connack{ReturnCode: 0})
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
				if !simValidFilter(tf.Topic) {
					codes[i] = subFailureCode
					continue
				}
				c.filters[tf.Topic] = struct{}{}
				codes[i] = tf.QoS
			}
			c.mu.Unlock()
			c.br.enqueue(c, &mqtt.Suback{PacketID: p.PacketID, Codes: codes})
		case *mqtt.Publish:
			c.br.recordPublish(p)
			if p.QoS == 1 {
				c.br.enqueue(c, &mqtt.Puback{PacketID: p.PacketID})
			}
			c.br.fanout(p.Topic, p.Payload)
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

// simMatchTopic reports whether an MQTT topic filter matches a topic name.
// Local re-implementation (M2a's MatchTopic is intentionally not imported;
// parallel development, see package comment). Supports '+' (single level)
// and '#' (remaining levels, last position only), per MQTT 3.1.1 minus the
// '$'-prefixed-topic exclusion, which a test broker need not enforce.
func simMatchTopic(filter, topic string) bool {
	if filter == "#" {
		return true
	}
	fl := strings.Split(filter, "/")
	tl := strings.Split(topic, "/")
	for i, f := range fl {
		switch {
		case f == "#":
			return true // trailing '#': matches the rest, including the parent level
		case i >= len(tl):
			return false
		case f == "+" || f == tl[i]:
			// wildcard or exact match on this level
		default:
			return false
		}
	}
	return len(fl) == len(tl)
}

// ---------------------------------------------------------------------------
// Local minimal MQTT 3.1.1 wire codec (encode side).
//
// pkg/mqtt's codec functions are unexported (see package comment); this
// subset serialises exactly the packet types the test broker needs, using
// mqtt's exported structs and the same wire format.
// ---------------------------------------------------------------------------

var errSimMalformed = errors.New("mqttsim: malformed packet")

func simBool(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// simAppendStr appends an MQTT UTF-8 string (u16 length prefix + bytes).
func simAppendStr(dst []byte, s string) []byte {
	dst = append(dst, byte(len(s)>>8), byte(len(s)))
	return append(dst, s...)
}

// simAppendVarint appends an MQTT remaining-length varint.
func simAppendVarint(dst []byte, v uint32) []byte {
	for {
		x := byte(v & 0x7F)
		v >>= 7
		if v > 0 {
			x |= 0x80
		}
		dst = append(dst, x)
		if v == 0 {
			return dst
		}
	}
}

// simValidFilter mirrors pkg/mqtt.validateTopicFilter rules locally.
func simValidFilter(filter string) bool {
	if filter == "" {
		return false
	}
	levels := strings.Split(filter, "/")
	for i, lvl := range levels {
		if lvl == "" {
			continue // empty level is legal
		}
		switch lvl[0] {
		case '#':
			if len(lvl) != 1 || i != len(levels)-1 {
				return false // '#' must be the entire last level
			}
		case '+':
			if len(lvl) != 1 {
				return false // '+' must occupy an entire level
			}
		default:
			for j := 0; j < len(lvl); j++ {
				if lvl[j] == 0 || lvl[j] == '#' || lvl[j] == '+' {
					return false
				}
			}
		}
	}
	return true
}

// encodePacket serialises one packet to w (QoS 0/1 subset).
func encodePacket(w io.Writer, p mqtt.Packet) error {
	var head, body []byte
	switch v := p.(type) {
	case *mqtt.Connect:
		head = []byte{0x10}
		body = simEncodeConnect(v)
	case *mqtt.Connack:
		head = []byte{0x20}
		body = []byte{simBool(v.SessionPresent), v.ReturnCode}
	case *mqtt.Publish:
		if v.QoS > 2 || v.Topic == "" {
			return errSimMalformed
		}
		if (v.QoS == 0 && v.PacketID != 0) || (v.QoS > 0 && v.PacketID == 0) {
			return errSimMalformed
		}
		b1 := byte(0x30) | (v.QoS << 1) | (simBool(v.Retain) << 0)
		if v.Dup != 0 {
			b1 |= 0x08
		}
		head = []byte{b1}
		body = simAppendStr(body, v.Topic)
		if v.QoS > 0 {
			body = append(body, byte(v.PacketID>>8), byte(v.PacketID))
		}
		body = append(body, v.Payload...)
	case *mqtt.Puback:
		head = []byte{0x40}
		body = []byte{byte(v.PacketID >> 8), byte(v.PacketID)}
	case *mqtt.Subscribe:
		if len(v.Topics) == 0 {
			return errSimMalformed
		}
		head = []byte{0x82} // SUBSCRIBE fixed-header flags are mandated as 0010
		body = append(body, byte(v.PacketID>>8), byte(v.PacketID))
		for _, tf := range v.Topics {
			if tf.QoS > 2 {
				return errSimMalformed
			}
			body = simAppendStr(body, tf.Topic)
			body = append(body, tf.QoS)
		}
	case *mqtt.Suback:
		head = []byte{0x90}
		body = append(body, byte(v.PacketID>>8), byte(v.PacketID))
		for _, c := range v.Codes {
			if c > 2 && c != 0x80 {
				return errSimMalformed
			}
			body = append(body, c)
		}
	case *mqtt.Pingreq:
		head = []byte{0xC0}
	case *mqtt.Pingresp:
		head = []byte{0xD0}
	case *mqtt.Disconnect:
		head = []byte{0xE0}
	default:
		return errSimMalformed
	}
	_, err := w.Write(append(simAppendVarint(head, uint32(len(body))), body...))
	return err
}

// simEncodeConnect writes the CONNECT variable header + payload with the
// protocol name/level fixed to MQTT/4 (3.1.1), mirroring the decoder.
func simEncodeConnect(c *mqtt.Connect) []byte {
	body := simAppendStr(nil, "MQTT")
	body = append(body, 0x04) // protocol level 4
	var flags byte
	if c.CleanSession {
		flags |= 0x02
	}
	if c.WillTopic != "" || c.WillMessage != "" {
		flags |= 0x04 | (c.WillQoS << 3) | (simBool(c.WillRetain) << 5)
	}
	if c.Username != "" {
		flags |= 0x80
	}
	if c.Password != "" {
		flags |= 0x40
	}
	body = append(body, flags, byte(c.KeepAlive>>8), byte(c.KeepAlive))
	body = simAppendStr(body, c.ClientID)
	if c.WillTopic != "" || c.WillMessage != "" {
		body = simAppendStr(body, c.WillTopic)
		body = simAppendStr(body, c.WillMessage)
	}
	if c.Username != "" {
		body = simAppendStr(body, c.Username)
	}
	if c.Password != "" {
		body = simAppendStr(body, c.Password)
	}
	return body
}

// decodePacket reads one packet from r (same subset as encodePacket).
// Any protocol violation yields an error; the caller treats that as fatal
// for the connection, mirroring the spec's "close on malformed" stance.
func decodePacket(r io.Reader) (mqtt.Packet, error) {
	var h [1]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	rl, err := simReadVarint(r)
	if err != nil {
		return nil, err
	}
	var body []byte
	if rl > 0 {
		body = make([]byte, rl)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
	}
	d := &simDecoder{b: body}
	switch h[0] >> 4 {
	case mqtt.PacketTypeCONNECT:
		if h[0]&0x0F != 0 {
			return nil, errSimMalformed
		}
		return d.connect()
	case mqtt.PacketTypeCONNACK:
		if h[0]&0x0F != 0 || len(body) != 2 {
			return nil, errSimMalformed
		}
		return &mqtt.Connack{SessionPresent: body[0]&0x01 == 1, ReturnCode: body[1]}, nil
	case mqtt.PacketTypePUBLISH:
		return d.publish(h[0] & 0x0F)
	case mqtt.PacketTypePUBACK:
		if h[0]&0x0F != 0 {
			return nil, errSimMalformed
		}
		id, err := d.u16()
		if err != nil || d.remaining() != 0 {
			return nil, errSimMalformed
		}
		return &mqtt.Puback{PacketID: id}, nil
	case mqtt.PacketTypeSUBSCRIBE:
		if h[0]&0x0F != 0x02 {
			return nil, errSimMalformed
		}
		return d.subscribe()
	case mqtt.PacketTypeSUBACK:
		if h[0]&0x0F != 0 || len(body) < 3 {
			return nil, errSimMalformed
		}
		id, err := d.u16()
		if err != nil {
			return nil, err
		}
		codes := d.rest()
		for _, c := range codes {
			if c > 2 && c != 0x80 {
				return nil, errSimMalformed
			}
		}
		return &mqtt.Suback{PacketID: id, Codes: codes}, nil
	case mqtt.PacketTypePINGREQ, mqtt.PacketTypePINGRESP, mqtt.PacketTypeDISCONNECT:
		if h[0]&0x0F != 0 || len(body) != 0 {
			return nil, errSimMalformed
		}
		switch h[0] >> 4 {
		case mqtt.PacketTypePINGREQ:
			return &mqtt.Pingreq{}, nil
		case mqtt.PacketTypePINGRESP:
			return &mqtt.Pingresp{}, nil
		default:
			return &mqtt.Disconnect{}, nil
		}
	default:
		return nil, errSimMalformed // reserved/unknown type
	}
}

func simReadVarint(r io.Reader) (uint32, error) {
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
	return 0, errSimMalformed // varint longer than 4 bytes
}

// simDecoder is a bounds-checked cursor over a packet body.
type simDecoder struct {
	b []byte
	i int
}

func (d *simDecoder) remaining() int { return len(d.b) - d.i }

func (d *simDecoder) byteVal() (byte, error) {
	if d.remaining() < 1 {
		return 0, errSimMalformed
	}
	v := d.b[d.i]
	d.i++
	return v, nil
}

func (d *simDecoder) u16() (uint16, error) {
	if d.remaining() < 2 {
		return 0, errSimMalformed
	}
	v := uint16(d.b[d.i])<<8 | uint16(d.b[d.i+1])
	d.i += 2
	return v, nil
}

func (d *simDecoder) str() (string, error) {
	n, err := d.u16()
	if err != nil {
		return "", err
	}
	if d.remaining() < int(n) {
		return "", errSimMalformed
	}
	s := string(d.b[d.i : d.i+int(n)])
	d.i += int(n)
	return s, nil
}

func (d *simDecoder) rest() []byte {
	out := d.b[d.i:]
	d.i = len(d.b)
	return out
}

func (d *simDecoder) connect() (mqtt.Packet, error) {
	name, err := d.str()
	if err != nil || name != "MQTT" {
		return nil, errSimMalformed
	}
	lvl, err := d.byteVal()
	if err != nil || lvl != 4 {
		return nil, errSimMalformed
	}
	flags, err := d.byteVal()
	if err != nil {
		return nil, err
	}
	if flags&0x01 != 0 {
		return nil, errSimMalformed // reserved connect-flags bit
	}
	ka, err := d.u16()
	if err != nil {
		return nil, err
	}
	c := &mqtt.Connect{KeepAlive: ka, CleanSession: flags&0x02 != 0}
	willQoS := (flags >> 3) & 3
	if c.ClientID, err = d.str(); err != nil {
		return nil, err
	}
	if flags&0x04 != 0 { // will flag
		if willQoS > 2 {
			return nil, errSimMalformed
		}
		if c.WillTopic, err = d.str(); err != nil {
			return nil, err
		}
		if c.WillMessage, err = d.str(); err != nil {
			return nil, err
		}
		c.WillQoS = willQoS
		c.WillRetain = flags&0x20 != 0
	} else if willQoS != 0 || flags&0x20 != 0 {
		return nil, errSimMalformed // willFlag=0 implies WillQoS=0, no retain
	}
	if flags&0x80 != 0 {
		if c.Username, err = d.str(); err != nil {
			return nil, err
		}
	}
	if flags&0x40 != 0 {
		if c.Password, err = d.str(); err != nil {
			return nil, err
		}
	}
	if d.remaining() != 0 {
		return nil, errSimMalformed // trailing bytes
	}
	return c, nil
}

func (d *simDecoder) publish(flags byte) (mqtt.Packet, error) {
	qos := (flags >> 1) & 3
	if qos == 3 {
		return nil, errSimMalformed
	}
	if qos == 0 && flags&0x08 != 0 {
		return nil, errSimMalformed // DUP must be 0 for QoS 0
	}
	topic, err := d.str()
	if err != nil {
		return nil, err
	}
	if topic == "" || strings.ContainsAny(topic, "+#") {
		return nil, errSimMalformed // wildcards illegal in a topic name
	}
	p := &mqtt.Publish{
		Dup:    (flags >> 3) & 1,
		QoS:    qos,
		Retain: flags&0x01 != 0,
		Topic:  topic,
	}
	if qos > 0 {
		if p.PacketID, err = d.u16(); err != nil {
			return nil, err
		}
	}
	p.Payload = d.rest()
	return p, nil
}

func (d *simDecoder) subscribe() (mqtt.Packet, error) {
	id, err := d.u16()
	if err != nil {
		return nil, err
	}
	s := &mqtt.Subscribe{PacketID: id}
	for d.remaining() > 0 {
		var tf mqtt.TopicFilter
		if tf.Topic, err = d.str(); err != nil {
			return nil, err
		}
		qos, err := d.byteVal()
		if err != nil || qos > 2 {
			return nil, errSimMalformed
		}
		tf.QoS = qos
		s.Topics = append(s.Topics, tf)
	}
	if len(s.Topics) == 0 {
		return nil, errSimMalformed
	}
	return s, nil
}
