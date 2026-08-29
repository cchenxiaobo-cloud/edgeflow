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
	case *Connect, *Connack, *Puback, *Suback, *Pingreq, *Pingresp, *Disconnect:
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
	e.writeByte(4)        // protocol level 4 (MQTT 3.1.1)
	e.writeByte(f)
	writeU16(e, c.KeepAlive)
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
	e.writeBytes(p.Payload)
	return nil
}

func (p *Puback) encodeUA(e *encoder) error {
	writeU16(e, p.PacketID)
	return nil
}

func (s *Subscribe) encodeUA(e *encoder) error {
	if len(s.Topics) == 0 {
		return ErrMalformed
	}
	writeU16(e, s.PacketID)
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
	for _, c := range s.Codes {
		switch c {
		case 0, 1, 2, 0x80:
		default:
			return ErrMalformed
		}
	}
	writeU16(e, s.PacketID)
	for _, c := range s.Codes {
		e.writeByte(c)
	}
	return nil
}

func (p *Pingreq) encodeUA(e *encoder) error { return nil }

func (p *Pingresp) encodeUA(e *encoder) error { return nil }

func (d *Disconnect) encodeUA(e *encoder) error { return nil }

// ---------------------------------------------------------------------------
// Decoding.
// ---------------------------------------------------------------------------

// decoder walks the fixed-size body slice of a packet.
type decoder struct {
	b   []byte
	pos int
}

func (d *decoder) remaining() int { return len(d.b) - d.pos }

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

// readRest consumes and returns everything left in the body (PUBLISH payload).
func (d *decoder) readRest() []byte {
	rest := d.b[d.pos:]
	d.pos = len(d.b)
	return rest
}

// decodePacket reads one control packet from r.
func decodePacket(r io.Reader) (Packet, error) {
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
		return decodeConnect(d)
	case PacketTypeCONNACK:
		if flags != 0 {
			return nil, ErrMalformedFixedHeader
		}
		return decodeConnack(d)
	case PacketTypePUBLISH:
		if (flags>>1)&0x03 == 3 {
			return nil, ErrMalformedFixedHeader // QoS 3 is illegal
		}
		return decodePublish(d, flags)
	case PacketTypePUBACK:
		if flags != 0 {
			return nil, ErrMalformedFixedHeader
		}
		return decodePuback(d)
	case PacketTypeSUBSCRIBE:
		if flags != 0x02 {
			return nil, ErrMalformedFixedHeader
		}
		return decodeSubscribe(d)
	case PacketTypeSUBACK:
		if flags != 0 {
			return nil, ErrMalformedFixedHeader
		}
		return decodeSuback(d)
	case PacketTypePINGREQ, PacketTypePINGRESP, PacketTypeDISCONNECT:
		if flags != 0 {
			return nil, ErrMalformedFixedHeader
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

func decodeConnect(d *decoder) (*Connect, error) {
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
	if level != 4 {
		return nil, ErrMalformedConnect
	}
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

func decodeConnack(d *decoder) (*Connack, error) {
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
	if d.remaining() != 0 {
		return nil, ErrMalformed
	}
	return &Connack{SessionPresent: sp&0x01 != 0, ReturnCode: rc}, nil
}

func decodePublish(d *decoder, flags byte) (*Publish, error) {
	p := &Publish{
		Dup:    (flags >> 3) & 0x01,
		QoS:    (flags >> 1) & 0x03,
		Retain: flags&0x01 != 0,
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
	p.Payload = d.readRest()
	return p, nil
}

func decodePuback(d *decoder) (*Puback, error) {
	id, err := d.readUint16()
	if err != nil {
		return nil, err
	}
	if d.remaining() != 0 {
		return nil, ErrMalformed
	}
	return &Puback{PacketID: id}, nil
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

func decodeSuback(d *decoder) (*Suback, error) {
	id, err := d.readUint16()
	if err != nil {
		return nil, err
	}
	if d.remaining() == 0 {
		return nil, ErrMalformed
	}
	codes := make([]byte, d.remaining())
	copy(codes, d.b[d.pos:])
	d.pos = len(d.b)
	for _, c := range codes {
		switch c {
		case 0, 1, 2, 0x80:
		default:
			return nil, ErrMalformed
		}
	}
	return &Suback{PacketID: id, Codes: codes}, nil
}
