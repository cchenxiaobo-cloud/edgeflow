package opcua

import (
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// startMockServer starts a TCP server on 127.0.0.1:0; every accepted
// connection runs handle in its own goroutine. Returns the address.
func startMockServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(c)
		}
	}()
	return ln.Addr().String()
}

// serverReadFrame reads one full frame on the server side.
func serverReadFrame(t *testing.T, c net.Conn) (string, []byte) {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		t.Fatalf("server read header: %v", err)
	}
	h, err := DecodeHeader(hdr[:])
	if err != nil {
		t.Fatalf("server decode header: %v", err)
	}
	body := make([]byte, h.MessageSize-HeaderSize)
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatalf("server read body: %v", err)
	}
	return h.MessageType, body
}

func serverWriteFrame(t *testing.T, c net.Conn, msgType string, body []byte) {
	t.Helper()
	hdr, err := EncodeHeader(MessageHeader{
		MessageType: msgType,
		ChunkType:   ChunkFinal,
		MessageSize: uint32(HeaderSize + len(body)),
	})
	if err != nil {
		t.Fatalf("server header: %v", err)
	}
	frame := append(hdr, body...)
	if _, err := c.Write(frame); err != nil {
		t.Fatalf("server write: %v", err)
	}
}

func serverWriteAck(t *testing.T, c net.Conn) {
	t.Helper()
	frame, err := Acknowledge{
		ProtocolVersion:   0,
		ReceiveBufferSize: 65536,
		SendBufferSize:    65536,
		MaxMessageSize:    65536,
		MaxChunkCount:     1,
	}.Encode()
	if err != nil {
		t.Fatalf("ack encode: %v", err)
	}
	if _, err := c.Write(frame); err != nil {
		t.Fatalf("ack write: %v", err)
	}
}

func TestDialHandshakeAndFrames(t *testing.T) {
	var addr string
	addr = startMockServer(t, func(c net.Conn) {
		defer c.Close()
		mt, body := serverReadFrame(t, c)
		if mt != MsgHello {
			t.Errorf("server: first frame type %q, want HEL", mt)
			return
		}
		hello, err := DecodeHello(body)
		if err != nil {
			t.Errorf("server: DecodeHello: %v", err)
			return
		}
		if hello.ProtocolVersion != DefaultProtocolVersion {
			t.Errorf("server: ProtocolVersion=%d", hello.ProtocolVersion)
		}
		if hello.ReceiveBufferSize != DefaultReceiveBufferSize || hello.SendBufferSize != DefaultSendBufferSize {
			t.Errorf("server: buffers recv=%d send=%d", hello.ReceiveBufferSize, hello.SendBufferSize)
		}
		if hello.EndpointUrl != "opc.tcp://"+addr {
			t.Errorf("server: EndpointUrl=%q", hello.EndpointUrl)
		}
		serverWriteAck(t, c)
		// Echo one MSG frame back.
		mt, body = serverReadFrame(t, c)
		if mt != MsgSecureMessage {
			t.Errorf("server: frame type %q, want MSG", mt)
			return
		}
		serverWriteFrame(t, c, MsgSecureMessage, body)
	})

	conn, err := Dial("opc.tcp://" + addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if conn.protocolVersion != 0 {
		t.Errorf("negotiated protocol version %d", conn.protocolVersion)
	}
	if conn.sendLimit != 65536 || conn.recvLimit != 65536 {
		t.Errorf("limits: send=%d recv=%d", conn.sendLimit, conn.recvLimit)
	}
	payload := []byte{0x01, 0x02, 0x03, 0x04}
	if err := conn.WriteMessage(MsgSecureMessage, payload); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	mt, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if mt != MsgSecureMessage || !reflect.DeepEqual(got, payload) {
		t.Fatalf("echo mismatch: type=%q payload=%X", mt, got)
	}
}

func TestDialServerErrorFrame(t *testing.T) {
	addr := startMockServer(t, func(c net.Conn) {
		defer c.Close()
		serverReadFrame(t, c)
		frame, err := ErrorMessage{ErrorCode: StatusBadTimeout, ErrorReason: "simulated failure"}.Encode()
		if err != nil {
			t.Errorf("encode: %v", err)
			return
		}
		c.Write(frame)
	})
	_, err := Dial("opc.tcp://" + addr)
	if err == nil {
		t.Fatal("Dial succeeded, want error")
	}
	if !strings.Contains(err.Error(), "simulated failure") || !strings.Contains(err.Error(), "BadTimeout") {
		t.Fatalf("error message: %v", err)
	}
}

func TestDialUnexpectedReply(t *testing.T) {
	addr := startMockServer(t, func(c net.Conn) {
		defer c.Close()
		serverReadFrame(t, c)
		serverWriteFrame(t, c, MsgSecureMessage, []byte{1})
	})
	if _, err := Dial("opc.tcp://" + addr); err == nil || !strings.Contains(err.Error(), "unexpected handshake reply") {
		t.Fatalf("Dial: got %v", err)
	}
}

func TestDialClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if _, err := Dial("opc.tcp://" + addr); err == nil {
		t.Fatal("Dial to closed port succeeded")
	}
}

func TestDialBadEndpoint(t *testing.T) {
	for _, ep := range []string{"", "http://x:1", "opc.tcp://"} {
		if _, err := Dial(ep); err == nil {
			t.Fatalf("Dial(%q) succeeded", ep)
		}
	}
}

func TestReadMessageBadMagic(t *testing.T) {
	addr := startMockServer(t, func(c net.Conn) {
		defer c.Close()
		serverReadFrame(t, c)
		serverWriteAck(t, c)
		// Garbage header: unknown message type.
		var hdr [HeaderSize]byte
		copy(hdr[:], "XXX")
		hdr[3] = ChunkFinal
		hdr[4] = 0
		hdr[5] = 0
		hdr[6] = 0
		hdr[7] = HeaderSize
		c.Write(hdr[:])
	})
	conn, err := Dial("opc.tcp://" + addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("bad magic: got %v", err)
	}
}

func TestReadMessageShortPacket(t *testing.T) {
	addr := startMockServer(t, func(c net.Conn) {
		defer c.Close()
		serverReadFrame(t, c)
		serverWriteAck(t, c)
		c.Write([]byte{0x4D, 0x53, 0x47}) // 3 bytes then close
	})
	conn, err := Dial("opc.tcp://" + addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short packet: got %v", err)
	}
}

func TestReadMessageTooLarge(t *testing.T) {
	addr := startMockServer(t, func(c net.Conn) {
		defer c.Close()
		serverReadFrame(t, c)
		serverWriteAck(t, c)
		var hdr [HeaderSize]byte
		copy(hdr[:], MsgSecureMessage)
		hdr[3] = ChunkFinal
		hdr[4] = 0x7F // MessageSize = 0x7FFFFFFF
		hdr[5] = 0xFF
		hdr[6] = 0xFF
		hdr[7] = 0xFF
		c.Write(hdr[:])
	})
	conn, err := Dial("opc.tcp://" + addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("overflow: got %v", err)
	}
}

func TestReadMessageIntermediateChunk(t *testing.T) {
	addr := startMockServer(t, func(c net.Conn) {
		defer c.Close()
		serverReadFrame(t, c)
		serverWriteAck(t, c)
		var hdr [HeaderSize]byte
		copy(hdr[:], MsgSecureMessage)
		hdr[3] = ChunkIntermediate
		hdr[7] = HeaderSize
		c.Write(hdr[:])
	})
	conn, err := Dial("opc.tcp://" + addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); !errors.Is(err, ErrChunkingUnsupported) {
		t.Fatalf("chunk: got %v", err)
	}
}

func TestWriteMessageTooLarge(t *testing.T) {
	conn := &Conn{netConn: &net.TCPConn{}, sendLimit: DefaultMaxMessageSize}
	err := conn.WriteMessage(MsgSecureMessage, make([]byte, DefaultMaxMessageSize))
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversized write: got %v", err)
	}
	// Header validation also applies.
	err = conn.WriteMessage("XXX", []byte{1})
	if !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("bad type: got %v", err)
	}
}

func TestSplitEndpoint(t *testing.T) {
	cases := []struct {
		endpoint string
		host     string
		port     string
		wantErr  bool
	}{
		{"opc.tcp://127.0.0.1:4840", "127.0.0.1", "4840", false},
		{"opc.tcp://localhost", "localhost", "4840", false},
		{"opc.tcp://host:1234/path/ignored", "host", "1234", false},
		{"opc.tcp://[::1]:4840", "::1", "4840", false},
		{"http://x:1", "", "", true},
		{"opc.tcp://", "", "", true},
	}
	for _, tc := range cases {
		host, port, err := splitEndpoint(tc.endpoint)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected error", tc.endpoint)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.endpoint, err)
			continue
		}
		if host != tc.host || port != tc.port {
			t.Errorf("%q: got %s:%s want %s:%s", tc.endpoint, host, port, tc.host, tc.port)
		}
	}
}
