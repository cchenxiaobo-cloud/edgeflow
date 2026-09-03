// v0281_security_test.go — v0.28.1 OPN 体加密与端到端 Basic256Sha256 互通。
//
// 覆盖：
//  1. 密码学接线层（包内）：Wrap/Unwrap roundtrip、Sign/Verify、
//     DeriveKeys 确定性、篡改拒绝。
//  2. 服务端语义（e2e，opcua_test）：WithIdentity 后 B256 端到端握手互通
//     + MSG 服务可用；未注入身份保持 v0.28.0 显式拒绝（v0280 冻结语义）。

package opcua

import (
	"bytes"
	"crypto/rsa"
	"testing"
)

// TestV0281WrapOPNBodyRoundTrip：RSA-OAEP 封包/解封 roundtrip + 篡改拒绝。
func TestV0281WrapOPNBodyRoundTrip(t *testing.T) {
	key := testKey(t)
	inner := append([]byte("0123456789abcdef0123456789abcdef"), []byte("legacy-opn-body")...)
	ct, err := WrapOPNBody(&key.PublicKey, inner)
	if err != nil {
		t.Fatalf("WrapOPNBody: %v", err)
	}
	got, err := UnwrapOPNBody(key, ct)
	if err != nil {
		t.Fatalf("UnwrapOPNBody: %v", err)
	}
	if !bytes.Equal(got, inner) {
		t.Fatal("roundtrip 不一致")
	}
	bad := append([]byte{}, ct...)
	bad[0] ^= 0x80
	if _, err := UnwrapOPNBody(key, bad); err == nil {
		t.Fatal("篡改密文应解封失败")
	}
}

// TestV0281SignVerifyOPNBody：签名验证通过 + 篡改拒绝 + 错误公钥拒绝。
func TestV0281SignVerifyOPNBody(t *testing.T) {
	key := testKey(t)
	other := testKey(t)
	hdr := []byte("asym-header-wire")
	body := []byte("plain-legacy-body")
	sig, err := SignOPNBody(key, hdr, body)
	if err != nil {
		t.Fatalf("SignOPNBody: %v", err)
	}
	if !VerifyOPNBody(&key.PublicKey, hdr, body, sig) {
		t.Fatal("合法签名应验证通过")
	}
	if VerifyOPNBody(&other.PublicKey, hdr, body, sig) {
		t.Fatal("错误公钥应验证失败")
	}
	if VerifyOPNBody(&key.PublicKey, hdr, append(body, 'x'), sig) {
		t.Fatal("篡改体应验证失败")
	}
	if VerifyOPNBody(&key.PublicKey, hdr, body, append(append([]byte{}, sig...), 0)) {
		t.Fatal("篡改签名应验证失败")
	}
}

// TestV0281DeriveKeysDeterministicAndSymmetric：同输入派生一致、换 nonce 变化、
// 全部密钥段长度正确（16/16/32/16/16/32）。
func TestV0281DeriveKeysDeterministicAndSymmetric(t *testing.T) {
	cn := bytes.Repeat([]byte{0x11}, B256NonceLen)
	sn := bytes.Repeat([]byte{0x22}, B256NonceLen)
	cc := bytes.Repeat([]byte{0x33}, 256)
	sc2 := bytes.Repeat([]byte{0x44}, 256)
	k1 := DeriveKeys(cn, sn, cc, sc2)
	k2 := DeriveKeys(cn, sn, cc, sc2)
	if !bytes.Equal(k1.ClientEncryptKey, k2.ClientEncryptKey) || !bytes.Equal(k1.ServerMACKey, k2.ServerMACKey) {
		t.Fatal("同输入派生应逐字节一致")
	}
	k3 := DeriveKeys(cn, bytes.Repeat([]byte{0x23}, B256NonceLen), cc, sc2)
	if bytes.Equal(k1.ClientEncryptKey, k3.ClientEncryptKey) {
		t.Fatal("serverNonce 变化应导致派生变化")
	}
	for name, seg := range map[string][]byte{
		"ClientEncryptKey": k1.ClientEncryptKey, "ClientEncryptIV": k1.ClientEncryptIV,
		"ClientMACKey": k1.ClientMACKey, "ServerEncryptKey": k1.ServerEncryptKey,
		"ServerEncryptIV": k1.ServerEncryptIV, "ServerMACKey": k1.ServerMACKey,
	} {
		want := 32
		if name == "ClientEncryptKey" || name == "ServerEncryptKey" || name == "ClientEncryptIV" || name == "ServerEncryptIV" {
			want = 16
		}
		if len(seg) != want {
			t.Fatalf("%s 长度 %d 应为 %d", name, len(seg), want)
		}
	}
}

// TestV0281ClientServerKeysMatch：客户端（派生输入=本端 nonce+证书）与
// 服务端（同函数同输入）密钥协商一致性——双向核对全部 6 段。
func TestV0281ClientServerKeysMatch(t *testing.T) {
	key := testKey(t)
	cert := realSelfSignedCert(t, key)
	cn := bytes.Repeat([]byte{0xAA}, B256NonceLen)
	sn := bytes.Repeat([]byte{0xBB}, B256NonceLen)
	clientSide := deriveKeys(cn, sn, cert.Raw, cert.Raw)
	serverSide := DeriveKeys(cn, sn, cert.Raw, cert.Raw)
	pairs := [][2][]byte{
		{clientSide.ClientEncryptKey, serverSide.ClientEncryptKey},
		{clientSide.ClientEncryptIV, serverSide.ClientEncryptIV},
		{clientSide.ClientMACKey, serverSide.ClientMACKey},
		{clientSide.ServerEncryptKey, serverSide.ServerEncryptKey},
		{clientSide.ServerEncryptIV, serverSide.ServerEncryptIV},
		{clientSide.ServerMACKey, serverSide.ServerMACKey},
	}
	for i, p := range pairs {
		if !bytes.Equal(p[0], p[1]) {
			t.Fatalf("密钥段 %d 双侧不一致", i)
		}
	}
	_ = rsa.PublicKey{} // 保持 rsa import（testKey 已用）
}
