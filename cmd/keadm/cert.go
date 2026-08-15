// keadm cert —— 证书管理（WBS 7.1 证书轮换自动化，ROADMAP 7.1）。
//
// 子命令：
//
//	rotate  重新签发节点证书（轮换）：先备份旧证书（backups/<时间戳>/，
//	        时间戳后缀防覆盖）→ 事务化重签（pkg/certs.Rotate*）→
//	        输出新证书路径与备份路径。失败不破坏旧证书（备份先行 +
//	        事务化写入，任一步失败旧文件保持原状，备份可人工恢复）。
//
// 与 hack/gen-certs.sh 的关系：该脚本是 shell 版证书初始化工具（幂等跳过），
// 无强制重签能力且不可单测；rotate 复用其「等效 Go 实现」pkg/certs
// （同一证书布局/CN 约定/算法），新增强制重签语义（RotateClientCert/
// RotateServerCertWithSANs），选可测方案（纯 Go 单测覆盖备份/重签/错误路径）。
//
// 轮换目标识别：以证书 CN 为准——edgecore.crt（边缘节点客户端证书）的
// CN 与 --node 一致则轮换客户端证书；--node=cloudcore 且 cloudcore.crt
// 存在则轮换云端服务端证书（自动继承旧证书的 SAN，避免轮换后服务端
// 证书不再覆盖边缘节点访问地址）。
package main

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"edgeflow/pkg/certs"
)

// certUsageText 是 keadm cert 的用法说明。
const certUsageText = `keadm cert —— 证书管理（WBS 7.1 证书轮换自动化）。

用法:
  keadm cert <subcommand> [flags]

子命令:
  rotate  重新签发节点证书（轮换）：备份旧证书 + 事务化重签 + 输出路径

全局帮助:
  keadm cert -h | --help   打印本帮助
  keadm cert rotate -h     查看 rotate 参数

示例:
  keadm cert rotate --node=edgeflow-edgecore --cert-dir=./data/certs
  keadm cert rotate --node=cloudcore --cert-dir=./data/certs
`

// certRotateOptions 是 keadm cert rotate 的参数集合。
type certRotateOptions struct {
	// Node 是节点证书 CN（edgecore.crt 的 CN，如 edgeflow-edgecore；
	// 云端服务端证书固定为 cloudcore）。
	Node string
	// CertDir 是证书目录（含 ca.crt/ca.key 与节点证书）。
	CertDir string
}

// runCert 是 cert 子命令入口（分发到 rotate）。
func runCert(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, certUsageText)
		_, _ = fmt.Fprintln(stderr, "错误: 缺少 cert 子命令（rotate）")
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "rotate":
		return runCertRotate(rest, stdout, stderr)
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, certUsageText)
		return exitOK
	default:
		_, _ = fmt.Fprintf(stderr, "错误: 未知 cert 子命令 %q\n\n", cmd)
		_, _ = fmt.Fprint(stderr, certUsageText)
		return exitUsage
	}
}

// certBackupManifest 是证书备份目录的清单（backups/<id>/manifest.json）。
type certBackupManifest struct {
	Node   string            `json:"node"`   // 证书 CN
	TS     string            `json:"ts"`     // 备份时间（RFC3339）
	Files  []string          `json:"files"`  // 备份文件清单
	SHA256 map[string]string `json:"sha256"` // 各文件 sha256
}

// backupCertFiles 把待轮换的证书/私钥备份到 <cert-dir>/backups/<时间戳>/，
// 写入 manifest.json（含 CN/时间/文件清单/sha256）。id 取秒级时间戳，
// 同一秒内多次轮换追加序号避免覆盖（与 createBackup 同一方案）。
// 私钥备份保留 0600 权限。
func backupCertFiles(certDir, node string, names []string) (string, error) {
	base := time.Now().Format("20060102-150405")
	id := base
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(certDir, backupsDir, id)); os.IsNotExist(err) {
			break
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
	dir := filepath.Join(certDir, backupsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建备份目录 %s 失败: %w", dir, err)
	}
	m := certBackupManifest{
		Node:   node,
		TS:     time.Now().Format(time.RFC3339),
		Files:  append([]string{}, names...),
		SHA256: map[string]string{},
	}
	for _, name := range names {
		src := filepath.Join(certDir, name)
		b, err := os.ReadFile(src)
		if err != nil {
			return "", fmt.Errorf("读取待备份文件 %s 失败: %w", src, err)
		}
		perm := os.FileMode(0o644)
		if strings.HasSuffix(name, ".key") {
			perm = 0o600 // 私钥备份保持最小权限
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, perm); err != nil {
			return "", fmt.Errorf("写入备份文件 %s 失败: %w", name, err)
		}
		sum := sha256.Sum256(b)
		m.SHA256[name] = hex.EncodeToString(sum[:])
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化备份清单失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644); err != nil {
		return "", fmt.Errorf("写入备份清单失败: %w", err)
	}
	return id, nil
}

// certTarget 描述一次轮换的目标（节点证书身份 + 服务端 SAN 继承）。
type certTarget struct {
	files    []string // 待轮换文件（相对 cert-dir）
	kind     string   // client | server
	ips      []net.IP // 服务端轮换：继承旧证书的 SAN
	dnsNames []string
}

// resolveCertTarget 定位 --node 对应的节点证书：
//   - 客户端证书优先：edgecore.crt 的 CN 与 --node 一致 → 轮换边缘节点证书；
//   - 服务端证书：--node=cloudcore 且 cloudcore.crt 存在 → 轮换云端证书
//     （读取旧证书 SAN，轮换时原样继承）；
//   - 都不匹配 → 节点不存在错误。
func resolveCertTarget(certDir, node string) (*certTarget, error) {
	_, _, serverCert, _, clientCert, _ := certs.CertPaths(certDir)

	if c := parseCertCN(clientCert); c != "" && c == node {
		return &certTarget{files: []string{certs.FileClientCert, certs.FileClientKey}, kind: "client"}, nil
	}
	if node == "cloudcore" {
		if c := parseCert(serverCert); c != nil && c.Subject.CommonName == "cloudcore" {
			// 继承旧服务端证书 SAN：轮换后仍覆盖边缘节点访问地址。
			return &certTarget{
				files:    []string{certs.FileServerCert, certs.FileServerKey},
				kind:     "server",
				ips:      c.IPAddresses,
				dnsNames: c.DNSNames,
			}, nil
		}
	}
	return nil, fmt.Errorf("节点 %q 不存在（证书目录 %s 中没有 CN 匹配的节点证书；可用 keadm cert rotate --help 查看用法）",
		node, certDir)
}

// parseCertCN 读取 PEM 证书文件的 CN；文件不存在/不可解析时返回空串。
func parseCertCN(path string) string {
	if c := parseCert(path); c != nil {
		return c.Subject.CommonName
	}
	return ""
}

// parseCert 读取并解析 PEM 证书文件；文件不存在/不可解析时返回 nil。
func parseCert(path string) *x509.Certificate {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return c
}

// runCertRotate 实现 keadm cert rotate：
//
//	① 参数校验（--node 必填、无空白）→ ② 证书目录校验 →
//	③ 定位节点证书（CN 匹配）→ ④ CA 存在性校验（轮换不自动创建 CA）→
//	⑤ 备份旧证书（时间戳后缀 + manifest）→ ⑥ 事务化重签 → ⑦ 输出路径。
//
// 任一环节失败：旧证书不受影响（备份先行 + 事务化写入），输出备份路径与
// 人工恢复命令。
func runCertRotate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("cert rotate", stderr)
	opts := certRotateOptions{CertDir: certs.DefaultCertDir}
	fs.StringVar(&opts.Node, "node", "", "节点证书 CN（必填；边缘节点如 edgeflow-edgecore，云端为 cloudcore）")
	fs.StringVar(&opts.CertDir, "cert-dir", opts.CertDir, "证书目录（含 ca.crt/ca.key 与节点证书，默认 data/certs）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "错误: cert rotate 不接受位置参数 %q\n", fs.Arg(0))
		return exitUsage
	}

	// ① 参数校验。
	if strings.TrimSpace(opts.Node) == "" {
		_, _ = fmt.Fprintln(stderr, "错误: 缺少必填参数 --node（证书 CN，示例: keadm cert rotate --node=edgeflow-edgecore）")
		return exitUsage
	}
	if strings.ContainsAny(opts.Node, " \t\n") {
		_, _ = fmt.Fprintln(stderr, "错误: --node 含空白字符，证书 CN 不允许空格")
		return exitUsage
	}

	// ② 证书目录校验。
	if info, err := os.Stat(opts.CertDir); err != nil || !info.IsDir() {
		_, _ = fmt.Fprintf(stderr, "错误: 证书目录 %s 不存在或不可访问（轮换前需先初始化证书，见 hack/gen-certs.sh 或 pkg/certs）\n", opts.CertDir)
		return exitRuntime
	}

	// ③ 定位节点证书（CN 匹配；不存在/不匹配 → 报错）。
	target, err := resolveCertTarget(opts.CertDir, opts.Node)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "错误: "+err.Error())
		return exitRuntime
	}

	// ④ CA 前置校验：轮换需要现有 CA（不自动创建，防止误轮换 CA）。
	caCert, caKey, serverCert, serverKey, clientCert, clientKey := certs.CertPaths(opts.CertDir)
	if _, err := os.Stat(caCert); err != nil || !isRegularFile(caCert) {
		_, _ = fmt.Fprintf(stderr, "错误: CA 证书 %s 不存在，无法重新签发（轮换不自动创建 CA；请先初始化证书）\n", caCert)
		return exitRuntime
	}
	if _, err := os.Stat(caKey); err != nil || !isRegularFile(caKey) {
		_, _ = fmt.Fprintf(stderr, "错误: CA 私钥 %s 不存在，无法重新签发（轮换不自动创建 CA；请先初始化证书）\n", caKey)
		return exitRuntime
	}

	// ⑤ 备份旧证书（备份先行：轮换失败不破坏旧证书）。
	backupID, err := backupCertFiles(opts.CertDir, opts.Node, target.files)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 备份旧证书失败: %v（轮换未开始，证书未变化）\n", err)
		return exitRuntime
	}
	backupDir := filepath.Join(opts.CertDir, backupsDir, backupID)

	// ⑥ 事务化重签（pkg/certs.Rotate*：新密钥 + 新序列号 + 原子替换）。
	var rotateErr error
	var newCertPath, newKeyPath string
	switch target.kind {
	case "client":
		_, rotateErr = certs.RotateClientCert(opts.CertDir, opts.Node)
		newCertPath, newKeyPath = clientCert, clientKey
	case "server":
		_, rotateErr = certs.RotateServerCertWithSANs(opts.CertDir, "cloudcore", target.ips, target.dnsNames)
		newCertPath, newKeyPath = serverCert, serverKey
	}
	if rotateErr != nil {
		_, _ = fmt.Fprintf(stderr, "错误: 轮换失败: %v\n", rotateErr)
		_, _ = fmt.Fprintf(stderr, "  旧证书未受影响，已备份至: %s\n", backupDir)
		_, _ = fmt.Fprintln(stderr, "  如需人工恢复（用备份覆盖回原路径）:")
		for _, name := range target.files {
			_, _ = fmt.Fprintf(stderr, "    cp %s %s\n", filepath.Join(backupDir, name), filepath.Join(opts.CertDir, name))
		}
		return exitRuntime
	}

	// ⑦ 输出新证书路径与备份路径。
	_, _ = fmt.Fprintf(stdout, "keadm cert rotate 完成: 节点 %s 证书已重新签发\n", opts.Node)
	_, _ = fmt.Fprintf(stdout, "  新证书: %s\n", newCertPath)
	_, _ = fmt.Fprintf(stdout, "  新私钥: %s\n", newKeyPath)
	_, _ = fmt.Fprintf(stdout, "  备份:   %s（轮换前旧证书与私钥，含 manifest.json）\n", backupDir)
	_, _ = fmt.Fprintln(stdout, "  提示: 将新证书分发到节点后重启 edgecore/cloudcore 生效；如需回退，用备份文件覆盖原路径即可。")
	return exitOK
}

// isRegularFile 判断路径是普通文件（非目录）。
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
