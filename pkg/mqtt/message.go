package mqtt

import (
	"encoding/binary"
	"io"
)

// MQTT 3.1.1 packet types (high nibble of the fixed-header byte).
const (
	PacketTypeCONNECT    byte = 1
	PacketTypeCONNACK    byte = 2
	PacketTypePUBLISH    byte = 3
	PacketTypePUBACK     byte = 4
	PacketTypePUBREC     byte = 5
	PacketTypePUBREL     byte = 6
	PacketTypePUBCOMP    byte = 7
	PacketTypeSUBSCRIBE  byte = 8
	PacketTypeSUBACK     byte = 9
	PacketTypePINGREQ    byte = 12
	PacketTypePINGRESP   byte = 13
	PacketTypeDISCONNECT byte = 14
)

// MaxRemainingLength is the maximum remaining-length value encodable as an
// MQTT varint (4 bytes: 0xFF 0xFF 0xFF 0x7F).
const MaxRemainingLength = 268435455

// Connect is the MQTT CONNECT packet.
type Connect struct {
	ClientID     string
	KeepAlive    uint16
	CleanSession bool
	Username     string
	Password     string
	WillTopic    string
	WillMessage  string
	WillQoS      byte
	WillRetain   bool
}

// Type implements Packet.
func (c *Connect) Type() byte { return PacketTypeCONNECT }

// Connack is the MQTT CONNACK packet.
type Connack struct {
	SessionPresent bool
	ReturnCode     byte
}

// Type implements Packet.
func (c *Connack) Type() byte { return PacketTypeCONNACK }

// Publish is the MQTT PUBLISH packet.
type Publish struct {
	Dup      byte // 0 or 1
	QoS      byte // 0, 1 or 2
	Retain   bool
	Topic    string
	PacketID uint16
	Payload  []byte
}

// Type implements Packet.
func (p *Publish) Type() byte { return PacketTypePUBLISH }

// Puback is the MQTT PUBACK packet (QoS 1 acknowledgement).
type Puback struct {
	PacketID uint16
}

// Type implements Packet.
func (p *Puback) Type() byte { return PacketTypePUBACK }

// Pubrec is the MQTT PUBREC packet (QoS 2, first acknowledgement).
type Pubrec struct {
	PacketID uint16
}

// Type implements Packet.
func (p *Pubrec) Type() byte { return PacketTypePUBREC }

// Pubrel is the MQTT PUBREL packet (QoS 2, sender release).
type Pubrel struct {
	PacketID uint16
}

// Type implements Packet.
func (p *Pubrel) Type() byte { return PacketTypePUBREL }

// Pubcomp is the MQTT PUBCOMP packet (QoS 2, final acknowledgement).
type Pubcomp struct {
	PacketID uint16
}

// Type implements Packet.
func (p *Pubcomp) Type() byte { return PacketTypePUBCOMP }

// TopicFilter is one subscription entry of a SUBSCRIBE packet.
type TopicFilter struct {
	Topic string
	QoS   byte
}

// Subscribe is the MQTT SUBSCRIBE packet.
type Subscribe struct {
	PacketID uint16
	Topics   []TopicFilter
}

// Type implements Packet.
func (s *Subscribe) Type() byte { return PacketTypeSUBSCRIBE }

// Suback is the MQTT SUBACK packet.
type Suback struct {
	PacketID uint16
	Codes    []byte
}

// Type implements Packet.
func (s *Suback) Type() byte { return PacketTypeSUBACK }

// Pingreq is the MQTT PINGREQ packet (empty body).
type Pingreq struct{}

// Type implements Packet.
func (p *Pingreq) Type() byte { return PacketTypePINGREQ }

// Pingresp is the MQTT PINGRESP packet (empty body).
type Pingresp struct{}

// Type implements Packet.
func (p *Pingresp) Type() byte { return PacketTypePINGRESP }

// Disconnect is the MQTT DISCONNECT packet (empty body).
type Disconnect struct{}

// Type implements Packet.
func (d *Disconnect) Type() byte { return PacketTypeDISCONNECT }

// Packet is any encodable MQTT control packet.
type Packet interface {
	Type() byte
	encodeUA(*encoder) error
}

// encoder is a minimal buffered writer used to assemble packet bodies.
type encoder struct {
	w   io.Writer
	err error
}

func (e *encoder) writeByte(b byte) {
	if e.err != nil {
		return
	}
	_, e.err = e.w.Write([]byte{b})
}

func (e *encoder) writeBytes(p []byte) {
	if e.err != nil {
		return
	}
	_, e.err = e.w.Write(p)
}

// writeString writes a UTF-8 string prefixed with its 16-bit byte length.
func (e *encoder) writeString(s string) {
	if e.err != nil {
		return
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(s)))
	_, e.err = e.w.Write(hdr[:])
	if e.err != nil {
		return
	}
	_, e.err = e.w.Write([]byte(s))
}
