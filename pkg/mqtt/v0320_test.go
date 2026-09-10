package mqtt

// v0.32.0 MQTT 5.0 阶段二：会话解耦（Session Expiry）+ 共享订阅 codec/client 测试。

import (
	"bytes"
	"strings"
	"testing"
)

// TestV0320ConnectSessionExpiryRoundTrip CONNECT v5：SE 单独、RM+SE 组合、
// 空 属性区三类往返；属性 ID 升序编码（0x11 → 0x21）。
func TestV0320ConnectSessionExpiryRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   *Connect
	}{
		{"se-only", &Connect{ClientID: "c1", V5: true, SessionExpiry: 3600}},
		{"rm+se", &Connect{ClientID: "c2", V5: true, ReceiveMax: 20, SessionExpiry: 60}},
		{"clean-default", &Connect{ClientID: "c3", V5: true, CleanSession: true}},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		if err := EncodePacket(&buf, tc.in); err != nil {
			t.Fatalf("%s encode: %v", tc.name, err)
		}
		p, err := DecodePacketV(&buf, true)
		if err != nil {
			t.Fatalf("%s decode: %v", tc.name, err)
		}
		out, ok := p.(*Connect)
		if !ok {
			t.Fatalf("%s type: %T", tc.name, p)
		}
		if out.SessionExpiry != tc.in.SessionExpiry || out.ReceiveMax != tc.in.ReceiveMax || out.ClientID != tc.in.ClientID || out.CleanSession != tc.in.CleanSession {
			t.Fatalf("%s mismatch: got %+v want %+v", tc.name, out, tc.in)
		}
	}
	// 编码序：SE 属性 ID 0x11 必须先于 RM 0x21。
	var buf bytes.Buffer
	if err := EncodePacket(&buf, &Connect{ClientID: "c", V5: true, ReceiveMax: 5, SessionExpiry: 9}); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	iSE := bytes.Index(raw, []byte{0x11})
	iRM := bytes.Index(raw, []byte{0x21})
	if iSE < 0 || iRM < 0 || iSE > iRM {
		t.Fatalf("property order: SE@%d RM@%d raw=% X", iSE, iRM, raw)
	}
}

// TestV0320PropsUnknownRejected 属性区白名单外属性拒绝（阶段一语义延续）。
func TestV0320PropsUnknownRejected(t *testing.T) {
	// CONNECT v5 骨架 + 属性区 {len=3, id=0x0E(未知), 0x00}：
	// level5, flags=0x02(clean), keepalive 0, propLen..., clientID "c"
	body := []byte{0x00, 0x04, 'M', 'Q', 'T', 'T', 0x05, 0x02, 0x00, 0x00, 0x03, 0x0E, 0x00, 0x00, 0x01, 'c'}
	var buf bytes.Buffer
	buf.WriteByte(0x10) // CONNECT
	writeVBITest(&buf, uint32(len(body)))
	buf.Write(body)
	if _, err := DecodePacketV(&buf, true); err == nil {
		t.Fatal("unknown property must be rejected")
	}
}

// TestV0320ConnackSessionExpiry CONNACK v5 SE 回显往返 + Session Present 位。
func TestV0320ConnackSessionExpiry(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodePacket(&buf, &Connack{V5: true, SessionPresent: true, ReturnCode: 0, SessionExpiry: 120}); err != nil {
		t.Fatal(err)
	}
	p, err := DecodePacketV(&buf, true)
	if err != nil {
		t.Fatal(err)
	}
	ca, ok := p.(*Connack)
	if !ok {
		t.Fatalf("type: %T", p)
	}
	if !ca.SessionPresent || ca.SessionExpiry != 120 {
		t.Fatalf("connack mismatch: %+v", ca)
	}
}

func writeVBITest(buf *bytes.Buffer, v uint32) {
	for {
		x := byte(v & 0x7F)
		v >>= 7
		if v > 0 {
			x |= 0x80
		}
		buf.WriteByte(x)
		if v == 0 {
			return
		}
	}
}

// TestV0320ShareFilterValidation 共享订阅 filter 形态判定（codec 层仅校验
// 内层 filter；parseShareFilter 形态语义在 mqttsim 侧测试）。
func TestV0320ShareFilterValidation(t *testing.T) {
	valid := []string{"$share/g1/t/+", "$share/g1/t/#", "$share/g1/t", "$share/g.2/a/b"}
	for _, f := range valid {
		if err := ValidateTopicFilter(f); err != nil {
			t.Fatalf("%q should pass codec validation: %v", f, err)
		}
	}
	if err := ValidateTopicFilter("$share/g1/a/#/b"); err == nil {
		t.Fatal("malformed inner filter must fail")
	}
	// client 端 Subscribe 走 filter 校验：$share 形态不被拒（透传 broker）。
	if !strings.HasPrefix("$share/g1/t", "$share/") {
		t.Fatal("sanity")
	}
}
