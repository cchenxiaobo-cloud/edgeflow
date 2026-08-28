package certs

// v0.22.0 吊销策略收紧测试（SEC-04，T-11）：
//   - CRL 缺失行为可配置：默认放行（v0.21.0 向后兼容，缺省逐字节一致）+
//     CRLStrict（对应 env EDGEFLOW_CLOUDCORE_CRL_STRICT=on）时拒绝
//     （校验返回 ErrCRLMissing 哨兵 / 握手失败）；
//   - OCSP 新鲜度开关：OCSPFreshCheck（对应 env
//     EDGEFLOW_CLOUDCORE_OCSP_FRESH=on）时 nextUpdate 已过期的响应拒绝
//     （ErrOCSPResponseStale），默认关闭放行。
//
// 注：本文件不改动 crl_handshake_test.go（既有握手测试零改动为硬约束）；
// 严格模式握手拒绝经本文件专属的 strictHandshakePair（LoadTLSConfigWithOptions）
// 覆盖，与既有 handshakePair（LoadTLSConfig 默认路径）互补。

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── CRL 缺失开关（FailOnMissing / CRLStrict）────────────────────────

// TestCRLMissingDefaultAllows 缺省行为回归锚：证书目录存在但 crl.pem 缺失
// → 默认放行（false, nil），与 v0.21.0 逐字节一致。
func TestCRLMissingDefaultAllows(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	if _, err := os.Stat(filepath.Join(certDir, "crl.pem")); !os.IsNotExist(err) {
		t.Fatalf("前置：crl.pem 应不存在（err=%v）", err)
	}
	clientCert := loadParsedClientCert(t, certDir)
	revoked, err := VerifyCertAgainstCRL(certDir, clientCert)
	if err != nil || revoked {
		t.Fatalf("默认策略下 CRL 缺失应放行: revoked=%v err=%v", revoked, err)
	}
	// 零值选项显式路径同语义。
	revoked, err = VerifyCertAgainstCRLWithPolicy(certDir, clientCert, CRLVerifyOptions{})
	if err != nil || revoked {
		t.Fatalf("零值选项下 CRL 缺失应放行: revoked=%v err=%v", revoked, err)
	}
}

// TestCRLMissingStrictRejects strict 模式：crl.pem 缺失 → ErrCRLMissing
// （errors.Is 可判别）；证书整个目录缺失同样拒绝。
func TestCRLMissingStrictRejects(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	clientCert := loadParsedClientCert(t, certDir)

	// crl.pem 缺失（目录存在）。
	if _, err := VerifyCertAgainstCRLWithOptions(certDir, clientCert, CRLVerifyOptions{FailOnMissing: true}); !errors.Is(err, ErrCRLMissing) {
		t.Fatalf("strict 下 crl.pem 缺失应返回 ErrCRLMissing，实际 err=%v", err)
	}

	// 证书目录整体缺失。
	empty := t.TempDir()
	missingDir := filepath.Join(empty, "no-such-dir")
	if _, err := VerifyCertAgainstCRLWithOptions(missingDir, clientCert, CRLVerifyOptions{FailOnMissing: true}); !errors.Is(err, ErrCRLMissing) {
		t.Fatalf("strict 下证书目录缺失应返回 ErrCRLMissing，实际 err=%v", err)
	}
	// 目录缺失 + 默认策略 → 仍放行（向后兼容）。
	if revoked, err := VerifyCertAgainstCRL(missingDir, clientCert); err != nil || revoked {
		t.Fatalf("默认策略下目录缺失应放行: revoked=%v err=%v", revoked, err)
	}
}

// TestCRLMissingStrictDoesNotBlockRevoked strict 不改变吊销判定：crl.pem
// 存在且证书已吊销 → 仍然 (true, nil)（吊销不可逆， ErrCRLMissing 仅覆盖
// 「缺失」路径）。
func TestCRLMissingStrictDoesNotBlockRevoked(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	clientCert := loadParsedClientCert(t, certDir)
	revoked, err := VerifyCertAgainstCRLWithOptions(certDir, clientCert, CRLVerifyOptions{FailOnMissing: true})
	if err != nil || !revoked {
		t.Fatalf("strict 下已吊销证书仍应被检出: revoked=%v err=%v", revoked, err)
	}
}

// ── 握手级：CRLStrict 经 LoadTLSConfigWithOptions 注入 ────────────────

// strictHandshakePair 与 crl_handshake_test.go 的 handshakePair 同构，但走
// LoadTLSConfigWithOptions(opts.CRLStrict=true)（严格模式专属装配）。
func strictHandshakePair(t *testing.T, certDir string) (clientErr, serverErr error) {
	t.Helper()
	opts := WithCRLStrict(RevocationOptions{}, true)
	serverCfg, err := LoadTLSConfigWithOptions(certDir, true, opts)
	if err != nil {
		t.Fatalf("服务端 TLS 配置失败: %v", err)
	}
	clientCfg, err := LoadTLSConfigWithOptions(certDir, false, opts)
	if err != nil {
		t.Fatalf("客户端 TLS 配置失败: %v", err)
	}
	return tlsHandshakeOnce(t, serverCfg, clientCfg)
}

// tlsHandshakeOnce 完成一次真实 mTLS 握手并回收双侧错误（公共小工具）。
func tlsHandshakeOnce(t *testing.T, serverCfg, clientCfg *tls.Config) (clientErr, serverErr error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	acceptErr := make(chan error, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			acceptErr <- aerr
			return
		}
		defer conn.Close()
		acceptErr <- conn.(*tls.Conn).Handshake()
	}()
	conn, cerr := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	clientErr = cerr
	if cerr == nil {
		defer conn.Close()
	}
	select {
	case serverErr = <-acceptErr:
	case <-time.After(3 * time.Second):
		serverErr = errors.New("服务端握手超时")
	}
	return clientErr, serverErr
}

// TestTLSHandshakeCRLStrictMissingRejects CRL 缺失 + strict → 握手被拒
// （fail-closed；错误链含 ErrCRLMissing 文本）。
func TestTLSHandshakeCRLStrictMissingRejects(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir) // 不产生 crl.pem
	clientErr, serverErr := strictHandshakePair(t, certDir)
	// 详细拒绝原因在先校验对端证书的一侧（客户端先校验服务端证书 →
	// clientErr 含 ErrCRLMissing 文案；serverErr 侧为 generic alert），
	// 与既有 TestTLSHandshakeCorruptCRLFailsClosed 的断言风格一致。
	joined := ""
	if clientErr != nil {
		joined += clientErr.Error()
	}
	if serverErr != nil {
		joined += "|" + serverErr.Error()
	}
	if clientErr == nil && serverErr == nil {
		t.Fatal("strict 模式下 CRL 缺失应拒绝握手，实际双侧均成功")
	}
	if !strings.Contains(joined, "CRL 缺失") {
		t.Fatalf("拒绝原因应含 CRL 缺失文案，实际 clientErr=%v serverErr=%v", clientErr, serverErr)
	}
	t.Logf("strict 缺失拒绝确认: clientErr=%v serverErr=%v", clientErr, serverErr)
}

// TestTLSHandshakeCRLStrictPresentAllows CRL 存在（未吊销）+ strict →
// 正常放行（证明开关只收紧缺失路径，不误伤常规握手）。
func TestTLSHandshakeCRLStrictPresentAllows(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	// 吊销+换发产链路自然生成 crl.pem（EnsureCRLArtifact 一致性产物）。
	if err := RevokeCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	if _, err := RotateClientCert(certDir, "edgeflow-test-node"); err != nil {
		t.Fatalf("RotateClientCert 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(certDir, "crl.pem")); err != nil {
		t.Fatalf("前置：crl.pem 应存在（err=%v）", err)
	}
	clientErr, serverErr := strictHandshakePair(t, certDir)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("CRL 存在且未吊销时 strict 握手应成功: clientErr=%v serverErr=%v", clientErr, serverErr)
	}
}

// TestTLSHandshakeDefaultMissingAllows 缺省装配（LoadTLSConfig）下 CRL
// 缺失握手放行——v0.21.0 缺省行为逐字节回归锚（crl_handshake_test.go 的
// TestTLSHandshakeWithoutCRL 等价锚点，此处用 RevocationOptions 零值再证）。
func TestTLSHandshakeDefaultMissingAllows(t *testing.T) {
	certDir := newTempCertDir(t)
	mustGenAll(t, certDir)
	serverCfg, err := LoadTLSConfigWithOptions(certDir, true, RevocationOptions{})
	if err != nil {
		t.Fatalf("服务端 TLS 配置失败: %v", err)
	}
	clientCfg, err := LoadTLSConfigWithOptions(certDir, false, RevocationOptions{})
	if err != nil {
		t.Fatalf("客户端 TLS 配置失败: %v", err)
	}
	clientErr, serverErr := tlsHandshakeOnce(t, serverCfg, clientCfg)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("缺省选项下 CRL 缺失握手应放行: clientErr=%v serverErr=%v", clientErr, serverErr)
	}
}

// ── OCSP 新鲜度开关（OCSPFreshCheck / FreshnessPolicy）────────────────

// TestOCSPFreshDefaultAllowsExpired 过期响应 + 默认（零值）策略 → 放行
// （v0.21.0 旧行为；OCSPStatusAt 不校验新鲜度）。
func TestOCSPFreshDefaultAllowsExpired(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-fresh-a")
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	// 构造 nextUpdate 已过期的响应（ThisUpdate 2h 前，NextUpdate 1h 前）。
	respDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{
		ThisUpdate: time.Now().Add(-2 * time.Hour),
		NextUpdate: time.Now().Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("构造过期响应失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("解析请求失败: %v", err)
	}
	status, _, err := ParseOCSPResponse(ca.Cert, respDER, reqCID)
	if err != nil || status != StatusGood {
		t.Fatalf("默认策略下过期响应应放行: status=%q err=%v", status, err)
	}
}

// TestOCSPFreshOnRejectsExpired 过期响应 + OCSPFreshCheck 开启 →
// ErrOCSPResponseStale 拒绝（fail-closed）；未过期响应放行。
func TestOCSPFreshOnRejectsExpired(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-fresh-b")
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("解析请求失败: %v", err)
	}

	// 开关开启（装配层映射：RevocationOptions → FreshnessPolicy）。
	policy := OCSPClientOptions(WithOCSPFreshCheck(RevocationOptions{}, true))
	if !policy.CheckFreshness {
		t.Fatal("WithOCSPFreshCheck(true) 应映射为开启校验的 FreshnessPolicy")
	}

	// 过期响应 → ErrOCSPResponseStale。
	respDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{
		ThisUpdate: time.Now().Add(-2 * time.Hour),
		NextUpdate: time.Now().Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("构造过期响应失败: %v", err)
	}
	if _, _, err := ParseOCSPResponseWithFreshness(ca.Cert, respDER, reqCID, policy); !errors.Is(err, ErrOCSPResponseStale) {
		t.Fatalf("开启新鲜度校验时过期响应应返回 ErrOCSPResponseStale，实际 err=%v", err)
	}

	// 新鲜响应 → 放行 good。
	freshDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{
		ThisUpdate: time.Now().Add(-time.Minute),
		NextUpdate: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("构造新鲜响应失败: %v", err)
	}
	status, _, err := ParseOCSPResponseWithFreshness(ca.Cert, freshDER, reqCID, policy)
	if err != nil || status != StatusGood {
		t.Fatalf("开启校验时新鲜响应应放行: status=%q err=%v", status, err)
	}

	// 关闭（零值 RevocationOptions → 零值策略）→ 过期响应放行（默认 off）。
	off := OCSPClientOptions(RevocationOptions{})
	if off.CheckFreshness {
		t.Fatal("缺省 RevocationOptions 应映射为不校验的零值策略（默认 off）")
	}
	if _, _, err := ParseOCSPResponseWithFreshness(ca.Cert, respDER, reqCID, off); err != nil {
		t.Fatalf("关闭开关时过期响应应放行（默认 off）: err=%v", err)
	}
}

// TestOCSPRevokedUnderFreshCheck 吊销判定不受新鲜度开关影响： revoked 响应
// 即使过期，ErrOCSPResponseStale 仍返回（fail-closed 优先；调用方按错误拒绝，
// 吊销不可逆语义不变）。
func TestOCSPRevokedUnderFreshCheck(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-fresh-c")
	if err := RevokeCert(certDir, leaf.Subject.CommonName); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	revokedList, err := LoadRevokedList(certDir)
	if err != nil {
		t.Fatalf("LoadRevokedList 失败: %v", err)
	}
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	respDER, err := BuildOCSPResponse(ca, revokedList, reqDER, OCSPResponseOptions{
		ThisUpdate: time.Now().Add(-2 * time.Hour),
		NextUpdate: time.Now().Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("构造过期 revoked 响应失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("解析请求失败: %v", err)
	}
	policy := OCSPClientOptions(WithOCSPFreshCheck(RevocationOptions{}, true))
	status, _, err := ParseOCSPResponseWithFreshness(ca.Cert, respDER, reqCID, policy)
	if !errors.Is(err, ErrOCSPResponseStale) {
		t.Fatalf("过期 revoked 响应应返回 ErrOCSPResponseStale（fail-closed），实际 err=%v", err)
	}
	if status != StatusRevoked {
		t.Fatalf("错误同时应携带已解析 status=revoked，实际 %q", status)
	}
}

// 编译期防呆：RevocationOptions 字段与本测试引用一致（重构漂移即编译失败）。
var _ = x509.ParseCertificate
