package mqtt

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClientClosed is returned by operations attempted after the client has
// been closed, or after the underlying connection has dropped.
var ErrClientClosed = errors.New("mqtt: client closed or disconnected")

// ackTimeout bounds how long Publish (QoS1) and Subscribe wait for the
// matching PUBACK/SUBACK from the broker.
const ackTimeout = 10 * time.Second

// Options configures Dial.
type Options struct {
	ClientID       string
	KeepAlive      time.Duration
	CleanSession   bool // reserved: v0.24.0 always requests a clean session
	Username       string
	Password       string
	ConnectTimeout time.Duration
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
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:        conn,
		handlers:    make(map[string][]Handler),
		pendingAcks: make(map[uint16]chan Packet),
		done:        make(chan struct{}),
	}

	keepSecs := int64(opts.KeepAlive / time.Second)
	if keepSecs > 65535 {
		keepSecs = 65535
	}
	ck := &Connect{
		ClientID:     opts.ClientID,
		KeepAlive:    uint16(keepSecs),
		CleanSession: true, // v0.24.0: always a clean session; no persistent-session support yet
		Username:     opts.Username,
		Password:     opts.Password,
		// Will is intentionally not set.
	}
	if err := c.write(ck); err != nil {
		conn.Close()
		return nil, fmt.Errorf("mqtt: send CONNECT: %w", err)
	}
	p, err := decodePacket(conn)
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
		return nil, fmt.Errorf("mqtt: connect refused, return code %d (0x%02X)", ca.ReturnCode, ca.ReturnCode)
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

// Subscribe sends a SUBSCRIBE for a single filter and waits for the matching
// SUBACK. On a granted code (< 0x80) the handler is registered under the
// exact filter string; on rejection the error carries the SUBACK code.
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
	c.handlersMu.Lock()
	c.handlers[topic] = append(c.handlers[topic], h)
	c.handlersMu.Unlock()
	return nil
}

// Publish sends a PUBLISH. QoS 0 is fire-and-forget; QoS 1 waits up to
// ackTimeout for the broker's PUBACK. QoS 2 is not supported.
func (c *Client) Publish(topic string, qos byte, payload []byte) error {
	if err := validateTopicName(topic); err != nil {
		return err
	}
	if qos > 1 {
		return fmt.Errorf("mqtt: unsupported QoS %d", qos)
	}
	if err := c.ensureOpen(); err != nil {
		return err
	}
	pk := &Publish{QoS: qos, Topic: topic, Payload: payload}
	if qos == 0 {
		return c.write(pk)
	}
	pk.PacketID = c.nextID()
	ch := c.registerAck(pk.PacketID)
	defer c.unregisterAck(pk.PacketID)
	if err := c.write(pk); err != nil {
		return err
	}
	ack, err := c.waitAck(ch)
	if err != nil {
		return err
	}
	if _, ok := ack.(*Puback); !ok {
		return fmt.Errorf("mqtt: expected PUBACK for packet id %d", pk.PacketID)
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
		p, err := decodePacket(c.conn)
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
				_ = c.write(&Puback{PacketID: pv.PacketID})
			}
			for _, h := range c.matchHandlers(pv.Topic) {
				h(pv.Topic, pv.Payload)
			}
		case *Puback:
			c.resolveAck(pv.PacketID, pv)
		case *Suback:
			c.resolveAck(pv.PacketID, pv)
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
