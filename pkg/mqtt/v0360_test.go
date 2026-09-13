package mqtt

// v0.36.0 MQTT 5.0 阶段五：v5 Will Properties 编解码测试。

import (
	"bytes"
	"reflect"
	"testing"
)

// v0360RTV5 v5 CONNECT roundtrip（encode → DecodePacketV(v5=true)）。
func v0360RTV5(t *testing.T, in Packet) Packet {
	t.Helper()
	var buf bytes.Buffer
	if err := EncodePacket(&buf, in); err != nil {
		t.Fatalf("encode %T: %v", in, err)
	}
	out, err := DecodePacketV(&buf, true)
	if err != nil {
		t.Fatalf("decode %T: %v", in, err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch for %T:\n got %#v\nwant %#v", in, out, in)
	}
	return out
}

// US-1：v5 will props roundtrip（delay 0/非 0）。
func TestV0360WillPropsRoundTrip(t *testing.T) {
	v0360RTV5(t, &Connect{V5: true, ClientID: "c", WillTopic: "w/t", WillMessage: "gone", WillQoS: 0, WillDelay: 0})
	v0360RTV5(t, &Connect{V5: true, ClientID: "c", WillTopic: "w/t", WillMessage: "gone", WillQoS: 1, WillDelay: 5})
	v0360RTV5(t, &Connect{V5: true, ClientID: "c", WillTopic: "w/t", WillMessage: "gone", WillDelay: 3600, WillRetain: true})
}

// US-1：手工字节解码（真实客户端形态：Will Properties 区 0x18）。
func TestV0360WillPropsManualDecode(t *testing.T) {
	wire := []byte{0x10, 0x1A,
		0x00, 0x04, 'M', 'Q', 'T', 'T', 0x05, 0x06, 0x00, 0x3C, 0x00,
		0x00, 0x01, 'c',
		0x05, 0x18, 0x00, 0x00, 0x00, 0x07,
		0x00, 0x01, 'w',
		0x00, 0x01, 'm'}
	p, err := DecodePacketV(bytes.NewReader(wire), true)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	c, ok := p.(*Connect)
	if !ok {
		t.Fatalf("expected *Connect, got %T", p)
	}
	if c.WillDelay != 7 || c.WillTopic != "w" || c.WillMessage != "m" || !c.V5 {
		t.Fatalf("decoded: %+v", c)
	}

	// 空 Will Properties 区（长度 0x00）同样合法。
	wire0 := []byte{0x10, 0x15,
		0x00, 0x04, 'M', 'Q', 'T', 'T', 0x05, 0x06, 0x00, 0x3C, 0x00,
		0x00, 0x01, 'c',
		0x00,
		0x00, 0x01, 'w',
		0x00, 0x01, 'm'}
	p0, err := DecodePacketV(bytes.NewReader(wire0), true)
	if err != nil {
		t.Fatalf("decode empty props: %v", err)
	}
	if c0 := p0.(*Connect); c0.WillDelay != 0 || c0.WillTopic != "w" {
		t.Fatalf("empty-props decoded: %+v", c0)
	}
}

// US-1：will props 拒绝（白名单外属性/重复属性）。
func TestV0360WillPropsReject(t *testing.T) {
	// 白名单外 will 属性（0x01 Payload Format Indicator）。
	bad := []byte{0x10, 0x17,
		0x00, 0x04, 'M', 'Q', 'T', 'T', 0x05, 0x06, 0x00, 0x3C, 0x00,
		0x00, 0x01, 'c',
		0x02, 0x01, 0x00,
		0x00, 0x01, 'w',
		0x00, 0x01, 'm'}
	_, err := DecodePacketV(bytes.NewReader(bad), true)
	wantErr(t, err, ErrMalformed)

	// 重复 Will Delay（0x18 ×2）。
	dup := []byte{0x10, 0x1F,
		0x00, 0x04, 'M', 'Q', 'T', 'T', 0x05, 0x06, 0x00, 0x3C, 0x00,
		0x00, 0x01, 'c',
		0x0A, 0x18, 0x00, 0x00, 0x00, 0x01, 0x18, 0x00, 0x00, 0x00, 0x02,
		0x00, 0x01, 'w',
		0x00, 0x01, 'm'}
	_, err = DecodePacketV(bytes.NewReader(dup), true)
	wantErr(t, err, ErrMalformed)
}

// US-1：编码侧校验（3.1.1 不允许 WillDelay；willFlag=0 不允许 WillDelay）。
func TestV0360WillPropsEncodeReject(t *testing.T) {
	err := encodePacket(&bytes.Buffer{}, &Connect{ClientID: "c", WillTopic: "w", WillMessage: "m", WillDelay: 1})
	wantErr(t, err, ErrMalformedConnect)
	err = encodePacket(&bytes.Buffer{}, &Connect{V5: true, ClientID: "c", WillDelay: 1})
	wantErr(t, err, ErrMalformedConnect)
}

// US-5：v5 无 will 连接编码字节不变（回归锚：Will Properties 区仅
// willFlag=1 时出现）。
func TestV0360NoWillByteIdentical(t *testing.T) {
	got := enc(t, &Connect{V5: true, ClientID: "x"})
	want := []byte{0x10, 0x0E,
		0x00, 0x04, 'M', 'Q', 'T', 'T', 0x05, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 'x'}
	if !bytes.Equal(got, want) {
		t.Fatalf("no-will v5 connect bytes changed:\n got % x\nwant % x", got, want)
	}
}
