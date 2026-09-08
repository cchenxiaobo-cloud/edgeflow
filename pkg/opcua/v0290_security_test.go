// v0290_security_test.go — v0.29.0 MSG/CLO 对称覆盖与换钥（package opcua 内部单测）。
//
// 覆盖：
//  1. SealMSGFrame/OpenMSGFrame 往返（双方向）+ msgType/channelID 绑定
//  2. 篡改（密文 1 bit / 尾足迹 1 bit / 截断 / 方向密钥错用 / nil 密钥）
//  3. §6.7.4 尾垫长度律（明文 0..47B 全覆盖往返 + 密文块对齐断言）
//  4. swapKeys 原子换组语义（cur 就位 / prev 回退保留）
//  5. Renew 拒绝 None 通道（显式报错，不静默）

package opcua

import (
	"bytes"
	"testing"
)

func v0290TestKeys(seed byte) *DerivedKeys {
	cn := bytes.Repeat([]byte{seed}, 16)
	sn := bytes.Repeat([]byte{seed + 0x40}, 16)
	return DeriveKeys(cn, sn, []byte("v0290-client-cert-DER"), []byte("v0290-server-cert-DER"))
}

// TestV0290SealOpenMSGRoundTrip：密封→解封往返（客户端/服务端双方向），
// 且 HMAC 输入绑定 msgType 与 channelID（错用即失败）。
func TestV0290SealOpenMSGRoundTrip(t *testing.T) {
	keys := v0290TestKeys(0xA1)
	plain := []byte("sequence-header-and-body-payload-0290")

	frame, err := SealMSGFrame(MsgSecureMessage, 7, 9, keys, true, plain)
	if err != nil {
		t.Fatalf("客户端方向密封: %v", err)
	}
	tok, got, err := OpenMSGFrame(MsgSecureMessage, 7, frame[HeaderSize:], keys, true)
	if err != nil {
		t.Fatalf("客户端方向解封: %v", err)
	}
	if tok != 9 || !bytes.Equal(got, plain) {
		t.Fatalf("往返不一致: tok=%d plain=%q", tok, got)
	}

	frameS, err := SealMSGFrame(MsgSecureMessage, 7, 10, keys, false, plain)
	if err != nil {
		t.Fatalf("服务端方向密封: %v", err)
	}
	tokS, gotS, err := OpenMSGFrame(MsgSecureMessage, 7, frameS[HeaderSize:], keys, false)
	if err != nil {
		t.Fatalf("服务端方向解封: %v", err)
	}
	if tokS != 10 || !bytes.Equal(gotS, plain) {
		t.Fatalf("服务端方向往返不一致: tok=%d", tokS)
	}

	if _, _, err := OpenMSGFrame(MsgCloseSecureChannel, 7, frame[HeaderSize:], keys, true); err == nil {
		t.Fatal("msgType 不符应解封失败（HMAC 输入绑定帧型）")
	}
	if _, _, err := OpenMSGFrame(MsgSecureMessage, 8, frame[HeaderSize:], keys, true); err == nil {
		t.Fatal("channelID 不符应解封失败（HMAC 输入绑定通道）")
	}
}

// TestV0290SealOpenMSGTamper：任何比特篡改/截断/方向错用都拒绝。
func TestV0290SealOpenMSGTamper(t *testing.T) {
	keys := v0290TestKeys(0xB2)
	plain := []byte("tamper-target-body")
	frame, err := SealMSGFrame(MsgSecureMessage, 3, 4, keys, true, plain)
	if err != nil {
		t.Fatalf("密封: %v", err)
	}

	ctFlip := append([]byte{}, frame...)
	ctFlip[HeaderSize+4+3] ^= 0xFF // 密文区 1 bit
	if _, _, err := OpenMSGFrame(MsgSecureMessage, 3, ctFlip[HeaderSize:], keys, true); err == nil {
		t.Fatal("密文篡改应解封失败")
	}

	macFlip := append([]byte{}, frame...)
	macFlip[len(macFlip)-1] ^= 0xFF // 尾足迹 1 bit
	if _, _, err := OpenMSGFrame(MsgSecureMessage, 3, macFlip[HeaderSize:], keys, true); err == nil {
		t.Fatal("足迹篡改应解封失败")
	}

	if _, _, err := OpenMSGFrame(MsgSecureMessage, 3, frame[HeaderSize:len(frame)-3], keys, true); err == nil {
		t.Fatal("截断帧应解封失败")
	}

	// 方向密钥错用：客户端密封的帧用服务端方向密钥解。
	if _, _, err := OpenMSGFrame(MsgSecureMessage, 3, frame[HeaderSize:], keys, false); err == nil {
		t.Fatal("方向密钥错用应解封失败")
	}

	if _, err := SealMSGFrame(MsgSecureMessage, 3, 4, nil, true, plain); err == nil {
		t.Fatal("nil 密钥组应密封失败")
	}
}

// TestV0290MSGPaddingLengths：尾垫长度律——密文恒块对齐，往返逐字恢复。
func TestV0290MSGPaddingLengths(t *testing.T) {
	keys := v0290TestKeys(0xC3)
	for size := 0; size <= 47; size++ {
		plain := bytes.Repeat([]byte{0xAB}, size)
		frame, err := SealMSGFrame(MsgSecureMessage, 1, 2, keys, true, plain)
		if err != nil {
			t.Fatalf("size=%d 密封: %v", size, err)
		}
		ctLen := len(frame) - HeaderSize - 4 - 20
		if ctLen <= 0 || ctLen%16 != 0 {
			t.Fatalf("size=%d 密文长度 %d 非块对齐", size, ctLen)
		}
		_, got, err := OpenMSGFrame(MsgSecureMessage, 1, frame[HeaderSize:], keys, true)
		if err != nil {
			t.Fatalf("size=%d 解封: %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("size=%d 往返不一致", size)
		}
	}
}

// TestV0290SwapKeysFallback：swapKeys 原子换组——新组就位、旧组转 prev。
func TestV0290SwapKeysFallback(t *testing.T) {
	sc := &SecureChannel{}
	k1 := v0290TestKeys(0x01)
	k2 := v0290TestKeys(0x02)
	sc.keysMu.Lock()
	sc.cur = keySet{keys: k1, tokenID: 1}
	sc.keysMu.Unlock()

	sc.swapKeys(k2, 2)

	cur := sc.curKeys()
	if cur.keys != k2 || cur.tokenID != 2 {
		t.Fatal("换钥后 cur 应为新组/新令牌")
	}
	if sc.prev == nil || sc.prev.keys != k1 || sc.prev.tokenID != 1 {
		t.Fatal("换钥后 prev 应保留旧组作在途回退")
	}
	if !sc.encrypted() {
		t.Fatal("换钥后通道应为加密态")
	}
}

// TestV0290RenewRejectsNoneChannel：None 通道 Renew 显式报错。
func TestV0290RenewRejectsNoneChannel(t *testing.T) {
	c := &Client{sc: &SecureChannel{}, timeout: 1e9}
	if err := c.Renew(1e9); err == nil {
		t.Fatal("None 通道 Renew 应显式拒绝")
	}
}
