// v0280_security_e2e_test.go — v0.28.0 安全策略端到端（package opcua_test，
// 可安全导入 opcuasim；包内测试不可导入以避免 import cycle）。
//
// 覆盖：
//  1. 模拟器对 Basic256Sha256 OPN 明确拒绝（ERR Bad_SecurityPolicyRejected），
//     不静默降级为 None —— 服务器侧策略守卫语义。
//  2. 同一模拟器上 None 路径照常工作（OpenWithOptions 零值 = Open 等价）。

package opcua_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/opcua"
	"edgeflow/pkg/opcuasim"
)

func v0280SelfSigned(t *testing.T) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 密钥: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2802),
		Subject:      pkix.Name{CommonName: "v0280-e2e"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("自签证书: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析证书: %v", err)
	}
	return key, cert
}

// TestV0280SimRejectsBasic256Sha256：sim 仅支持 None，B256 请求被显式拒绝。
func TestV0280SimRejectsBasic256Sha256(t *testing.T) {
	sim := opcuasim.New("127.0.0.1:0")
	if err := sim.Start(); err != nil {
		t.Fatalf("sim 启动: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	endpoint := "opc.tcp://" + sim.Addr()

	key, cert := v0280SelfSigned(t)
	opts := opcua.OpenSecureChannelOptions{
		SecurityPolicyURI: opcua.SecurityPolicyBasic256Sha256URI,
		ClientCert:        cert,
		ClientKey:         key,
		ServerCert:        cert,
	}
	_, err := opcua.OpenWithOptions(endpoint, 3*time.Second, opts)
	if err == nil {
		t.Fatal("sim 不支持 Basic256Sha256，OPN 应失败")
	}
	if !strings.Contains(err.Error(), "Bad_SecurityPolicyRejected") {
		t.Fatalf("应返回 Bad_SecurityPolicyRejected，得到: %v", err)
	}
}

// TestV0280SimNoneStillWorks：零值 opts 的 OpenWithOptions 与 None 语义一致。
func TestV0280SimNoneStillWorks(t *testing.T) {
	sim := opcuasim.New("127.0.0.1:0")
	if err := sim.Start(); err != nil {
		t.Fatalf("sim 启动: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	endpoint := "opc.tcp://" + sim.Addr()

	c, err := opcua.OpenWithOptions(endpoint, 3*time.Second, opcua.OpenSecureChannelOptions{})
	if err != nil {
		t.Fatalf("None 路径应照常工作: %v", err)
	}
	defer c.Close()
	_ = c.PubAck() // 通道就绪的轻量探针（无订阅时也返回无错）
}
