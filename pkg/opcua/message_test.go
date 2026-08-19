package opcua

import (
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestHelloEncodeDecode(t *testing.T) {
	hello := Hello{
		ProtocolVersion:   0,
		ReceiveBufferSize: DefaultReceiveBufferSize,
		SendBufferSize:    DefaultSendBufferSize,
		MaxMessageSize:    DefaultMaxMessageSize,
		MaxChunkCount:     DefaultMaxChunkCount,
		EndpointUrl:       "opc.tcp://127.0.0.1:4840",
	}
	frame, err := hello.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(frame) != HeaderSize+4*5+4+len(hello.EndpointUrl) {
		t.Fatalf("frame size: got %d", len(frame))
	}
	h, err := DecodeHeader(frame[:HeaderSize])
	if err != nil {
		t.Fatalf("DecodeHeader: %v", err)
	}
	if h.MessageType != MsgHello || h.ChunkType != ChunkFinal {
		t.Fatalf("header: type=%q chunk=%q", h.MessageType, h.ChunkType)
	}
	if int(h.MessageSize) != len(frame) {
		t.Fatalf("MessageSize: got %d want %d", h.MessageSize, len(frame))
	}
	if h.ChannelId != 0 {
		t.Fatalf("ChannelId: got %d", h.ChannelId)
	}
	got, err := DecodeHello(frame[HeaderSize:])
	if err != nil {
		t.Fatalf("DecodeHello: %v", err)
	}
	if !reflect.DeepEqual(got, &hello) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, &hello)
	}
}

func TestHelloRequiresEndpoint(t *testing.T) {
	if _, err := (Hello{}).Encode(); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("empty endpoint: got %v", err)
	}
}

func TestAcknowledgeRoundTrip(t *testing.T) {
	ack := Acknowledge{
		ProtocolVersion:   0,
		ReceiveBufferSize: 65536,
		SendBufferSize:    65536,
		MaxMessageSize:    65536,
		MaxChunkCount:     1,
	}
	frame, err := ack.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(frame[0:3]) != MsgAcknowledge {
		t.Fatalf("type: %q", frame[0:3])
	}
	got, err := DecodeAcknowledge(frame[HeaderSize:])
	if err != nil {
		t.Fatalf("DecodeAcknowledge: %v", err)
	}
	if !reflect.DeepEqual(got, &ack) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, &ack)
	}
}

func TestErrorMessageRoundTrip(t *testing.T) {
	em := ErrorMessage{ErrorCode: StatusBadTimeout, ErrorReason: "simulated failure"}
	frame, err := em.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(frame[0:3]) != MsgError {
		t.Fatalf("type: %q", frame[0:3])
	}
	got, err := DecodeError(frame[HeaderSize:])
	if err != nil {
		t.Fatalf("DecodeError: %v", err)
	}
	if !reflect.DeepEqual(got, &em) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, &em)
	}
}

func TestEncodeHeaderErrors(t *testing.T) {
	cases := []MessageHeader{
		{MessageType: "XXX", ChunkType: ChunkFinal, MessageSize: HeaderSize},
		{MessageType: MsgSecureMessage, ChunkType: 'X', MessageSize: HeaderSize},
		{MessageType: MsgSecureMessage, ChunkType: ChunkFinal, MessageSize: HeaderSize - 1},
	}
	for _, h := range cases {
		if _, err := EncodeHeader(h); !errors.Is(err, ErrInvalidEncoding) {
			t.Fatalf("%+v: got %v, want ErrInvalidEncoding", h, err)
		}
	}
}

func TestDecodeHeaderErrors(t *testing.T) {
	// Short buffer.
	if _, err := DecodeHeader(make([]byte, HeaderSize-1)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short: got %v", err)
	}
	// Bad magic.
	hdr := make([]byte, HeaderSize)
	copy(hdr, "XXX")
	hdr[3] = ChunkFinal
	if _, err := DecodeHeader(hdr); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("bad magic: got %v", err)
	}
	// Bad chunk type.
	copy(hdr, MsgSecureMessage)
	hdr[3] = 'X'
	if _, err := DecodeHeader(hdr); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("bad chunk: got %v", err)
	}
	// MessageSize below header size.
	copy(hdr, MsgSecureMessage)
	hdr[3] = ChunkFinal
	hdr[7] = byte(HeaderSize - 1) // low byte of MessageSize
	if _, err := DecodeHeader(hdr); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("small size: got %v", err)
	}
}
