package mqtt

// v0260_mtls_test.go — MQTT Mapper mTLS（双向认证，v0.26.0）。
//
// 策略：mqttsim.NewBrokerTLS + ClientAuth: RequireAndVerifyClientCert
// （严格模式：客户端证书必须由测试 CA 签发），mapper 侧
// EDGEFLOW_MQTT_TLS_CERT / EDGEFLOW_MQTT_TLS_KEY 注入证书对：
//   - 全链路：Start → mTLS 握手（双向验证）→ 订阅 → 上报可见；
//   - fail-fast：证书对只配一半、文件缺失、内容非法 → connect() 立即报错；
//   - 拒绝：服务端要求客户端证书而 mapper 未提供 → 握手失败。

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/mqttsim"
)

// v0260GenCerts 生成一套双端证书：服务器证书（IsCA 充当测试 RootCA，SAN
// 含 127.0.0.1/localhost）与由它签发的客户端证书（ExtKeyUsage ClientAuth）。
// 全部 ECDSA P-256。
func v0260GenCerts(t *testing.T) (srvCertPEM, srvKeyPEM, cliCertPEM, cliKeyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 私钥失败: %v", err)
	}
	caTmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "edgeflow-mtls-test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		// 注：CA 不设 ExtKeyUsage 约束——Go x509 的 EKU 嵌套检查要求 CA 的
		// EKU 覆盖叶子用途；若限定 ServerAuth 会导致 ClientAuth 叶子链校验失败。
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	caDER, err := x509.CreateCertificate(rand.Reader, &caTmpl, &caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("生成 CA 证书失败: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("解析 CA 证书失败: %v", err)
	}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成服务器私钥失败: %v", err)
	}
	srvTmpl := x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, &srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("签发服务器证书失败: %v", err)
	}

	cliKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成客户端私钥失败: %v", err)
	}
	cliTmpl := x509.Certificate{
		SerialNumber:          big.NewInt(4),
		Subject:               pkix.Name{CommonName: "edgeflow-mqtt-client"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	cliDER, err := x509.CreateCertificate(rand.Reader, &cliTmpl, caCert, &cliKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("签发客户端证书失败: %v", err)
	}

	cliKeyDER, err := x509.MarshalECPrivateKey(cliKey)
	if err != nil {
		t.Fatalf("序列化客户端私钥失败: %v", err)
	}
	srvKeyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatalf("序列化服务器私钥失败: %v", err)
	}
	// 服务器证书 PEM = 专用叶子 + CA 链拼接（X509KeyPair 支持多 CERTIFICATE
	// 段，等价于 inline 探针已验证通过的 [leaf, CA] 链形态）。
	srvChain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	return srvChain,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: srvKeyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cliDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: cliKeyDER})
}

// startMTLSBroker 起一个要求并校验客户端证书的 TLS sim broker，写出
// (ca.pem, client-cert.pem, client-key.pem) 三件套并清空相关 env。
func startMTLSBroker(t *testing.T) (*mqttsim.Broker, string, string, string) {
	t.Helper()
	for _, k := range []string{EnvTopics, EnvCmdTopic, EnvDeviceName, EnvNamespace, EnvTLSInsecure, EnvTLSCert, EnvTLSKey, EnvTLSCA} {
		t.Setenv(k, "")
	}
	srvCertPEM, srvKeyPEM, cliCertPEM, cliKeyPEM := v0260GenCerts(t)
	srvCert, err := tls.X509KeyPair(srvCertPEM, srvKeyPEM)
	if err != nil {
		t.Fatalf("加载服务器证书对失败: %v", err)
	}
	cliPool := x509.NewCertPool()
	if !cliPool.AppendCertsFromPEM(srvCertPEM) {
		t.Fatalf("构建 ClientCAs 池失败")
	}
	sim, err := mqttsim.NewBrokerTLS(&tls.Config{
		Certificates: []tls.Certificate{srvCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    cliPool,
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("起 mTLS sim broker 失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	certPath := filepath.Join(dir, "client-cert.pem")
	keyPath := filepath.Join(dir, "client-key.pem")
	for path, data := range map[string][]byte{caPath: srvCertPEM, certPath: cliCertPEM, keyPath: cliKeyPEM} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("写 %s 失败: %v", path, err)
		}
	}
	return sim, caPath, certPath, keyPath
}

// TestV0260MapperMTLSConnectReport 全链路：mapper 持客户端证书对通过
// RequireAndVerifyClientCert 校验，订阅/上报在 mTLS 通道上闭环。
func TestV0260MapperMTLSConnectReport(t *testing.T) {
	sim, caPath, certPath, keyPath := startMTLSBroker(t)
	t.Setenv(EnvTLSCA, caPath)
	t.Setenv(EnvTLSCert, certPath)
	t.Setenv(EnvTLSKey, keyPath)

	m := New(sim.Addr())
	if m.tlsCAPath == "" || m.tlsCertPath == "" || m.tlsKeyPath == "" {
		t.Fatalf("DEBUG 探针: ca=%q cert=%q key=%q", m.tlsCAPath, m.tlsCertPath, m.tlsKeyPath)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	waitFor(t, "mapper 连上 mTLS broker", func() bool { return m.clientSnapshot() != nil })

	if err := sim.Publish("devices/mtls-dev/state", []byte(`{"humidity":41.0}`)); err != nil {
		t.Fatalf("sim.Publish 失败: %v", err)
	}
	waitFor(t, "mTLS 链路上报进入快照", func() bool {
		props, err := m.Collect()
		if err != nil {
			return false
		}
		v, ok := props["humidity"]
		return ok && v == 41.0
	})
}

// TestV0260MapperMTLSFailFast 证书对配置不完整/非法时 connect() 立即报错
// （确定性断言，不经 supervise 异步重试；Dial 前失败用占位地址）。
func TestV0260MapperMTLSFailFast(t *testing.T) {
	sim, caPath, certPath, keyPath := startMTLSBroker(t)
	_ = sim
	badPEM := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badPEM, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("写非法 PEM 失败: %v", err)
	}
	cases := []struct {
		name       string
		cert, key  string
		wantSubstr string
	}{
		{"只配证书缺私钥", certPath, "", "成对提供"},
		{"只配私钥缺证书", "", keyPath, "成对提供"},
		{"证书文件不存在", filepath.Join(t.TempDir(), "nope.pem"), keyPath, "读取 MQTT 客户端证书"},
		{"私钥文件不存在", certPath, filepath.Join(t.TempDir(), "nope.key"), "读取 MQTT 客户端私钥"},
		{"证书内容非法", badPEM, keyPath, "证书对解析失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvTLSCA, caPath) // CA 有效：让失败精确落在证书对逻辑上
			t.Setenv(EnvTLSCert, tc.cert)
			t.Setenv(EnvTLSKey, tc.key)
			m := New("127.0.0.1:1") // 不会真的拨号：证书对校验先于 Dial
			_, err := m.connect()
			if err == nil {
				t.Fatalf("%s: connect() = nil error，期望含 %q 的错误", tc.name, tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("%s: connect() 错误 = %v，期望含 %q", tc.name, err, tc.wantSubstr)
			}
		})
	}
}

// TestV0260MapperMTLSRejectsNoCert 服务端要求客户端证书而 mapper 未提供
// 时，TLS 握手失败，connect() 报拨号错误（不静默降级）。
func TestV0260MapperMTLSRejectsNoCert(t *testing.T) {
	sim, caPath, _, _ := startMTLSBroker(t)
	t.Setenv(EnvTLSCA, caPath) // 只带 CA，不带客户端证书对
	m := New(sim.Addr())
	if _, err := m.connect(); err == nil {
		t.Fatal("无客户端证书连接要求 mTLS 的 broker：期望握手失败，got nil error")
	}
}
