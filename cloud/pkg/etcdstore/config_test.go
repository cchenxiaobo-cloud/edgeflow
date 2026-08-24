package etcdstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reflect"
)

// 环境变量清理：确保本文件用例不受其他测试遗留 env 影响。
func clearExternalEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvEndpoints, EnvTLSCA, EnvTLSCert, EnvTLSKey, EnvAllowInsecure} {
		os.Unsetenv(k)
	}
}

// --- ENDPOINTS 解析 ---

func TestConfigEndpoints(t *testing.T) {
	clearExternalEnv(t)

	// 正常：多 endpoint 全 http（含空格裁剪原样保留）。开逃生门绕过 M9
	// 护栏验证"解析能力与保留原样"；护栏单独在下方用例覆盖。
	t.Setenv(EnvAllowInsecure, "1")
	t.Setenv(EnvEndpoints, "http://127.0.0.1:2379, http://etcd-1.example:2379 ,http://etcd-2.example:2379")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("合法 ENDPOINTS 应解析成功: %v", err)
	}
	want := []string{"http://127.0.0.1:2379", "http://etcd-1.example:2379", "http://etcd-2.example:2379"}
	if len(cfg.Endpoints) != 3 {
		t.Fatalf("Endpoints=%v，应为 3 条", cfg.Endpoints)
	}
	for i := range want {
		if cfg.Endpoints[i] != want[i] {
			t.Errorf("Endpoints[%d]=%q，应为 %q", i, cfg.Endpoints[i], want[i])
		}
	}
	if cfg.TLSEnabled() {
		t.Errorf("未设置 CA 时 TLSEnabled 应为 false: %+v", cfg)
	}

	// 设计 §2.2 scheme⇔TLS 一致性（复核 🔴-A）：未启用 TLS 时混合
	// http+https → 配置期拒绝（https 端点会静默失效并被 failover 掩盖）。
	clearExternalEnv(t)
	t.Setenv(EnvAllowInsecure, "1")
	t.Setenv(EnvEndpoints, "http://127.0.0.1:2379,https://etcd-1.example:2379")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "全部端点必须一致使用 http") {
		t.Errorf("无 TLS 时混合 scheme 应拒绝且点名 http，got err=%v", err)
	}

	// 明文护栏（M9/M10）与全回环放行（注意：scheme⇔TLS 一致性校验先于护栏执行，
	// 护栏用例统一用全 http 端点聚焦 M9 语义）
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "http://etcd-1.example:2379,http://etcd-2.example:2379")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "明文护栏") {
		t.Errorf("非回环+无 TLS+无逃生门应被 M9 拒绝且点名护栏，got err=%v", err)
	}
	clearExternalEnv(t)
	t.Setenv(EnvAllowInsecure, "1")
	t.Setenv(EnvEndpoints, "http://etcd-1.example:2379")
	if _, err := ConfigFromEnv(); err != nil {
		t.Errorf("M10 逃生门=1 应放行，got err=%v", err)
	}
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "http://127.0.0.1:2379,http://localhost:2379,http://[::1]:2379")
	if cfg, err := ConfigFromEnv(); err != nil || len(cfg.Endpoints) != 3 {
		t.Errorf("全回环端点组合不应触发护栏，cfg=%+v err=%v", cfg, err)
	}

	// 单条
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "http://127.0.0.1:12379")
	cfg, err = ConfigFromEnv()
	if err != nil || len(cfg.Endpoints) != 1 || cfg.Endpoints[0] != "http://127.0.0.1:12379" {
		t.Fatalf("单 endpoint 解析失败: cfg=%+v err=%v", cfg, err)
	}

	// 非法值 fail-fast：非法 scheme / 空条目 / 无 host / 不可解析
	invalid := []string{
		"ftp://127.0.0.1:2379",       // 非法 scheme
		"http://a:2379,,https://b:2", // 空条目
		"http://",                    // 无 host
		"http://a:2379,  ,https://b", // 空格空条目
		"::::",                       // 不可解析
		"http://a:2379,tcp://b:2379", // 混合中有一条非法
	}
	for _, v := range invalid {
		clearExternalEnv(t)
		t.Setenv(EnvEndpoints, v)
		if _, err := ConfigFromEnv(); err == nil {
			t.Errorf("ENDPOINTS=%q 应报错，实际未报错", v)
		}
	}
}

// --- TLS 组合 ---

// writeTestCertPEM 生成自签证书+私钥 PEM 文件（测试用；CA 与叶子均可自签，
// tls.LoadX509KeyPair 不校验链，BuildTLS 只要求 PEM 合法）。
func writeTestCertPEM(t *testing.T, dir, name, cn string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         cn == "edgeflow-test-ca",
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("写证书失败: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥失败: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("写私钥失败: %v", err)
	}
	return certPath, keyPath
}

func TestConfigTLS(t *testing.T) {
	dir := t.TempDir()
	caCrt, caKey := writeTestCertPEM(t, dir, "ca", "edgeflow-test-ca")
	leafCrt, leafKey := writeTestCertPEM(t, dir, "leaf", "edgeflow-test-leaf")
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379") // TLS 段门控于外部模式（§1.4）
	// TLS 段在外部段门控内（设计 §1.4）：本用例统一开启外部模式
	// （https 端点满足 scheme⇔TLS 一致性）。
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379")

	// CA 单独 → TLS 启用，无客户端证书
	t.Setenv(EnvTLSCA, caCrt)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("CA 单独应解析成功: %v", err)
	}
	if !cfg.TLSEnabled() || cfg.CertFile != "" || cfg.KeyFile != "" {
		t.Errorf("CA 单独解析错误: cfg=%+v", cfg)
	}
	if cfg.CAFile != caCrt {
		t.Errorf("CAFile=%q", cfg.CAFile)
	}

	// CA + CERT + KEY → mTLS
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379") // TLS 段门控于外部模式（§1.4）
	t.Setenv(EnvTLSCA, caCrt)
	t.Setenv(EnvTLSCert, leafCrt)
	t.Setenv(EnvTLSKey, leafKey)
	cfg, err = ConfigFromEnv()
	if err != nil {
		t.Fatalf("mTLS 组合应解析成功: %v", err)
	}
	if cfg.CertFile != leafCrt || cfg.KeyFile != leafKey {
		t.Errorf("mTLS 文件解析错误: cfg=%+v", cfg)
	}

	// fail-fast 组合：CERT 缺 KEY / KEY 缺 CERT / CERT+KEY 无 CA / CA 文件不存在
	cases := []struct {
		name string
		set  func()
	}{
		{"cert-only", func() { t.Setenv(EnvTLSCA, caCrt); t.Setenv(EnvTLSCert, leafCrt) }},
		{"key-only", func() { t.Setenv(EnvTLSCA, caCrt); t.Setenv(EnvTLSKey, leafKey) }},
		{"cert-key-no-ca", func() { t.Setenv(EnvTLSCert, leafCrt); t.Setenv(EnvTLSKey, leafKey) }},
		{"ca-missing-file", func() { t.Setenv(EnvTLSCA, filepath.Join(dir, "nope.pem")) }},
		{"cert-missing-file", func() {
			t.Setenv(EnvTLSCA, caCrt)
			t.Setenv(EnvTLSCert, filepath.Join(dir, "nope.crt"))
			t.Setenv(EnvTLSKey, leafKey)
		}},
	}
	for _, c := range cases {
		clearExternalEnv(t)
		t.Setenv(EnvEndpoints, "https://127.0.0.1:2379") // TLS 段门控于外部模式（§1.4）
		c.set()
		if _, err := ConfigFromEnv(); err == nil {
			t.Errorf("%s: 应报错，实际未报错", c.name)
		} else {
			t.Logf("%s → %v", c.name, err)
		}
	}

	// BuildTLS 验证
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379") // TLS 段门控于外部模式（§1.4）
	t.Setenv(EnvTLSCA, caCrt)
	cfg, _ = ConfigFromEnv()
	tlsCfg, err := cfg.BuildTLS()
	if err != nil || tlsCfg == nil || tlsCfg.RootCAs == nil || len(tlsCfg.Certificates) != 0 {
		t.Fatalf("CA-only BuildTLS 错误: cfg=%+v err=%v", tlsCfg, err)
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion=%d，应 ≥TLS1.2", tlsCfg.MinVersion)
	}

	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379") // TLS 段门控于外部模式（§1.4）
	t.Setenv(EnvTLSCA, caCrt)
	t.Setenv(EnvTLSCert, leafCrt)
	t.Setenv(EnvTLSKey, leafKey)
	cfg, _ = ConfigFromEnv()
	tlsCfg, err = cfg.BuildTLS()
	if err != nil || tlsCfg == nil || len(tlsCfg.Certificates) != 1 {
		t.Fatalf("mTLS BuildTLS 错误: err=%v certs=%d", err, len(tlsCfg.Certificates))
	}

	// 未启用 TLS → nil
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379") // TLS 段门控于外部模式（§1.4）
	cfg, _ = ConfigFromEnv()
	if tlsCfg, err := cfg.BuildTLS(); err != nil || tlsCfg != nil {
		t.Fatalf("未启用 TLS 时 BuildTLS 应为 (nil, nil): %v %v", tlsCfg, err)
	}

	// 合法但内容非 PEM → 报错（fail-fast 前置）
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379") // TLS 段门控于外部模式（§1.4）
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a pem"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	t.Setenv(EnvTLSCA, junk)
	cfg, err = ConfigFromEnv()
	if err != nil {
		t.Fatalf("文件存在即可通过 ConfigFromEnv（内容校验在 BuildTLS）: %v", err)
	}
	if _, err := cfg.BuildTLS(); err == nil || !strings.Contains(err.Error(), "不含合法 PEM") {
		t.Errorf("BuildTLS 对非 PEM 内容应报错: %v", err)
	}
	_ = caKey // CA 私钥由 BuildTLS 的 LoadX509KeyPair 场景以外的用例使用（保留防漏）

	// 设计 §2.2 scheme⇔TLS 一致性（复核 🔴-A）：TLS 启用时全 https 通过、
	// 含 http 端点拒绝（http 端点会被 clientv3 failover 掩盖 TLS 语义）。
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379") // TLS 段门控于外部模式（§1.4）
	t.Setenv(EnvTLSCA, caCrt)
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379,https://etcd-1.example:2379")
	cfg, err = ConfigFromEnv()
	if err != nil || !cfg.TLSEnabled() || len(cfg.Endpoints) != 2 {
		t.Fatalf("TLS+全 https 应解析成功: cfg=%+v err=%v", cfg, err)
	}
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379") // TLS 段门控于外部模式（§1.4）
	t.Setenv(EnvTLSCA, caCrt)
	t.Setenv(EnvEndpoints, "https://127.0.0.1:2379,http://etcd-1.example:2379")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "全部端点必须一致使用 https") {
		t.Errorf("TLS 启用时混合 scheme 应拒绝且点名 https，got err=%v", err)
	}
}

// --- 向后兼容：无新环境变量时默认值不变 ---

func TestConfigBackwardCompatNoExternalEnv(t *testing.T) {
	clearExternalEnv(t)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("默认配置解析失败: %v", err)
	}
	if !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Errorf("无外部 env 时配置应等于 DefaultConfig:\n got=%+v\nwant=%+v", cfg, DefaultConfig())
	}
	if len(cfg.Endpoints) != 0 || cfg.TLSEnabled() {
		t.Errorf("无外部 env 时 Endpoints/TLS 应零值: %+v", cfg)
	}
}

// 设计 §1.1 六项端点校验（复核 ⚠️-1）：缺端口/带路径/带 query/带 fragment/
// 带 userinfo/空 host 带端口 全部配置期 fail-fast。开逃生门排除 M9 干扰，
// 聚焦 §1.1 形状校验本身。
func TestConfigEndpointShapeValidation(t *testing.T) {
	invalid := []struct {
		name, ep string
	}{
		{"缺端口", "http://127.0.0.1"},
		{"带路径", "http://127.0.0.1:2379/v3"},
		{"带查询", "http://127.0.0.1:2379?x=1"},
		{"带 fragment", "http://127.0.0.1:2379#frag"},
		{"URL 内凭证", "http://user:pass@127.0.0.1:2379"},
		{"空 host 带端口", "http://:2379"},
	}
	for _, c := range invalid {
		clearExternalEnv(t)
		t.Setenv(EnvAllowInsecure, "1")
		t.Setenv(EnvEndpoints, c.ep)
		if _, err := ConfigFromEnv(); err == nil {
			t.Errorf("%s（%q）应配置期拒绝，实际未报错", c.name, c.ep)
		}
	}
}

// M1 短路与配置段隔离（复核 ⚠️-2/⚠️-7）：Enabled=false 时外部段全部不解析
// （配错不阻断纯内存逃生）；embed 模式忽略外部段（TLS_CERT-only 不报错）；
// 外部模式忽略 embed 段（CLIENT_URL 非回环不报错，M3 两段互不串扰）。
func TestConfigSegmentIsolation(t *testing.T) {
	dir := t.TempDir()
	leafCrt, leafKey := writeTestCertPEM(t, dir, "leaf", "edgeflow-test-leaf")

	// M1：总开关关 → 坏 ENDPOINTS/非法 ALLOW_INSECURE 全部不报错（纯内存逃生）
	clearExternalEnv(t)
	t.Setenv(EnvEnabled, "false")
	t.Setenv(EnvEndpoints, "ftp://bad:1,::::")
	t.Setenv(EnvAllowInsecure, "abc")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("M1 短路：Enabled=false 不应解析外部段: %v", err)
	}
	if len(cfg.Endpoints) != 0 || cfg.AllowInsecure {
		t.Errorf("M1 短路后外部字段应零值: %+v", cfg)
	}

	// embed 模式（ENDPOINTS 空）忽略外部段：TLS_CERT-only 配错不阻断 embed 启动
	clearExternalEnv(t)
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvTLSCert, leafCrt)
	t.Setenv(EnvTLSKey, leafKey)
	if _, err := ConfigFromEnv(); err != nil {
		t.Errorf("embed 模式不应校验外部段（TLS_CERT-only 配错不阻断）: %v", err)
	}

	// 外部模式忽略 embed 段：CLIENT_URL 非回环不报错（两段互不串扰，M3）
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "http://127.0.0.1:2379")
	t.Setenv(EnvClientURL, "http://0.0.0.0:12379")
	if _, err := ConfigFromEnv(); err != nil {
		t.Errorf("外部模式不应校验 embed 段（CLIENT_URL 非回环应被忽略）: %v", err)
	}

	// ALLOW_INSECURE 非法值（外部模式）fail-fast（复核 ⚠️-7）
	clearExternalEnv(t)
	t.Setenv(EnvEndpoints, "http://127.0.0.1:2379")
	t.Setenv(EnvAllowInsecure, "abc")
	if _, err := ConfigFromEnv(); err == nil {
		t.Error("ALLOW_INSECURE=abc 应 fail-fast")
	}
}
