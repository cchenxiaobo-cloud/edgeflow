package main

// keadm cert rotate 测试（WBS 7.1 证书轮换自动化）：
// 备份先行、事务化重签、成功/错误路径、重复轮换不产生垃圾。

import (
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"edgeflow/pkg/certs"
)

// setupCertDir 创建含全套证书的临时证书目录（CA + 客户端 + 服务端，
// 服务端带自定义 SAN）。
func setupCertDir(t *testing.T, cn string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "certs")
	if _, err := certs.EnsureCA(dir); err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	if _, err := certs.EnsureClientCert(dir, cn); err != nil {
		t.Fatalf("EnsureClientCert 失败: %v", err)
	}
	if _, err := certs.EnsureServerCertWithSANs(dir, "cloudcore",
		[]net.IP{net.ParseIP("192.168.1.10")}, []string{"edge.example.com"}); err != nil {
		t.Fatalf("EnsureServerCertWithSANs 失败: %v", err)
	}
	return dir
}

// parseCertFile 解析证书文件并返回 x509.Certificate。

// loadClientCertCN 读取 edgecore.crt 的 CN（通过 pkg/certs 幂等加载）。
func loadClientCertCN(t *testing.T, dir string) string {
	t.Helper()
	leaf, err := certs.EnsureClientCert(dir, "edgeflow-test-node")
	if err != nil {
		t.Fatalf("加载客户端证书失败: %v", err)
	}
	c, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("解析客户端证书失败: %v", err)
	}
	return c.Subject.CommonName
}

// listCertBackupDirs 列出 <cert-dir>/backups/ 下的备份目录名。
func listCertBackupDirs(t *testing.T, certDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(certDir, backupsDir))
	if err != nil {
		t.Fatalf("读取证书备份目录失败: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// assertNoCertTemp 断言证书目录无 .*.tmp-* 残留。
func assertNoCertTemp(t *testing.T, certDir string) {
	t.Helper()
	entries, err := os.ReadDir(certDir)
	if err != nil {
		t.Fatalf("读取证书目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("临时文件未清理: %s", e.Name())
		}
	}
}

// TestCertRotateClientSuccess 验证客户端证书轮换成功路径：新证书 CN 不变、
// 备份含旧证书/私钥与 manifest、输出新路径与备份路径、无临时残留。
func TestCertRotateClientSuccess(t *testing.T) {
	dir := setupCertDir(t, "edgeflow-test-node")
	_, _, _, _, clientCert, clientKey := certs.CertPaths(dir)
	oldKey, err := os.ReadFile(clientKey)
	if err != nil {
		t.Fatalf("读取旧私钥失败: %v", err)
	}

	code, stdout, stderr := runCapture([]string{
		"cert", "rotate", "--node=edgeflow-test-node", "--cert-dir=" + dir,
	}, "")
	if code != 0 {
		t.Fatalf("cert rotate 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "已重新签发") {
		t.Errorf("stdout 应含完成提示: %q", stdout)
	}
	if !strings.Contains(stdout, clientCert) || !strings.Contains(stdout, clientKey) {
		t.Errorf("stdout 应输出新证书/私钥路径: %q", stdout)
	}
	if !strings.Contains(stdout, filepath.Join(dir, backupsDir)) {
		t.Errorf("stdout 应输出备份路径: %q", stdout)
	}

	// CN 不变（身份保持）。
	if cn := loadClientCertCN(t, dir); cn != "edgeflow-test-node" {
		t.Errorf("轮换后 CN = %q，期望 edgeflow-test-node", cn)
	}
	// 私钥已更新（真正的轮换）。
	newKey, err := os.ReadFile(clientKey)
	if err != nil {
		t.Fatalf("读取新私钥失败: %v", err)
	}
	if string(oldKey) == string(newKey) {
		t.Error("轮换后私钥未变化")
	}

	// 备份：1 个目录，含旧证书/私钥 + manifest。
	dirs := listCertBackupDirs(t, dir)
	if len(dirs) != 1 {
		t.Fatalf("应有 1 个备份目录，实际 %v", dirs)
	}
	backupDir := filepath.Join(dir, backupsDir, dirs[0])
	for _, name := range []string{certs.FileClientCert, certs.FileClientKey} {
		if _, err := os.Stat(filepath.Join(backupDir, name)); err != nil {
			t.Errorf("备份缺少 %s: %v", name, err)
		}
	}
	backupKey, err := os.ReadFile(filepath.Join(backupDir, certs.FileClientKey))
	if err != nil {
		t.Fatalf("读取备份私钥失败: %v", err)
	}
	if string(backupKey) != string(oldKey) {
		t.Error("备份私钥应与轮换前一致")
	}
	// 备份 manifest 记录 CN 与文件清单。
	var m certBackupManifest
	mb, err := os.ReadFile(filepath.Join(backupDir, "manifest.json"))
	if err != nil {
		t.Fatalf("读取备份清单失败: %v", err)
	}
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatalf("备份清单解析失败: %v", err)
	}
	if m.Node != "edgeflow-test-node" || len(m.Files) != 2 || m.SHA256[certs.FileClientKey] == "" {
		t.Errorf("备份 manifest 字段不完整: %+v", m)
	}
	assertNoCertTemp(t, dir)
}

// TestCertRotateServerPreservesSAN 验证服务端证书轮换保留 SAN
// （轮换后仍覆盖边缘节点访问地址）。
func TestCertRotateServerPreservesSAN(t *testing.T) {
	dir := setupCertDir(t, "edgeflow-test-node")

	code, stdout, stderr := runCapture([]string{
		"cert", "rotate", "--node=cloudcore", "--cert-dir=" + dir,
	}, "")
	if code != 0 {
		t.Fatalf("cert rotate(cloudcore) 退出码 = %d，期望 0；stderr=%s", code, stderr)
	}
	// 轮换后服务端证书 SAN 保留。
	leaf, err := certs.EnsureServerCert(dir, "cloudcore")
	if err != nil {
		t.Fatalf("加载轮换后服务端证书失败: %v", err)
	}
	c, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("解析服务端证书失败: %v", err)
	}
	if len(c.IPAddresses) != 1 || c.IPAddresses[0].String() != "192.168.1.10" {
		t.Errorf("轮换后 SAN IP 丢失: %v", c.IPAddresses)
	}
	if len(c.DNSNames) != 1 || c.DNSNames[0] != "edge.example.com" {
		t.Errorf("轮换后 SAN DNS 丢失: %v", c.DNSNames)
	}
	if !strings.Contains(stdout, "cloudcore") {
		t.Errorf("stdout 应含 cloudcore 提示: %q", stdout)
	}
	// 备份目录记录的是 cloudcore 证书。
	dirs := listCertBackupDirs(t, dir)
	if len(dirs) != 1 {
		t.Fatalf("应有 1 个备份目录，实际 %v", dirs)
	}
	if _, err := os.Stat(filepath.Join(dir, backupsDir, dirs[0], certs.FileServerCert)); err != nil {
		t.Errorf("备份应含 cloudcore.crt: %v", err)
	}
}

// TestCertRotateUsageErrors 验证用法错误：缺 --node、CN 含空白 → exit 2。
func TestCertRotateUsageErrors(t *testing.T) {
	dir := setupCertDir(t, "edgeflow-test-node")
	cases := [][]string{
		{"cert", "rotate", "--cert-dir=" + dir},                          // 缺 --node
		{"cert", "rotate", "--node=edge a", "--cert-dir=" + dir},         // CN 含空格
		{"cert", "rotate", "--node=x", "--cert-dir=" + dir, "--bogus=1"}, // 未知 flag
	}
	for i, args := range cases {
		code, _, stderr := runCapture(args, "")
		if code != 2 {
			t.Errorf("用例 %d: 退出码 = %d，期望 2；stderr=%s", i, code, stderr)
		}
		if stderr == "" {
			t.Errorf("用例 %d: stderr 应有错误提示", i)
		}
	}
}

// TestCertRotateRuntimeErrors 验证运行时错误：证书目录不存在、节点不存在
// （目录空 / CN 不匹配）、CA 缺失 → exit 1 且证书不受影响。
func TestCertRotateRuntimeErrors(t *testing.T) {
	// 证书目录不存在。
	missing := filepath.Join(t.TempDir(), "no-such-certs")
	code, _, stderr := runCapture([]string{"cert", "rotate", "--node=edgeflow-x", "--cert-dir=" + missing}, "")
	if code != 1 || !strings.Contains(stderr, "证书目录") {
		t.Errorf("证书目录不存在应 exit=1 并提示: code=%d stderr=%q", code, stderr)
	}

	// 节点不存在：CN 不匹配。
	dir := setupCertDir(t, "edgeflow-test-node")
	code, _, stderr = runCapture([]string{"cert", "rotate", "--node=edgeflow-other", "--cert-dir=" + dir}, "")
	if code != 1 || !strings.Contains(stderr, "不存在") {
		t.Errorf("CN 不匹配应 exit=1 并提示节点不存在: code=%d stderr=%q", code, stderr)
	}

	// 节点不存在：目录为空（无任何证书）。
	empty := filepath.Join(t.TempDir(), "empty-certs")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir 失败: %v", err)
	}
	code, _, stderr = runCapture([]string{"cert", "rotate", "--node=edgeflow-x", "--cert-dir=" + empty}, "")
	if code != 1 || !strings.Contains(stderr, "不存在") {
		t.Errorf("空目录应 exit=1 并提示节点不存在: code=%d stderr=%q", code, stderr)
	}

	// CA 缺失（移除 ca.key）：轮换被拒，客户端证书不受影响。
	dir2 := setupCertDir(t, "edgeflow-test-node")
	if err := os.Remove(filepath.Join(dir2, certs.FileCAKey)); err != nil {
		t.Fatalf("移除 CA 私钥失败: %v", err)
	}
	before := readFile(t, filepath.Join(dir2, certs.FileClientCert))
	code, _, stderr = runCapture([]string{"cert", "rotate", "--node=edgeflow-test-node", "--cert-dir=" + dir2}, "")
	if code != 1 || !strings.Contains(stderr, "CA 私钥") {
		t.Errorf("CA 缺失应 exit=1 并提示 CA 私钥: code=%d stderr=%q", code, stderr)
	}
	if got := readFile(t, filepath.Join(dir2, certs.FileClientCert)); got != before {
		t.Error("CA 缺失拒绝轮换后客户端证书不应变化")
	}
	// CA 校验在备份之前：不应产生备份目录。
	if _, err := os.Stat(filepath.Join(dir2, backupsDir)); !os.IsNotExist(err) {
		t.Errorf("CA 校验失败不应产生备份（备份在 CA 校验之后）")
	}
}

// TestCertRotateFailureKeepsOldCert 验证轮换失败不破坏旧证书（备份先行）：
// CA 私钥损坏 → 重签失败 → 旧证书字节不变、备份已生成且含旧证书、
// stderr 输出人工恢复命令。
func TestCertRotateFailureKeepsOldCert(t *testing.T) {
	dir := setupCertDir(t, "edgeflow-test-node")
	_, _, _, _, clientCert, _ := certs.CertPaths(dir)
	before := readFile(t, clientCert)
	// 损坏 CA 私钥（存在但不可解析，模拟 CA 文件损坏）。
	if err := os.WriteFile(filepath.Join(dir, certs.FileCAKey), []byte("garbage not a key\n"), 0o600); err != nil {
		t.Fatalf("覆盖 CA 私钥失败: %v", err)
	}

	code, _, stderr := runCapture([]string{"cert", "rotate", "--node=edgeflow-test-node", "--cert-dir=" + dir}, "")
	if code != 1 {
		t.Fatalf("CA 损坏轮换应 exit=1，实际 %d；stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "轮换失败") {
		t.Errorf("stderr 应提示轮换失败: %q", stderr)
	}
	if !strings.Contains(stderr, filepath.Join(dir, backupsDir)) {
		t.Errorf("stderr 应输出备份路径: %q", stderr)
	}
	if !strings.Contains(stderr, "cp ") {
		t.Errorf("stderr 应输出人工恢复 cp 命令: %q", stderr)
	}

	// 旧证书字节不变（轮换失败未破坏旧证书）。
	if got := readFile(t, clientCert); got != before {
		t.Error("轮换失败后旧证书不应变化")
	}
	// 备份已生成且含轮换前的旧证书。
	dirs := listCertBackupDirs(t, dir)
	if len(dirs) != 1 {
		t.Fatalf("应有 1 个备份目录，实际 %v", dirs)
	}
	backupCert := readFile(t, filepath.Join(dir, backupsDir, dirs[0], certs.FileClientCert))
	if backupCert != before {
		t.Error("备份应含轮换前的旧证书")
	}
	assertNoCertTemp(t, dir)
}

// TestCertRotateRepeatNoGarbage 验证重复轮换幂等可用、不产生垃圾：
// 每次轮换新增一个备份目录（时间戳后缀防覆盖），无临时文件残留。
func TestCertRotateRepeatNoGarbage(t *testing.T) {
	dir := setupCertDir(t, "edgeflow-test-node")
	_, _, _, _, _, clientKey := certs.CertPaths(dir)
	keys := map[string]bool{}

	for i := 0; i < 2; i++ {
		code, _, stderr := runCapture([]string{
			"cert", "rotate", "--node=edgeflow-test-node", "--cert-dir=" + dir,
		}, "")
		if code != 0 {
			t.Fatalf("第 %d 次轮换退出码 = %d，期望 0；stderr=%s", i+1, code, stderr)
		}
		b, err := os.ReadFile(clientKey)
		if err != nil {
			t.Fatalf("读取私钥失败: %v", err)
		}
		keys[string(b)] = true
	}
	if len(keys) != 2 {
		t.Error("两次轮换应产生两把不同的私钥（每次都是真实轮换）")
	}
	dirs := listCertBackupDirs(t, dir)
	if len(dirs) != 2 {
		t.Errorf("两次轮换应有 2 个备份目录，实际 %v", dirs)
	}
	assertNoCertTemp(t, dir)
	// 两个备份目录名互不相同（时间戳后缀防覆盖）。
	if dirs[0] == dirs[1] {
		t.Errorf("备份目录名不应重复: %v", dirs)
	}
}
