package mqtt

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// Remaining-length varint.
// ---------------------------------------------------------------------------

// encodeVarint encodes v using the MQTT variable-length integer scheme.
func encodeVarint(v uint32) []byte {
	var out []byte
	for {
		b := byte(v % 128)
		v /= 128
		if v > 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}

// decodeVarint reads a remaining-length varint from r. More than 4 bytes or a
// value above MaxRemainingLength yields ErrMalformedVarint.
func decodeVarint(r io.Reader) (uint32, error) {
	var value uint32
	var multiplier uint32 = 1
	var buf [1]byte
	for i := 0; i < 4; i++ {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, ErrMalformedVarint
		}
		value += uint32(buf[0]&0x7f) * multiplier
		if buf[0]&0x80 == 0 {
			if value > MaxRemainingLength {
				return 0, ErrMalformedVarint
			}
			return value, nil
		}
		multiplier *= 128
	}
	// Fifth byte or continuation past 4 bytes: malformed.
	return 0, ErrMalformedVarint
}

// ---------------------------------------------------------------------------
// Topic validation.
// ---------------------------------------------------------------------------

// validateTopicName checks a PUBLISH topic: non-empty, no U+0000, no wildcards.
func validateTopicName(name string) error {
	if name == "" {
		return ErrMalformedTopic
	}
	for _, r := range name {
		if r == 0 || r == '#' || r == '+' {
			return ErrMalformedTopic
		}
	}
	return nil
}

// validateTopicFilter checks a SUBSCRIBE filter: non-empty, no U+0000; '#'
// must be the entire last level, '+' an entire level. Empty levels are allowed
// (e.g. "/a" and "a/").
func validateTopicFilter(filter string) error {
	if filter == "" {
		return ErrMalformedTopic
	}
	levels := strings.Split(filter, "/")
	for i, lvl := range levels {
		if lvl == "" {
			continue // empty level is legal
		}
		switch lvl[0] {
		case '#':
			if len(lvl) != 1 || i != len(levels)-1 {
				return ErrMalformedTopic // '#' must be the entire last level
			}
		case '+':
			if len(lvl) != 1 {
				return ErrMalformedTopic // '+' must occupy an entire level
			}
		default:
			for _, r := range lvl {
				if r == 0 || r == '#' || r == '+' {
					return ErrMalformedTopic
				}
			}
		}
	}
	return nil
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func writeU16(e *encoder, v uint16) {
	e.writeByte(byte(v >> 8))
	e.writeByte(byte(v))
}

// ---------------------------------------------------------------------------
// MQTT 5.0 属性区（v0.30.0 阶段一：仅 Receive Maximum 0x21）。
// 属性区仅存在于 V5=true 的报文；3.1.1 路径不触碰（冻结保证）。
// ---------------------------------------------------------------------------

// encodeConnectProps 编码 v5 属性区（v0.32.0 阶段二白名单：Session
// Expiry 0x11 + Receive Maximum 0x21；固定按 ID 升序）。全空 → 单字节 0x00。
func encodeConnectProps(e *encoder, receiveMax uint16, sessionExpiry uint32) {
	var body []byte
	if sessionExpiry > 0 {
		body = append(body, 0x11)
		body = append(body, byte(sessionExpiry>>24), byte(sessionExpiry>>16), byte(sessionExpiry>>8), byte(sessionExpiry))
	}
	if receiveMax > 0 {
		body = append(body, 0x21)
		body = append(body, byte(receiveMax>>8), byte(receiveMax))
	}
	if len(body) == 0 {
		e.writeByte(0x00) // 空属性区
		return
	}
	e.writeVBI(uint32(len(body)))
	e.writeBytes(body)
}

// propsV5 是 v5 属性区白名单解析结果（v0.32.0 阶段二扩展）。
type propsV5 struct {
	ReceiveMax    uint16 // 0x21（0 = 未携带）
	SessionExpiry uint32 // 0x11（0 = 未携带）
}

// decodeProps 解析 v5 属性区：白名单内属性任意组合与出现顺序（每条
// = ID 1B + 定长值），未知属性拒绝（阶段一语义延续，spec 0005 as-built）。
func decodeProps(d *decoder) (propsV5, error) {
	var out propsV5
	propLen, err := d.readVBI()
	if err != nil {
		return out, err
	}
	if propLen == 0 {
		return out, nil
	}
	end := d.consumed() + int(propLen) // 属性区边界（长度自洽校验）
	for d.consumed() < end {
		id, err := d.readByte()
		if err != nil {
			return out, err
		}
		switch id {
		case 0x21: // Receive Maximum (u16)
			if out.ReceiveMax, err = d.readUint16(); err != nil {
				return out, err
			}
		case 0x11: // Session Expiry Interval (u32)
			if out.SessionExpiry, err = d.readUint32(); err != nil {
				return out, err
			}
		default:
			return out, ErrMalformed // 白名单外属性拒绝
		}
	}
	if d.consumed() != end {
		return out, ErrMalformed // 属性值越界（长度与内容不符）
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Encoding.
// ---------------------------------------------------------------------------

// fixedByte1 computes the fixed-header first byte for p.
func fixedByte1(p Packet) (byte, error) {
	base := p.Type() << 4
	switch pkt := p.(type) {
	case *Publish:
		if pkt.QoS > 2 {
			return 0, ErrMalformed // panic-defence: reject QoS 3 instead of panicking
		}
		return base | (pkt.Dup&1)<<3 | pkt.QoS<<1 | boolByte(pkt.Retain), nil
	case *Subscribe:
		return base | 0x02, nil // fixed flags 0b0010
	case *Pubrel:
		return base | 0x02, nil // PUBREL fixed flags 0b0010 (MQTT 3.1.1 §2.3.1)
	case *Connect, *Connack, *Puback, *Pubrec, *Pubcomp, *Suback, *Pingreq, *Pingresp, *Disconnect:
		return base, nil
	default:
		return 0, ErrMalformed
	}
}

// encodePacket serialises p to w: fixed header, varint remaining length, body.
func encodePacket(w io.Writer, p Packet) error {
	byte1, err := fixedByte1(p)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	enc := &encoder{w: &body}
	if err := p.encodeUA(enc); err != nil {
		return err
	}
	if enc.err != nil {
		return enc.err
	}
	if body.Len() > MaxRemainingLength {
		return ErrMalformed
	}
	var hdr bytes.Buffer
	hdr.WriteByte(byte1)
	hdr.Write(encodeVarint(uint32(body.Len())))
	if _, err := w.Write(hdr.Bytes()); err != nil {
		return err
	}
	_, err = w.Write(body.Bytes())
	return err
}

// validateConnect enforces the CONNECT semantic constraints shared by the
// encoder and the decoder.
func validateConnect(c *Connect) error {
	if c.ClientID == "" {
		return ErrMalformedConnect
	}
	if c.WillQoS > 2 {
		return ErrMalformedConnect
	}
	willFlag := c.WillTopic != "" || c.WillMessage != ""
	if !willFlag {
		if c.WillQoS != 0 || c.WillRetain {
			return ErrMalformedConnect // willFlag=0 implies WillQoS=0 and no Will fields
		}
	} else if err := validateTopicName(c.WillTopic); err != nil {
		return ErrMalformedConnect
	}
	return nil
}

func (c *Connect) encodeUA(e *encoder) error {
	if err := validateConnect(c); err != nil {
		return err
	}
	var f byte
	if c.CleanSession {
		f |= 0x02
	}
	willFlag := c.WillTopic != "" || c.WillMessage != ""
	if willFlag {
		f |= 0x04
	}
	f |= (c.WillQoS & 0x03) << 3
	if c.WillRetain {
		f |= 0x20
	}
	if c.Password != "" {
		f |= 0x40
	}
	if c.Username != "" {
		f |= 0x80
	}
	e.writeString("MQTT") // protocol name
	if c.V5 {
		e.writeByte(5) // protocol level 5 (MQTT 5.0, v0.30.0)
	} else {
		e.writeByte(4) // protocol level 4 (MQTT 3.1.1) — 冻结路径
	}
	e.writeByte(f)
	writeU16(e, c.KeepAlive)
	if c.V5 {
		encodeConnectProps(e, c.ReceiveMax, c.SessionExpiry) // v5：属性区在 keepalive 后、payload 前
	}
	e.writeString(c.ClientID)
	if willFlag {
		e.writeString(c.WillTopic)
		e.writeString(c.WillMessage)
	}
	if c.Username != "" {
		e.writeString(c.Username)
	}
	if c.Password != "" {
		e.writeString(c.Password)
	}
	return nil
}

func (c *Connack) encodeUA(e *encoder) error {
	e.writeByte(boolByte(c.SessionPresent))
	e.writeByte(c.ReturnCode)
	if c.V5 {
		encodeConnectProps(e, c.ReceiveMax, c.SessionExpiry) // v0.32.0：与 CONNECT 同一白名单编码器
	}
	return nil
}

func (p *Publish) encodeUA(e *encoder) error {
	if p.QoS > 2 {
		return ErrMalformed
	}
	if err := validateTopicName(p.Topic); err != nil {
		return err
	}
	e.writeString(p.Topic)
	if p.QoS == 0 && p.PacketID != 0 {
		return ErrMalformed // QoS0 must not carry a packet id
	}
	if p.QoS > 0 {
		if p.PacketID == 0 {
			return ErrMalformed // QoS 1/2 must carry a packet id
		}
		writeU16(e, p.PacketID)
	}
	if p.V5 {
		e.writeByte(0x00) // v5 PUBLISH 属性长度恒 0（阶段一无 PUBLISH 属性）
	}
	e.writeBytes(p.Payload)
	return nil
}

func (p *Puback) encodeUA(e *encoder) error {
	writeU16(e, p.PacketID)
	if p.V5 {
		e.writeByte(p.ReasonCode) // v5：追加原因码（规范允许省略，本仓恒携带）
	}
	return nil
}

func (p *Pubrec) encodeUA(e *encoder) error {
	writeU16(e, p.PacketID)
	if p.V5 {
		e.writeByte(p.ReasonCode)
	}
	return nil
}

func (p *Pubrel) encodeUA(e *encoder) error {
	writeU16(e, p.PacketID)
	if p.V5 {
		e.writeByte(p.ReasonCode)
	}
	return nil
}

func (p *Pubcomp) encodeUA(e *encoder) error {
	writeU16(e, p.PacketID)
	if p.V5 {
		e.writeByte(p.ReasonCode)
	}
	return nil
}

func (s *Subscribe) encodeUA(e *encoder) error {
	if len(s.Topics) == 0 {
		return ErrMalformed
	}
	writeU16(e, s.PacketID)
	if s.V5 {
		e.writeByte(0x00) // v5 属性长度恒 0（阶段一无 SUBSCRIBE 属性）
	}
	for _, tf := range s.Topics {
		if err := validateTopicFilter(tf.Topic); err != nil {
			return err
		}
		if tf.QoS > 2 {
			return ErrMalformed
		}
		e.writeString(tf.Topic)
		e.writeByte(tf.QoS)
	}
	return nil
}

func (s *Suback) encodeUA(e *encoder) error {
	if len(s.Codes) == 0 {
		return ErrMalformed
	}
	if !s.V5 {
		for _, c := range s.Codes {
			switch c {
			case 0, 1, 2, 0x80:
			default:
				return ErrMalformed
			}
		}
	}
	writeU16(e, s.PacketID)
	if s.V5 {
		e.writeByte(0x00) // v5 属性长度恒 0
	}
	for _, c := range s.Codes {
		e.writeByte(c)
	}
	return nil
}

func (p *Pingreq) encodeUA(e *encoder) error { return nil }

func (p *Pingresp) encodeUA(e *encoder) error { return nil }

func (d *Disconnect) encodeUA(e *encoder) error {
	if d.V5 && d.ReasonCode != 0 {
		e.writeByte(d.ReasonCode)
		e.writeByte(0x00) // v5 属性长度恒 0
	}
	return nil // v5 rc=0 与 3.1.1 均为空体
}

// ---------------------------------------------------------------------------
// Decoding.
// ---------------------------------------------------------------------------

// decoder walks the fixed-size body slice of a packet.
type decoder struct {
	b   []byte
	pos int
}

func (d *decoder) remaining() int { return len(d.b) - d.pos }

// consumed reports the number of bytes already consumed（v0.32.0 属性区
// 长度自洽校验需要）。
func (d *decoder) consumed() int { return d.pos }

// readUint32 reads a big-endian uint32（v0.32.0 Session Expiry 属性）。
func (d *decoder) readUint32() (uint32, error) {
	if d.remaining() < 4 {
		return 0, ErrMalformed
	}
	v := uint32(d.b[d.pos])<<24 | uint32(d.b[d.pos+1])<<16 | uint32(d.b[d.pos+2])<<8 | uint32(d.b[d.pos+3])
	d.pos += 4
	return v, nil
}

func (d *decoder) readByte() (byte, error) {
	if d.remaining() < 1 {
		return 0, ErrShortBody
	}
	v := d.b[d.pos]
	d.pos++
	return v, nil
}

func (d *decoder) readUint16() (uint16, error) {
	if d.remaining() < 2 {
		return 0, ErrShortBody
	}
	v := binary.BigEndian.Uint16(d.b[d.pos : d.pos+2])
	d.pos += 2
	return v, nil
}

// readString reads a u16-length-prefixed UTF-8 string.
func (d *decoder) readString() (string, error) {
	n, err := d.readUint16()
	if err != nil {
		return "", err
	}
	if d.remaining() < int(n) {
		return "", ErrMalformedString
	}
	s := string(d.b[d.pos : d.pos+int(n)])
	d.pos += int(n)
	return s, nil
}

// readVBI 从报文体读取一个 MQTT 变长整数（属性长度用，v0.30.0）。
// 超 4 字节或续传位溢出为 ErrMalformed。
func (d *decoder) readVBI() (uint32, error) {
	var value uint32
	var multiplier uint32 = 1
	for i := 0; i < 4; i++ {
		b, err := d.readByte()
		if err != nil {
			return 0, ErrMalformed
		}
		value += uint32(b&0x7f) * multiplier
		if b&0x80 == 0 {
			return value, nil
		}
		multiplier *= 128
	}
	return 0, ErrMalformed
}

// readRest consumes and returns everything left in the body (PUBLISH payload).
func (d *decoder) readRest() []byte {
	rest := d.b[d.pos:]
	d.pos = len(d.b)
	return rest
}

// decodePacket reads one control packet from r（3.1.1 严格路径，冻结：
// CONNECT 级别仅 4，其余报文按 3.1.1 形态严格解析）。
func decodePacket(r io.Reader) (Packet, error) {
	return decodePacketV(r, false)
}

// DecodePacketV 是 v5 感知解码入口（v0.30.0）：v5=true 时 CONNECT 接受
// 级别 4 或 5（按级别字节自动分派，Connect.V5 反映协商结果），其余报文
// 按 v5 形态解析（可选原因码/属性区）；v5=false 与冻结的 decodePacket
// 完全一致。
func DecodePacketV(r io.Reader, v5 bool) (Packet, error) {
	return decodePacketV(r, v5)
}

func decodePacketV(r io.Reader, v5 bool) (Packet, error) {
	var hdr [1]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, ErrMalformedFixedHeader
	}
	byte1 := hdr[0]
	ptype := byte1 >> 4
	flags := byte1 & 0x0f
	n, err := decodeVarint(r)
	if err != nil {
		return nil, err
	}
	if int(n) > MaxRemainingLength {
		return nil, ErrMalformedVarint
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, ErrShortBody // declared length exceeds actual stream
	}
	d := &decoder{b: body}
	switch ptype {
	case PacketTypeCONNECT:
		if flags != 0 {
			return nil, ErrMalformedFixedHeader // reserved fixed-header flags
		}
		return decodeConnect(d, v5)
	case PacketTypeCONNACK:
		if flags != 0 {
			return nil, ErrMalformedFixedHeader
		}
		return decodeConnack(d, v5)
	case PacketTypePUBLISH:
		if (flags>>1)&0x03 == 3 {
			return nil, ErrMalformedFixedHeader // QoS 3 is illegal
		}
		return decodePublish(d, flags, v5)
	case PacketTypePUBACK:
		if flags != 0 {
			return nil, ErrMalformedFixedHeader
		}
		return decodePuback(d, v5)
	case PacketTypePUBREC, PacketTypePUBREL, PacketTypePUBCOMP:
		if ptype == PacketTypePUBREL {
			if flags != 0x02 {
				return nil, ErrMalformedFixedHeader // PUBREL fixed flags 0b0010
			}
		} else if flags != 0 {
			return nil, ErrMalformedFixedHeader
		}
		return decodePubRelID(ptype, d, v5)
	case PacketTypeSUBSCRIBE:
		if flags != 0x02 {
			return nil, ErrMalformedFixedHeader
		}
		return decodeSubscribe(d)
	case PacketTypeSUBACK:
		if flags != 0 {
			return nil, ErrMalformedFixedHeader
		}
		return decodeSuback(d, v5)
	case PacketTypePINGREQ, PacketTypePINGRESP, PacketTypeDISCONNECT:
		if flags != 0 {
			return nil, ErrMalformedFixedHeader
		}
		if ptype == PacketTypeDISCONNECT && v5 && n > 0 {
			// v5 DISCONNECT：rc(1B) + 属性长度（阶段一属性区恒 0）。
			rc, err := d.readByte()
			if err != nil {
				return nil, err
			}
			if _, err := decodeProps(d); err != nil {
				return nil, err
			}
			if d.remaining() != 0 {
				return nil, ErrMalformed
			}
			return &Disconnect{V5: true, ReasonCode: rc}, nil
		}
		if n != 0 {
			return nil, ErrMalformed // PINGREQ/PINGRESP/DISCONNECT must be empty
		}
		switch ptype {
		case PacketTypePINGREQ:
			return &Pingreq{}, nil
		case PacketTypePINGRESP:
			return &Pingresp{}, nil
		default:
			return &Disconnect{}, nil
		}
	default:
		return nil, ErrMalformedFixedHeader // unknown/reserved type
	}
}

func decodeConnect(d *decoder, allowV5 bool) (*Connect, error) {
	name, err := d.readString()
	if err != nil {
		return nil, err
	}
	if name != "MQTT" {
		return nil, ErrMalformedConnect
	}
	level, err := d.readByte()
	if err != nil {
		return nil, err
	}
	if level != 4 && !(allowV5 && level == 5) {
		return nil, ErrMalformedConnect
	}
	isV5 := level == 5
	f, err := d.readByte()
	if err != nil {
		return nil, err
	}
	if f&0x01 != 0 {
		return nil, ErrMalformedConnect // reserved connect-flags bit 0
	}
	ka, err := d.readUint16()
	if err != nil {
		return nil, err
	}
	c := &Connect{
		KeepAlive:    ka,
		CleanSession: f&0x02 != 0,
		WillQoS:      (f >> 3) & 0x03,
		WillRetain:   f&0x20 != 0,
		V5:           isV5,
	}
	if isV5 {
		// v5：属性区在 keepalive 后、payload 前（阶段一仅 Receive Maximum）。
		var pr propsV5
		if pr, err = decodeProps(d); err != nil {
			return nil, err
		}
		c.ReceiveMax = pr.ReceiveMax
		c.SessionExpiry = pr.SessionExpiry
	}
	// Payload order: ClientID, WillTopic, WillMessage, Username, Password.
	if c.ClientID, err = d.readString(); err != nil {
		return nil, err
	}
	if f&0x04 != 0 { // willFlag
		if c.WillTopic, err = d.readString(); err != nil {
			return nil, err
		}
		if c.WillMessage, err = d.readString(); err != nil {
			return nil, err
		}
	}
	if f&0x80 != 0 { // usernameFlag
		if c.Username, err = d.readString(); err != nil {
			return nil, err
		}
	}
	if f&0x40 != 0 { // passwordFlag
		if c.Password, err = d.readString(); err != nil {
			return nil, err
		}
	}
	if f&0x04 == 0 && (c.WillQoS != 0 || c.WillRetain) {
		return nil, ErrMalformedConnect // willFlag=0 implies WillQoS=0, no retain
	}
	if err := validateConnect(c); err != nil {
		return nil, err
	}
	if d.remaining() != 0 {
		return nil, ErrMalformedConnect // trailing bytes
	}
	return c, nil
}

func decodeConnack(d *decoder, v5 bool) (*Connack, error) {
	sp, err := d.readByte()
	if err != nil {
		return nil, err
	}
	if sp&0xFE != 0 {
		return nil, ErrMalformed // reserved session-present bits
	}
	rc, err := d.readByte()
	if err != nil {
		return nil, err
	}
	ca := &Connack{SessionPresent: sp&0x01 != 0, ReturnCode: rc}
	if v5 {
		ca.V5 = true
		var pr propsV5
		if pr, err = decodeProps(d); err != nil {
			return nil, err
		}
		ca.ReceiveMax = pr.ReceiveMax
		ca.SessionExpiry = pr.SessionExpiry
	}
	if d.remaining() != 0 {
		return nil, ErrMalformed
	}
	return ca, nil
}

func decodePublish(d *decoder, flags byte, v5 bool) (*Publish, error) {
	p := &Publish{
		Dup:    (flags >> 3) & 0x01,
		QoS:    (flags >> 1) & 0x03,
		Retain: flags&0x01 != 0,
		V5:     v5,
	}
	var err error
	if p.Topic, err = d.readString(); err != nil {
		return nil, err
	}
	if err := validateTopicName(p.Topic); err != nil {
		return nil, err
	}
	if p.QoS > 0 {
		if p.PacketID, err = d.readUint16(); err != nil {
			return nil, err
		}
		if p.PacketID == 0 {
			return nil, ErrMalformed
		}
	}
	if v5 {
		// v5 PUBLISH：属性长度（阶段一仅接受空属性区）。
		if n, perr := d.readVBI(); perr != nil || n != 0 {
			return nil, ErrMalformed
		}
	}
	p.Payload = d.readRest()
	return p, nil
}

// decodePuback 解码 PUBACK：3.1.1 严格 2B；v5 接受 2B（rc=0）/3B（id+rc）/
// 4B（id+rc+空属性区）。
func decodePuback(d *decoder, v5 bool) (*Puback, error) {
	id, err := d.readUint16()
	if err != nil {
		return nil, err
	}
	pa := &Puback{PacketID: id}
	if v5 {
		pa.V5 = true
		switch d.remaining() {
		case 0:
			// rc 视为 0x00（规范 §3.4.2.1）
		case 1:
			if pa.ReasonCode, err = d.readByte(); err != nil {
				return nil, err
			}
		default:
			if pa.ReasonCode, err = d.readByte(); err != nil {
				return nil, err
			}
			if _, err := decodeProps(d); err != nil {
				return nil, err
			}
		}
	}
	if d.remaining() != 0 {
		return nil, ErrMalformed
	}
	return pa, nil
}

// decodePubRelID decodes the shared body shape of PUBREC/PUBREL/PUBCOMP:
// a two-byte packet identifier followed by nothing else (v0.26.0)；v5
// 形态与 PUBACK 同构（可选 rc / rc+空属性区，v0.30.0）。
func decodePubRelID(ptype byte, d *decoder, v5 bool) (Packet, error) {
	id, err := d.readUint16()
	if err != nil {
		return nil, err
	}
	var rc byte
	if v5 {
		switch d.remaining() {
		case 0:
		case 1:
			if rc, err = d.readByte(); err != nil {
				return nil, err
			}
		default:
			if rc, err = d.readByte(); err != nil {
				return nil, err
			}
			if _, err := decodeProps(d); err != nil {
				return nil, err
			}
		}
	}
	if d.remaining() != 0 {
		return nil, ErrMalformed
	}
	switch ptype {
	case PacketTypePUBREC:
		return &Pubrec{PacketID: id, V5: v5, ReasonCode: rc}, nil
	case PacketTypePUBREL:
		return &Pubrel{PacketID: id, V5: v5, ReasonCode: rc}, nil
	default:
		return &Pubcomp{PacketID: id, V5: v5, ReasonCode: rc}, nil
	}
}

func decodeSubscribe(d *decoder) (*Subscribe, error) {
	id, err := d.readUint16()
	if err != nil {
		return nil, err
	}
	s := &Subscribe{PacketID: id}
	for d.remaining() > 0 {
		var tf TopicFilter
		if tf.Topic, err = d.readString(); err != nil {
			return nil, err
		}
		if tf.QoS, err = d.readByte(); err != nil {
			return nil, err
		}
		if tf.QoS > 2 {
			return nil, ErrMalformed
		}
		if err := validateTopicFilter(tf.Topic); err != nil {
			return nil, err
		}
		s.Topics = append(s.Topics, tf)
	}
	if len(s.Topics) == 0 {
		return nil, ErrMalformed // at least one filter is required
	}
	return s, nil
}

func decodeSuback(d *decoder, v5 bool) (*Suback, error) {
	id, err := d.readUint16()
	if err != nil {
		return nil, err
	}
	if v5 {
		if _, err := decodeProps(d); err != nil {
			return nil, err
		}
	}
	if d.remaining() == 0 {
		return nil, ErrMalformed
	}
	codes := make([]byte, d.remaining())
	copy(codes, d.b[d.pos:])
	d.pos = len(d.b)
	if !v5 {
		for _, c := range codes {
			switch c {
			case 0, 1, 2, 0x80:
			default:
				return nil, ErrMalformed
			}
		}
	}
	return &Suback{PacketID: id, Codes: codes, V5: v5}, nil
}

// ---------------------------------------------------------------------------
// Exported codec surface (v0.25.0 R-6 consolidation).
//
// Thin wrappers over the unexported implementations in this file, exported
// so pkg/mqttsim and external test brokers reuse the single wire codec
// instead of carrying private copies. The unexported functions stay as-is:
// existing in-package tests and call sites are untouched.
// ---------------------------------------------------------------------------

// EncodePacket serialises one MQTT packet to w using the 3.1.1 wire format.
func EncodePacket(w io.Writer, p Packet) error { return encodePacket(w, p) }

// DecodePacket reads exactly one MQTT packet from r and returns it.
func DecodePacket(r io.Reader) (Packet, error) { return decodePacket(r) }

// ValidateTopicFilter reports whether filter is a legal 3.1.1 subscription
// filter ('+'/'#' wildcard placement rules).
func ValidateTopicFilter(filter string) error { return validateTopicFilter(filter) }
