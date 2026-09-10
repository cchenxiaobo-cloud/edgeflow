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

	// MQTT 5.0（v0.30.0，opt-in）：V5=true 时按级别 0x05 编码，并在
	// keepalive 后插入属性区。ReceiveMax>0 时携带 Receive Maximum
	// （0x21）属性；0 = 不携带（语义等同 65535 无限制）。3.1.1 路径
	// （V5=false）编码与历史版本逐字节一致。
	V5         bool
	ReceiveMax uint16
}

// Type implements Packet.
func (c *Connect) Type() byte { return PacketTypeCONNECT }

// Connack is the MQTT CONNACK packet.
type Connack struct {
	SessionPresent bool
	ReturnCode     byte

	// MQTT 5.0（v0.30.0）：V5=true 时 ReturnCode 字段承载 v5 原因码
	//（布局与 3.1.1 returnCode 同位），随后为属性区；ReceiveMax>0 时
	// 携带 Receive Maximum（0x21）属性。
	V5         bool
	ReceiveMax uint16
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

	// V5=true 时 payload 前插入属性长度字节 0x00（v5 帧形态，v0.30.0；
	// 阶段一不支持 PUBLISH 属性）。3.1.1 路径不受影响。
	V5 bool
}

// Type implements Packet.
func (p *Publish) Type() byte { return PacketTypePUBLISH }

// Puback is the MQTT PUBACK packet (QoS 1 acknowledgement).
type Puback struct {
	PacketID uint16

	// MQTT 5.0（v0.30.0）：V5=true 时 PacketID 后追加 1 字节原因码
	//（0x00 成功、0x10 无匹配订阅者警告、其他为失败语义）。
	V5         bool
	ReasonCode byte
}

// Type implements Packet.
func (p *Puback) Type() byte { return PacketTypePUBACK }

// Pubrec is the MQTT PUBREC packet (QoS 2, first acknowledgement).
type Pubrec struct {
	PacketID uint16

	V5         bool // v5：id 后追加原因码（v0.30.0）
	ReasonCode byte
}

// Type implements Packet.
func (p *Pubrec) Type() byte { return PacketTypePUBREC }

// Pubrel is the MQTT PUBREL packet (QoS 2, sender release).
type Pubrel struct {
	PacketID uint16

	V5         bool // v5：id 后追加原因码（v0.30.0）
	ReasonCode byte
}

// Type implements Packet.
func (p *Pubrel) Type() byte { return PacketTypePUBREL }

// Pubcomp is the MQTT PUBCOMP packet (QoS 2, final acknowledgement).
type Pubcomp struct {
	PacketID uint16

	V5         bool // v5：id 后追加原因码（v0.30.0）
	ReasonCode byte
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

	// V5=true 时 packetID 后插入属性长度字节（阶段一恒为 0x00，v0.30.0）。
	V5 bool
}

// Type implements Packet.
func (s *Subscribe) Type() byte { return PacketTypeSUBSCRIBE }

// Suback is the MQTT SUBACK packet.
type Suback struct {
	PacketID uint16
	Codes    []byte

	// V5=true 时 packetID 后插入属性长度字节（阶段一恒为 0x00）,
	// Codes 承载 v5 逐订阅原因码（0x00/0x01/0x02 授予、0x8F 等失败）
	//（v0.30.0）。
	V5 bool
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

// Disconnect is the MQTT DISCONNECT packet.
type Disconnect struct {
	// MQTT 5.0（v0.30.0）：V5=true 且 ReasonCode!=0 时编码 rc + 属性长度；
	// V5=true 且 ReasonCode==0 时仍为空体（v5 规范允许，rc 视为 0x00）。
	V5         bool
	ReasonCode byte
}

// Type implements Packet.
func (d *Disconnect) Type() byte { return PacketTypeDISCONNECT }

// Packet is any encodable MQTT control packet.
type Packet interface {
	Type() byte
	encodeUA(*encoder) error
}

// MQTT 5.0 原因码（v0.30.0 阶段一使用的子集；语义见 OASIS MQTT 5.0 规范）。
const (
	MQTTV5Success                byte = 0x00
	MQTTV5NoMatchingSubscribers  byte = 0x10 // PUBACK 警告级：无订阅者
	MQTTV5GrantedQoS0            byte = 0x00 // SUBACK 授予 QoS0
	MQTTV5GrantedQoS1            byte = 0x01
	MQTTV5GrantedQoS2            byte = 0x02
	MQTTV5MalformedPacket        byte = 0x81
	MQTTV5ProtocolError          byte = 0x82
	MQTTV5UnsupportedProtocolVer byte = 0x84
	MQTTV5ClientIDNotValid       byte = 0x85
	MQTTV5BadUserpass            byte = 0x86
	MQTTV5NotAuthorized          byte = 0x87
	MQTTV5ServerUnavailable      byte = 0x88
	MQTTV5TopicFilterInvalid     byte = 0x8F
	MQTTV5ReceiveMaxExceeded     byte = 0x93 // DISCONNECT：流控违规
)

// v5ReasonText 返回 v5 原因码的可读语义（未知码返回空串，调用方自行
// 退化为十六进制展示）。
func v5ReasonText(rc byte) string {
	switch rc {
	case MQTTV5Success:
		return "success"
	case MQTTV5NoMatchingSubscribers:
		return "no matching subscribers"
	case MQTTV5MalformedPacket:
		return "malformed packet"
	case MQTTV5ProtocolError:
		return "protocol error"
	case MQTTV5UnsupportedProtocolVer:
		return "unsupported protocol version"
	case MQTTV5ClientIDNotValid:
		return "client identifier not valid"
	case MQTTV5BadUserpass:
		return "bad user name or password"
	case MQTTV5NotAuthorized:
		return "not authorized"
	case MQTTV5ServerUnavailable:
		return "server unavailable"
	case MQTTV5TopicFilterInvalid:
		return "topic filter invalid"
	case MQTTV5ReceiveMaxExceeded:
		return "receive maximum exceeded"
	}
	return ""
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
