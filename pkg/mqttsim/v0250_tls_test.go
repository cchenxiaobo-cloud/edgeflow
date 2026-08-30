package mqttsim

// v0.25.0 TLS broker 测试：真实 TLS 握手 + CONNECT/CONNACK 全程走密钥
// 通道（客户端以测试内生成的自签证书为 RootCA），并验证 nil 配置 fail-fast。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"edgeflow/pkg/mqtt"
)

// writeTestCert 生成自签服务器证书（ECDSA P-256，IsCA 便于直接充当测试
// RootCA，SAN 含 127.0.0.1 与 localhost），返回 (certPEM, keyPEM)。
func writeTestCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "edgeflow-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("序列化私钥失败: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// TestNewBrokerTLSHandshake 验证 TLS broker 全链路：TLS 握手成功、
// CONNECT 经密钥通道到达 serve()、CONNACK 经密钥通道返回。
func TestNewBrokerTLSHandshake(t *testing.T) {
	certPEM, keyPEM := writeTestCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("加载证书对失败: %v", err)
	}
	b, err := NewBrokerTLS(&tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("NewBrokerTLS 失败: %v", err)
	}
	defer func() { _ = b.Close() }()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("RootCA 池解析失败")
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", b.Addr(),
		&tls.Config{RootCAs: pool})
	if err != nil {
		t.Fatalf("TLS 拨号失败: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := mqtt.EncodePacket(conn, &mqtt.Connect{ClientID: "tls-c1", KeepAlive: 60, CleanSession: true}); err != nil {
		t.Fatalf("编码 CONNECT 失败: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	pkt, err := mqtt.DecodePacket(conn)
	if err != nil {
		t.Fatalf("解码 CONNACK 失败: %v", err)
	}
	ack, ok := pkt.(*mqtt.Connack)
	if !ok {
		t.Fatalf("收到的报文类型 = %T，期望 *mqtt.Connack", pkt)
	}
	if ack.ReturnCode != 0 {
		t.Fatalf("CONNACK ReturnCode = %d，期望 0（接受连接）", ack.ReturnCode)
	}
	if n := b.PingCount(); n != 0 {
		t.Fatalf("PingCount = %d，期望 0（新 broker 无 ping）", n)
	}
}

// TestNewBrokerTLSNilConfig 验证 nil TLS 配置 fail-fast（不留半初始化
// listener）。
func TestNewBrokerTLSNilConfig(t *testing.T) {
	if _, err := NewBrokerTLS(nil); err == nil {
		t.Fatal("NewBrokerTLS(nil) = nil error，期望报错")
	}
}
