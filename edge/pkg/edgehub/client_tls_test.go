package edgehub

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/certs"
	"edgeflow/pkg/protocol"
)

// startMockCloudTLS 启动 TLS 版 mock 云端（与 startMockCloud 同构，
// 只是监听层套上 mTLS：服务端证书 + 强制要求客户端证书）。
func startMockCloudTLS(t *testing.T, cfg mockConfig, tlsCfg *tls.Config) *mockCloud {
	t.Helper()
	m := &mockCloud{
		t:          t,
		cfg:        cfg,
		received:   make(chan *protocol.Message, 256),
		registers:  make(chan *protocol.Message, 64),
		heartbeats: make(chan struct{}, 256),
		connCount:  make(chan struct{}, 64),
	}
	m.srv = httptest.NewUnstartedServer(http.HandlerFunc(m.handle))
	m.srv.TLS = tlsCfg
	m.srv.StartTLS()
	t.Cleanup(m.srv.Close)
	m.url = "wss" + strings.TrimPrefix(m.srv.URL, "https") + channelPath
	return m
}

// tlsFixture 生成一套 mTLS 证书（CA + 服务端 + 客户端），返回两侧 tls.Config。
func tlsFixture(t *testing.T) (serverCfg, clientCfg *tls.Config) {
	t.Helper()
	certDir := t.TempDir() + "/certs"
	if _, err := certs.EnsureCA(certDir); err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	if _, err := certs.EnsureServerCert(certDir, "cloudcore"); err != nil {
		t.Fatalf("EnsureServerCert 失败: %v", err)
	}
	if _, err := certs.EnsureClientCert(certDir, "edgeflow-tls-node"); err != nil {
		t.Fatalf("EnsureClientCert 失败: %v", err)
	}
	serverCfg, err := certs.LoadTLSConfig(certDir, true)
	if err != nil {
		t.Fatalf("服务端 TLS 配置失败: %v", err)
	}
	clientCfg, err = certs.LoadTLSConfig(certDir, false)
	if err != nil {
		t.Fatalf("客户端 TLS 配置失败: %v", err)
	}
	return serverCfg, clientCfg
}

// TestNew_TLSAddressNormalization 验证地址归一化：
//   - TLS off（默认）：ws:// 原样，路径自动补全；
//   - TLS on（注入 TLSConfig）：ws:// 自动升级为 wss://；
//   - 地址本身 wss://：协议保持，路径自动补全。
func TestNew_TLSAddressNormalization(t *testing.T) {
	// 基线：TLS off，行为与历史完全一致
	c := New(Options{})
	if got := c.Address(); got != "ws://127.0.0.1:10000/v1/edge" {
		t.Errorf("TLS off Address() = %q，期望 ws://127.0.0.1:10000/v1/edge", got)
	}

	// TLS on：ws:// → wss://
	c = New(Options{TLSConfig: &tls.Config{}})
	if got := c.Address(); got != "wss://127.0.0.1:10000/v1/edge" {
		t.Errorf("TLS on Address() = %q，期望 wss://127.0.0.1:10000/v1/edge", got)
	}

	// 地址显式 wss://（如经 TLS 网关），无 TLSConfig：协议与路径均保持
	c = New(Options{CloudAddr: "wss://hub.example.com:10443"})
	if got := c.Address(); got != "wss://hub.example.com:10443/v1/edge" {
		t.Errorf("显式 wss 地址 Address() = %q，期望 wss://hub.example.com:10443/v1/edge", got)
	}

	// 显式 wss:// + TLSConfig：保持 wss
	c = New(Options{CloudAddr: "wss://hub.example.com:10443", TLSConfig: &tls.Config{}})
	if got := c.Address(); got != "wss://hub.example.com:10443/v1/edge" {
		t.Errorf("wss+TLSConfig Address() = %q", got)
	}
}

// TestClient_FullRegisterOverTLS 集成测试：EdgeHub 经真实 mTLS 通道完成
// 连接→注册→心跳全流程（云侧 RequireAndVerifyClientCert，边侧携带证书）。
func TestClient_FullRegisterOverTLS(t *testing.T) {
	serverCfg, clientCfg := tlsFixture(t)
	mock := startMockCloudTLS(t, mockConfig{}, serverCfg)

	c := New(Options{
		CloudAddr:         mock.url,
		NodeID:            "edge-tls-1",
		TLSConfig:         clientCfg,
		HeartbeatInterval: 50 * time.Millisecond,
		BackoffBase:       10 * time.Millisecond,
	})
	connected := make(chan bool, 4)
	c.SetStatusHandler(func(v bool) { connected <- v })
	c.Start()
	defer c.Stop()

	// 等待注册成功（上限 5s，防挂死）
	select {
	case v := <-connected:
		if !v {
			t.Fatal("状态回调 = disconnected，期望 connected")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内未完成 mTLS 注册（连接未建立？）")
	}
	if !c.IsConnected() {
		t.Fatal("IsConnected() = false，期望 true")
	}

	// 云端侧确认真实收到 Register（证据：mTLS 通道上的完整注册流程）
	select {
	case reg := <-mock.registers:
		if reg.Source != "edge-tls-1" {
			t.Errorf("云端收到 Register source=%q，期望 edge-tls-1", reg.Source)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("云端 3s 内未收到 Register")
	}
	// 心跳也在 mTLS 通道上正常往返
	select {
	case <-mock.heartbeats:
	case <-time.After(3 * time.Second):
		t.Fatal("云端 3s 内未收到 Heartbeat")
	}
}

// TestClient_TLSRejectsUntrustedServer 集成测试：客户端 RootCAs 不信任
// 服务端证书（用另一个 CA 的证书树）→ 握手被客户端拒绝，连接无法建立，
// 云端也收不到任何升级请求（upgrades == 0）。
func TestClient_TLSRejectsUntrustedServer(t *testing.T) {
	serverCfg, _ := tlsFixture(t)
	mock := startMockCloudTLS(t, mockConfig{}, serverCfg)

	// 恶意/错误客户端：信任另一个 CA（rogue），不携带服务端 CA 的信任链
	rogueDir := t.TempDir() + "/rogue"
	if _, err := certs.EnsureCA(rogueDir); err != nil {
		t.Fatalf("生成 rogue CA 失败: %v", err)
	}
	rogueCfg, err := certs.LoadTLSConfig(rogueDir, false)
	if err != nil {
		t.Fatalf("rogue 客户端 TLS 配置失败: %v", err)
	}

	c := New(Options{
		CloudAddr:   mock.url,
		NodeID:      "edge-rogue",
		TLSConfig:   rogueCfg,
		BackoffBase: 20 * time.Millisecond, // 快速重试，尽快暴露失败
	})
	c.Start()
	defer c.Stop()

	// 等待多个重试周期，确认从未连上
	time.Sleep(300 * time.Millisecond)
	if c.IsConnected() {
		t.Fatal("不可信服务端证书时不应建立连接")
	}
	mock.mu.Lock()
	upgrades := mock.upgrades
	mock.mu.Unlock()
	if upgrades != 0 {
		t.Errorf("云端收到 %d 次升级请求（TLS 握手应全部在升级前失败）", upgrades)
	}
}
