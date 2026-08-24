package etcdstore_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	"go.etcd.io/etcd/server/v3/embed"
)

// --- TLS 握手级测试（复核 🔴-B：设计 §2.3 mTLS 矩阵落地） ---
// 矩阵：① CA-only 服务器认证 → 探活成功；② mTLS 客户端证书 → 成功；
// ③ ClientCertAuth 开启但缺客户端证书 → 失败；④ 信任错误 CA → 失败。

func tlsTestFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("探测空闲端口失败: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeTestPEM(t *testing.T, path string, block *pem.Block) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o644); err != nil {
		t.Fatalf("写 %s 失败: %v", path, err)
	}
}

// tlsTestPKI 生成测试 PKI：CA（自签）+ 服务器证书（CA 签发，SAN=IP:127.0.0.1
// + DNS:localhost，ServerAuth）+ 客户端证书（CA 签发，ClientAuth）。
func tlsTestPKI(t *testing.T, dir string) (caCrt, serverCrt, serverKey, clientCrt, clientKey string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 密钥失败: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tls-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("签发 CA 失败: %v", err)
	}
	caCrt = filepath.Join(dir, "ca.crt")
	writeTestPEM(t, caCrt, &pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	issue := func(serial int64, cn string, eku x509.ExtKeyUsage, ip net.IP) (string, string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("生成密钥失败: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		}
		if ip != nil {
			tmpl.IPAddresses = []net.IP{ip}
			tmpl.DNSNames = []string{"localhost"}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caTmpl, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("签发 %s 失败: %v", cn, err)
		}
		crt, keyPath := filepath.Join(dir, cn+".crt"), filepath.Join(dir, cn+".key")
		writeTestPEM(t, crt, &pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("编码私钥失败: %v", err)
		}
		writeTestPEM(t, keyPath, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
		return crt, keyPath
	}

	serverCrt, serverKey = issue(2, "edgeflow-tls-server", x509.ExtKeyUsageServerAuth, net.ParseIP("127.0.0.1"))
	clientCrt, clientKey = issue(3, "edgeflow-tls-client", x509.ExtKeyUsageClientAuth, nil)
	return caCrt, serverCrt, serverKey, clientCrt, clientKey
}

// startTLSEtcd 启动 https 单节点 embed（模拟启用 TLS 的外部集群）。
func startTLSEtcd(t *testing.T, dir, clientHost string, tlsInfo transport.TLSInfo) (string, *embed.Etcd) {
	t.Helper()
	cfg := embed.NewConfig()
	cfg.Dir = dir
	cfg.LogLevel = "error"
	clientURL := fmt.Sprintf("https://%s", clientHost)
	cfg.ListenClientUrls = []url.URL{{Scheme: "https", Host: clientHost}}
	cfg.AdvertiseClientUrls = []url.URL{{Scheme: "https", Host: clientHost}}
	peerHost := fmt.Sprintf("127.0.0.1:%d", tlsTestFreePort(t))
	cfg.ListenPeerUrls = []url.URL{{Scheme: "http", Host: peerHost}}
	cfg.AdvertisePeerUrls = []url.URL{{Scheme: "http", Host: peerHost}}
	// embed.NewConfig 默认 InitialCluster 指向 localhost:2380，必须与
	// AdvertisePeerUrls 对齐（单节点自举 default=<advertise-peer>）。
	cfg.InitialCluster = "default=http://" + peerHost
	cfg.ClientTLSInfo = tlsInfo
	et, err := embed.StartEtcd(cfg)
	if err != nil {
		t.Fatalf("TLS embed 启动失败: %v", err)
	}
	select {
	case <-et.Server.ReadyNotify():
	case <-time.After(15 * time.Second):
		t.Fatal("TLS embed 就绪超时")
	}
	t.Cleanup(func() { et.Close() })
	return clientURL, et
}

func TestExternalTLSHandshake(t *testing.T) {
	dir := t.TempDir()
	caCrt, serverCrt, serverKey, clientCrt, clientKey := tlsTestPKI(t, dir)
	host := fmt.Sprintf("127.0.0.1:%d", tlsTestFreePort(t))

	// 服务器证书由 PKI 签发（SAN 127.0.0.1/localhost）。注意 transport.TLSInfo 语义：
	// TrustedCAFile 非空即 RequireAndVerifyClientCert（强制客户端证书）——CA-only
	// 场景（server-auth / wrong-ca）必须只给 CertFile/KeyFile；mTLS 场景才附加
	// TrustedCAFile 并依赖其隐式强制客户端证书校验。
	serverTLS := transport.TLSInfo{CertFile: serverCrt, KeyFile: serverKey}
	mtlsServerTLS := transport.TLSInfo{CertFile: serverCrt, KeyFile: serverKey, TrustedCAFile: caCrt}

	// ① CA-only 服务器认证：信任 CA → https 探活成功（服务端证书校验通过）
	t.Run("server-auth", func(t *testing.T) {
		clientURL, _ := startTLSEtcd(t, t.TempDir(), host, serverTLS)
		cfg := etcdstore.Config{CAFile: caCrt}
		tlsCfg, err := cfg.BuildTLS()
		if err != nil {
			t.Fatalf("BuildTLS 失败: %v", err)
		}
		kv, err := etcdstore.NewKVStoreWithTLS([]string{clientURL}, tlsCfg)
		if err != nil {
			t.Fatalf("NewKVStoreWithTLS 失败: %v", err)
		}
		defer kv.Close()
		if err := etcdstore.ProbeAlive(kv); err != nil {
			t.Fatalf("CA-only 服务器认证探活应成功: %v", err)
		}
	})

	// ② mTLS：ClientCertAuth=true + 客户端证书 → 成功
	t.Run("mtls-success", func(t *testing.T) {
		clientURL, _ := startTLSEtcd(t, t.TempDir(), host, mtlsServerTLS)
		cfg := etcdstore.Config{CAFile: caCrt, CertFile: clientCrt, KeyFile: clientKey}
		tlsCfg, err := cfg.BuildTLS()
		if err != nil {
			t.Fatalf("BuildTLS 失败: %v", err)
		}
		kv, err := etcdstore.NewKVStoreWithTLS([]string{clientURL}, tlsCfg)
		if err != nil {
			t.Fatalf("NewKVStoreWithTLS 失败: %v", err)
		}
		defer kv.Close()
		if err := etcdstore.ProbeAlive(kv); err != nil {
			t.Fatalf("mTLS 探活应成功: %v", err)
		}
	})

	// ③ mTLS 缺客户端证书：ClientCertAuth=true + 仅 CA → 握手被拒
	t.Run("mtls-missing-client-cert", func(t *testing.T) {
		clientURL, _ := startTLSEtcd(t, t.TempDir(), host, mtlsServerTLS)
		cfg := etcdstore.Config{CAFile: caCrt}
		tlsCfg, err := cfg.BuildTLS()
		if err != nil {
			t.Fatalf("BuildTLS 失败: %v", err)
		}
		kv, err := etcdstore.NewKVStoreWithTLS([]string{clientURL}, tlsCfg)
		if err != nil {
			t.Fatalf("NewKVStoreWithTLS 失败: %v", err)
		}
		defer kv.Close()
		if err := etcdstore.ProbeAlive(kv); err == nil {
			t.Fatal("缺客户端证书时探活应失败（服务端要求 mTLS）")
		}
	})

	// ④ 错误 CA：信任另一把 CA → 证书链校验失败
	t.Run("wrong-ca", func(t *testing.T) {
		clientURL, _ := startTLSEtcd(t, t.TempDir(), host, serverTLS)
		otherDir := t.TempDir()
		otherCA, _, _, _, _ := tlsTestPKI(t, otherDir)
		cfg := etcdstore.Config{CAFile: otherCA}
		tlsCfg, err := cfg.BuildTLS()
		if err != nil {
			t.Fatalf("BuildTLS 失败: %v", err)
		}
		kv, err := etcdstore.NewKVStoreWithTLS([]string{clientURL}, tlsCfg)
		if err != nil {
			t.Fatalf("NewKVStoreWithTLS 失败: %v", err)
		}
		defer kv.Close()
		if err := etcdstore.ProbeAlive(kv); err == nil {
			t.Fatal("错误 CA 探活应失败（unknown authority）")
		}
	})
}
