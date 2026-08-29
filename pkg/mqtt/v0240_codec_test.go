package mqtt

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// ---- helpers ----

func enc(t *testing.T, p Packet) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := encodePacket(&buf, p); err != nil {
		t.Fatalf("encode %T: %v", p, err)
	}
	return buf.Bytes()
}

func dec(t *testing.T, b []byte) Packet {
	t.Helper()
	p, err := decodePacket(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

func rt(t *testing.T, in Packet) {
	t.Helper()
	out := dec(t, enc(t, in))
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch for %T:\n got %#v\nwant %#v", in, out, in)
	}
}

func wantErr(t *testing.T, err error, target error) {
	t.Helper()
	if err == nil || !errors.Is(err, target) {
		t.Fatalf("want error %v, got %v", target, err)
	}
}

// ---- varint ----

func TestEncodeVarint(t *testing.T) {
	cases := []struct {
		v    uint32
		want []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{16383, []byte{0xff, 0x7f}},
		{16384, []byte{0x80, 0x80, 0x01}},
		{MaxRemainingLength, []byte{0xff, 0xff, 0xff, 0x7f}},
	}
	for _, c := range cases {
		got := encodeVarint(c.v)
		if !bytes.Equal(got, c.want) {
			t.Errorf("encodeVarint(%d) = % x, want % x", c.v, got, c.want)
		}
	}
}

func TestDecodeVarint(t *testing.T) {
	cases := []struct {
		in   []byte
		want uint32
	}{
		{[]byte{0x00}, 0},
		{[]byte{0x7f}, 127},
		{[]byte{0x80, 0x01}, 128},
		{[]byte{0xff, 0x7f}, 16383},
		{[]byte{0x80, 0x80, 0x01}, 16384},
		{[]byte{0xff, 0xff, 0xff, 0x7f}, MaxRemainingLength},
	}
	for _, c := range cases {
		got, err := decodeVarint(bytes.NewReader(c.in))
		if err != nil || got != c.want {
			t.Errorf("decodeVarint(% x) = %d, %v; want %d, nil", c.in, got, err, c.want)
		}
	}
}

func TestDecodeVarintTooLong(t *testing.T) {
	// Continuation bit still set on the 4th byte: a 5-byte varint.
	_, err := decodeVarint(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff, 0x01}))
	wantErr(t, err, ErrMalformedVarint)
	// Truncated stream.
	_, err = decodeVarint(bytes.NewReader([]byte{0x80}))
	wantErr(t, err, ErrMalformedVarint)
}

// ---- topic validation ----

func TestValidateTopicName(t *testing.T) {
	for _, ok := range []string{"a", "/a", "a/", "a/b", "a b", "/"} {
		if err := validateTopicName(ok); err != nil {
			t.Errorf("validateTopicName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "a\x00b", "a#", "a+", "#", "+"} {
		if err := validateTopicName(bad); !errors.Is(err, ErrMalformedTopic) {
			t.Errorf("validateTopicName(%q) = %v, want ErrMalformedTopic", bad, err)
		}
	}
}

func TestValidateTopicFilter(t *testing.T) {
	for _, ok := range []string{"#", "+", "a/#", "a/+", "b//a", "/a", "a/", "/+", "sport/#", "+/tennis/#"} {
		if err := validateTopicFilter(ok); err != nil {
			t.Errorf("validateTopicFilter(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "#a", "a#", "a+", "+a", "a/+/b/#/c", "a/\x00b"} {
		if err := validateTopicFilter(bad); !errors.Is(err, ErrMalformedTopic) {
			t.Errorf("validateTopicFilter(%q) = %v, want ErrMalformedTopic", bad, err)
		}
	}
}

// ---- illegal fixed headers / bodies ----

func TestIllegalFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{
			// SUBSCRIBE must carry fixed flags 0b0010; here flags=0b0001.
			name: "subscribe wrong flags",
			in:   []byte{0x81, 0x05, 0x00, 0x01, 0x00, 0x03, 'a', '/', 'b', 0x00},
			want: ErrMalformedFixedHeader,
		},
		{
			// PUBLISH QoS=3 (0b11) is reserved.
			name: "publish qos3",
			in:   []byte{0x36, 0x04, 0x00, 0x01, 'a', 0x00, 0x01},
			want: ErrMalformedFixedHeader,
		},
		{
			// CONNECT fixed-header flags (low nibble) must be 0.
			name: "connect reserved header flags",
			in:   []byte{0x11, 0x0e, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04, 0x02, 0x00, 0x3c, 0x00, 0x03, 'c', '1'},
			want: ErrMalformedFixedHeader,
		},
		{
			// CONNECT flags bit0 is reserved and must be 0.
			name: "connect reserved body bit0",
			in:   []byte{0x10, 0x0e, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04, 0x03, 0x00, 0x3c, 0x00, 0x03, 'c', '1'},
			want: ErrMalformedConnect,
		},
		{
			// PINGREQ must have an empty body.
			name: "pingreq with payload",
			in:   []byte{0xc0, 0x01, 0x00},
			want: ErrMalformed,
		},
		{
			// PINGREQ fixed-header flags must be 0.
			name: "pingreq with header flags",
			in:   []byte{0xc1, 0x00},
			want: ErrMalformedFixedHeader,
		},
		{
			// DISCONNECT must have an empty body.
			name: "disconnect with payload",
			in:   []byte{0xe0, 0x01, 0x00},
			want: ErrMalformed,
		},
		{
			// Type 6 is reserved in MQTT 3.1.1.
			name: "unknown type",
			in:   []byte{0x60, 0x00},
			want: ErrMalformedFixedHeader,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodePacket(bytes.NewReader(c.in))
			wantErr(t, err, c.want)
		})
	}
}

func TestTruncatedStream(t *testing.T) {
	full := enc(t, &Publish{Topic: "a/b", Payload: []byte("hi")})
	for cut := 1; cut < len(full); cut++ {
		if _, err := decodePacket(bytes.NewReader(full[:cut])); err == nil {
			t.Fatalf("decode(truncated to %d bytes) = nil error, want error", cut)
		}
	}
}

func TestLengthCheat(t *testing.T) {
	// Declared remaining length 7 but only 6 body bytes follow.
	in := []byte{0x30, 0x07, 0x00, 0x03, 'a', '/', 'b', 'h'}
	_, err := decodePacket(bytes.NewReader(in))
	wantErr(t, err, ErrShortBody)
}

// ---- QoS / packet-id rules ----

func TestQoSPacketIDRules(t *testing.T) {
	// QoS0 must not carry a packet id.
	err := encodePacket(&bytes.Buffer{}, &Publish{Topic: "a", PacketID: 5})
	wantErr(t, err, ErrMalformed)
	// QoS1 must carry a packet id.
	err = encodePacket(&bytes.Buffer{}, &Publish{QoS: 1, Topic: "a"})
	wantErr(t, err, ErrMalformed)
	// QoS3 is rejected without panicking.
	err = encodePacket(&bytes.Buffer{}, &Publish{QoS: 3, Topic: "a"})
	wantErr(t, err, ErrMalformed)
	// QoS1/2 with packet id round-trip.
	rt(t, &Publish{QoS: 1, Topic: "x/y", PacketID: 7, Payload: []byte("pay")})
	rt(t, &Publish{QoS: 2, Topic: "x/y", PacketID: 9, Payload: []byte{}})
	// QoS1 decoded from wire keeps the packet id.
	out := dec(t, enc(t, &Publish{QoS: 1, Dup: 1, Topic: "t", PacketID: 42, Payload: []byte("z")})).(*Publish)
	if out.PacketID != 42 || out.Dup != 1 || out.QoS != 1 {
		t.Fatalf("qos1 publish decoded wrong: %+v", out)
	}
}

// ---- will-flag combinations ----

func TestWillFlagCombos(t *testing.T) {
	for qos := byte(0); qos <= 2; qos++ {
		rt(t, &Connect{ClientID: "c", CleanSession: true, WillTopic: "w/t", WillMessage: "gone", WillQoS: qos})
	}
	rt(t, &Connect{ClientID: "c", WillTopic: "w", WillMessage: "m", WillQoS: 1, WillRetain: true})
	// willFlag=0 but WillQoS!=0 is rejected on encode and decode.
	err := encodePacket(&bytes.Buffer{}, &Connect{ClientID: "c", WillQoS: 1})
	wantErr(t, err, ErrMalformedConnect)
	err = encodePacket(&bytes.Buffer{}, &Connect{ClientID: "c", WillRetain: true})
	wantErr(t, err, ErrMalformedConnect)
	// willFlag with an empty will topic is rejected.
	err = encodePacket(&bytes.Buffer{}, &Connect{ClientID: "c", WillMessage: "m"})
	wantErr(t, err, ErrMalformedConnect)
	// Empty ClientID is rejected.
	err = encodePacket(&bytes.Buffer{}, &Connect{})
	wantErr(t, err, ErrMalformedConnect)
	_, err = decodePacket(bytes.NewReader([]byte{0x10, 0x0c, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04, 0x02, 0x00, 0x3c, 0x00, 0x00}))
	wantErr(t, err, ErrMalformedConnect)
}

// ---- round-trips ----

func TestRoundTripConnect(t *testing.T) {
	rt(t, &Connect{ClientID: "edge-01", KeepAlive: 30, CleanSession: true})
	rt(t, &Connect{ClientID: "edge-01", KeepAlive: 65535,
		Username: "u", Password: "p", WillTopic: "w/t", WillMessage: "gone", WillQoS: 2, WillRetain: true})
}

func TestRoundTripConnack(t *testing.T) {
	rt(t, &Connack{SessionPresent: true, ReturnCode: 0})
	rt(t, &Connack{ReturnCode: 5})
}

func TestRoundTripPuback(t *testing.T) {
	rt(t, &Puback{PacketID: 3})
}

func TestRoundTripSubscribeSuback(t *testing.T) {
	rt(t, &Subscribe{PacketID: 2, Topics: []TopicFilter{{Topic: "a/b", QoS: 1}, {Topic: "c/#", QoS: 0}}})
	rt(t, &Suback{PacketID: 2, Codes: []byte{0, 1, 2, 0x80}})
	err := encodePacket(&bytes.Buffer{}, &Suback{PacketID: 2, Codes: []byte{3}})
	wantErr(t, err, ErrMalformed)
	_, err = decodePacket(bytes.NewReader([]byte{0x90, 0x03, 0x00, 0x02, 0x03}))
	wantErr(t, err, ErrMalformed)
}

func TestRoundTripPingDisconnect(t *testing.T) {
	rt(t, &Pingreq{})
	rt(t, &Pingresp{})
	rt(t, &Disconnect{})
}

// ---- golden bytes ----

func TestGoldenConnect(t *testing.T) {
	// CONNECT: protocol "MQTT" level 4, clean session, keepAlive=60, ClientID="c1".
	got := enc(t, &Connect{ClientID: "c1", KeepAlive: 60, CleanSession: true})
	want := []byte{
		0x10,                           // byte1: type CONNECT, flags 0
		0x0e,                           // remaining length 14
		0x00, 0x04, 'M', 'Q', 'T', 'T', // protocol name
		0x04,       // protocol level 4 (MQTT 3.1.1)
		0x02,       // connect flags: clean session (bit1)
		0x00, 0x3c, // keep alive 60
		0x00, 0x02, 'c', '1', // client id "c1" (2-byte string; the brief's "00 03" prefix was a length typo)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden CONNECT = % x, want % x", got, want)
	}
	out := dec(t, want).(*Connect)
	if out.ClientID != "c1" || out.KeepAlive != 60 || !out.CleanSession {
		t.Fatalf("golden CONNECT decoded wrong: %+v", out)
	}
}

func TestGoldenConnectTaskBytesRejected(t *testing.T) {
	// The task brief's golden listed connect flags 0xc2 (username+password set)
	// with remaining length 14 and a client-id length prefix of 3 for the
	// 2-byte string "c1" — the buffer runs out inside that string field, so it
	// must decode as malformed (string-field truncation).
	in := []byte{0x10, 0x0e, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04, 0xc2, 0x00, 0x3c, 0x00, 0x03, 'c', '1'}
	_, err := decodePacket(bytes.NewReader(in))
	wantErr(t, err, ErrMalformed)
}

func TestGoldenPublish(t *testing.T) {
	// PUBLISH QoS0, topic "a/b", payload "hi".
	got := enc(t, &Publish{Topic: "a/b", Payload: []byte("hi")})
	want := []byte{
		0x30,                      // byte1: type PUBLISH, DUP=0 QoS=0 RETAIN=0
		0x07,                      // remaining length 7
		0x00, 0x03, 'a', '/', 'b', // topic "a/b"
		'h', 'i', // payload
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden PUBLISH = % x, want % x", got, want)
	}
	out := dec(t, want).(*Publish)
	if out.Topic != "a/b" || string(out.Payload) != "hi" || out.QoS != 0 || out.PacketID != 0 {
		t.Fatalf("golden PUBLISH decoded wrong: %+v", out)
	}
}
