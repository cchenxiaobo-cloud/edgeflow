package certs

// CRL 消费端握手级测试（WBS 7.1）：验证 LoadTLSConfig 附加的对端证书
// 吊销校验在真实 TLS 握手中生效。

import (
	"crypto/tls"
	"errors"

	"os"
	"strings"
	"testing"
	"time"
)

// handshakePair 用同一 certDir 起一对 mTLS 服务端/客户端并完成一次握手，
// 返回（客户端错误，服务端错误）：两者皆 nil 表示全链路放行；
// 拒绝场景的具体原因（吊销/fail-closed）在拒绝方一侧的错误文本中。
func handshakePair(t *testing.T, certDir string) (clientErr, serverErr error) {
	t.Helper()
	serverCfg, err := LoadTLSConfig(certDir, true)
	if err != nil {
		t.Fatalf("服务端 TLS 配置失败: %v", err)
	}
	clientCfg, err := LoadTLSConfig(certDir, false)
	if err != nil {
		t.Fatalf("客户端 TLS 配置失败: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()

	// 服务端接受侧：完成握手并把握手错误（含拒绝原因）传回。
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		defer conn.Close()
		acceptErr <- conn.(*tls.Conn).Handshake()
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	clientErr = err
	if err == nil {
		defer conn.Close()
	}
	select {
	case serverErr = <-acceptErr:
	case <-time.After(3 * time.Second):
		serverErr = errors.New("服务端握手超时")
	}
	return clientErr, serverErr
}

// wantServerReject 断言：客户端侧失败（收到 alert）且服务端侧含期望拒绝原因。
func wantServerReject(t *testing.T, clientErr, serverErr error, reason string) {
	t.Helper()
	if serverErr == nil || !strings.Contains(serverErr.Error(), reason) {
		t.Fatalf("服务端应拒绝握手（%s），实际 serverErr=%v", reason, serverErr)
	}
	if clientErr == nil && reason == "已被吊销" {
		// TLS1.3 下客户端可能已完成本地握手后才收到 alert；
		// 服务端拒绝已足以证明吊销校验生效。
		t.Log("注：客户端侧未立即报错（alert 延迟），以服务端拒绝为准")
	}
	t.Logf("拒绝原因确认: %v", serverErr)
}

func newCRLCertDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := EnsureCA(dir); err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	if _, err := EnsureServerCert(dir, "cloudcore"); err != nil {
		t.Fatalf("EnsureServerCert 失败: %v", err)
	}
	if _, err := EnsureClientCert(dir, "edgeflow-edgecore"); err != nil {
		t.Fatalf("EnsureClientCert 失败: %v", err)
	}
	return dir
}

func TestTLSHandshakeWithoutCRL(t *testing.T) {
	dir := newCRLCertDir(t)
	clientErr, serverErr := handshakePair(t, dir)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("无 CRL 环境应放行，实际 clientErr=%v serverErr=%v", clientErr, serverErr)
	}
}

func TestTLSHandshakeRejectsRevokedClientCert(t *testing.T) {
	dir := newCRLCertDir(t)
	// 先确认基线握手成功。
	if clientErr, serverErr := handshakePair(t, dir); clientErr != nil || serverErr != nil {
		t.Fatalf("吊销前握手应成功: clientErr=%v serverErr=%v", clientErr, serverErr)
	}
	// 吊销客户端证书（边缘节点 edgeflow-edgecore）。
	if err := RevokeCert(dir, "edgeflow-edgecore"); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	clientErr, serverErr := handshakePair(t, dir)
	wantServerReject(t, clientErr, serverErr, "已被吊销")
}

func TestTLSHandshakeRejectsRevokedServerCert(t *testing.T) {
	// 对称校验：吊销服务端证书后，客户端应拒绝握手。
	dir := newCRLCertDir(t)
	if err := RevokeCert(dir, "cloudcore"); err != nil {
		t.Fatalf("RevokeCert(cloudcore) 失败: %v", err)
	}
	clientErr, serverErr := handshakePair(t, dir)
	// 客户端校验服务端证书，拒绝原因出现在客户端侧。
	if clientErr == nil || !strings.Contains(clientErr.Error(), "已被吊销") {
		t.Fatalf("客户端应拒绝被吊销的服务端证书，实际 clientErr=%v", clientErr)
	}
	if serverErr == nil {
		t.Log("注：服务端侧未感知拒绝（客户端主动中止）")
	}
	t.Logf("客户端拒绝确认: %v", clientErr)
}

func TestTLSHandshakeCorruptCRLFailsClosed(t *testing.T) {
	dir := newCRLCertDir(t)
	crlPEM, _ := CRLPaths(dir)
	if err := os.WriteFile(crlPEM, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("写损坏 CRL 失败: %v", err)
	}
	clientErr, serverErr := handshakePair(t, dir)
	// 双侧均附加 CRL 校验：客户端先校验服务端证书时即遇到损坏 CRL 并
	// fail-closed 拒绝（serverErr 侧只能收到通用 alert）。
	if clientErr == nil || !strings.Contains(clientErr.Error(), "fail-closed") {
		t.Fatalf("客户端应 fail-closed 拒绝，实际 clientErr=%v serverErr=%v", clientErr, serverErr)
	}
	t.Logf("CRL 损坏时握手按预期 fail-closed: %v", clientErr)
}

func TestTLSHandshakeRevokedThenReissue(t *testing.T) {
	// 吊销后重新签发（rotate 场景）：新证书应恢复接入。
	dir := newCRLCertDir(t)
	if err := RevokeCert(dir, "edgeflow-edgecore"); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	if _, serverErr := handshakePair(t, dir); serverErr == nil {
		t.Fatal("吊销后握手应被拒绝")
	}
	if _, err := RotateClientCert(dir, "edgeflow-edgecore"); err != nil {
		t.Fatalf("RotateClientCert 失败: %v", err)
	}
	if clientErr, serverErr := handshakePair(t, dir); clientErr != nil || serverErr != nil {
		t.Fatalf("重新签发后应恢复接入，实际 clientErr=%v serverErr=%v", clientErr, serverErr)
	}
}

func TestLoadTLSConfigRejectsMissingCA(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadTLSConfig(dir, true); err == nil {
		t.Fatal("缺 CA 时应报错")
	}
}
