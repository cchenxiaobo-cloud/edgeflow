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

// newBrokerFromListener wires up the broker around an already-created
// listener and starts the accept loop.
func newBrokerFromListener(ln net.Listener) *Broker {
	b := &Broker{
		ln:      ln,
		clients: make(map[*simClient]struct{}),
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
				if mqtt.ValidateTopicFilter(tf.Topic) != nil {
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
	var h [1]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	if h[0]>>4 != mqtt.PacketTypeSUBSCRIBE {
		return mqtt.DecodePacket(io.MultiReader(bytes.NewReader(h[:]), r))
	}
	rl, err := readVarintBytes(r)
	if err != nil {
		return nil, err
	}
	body := make([]byte, rl)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return decodePermissiveSubscribe(body)
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
func decodePermissiveSubscribe(body []byte) (mqtt.Packet, error) {
	if len(body) < 3 {
		return nil, mqtt.ErrMalformed
	}
	s := &mqtt.Subscribe{PacketID: uint16(body[0])<<8 | uint16(body[1])}
	i := 2
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
		if qos > 2 {
			return nil, mqtt.ErrMalformed
		}
		s.Topics = append(s.Topics, mqtt.TopicFilter{Topic: topic, QoS: qos})
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
