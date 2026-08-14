package certs

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
