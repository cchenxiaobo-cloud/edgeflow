// v0280_security_test.go — v0.28.0 Basic256Sha256 安全策略回归（v0280_*）。
//
// 覆盖维度：
//   1) 零依赖：纯标准库实现；无 OPC-UA 之外的第三方库。
//   2) 默认行为：OpenSecureChannel 零值 opts 时与 v0.27.0 字节一致。
//   3) 密码学基础：密钥派生（Part 6 §6.7.5.2）+ AES-128-CBC roundtrip +
//      RSA-OAEP-SHA1 roundtrip + HMAC-SHA1 签名/验证 + SHA-1 指纹。
//   4) 策略拒绝：Basic128Rsa15 / Basic256 等未实现策略 → 拒绝；缺证书字段
//      的 Basic256Sha256 也拒绝（半加密状态 PROT-1）。
//   5) 派生确定性：相同输入产生相同密钥（可用于 v0.28.1 跨进程握手测试）。

package opcua

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // Part 6 强制 SHA-1
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"
)

// TestV0280SecurityPolicyNoneZeroOpts：零值 opts 接受 NoneURI 且不要求证书。
func TestV0280SecurityPolicyNoneZeroOpts(t *testing.T) {
	opts := OpenSecureChannelOptions{}
	uri, err := opts.validateSecurityOptions()
	if err != nil {
		t.Fatalf("零值 opts 应接受 NoneURI：%v", err)
	}
	if uri != SecurityPolicyNoneURI {
		t.Fatalf("零值 opts 应派生 NoneURI，得到 %q", uri)
	}
}

// TestV0280SecurityPolicyBasic256Sha256RequireCerts：Basic256Sha256
// 缺证书时拒绝（PROT-1）。
func TestV0280SecurityPolicyBasic256Sha256RequireCerts(t *testing.T) {
	cases := []OpenSecureChannelOptions{
		{SecurityPolicyURI: SecurityPolicyBasic256Sha256URI},                          // 全空
		{SecurityPolicyURI: SecurityPolicyBasic256Sha256URI, ClientCert: testCert(t)}, // 只 client
		{SecurityPolicyURI: SecurityPolicyBasic256Sha256URI, ServerCert: testCert(t)}, // 只 server
	}
	for i, opts := range cases {
		if _, err := opts.validateSecurityOptions(); err == nil {
			t.Fatalf("case %d 应拒绝缺失证书", i)
		}
	}
}

// TestV0280SecurityPolicyBasic256Sha256AcceptsFullSet：完整三字段接受。
func TestV0280SecurityPolicyBasic256Sha256AcceptsFullSet(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成测试 RSA 密钥：%v", err)
	}
	cert := testCert(t)
	opts := OpenSecureChannelOptions{
		SecurityPolicyURI: SecurityPolicyBasic256Sha256URI,
		ClientCert:        cert,
		ClientKey:         key,
		ServerCert:        cert,
	}
	uri, err := opts.validateSecurityOptions()
	if err != nil {
		t.Fatalf("完整三字段应接受：%v", err)
	}
	if uri != SecurityPolicyBasic256Sha256URI {
		t.Fatalf("应返回 Basic256Sha256URI，得到 %q", uri)
	}
}

// TestV0280SecurityPolicyBasic128Rsa15Rejected：未实现策略显式拒绝。
func TestV0280SecurityPolicyBasic128Rsa15Rejected(t *testing.T) {
	opts := OpenSecureChannelOptions{
		SecurityPolicyURI: SecurityPolicyBasic128Rsa15URI,
		ClientCert:        testCert(t),
		ClientKey:         testKey(t),
		ServerCert:        testCert(t),
	}
	if _, err := opts.validateSecurityOptions(); err == nil {
		t.Fatal("Basic128Rsa15 应拒绝（v0.28.0 不实现）")
	}
}

// TestV0280SHA1ThumbprintIsCanonical：SHA-1 指纹对相同输入稳定。
func TestV0280SHA1ThumbprintIsCanonical(t *testing.T) {
	cert := testCert(t)
	a := SHA1Thumbprint(cert.Raw)
	b := SHA1Thumbprint(cert.Raw)
	if !bytes.Equal(a, b) {
		t.Fatal("SHA-1 指纹对相同 DER 应稳定")
	}
	if len(a) != sha1.Size {
		t.Fatalf("指纹长度 %d 不是 SHA-1 (%d)", len(a), sha1.Size)
	}
}

// TestV0280KeyDerivationDeterminism：相同 nonce+cert 输入应派生相同密钥
// （Part 6 §6.7.5.2 链式 SHA-1，零随机性）。
func TestV0280KeyDerivationDeterminism(t *testing.T) {
	c := testCert(t)
	cn := bytes.Repeat([]byte{0x11}, 32)
	sn := bytes.Repeat([]byte{0x22}, 32)
	k1 := deriveKeys(cn, sn, c.Raw, c.Raw)
	k2 := deriveKeys(cn, sn, c.Raw, c.Raw)
	if !bytes.Equal(k1.ClientEncryptKey, k2.ClientEncryptKey) {
		t.Fatal("ClientEncryptKey 不稳定")
	}
	if !bytes.Equal(k1.ServerMACKey, k2.ServerMACKey) {
		t.Fatal("ServerMACKey 不稳定")
	}
	if len(k1.ClientEncryptKey) != 16 || len(k1.ServerEncryptKey) != 16 {
		t.Fatalf("AES key 长度应为 16，得到 %d/%d", len(k1.ClientEncryptKey), len(k1.ServerEncryptKey))
	}
	if len(k1.ClientMACKey) != 32 || len(k1.ServerMACKey) != 32 {
		t.Fatalf("HMAC-SHA1 padded key 长度应为 32，得到 %d/%d", len(k1.ClientMACKey), len(k1.ServerMACKey))
	}
}

// TestV0280AESCBCRoundTrip：AES-128-CBC encrypt+decrypt 还原原文。
func TestV0280AESCBCRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, 16)
	iv := bytes.Repeat([]byte{0x44}, 16)
	plain := []byte("opcua Basic256Sha256 v0.28.0 OPN test")
	ct, err := aesCBCEncrypt(key, iv, plain)
	if err != nil {
		t.Fatalf("encrypt：%v", err)
	}
	if bytes.Equal(ct, plain) {
		t.Fatal("密文不应与明文相等")
	}
	got, err := aesCBCDecrypt(key, iv, ct)
	if err != nil {
		t.Fatalf("decrypt：%v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip 不一致：want %q got %q", plain, got)
	}
}

// TestV0280HMACSHA1SignVerify：正确签名通过，篡改 1 字节拒绝。
func TestV0280HMACSHA1SignVerify(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	payload := []byte("OPC-UA Part 6 §6.7.5 SymmetricSignature")
	sig := hmacSHA1Sign(key, payload)
	if len(sig) != sha1.Size {
		t.Fatalf("HMAC-SHA1 输出长度 %d 不是 %d", len(sig), sha1.Size)
	}
	if !hmacSHA1Verify(key, payload, sig) {
		t.Fatal("合法签名应验证通过")
	}
	tampered := append([]byte{}, payload...)
	tampered[0] ^= 0x80
	if hmacSHA1Verify(key, tampered, sig) {
		t.Fatal("篡改负载应拒绝")
	}
	wrongKey := bytes.Repeat([]byte{0x66}, 32)
	if hmacSHA1Verify(wrongKey, payload, sig) {
		t.Fatal("错误密钥应拒绝")
	}
}

// TestV0280IsBasic256Sha256Policy：URI 识别 helper 正确。
func TestV0280IsBasic256Sha256Policy(t *testing.T) {
	if !IsBasic256Sha256Policy(SecurityPolicyBasic256Sha256URI) {
		t.Fatal("应识别 Basic256Sha256 URI")
	}
	if IsBasic256Sha256Policy(SecurityPolicyNoneURI) {
		t.Fatal("None URI 不应被识别为 Basic256Sha256")
	}
}

// TestV0280AESCBCBadKeyLength：非 16B key 拒绝。
func TestV0280AESCBCBadKeyLength(t *testing.T) {
	if _, err := aesCBCEncrypt(make([]byte, 8), make([]byte, 16), []byte("x")); err == nil {
		t.Fatal("短 key 应拒绝")
	}
}

// ---- 工具 ----

func testCert(t *testing.T) *x509.Certificate {
	t.Helper()
	c := &x509.Certificate{Raw: bytes.Repeat([]byte{0xab}, 256)}
	return c
}

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 key：%v", err)
	}
	return k
}

// TestV0280ValidateOPNResponseNone：None 响应校验保留 PRT-08 原语义。
func TestV0280ValidateOPNResponseNone(t *testing.T) {
	opts := OpenSecureChannelOptions{}
	if err := validateOPNResponseHeader(AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI}, SecurityPolicyNoneURI, opts); err != nil {
		t.Fatalf("合法 None 响应应通过：%v", err)
	}
	err := validateOPNResponseHeader(AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI, SenderCertificate: []byte{1}}, SecurityPolicyNoneURI, opts)
	if err == nil {
		t.Fatal("None 响应带证书字段应拒绝")
	}
	err = validateOPNResponseHeader(AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyBasic256Sha256URI}, SecurityPolicyNoneURI, opts)
	if err == nil {
		t.Fatal("None 协商下收到 Basic256Sha256 响应应拒绝")
	}
}

// TestV0280ValidateOPNResponseB256：Basic256Sha256 响应校验四分支。
func TestV0280ValidateOPNResponseB256(t *testing.T) {
	key := testKey(t)
	cert := realSelfSignedCert(t, key)
	opts := OpenSecureChannelOptions{SecurityPolicyURI: SecurityPolicyBasic256Sha256URI, ClientCert: cert, ClientKey: key, ServerCert: cert}
	good := AsymmetricSecurityHeader{
		SecurityPolicyURI:             SecurityPolicyBasic256Sha256URI,
		SenderCertificate:             cert.Raw,
		ReceiverCertificateThumbprint: SHA1ThumbprintOfCert(cert),
	}
	if err := validateOPNResponseHeader(good, SecurityPolicyBasic256Sha256URI, opts); err != nil {
		t.Fatalf("合法 B256 响应应通过：%v", err)
	}
	wrongPolicy := good
	wrongPolicy.SecurityPolicyURI = SecurityPolicyNoneURI
	if err := validateOPNResponseHeader(wrongPolicy, SecurityPolicyBasic256Sha256URI, opts); err == nil {
		t.Fatal("B256 协商下收到 None 响应应拒绝")
	}
	wrongSender := good
	wrongSender.SenderCertificate = append([]byte{}, cert.Raw...)
	wrongSender.SenderCertificate[0] ^= 0xFF
	if err := validateOPNResponseHeader(wrongSender, SecurityPolicyBasic256Sha256URI, opts); err == nil {
		t.Fatal("服务端证书与 pin 不符应拒绝")
	}
	wrongThumb := good
	wrongThumb.ReceiverCertificateThumbprint = make([]byte, 20)
	if err := validateOPNResponseHeader(wrongThumb, SecurityPolicyBasic256Sha256URI, opts); err == nil {
		t.Fatal("ReceiverCertificateThumbprint 不符应拒绝")
	}
}

// TestV0280OpenWithOptionsRejectsBeforeDial：未知策略在拨号前拒绝。
func TestV0280OpenWithOptionsRejectsBeforeDial(t *testing.T) {
	opts := OpenSecureChannelOptions{
		SecurityPolicyURI: SecurityPolicyBasic128Rsa15URI,
		ClientCert:        testCert(t),
		ClientKey:         testKey(t),
		ServerCert:        testCert(t),
	}
	_, err := OpenWithOptions("127.0.0.1:1", 50*time.Millisecond, opts)
	if err == nil {
		t.Fatal("Basic128Rsa15 应在拨号前拒绝")
	}
	if !strings.Contains(err.Error(), "不支持") {
		t.Fatalf("错误应说明策略不支持，得到：%v", err)
	}
}

// realSelfSignedCert 生成一张真实自签证书（DER），用于响应校验测试。
func realSelfSignedCert(t *testing.T, key *rsa.PrivateKey) *x509.Certificate {
	t.Helper()
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2801),
		Subject:      pkix.Name{CommonName: "v0280-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成自签证书：%v", err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析自签证书：%v", err)
	}
	return c
}

// TestV0280RSAOAEPRoundTrip：RSA-OAEP-SHA1 封包/解封 roundtrip + 篡改拒绝
// （v0.28.1 OPN 体加密的关键依赖，复核 P2-3 补测）。
func TestV0280RSAOAEPRoundTrip(t *testing.T) {
	key := testKey(t)
	plain := []byte("v0.28.1 OPN body crypto dependency")
	ct, err := rsaOAEPEncrypt(&key.PublicKey, plain, nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := rsaOAEPDecrypt(key, ct, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("RSA-OAEP roundtrip 不一致")
	}
	bad := append([]byte{}, ct...)
	bad[0] ^= 0x80
	if _, err := rsaOAEPDecrypt(key, bad, nil); err == nil {
		t.Fatal("篡改密文应解封失败")
	}
}

// TestV0280RSASHA1Verify：RSA-PKCS1v1.5-SHA1 签名验证通过 + 篡改摘要拒绝。
func TestV0280RSASHA1Verify(t *testing.T) {
	key := testKey(t)
	payload := []byte("opcua OPN asymmetric signature")
	digest := sha1.Sum(payload) //nolint:gosec // Part 6 强制 SHA-1
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := rsaSHA1Verify(&key.PublicKey, digest[:], sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
	badDigest := digest
	badDigest[0] ^= 0x80
	if err := rsaSHA1Verify(&key.PublicKey, badDigest[:], sig); err == nil {
		t.Fatal("篡改摘要应验证失败")
	}
}
