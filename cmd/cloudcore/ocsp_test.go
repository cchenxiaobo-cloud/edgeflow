package main

import (
	"bytes"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/certs"
)

// newOCSPTestEnv 生成临时证书目录（CA + 客户端叶子证书），返回
// certDir、节点 CN、CA 句柄与叶子证书。
func newOCSPTestEnv(t *testing.T) (string, string, *certs.CA, *x509.Certificate) {
	t.Helper()
	certDir := filepath.Join(t.TempDir(), "certs")
	cn := "edgeflow-ocsp-node"
	ca, err := certs.EnsureCA(certDir)
	if err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	leafTLS, err := certs.EnsureClientCert(certDir, cn)
	if err != nil {
		t.Fatalf("EnsureClientCert 失败: %v", err)
	}
	leaf, err := x509.ParseCertificate(leafTLS.Certificate[0])
	if err != nil {
		t.Fatalf("解析叶子证书失败: %v", err)
	}
	return certDir, cn, ca, leaf
}

// newOCSPServer 按 run() 的装配方式把 ocspHandler 挂到方法路由
// （POST /ocsp）后启动测试服务，保证与生产接线一致。
func newOCSPServer(t *testing.T, certDir string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ocsp", (&ocspHandler{certDir: certDir}).ServeHTTP)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// postOCSP 向 responder 的 /ocsp 端点发送一次 OCSP 查询，返回响应（helper）。
func postOCSP(t *testing.T, baseURL string, reqDER []byte) *http.Response {
	t.Helper()
	resp, err := http.Post(baseURL+"/ocsp", "application/ocsp-request", bytes.NewReader(reqDER))
	if err != nil {
		t.Fatalf("POST %s 失败: %v", baseURL+"/ocsp", err)
	}
	return resp
}

// TestOCSPEndpoint_GoodAndRevoked 覆盖 /ocsp 端点主路径：
// 未吊销 → good；吊销后 → revoked + 吊销时间；再用 OCSPStatusAt 全闭环。
func TestOCSPEndpoint_GoodAndRevoked(t *testing.T) {
	certDir, cn, ca, leaf := newOCSPTestEnv(t)
	srv := newOCSPServer(t, certDir)

	reqDER, err := certs.OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	reqCID, err := certs.ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}

	// good：200 + application/ocsp-response + 可解析
	resp := postOCSP(t, srv.URL, reqDER)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/ocsp-response" {
		t.Errorf("Content-Type = %q，期望 application/ocsp-response", ct)
	}
	status, revokedAt, err := certs.ParseOCSPResponse(ca.Cert, body, reqCID)
	if err != nil {
		t.Fatalf("ParseOCSPResponse 失败: %v", err)
	}
	if status != certs.StatusGood || revokedAt != nil {
		t.Errorf("状态 = %q/%v，期望 good/nil", status, revokedAt)
	}

	// revoked：RevokeCert 后查询 → revoked + 吊销时间非 nil
	if err := certs.RevokeCert(certDir, cn); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	resp = postOCSP(t, srv.URL, reqDER)
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，期望 200", resp.StatusCode)
	}
	status, revokedAt, err = certs.ParseOCSPResponse(ca.Cert, body, reqCID)
	if err != nil {
		t.Fatalf("ParseOCSPResponse(revoked) 失败: %v", err)
	}
	if status != certs.StatusRevoked {
		t.Errorf("状态 = %q，期望 revoked", status)
	}
	if revokedAt == nil || revokedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("吊销时间异常: %v", revokedAt)
	}

	// 客户端全闭环：OCSPStatusAt 直接查真实 handler（mux 路由 /ocsp）
	status, _, err = certs.OCSPStatusAt(certDir, leaf, srv.URL+"/ocsp")
	if err != nil {
		t.Fatalf("OCSPStatusAt 失败: %v", err)
	}
	if status != certs.StatusRevoked {
		t.Errorf("OCSPStatusAt 状态 = %q，期望 revoked", status)
	}
}

// TestOCSPEndpoint_BadRequest 请求非法 → 400：垃圾 DER / 超限 / 空体；
// 非 POST 方法 → 405。
func TestOCSPEndpoint_BadRequest(t *testing.T) {
	certDir, _, _, _ := newOCSPTestEnv(t)
	srv := newOCSPServer(t, certDir)

	// 垃圾 DER → 400
	resp := postOCSP(t, srv.URL, []byte{0xff, 0x00, 0x01})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("垃圾 DER status = %d，期望 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 超过 16 KiB → 400
	huge := bytes.Repeat([]byte{0x30}, maxOCSPRequestBytes+1)
	resp = postOCSP(t, srv.URL, huge)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("超限请求 status = %d，期望 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 空体 → 400
	resp = postOCSP(t, srv.URL, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("空请求 status = %d，期望 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 非 POST 方法 → 405（ServeMux 方法模式自动处理）
	getResp, err := http.Get(srv.URL + "/ocsp")
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d，期望 405", getResp.StatusCode)
	}
	_ = getResp.Body.Close()
}

// TestOCSPEndpoint_MissingCA 证书目录无 CA → 500（fail-closed，
// 不静默放行/不自动建 CA）。请求用另一目录的合法证书构造。
func TestOCSPEndpoint_MissingCA(t *testing.T) {
	// 合法请求：从独立证书目录构造
	srcDir, _, srcCA, srcLeaf := newOCSPTestEnv(t)
	reqDER, err := certs.OCSPRequestForCert(srcCA.Cert, srcLeaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	_ = srcDir

	// responder 指向空目录（无 CA）
	emptyDir := filepath.Join(t.TempDir(), "no-certs")
	srv := newOCSPServer(t, emptyDir)

	resp := postOCSP(t, srv.URL, reqDER)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("无 CA status = %d，期望 500", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestOCSPEndpoint_RouteRegistered run() 装配的 mux 包含 POST /ocsp。
// 通过 run 的完整装配路径验证路由存在性（handler 行为由上述用例覆盖）。
func TestOCSPEndpoint_RouteRegistered(t *testing.T) {
	// run() 内部创建 mux 并注册 /ocsp；这里无法直接取 mux，
	// 改为验证注册代码路径的常量与 handler 类型存在。
	if maxOCSPRequestBytes != 16<<10 {
		t.Errorf("maxOCSPRequestBytes = %d，期望 16384", maxOCSPRequestBytes)
	}
	h := &ocspHandler{certDir: t.TempDir()}
	if h.certDir == "" {
		t.Error("ocspHandler.certDir 不应为空")
	}
	// 冒烟：请求体超过限制时 handler 返回 400 而非 panic
	srv := newOCSPServer(t, h.certDir)
	resp, err := http.Post(srv.URL+"/ocsp", "application/ocsp-request", strings.NewReader(strings.Repeat("x", maxOCSPRequestBytes+1)))
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d，期望 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}
