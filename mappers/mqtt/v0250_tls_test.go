package mqtt

// v0.25.0 MQTT Mapper TLS 单元测试（主线接管交付）。
//
// 策略：经 mqttsim.NewBrokerTLS（真实 TLS listener）+ mapper TLS env
// （EDGEFLOW_MQTT_TLS_CA / EDGEFLOW_MQTT_TLS_INSECURE）验证：
//   - CA 注入全链路：Start → TLS Dial → Subscribe → 设备上报 → 快照可见；
//   - CA fail-fast：文件不存在 / 非 PEM 内容，connect() 直接报错（确定性
//     断言，不经 supervise 异步重试）；
//   - INSECURE 逃生通道：无 CA 也能连自签 broker；
//   - 明文回归：两 env 均空时行为与 v0.24.0 逐字一致（由 v0240 全套覆盖，
//     此处只断言 env 未设时 TLSConfig 为 nil）。

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
	"testing"
	"time"

	"edgeflow/pkg/mqttsim"
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

// startTLSWithCert 起 TLS sim broker、写 CA 文件、设 env、Start Mapper 并
// 等连接建立。Cleanup（LIFO）：先 Stop Mapper 再关 broker。
func startTLSWithCert(t *testing.T) (*mqttsim.Broker, *MQTTMapper, string) {
	t.Helper()
	for _, k := range []string{EnvTopics, EnvCmdTopic, EnvDeviceName, EnvNamespace, EnvTLSInsecure} {
		t.Setenv(k, "")
	}
	certPEM, keyPEM := writeTestCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("加载证书对失败: %v", err)
	}
	sim, err := mqttsim.NewBrokerTLS(&tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("起 TLS sim broker 失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatalf("写 CA 文件失败: %v", err)
	}
	t.Setenv(EnvTLSCA, caPath)
	m := New(sim.Addr())
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	waitFor(t, "mapper 连上 TLS broker", func() bool { return m.clientSnapshot() != nil })
	return sim, m, caPath
}

// TestV0250MapperTLSConnectReport 全链路：TLS 连接建立 → sim 发布设备
// 上报 → 快照可见（CA 注入 + 加密通道 + 订阅分发全真实）。
func TestV0250MapperTLSConnectReport(t *testing.T) {
	sim, m, _ := startTLSWithCert(t)
	if err := sim.Publish("devices/tls-dev/state", []byte(`{"temperature":25.5}`)); err != nil {
		t.Fatalf("sim.Publish 失败: %v", err)
	}
	waitFor(t, "TLS 链路上报进入快照", func() bool {
		props, err := m.Collect()
		if err != nil {
			return false
		}
		v, ok := props["temperature"]
		return ok && v == 25.5
	})
}

// TestV0250MapperTLSCAFailFast 验证 CA 文件缺失 / 非 PEM 时 connect()
// 立即报错（fail-fast，不进入无限重试）。
func TestV0250MapperTLSCAFailFast(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
		wantErr string
	}{
		{"CA文件不存在", nil, "读取 MQTT TLS CA"},
		{"CA不是PEM", []byte("this is not a certificate"), "不是有效 PEM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{EnvTopics, EnvCmdTopic, EnvDeviceName, EnvNamespace, EnvTLSInsecure} {
				t.Setenv(k, "")
			}
			caPath := filepath.Join(t.TempDir(), "ca.pem")
			if tc.content != nil {
				if err := os.WriteFile(caPath, tc.content, 0o600); err != nil {
					t.Fatalf("写 CA 文件失败: %v", err)
				}
			}
			t.Setenv(EnvTLSCA, caPath)
			m := New("127.0.0.1:1") // 不会真的拨号：CA 校验先于 Dial 失败
			_, err := m.connect()
			if err == nil {
				t.Fatalf("%s: connect() = nil error，期望含 %q 的错误", tc.name, tc.wantErr)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s: connect() 错误 = %v，期望含 %q", tc.name, err, tc.wantErr)
			}
		})
	}
}

// contains 是 strings.Contains 的窄封装（错误信息断言用）。
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestV0250MapperTLSInsecure 验证无 CA + INSECURE=1 时仍能连自签 broker
// （开发/测试逃生通道），并断言 env 未设时 TLSConfig 保持 nil（明文回归）。
func TestV0250MapperTLSInsecure(t *testing.T) {
	for _, k := range []string{EnvTopics, EnvCmdTopic, EnvDeviceName, EnvNamespace, EnvTLSCA} {
		t.Setenv(k, "")
	}
	certPEM, keyPEM := writeTestCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("加载证书对失败: %v", err)
	}
	sim, err := mqttsim.NewBrokerTLS(&tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("起 TLS sim broker 失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })
	t.Setenv(EnvTLSInsecure, "1")
	m := New(sim.Addr())
	cl, err := m.connect()
	if err != nil {
		t.Fatalf("INSECURE 模式 connect() 失败: %v", err)
	}
	_ = cl.Close()

	// 明文回归：INSECURE 未设、无 CA → env 均空时 mapper 不注入 TLS。
	t.Setenv(EnvTLSInsecure, "")
	m2 := New("127.0.0.1:1")
	if m2.tlsCAPath != "" || m2.tlsInsecure {
		t.Fatal("env 全空时不应启用 TLS（tlsCAPath/tlsInsecure 应为零值）")
	}
}
