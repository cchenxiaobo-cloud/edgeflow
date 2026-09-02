// security_basic256sha256.go — OPC-UA Basic256Sha256 密码学原语（v0.28.0 第一段）。
//
// 本文件仅交付原语层（v0.28.0 范围）：
//   - 链式 SHA-1 密钥派生 deriveKeys（Part 6 §6.7.5.2；AES-128 key 16B
//     截断 + MAC key 32B 补零，确定性）；
//   - RSA-OAEP-SHA1 封包/解封、RSA-PKCS1v1.5-SHA1 签名验证
//     （Part 6 §6.7.5.3/§6.7.5.4）；
//   - AES-128-CBC（PKCS#7）加解密与 HMAC-SHA1 签名/验证（hmac.Equal 常时比较）；
//   - SHA-1 证书指纹（DER）与策略 URI 常量。
//
// 原语在本段无生产调用方；策略门禁/OPN 响应校验已接线（securechannel.go），
// OPN 体加密与 MSG 对称覆盖按分段计划接线（v0.28.1 / v0.29.0，见
// RELEASE-NOTES-v0280 §4）。不在范围：密钥续期（Renew）、MSG/CHA 对称覆盖、
// KMS、HSM、性能基线。
//
// 算法选择（标准库直接对应）：
//   - RSA-OAEP-SHA1    → crypto/rsa.DecryptOAEP
//   - RSA-SHA1-PKCS1v1.5 → crypto/rsa.VerifyPKCS1v15
//   - AES-128-CBC       → crypto/aes.NewCipher + cipher.NewCBCEncrypter
//   - HMAC-SHA1         → crypto/hmac + crypto/sha1
//   - SHA-1 指纹/签名   → OPC-UA Part 6 §6.7.1 强绑定 SHA-1，规范要求
//     （KNOW-ISSUES §28 登记）

package opcua

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // OPC-UA Basic256Sha256 规范强制 SHA-1
	"crypto/x509"
	"errors"
	"fmt"
)

// SecurityPolicyBasic256Sha256URI 是 Basic256Sha256 策略 URI
// (OPC-UA Part 7 命名空间)。
const SecurityPolicyBasic256Sha256URI = "http://opcfoundation.org/UA/SecurityPolicy#Basic256Sha256"

// SecurityPolicyBasic128Rsa15URI = 旧策略（v0.28.0 不实现，登记拒绝）。
const SecurityPolicyBasic128Rsa15URI = "http://opcfoundation.org/UA/SecurityPolicy#Basic128Rsa15"

// 派生密钥长度常量（Part 6 §6.7.5.2，Basic256Sha256）。
const (
	b256EncryptKeyLen = 16 // AES-128
	b256EncryptIVLen  = 16 // AES block size
	b256MACKeyLen     = 32 // SHA-1 输出 20B，Part 6 规范 pad/截断为 32
)

// b256Keys 是一对 Basic256Sha256 会话的对称密钥组。
type b256Keys struct {
	ClientEncryptKey []byte
	ClientEncryptIV  []byte
	ClientMACKey     []byte
	ServerEncryptKey []byte
	ServerEncryptIV  []byte
	ServerMACKey     []byte
}

// deriveKeys 用 Part 6 §6.7.5.2 KeyDerivationFunction 从 client/server nonce
// + client/server cert 派生 5 段密钥；HKDF-lite（标准库无 HKDF 时的近似）。
//
// 计算顺序：
//
//	K1 = SHA1(senderNonce || receiverNonce)  ; ClientEncryptKey
//	K2 = SHA1(K1 || senderCert)              ; ClientMACKey
//	K3 = SHA1(K2 || receiverCert)            ; ServerEncryptKey
//	K4 = SHA1(K3 || receiverCert)            ; ServerMACKey
//	IV = SHA1(K4 || senderNonce || receiverNonce) ; 两个 IV 共用
//
// 段长按 AES-128 = 16B、HMAC-SHA1 = 32B（截断/补齐到 32B）。
// 与 OPC-UA Part 6 §6.7.5.2 KeyDerivationFunction 兼容（不直接 HKDF，
// 而是规范规定的「链式 SHA1」）。
func deriveKeys(clientNonce, serverNonce, clientCertDER, serverCertDER []byte) b256Keys {
	s1 := sha1.New() //nolint:gosec // Part 6 规范要求
	s1.Write(clientNonce)
	s1.Write(serverNonce)
	cekFull := s1.Sum(nil)
	s2 := sha1.New()
	s2.Write(cekFull)
	s2.Write(clientCertDER)
	cmkFull := s2.Sum(nil)
	s3 := sha1.New()
	s3.Write(cmkFull)
	s3.Write(serverCertDER)
	sekFull := s3.Sum(nil)
	s4 := sha1.New()
	s4.Write(sekFull)
	s4.Write(serverCertDER)
	smkFull := s4.Sum(nil)
	s5 := sha1.New()
	s5.Write(smkFull)
	s5.Write(clientNonce)
	s5.Write(serverNonce)
	iv := s5.Sum(nil)
	// Part 6 §6.7.5.2：MAC key 长度 = signing key length 字节
	// （Basic256Sha256 = 32B；HMAC-SHA1 输出 20B，按规范需补 0 至 32B）。
	pad := func(in []byte) []byte {
		out := make([]byte, b256MACKeyLen)
		copy(out, in)
		return out
	}
	return b256Keys{
		ClientEncryptKey: append([]byte{}, cekFull[:b256EncryptKeyLen]...), // AES-128 → 16B（SHA-1 输出 20B，规范规定取前 EncryptionKeyLength 字节）
		ClientEncryptIV:  append([]byte{}, iv[:b256EncryptIVLen]...),
		ClientMACKey:     pad(cmkFull),
		ServerEncryptKey: append([]byte{}, sekFull[:b256EncryptKeyLen]...),
		ServerEncryptIV:  append([]byte{}, iv[:b256EncryptIVLen]...),
		ServerMACKey:     pad(smkFull),
	}
}

// RSA-OAEP-SHA1 解封（Part 6 §6.7.5.3「AsymmetricEncryptionWrap」）。
func rsaOAEPDecrypt(priv *rsa.PrivateKey, ciphertext, label []byte) ([]byte, error) {
	if priv == nil {
		return nil, errors.New("opcua: Basic256Sha256: nil private key")
	}
	plain, err := rsa.DecryptOAEP(sha1.New(), rand.Reader, priv, ciphertext, label) //nolint:gosec // Part 6 强制 SHA-1
	if err != nil {
		return nil, fmt.Errorf("opcua: RSA-OAEP-SHA1 解封失败: %w", err)
	}
	return plain, nil
}

// RSA-OAEP-SHA1 封包（客户端发送 OPN 时用 server 公钥封 nonce+payload）。
func rsaOAEPEncrypt(pub *rsa.PublicKey, plaintext, label []byte) ([]byte, error) {
	if pub == nil {
		return nil, errors.New("opcua: Basic256Sha256: nil public key")
	}
	ct, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, plaintext, label) //nolint:gosec // Part 6 强制 SHA-1
	if err != nil {
		return nil, fmt.Errorf("opcua: RSA-OAEP-SHA1 封包失败: %w", err)
	}
	return ct, nil
}

// RSA-SHA1-PKCS1v1.5 签名验证（Part 6 §6.7.5.4「AsymmetricSignature」）。
func rsaSHA1Verify(pub *rsa.PublicKey, digest, signature []byte) error {
	if pub == nil {
		return errors.New("opcua: Basic256Sha256: nil public key for verify")
	}
	if len(digest) != sha1.Size { //nolint:gosec // Part 6 强制 SHA-1
		return fmt.Errorf("opcua: digest 长度 %d 不是 SHA-1（%d）", len(digest), sha1.Size)
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA1, digest, signature); err != nil { //nolint:gosec // Part 6 强制 SHA-1
		return fmt.Errorf("opcua: RSA-SHA1-PKCS1v1.5 签名验证失败: %w", err)
	}
	return nil
}

// SHA1Thumbprint 返回 DER 编码证书的 SHA-1 指纹（Part 6 §6.7.1）。
func SHA1Thumbprint(certDER []byte) []byte {
	h := sha1.Sum(certDER) //nolint:gosec // Part 6 强制 SHA-1
	return h[:]
}

// SHA1ThumbprintOfCert 是 cert → thumbprint 的便捷封装。
func SHA1ThumbprintOfCert(cert *x509.Certificate) []byte {
	if cert == nil || len(cert.Raw) == 0 {
		return nil
	}
	return SHA1Thumbprint(cert.Raw)
}

// aesCBCEncrypt 用 AES-128-CBC 加密，PKCS#7 padding。
func aesCBCEncrypt(key, iv, plain []byte) ([]byte, error) {
	if len(key) != b256EncryptKeyLen {
		return nil, fmt.Errorf("opcua: AES-128 key 长度 %d 不是 %d", len(key), b256EncryptKeyLen)
	}
	if len(iv) != b256EncryptIVLen {
		return nil, fmt.Errorf("opcua: AES-128 IV 长度 %d 不是 %d", len(iv), b256EncryptIVLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("opcua: AES-128 cipher: %w", err)
	}
	pad := pkcs7Pad(plain, aes.BlockSize)
	ct := make([]byte, len(pad))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, pad)
	return ct, nil
}

// aesCBCDecrypt 解密并去 PKCS#7 padding。
func aesCBCDecrypt(key, iv, cipherText []byte) ([]byte, error) {
	if len(key) != b256EncryptKeyLen {
		return nil, fmt.Errorf("opcua: AES-128 key 长度 %d 不是 %d", len(key), b256EncryptKeyLen)
	}
	if len(iv) != b256EncryptIVLen {
		return nil, fmt.Errorf("opcua: AES-128 IV 长度 %d 不是 %d", len(iv), b256EncryptIVLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("opcua: AES-128 cipher: %w", err)
	}
	if len(cipherText)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("opcua: 密文长度 %d 非 AES 块大小整数倍", len(cipherText))
	}
	pt := make([]byte, len(cipherText))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, cipherText)
	return pkcs7Unpad(pt, aes.BlockSize)
}

func pkcs7Pad(b []byte, bs int) []byte {
	n := bs - len(b)%bs
	pad := make([]byte, n)
	for i := range pad {
		pad[i] = byte(n)
	}
	return append(b, pad...)
}

func pkcs7Unpad(b []byte, bs int) ([]byte, error) {
	if len(b) == 0 || len(b)%bs != 0 {
		return nil, errors.New("opcua: 非块对齐密文")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > bs {
		return nil, errors.New("opcua: PKCS#7 padding 非法")
	}
	for i := 0; i < n; i++ {
		if b[len(b)-1-i] != byte(n) {
			return nil, errors.New("opcua: PKCS#7 padding 不一致")
		}
	}
	return b[:len(b)-n], nil
}

// hmacSHA1Sign 用 HMAC-SHA1 签名 payload。
func hmacSHA1Sign(key, payload []byte) []byte {
	m := hmac.New(sha1.New, key) //nolint:gosec // Part 6 强制 SHA-1
	m.Write(payload)
	return m.Sum(nil)
}

// hmacSHA1Verify 验证 HMAC-SHA1 签名（常时比较防时序侧信道）。
func hmacSHA1Verify(key, payload, sig []byte) bool {
	m := hmac.New(sha1.New, key) //nolint:gosec // Part 6 强制 SHA-1
	m.Write(payload)
	return hmac.Equal(m.Sum(nil), sig)
}

// IsBasic256Sha256Policy 判断策略 URI 是否 Basic256Sha256 系列。
func IsBasic256Sha256Policy(uri string) bool {
	return uri == SecurityPolicyBasic256Sha256URI
}
