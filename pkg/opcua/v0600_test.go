// v0600_test.go：OPC-UA D4/D5 单测（v0.60.0，spec 0033）——
// D5 PSHA 规范化 golden 锚 + D4 TokenID 严格比对拒绝路径。
package opcua

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// TestV0600PSHAGoldenAnchor：固定 nonce 输入 → 固定密钥输出（六段 hex 锚，
// 防 KDF 实现漂移；PSHA 结构正确性由 A 链 + 块推导实现保证）。
func TestV0600PSHAGoldenAnchor(t *testing.T) {
	cn := bytes.Repeat([]byte{0x11}, 32)
	sn := bytes.Repeat([]byte{0x22}, 32)
	k := deriveKeys(cn, sn)
	golden := strings.Join([]string{
		hex.EncodeToString(k.ClientEncryptKey),
		hex.EncodeToString(k.ClientMACKey),
		hex.EncodeToString(k.ClientEncryptIV),
		hex.EncodeToString(k.ServerEncryptKey),
		hex.EncodeToString(k.ServerMACKey),
		hex.EncodeToString(k.ServerEncryptIV),
	}, ",")
	const want = "b215456a6843e40769d120b403bf075e,0463ff7cd7160c3642a495b6a0011a5dc2d0f9d537407ba3b723633506382de2,f146fc1d745a0200a10c36db791c8d91,8bf5cf64d5389b5037a88984cd040752,b6dfaa8b6178a34f945969647f922b2159dfab2348d31ce7c85da71b8337604c,0ca7be9cad444cac7bf596772d482c58"
	if golden != want {
		t.Fatalf("PSHA golden 锚漂移:\n got %s\nwant %s", golden, want)
	}
}

// TestV0600TokenIDStrictRejection：D4 严格比对——正确令牌帧通过；错误令牌
// 帧（密钥匹配但 TokenID 不符）显式拒绝；未知密钥帧（两组试钥均失败）拒绝。
func TestV0600TokenIDStrictRejection(t *testing.T) {
	cn := bytes.Repeat([]byte{0x11}, 32)
	sn := bytes.Repeat([]byte{0x22}, 32)
	keys := DeriveKeys(cn, sn)
	sc := &SecureChannel{channelId: 1}
	sc.keysMu.Lock()
	sc.cur = keySet{keys: keys, tokenID: 7}
	sc.keysMu.Unlock()

	plain := []byte("hello")
	// ① 正确令牌帧 → 通过
	frame, err := SealMSGFrame(MsgSecureMessage, 1, 7, keys, false, plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sc.openIncoming(MsgSecureMessage, frame[HeaderSize:])
	if err != nil || string(got) != string(plain) {
		t.Fatalf("正确令牌帧应通过: %v %q", err, got)
	}
	// ② 错误令牌帧（密钥同、TokenID=9）→ 显式拒绝
	frame9, err := SealMSGFrame(MsgSecureMessage, 1, 9, keys, false, plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sc.openIncoming(MsgSecureMessage, frame9[HeaderSize:]); err == nil {
		t.Fatal("错误 TokenID 帧应显式拒绝")
	}
	// ③ 未知密钥帧（他组密钥密封）→ 两组试钥均失败，拒绝
	other := DeriveKeys(bytes.Repeat([]byte{0xEE}, 32), bytes.Repeat([]byte{0xFF}, 32))
	frameOther, err := SealMSGFrame(MsgSecureMessage, 1, 9, other, false, plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sc.openIncoming(MsgSecureMessage, frameOther[HeaderSize:]); err == nil {
		t.Fatal("未知密钥帧应拒绝")
	}
}
