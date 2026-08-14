// Package certs 提供云边通道 mTLS 的证书管理能力（WBS 7.1 证书管理 + 7.4 云边认证）。
//
// 设计要点：
//   - 纯标准库实现（crypto/x509、crypto/rsa、crypto/tls），零第三方依赖；
//   - 单 CA 模型：自签 CA（CN=edgeflow-ca，10 年）为 cloudcore 签发服务端
//     证书（SAN: 127.0.0.1/localhost/cloudcore，1 年）、为 edgecore 签发
//     客户端证书（CN=edgeflow-<nodeID>，1 年，EKU 含 ClientAuth）；
//   - 全部生成/加载函数幂等：目标文件已存在则加载并校验，不重新生成；
//     证书目录不存在时自动创建（0755）；
//   - 私钥文件权限 0600，证书文件 0644；
//   - 证书布局（目录默认 data/certs/，可用环境变量覆盖）：
//     ca.crt / ca.key / cloudcore.crt / cloudcore.key / edgecore.crt / edgecore.key
//
// 算法选择：RSA 2048。
//   - 兼容性最广：边缘设备生态与 openssl 等工具链对 RSA 支持最完备；
//   - 性能考量：握手是低频操作，RSA 加解密开销可忽略（后续如需降低
//     边侧握手开销可平滑迁移 ECDSA P256，不影响证书布局与调用方）。
//
// 安全边界（本版本明确不做，见 docs/SECURITY.md）：
//   - CA 私钥保护依赖文件权限（0600）与部署侧密钥管理，生产建议离线签发；
//   - 证书轮换需人工编排（重新签发后滚动重启组件）；
//   - 吊销（CRL/OCSP）未实现：一旦私钥泄露，需轮换 CA 并全量重签。
package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// 证书目录与文件名约定（与装配层、hack/gen-certs.sh 保持一致，勿单独修改）。
const (
	// DefaultCertDir 是证书目录的默认值（相对进程工作目录）。
	DefaultCertDir = "data/certs"

	// FileCACert / FileCAKey 是自签 CA 的证书与私钥文件。
	FileCACert = "ca.crt"
	FileCAKey  = "ca.key"
	// FileServerCert / FileServerKey 是 cloudcore 服务端证书与私钥。
	FileServerCert = "cloudcore.crt"
	FileServerKey  = "cloudcore.key"
	// FileClientCert / FileClientKey 是 edgecore 客户端证书与私钥。
	FileClientCert = "edgecore.crt"
	FileClientKey  = "edgecore.key"
)

// 证书有效期与算法参数。
const (
	// caValidity 是 CA 证书有效期（10 年）。
	caValidity = 10 * 365 * 24 * time.Hour
	// leafValidity 是叶子证书（服务端/客户端）有效期（1 年）。
	leafValidity = 365 * 24 * time.Hour
	// rsaBits 是 RSA 密钥长度（2048 位，兼容性与安全性平衡）。
	rsaBits = 2048
	// caCommonName 是自签 CA 的 CN。
	caCommonName = "edgeflow-ca"
)

// CA 是自签 CA 的句柄：包含 CA 证书与私钥，用于签发叶子证书。
// 由 EnsureCA 返回，EnsureServerCert/EnsureClientCert 内部使用。
type CA struct {
	// CertDir 是证书所在目录。
	CertDir string
	// Cert 是解析后的 CA 证书。
	Cert *x509.Certificate
	// Key 是 CA 私钥（RSA）。
	Key *rsa.PrivateKey
}

// CertPaths 返回证书目录下的全部文件路径（便于日志与脚本对齐）。
func CertPaths(certDir string) (caCert, caKey, serverCert, serverKey, clientCert, clientKey string) {
	return filepath.Join(certDir, FileCACert),
		filepath.Join(certDir, FileCAKey),
		filepath.Join(certDir, FileServerCert),
		filepath.Join(certDir, FileServerKey),
		filepath.Join(certDir, FileClientCert),
		filepath.Join(certDir, FileClientKey)
}

// ensureDir 确保证书目录存在（不存在则创建，0755）。
func ensureDir(certDir string) error {
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return fmt.Errorf("创建证书目录 %s 失败: %w", certDir, err)
	}
	return nil
}

// EnsureCA 确保 CA 证书/私钥存在：
//   - 目录不存在 → 自动创建；
//   - ca.crt 与 ca.key 均已存在 → 加载并校验（损坏/不匹配报错，不重新生成，
//     避免静默轮换 CA 导致所有已签发叶子证书失效）；
//   - 只存在其一 → 报错（不完整状态，需人工处理，防止误用残留密钥）；
//   - 均不存在 → 生成自签 CA（CN=edgeflow-ca，有效期 10 年，RSA 2048）。
//
// 幂等：重复调用不重新生成，返回同一 CA。
func EnsureCA(certDir string) (*CA, error) {
	if err := ensureDir(certDir); err != nil {
		return nil, err
	}
	caCertPath, caKeyPath, _, _, _, _ := CertPaths(certDir)
	caCertExists := fileExists(caCertPath)
	caKeyExists := fileExists(caKeyPath)

	switch {
	case caCertExists && caKeyExists:
		return loadCA(certDir)
	case caCertExists != caKeyExists:
		return nil, fmt.Errorf("CA 证书/私钥不完整（%s 与 %s 必须同时存在），请人工检查证书目录 %s",
			FileCACert, FileCAKey, certDir)
	}

	// 全新生成：自签 CA
	key, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, fmt.Errorf("生成 CA 私钥失败: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             time.Now().Add(-time.Hour), // 容忍轻微时钟偏差
		NotAfter:              time.Now().Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("创建 CA 证书失败: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("解析新生成的 CA 证书失败: %w", err)
	}

	// 先写私钥（0600）再写证书（0644）；证书写入失败时回滚已写的私钥，
	// 保持目录状态一致（不会出现"有 key 无 crt"的不完整状态）。
	if err := writePrivateKey(caKeyPath, key); err != nil {
		return nil, err
	}
	if err := writeCert(caCertPath, der); err != nil {
		_ = os.Remove(caKeyPath)
		return nil, err
	}
	return &CA{CertDir: certDir, Cert: cert, Key: key}, nil
}

// EnsureServerCert 确保 cloudcore 服务端证书存在（cloudcore.crt/key）：
//   - 已存在 → 加载并返回（幂等，不重新生成）；
//   - 不存在 → 由 CA 签发：CN=cn（默认 cloudcore），
//     SAN 含 IP:127.0.0.1、DNS:localhost、DNS:cloudcore，有效期 1 年，
//     EKU 含 ServerAuth（供边缘侧验证云身份）。
//
// 需要 CA 已就绪（内部调用 EnsureCA）。
func EnsureServerCert(certDir, cn string) (*tls.Certificate, error) {
	return ensureLeafCert(certDir, cn, x509.ExtKeyUsageServerAuth, FileServerCert, FileServerKey)
}

// EnsureClientCert 确保 edgecore 客户端证书存在（edgecore.crt/key）：
//   - 已存在 → 加载并返回（幂等，不重新生成）；
//   - 不存在 → 由 CA 签发：CN=cn（约定 edgeflow-<nodeID>），有效期 1 年，
//     EKU 含 ClientAuth（供云端验证边身份）。
//
// 说明：CN 仅作可读标识，云端认证依据是「证书由可信 CA 签发」本身
// （RequireAndVerifyClientCert 校验签名链），不校验 CN 与 nodeID 的映射
// （nodeID 与证书绑定属后续增强，见 docs/SECURITY.md）。
// 需要 CA 已就绪（内部调用 EnsureCA）。
func EnsureClientCert(certDir, cn string) (*tls.Certificate, error) {
	return ensureLeafCert(certDir, cn, x509.ExtKeyUsageClientAuth, FileClientCert, FileClientKey)
}

// ensureLeafCert 是服务端/客户端证书的公共实现（幂等生成或加载）。
func ensureLeafCert(certDir, cn string, eku x509.ExtKeyUsage, certFile, keyFile string) (*tls.Certificate, error) {
	if cn == "" {
		cn = "edgeflow-leaf" // 兜底 CN，正常装配都会显式传入
	}
	certPath := filepath.Join(certDir, certFile)
	keyPath := filepath.Join(certDir, keyFile)
	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)

	switch {
	case certExists && keyExists:
		return loadLeafCert(certPath, keyPath)
	case certExists != keyExists:
		return nil, fmt.Errorf("证书/私钥不完整（%s 与 %s 必须同时存在），请人工检查证书目录 %s",
			certFile, keyFile, certDir)
	}

	// 全新签发：先确保 CA 就绪，再由 CA 签名
	ca, err := EnsureCA(certDir)
	if err != nil {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, fmt.Errorf("生成 %s 私钥失败: %w", cn, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
	}
	if eku == x509.ExtKeyUsageServerAuth {
		// 服务端证书 SAN：覆盖本机回环与容器内 DNS 名（云边通道默认连接
		// 方式为 wss://127.0.0.1:10000，边缘侧按地址校验 ServerName）。
		tmpl.DNSNames = []string{"localhost", "cloudcore"}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("签发 %s 证书失败: %w", cn, err)
	}
	if err := writePrivateKey(keyPath, key); err != nil {
		return nil, err
	}
	if err := writeCert(certPath, der); err != nil {
		_ = os.Remove(keyPath)
		return nil, err
	}
	// 内存态直接由 DER 构造，不经过 PEM 往返（避免编码不一致）
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}, nil
}

// LoadTLSConfig 加载 CA + 叶子证书并组装 tls.Config：
//   - isServer=true（CloudHub 侧）：携带 cloudcore.crt/key，ClientCAs=CA 池，
//     ClientAuth=RequireAndVerifyClientCert（强制要求并验证客户端证书，mTLS 核心）；
//   - isServer=false（EdgeHub 侧）：携带 edgecore.crt/key，RootCAs=CA 池，
//     以 wss:// 拨号时自动校验收到的服务端证书。
//
// 两侧均强制 TLS1.2+（拒绝 TLS1.0/1.1 等已废弃协议）。
// 调用前需先 EnsureCA + EnsureServerCert/EnsureClientCert。
func LoadTLSConfig(certDir string, isServer bool) (*tls.Config, error) {
	ca, err := loadCA(certDir)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)

	if isServer {
		leaf, err := EnsureServerCert(certDir, "cloudcore")
		if err != nil {
			return nil, fmt.Errorf("加载服务端证书失败: %w", err)
		}
		return &tls.Config{
			Certificates: []tls.Certificate{*leaf},
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		}, nil
	}

	leaf, err := EnsureClientCert(certDir, "edgeflow-edgecore")
	if err != nil {
		return nil, fmt.Errorf("加载客户端证书失败: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// loadCA 从磁盘加载并解析 CA（内部用，要求两文件都存在）。
func loadCA(certDir string) (*CA, error) {
	caCertPath, caKeyPath, _, _, _, _ := CertPaths(certDir)
	certPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("读取 CA 证书 %s 失败: %w", caCertPath, err)
	}
	keyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, fmt.Errorf("读取 CA 私钥 %s 失败: %w", caKeyPath, err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("CA 证书 %s 不是合法的 PEM 证书（文件损坏？）", caCertPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析 CA 证书 %s 失败: %w", caCertPath, err)
	}
	key, err := parseRSAPrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("解析 CA 私钥 %s 失败: %w", caKeyPath, err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("CA 证书 %s 的 IsCA 为 false，不是合法 CA", caCertPath)
	}
	return &CA{CertDir: certDir, Cert: cert, Key: key}, nil
}

// loadLeafCert 从磁盘加载叶子证书（内部用，要求两文件都存在）。
func loadLeafCert(certPath, keyPath string) (*tls.Certificate, error) {
	leaf, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("加载证书 %s 失败（文件损坏或公私钥不匹配）: %w", certPath, err)
	}
	return &leaf, nil
}

// parseRSAPrivateKey 解析 PEM 格式的 RSA 私钥（兼容 PKCS#1 与 PKCS#8 两种编码）。
func parseRSAPrivateKey(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("PEM 解析失败（文件损坏？）")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("私钥不是 RSA 类型")
	}
	return nil, errors.New("私钥不是合法的 PKCS#1/PKCS#8 RSA 密钥")
}

// randomSerial 生成 128 位随机正序列号（防序列号猜测/碰撞）。
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("生成证书序列号失败: %w", err)
	}
	return serial, nil
}

// writePrivateKey 以 0600 权限写私钥文件（PEM，PKCS#1 编码）。
func writePrivateKey(path string, key *rsa.PrivateKey) error {
	pemData := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, pemData, 0o600); err != nil {
		return fmt.Errorf("写入私钥 %s 失败: %w", path, err)
	}
	return nil
}

// writeCert 以 0644 权限写证书文件（PEM 编码）。
func writeCert(path string, der []byte) error {
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemData, 0o644); err != nil {
		return fmt.Errorf("写入证书 %s 失败: %w", path, err)
	}
	return nil
}

// fileExists 判断文件是否存在且为普通文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
