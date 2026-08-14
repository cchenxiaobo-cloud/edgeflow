package cloudhub

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/certs"
	"edgeflow/pkg/protocol"

	"github.com/gorilla/websocket"
)

// tlsTestFixture 生成一套证书并返回服务端/客户端 TLS 配置（mTLS 集成测试用）。
// certDir 由调用方提供（t.TempDir() 子目录）。
func tlsTestFixture(t *testing.T, certDir string) (serverCfg, clientCfg *tls.Config) {
	t.Helper()
	var err error
	if _, err := certs.EnsureCA(certDir); err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	if _, err := certs.EnsureServerCert(certDir, "cloudcore"); err != nil {
		t.Fatalf("EnsureServerCert 失败: %v", err)
	}
	if _, err := certs.EnsureClientCert(certDir, "edgeflow-tls-test"); err != nil {
		t.Fatalf("EnsureClientCert 失败: %v", err)
	}
	serverCfg, err = certs.LoadTLSConfig(certDir, true)
	if err != nil {
		t.Fatalf("服务端 TLS 配置失败: %v", err)
	}
	clientCfg, err = certs.LoadTLSConfig(certDir, false)
	if err != nil {
		t.Fatalf("客户端 TLS 配置失败: %v", err)
	}
	return serverCfg, clientCfg
}

// TestServer_WSRegisterOverTLS 集成测试：真实 mTLS 握手 + WebSocket 升级 +
// 完整注册流程（Register → RegisterAck accepted=true）。
// 验证 WithTLS 注入的 tls.Config 在监听层生效，且不影响协议行为。
func TestServer_WSRegisterOverTLS(t *testing.T) {
	certDir := t.TempDir() + "/certs"
	serverCfg, clientCfg := tlsTestFixture(t, certDir)

	srv := New("127.0.0.1:0", WithTLS(serverCfg))
	// 与 server_test.go 的 newTestServer 同构，但用未启动的 TLS 服务：
	// httptest 负责 TLS 监听层（等价于 Start 内 tls.NewListener 的路径），
	// 这里同时覆盖 WithTLS 注入的配置被真实握手使用。
	ts := httptest.NewUnstartedServer(srv.handler())
	ts.TLS = serverCfg
	ts.StartTLS()
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	wsURL := "wss" + strings.TrimPrefix(ts.URL, "https") + PathEdge
	dialer := &websocket.Dialer{TLSClientConfig: clientCfg, HandshakeTimeout: 5 * time.Second}
	ws, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("mTLS 拨号 %s 失败: %v", wsURL, err)
	}
	defer func() { _ = ws.Close() }()
	if resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("升级状态码 = %v，期望 101", resp)
	}

	// 走完整注册流程
	m, err := protocol.NewMessage(protocol.TypeRegister, "edge-tls-1", "cloud", RegisterPayload{
		NodeID:          "edge-tls-1",
		Arch:            "amd64",
		OS:              "linux",
		EdgecoreVersion: "v0.1.0",
		CPU:             4,
		Memory:          8192,
	})
	if err != nil {
		t.Fatalf("构造 Register 失败: %v", err)
	}
	sendMsg(t, ws, m)
	ack := readMsg(t, ws)
	if ack.Type != protocol.TypeRegisterAck {
		t.Fatalf("应答类型 = %q，期望 RegisterAck", ack.Type)
	}
	var payload RegisterAckPayload
	if err := ack.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 RegisterAck 失败: %v", err)
	}
	if !payload.Accepted {
		t.Fatalf("注册未被接受: %s", payload.Message)
	}
	if got := srv.NodeCount(); got != 1 {
		t.Errorf("NodeCount = %d，期望 1（mTLS 连接已注册）", got)
	}
}

// TestServer_RejectPlainWSOnTLS 集成测试：TLS 监听下用普通 ws:// 拨号
// → 握手失败（服务端在 TLS 层拒绝明文流量），协议层不可达。
func TestServer_RejectPlainWSOnTLS(t *testing.T) {
	certDir := t.TempDir() + "/certs"
	serverCfg, _ := tlsTestFixture(t, certDir)

	srv := New("127.0.0.1:0", WithTLS(serverCfg))
	ts := httptest.NewUnstartedServer(srv.handler())
	ts.TLS = serverCfg
	ts.StartTLS()
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// 注意：普通 ws:// 走 httptest 的 TLS 端口，必须显式关掉 TLSClientConfig
	// 或直接拨 TCP——这里模拟"edgecore 未开 TLS 却连到 TLS 端口"的真实场景：
	// gorilla 拨 ws:// 到 TLS 端口，TLS 层第一帧校验失败
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "https") + PathEdge
	dialer := &websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	_, _, err := dialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("普通 ws:// 拨 TLS 端口应失败（TLS 握手拒绝明文），实际成功")
	}
	t.Logf("明文拨号被拒（期望）：%v", err)

	// 服务端应不受影响：仍能正常服务 mTLS 连接
	clientCfg, err := certs.LoadTLSConfig(certDir, false)
	if err != nil {
		t.Fatalf("客户端 TLS 配置失败: %v", err)
	}
	secureDialer := &websocket.Dialer{TLSClientConfig: clientCfg, HandshakeTimeout: 5 * time.Second}
	secureURL := "wss" + strings.TrimPrefix(ts.URL, "https") + PathEdge
	ws, _, err := secureDialer.Dial(secureURL, nil)
	if err != nil {
		t.Fatalf("拒绝明文后 mTLS 拨号也应成功: %v", err)
	}
	_ = ws.Close()
}

// TestServer_RejectUntrustedClientCert 集成测试：携带非本 CA 签发证书的
// 客户端 → TLS 握手被服务端拒绝（mTLS 核心防线）。
func TestServer_RejectUntrustedClientCert(t *testing.T) {
	certDir := t.TempDir() + "/certs"
	serverCfg, _ := tlsTestFixture(t, certDir)

	srv := New("127.0.0.1:0", WithTLS(serverCfg))
	ts := httptest.NewUnstartedServer(srv.handler())
	ts.TLS = serverCfg
	ts.StartTLS()
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// 恶意 CA 签发的客户端证书
	rogueDir := t.TempDir() + "/rogue"
	if _, err := certs.EnsureCA(rogueDir); err != nil {
		t.Fatalf("生成恶意 CA 失败: %v", err)
	}
	rogueCert, err := certs.EnsureClientCert(rogueDir, "edgeflow-rogue")
	if err != nil {
		t.Fatalf("生成恶意客户端证书失败: %v", err)
	}
	rogueCfg, err := certs.LoadTLSConfig(rogueDir, false)
	if err != nil {
		t.Fatalf("恶意客户端 TLS 配置失败: %v", err)
	}
	_ = rogueCert // 已包含在 rogueCfg 内

	wsURL := "wss" + strings.TrimPrefix(ts.URL, "https") + PathEdge
	dialer := &websocket.Dialer{TLSClientConfig: rogueCfg, HandshakeTimeout: 3 * time.Second}
	_, _, err = dialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("非可信 CA 签发的客户端证书应被服务端拒绝，实际握手成功")
	}
	t.Logf("不可信客户端证书被拒（期望）：%v", err)
}
