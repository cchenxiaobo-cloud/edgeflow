package opcua

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Default values sent in the client Hello. Buffer sizes and the
// maximum message size are kept consistent (single-chunk operation,
// MaxChunkCount = 1).
const (
	DefaultProtocolVersion   uint32 = 0
	DefaultReceiveBufferSize uint32 = 65536
	DefaultSendBufferSize    uint32 = 65536
	DefaultMaxMessageSize    uint32 = 65536
	DefaultMaxChunkCount     uint32 = 1
	// MinBufferSize is the smallest ReceiveBufferSize/SendBufferSize a
	// peer may advertise; smaller values abort the handshake
	// (OPC UA Part 6 §6.7.2.3/4).
	MinBufferSize uint32 = 8192
	// DefaultDialTimeout bounds the TCP connect and the Hello/Ack
	// handshake.
	DefaultDialTimeout = 10 * time.Second
)

// Transport errors.
var (
	// ErrMessageTooLarge is returned when a frame's MessageSize
	// exceeds the negotiated limit.
	ErrMessageTooLarge = errors.New("opcua: message exceeds maximum message size")
	// ErrChunkingUnsupported is returned for intermediate chunks ('C'):
	// this milestone is single-chunk only (MaxChunkCount = 1).
	ErrChunkingUnsupported = errors.New("opcua: intermediate chunks not supported")
)

// Conn is a SecurityPolicy None OPC UA TCP transport.
//
// Dial performs the connection handshake (TCP connect → Hello →
// Acknowledge) and returns a Conn ready for frame-level I/O.
// ReadMessage and WriteMessage exchange single-chunk frames; the
// SecureChannel layer (OPN/CLO) is a later milestone, so ChannelId is
// always 0.
type Conn struct {
	netConn net.Conn

	// Negotiated limits from the Hello/Acknowledge exchange
	// (0 = unlimited). sendLimit bounds outbound frame sizes,
	// recvLimit bounds inbound frame sizes.
	protocolVersion uint32
	sendLimit       uint32
	recvLimit       uint32
	channelId       uint32
}

// Dial connects to the given OPC UA endpoint (e.g.
// "opc.tcp://127.0.0.1:4840"), performs the Hello/Acknowledge
// handshake with SecurityPolicy None and returns a ready connection.
// The port defaults to 4840 when omitted.
func Dial(endpoint string) (*Conn, error) {
	return DialTimeout(endpoint, DefaultDialTimeout)
}

// DialTimeout is Dial with an explicit handshake timeout.
func DialTimeout(endpoint string, timeout time.Duration) (*Conn, error) {
	host, port, err := splitEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	nc, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
	if err != nil {
		return nil, fmt.Errorf("opcua: dial %s: %w", endpoint, err)
	}
	c := &Conn{netConn: nc, recvLimit: DefaultMaxMessageSize}
	if err := c.handshake(endpoint, timeout); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

// splitEndpoint parses "opc.tcp://host[:port][/path]" into host and
// port, defaulting the port to 4840.
func splitEndpoint(endpoint string) (host, port string, err error) {
	if !strings.HasPrefix(endpoint, "opc.tcp://") {
		return "", "", fmt.Errorf("opcua: unsupported endpoint %q (want opc.tcp://host:port)", endpoint)
	}
	rest := strings.TrimPrefix(endpoint, "opc.tcp://")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return "", "", fmt.Errorf("opcua: empty endpoint host in %q", endpoint)
	}
	host, port, err = net.SplitHostPort(rest)
	if err != nil {
		// No port given: treat the whole rest as the host.
		host, port, err = rest, "4840", nil
	}
	return host, port, err
}

// handshake sends the Hello and validates the Acknowledge (or
// translates an Error reply). The connection deadline covers the whole
// exchange and is cleared afterwards.
func (c *Conn) handshake(endpoint string, timeout time.Duration) error {
	if timeout > 0 {
		if err := c.netConn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		defer c.netConn.SetDeadline(time.Time{})
	}
	hello := Hello{
		ProtocolVersion:   DefaultProtocolVersion,
		ReceiveBufferSize: DefaultReceiveBufferSize,
		SendBufferSize:    DefaultSendBufferSize,
		MaxMessageSize:    DefaultMaxMessageSize,
		MaxChunkCount:     DefaultMaxChunkCount,
		EndpointUrl:       endpoint,
	}
	frame, err := hello.Encode()
	if err != nil {
		return err
	}
	if err := writeAll(c.netConn, frame); err != nil {
		return fmt.Errorf("opcua: send Hello: %w", err)
	}
	msgType, body, err := c.readFrame()
	if err != nil {
		return fmt.Errorf("opcua: read handshake reply: %w", err)
	}
	switch msgType {
	case MsgAcknowledge:
		ack, err := DecodeAcknowledge(body)
		if err != nil {
			return fmt.Errorf("opcua: decode Acknowledge: %w", err)
		}
		return c.applyAck(ack)
	case MsgError:
		em, err := DecodeError(body)
		if err != nil {
			return fmt.Errorf("opcua: decode Error: %w", err)
		}
		return fmt.Errorf("opcua: server rejected Hello: %s (%s)", em.ErrorReason, em.ErrorCode)
	default:
		return fmt.Errorf("opcua: unexpected handshake reply %q", msgType)
	}
}

// applyAck validates the server's Acknowledge and derives the
// negotiated message-size limits.
func (c *Conn) applyAck(a *Acknowledge) error {
	if a.ProtocolVersion != DefaultProtocolVersion {
		return fmt.Errorf("opcua: server protocol version %d, want %d", a.ProtocolVersion, DefaultProtocolVersion)
	}
	if a.ReceiveBufferSize < MinBufferSize || a.SendBufferSize < MinBufferSize {
		return fmt.Errorf("opcua: server buffer sizes too small (recv=%d send=%d, min=%d)",
			a.ReceiveBufferSize, a.SendBufferSize, MinBufferSize)
	}
	c.protocolVersion = a.ProtocolVersion
	// Outbound: we must respect the server's receive buffer and its
	// maximum accepted message size (0 = unlimited).
	c.sendLimit = a.ReceiveBufferSize
	if a.MaxMessageSize != 0 && a.MaxMessageSize < c.sendLimit {
		c.sendLimit = a.MaxMessageSize
	}
	// Inbound: the server must not exceed its own send buffer or our
	// advertised maximum message size.
	c.recvLimit = DefaultMaxMessageSize
	if a.SendBufferSize != 0 && a.SendBufferSize < c.recvLimit {
		c.recvLimit = a.SendBufferSize
	}
	return nil
}

// readFrame reads one complete frame: 12-byte header (validated),
// then the body. Intermediate chunks are rejected.
func (c *Conn) readFrame() (string, []byte, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(c.netConn, hdr[:]); err != nil {
		return "", nil, err
	}
	h, err := DecodeHeader(hdr[:])
	if err != nil {
		return "", nil, err
	}
	if h.ChunkType != ChunkFinal {
		return "", nil, ErrChunkingUnsupported
	}
	if c.recvLimit != 0 && h.MessageSize > c.recvLimit {
		return "", nil, fmt.Errorf("%w: MessageSize=%d limit=%d", ErrMessageTooLarge, h.MessageSize, c.recvLimit)
	}
	body := make([]byte, h.MessageSize-HeaderSize)
	if _, err := io.ReadFull(c.netConn, body); err != nil {
		return "", nil, err
	}
	return h.MessageType, body, nil
}

// WriteMessage writes a single-chunk frame with the given 3-letter
// message type (HEL/ACK/ERR/OPN/MSG/CLO) and payload.
func (c *Conn) WriteMessage(msgType string, payload []byte) error {
	size := uint64(HeaderSize) + uint64(len(payload))
	if c.sendLimit != 0 && size > uint64(c.sendLimit) {
		return fmt.Errorf("%w: %d bytes, limit=%d", ErrMessageTooLarge, size, c.sendLimit)
	}
	hdr, err := EncodeHeader(MessageHeader{
		MessageType: msgType,
		ChunkType:   ChunkFinal,
		MessageSize: uint32(size),
		ChannelId:   c.channelId,
	})
	if err != nil {
		return err
	}
	buf := make([]byte, 0, size)
	buf = append(buf, hdr...)
	buf = append(buf, payload...)
	return writeAll(c.netConn, buf)
}

// ReadMessage reads the next complete frame and returns its message
// type and payload. Frames whose MessageSize exceeds the negotiated
// limit are rejected before the body is read.
func (c *Conn) ReadMessage() (string, []byte, error) {
	return c.readFrame()
}

// Close closes the underlying TCP connection.
func (c *Conn) Close() error { return c.netConn.Close() }

// ChannelID returns the negotiated SecureChannel id. It is always 0
// until the SecureChannel milestone implements OPN/CLO.
func (c *Conn) ChannelID() uint32 { return c.channelId }

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
