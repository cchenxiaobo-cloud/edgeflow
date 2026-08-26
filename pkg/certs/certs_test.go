package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTempCertDir 创建临时证书目录（测试用，自动清理）。
func newTempCertDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "certs")
}

// mustGenAll 生成全套证书（CA + 服务端 + 客户端），失败即终止测试。
func mustGenAll(t *testing.T, certDir string) {
	t.Helper()
	if _, err := EnsureCA(certDir); err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	if _, err := EnsureServerCert(certDir, "cloudcore"); err != nil {
		t.Fatalf("EnsureServerCert 失败: %v", err)
	}
	if _, err := EnsureClientCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("EnsureClientCert 失败: %v", err)
	}
}

// TestEnsureCA_GenerateAndLoad 验证 CA 生成→加载往返：文件存在、内容可解析、
// CN/有效期/IsCA 符合约定。
func TestEnsureCA_GenerateAndLoad(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, err := EnsureCA(certDir)
	if err != nil {
		t.Fatalf("EnsureCA 生成失败: %v", err)
	}
	if ca.Cert.Subject.CommonName != "edgeflow-ca" {
		t.Errorf("CA CN = %q，期望 edgeflow-ca", ca.Cert.Subject.CommonName)
	}
	if !ca.Cert.IsCA {
		t.Error("CA IsCA = false，期望 true")
	}
	// 有效期约 10 年（允许 ±1 天偏差）
	got := ca.Cert.NotAfter.Sub(ca.Cert.NotBefore)
	want := 10 * 365 * 24 * time.Hour
	if diff := got - want; diff > 24*time.Hour || diff < -24*time.Hour {
		t.Errorf("CA 有效期 = %v，期望约 %v", got, want)
	}

	// 文件落盘：证书 0644、私钥 0600
	info, err := os.Stat(filepath.Join(certDir, FileCACert))
	if err != nil {
		t.Fatalf("CA 证书文件不存在: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("CA 证书权限 = %o，期望 644", perm)
	}
	info, err = os.Stat(filepath.Join(certDir, FileCAKey))
	if err != nil {
		t.Fatalf("CA 私钥文件不存在: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("CA 私钥权限 = %o，期望 600（私钥必须 0600）", perm)
	}

	// 加载往返：重新 EnsureCA 应加载同一 CA（幂等，见 TestEnsureCA_Idempotent），
	// 直接 loadCA 也应成功
	loaded, err := loadCA(certDir)
	if err != nil {
		t.Fatalf("loadCA 失败: %v", err)
	}
	if !loaded.Cert.Equal(ca.Cert) {
		t.Error("加载的 CA 证书与生成的不一致")
	}
}

// TestEnsureCA_Idempotent 验证幂等：二次调用不重新生成（CA 私钥字节不变）。
func TestEnsureCA_Idempotent(t *testing.T) {
	certDir := newTempCertDir(t)
	if _, err := EnsureCA(certDir); err != nil {
		t.Fatalf("首次 EnsureCA 失败: %v", err)
	}
	keyPath := filepath.Join(certDir, FileCAKey)
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读取首次生成的 CA 私钥失败: %v", err)
	}
	// 记录生成时间（文件系统精度内比较 mtime）
	infoBefore, _ := os.Stat(keyPath)

	time.Sleep(10 * time.Millisecond) // 确保 mtime 可区分
	if _, err := EnsureCA(certDir); err != nil {
		t.Fatalf("二次 EnsureCA 失败: %v", err)
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读取二次后的 CA 私钥失败: %v", err)
	}
	if string(before) != string(after) {
		t.Error("二次 EnsureCA 重新生成了 CA 私钥（应加载已有文件）")
	}
	infoAfter, _ := os.Stat(keyPath)
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Error("二次 EnsureCA 改写了 CA 私钥文件（应保持不动）")
	}
}

// TestEnsureLeaf_GenerateAndLoad 验证叶子证书生成→加载往返与内容约定：
// 服务端证书带 SAN/EKU ServerAuth；客户端证书 CN 与 EKU ClientAuth。
func TestEnsureLeaf_GenerateAndLoad(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)

	// 服务端证书
	server, err := EnsureServerCert(certDir, "cloudcore")
	if err != nil {
		t.Fatalf("EnsureServerCert 加载失败: %v", err)
	}
	serverCert, err := x509.ParseCertificate(server.Certificate[0])
	if err != nil {
		t.Fatalf("解析服务端证书失败: %v", err)
	}
	if serverCert.Subject.CommonName != "cloudcore" {
		t.Errorf("服务端 CN = %q，期望 cloudcore", serverCert.Subject.CommonName)
	}
	if len(serverCert.DNSNames) != 2 || serverCert.DNSNames[0] != "localhost" || serverCert.DNSNames[1] != "cloudcore" {
		t.Errorf("服务端 SAN DNS = %v，期望 [localhost cloudcore]", serverCert.DNSNames)
	}
	if len(serverCert.IPAddresses) != 1 || serverCert.IPAddresses[0].String() != "127.0.0.1" {
		t.Errorf("服务端 SAN IP = %v，期望 [127.0.0.1]", serverCert.IPAddresses)
	}
	if !hasExtKeyUsage(serverCert, x509.ExtKeyUsageServerAuth) {
		t.Error("服务端证书缺少 EKU ServerAuth")
	}
	if serverCert.NotAfter.Sub(serverCert.NotBefore) > 366*24*time.Hour {
		t.Errorf("服务端证书有效期过长: %v", serverCert.NotAfter.Sub(serverCert.NotBefore))
	}

	// 客户端证书
	client, err := EnsureClientCert(certDir, "edgeflow-test-node")
	if err != nil {
		t.Fatalf("EnsureClientCert 加载失败: %v", err)
	}
	clientCert, err := x509.ParseCertificate(client.Certificate[0])
	if err != nil {
		t.Fatalf("解析客户端证书失败: %v", err)
	}
	if clientCert.Subject.CommonName != "edgeflow-test-node" {
		t.Errorf("客户端 CN = %q，期望 edgeflow-test-node", clientCert.Subject.CommonName)
	}
	if !hasExtKeyUsage(clientCert, x509.ExtKeyUsageClientAuth) {
		t.Error("客户端证书缺少 EKU ClientAuth")
	}
	if hasExtKeyUsage(clientCert, x509.ExtKeyUsageServerAuth) {
		t.Error("客户端证书不应包含 EKU ServerAuth")
	}

	// 叶子私钥权限 0600
	for _, keyFile := range []string{FileServerKey, FileClientKey} {
		info, err := os.Stat(filepath.Join(certDir, keyFile))
		if err != nil {
			t.Fatalf("%s 不存在: %v", keyFile, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s 权限 = %o，期望 600", keyFile, perm)
		}
	}

	// 证书链校验：叶子由 CA 签发
	ca, err := loadCA(certDir)
	if err != nil {
		t.Fatalf("loadCA 失败: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	intermediates := x509.NewCertPool()
	for _, cert := range []*x509.Certificate{serverCert, clientCert} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
			t.Errorf("叶子证书链校验失败: %v", err)
		}
	}
}

// TestEnsureLeaf_Idempotent 验证叶子证书幂等：二次调用不重新生成（私钥不变）。
func TestEnsureLeaf_Idempotent(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)

	keyPath := filepath.Join(certDir, FileClientKey)
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读取客户端私钥失败: %v", err)
	}
	infoBefore, _ := os.Stat(keyPath)
	time.Sleep(10 * time.Millisecond)

	if _, err := EnsureClientCert(certDir, "edgeflow-other-node"); err != nil {
		t.Fatalf("二次 EnsureClientCert 失败: %v", err)
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读取二次后的客户端私钥失败: %v", err)
	}
	if string(before) != string(after) {
		t.Error("二次 EnsureClientCert 重新生成了私钥（应加载已有文件）")
	}
	infoAfter, _ := os.Stat(keyPath)
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Error("二次 EnsureClientCert 改写了私钥文件")
	}
}

// TestEnsureCA_AutoCreateDir 验证目录不存在时自动创建。
func TestEnsureCA_AutoCreateDir(t *testing.T) {
	certDir := filepath.Join(t.TempDir(), "a", "b", "certs") // 多级不存在
	if _, err := EnsureCA(certDir); err != nil {
		t.Fatalf("EnsureCA 应自动创建目录，却失败: %v", err)
	}
	if info, err := os.Stat(certDir); err != nil || !info.IsDir() {
		t.Fatalf("证书目录未创建: %v", err)
	}
}

// TestEnsureCA_CorruptFile 验证损坏的 CA 文件报错（不静默重新生成）。
func TestEnsureCA_CorruptFile(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)

	// 损坏 ca.crt：写入垃圾内容
	if err := os.WriteFile(filepath.Join(certDir, FileCACert), []byte("not a pem"), 0o644); err != nil {
		t.Fatalf("写入损坏文件失败: %v", err)
	}
	if _, err := EnsureCA(certDir); err == nil {
		t.Fatal("CA 证书损坏时应报错，实际返回 nil")
	}
}

// TestEnsureCA_PartialPair 验证只存在一半（有 key 无 crt）时报错。
func TestEnsureCA_PartialPair(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	if err := os.Remove(filepath.Join(certDir, FileCACert)); err != nil {
		t.Fatalf("删除 ca.crt 失败: %v", err)
	}
	if _, err := EnsureCA(certDir); err == nil {
		t.Fatal("CA 证书缺失时应报错（防止静默轮换 CA），实际返回 nil")
	}
}

// TestLoadTLSConfig 验证两侧 tls.Config 语义：
// 服务端强制 mTLS（RequireAndVerifyClientCert + ClientCAs），客户端带 RootCAs。
func TestLoadTLSConfig(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)

	serverCfg, err := LoadTLSConfig(certDir, true)
	if err != nil {
		t.Fatalf("LoadTLSConfig(server) 失败: %v", err)
	}
	if serverCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("服务端 ClientAuth = %v，期望 RequireAndVerifyClientCert", serverCfg.ClientAuth)
	}
	if serverCfg.ClientCAs == nil {
		t.Error("服务端 ClientCAs 为 nil，应包含 CA 池")
	}
	if len(serverCfg.Certificates) != 1 {
		t.Errorf("服务端 Certificates = %d 张，期望 1", len(serverCfg.Certificates))
	}
	if serverCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("服务端 MinVersion = %x，期望 TLS1.2", serverCfg.MinVersion)
	}

	clientCfg, err := LoadTLSConfig(certDir, false)
	if err != nil {
		t.Fatalf("LoadTLSConfig(client) 失败: %v", err)
	}
	if clientCfg.RootCAs == nil {
		t.Error("客户端 RootCAs 为 nil，应包含 CA 池")
	}
	if len(clientCfg.Certificates) != 1 {
		t.Errorf("客户端 Certificates = %d 张，期望 1", len(clientCfg.Certificates))
	}
}

// TestTLSHandshake_Success 集成测试：真实 TLS 握手。
// 服务端 RequireAndVerifyClientCert + 客户端带 CA 签发证书 → 握手成功。
func TestTLSHandshake_Success(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)

	serverCfg, err := LoadTLSConfig(certDir, true)
	if err != nil {
		t.Fatalf("服务端 TLS 配置失败: %v", err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	ts.TLS = serverCfg
	ts.StartTLS()
	defer ts.Close()

	clientCfg, err := LoadTLSConfig(certDir, false)
	if err != nil {
		t.Fatalf("客户端 TLS 配置失败: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientCfg}}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("带证书的 TLS 握手失败（应成功）: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200", resp.StatusCode)
	}
}

// TestTLSHandshake_NoClientCert 集成测试：客户端不带证书 → 服务端拒绝握手。
func TestTLSHandshake_NoClientCert(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)

	serverCfg, err := LoadTLSConfig(certDir, true)
	if err != nil {
		t.Fatalf("服务端 TLS 配置失败: %v", err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	ts.TLS = serverCfg
	ts.StartTLS()
	defer ts.Close()

	// 客户端只信任 CA、不携带客户端证书
	ca, err := loadCA(certDir)
	if err != nil {
		t.Fatalf("loadCA 失败: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	_, err = client.Get(ts.URL)
	if err == nil {
		t.Fatal("无客户端证书的握手应被拒绝（RequireAndVerifyClientCert），实际成功")
	}
	if !strings.Contains(err.Error(), "certificate required") &&
		!strings.Contains(err.Error(), "bad certificate") &&
		!strings.Contains(err.Error(), "unknown certificate authority") {
		t.Logf("拒绝原因: %v", err)
	}
}

// TestTLSHandshake_WrongCA 集成测试：客户端携带其他 CA 签发的证书 → 握手被拒。
func TestTLSHandshake_WrongCA(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)

	serverCfg, err := LoadTLSConfig(certDir, true)
	if err != nil {
		t.Fatalf("服务端 TLS 配置失败: %v", err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	ts.TLS = serverCfg
	ts.StartTLS()
	defer ts.Close()

	// 构造“恶意”CA：独立自签 CA + 由其签发的客户端证书
	rogueDir := filepath.Join(t.TempDir(), "rogue")
	if _, err := EnsureCA(rogueDir); err != nil {
		t.Fatalf("生成恶意 CA 失败: %v", err)
	}
	rogueClient, err := EnsureClientCert(rogueDir, "edgeflow-rogue")
	if err != nil {
		t.Fatalf("生成恶意客户端证书失败: %v", err)
	}
	rogueCA, err := loadCA(rogueDir)
	if err != nil {
		t.Fatalf("加载恶意 CA 失败: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(rogueCA.Cert)

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{*rogueClient},
		RootCAs:      pool,
	}}}
	_, err = client.Get(ts.URL)
	if err == nil {
		t.Fatal("非本 CA 签发的客户端证书应被拒绝，实际握手成功")
	}
	if !strings.Contains(err.Error(), "unknown certificate authority") &&
		!strings.Contains(err.Error(), "bad certificate") &&
		!strings.Contains(err.Error(), "certificate required") {
		t.Logf("拒绝原因: %v", err)
	}
}

// hasExtKeyUsage 判断证书是否包含指定 EKU。
func hasExtKeyUsage(cert *x509.Certificate, want x509.ExtKeyUsage) bool {
	for _, u := range cert.ExtKeyUsage {
		if u == want {
			return true
		}
	}
	return false
}

// TestEnsureServerCertWithSANs 验证 M4B P1-4：显式 SAN 注入生效。
func TestEnsureServerCertWithSANs(t *testing.T) {
	dir := t.TempDir()
	ip := net.ParseIP("10.0.0.5")
	if _, err := EnsureServerCertWithSANs(dir, "cloudcore", []net.IP{ip}, []string{"cloudcore.svc"}); err != nil {
		t.Fatalf("EnsureServerCertWithSANs 失败: %v", err)
	}
	certPath := filepath.Join(dir, FileServerCert)
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("读取证书失败: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("证书 PEM 解码失败")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	foundIP, foundDNS := false, false
	for _, v := range cert.IPAddresses {
		if v.Equal(ip) {
			foundIP = true
		}
	}
	for _, d := range cert.DNSNames {
		if d == "cloudcore.svc" {
			foundDNS = true
		}
	}
	if !foundIP || !foundDNS {
		t.Errorf("SAN 注入未生效：IP=%v DNS=%v（证书 SAN: %v / %v）", foundIP, foundDNS, cert.IPAddresses, cert.DNSNames)
	}
}

// TestLoadCAMismatchedKey 验证 M4B P2-1：CA 证书与私钥错配时加载报错。
func TestLoadCAMismatchedKey(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	// 用另一把新私钥覆盖 CA 私钥（制造错配）
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成错配私钥失败: %v", err)
	}
	keyPath := filepath.Join(dir, FileCAKey)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(newKey)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("覆盖私钥失败: %v", err)
	}
	if _, err := loadCA(dir); err == nil {
		t.Fatal("错配 key 应报错（P2-1），实际静默通过")
	}
	_ = ca
}

// TestRotateClientCert_Regenerates 验证轮换成功路径：新密钥 + 新序列号、
// CN 不变、新证书由同一 CA 签发（签名链有效）、文件权限与无临时文件残留。
func TestRotateClientCert_Regenerates(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)

	// 记录轮换前的旧证书/私钥。
	oldKey, err := os.ReadFile(filepath.Join(certDir, FileClientKey))
	if err != nil {
		t.Fatalf("读取旧私钥失败: %v", err)
	}
	oldCert, err := loadLeafCert(filepath.Join(certDir, FileClientCert), filepath.Join(certDir, FileClientKey))
	if err != nil {
		t.Fatalf("加载旧证书失败: %v", err)
	}
	oldParsed, err := x509.ParseCertificate(oldCert.Certificate[0])
	if err != nil {
		t.Fatalf("解析旧证书失败: %v", err)
	}

	leaf, err := RotateClientCert(certDir, "edgeflow-test-node")
	if err != nil {
		t.Fatalf("RotateClientCert 失败: %v", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("解析新证书失败: %v", err)
	}

	// CN 不变；序列号与私钥必须变化（否则不是轮换）。
	if parsed.Subject.CommonName != "edgeflow-test-node" {
		t.Errorf("新证书 CN = %q，期望 edgeflow-test-node", parsed.Subject.CommonName)
	}
	if parsed.SerialNumber.Cmp(oldParsed.SerialNumber) == 0 {
		t.Error("轮换后序列号未变化（应生成新序列号）")
	}
	newKey, err := os.ReadFile(filepath.Join(certDir, FileClientKey))
	if err != nil {
		t.Fatalf("读取新私钥失败: %v", err)
	}
	if string(newKey) == string(oldKey) {
		t.Error("轮换后私钥未变化（应生成新私钥）")
	}
	if !hasExtKeyUsage(parsed, x509.ExtKeyUsageClientAuth) {
		t.Error("新证书缺少 EKU ClientAuth")
	}

	// 新证书由现有 CA 签发（签名链校验）。
	ca, err := loadCA(certDir)
	if err != nil {
		t.Fatalf("loadCA 失败: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := parsed.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("新证书无法通过 CA 签名链校验: %v", err)
	}

	// 文件权限：证书 0644 / 私钥 0600。
	for file, want := range map[string]os.FileMode{FileClientCert: 0o644, FileClientKey: 0o600} {
		info, err := os.Stat(filepath.Join(certDir, file))
		if err != nil {
			t.Fatalf("stat %s 失败: %v", file, err)
		}
		if perm := info.Mode().Perm(); perm != want {
			t.Errorf("%s 权限 = %o，期望 %o", file, perm, want)
		}
	}

	// 无临时文件残留（事务化落盘清理）。
	assertNoCertTempFiles(t, certDir)
}

// TestRotateClientCert_CNMismatchRejected 验证 CN 不一致拒绝轮换（身份保护）。
func TestRotateClientCert_CNMismatchRejected(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	before, err := os.ReadFile(filepath.Join(certDir, FileClientKey))
	if err != nil {
		t.Fatalf("读取旧私钥失败: %v", err)
	}

	if _, err := RotateClientCert(certDir, "edgeflow-other-node"); err == nil {
		t.Fatal("CN 不一致应拒绝轮换，实际成功")
	} else if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("错误应提示 CN 不一致: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(certDir, FileClientKey))
	if err != nil {
		t.Fatalf("读取私钥失败: %v", err)
	}
	if string(before) != string(after) {
		t.Error("拒绝轮换后私钥不应变化")
	}
}

// TestRotateClientCert_MissingLeaf 验证无旧证书时拒绝（轮换不是初始化）。
func TestRotateClientCert_MissingLeaf(t *testing.T) {
	certDir := newTempCertDir(t)
	if _, err := EnsureCA(certDir); err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	if _, err := RotateClientCert(certDir, "edgeflow-test-node"); err == nil {
		t.Fatal("无旧证书应拒绝轮换，实际成功")
	} else if !strings.Contains(err.Error(), "没有可轮换") {
		t.Errorf("错误应提示没有可轮换的旧证书: %v", err)
	}
	if _, err := os.Stat(filepath.Join(certDir, FileClientCert)); !os.IsNotExist(err) {
		t.Error("拒绝轮换后不应生成 edgecore.crt")
	}
	assertNoCertTempFiles(t, certDir)
}

// TestRotateClientCert_MissingCA 验证 CA 缺失时轮换失败且旧证书不受影响
// （轮换绝不自动创建 CA，防止静默换 CA 导致全量叶子证书失效）。
func TestRotateClientCert_MissingCA(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	// 移除 CA 私钥（CA 不完整）。
	if err := os.Remove(filepath.Join(certDir, FileCAKey)); err != nil {
		t.Fatalf("移除 CA 私钥失败: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(certDir, FileClientCert))
	if err != nil {
		t.Fatalf("读取旧证书失败: %v", err)
	}

	if _, err := RotateClientCert(certDir, "edgeflow-test-node"); err == nil {
		t.Fatal("CA 缺失应轮换失败，实际成功")
	} else if !strings.Contains(err.Error(), "CA") {
		t.Errorf("错误应提示 CA 问题: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(certDir, FileClientCert))
	if err != nil {
		t.Fatalf("读取证书失败: %v", err)
	}
	if string(before) != string(after) {
		t.Error("轮换失败后旧证书不应变化")
	}
	assertNoCertTempFiles(t, certDir)
}

// TestRotateServerCert_PreservesSAN 验证服务端证书轮换保留 SAN
// （轮换后服务端证书必须继续覆盖边缘节点访问地址）。
func TestRotateServerCert_PreservesSAN(t *testing.T) {
	certDir := newTempCertDir(t)
	if _, err := EnsureCA(certDir); err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	ips := []net.IP{net.ParseIP("192.168.1.10")}
	dns := []string{"edge.example.com"}
	if _, err := EnsureServerCertWithSANs(certDir, "cloudcore", ips, dns); err != nil {
		t.Fatalf("EnsureServerCertWithSANs 失败: %v", err)
	}

	if _, err := RotateServerCertWithSANs(certDir, "cloudcore", ips, dns); err != nil {
		t.Fatalf("RotateServerCertWithSANs 失败: %v", err)
	}
	leaf, err := EnsureServerCert(certDir, "cloudcore")
	if err != nil {
		t.Fatalf("加载轮换后证书失败: %v", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("解析轮换后证书失败: %v", err)
	}
	if parsed.Subject.CommonName != "cloudcore" {
		t.Errorf("轮换后 CN = %q，期望 cloudcore", parsed.Subject.CommonName)
	}
	if len(parsed.IPAddresses) != 1 || parsed.IPAddresses[0].String() != "192.168.1.10" {
		t.Errorf("轮换后 SAN IP 丢失: %v", parsed.IPAddresses)
	}
	if len(parsed.DNSNames) != 1 || parsed.DNSNames[0] != "edge.example.com" {
		t.Errorf("轮换后 SAN DNS 丢失: %v", parsed.DNSNames)
	}
	if !hasExtKeyUsage(parsed, x509.ExtKeyUsageServerAuth) {
		t.Error("轮换后证书缺少 EKU ServerAuth")
	}
	assertNoCertTempFiles(t, certDir)
}

// assertNoCertTempFiles 断言证书目录无 .*.tmp-* 残留（事务化落盘清理）。
func assertNoCertTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取证书目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("临时文件未清理: %s", e.Name())
		}
	}
}

// loadParsedClientCert 加载并解析 edgecore.crt（测试辅助）。
func loadParsedClientCert(t *testing.T, certDir string) *x509.Certificate {
	t.Helper()
	leaf, err := EnsureClientCert(certDir, "edgeflow-test-node")
	if err != nil {
		t.Fatalf("加载客户端证书失败: %v", err)
	}
	c, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("解析客户端证书失败: %v", err)
	}
	return c
}

// readCRLJSON 读取并解析 crl.json（测试辅助）。
func readCRLJSON(t *testing.T, certDir string) *revokedList {
	t.Helper()
	_, crlJSON := CRLPaths(certDir)
	b, err := os.ReadFile(crlJSON)
	if err != nil {
		t.Fatalf("读取 crl.json 失败: %v", err)
	}
	var list revokedList
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("解析 crl.json 失败: %v", err)
	}
	return &list
}

// parseCRLFile 读取并解析 crl.pem（测试辅助）。
func parseCRLFile(t *testing.T, certDir string) *x509.RevocationList {
	t.Helper()
	crlPEMPath, _ := CRLPaths(certDir)
	b, err := os.ReadFile(crlPEMPath)
	if err != nil {
		t.Fatalf("读取 crl.pem 失败: %v", err)
	}
	rl, err := parseCRLPEM(b)
	if err != nil {
		t.Fatalf("解析 crl.pem 失败: %v", err)
	}
	return rl
}

// TestRevokeCert_Success 验证吊销成功路径：crl.json 记录序列号、crl.pem 产物
// 生成且由 CA 签名、VerifyCertAgainstCRL 对被吊销证书返回 true、未吊销证书
// 返回 false、无临时文件残留。
func TestRevokeCert_Success(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)

	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	crlPEMPath, crlJSONPath := CRLPaths(certDir)
	for _, p := range []string{crlPEMPath, crlJSONPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("吊销后缺少产物 %s: %v", p, err)
		}
	}

	// crl.json 记录该序列号（且仅一条）。
	list := readCRLJSON(t, certDir)
	if len(list.Entries) != 1 || list.Entries[0].Serial != clientCert.SerialNumber.Text(16) {
		t.Errorf("crl.json 记录异常: %+v（期望序列号 %s）", list.Entries, clientCert.SerialNumber.Text(16))
	}
	if list.Number != 1 {
		t.Errorf("crl.json Number = %d，期望 1", list.Number)
	}

	// crl.pem 可解析、由本 CA 签发、含该序列号。
	rl := parseCRLFile(t, certDir)
	ca, err := loadCA(certDir)
	if err != nil {
		t.Fatalf("loadCA 失败: %v", err)
	}
	if err := rl.CheckSignatureFrom(ca.Cert); err != nil {
		t.Errorf("CRL 签名校验失败: %v", err)
	}
	if len(rl.RevokedCertificateEntries) != 1 ||
		rl.RevokedCertificateEntries[0].SerialNumber.Cmp(clientCert.SerialNumber) != 0 {
		t.Errorf("CRL 条目异常: %+v", rl.RevokedCertificateEntries)
	}
	if rl.Number == nil || rl.Number.Int64() != 1 {
		t.Errorf("CRL Number = %v，期望 1", rl.Number)
	}

	// 校验：被吊销证书被拒，未吊销证书通过。
	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil {
		t.Fatalf("VerifyCertAgainstCRL(被吊销) 失败: %v", err)
	}
	if !revoked {
		t.Error("被吊销证书应返回 true（被拒），实际 false")
	}
	server, err := EnsureServerCert(certDir, "cloudcore")
	if err != nil {
		t.Fatalf("加载服务端证书失败: %v", err)
	}
	serverCert, err := x509.ParseCertificate(server.Certificate[0])
	if err != nil {
		t.Fatalf("解析服务端证书失败: %v", err)
	}
	ok, err := VerifyCertAgainstCRL(certDir, serverCert)
	if err != nil {
		t.Fatalf("VerifyCertAgainstCRL(未吊销) 失败: %v", err)
	}
	if ok {
		t.Error("未吊销证书应返回 false，实际 true")
	}
	assertNoCertTempFiles(t, certDir)
}

// TestRevokeCert_ServerCert 验证可吊销服务端证书（cloudcore.crt，CN=cloudcore）。
func TestRevokeCert_ServerCert(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	server, err := EnsureServerCert(certDir, "cloudcore")
	if err != nil {
		t.Fatalf("加载服务端证书失败: %v", err)
	}
	serverCert, err := x509.ParseCertificate(server.Certificate[0])
	if err != nil {
		t.Fatalf("解析服务端证书失败: %v", err)
	}

	if err := RevokeCert(certDir, "cloudcore"); err != nil {
		t.Fatalf("RevokeCert(cloudcore) 失败: %v", err)
	}
	revoked, err := VerifyCertAgainstCRL(certDir, serverCert)
	if err != nil || !revoked {
		t.Errorf("cloudcore 证书应被吊销: revoked=%v err=%v", revoked, err)
	}
}

// TestRevokeCert_Idempotent 验证重复吊销幂等：不报错、序列号不重复、
// crl.pem 不重复写入（二次吊销后产物字节不变）。
func TestRevokeCert_Idempotent(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)

	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("首次吊销失败: %v", err)
	}
	crlPEMPath, _ := CRLPaths(certDir)
	afterFirst, err := os.ReadFile(crlPEMPath)
	if err != nil {
		t.Fatalf("读取 crl.pem 失败: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // 确保 mtime/内容可区分

	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("重复吊销应幂等成功，实际报错: %v", err)
	}
	list := readCRLJSON(t, certDir)
	if len(list.Entries) != 1 {
		t.Errorf("重复吊销后序列号应不重复（1 条），实际 %d 条: %+v", len(list.Entries), list.Entries)
	}
	afterSecond, err := os.ReadFile(crlPEMPath)
	if err != nil {
		t.Fatalf("读取 crl.pem 失败: %v", err)
	}
	if string(afterFirst) != string(afterSecond) {
		t.Error("幂等吊销不应重写 crl.pem（产物已一致）")
	}
}

// TestRevokeCert_UnknownCN 验证吊销不存在的 CN 返回明确错误（含节点提示）。
func TestRevokeCert_UnknownCN(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	err := RevokeCert(certDir, "edgeflow-ghost-node")
	if err == nil {
		t.Fatal("吊销不存在的 CN 应报错，实际成功")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误应提示节点不存在: %v", err)
	}
	// 吊销失败不应产生任何吊销产物。
	crlPEMPath, crlJSONPath := CRLPaths(certDir)
	if _, err := os.Stat(crlPEMPath); !os.IsNotExist(err) {
		t.Error("吊销失败不应生成 crl.pem")
	}
	if _, err := os.Stat(crlJSONPath); !os.IsNotExist(err) {
		t.Error("吊销失败不应生成 crl.json")
	}
}

// TestRevokeCert_MissingCA 验证 CA 缺失时报错且不自动创建 CA。
func TestRevokeCert_MissingCA(t *testing.T) {
	certDir := newTempCertDir(t)
	if err := RevokeCert(certDir, "edgeflow-test-node"); err == nil {
		t.Fatal("CA 缺失应报错，实际成功")
	} else if !strings.Contains(err.Error(), "CA") {
		t.Errorf("错误应提示 CA 问题: %v", err)
	}
	// 绝不自动创建 CA。
	if _, err := os.Stat(filepath.Join(certDir, FileCACert)); !os.IsNotExist(err) {
		t.Error("吊销不应自动创建 CA")
	}
}

// TestVerifyCertAgainstCRL_NoCRL 验证 CRL 缺失视为无吊销（向后兼容）。
func TestVerifyCertAgainstCRL_NoCRL(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	if _, err := os.Stat(filepath.Join(certDir, FileCRL)); !os.IsNotExist(err) {
		t.Fatalf("测试前置：crl.pem 不应存在")
	}
	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil {
		t.Fatalf("CRL 缺失应返回 (false, nil)，实际报错: %v", err)
	}
	if revoked {
		t.Error("CRL 缺失应视为无吊销（false），实际 true")
	}
}

// TestVerifyCertAgainstCRL_CorruptCRL 验证 CRL 损坏时返回错误（含路径）而非 panic。
func TestVerifyCertAgainstCRL_CorruptCRL(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	crlPEMPath, _ := CRLPaths(certDir)
	if err := os.WriteFile(crlPEMPath, []byte("this is not a CRL\n"), 0o644); err != nil {
		t.Fatalf("写入损坏 CRL 失败: %v", err)
	}
	if _, err := VerifyCertAgainstCRL(certDir, clientCert); err == nil {
		t.Fatal("CRL 损坏应返回错误，实际 nil")
	} else if !strings.Contains(err.Error(), crlPEMPath) {
		t.Errorf("错误信息应含 CRL 路径: %v", err)
	}
}

// TestRevokeCert_CorruptCRLNoJSON 验证 crl.json 与 crl.pem 都不可用时报错
// （含路径），不静默清空吊销状态。
func TestRevokeCert_CorruptCRLNoJSON(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	crlPEMPath, _ := CRLPaths(certDir)
	if err := os.WriteFile(crlPEMPath, []byte("garbage\n"), 0o644); err != nil {
		t.Fatalf("写入损坏 CRL 失败: %v", err)
	}
	err := RevokeCert(certDir, "edgeflow-test-node")
	if err == nil {
		t.Fatal("crl.json 缺失 + crl.pem 损坏应报错（无法恢复吊销状态），实际成功")
	}
	if !strings.Contains(err.Error(), crlPEMPath) {
		t.Errorf("错误信息应含 CRL 路径: %v", err)
	}
}

// TestRevokeCert_HealsMissingArtifact 验证自愈：吊销后删除 crl.pem，再次吊销
// 同一证书（幂等路径）应重新生成 crl.pem 且吊销状态不丢。
func TestRevokeCert_HealsMissingArtifact(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("首次吊销失败: %v", err)
	}
	crlPEMPath, _ := CRLPaths(certDir)
	if err := os.Remove(crlPEMPath); err != nil {
		t.Fatalf("删除 crl.pem 失败: %v", err)
	}

	// 幂等路径（序列号已在 crl.json）应自愈 crl.pem。
	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("自愈吊销失败: %v", err)
	}
	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil || !revoked {
		t.Errorf("自愈后吊销状态应恢复: revoked=%v err=%v", revoked, err)
	}
}

// TestRevokeCert_RecoversFromCRLPEM 验证 crl.json 缺失时从 crl.pem 恢复
// 序列号（不丢已吊销状态）。
func TestRevokeCert_RecoversFromCRLPEM(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("首次吊销失败: %v", err)
	}
	_, crlJSONPath := CRLPaths(certDir)
	if err := os.Remove(crlJSONPath); err != nil {
		t.Fatalf("删除 crl.json 失败: %v", err)
	}

	// 吊销另一个证书：应从 crl.pem 恢复原序列号，两个都在列表。
	if err := RevokeCert(certDir, "cloudcore"); err != nil {
		t.Fatalf("吊销 cloudcore 失败: %v", err)
	}
	list := readCRLJSON(t, certDir)
	if len(list.Entries) != 2 {
		t.Fatalf("恢复后应含 2 条记录，实际 %d: %+v", len(list.Entries), list.Entries)
	}
	wantSerial := clientCert.SerialNumber.Text(16)
	found := false
	for _, e := range list.Entries {
		if e.Serial == wantSerial {
			found = true
		}
	}
	if !found {
		t.Errorf("crl.json 恢复后缺少原序列号 %s: %+v", wantSerial, list.Entries)
	}
	// 两个证书都被拒。
	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil || !revoked {
		t.Errorf("恢复后原证书应仍被吊销: revoked=%v err=%v", revoked, err)
	}
}

// TestVerifyCertAgainstCRL_WrongCA 验证伪造/错配 CA 签发的 CRL 被拒绝。
func TestVerifyCertAgainstCRL_WrongCA(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)

	// 用另一把 CA 生成 CRL 覆盖 crl.pem（伪造场景）。
	rogueDir := filepath.Join(t.TempDir(), "rogue")
	rogueCA, err := EnsureCA(rogueDir)
	if err != nil {
		t.Fatalf("生成伪造 CA 失败: %v", err)
	}
	if err := writeCRLArtifact(rogueCA, certDir, &revokedList{
		Entries: []revokedEntry{{Serial: clientCert.SerialNumber.Text(16), Time: time.Now().UTC().Format(time.RFC3339)}},
		Number:  1,
	}); err != nil {
		t.Fatalf("写入伪造 CRL 失败: %v", err)
	}
	if _, err := VerifyCertAgainstCRL(certDir, clientCert); err == nil {
		t.Fatal("非本 CA 签发的 CRL 应被拒绝，实际通过")
	} else if !strings.Contains(err.Error(), "签名校验失败") {
		t.Errorf("错误应提示签名校验失败: %v", err)
	}
}

// TestVerifyCertAgainstCRL_NilCert 验证 nil 证书返回错误而非 panic。
func TestVerifyCertAgainstCRL_NilCert(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	if _, err := VerifyCertAgainstCRL(certDir, nil); err == nil {
		t.Fatal("nil 证书应返回错误，实际 nil")
	}
}

// TestRevokeCert_Concurrent 验证并发吊销互斥（进程级文件锁）：并行
// RevokeCert 不同 CN（同一证书目录），两个序列号都必须入列且不重复，
// CRL Number 正确（每个新序列号恰好一次新签发 +1）。无锁时读-改-写
// 竞争会静默丢失一个序列号（后写者覆盖先写者）。
func TestRevokeCert_Concurrent(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	server, err := EnsureServerCert(certDir, "cloudcore")
	if err != nil {
		t.Fatalf("加载服务端证书失败: %v", err)
	}
	serverCert, err := x509.ParseCertificate(server.Certificate[0])
	if err != nil {
		t.Fatalf("解析服务端证书失败: %v", err)
	}

	const perCN = 4
	var wg sync.WaitGroup
	errCh := make(chan error, perCN*2)
	for i := 0; i < perCN; i++ {
		for _, cn := range []string{"edgeflow-test-node", "cloudcore"} {
			wg.Add(1)
			go func(cn string) {
				defer wg.Done()
				if err := RevokeCert(certDir, cn); err != nil {
					errCh <- fmt.Errorf("RevokeCert(%s): %w", cn, err)
				}
			}(cn)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("并发吊销失败: %v", err)
	}

	// 两个序列号都入列，且各只有一条（无丢失/无重复）。
	list := readCRLJSON(t, certDir)
	if len(list.Entries) != 2 {
		t.Fatalf("并发吊销后应恰好 2 条记录（无丢失/重复），实际 %d 条: %+v", len(list.Entries), list.Entries)
	}
	want := map[string]bool{
		clientCert.SerialNumber.Text(16): true,
		serverCert.SerialNumber.Text(16): true,
	}
	for _, e := range list.Entries {
		if !want[e.Serial] {
			t.Errorf("意外序列号 %q: %+v", e.Serial, list.Entries)
		}
		delete(want, e.Serial)
	}
	for serial := range want {
		t.Errorf("并发吊销丢失序列号 %s", serial)
	}

	// CRL Number 正确：两个新序列号 → 两次签发 → Number=2，json/pem 一致。
	if list.Number != 2 {
		t.Errorf("crl.json Number = %d，期望 2（每个新序列号一次签发 +1）", list.Number)
	}
	rl := parseCRLFile(t, certDir)
	if len(rl.RevokedCertificateEntries) != 2 {
		t.Errorf("crl.pem 条目数 = %d，期望 2", len(rl.RevokedCertificateEntries))
	}
	if rl.Number == nil || rl.Number.Int64() != 2 {
		t.Errorf("crl.pem Number = %v，期望 2", rl.Number)
	}

	// 两个证书都被拒。
	for _, c := range []*x509.Certificate{clientCert, serverCert} {
		revoked, err := VerifyCertAgainstCRL(certDir, c)
		if err != nil || !revoked {
			t.Errorf("并发吊销后证书（serial=%s）应被拒: revoked=%v err=%v", c.SerialNumber.Text(16), revoked, err)
		}
	}
}

// TestRevokeCertBySerial 验证按序列号吊销：不依赖证书文件，合法序列号
// （即使不在任何已知证书中）直接入列；大小写/0x 前缀归一化；幂等。
func TestRevokeCertBySerial(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	serial := clientCert.SerialNumber.Text(16)

	// 大写 + 0x 前缀输入 → 归一化为小写规范形式入列。
	if err := RevokeCertBySerial(certDir, "0X"+strings.ToUpper(serial)); err != nil {
		t.Fatalf("RevokeCertBySerial(大写) 失败: %v", err)
	}
	list := readCRLJSON(t, certDir)
	if len(list.Entries) != 1 || list.Entries[0].Serial != serial {
		t.Fatalf("crl.json 记录异常: %+v（期望小写序列号 %s）", list.Entries, serial)
	}
	if list.Number != 1 {
		t.Errorf("Number = %d，期望 1", list.Number)
	}
	// 吊销真实生效：该证书被拒。
	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil || !revoked {
		t.Errorf("按序列号吊销后证书应被拒: revoked=%v err=%v", revoked, err)
	}

	// 重复吊销幂等：不报错、不重复入列。
	if err := RevokeCertBySerial(certDir, serial); err != nil {
		t.Fatalf("重复吊销应幂等成功，实际报错: %v", err)
	}
	list = readCRLJSON(t, certDir)
	if len(list.Entries) != 1 || list.Number != 1 {
		t.Errorf("重复吊销后应仍为 1 条、Number 不变: %+v number=%d", list.Entries, list.Number)
	}
}

// TestRevokeCertBySerial_UnknownSerial 验证序列号不在任何已知证书时仍可
// 入列（轮换后旧证书泄露场景：吊销已不存在的历史序列号）。
func TestRevokeCertBySerial_UnknownSerial(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	unknown := "1234567890abcdef1234567890abcdef" // 128 位，非任何已知证书

	if err := RevokeCertBySerial(certDir, unknown); err != nil {
		t.Fatalf("吊销未知序列号应成功入列，实际报错: %v", err)
	}
	list := readCRLJSON(t, certDir)
	if len(list.Entries) != 1 || list.Entries[0].Serial != unknown {
		t.Fatalf("crl.json 应含未知序列号: %+v", list.Entries)
	}
	// crl.pem 也含该序列号。
	rl := parseCRLFile(t, certDir)
	if len(rl.RevokedCertificateEntries) != 1 || rl.RevokedCertificateEntries[0].SerialNumber.Text(16) != unknown {
		t.Errorf("crl.pem 应含未知序列号: %+v", rl.RevokedCertificateEntries)
	}
}

// TestRevokeCertBySerial_InvalidSerial 验证非法序列号返回明确错误且不产生产物。
func TestRevokeCertBySerial_InvalidSerial(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	for _, bad := range []string{"", "zzzz", "0", "-1", "1.5", "0x", "12 34"} {
		if err := RevokeCertBySerial(certDir, bad); err == nil {
			t.Errorf("序列号 %q 应报错，实际成功", bad)
		}
	}
	crlPEMPath, crlJSONPath := CRLPaths(certDir)
	if _, err := os.Stat(crlPEMPath); !os.IsNotExist(err) {
		t.Error("非法序列号不应生成 crl.pem")
	}
	if _, err := os.Stat(crlJSONPath); !os.IsNotExist(err) {
		t.Error("非法序列号不应生成 crl.json")
	}
}

// TestRevokeCert_HealRegenerationIncrementsNumber 验证幂等自愈重生成
// 路径 CRL Number 也单调递增（RFC 5280 §5.2.3：每次签发新 CRL 应单调
// 递增，不复用旧值），且 crl.json 与 crl.pem 的 Number 保持一致。
func TestRevokeCert_HealRegenerationIncrementsNumber(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)

	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("首次吊销失败: %v", err)
	}
	crlPEMPath, _ := CRLPaths(certDir)
	for i := int64(2); i <= 3; i++ {
		// 删除 crl.pem（模拟自愈场景：幂等路径需要重生成产物）。
		if err := os.Remove(crlPEMPath); err != nil {
			t.Fatalf("删除 crl.pem 失败: %v", err)
		}
		if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
			t.Fatalf("自愈吊销失败（第 %d 轮）: %v", i, err)
		}
		// 重生成即新签发：Number 递增而非复用旧值。
		list := readCRLJSON(t, certDir)
		if list.Number != i {
			t.Errorf("第 %d 轮自愈后 crl.json Number = %d，期望 %d（单调递增）", i, list.Number, i)
		}
		rl := parseCRLFile(t, certDir)
		if rl.Number == nil || rl.Number.Int64() != i {
			t.Errorf("第 %d 轮自愈后 crl.pem Number = %v，期望 %d（与 crl.json 一致）", i, rl.Number, i)
		}
	}
	// 吊销状态不丢。
	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil || !revoked {
		t.Errorf("自愈后吊销状态应保持: revoked=%v err=%v", revoked, err)
	}
}

// TestRevokeCert_NormalizesMixedCaseSerial 验证加载 crl.json 时序列号
// 统一小写归一化（含去前导零）并去重：手工编辑大写/带前导零序列号
// 不会产生重复条目与 CRL 重复项。
func TestRevokeCert_NormalizesMixedCaseSerial(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	serial := clientCert.SerialNumber.Text(16)

	// 模拟手工编辑：大写 + 原样 + 前导零（同一数值）三条重复记录。
	if err := writeRevokedList(certDir, &revokedList{
		Entries: []revokedEntry{
			{Serial: strings.ToUpper(serial), Time: "2026-01-01T00:00:00Z"},
			{Serial: serial, Time: "2026-01-02T00:00:00Z"},
			{Serial: "0" + serial, Time: "2026-01-03T00:00:00Z"},
		},
		Number:  1,
		Updated: "2026-01-03T00:00:00Z",
	}); err != nil {
		t.Fatalf("写入手工编辑的 crl.json 失败: %v", err)
	}

	// 加载即归一化：1 条规范小写记录（保留首条）。
	list, err := loadRevokedList(certDir)
	if err != nil {
		t.Fatalf("loadRevokedList 失败: %v", err)
	}
	if len(list.Entries) != 1 || list.Entries[0].Serial != serial {
		t.Fatalf("归一化后应恰好 1 条规范记录，实际 %+v", list.Entries)
	}

	// 继续吊销其他证书正常；CRL 序列号全部规范（无重复项）。
	if err := RevokeCert(certDir, "cloudcore"); err != nil {
		t.Fatalf("吊销 cloudcore 失败: %v", err)
	}
	rl := parseCRLFile(t, certDir)
	if len(rl.RevokedCertificateEntries) != 2 {
		t.Fatalf("crl.pem 应含 2 条规范条目，实际 %d", len(rl.RevokedCertificateEntries))
	}
	seen := map[string]bool{}
	for _, e := range rl.RevokedCertificateEntries {
		s := e.SerialNumber.Text(16)
		if s != strings.ToLower(s) {
			t.Errorf("CRL 序列号未小写: %s", s)
		}
		if seen[s] {
			t.Errorf("CRL 出现重复序列号: %s", s)
		}
		seen[s] = true
	}
	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil || !revoked {
		t.Errorf("归一化后原证书应仍被吊销: revoked=%v err=%v", revoked, err)
	}
}

// TestVerifyCertAgainstCRL_ReconcilesAheadJSON 验证崩溃窗口（crl.json
// 已领先、crl.pem 未更新）下 verify 侧自动对账：重生成 crl.pem 后
// 拒绝新吊销的证书（吊销立即生效，无需等下一次 revoke）。
func TestVerifyCertAgainstCRL_ReconcilesAheadJSON(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	server, err := EnsureServerCert(certDir, "cloudcore")
	if err != nil {
		t.Fatalf("加载服务端证书失败: %v", err)
	}
	serverCert, err := x509.ParseCertificate(server.Certificate[0])
	if err != nil {
		t.Fatalf("解析服务端证书失败: %v", err)
	}

	// 吊销 A（json+pem 一致，Number=1）。
	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("首次吊销失败: %v", err)
	}
	// 手工构造崩溃窗口：crl.json 领先（新增 B，pem 未写）。
	list := readCRLJSON(t, certDir)
	list.Entries = append(list.Entries, revokedEntry{
		Serial: serverCert.SerialNumber.Text(16),
		Time:   time.Now().UTC().Format(time.RFC3339),
	})
	if err := writeRevokedList(certDir, list); err != nil {
		t.Fatalf("写领先的 crl.json 失败: %v", err)
	}

	// verify B：应自愈（重生成 pem 含 B）并拒绝。
	revoked, err := VerifyCertAgainstCRL(certDir, serverCert)
	if err != nil {
		t.Fatalf("verify 对账自愈失败: %v", err)
	}
	if !revoked {
		t.Fatal("json 领先场景下 verify 应自愈并拒绝新吊销证书，实际放行")
	}
	rl := parseCRLFile(t, certDir)
	if len(rl.RevokedCertificateEntries) != 2 {
		t.Errorf("自愈后 crl.pem 应含 2 条，实际 %d", len(rl.RevokedCertificateEntries))
	}
	// A 仍被拒。
	revoked, err = VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil || !revoked {
		t.Errorf("自愈后原吊销证书应仍被拒: revoked=%v err=%v", revoked, err)
	}
}

// TestVerifyCertAgainstCRL_RecreatesMissingPEM 验证崩溃窗口另一形态：
// crl.json 存在而 crl.pem 缺失（首次吊销后未写出产物），verify 自动
// 重生成 crl.pem 并拒绝被吊销证书。
func TestVerifyCertAgainstCRL_RecreatesMissingPEM(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)

	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("首次吊销失败: %v", err)
	}
	crlPEMPath, _ := CRLPaths(certDir)
	if err := os.Remove(crlPEMPath); err != nil {
		t.Fatalf("删除 crl.pem 失败: %v", err)
	}

	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil {
		t.Fatalf("verify 重生成失败: %v", err)
	}
	if !revoked {
		t.Fatal("pem 缺失时 verify 应自愈并拒绝被吊销证书，实际放行")
	}
	if _, err := os.Stat(crlPEMPath); err != nil {
		t.Errorf("verify 自愈后 crl.pem 应存在: %v", err)
	}
}

// TestVerifyCertAgainstCRL_ReconcileFailClosed 验证对账失败 fail-closed：
// crl.json 损坏且 crl.pem 缺失/损坏 → verify 返回错误（不静默放行）；
// 旧行为（只读 pem、pem 缺失即放行）会把已入列吊销的证书放行。
func TestVerifyCertAgainstCRL_ReconcileFailClosed(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	_, crlJSONPath := CRLPaths(certDir)

	// crl.json 损坏 + 无 crl.pem：吊销记录存在但不可用 → fail-closed。
	if err := os.WriteFile(crlJSONPath, []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("写损坏 crl.json 失败: %v", err)
	}
	if _, err := VerifyCertAgainstCRL(certDir, clientCert); err == nil {
		t.Fatal("crl.json 损坏且 crl.pem 缺失应返回错误（fail-closed），实际放行")
	}

	// 对账重生成失败（json 领先但 CA 不可用）→ fail-closed 错误。
	certDir2 := newTempCertDir(t)
	mustGenAll(t, certDir2)
	if err := RevokeCert(certDir2, "edgeflow-test-node"); err != nil {
		t.Fatalf("首次吊销失败: %v", err)
	}
	crlPEMPath2, _ := CRLPaths(certDir2)
	if err := os.Remove(crlPEMPath2); err != nil {
		t.Fatalf("删除 crl.pem 失败: %v", err)
	}
	if err := os.Remove(filepath.Join(certDir2, FileCAKey)); err != nil {
		t.Fatalf("移除 CA 私钥失败: %v", err)
	}
	if _, err := VerifyCertAgainstCRL(certDir2, clientCert); err == nil {
		t.Fatal("json 领先但 CA 不可用（无法重生成）应返回错误（fail-closed），实际放行")
	} else if !strings.Contains(err.Error(), "重生成失败") {
		t.Errorf("错误应提示重生成失败（对账失败）: %v", err)
	}
}

// TestVerifyCertAgainstCRLWithPolicy_Expired 验证过期 CRL 策略：
// 默认行为（VerifyCertAgainstCRL）过期仍放行（向后兼容）；
// FailOnExpired=true 时未吊销证书 fail-closed 拒绝；已吊销证书不受影响
// （吊销不可逆）。
func TestVerifyCertAgainstCRLWithPolicy_Expired(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	server, err := EnsureServerCert(certDir, "cloudcore")
	if err != nil {
		t.Fatalf("加载服务端证书失败: %v", err)
	}
	serverCert, err := x509.ParseCertificate(server.Certificate[0])
	if err != nil {
		t.Fatalf("解析服务端证书失败: %v", err)
	}

	// 构造已过期 CRL：8 天前签发（有效期 7 天 → 已过期 1 天），吊销 client。
	ca, err := loadCA(certDir)
	if err != nil {
		t.Fatalf("loadCA 失败: %v", err)
	}
	if err := writeCRLArtifactAt(ca, certDir, &revokedList{
		Entries: []revokedEntry{{Serial: clientCert.SerialNumber.Text(16), Time: time.Now().UTC().Format(time.RFC3339)}},
		Number:  1,
	}, time.Now().Add(-8*24*time.Hour)); err != nil {
		t.Fatalf("写已过期 CRL 失败: %v", err)
	}

	// 默认行为：过期仍放行（向后兼容），吊销仍生效。
	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil || !revoked {
		t.Errorf("默认行为下已吊销证书应被拒: revoked=%v err=%v", revoked, err)
	}
	ok, err := VerifyCertAgainstCRL(certDir, serverCert)
	if err != nil || ok {
		t.Errorf("默认行为下过期 CRL 应放行未吊销证书: revoked=%v err=%v", ok, err)
	}

	// FailOnExpired：未吊销证书 fail-closed 拒绝（错误含过期信息）。
	if _, err := VerifyCertAgainstCRLWithPolicy(certDir, serverCert, CRLVerifyOptions{FailOnExpired: true}); err == nil {
		t.Fatal("FailOnExpired 时过期 CRL 应拒绝未吊销证书，实际放行")
	} else if !strings.Contains(err.Error(), "过期") {
		t.Errorf("错误应提示 CRL 过期: %v", err)
	}
	// 已吊销证书不受过期影响（吊销不可逆）。
	revoked, err = VerifyCertAgainstCRLWithPolicy(certDir, clientCert, CRLVerifyOptions{FailOnExpired: true})
	if err != nil || !revoked {
		t.Errorf("FailOnExpired 时已吊销证书仍应被拒: revoked=%v err=%v", revoked, err)
	}
}

// TestRevokeCert_CorruptJSONIntactPEM 验证 crl.json 损坏 + crl.pem 完好
// 组合：RevokeCert fail-closed 报错（不静默清空吊销状态、不动 crl.pem）；
// Verify 只读 crl.pem 不受影响（被吊销证书仍被拒、未吊销证书仍放行）。
func TestRevokeCert_CorruptJSONIntactPEM(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	server, err := EnsureServerCert(certDir, "cloudcore")
	if err != nil {
		t.Fatalf("加载服务端证书失败: %v", err)
	}
	serverCert, err := x509.ParseCertificate(server.Certificate[0])
	if err != nil {
		t.Fatalf("解析服务端证书失败: %v", err)
	}
	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("首次吊销失败: %v", err)
	}

	crlPEMPath, crlJSONPath := CRLPaths(certDir)
	pemBefore, err := os.ReadFile(crlPEMPath)
	if err != nil {
		t.Fatalf("读取 crl.pem 失败: %v", err)
	}
	if err := os.WriteFile(crlJSONPath, []byte("garbage\n"), 0o644); err != nil {
		t.Fatalf("写损坏 crl.json 失败: %v", err)
	}

	// RevokeCert fail-closed：报错（含 crl.json 路径），不静默清空。
	err = RevokeCert(certDir, "edgeflow-test-node")
	if err == nil {
		t.Fatal("crl.json 损坏时 RevokeCert 应报错（fail-closed），实际成功")
	}
	if !strings.Contains(err.Error(), crlJSONPath) {
		t.Errorf("错误信息应含 crl.json 路径: %v", err)
	}
	pemAfter, err := os.ReadFile(crlPEMPath)
	if err != nil {
		t.Fatalf("读取 crl.pem 失败: %v", err)
	}
	if string(pemBefore) != string(pemAfter) {
		t.Error("RevokeCert 失败不应改动 crl.pem")
	}

	// Verify 只读 pem 不受影响。
	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil || !revoked {
		t.Errorf("crl.json 损坏不应影响 verify（被吊销证书仍被拒）: revoked=%v err=%v", revoked, err)
	}
	ok, err := VerifyCertAgainstCRL(certDir, serverCert)
	if err != nil || ok {
		t.Errorf("crl.json 损坏不应影响 verify（未吊销证书仍放行）: revoked=%v err=%v", ok, err)
	}
}

// TestReconcileCRLArtifacts 验证独立对账入口：json 领先时重生成 pem；
// 无产物时无事可做；crl.json 损坏时严格报错。
func TestReconcileCRLArtifacts(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)

	// 无产物：nil。
	if err := ReconcileCRLArtifacts(certDir); err != nil {
		t.Fatalf("无产物对账应 nil: %v", err)
	}
	// 目录不存在：nil。
	if err := ReconcileCRLArtifacts(filepath.Join(t.TempDir(), "ghost")); err != nil {
		t.Fatalf("目录不存在对账应 nil: %v", err)
	}

	// json 领先 pem：重生成。
	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("首次吊销失败: %v", err)
	}
	crlPEMPath, _ := CRLPaths(certDir)
	if err := os.Remove(crlPEMPath); err != nil {
		t.Fatalf("删除 crl.pem 失败: %v", err)
	}
	if err := ReconcileCRLArtifacts(certDir); err != nil {
		t.Fatalf("对账重生成失败: %v", err)
	}
	if _, err := os.Stat(crlPEMPath); err != nil {
		t.Errorf("对账后 crl.pem 应存在: %v", err)
	}

	// crl.json 损坏：严格报错。
	_, crlJSONPath := CRLPaths(certDir)
	if err := os.WriteFile(crlJSONPath, []byte("garbage\n"), 0o644); err != nil {
		t.Fatalf("写损坏 crl.json 失败: %v", err)
	}
	if err := ReconcileCRLArtifacts(certDir); err == nil {
		t.Fatal("crl.json 损坏时对账应报错（fail-closed），实际 nil")
	}
}

// ---- M3：CRL 锁失败降级日志 ----

// resetCRLDegradedWarnState 重置降级日志限频状态（测试隔离用，
// 避免其他测试的 5 分钟窗口干扰断言）。
func resetCRLDegradedWarnState() {
	crlLockDegradedWarnMu.Lock()
	crlLockDegradedWarnLast = time.Time{}
	crlLockDegradedWarnMu.Unlock()
}

// TestVerifyCertAgainstCRL_LockFailureDegrades 锁文件不可打开（crl.lock
// 路径被目录占据，OpenFile 必然失败）→ 降级为无锁校验：
//   - 功能语义不变：已吊销仍判吊销、未吊销仍放行，不返回锁错误；
//   - 降级 Warn 日志首条即出（含原因与影响）；
//   - 5 分钟内限频：连续第二次校验不再重复刷日志。
func TestVerifyCertAgainstCRL_LockFailureDegrades(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)
	serverCert, err := EnsureServerCert(certDir, "cloudcore")
	if err != nil {
		t.Fatalf("加载服务端证书失败: %v", err)
	}
	serverLeaf, err := x509.ParseCertificate(serverCert.Certificate[0])
	if err != nil {
		t.Fatalf("解析服务端证书失败: %v", err)
	}
	// 吊销客户端证书（顺带生成 crl.pem/crl.json/crl.lock）。
	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	// 把锁文件替换为目录：lockCRLFile 的 OpenFile(O_CREATE|O_RDWR) 必然
	// 失败（EISDIR），且非 os.IsNotExist → 触发无锁降级路径。
	lockPath := filepath.Join(certDir, FileCRLLock)
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("删除锁文件失败: %v", err)
	}
	if err := os.Mkdir(lockPath, 0o755); err != nil {
		t.Fatalf("创建目录占位锁路径失败: %v", err)
	}
	resetCRLDegradedWarnState()

	// 首次校验：降级 + Warn 日志（截获 fd 2 验证）。
	out := captureStderrFd(t, func() {
		revoked, err := VerifyCertAgainstCRLWithPolicy(certDir, clientCert, CRLVerifyOptions{})
		if err != nil {
			t.Errorf("锁失败降级不应报错（功能语义不变）: %v", err)
		}
		if !revoked {
			t.Error("降级后已吊销证书仍应判吊销（功能语义不变）")
		}
		// 未吊销证书（服务端）在降级路径下仍放行
		revokedServer, err := VerifyCertAgainstCRLWithPolicy(certDir, serverLeaf, CRLVerifyOptions{})
		if err != nil || revokedServer {
			t.Errorf("降级后未吊销证书应放行: revoked=%v err=%v", revokedServer, err)
		}
	})
	if !strings.Contains(out, "WARN") {
		t.Errorf("降级 Warn 日志未发出: %q", out)
	}
	if !strings.Contains(out, "降级") || !strings.Contains(out, "无锁") {
		t.Errorf("降级日志内容缺少原因/影响说明: %q", out)
	}

	// 再次校验：功能仍正确，且日志限频（5 分钟窗口内不重复）。
	out2 := captureStderrFd(t, func() {
		revoked, err := VerifyCertAgainstCRLWithPolicy(certDir, clientCert, CRLVerifyOptions{})
		if err != nil || !revoked {
			t.Errorf("二次降级校验失败: revoked=%v err=%v", revoked, err)
		}
	})
	if strings.Contains(out2, "降级") {
		t.Errorf("5 分钟内降级日志应限频不重复，实际: %q", out2)
	}
}

// TestWarnCRLLockDegraded_RateLimited 限频状态单元验证：窗口内多次调用
// 只更新一次时间戳；重置（或窗口过期）后恢复记录。
func TestWarnCRLLockDegraded_RateLimited(t *testing.T) {
	resetCRLDegradedWarnState()
	// 首次调用：记录时间
	first := time.Now()
	out := captureStderrFd(t, func() {
		warnCRLLockDegraded(errors.New("injected lock failure"))
	})
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "injected lock failure") {
		t.Errorf("首次降级日志应含原因: %q", out)
	}
	crlLockDegradedWarnMu.Lock()
	last := crlLockDegradedWarnLast
	crlLockDegradedWarnMu.Unlock()
	if last.Before(first) {
		t.Errorf("首次调用后应更新时间戳: last=%v first=%v", last, first)
	}
	// 窗口内再次调用：日志被限频（fd 2 无输出）
	out2 := captureStderrFd(t, func() {
		warnCRLLockDegraded(errors.New("injected lock failure"))
	})
	if out2 != "" {
		t.Errorf("窗口内应限频无输出，实际: %q", out2)
	}
	// 人为把窗口时间戳回拨到 5 分钟前 → 再次触发（模拟窗口过期）
	crlLockDegradedWarnMu.Lock()
	crlLockDegradedWarnLast = time.Now().Add(-crlLockDegradedWarnInterval)
	crlLockDegradedWarnMu.Unlock()
	out3 := captureStderrFd(t, func() {
		warnCRLLockDegraded(errors.New("injected lock failure"))
	})
	if !strings.Contains(out3, "WARN") {
		t.Errorf("窗口过期后应再次输出日志，实际: %q", out3)
	}
}
