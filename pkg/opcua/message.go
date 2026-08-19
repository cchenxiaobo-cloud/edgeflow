package opcua

import (
	"fmt"
	"io"
)

// Message type identifiers (first three bytes of the message header).
const (
	MsgHello              = "HEL"
	MsgAcknowledge        = "ACK"
	MsgError              = "ERR"
	MsgOpenSecureChannel  = "OPN"
	MsgSecureMessage      = "MSG"
	MsgCloseSecureChannel = "CLO"
)

// Chunk type byte (the fourth byte of the message header).
const (
	ChunkFinal        byte = 'F'
	ChunkIntermediate byte = 'C'
	ChunkAbort        byte = 'A'
)

// HeaderSize is the size in bytes of the UA Binary MessageHeader:
// 3 bytes message type + 1 byte chunk type + 4 bytes MessageSize +
// 4 bytes ChannelId (OPC UA Part 6 §6.7.2.2).
const HeaderSize = 12

// MessageHeader is the fixed 12-byte prefix of every UA Binary
// message. MessageSize covers the whole message, header included.
// ChannelId is 0 for HEL/ACK/ERR messages.
type MessageHeader struct {
	MessageType string // HEL / ACK / ERR / OPN / MSG / CLO
	ChunkType   byte   // 'F', 'C' or 'A'
	MessageSize uint32
	ChannelId   uint32
}

func validMessageType(t string) bool {
	switch t {
	case MsgHello, MsgAcknowledge, MsgError, MsgOpenSecureChannel, MsgSecureMessage, MsgCloseSecureChannel:
		return true
	}
	return false
}

func validChunkType(c byte) bool {
	return c == ChunkFinal || c == ChunkIntermediate || c == ChunkAbort
}

// EncodeHeader renders the header into its 12-byte wire form.
func EncodeHeader(h MessageHeader) ([]byte, error) {
	if !validMessageType(h.MessageType) {
		return nil, fmt.Errorf("%w: message type %q", ErrInvalidEncoding, h.MessageType)
	}
	if !validChunkType(h.ChunkType) {
		return nil, fmt.Errorf("%w: chunk type 0x%02X", ErrInvalidEncoding, h.ChunkType)
	}
	if h.MessageSize < HeaderSize {
		return nil, fmt.Errorf("%w: MessageSize %d < %d", ErrInvalidEncoding, h.MessageSize, HeaderSize)
	}
	var e encoder
	e.raw([]byte(h.MessageType))
	e.u8(h.ChunkType)
	e.u32(h.MessageSize)
	e.u32(h.ChannelId)
	return e.buf, nil
}

// DecodeHeader parses a 12-byte wire header and validates its magic
// bytes and sizes.
func DecodeHeader(b []byte) (MessageHeader, error) {
	if len(b) < HeaderSize {
		return MessageHeader{}, io.ErrUnexpectedEOF
	}
	h := MessageHeader{
		MessageType: string(b[0:3]),
		ChunkType:   b[3],
		MessageSize: uint32(b[4])<<24 | uint32(b[5])<<16 | uint32(b[6])<<8 | uint32(b[7]),
		ChannelId:   uint32(b[8])<<24 | uint32(b[9])<<16 | uint32(b[10])<<8 | uint32(b[11]),
	}
	if !validMessageType(h.MessageType) {
		return MessageHeader{}, fmt.Errorf("%w: message type %q", ErrInvalidEncoding, h.MessageType)
	}
	if !validChunkType(h.ChunkType) {
		return MessageHeader{}, fmt.Errorf("%w: chunk type 0x%02X", ErrInvalidEncoding, h.ChunkType)
	}
	if h.MessageSize < HeaderSize {
		return MessageHeader{}, fmt.Errorf("%w: MessageSize %d < %d", ErrInvalidEncoding, h.MessageSize, HeaderSize)
	}
	return h, nil
}

// Hello is the first message a client sends after connecting
// (OPC UA Part 6 §6.7.2.3). Its ChannelId is always 0.
type Hello struct {
	ProtocolVersion   uint32 // shall be 0 (OPC UA 1.0x)
	ReceiveBufferSize uint32 // largest message this side will receive
	SendBufferSize    uint32 // largest message this side will send
	MaxMessageSize    uint32 // largest message this side accepts (0 = unlimited)
	MaxChunkCount     uint32 // max chunks per message (0 = unlimited)
	EndpointUrl       string
}

// Encode returns the complete HELF frame (header + body).
func (h Hello) Encode() ([]byte, error) {
	if h.EndpointUrl == "" {
		return nil, fmt.Errorf("%w: Hello requires an endpoint URL", ErrInvalidEncoding)
	}
	var e encoder
	e.u32(h.ProtocolVersion)
	e.u32(h.ReceiveBufferSize)
	e.u32(h.SendBufferSize)
	e.u32(h.MaxMessageSize)
	e.u32(h.MaxChunkCount)
	e.str(h.EndpointUrl)
	return frameWithHeader(MsgHello, e.buf, 0)
}

// DecodeHello parses a Hello body (excluding the 12-byte header).
func DecodeHello(body []byte) (*Hello, error) {
	var d decoder
	d.b = body
	var h Hello
	var err error
	if h.ProtocolVersion, err = d.u32(); err != nil {
		return nil, err
	}
	if h.ReceiveBufferSize, err = d.u32(); err != nil {
		return nil, err
	}
	if h.SendBufferSize, err = d.u32(); err != nil {
		return nil, err
	}
	if h.MaxMessageSize, err = d.u32(); err != nil {
		return nil, err
	}
	if h.MaxChunkCount, err = d.u32(); err != nil {
		return nil, err
	}
	if h.EndpointUrl, err = d.str(); err != nil {
		return nil, err
	}
	return &h, nil
}

// Acknowledge is the server's reply to a Hello
// (OPC UA Part 6 §6.7.2.4).
type Acknowledge struct {
	ProtocolVersion   uint32
	ReceiveBufferSize uint32
	SendBufferSize    uint32
	MaxMessageSize    uint32
	MaxChunkCount     uint32
}

// Encode returns the complete ACKF frame.
func (a Acknowledge) Encode() ([]byte, error) {
	var e encoder
	e.u32(a.ProtocolVersion)
	e.u32(a.ReceiveBufferSize)
	e.u32(a.SendBufferSize)
	e.u32(a.MaxMessageSize)
	e.u32(a.MaxChunkCount)
	return frameWithHeader(MsgAcknowledge, e.buf, 0)
}

// DecodeAcknowledge parses an Acknowledge body.
func DecodeAcknowledge(body []byte) (*Acknowledge, error) {
	var d decoder
	d.b = body
	var a Acknowledge
	var err error
	if a.ProtocolVersion, err = d.u32(); err != nil {
		return nil, err
	}
	if a.ReceiveBufferSize, err = d.u32(); err != nil {
		return nil, err
	}
	if a.SendBufferSize, err = d.u32(); err != nil {
		return nil, err
	}
	if a.MaxMessageSize, err = d.u32(); err != nil {
		return nil, err
	}
	if a.MaxChunkCount, err = d.u32(); err != nil {
		return nil, err
	}
	return &a, nil
}

// ErrorMessage is the ERR message that reports a transport-level
// failure (OPC UA Part 6 §6.7.2.5).
type ErrorMessage struct {
	ErrorCode   StatusCode
	ErrorReason string
}

// Encode returns the complete ERRF frame.
func (m ErrorMessage) Encode() ([]byte, error) {
	var e encoder
	e.u32(uint32(m.ErrorCode))
	e.str(m.ErrorReason)
	return frameWithHeader(MsgError, e.buf, 0)
}

// DecodeError parses an Error body.
func DecodeError(body []byte) (*ErrorMessage, error) {
	var d decoder
	d.b = body
	var m ErrorMessage
	code, err := d.u32()
	if err != nil {
		return nil, err
	}
	m.ErrorCode = StatusCode(code)
	if m.ErrorReason, err = d.str(); err != nil {
		return nil, err
	}
	return &m, nil
}

// frameWithHeader assembles a full UA Binary frame from a message
// type, body and channel id.
func frameWithHeader(msgType string, body []byte, channelId uint32) ([]byte, error) {
	hdr, err := EncodeHeader(MessageHeader{
		MessageType: msgType,
		ChunkType:   ChunkFinal,
		MessageSize: uint32(HeaderSize + len(body)),
		ChannelId:   channelId,
	})
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, HeaderSize+len(body))
	out = append(out, hdr...)
	out = append(out, body...)
	return out, nil
}
