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
//     crl.pem（X.509 CRL 吊销列表产物） / crl.json（吊销序列号持久化记录）
//     crl.lock（吊销写路径进程级锁文件，flock 目标，从不被替换/删除）
//
// 算法选择：RSA 2048。
//   - 兼容性最广：边缘设备生态与 openssl 等工具链对 RSA 支持最完备；
//   - 性能考量：握手是低频操作，RSA 加解密开销可忽略（后续如需降低
//     边侧握手开销可平滑迁移 ECDSA P256，不影响证书布局与调用方）。
//
// 安全边界（本版本明确不做，见 docs/SECURITY.md）：
//   - CA 私钥保护依赖文件权限（0600）与部署侧密钥管理，生产建议离线签发；
//   - 证书轮换需人工编排（重新签发后滚动重启组件）；
//   - 吊销（CRL + OCSP）已实现：keadm cert revoke（序列号入 crl.json +
//     生成 crl.pem），cloudcore /ocsp 标准 OCSP responder + 客户端查询
//     （pkg/certs/ocsp.go，RFC 6960，状态与 CRL 同源 crl.json）。
package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"edgeflow/pkg/log"
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
	// FileCRL 是吊销列表产物文件（PEM 编码的 X.509 CRL，供校验方消费）。
	FileCRL = "crl.pem"
	// FileCRLJSON 是吊销序列号持久化记录（crl.json，来源记录；
	// crl.pem 由它派生，二者一致性见 RevokeCert 的注释）。
	FileCRLJSON = "crl.json"
	// FileCRLLock 是吊销写路径的进程级锁文件（syscall.Flock 目标）。
	// 锁文件独立于 crl.json/crl.pem：这两个文件会被原子替换（rename），
	// 若直接锁它们，锁会随旧 inode 失效（换名后新 inode 可被他人立即
	// 锁住），互斥语义被破坏；crl.lock 从不被替换/删除，锁语义稳定。
	FileCRLLock = "crl.lock"
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
	// crlValidity 是 CRL 的 NextUpdate 间隔（每次吊销重新生成，7 天足够
	// 覆盖人工分发周期）。
	crlValidity = 7 * 24 * time.Hour
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

// CRLPaths 返回吊销列表相关文件路径：crlPEM 是 CRL 产物（X.509 CRL，
// PEM 编码），crlJSON 是吊销序列号持久化记录（来源记录）。
func CRLPaths(certDir string) (crlPEM, crlJSON string) {
	return filepath.Join(certDir, FileCRL), filepath.Join(certDir, FileCRLJSON)
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
// 需要 CA 已就绪（内部调用 EnsureCA）。跨主机部署时请使用
// EnsureServerCertWithSANs 注入集群 Service DNS / 节点 IP（M4B P1-4）。
func EnsureServerCert(certDir, cn string) (*tls.Certificate, error) {
	return EnsureServerCertWithSANs(certDir, cn, nil, nil)
}

// EnsureServerCertWithSANs 同 EnsureServerCert，但允许显式注入 SAN：
// ips 与 dnsNames 为空时回退默认（127.0.0.1/localhost/cloudcore）。
// 供跨主机/集群 Service 场景使用（环境变量 EDGEFLOW_CLOUDCORE_TLS_SAN）。
func EnsureServerCertWithSANs(certDir, cn string, ips []net.IP, dnsNames []string) (*tls.Certificate, error) {
	return ensureLeafCert(certDir, cn, x509.ExtKeyUsageServerAuth, FileServerCert, FileServerKey, ips, dnsNames)
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
	return ensureLeafCert(certDir, cn, x509.ExtKeyUsageClientAuth, FileClientCert, FileClientKey, nil, nil)
}

// ensureLeafCert 是服务端/客户端证书的公共实现（幂等生成或加载）。
func ensureLeafCert(certDir, cn string, eku x509.ExtKeyUsage, certFile, keyFile string,
	ips []net.IP, dnsNames []string) (*tls.Certificate, error) {
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
		// 服务端证书 SAN：显式注入优先；为空回退本机回环与容器内 DNS 名
		// （云边通道默认连接方式为 wss://127.0.0.1:10000，边缘侧按地址
		// 校验 ServerName）。跨主机部署必须显式注入（M4B P1-4）。
		tmpl.DNSNames = dnsNames
		tmpl.IPAddresses = ips
		if len(dnsNames) == 0 && len(ips) == 0 {
			tmpl.DNSNames = []string{"localhost", "cloudcore"}
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
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

// RotateClientCert 强制重新签发 edgecore 客户端证书（轮换，WBS 7.1/ROADMAP 7.1）。
//
// 与 EnsureClientCert 的幂等语义不同：轮换必须生成新密钥 + 新序列号并覆盖
// 旧文件（旧证书即将到期/私钥泄露场景）。约束：
//   - 旧证书必须已存在且 CN 与请求一致（轮换不改变节点身份，防止 CN 漂移）；
//   - CA 必须已存在且可加载（严格加载，绝不自动创建——轮换场景静默新建 CA
//     会让所有已签发叶子证书失效）；
//   - 事务化落盘：新证书/私钥先写入同目录临时文件并回读校验，全部就绪后
//     再原子替换旧文件；任一步失败只清理临时文件，旧证书保持原状。
//     （两个 Rename 之间存在极短的新旧混搭窗口，属已知可接受窗口；
//     调用方应先备份旧证书再轮换，见 keadm cert rotate。）
//
// 返回新签发的叶子证书（与 Ensure* 返回值形态一致）。
func RotateClientCert(certDir, cn string) (*tls.Certificate, error) {
	return rotateLeafCert(certDir, cn, x509.ExtKeyUsageClientAuth, FileClientCert, FileClientKey, nil, nil)
}

// RotateServerCertWithSANs 强制重新签发 cloudcore 服务端证书（轮换）。
// 与 RotateClientCert 同语义；ips/dnsNames 是重新签发的 SAN（为空回退默认
// 127.0.0.1/localhost/cloudcore）。调用方应读取旧证书的 SAN 原样传入，
// 避免轮换后服务端证书不再覆盖边缘节点访问地址。
func RotateServerCertWithSANs(certDir, cn string, ips []net.IP, dnsNames []string) (*tls.Certificate, error) {
	return rotateLeafCert(certDir, cn, x509.ExtKeyUsageServerAuth, FileServerCert, FileServerKey, ips, dnsNames)
}

// rotateLeafCert 是服务端/客户端证书轮换的公共实现（见 RotateClientCert 的
// 约束说明；证书模板与 ensureLeafCert 保持一致，保证轮换前后证书约定不变）。
func rotateLeafCert(certDir, cn string, eku x509.ExtKeyUsage, certFile, keyFile string,
	ips []net.IP, dnsNames []string) (*tls.Certificate, error) {
	if err := ensureDir(certDir); err != nil {
		return nil, err
	}
	certPath := filepath.Join(certDir, certFile)
	keyPath := filepath.Join(certDir, keyFile)

	// 轮换必须基于已有证书：缺旧证书/私钥则拒绝（轮换不是初始化，
	// 防止误把轮换当首次签发而绕过 CA 就绪检查）。
	if !fileExists(certPath) || !fileExists(keyPath) {
		return nil, fmt.Errorf("没有可轮换的旧证书（%s/%s 不存在）；轮换仅支持已存在的证书", certFile, keyFile)
	}

	// 旧证书加载 + CN 一致性校验：轮换不得改变节点身份。
	oldLeaf, err := loadLeafCert(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("加载旧证书失败: %w", err)
	}
	oldCert, err := x509.ParseCertificate(oldLeaf.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("解析旧证书失败: %w", err)
	}
	if oldCert.Subject.CommonName != cn {
		return nil, fmt.Errorf("旧证书 CN=%q 与请求 CN=%q 不一致，拒绝轮换（轮换不改变节点身份）",
			oldCert.Subject.CommonName, cn)
	}

	// CA 严格加载（loadCA 而非 EnsureCA）：轮换绝不自动创建 CA。
	ca, err := loadCA(certDir)
	if err != nil {
		return nil, fmt.Errorf("加载 CA 失败（轮换需要现有 CA，不会自动创建）: %w", err)
	}

	// 新密钥 + 新序列号 + 证书模板（与 ensureLeafCert 完全一致的约定）。
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
		tmpl.DNSNames = dnsNames
		tmpl.IPAddresses = ips
		if len(dnsNames) == 0 && len(ips) == 0 {
			tmpl.DNSNames = []string{"localhost", "cloudcore"}
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("签发 %s 证书失败: %w", cn, err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	// 事务化落盘：临时文件（同目录同设备，CreateTemp 初始 0600）→ 回读校验
	// → 全部就绪后逐个 Rename 原子替换。任一步失败只清理临时文件，
	// 旧证书/私钥保持原状（配合调用方的事先备份，实现「轮换失败不破坏旧证书」）。
	tmpCert, err := os.CreateTemp(certDir, "."+certFile+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时证书文件失败: %w", err)
	}
	tmpCertPath := tmpCert.Name()
	defer func() { _ = os.Remove(tmpCertPath) }()
	_ = tmpCert.Close()

	tmpKey, err := os.CreateTemp(certDir, "."+keyFile+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时私钥文件失败: %w", err)
	}
	tmpKeyPath := tmpKey.Name()
	defer func() { _ = os.Remove(tmpKeyPath) }()
	_ = tmpKey.Close()

	if err := os.WriteFile(tmpCertPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("写入临时证书失败: %w", err)
	}
	if err := os.Chmod(tmpCertPath, 0o644); err != nil {
		return nil, fmt.Errorf("设置临时证书权限失败: %w", err)
	}
	if err := os.WriteFile(tmpKeyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("写入临时私钥失败: %w", err)
	}

	// 回读校验：临时文件必须能组成合法密钥对（防止落盘损坏后替换旧证书）。
	if _, err := tls.LoadX509KeyPair(tmpCertPath, tmpKeyPath); err != nil {
		return nil, fmt.Errorf("回读校验新证书失败: %w", err)
	}

	// 原子替换：先证书后私钥（窗口内新旧混搭属已知可接受窗口，见函数注释）。
	if err := os.Rename(tmpCertPath, certPath); err != nil {
		return nil, fmt.Errorf("替换证书 %s 失败: %w", certPath, err)
	}
	if err := os.Rename(tmpKeyPath, keyPath); err != nil {
		return nil, fmt.Errorf("替换私钥 %s 失败（证书已更新，请从备份恢复私钥）: %w", keyPath, err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}, nil
}

// RevokeCert 吊销指定 CN 的节点证书（WBS 7.1 吊销能力，keadm cert revoke）：
//
//	① 按 CN 定位节点证书（优先客户端证书 edgecore.crt，其次服务端证书
//	   cloudcore.crt），读取其序列号；证书不存在/不可解析 → 明确错误；
//	② 把序列号追加进吊销列表（crl.json 持久化记录，来源记录）并生成/更新
//	   CRL 产物（crl.pem，x509.CreateRevocationList 签名，PEM 编码）；
//	③ 重复吊销同一证书幂等：不报错、不重复序列号，仅确保 crl.pem 产物与
//	   吊销列表一致（缺失/损坏/不一致时自动重新生成）。
//
// 约束：
//   - CA 严格加载（loadCA 而非 EnsureCA）：吊销绝不自动创建 CA
//     （静默新建 CA 会让吊销列表失去意义）；
//   - CA 私钥已具备 CRLSign 用途（EnsureCA 设置 KeyUsageCertSign|
//     KeyUsageCRLSign），且 CA 证书自带 SubjectKeyId（IsCA 自动生成），
//     满足 x509.CreateRevocationList 的签发前置条件；
//   - 只更新吊销列表与 CRL，不删除证书文件（已分发证书由对端按 CRL 拒绝）。
//
// 并发与一致性策略：吊销写路径（crl.json/crl.pem 读-改-写全程）持有
// crl.lock 进程级文件锁（syscall.Flock，阻塞等待，见 lockCRLFile），
// 并发 RevokeCert 互斥，不会读-改-写竞争丢序列号。crl.json 是来源记录
// （先写），crl.pem 是由它派生的签名产物（后写）。写入失败时 crl.json
// 可能领先于 crl.pem，重试（幂等路径）会比对两者并重新生成 crl.pem，
// 不会丢已吊销序列号；crl.json 缺失时从 crl.pem 恢复序列号（见
// loadRevokedList），两者均损坏则报错不静默清空。
func RevokeCert(certDir, cn string) error {
	if err := ensureDir(certDir); err != nil {
		return err
	}
	f, err := lockCRLFile(certDir)
	if err != nil {
		return fmt.Errorf("获取吊销锁失败: %w", err)
	}
	defer unlockCRLFile(f)

	// CA 严格加载：吊销需要现有 CA（不自动创建）。
	ca, err := loadCA(certDir)
	if err != nil {
		return fmt.Errorf("加载 CA 失败（吊销需要现有 CA，不会自动创建）: %w", err)
	}
	if len(ca.Cert.SubjectKeyId) == 0 {
		return fmt.Errorf("CA 证书 %s 缺少 SubjectKeyId，无法签发 CRL（请重新初始化 CA）",
			filepath.Join(certDir, FileCACert))
	}

	// ① 按 CN 定位节点证书（不存在/不可解析 → 明确错误）。
	leaf, _, err := findLeafByCN(certDir, cn)
	if err != nil {
		return err
	}

	// ② 序列号入列 + ③ 确保 crl.pem 产物一致（锁内读-改-写全程）。
	return revokeSerialLocked(ca, certDir, leaf.SerialNumber.Text(16))
}

// RevokeCertBySerial 按序列号吊销证书（keadm cert revoke --serial）：
// 不依赖证书文件，序列号直接入列——轮换后旧证书泄露场景下，可吊销
// 已不在证书目录中的历史序列号（序列号不要求存在于任何已知证书）。
//
// serialHex 接受大小写十六进制（可选 0x/0X 前缀），统一归一化为小写
// 规范形式（去前导零）；非法序列号（非十六进制/非正整数/空）→ 明确错误。
// 其余语义与 RevokeCert 一致：进程锁互斥、幂等、CRL Number 单调递增。
func RevokeCertBySerial(certDir, serialHex string) error {
	serialHex = strings.TrimPrefix(strings.TrimPrefix(serialHex, "0x"), "0X")
	n, ok := new(big.Int).SetString(serialHex, 16)
	if !ok || n.Sign() <= 0 {
		return fmt.Errorf("吊销序列号 %q 不是合法的正整数十六进制", serialHex)
	}
	serialHex = n.Text(16) // 归一化：小写 + 去前导零

	if err := ensureDir(certDir); err != nil {
		return err
	}
	f, err := lockCRLFile(certDir)
	if err != nil {
		return fmt.Errorf("获取吊销锁失败: %w", err)
	}
	defer unlockCRLFile(f)

	ca, err := loadCA(certDir)
	if err != nil {
		return fmt.Errorf("加载 CA 失败（吊销需要现有 CA，不会自动创建）: %w", err)
	}
	if len(ca.Cert.SubjectKeyId) == 0 {
		return fmt.Errorf("CA 证书 %s 缺少 SubjectKeyId，无法签发 CRL（请重新初始化 CA）",
			filepath.Join(certDir, FileCACert))
	}
	return revokeSerialLocked(ca, certDir, serialHex)
}

// revokeSerialLocked 在锁内把序列号追加进吊销列表并确保 crl.pem 产物一致
// （幂等：已入列则不再追加；CRL Number 的递增统一由 ensureCRLArtifact 在
// 每次新签发时完成，见其注释）。serialHex 须为小写规范十六进制。
func revokeSerialLocked(ca *CA, certDir, serialHex string) error {
	// 加载现有吊销列表（crl.json 优先；缺失时从 crl.pem 恢复；都缺失为空）。
	list, err := loadRevokedList(certDir)
	if err != nil {
		return err
	}
	if !list.contains(serialHex) {
		list.Entries = append(list.Entries, revokedEntry{
			Serial: serialHex,
			Time:   time.Now().UTC().Format(time.RFC3339),
		})
	}
	// 确保 crl.pem 产物与列表一致（幂等路径自愈：缺失/损坏/不一致 → 重生成）。
	return ensureCRLArtifact(ca, certDir, list)
}

// lockCRLFile / unlockCRLFile 实现按平台分文件：
//   - crl_lock_unix.go（!windows）：syscall.Flock（LOCK_EX/LOCK_UN）；
//   - crl_lock_windows.go（windows）：LockFileEx/UnlockFileEx（标准库 syscall，
//     零新依赖）。
// 语义统一：crl.lock 进程级独占锁，锁竞争时阻塞等待；crl.lock 从不被
// 替换/删除，锁语义稳定（writeFileAtomic 换名不影响）。

// crlLockDegradedWarnInterval 是锁失败降级 Warn 日志的最小间隔
// （避免每条握手刷屏；降级本身不改变校验功能语义）。
const crlLockDegradedWarnInterval = 5 * time.Minute

// crlLockDegradedWarn 是锁失败降级日志的限频状态（首次 + 每 5 分钟最多一条）。
var (
	crlLockDegradedWarnMu   sync.Mutex
	crlLockDegradedWarnLast time.Time
)

// warnCRLLockDegraded 记录吊销锁获取失败导致的无锁降级（限频 Warn 日志，
// 经 pkg/log 输出到 stderr）。日志含原因（err）与影响（吊销即时性降级：
// crl.json 领先 crl.pem 时无法即时重生成）；校验功能语义不变。
func warnCRLLockDegraded(err error) {
	crlLockDegradedWarnMu.Lock()
	defer crlLockDegradedWarnMu.Unlock()
	if time.Since(crlLockDegradedWarnLast) < crlLockDegradedWarnInterval {
		return
	}
	crlLockDegradedWarnLast = time.Now()
	log.Warnf("CRL 吊销校验降级为无锁路径：获取吊销锁失败（原因: %v；影响: crl.json 领先 crl.pem 时吊销无法即时生效，校验功能本身不受影响）", err)
}

// VerifyCertAgainstCRL 校验证书是否在吊销列表（WBS 7.1）：
//
//	吊销 → (true, nil)；未吊销 → (false, nil)。
//
// 兼容性约定：
//   - CRL 文件（crl.pem）缺失 → 视为无吊销（向后兼容，返回 false, nil）；
//   - CRL 存在但损坏/无法解析 → 返回错误（错误信息含 CRL 路径），不 panic；
//   - CRL 必须由本目录 CA 签发（CheckSignatureFrom 校验），防止伪造/错配
//     的 CRL 绕过吊销；
//   - CRL 过期（NextUpdate 已过）→ 默认仍放行（向后兼容）；需要
//     fail-closed 语义用 VerifyCertAgainstCRLWithPolicy。
//
// 吊销立即生效：校验前先做 crl.json↔crl.pem 对账（进程锁内），crl.json
// 序列号集合领先 crl.pem（如崩溃窗口：json 已写、pem 未写）时自动重生成
// crl.pem（原子写），再基于新产物校验；对账失败（如 json 损坏且 pem 不可
// 用、或 json 领先但无法重生成）→ 返回错误（fail-closed）。crl.json 损坏
// 但 crl.pem 完好时对账不阻断，仍按 pem 校验（与旧行为一致）。
func VerifyCertAgainstCRL(certDir string, cert *x509.Certificate) (bool, error) {
	return VerifyCertAgainstCRLWithPolicy(certDir, cert, CRLVerifyOptions{})
}

// CRLVerifyOptions 是 VerifyCertAgainstCRLWithPolicy 的策略选项。
// 零值 = 与 VerifyCertAgainstCRL 完全一致的默认行为（过期仍放行）。
type CRLVerifyOptions struct {
	// FailOnExpired 为 true 时，CRL 已过期（now > NextUpdate）且证书未被
	// 吊销 → 返回错误（fail-closed，拒绝以陈旧 CRL 放行证书）。
	// 已吊销的证书不受影响（吊销不可逆，仍返回 true, nil）。
	FailOnExpired bool
}

// VerifyCertAgainstCRLWithPolicy 是 VerifyCertAgainstCRL 的策略化版本：
// 行为与 VerifyCertAgainstCRL 一致（含 json↔pem 对账自愈），并额外支持
// opts.FailOnExpired（过期 CRL fail-closed 拒绝）。
func VerifyCertAgainstCRLWithPolicy(certDir string, cert *x509.Certificate, opts CRLVerifyOptions) (bool, error) {
	if cert == nil {
		return false, errors.New("VerifyCertAgainstCRL: cert 为 nil")
	}

	// 持锁对账（crl.json 领先时重生成 crl.pem），再基于新产物校验。
	f, err := lockCRLFile(certDir)
	if err != nil {
		if os.IsNotExist(err) {
			// 证书目录不存在 → 无 CRL 可言（向后兼容：视为无吊销）。
			return false, nil
		}
		// 无法取得锁（如只读目录无法创建 crl.lock）：降级为无锁校验。
		// 只读目录不存在并发写者，无锁对账/校验是安全的；对账失败
		// （如 json 领先但无法重生成）→ fail-closed 返回错误。
		warnCRLLockDegraded(err)
		if err := reconcileCRLForVerify(certDir); err != nil {
			return false, err
		}
		return verifyCRLUnlocked(certDir, cert, opts)
	}
	defer unlockCRLFile(f)
	if err := reconcileCRLForVerify(certDir); err != nil {
		return false, err
	}
	return verifyCRLUnlocked(certDir, cert, opts)
}

// verifyCRLUnlocked 是锁外/锁内的纯校验逻辑（不涉及锁与对账）：
// 读取 crl.pem → 解析 → CA 签名校验 → 序列号比对 → 可选过期策略。
func verifyCRLUnlocked(certDir string, cert *x509.Certificate, opts CRLVerifyOptions) (bool, error) {
	crlPEMPath, _ := CRLPaths(certDir)
	b, err := os.ReadFile(crlPEMPath)
	if os.IsNotExist(err) {
		return false, nil // CRL 缺失视为无吊销（向后兼容）
	}
	if err != nil {
		return false, fmt.Errorf("读取 CRL %s 失败: %w", crlPEMPath, err)
	}
	rl, err := parseCRLPEM(b)
	if err != nil {
		return false, fmt.Errorf("解析 CRL %s 失败: %w", crlPEMPath, err)
	}
	// 吊销状态依赖 CA 签名链：CRL 必须由本 CA 签发（防伪造 CRL 绕过吊销）。
	ca, err := loadCA(certDir)
	if err != nil {
		return false, fmt.Errorf("加载 CA 失败（校验吊销状态需要 CA 验证 CRL 签名）: %w", err)
	}
	if err := rl.CheckSignatureFrom(ca.Cert); err != nil {
		return false, fmt.Errorf("CRL %s 签名校验失败（非本 CA 签发或文件损坏）: %w", crlPEMPath, err)
	}
	for _, e := range rl.RevokedCertificateEntries {
		if e.SerialNumber != nil && e.SerialNumber.Cmp(cert.SerialNumber) == 0 {
			return true, nil // 已吊销：吊销不可逆，不因 CRL 过期而放行
		}
	}
	if opts.FailOnExpired && time.Now().After(rl.NextUpdate) {
		return false, fmt.Errorf("CRL %s 已过期（NextUpdate=%s），fail-closed 拒绝（证书吊销状态无法确认）",
			crlPEMPath, rl.NextUpdate.Format(time.RFC3339))
	}
	return false, nil
}

// ReconcileCRLArtifacts 对账 crl.json 与 crl.pem（独立对账入口，可启动时
// 调用；VerifyCertAgainstCRL 内部也会自动对账）：crl.json 序列号集合领先
// crl.pem 时自动重生成 crl.pem（原子写，CRL Number +1），使吊销立即生效。
//
//   - 证书目录/crl.json 缺失 → 无事可做（nil）；
//   - crl.json 损坏 → 返回错误（fail-closed，不静默清空吊销状态）；
//   - 对账只向前补齐：crl.pem 领先（含手工编辑 crl.json 移除条目）时保持
//     不动，不会回退已吊销序列号。
func ReconcileCRLArtifacts(certDir string) error {
	if info, err := os.Stat(certDir); err != nil || !info.IsDir() {
		return nil // 无证书目录 → 无吊销产物可对账
	}
	f, err := lockCRLFile(certDir)
	if err != nil {
		return fmt.Errorf("获取吊销锁失败: %w", err)
	}
	defer unlockCRLFile(f)
	return reconcileCRLForVerifyStrict(certDir)
}

// reconcileCRLForVerifyStrict 是严格版对账（ReconcileCRLArtifacts 用）：
// crl.json 损坏直接报错。
func reconcileCRLForVerifyStrict(certDir string) error {
	_, crlJSONPath := CRLPaths(certDir)
	jb, err := os.ReadFile(crlJSONPath)
	if os.IsNotExist(err) {
		return nil // 无来源记录 → 无事可做
	}
	if err != nil {
		return fmt.Errorf("读取吊销列表记录 %s 失败: %w", crlJSONPath, err)
	}
	var list revokedList
	if err := json.Unmarshal(jb, &list); err != nil {
		return fmt.Errorf("解析吊销列表记录 %s 失败（文件损坏？）: %w", crlJSONPath, err)
	}
	if err := validateRevokedList(&list); err != nil {
		return fmt.Errorf("吊销列表记录 %s 内容非法: %w", crlJSONPath, err)
	}
	if crlCoveredByPEM(certDir, &list) {
		return nil
	}
	return regenerateCRLFromList(certDir, &list)
}

// reconcileCRLForVerify 是校验侧对账（VerifyCertAgainstCRLWithPolicy 用）：
// crl.json 损坏但 crl.pem 完好 → 不阻断（维持 verify 只读 pem 的旧语义）；
// crl.json 损坏且 crl.pem 缺失/损坏 → 错误（fail-closed：吊销记录存在但
// 不可用，无法确认吊销状态）。
func reconcileCRLForVerify(certDir string) error {
	crlPEMPath, crlJSONPath := CRLPaths(certDir)
	jb, err := os.ReadFile(crlJSONPath)
	if os.IsNotExist(err) {
		return nil // 无来源记录 → 只按 crl.pem 校验（旧语义）
	}
	if err != nil {
		return fmt.Errorf("读取吊销列表记录 %s 失败: %w", crlJSONPath, err)
	}
	var list revokedList
	if err := json.Unmarshal(jb, &list); err != nil {
		return reconcileJSONCorruptFallback(certDir, crlPEMPath, crlJSONPath, err)
	}
	if err := validateRevokedList(&list); err != nil {
		return reconcileJSONCorruptFallback(certDir, crlPEMPath, crlJSONPath, err)
	}
	if crlCoveredByPEM(certDir, &list) {
		return nil
	}
	if len(list.Entries) == 0 {
		return nil // 空记录无需生成空 CRL（保持无 CRL 旧语义）
	}
	return regenerateCRLFromList(certDir, &list)
}

// reconcileJSONCorruptFallback 处理 crl.json 损坏时的降级判定：
// crl.pem 完好可用 → 不阻断（verify 只读 pem 的旧语义）；否则 fail-closed。
func reconcileJSONCorruptFallback(certDir, crlPEMPath, crlJSONPath string, cause error) error {
	if b, perr := os.ReadFile(crlPEMPath); perr == nil {
		if _, perr2 := parseCRLPEM(b); perr2 == nil {
			return nil // pem 完好：对账不阻断，按 pem 校验
		}
	}
	return fmt.Errorf("吊销列表记录 %s 损坏且 CRL %s 不可用，无法确认吊销状态（fail-closed）: %w",
		crlJSONPath, crlPEMPath, cause)
}

// crlCoveredByPEM 判断 crl.pem 的吊销序列号集合是否已覆盖 crl.json 记录
// （顺序无关；只向前补齐——pem 额外多出的序列号不影响判定）。
func crlCoveredByPEM(certDir string, list *revokedList) bool {
	crlPEMPath, _ := CRLPaths(certDir)
	b, err := os.ReadFile(crlPEMPath)
	if err != nil {
		return false
	}
	rl, err := parseCRLPEM(b)
	if err != nil {
		return false
	}
	have := make(map[string]bool, len(rl.RevokedCertificateEntries))
	for _, e := range rl.RevokedCertificateEntries {
		if e.SerialNumber != nil {
			have[e.SerialNumber.Text(16)] = true
		}
	}
	for _, e := range list.Entries {
		if !have[e.Serial] {
			return false
		}
	}
	return true
}

// regenerateCRLFromList 由 crl.json 记录重生成 crl.pem（对账路径）：
// 需要 CA（加载失败 → 明确错误，fail-closed）。
func regenerateCRLFromList(certDir string, list *revokedList) error {
	ca, err := loadCA(certDir)
	if err != nil {
		return fmt.Errorf("吊销列表记录领先 CRL 产物，重生成失败（fail-closed）: %w", err)
	}
	return ensureCRLArtifact(ca, certDir, list)
}

// revokedEntry 是吊销列表中的一条记录（crl.json）。
// Serial 是证书序列号（小写十六进制，无 0x 前缀），Time 是吊销时间（RFC3339）。
type revokedEntry struct {
	Serial string `json:"serial"`
	Time   string `json:"time"`
}

// revokedList 是吊销序列号持久化记录（crl.json）的磁盘格式：
// crl.json 是来源记录（谁被吊销），crl.pem 是由它派生的签名产物（怎么验证）。
// Number 是最近一次生成 CRL 的 cRLNumber（0=尚未生成，每次吊销 +1，
// RFC 5280 建议单调递增）；Updated 是记录最后更新时间。
type revokedList struct {
	Entries []revokedEntry `json:"entries"`
	Number  int64          `json:"number"`
	Updated string         `json:"updated"`
}

// contains 判断序列号是否已在吊销列表。
func (l *revokedList) contains(serialHex string) bool {
	for _, e := range l.Entries {
		if e.Serial == serialHex {
			return true
		}
	}
	return false
}

// findLeafByCN 按 CN 查找节点证书：优先客户端证书（edgecore.crt），其次
// 服务端证书（cloudcore.crt）；返回解析后的证书与证书文件路径。
// 找不到匹配 CN（或对应文件不可解析）时返回明确错误。
func findLeafByCN(certDir, cn string) (*x509.Certificate, string, error) {
	_, _, serverCert, _, clientCert, _ := CertPaths(certDir)
	for _, path := range []string{clientCert, serverCert} {
		c, err := parseCertFile(path)
		if err == nil && c.Subject.CommonName == cn {
			return c, path, nil
		}
	}
	return nil, "", fmt.Errorf(
		"节点 %q 不存在（证书目录 %s 中没有 CN 匹配的证书，或证书文件不可解析；"+
			"可用 keadm cert rotate --help 查看证书约定）", cn, certDir)
}

// parseCertFile 读取并解析 PEM 证书文件；文件不存在/不可解析时返回错误。
func parseCertFile(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取证书 %s 失败: %w", path, err)
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("证书 %s 不是合法的 PEM 证书（文件损坏？）", path)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析证书 %s 失败: %w", path, err)
	}
	return c, nil
}

// loadRevokedList 加载吊销序列号记录：
//   - crl.json 存在 → 解析（损坏 → 报错，错误含路径；不静默清空，
//     否则已吊销证书会被重新放行）；
//   - crl.json 缺失但 crl.pem 存在 → 从 CRL 产物恢复序列号（不丢吊销状态）；
//   - 两者都缺失 → 空列表（首次吊销）。
func loadRevokedList(certDir string) (*revokedList, error) {
	crlPEMPath, crlJSONPath := CRLPaths(certDir)
	if b, err := os.ReadFile(crlJSONPath); err == nil {
		var list revokedList
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, fmt.Errorf("解析吊销列表记录 %s 失败（文件损坏？）: %w", crlJSONPath, err)
		}
		if err := validateRevokedList(&list); err != nil {
			return nil, fmt.Errorf("吊销列表记录 %s 内容非法: %w", crlJSONPath, err)
		}
		return &list, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取吊销列表记录 %s 失败: %w", crlJSONPath, err)
	}

	// crl.json 缺失：从 crl.pem 恢复（产物 → 记录），保证不丢已吊销序列号。
	if b, err := os.ReadFile(crlPEMPath); err == nil {
		rl, err := parseCRLPEM(b)
		if err != nil {
			return nil, fmt.Errorf("解析 CRL %s 失败（吊销记录 crl.json 缺失且 CRL 损坏，无法恢复已吊销序列号）: %w",
				crlPEMPath, err)
		}
		return revokedListFromCRL(rl), nil
	}
	return &revokedList{}, nil
}

// validateRevokedList 校验并归一化吊销记录内容（加载 crl.json 时调用）：
//   - 序列号必须是正整数十六进制（大小写均可），统一归一化为小写规范形式
//     （去前导零，如 "0ABC"/"abc" → "abc"）；
//   - 归一化后重复的序列号只保留首条（防手工编辑大写/带前导零的序列号
//     产生重复条目与 CRL 重复项）。
func validateRevokedList(l *revokedList) error {
	seen := make(map[string]bool, len(l.Entries))
	out := l.Entries[:0]
	for _, e := range l.Entries {
		if e.Serial == "" {
			return errors.New("存在空序列号记录")
		}
		n, ok := new(big.Int).SetString(e.Serial, 16)
		if !ok || n.Sign() <= 0 {
			return fmt.Errorf("序列号 %q 不是合法的正整数十六进制", e.Serial)
		}
		canonical := n.Text(16)
		if seen[canonical] {
			continue // 重复条目：保留首条（时间取首次记录）
		}
		seen[canonical] = true
		e.Serial = canonical
		out = append(out, e)
	}
	l.Entries = out
	return nil
}

// revokedListFromCRL 从解析后的 CRL 产物恢复吊销记录（crl.json 缺失时的恢复路径）。
// Number 取 CRL 的 cRLNumber（最后一次生成的编号；下次吊销时 +1）。
func revokedListFromCRL(rl *x509.RevocationList) *revokedList {
	list := &revokedList{}
	if rl.Number != nil {
		list.Number = rl.Number.Int64()
	}
	for _, e := range rl.RevokedCertificateEntries {
		if e.SerialNumber == nil {
			continue
		}
		list.Entries = append(list.Entries, revokedEntry{
			Serial: e.SerialNumber.Text(16),
			Time:   e.RevocationTime.UTC().Format(time.RFC3339),
		})
	}
	return list
}

// writeRevokedList 事务化落盘 crl.json（临时文件 + Rename，与轮换同一方案）。
func writeRevokedList(certDir string, list *revokedList) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化吊销列表记录失败: %w", err)
	}
	_, crlJSONPath := CRLPaths(certDir)
	if err := writeFileAtomic(crlJSONPath, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入吊销列表记录 %s 失败: %w", crlJSONPath, err)
	}
	return nil
}

// ensureCRLArtifact 确保 crl.pem 产物与吊销记录一致（锁内调用）：
// crl.pem 缺失/损坏/序列号集合不一致 → 重新生成；已一致 → 不动（幂等）。
//
// 重生成即一次新的 CRL 签发：CRL Number 统一在此 +1（RFC 5280 §5.2.3
// 每次签发新 CRL 应单调递增，含幂等自愈/对账重生成路径，不复用旧值），
// 并先写 crl.json（来源记录，保持 Number 与产物一致），再原子写 crl.pem。
// 新增序列号的常规吊销路径同样经此递增（列表追加本身不修改 Number）。
func ensureCRLArtifact(ca *CA, certDir string, list *revokedList) error {
	crlPEMPath, _ := CRLPaths(certDir)
	if b, err := os.ReadFile(crlPEMPath); err == nil {
		if rl, perr := parseCRLPEM(b); perr == nil && revokedSetEqual(rl, list) {
			return nil // crl.pem 已与记录一致
		}
	}
	list.Number++ // 新签发：CRL Number 单调递增（每次重生成 +1）
	list.Updated = time.Now().UTC().Format(time.RFC3339)
	// 先写 crl.json（来源记录，与新的 Number 保持一致）；crl.pem 随后写。
	if err := writeRevokedList(certDir, list); err != nil {
		return err
	}
	return writeCRLArtifact(ca, certDir, list)
}

// revokedSetEqual 比较 CRL 产物的吊销序列号集合与记录是否一致（顺序无关）。
func revokedSetEqual(rl *x509.RevocationList, list *revokedList) bool {
	if len(rl.RevokedCertificateEntries) != len(list.Entries) {
		return false
	}
	want := make(map[string]bool, len(list.Entries))
	for _, e := range list.Entries {
		want[e.Serial] = true
	}
	for _, e := range rl.RevokedCertificateEntries {
		if e.SerialNumber == nil || !want[e.SerialNumber.Text(16)] {
			return false
		}
	}
	return true
}

// writeCRLArtifact 由吊销记录生成 CRL 并事务化落盘 crl.pem（PEM 编码，
// 块类型 X509 CRL）。
func writeCRLArtifact(ca *CA, certDir string, list *revokedList) error {
	return writeCRLArtifactAt(ca, certDir, list, time.Now())
}

// writeCRLArtifactAt 是 writeCRLArtifact 的时间注入版本（测试可构造
// 已过期 CRL）：now 用作 ThisUpdate 基准（NextUpdate = now + crlValidity）。
func writeCRLArtifactAt(ca *CA, certDir string, list *revokedList, now time.Time) error {
	entries := make([]x509.RevocationListEntry, 0, len(list.Entries))
	for _, e := range list.Entries {
		serial, ok := new(big.Int).SetString(e.Serial, 16)
		if !ok {
			return fmt.Errorf("吊销列表含非法序列号 %q，无法生成 CRL", e.Serial)
		}
		t, err := time.Parse(time.RFC3339, e.Time)
		if err != nil {
			return fmt.Errorf("吊销列表序列号 %s 的吊销时间 %q 非法: %w", e.Serial, e.Time, err)
		}
		entries = append(entries, x509.RevocationListEntry{
			SerialNumber:   serial,
			RevocationTime: t,
			// ReasonCode 零值（Unspecified）：吊销原因未指定
		})
	}
	now = now.UTC()
	tmpl := &x509.RevocationList{
		Number:                    big.NewInt(list.Number),
		ThisUpdate:                now,
		NextUpdate:                now.Add(crlValidity),
		RevokedCertificateEntries: entries,
	}
	der, err := x509.CreateRevocationList(rand.Reader, tmpl, ca.Cert, ca.Key)
	if err != nil {
		return fmt.Errorf("生成 CRL 失败: %w", err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})
	crlPEMPath, _ := CRLPaths(certDir)
	if err := writeFileAtomic(crlPEMPath, pemData, 0o644); err != nil {
		return fmt.Errorf("写入 CRL %s 失败: %w", crlPEMPath, err)
	}
	return nil
}

// parseCRLPEM 解析 PEM 编码的 CRL（块类型 X509 CRL）。
func parseCRLPEM(b []byte) (*x509.RevocationList, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "X509 CRL" {
		return nil, errors.New("不是合法的 PEM CRL（PEM 块类型应为 X509 CRL）")
	}
	rl, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("CRL 解析失败: %w", err)
	}
	return rl, nil
}

// writeFileAtomic 事务化写文件：同目录临时文件（CreateTemp 初始 0600）→
// 写入 + 设置权限 → Rename 原子替换。任一步失败只清理临时文件，
// 原文件保持原状（与轮换落盘同一方案）。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("设置临时文件权限失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("替换文件失败: %w", err)
	}
	return nil
}

// LoadTLSConfig 加载 CA + 叶子证书并组装 tls.Config：
//   - isServer=true（CloudHub 侧）：携带 cloudcore.crt/key，ClientCAs=CA 池，
//     ClientAuth=RequireAndVerifyClientCert（强制要求并验证客户端证书，mTLS 核心）；
//   - isServer=false（EdgeHub 侧）：携带 edgecore.crt/key，RootCAs=CA 池，
//     以 wss:// 拨号时自动校验收到的服务端证书。
//
// 两侧均强制 TLS1.2+（拒绝 TLS1.0/1.1 等已废弃协议）。
// 两侧均附加对端证书吊销校验（WBS 7.1 CRL 消费端）：CRL 存在时按序列号
// 校验对端叶子证书，被吊销则拒绝握手；CRL 缺失放行（无吊销环境兼容）；
// CRL 损坏/签名无效 fail-closed 拒绝（防伪造绕过）。
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
		cfg := &tls.Config{
			Certificates: []tls.Certificate{*leaf},
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		}
		withCRLPeerCheck(cfg, certDir)
		return cfg, nil
	}

	leaf, err := EnsureClientCert(certDir, "edgeflow-edgecore")
	if err != nil {
		return nil, fmt.Errorf("加载客户端证书失败: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}
	withCRLPeerCheck(cfg, certDir)
	return cfg, nil
}

// withCRLPeerCheck 附加对端证书吊销校验（WBS 7.1 CRL 消费端接线）：
//
//	在标准证书链验证之外，按本 CA 签发的 CRL 检查对端叶子证书序列号：
//	  - 无 CRL 文件 → 放行（无吊销环境向后兼容）；
//	  - CRL 存在且对端证书被吊销 → 拒绝握手；
//	  - CRL 损坏/签名无效/CA 不可加载 → 拒绝（fail-closed，防伪造绕过）。
func withCRLPeerCheck(cfg *tls.Config, certDir string) {
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return nil // 无对端证书（非 mTLS 请求方不会触发回调）
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("解析对端证书失败: %w", err)
		}
		revoked, err := VerifyCertAgainstCRL(certDir, leaf)
		if err != nil {
			return fmt.Errorf("对端证书吊销状态校验失败（fail-closed）: %w", err)
		}
		if revoked {
			return fmt.Errorf("对端证书已被吊销（serial=%x），拒绝握手", leaf.SerialNumber)
		}
		return nil
	}
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
	// M4B P2-1：校验 CA 证书与私钥确实配对（防止目录里混入错配文件后
	// 静默启动，直到签发时才 fail）。tls.X509KeyPair 会校验公钥匹配。
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil, fmt.Errorf("CA 证书与私钥不匹配（%s/%s）: %w", caCertPath, caKeyPath, err)
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
