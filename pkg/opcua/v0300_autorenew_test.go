package opcua_test

// v0.30.0 OPC-UA 自动续期测试（spec 0003 US-6）：短寿命令牌下 75% 比例
// 自动 Renew 持续换钥、开关关闭时保持 v0.29.0 行为（冻结）、None 通道
// 不启动循环。复用 v0280SelfSigned（同包助手）与 opcuasim。

import (
	"testing"
	"time"

	"edgeflow/pkg/opcua"
	"edgeflow/pkg/opcuasim"
)

// v0300WaitTokenChange 轮询等待 TokenID 变化（自动续期发生）。
func v0300WaitTokenChange(t *testing.T, c *opcua.Client, baseline uint32, wait time.Duration) uint32 {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if id := c.TokenID(); id != baseline {
			return id
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("令牌 %d 在 %v 内未变化（自动续期未发生）", baseline, wait)
	return 0
}

// TestV0300AutoRenewBasic256Sha256：短寿命 + ratio=0.75 → 自动续期反复
// 发生，续期间客户端服务调用持续可用（换钥窗口收敛语义复用 v0.29.0）。
func TestV0300AutoRenewBasic256Sha256(t *testing.T) {
	key, cert := v0280SelfSigned(t)
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithSeed(41), opcuasim.WithIdentity(cert, key), opcuasim.WithTokenLifetime(800))
	if err := sim.Start(); err != nil {
		t.Fatalf("sim 启动: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	endpoint := "opc.tcp://" + sim.Addr()

	opts := opcua.OpenSecureChannelOptions{
		SecurityPolicyURI: opcua.SecurityPolicyBasic256Sha256URI,
		ClientCert:        cert,
		ClientKey:         key,
		ServerCert:        cert,
		AutoRenewRatio:    0.75,
	}
	c, err := opcua.OpenWithOptions(endpoint, 5*time.Second, opts)
	if err != nil {
		t.Fatalf("B256 + AutoRenew OPN 应成功: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 第一轮自动续期（0.75 × 800ms = 600ms 触发）
	tok0 := c.TokenID()
	tok1 := v0300WaitTokenChange(t, c, tok0, 3*time.Second)
	// 第二轮（新寿命 800ms 的 75% 再触发）→ 至少出现第三个令牌
	tok2 := v0300WaitTokenChange(t, c, tok1, 3*time.Second)
	_ = tok2

	// 续期间服务调用持续可用（新钥组生效的证明）
	if _, err := c.Read([]opcua.NodeId{opcua.NewNodeID(2, 1001)}); err != nil {
		t.Fatalf("自动续期后 Read 应可用: %v", err)
	}
	// 显式 Renew 与自动续期互斥且仍可用
	if err := c.Renew(5 * time.Second); err != nil {
		t.Fatalf("显式 Renew: %v", err)
	}
}

// TestV0300AutoRenewOffFrozen：ratio=0（默认）→ 无自动续期（v0.29.0
// 行为冻结）；服务调用正常。
func TestV0300AutoRenewOffFrozen(t *testing.T) {
	key, cert := v0280SelfSigned(t)
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithSeed(42), opcuasim.WithIdentity(cert, key), opcuasim.WithTokenLifetime(800))
	if err := sim.Start(); err != nil {
		t.Fatalf("sim 启动: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	endpoint := "opc.tcp://" + sim.Addr()

	c, err := opcua.OpenWithOptions(endpoint, 5*time.Second, opcua.OpenSecureChannelOptions{
		SecurityPolicyURI: opcua.SecurityPolicyBasic256Sha256URI,
		ClientCert:        cert,
		ClientKey:         key,
		ServerCert:        cert,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	tok0 := c.TokenID()
	time.Sleep(1500 * time.Millisecond) // > 两个短寿命周期
	if c.TokenID() != tok0 {
		t.Fatal("ratio=0 时不应发生自动续期")
	}
	if _, err := c.Read([]opcua.NodeId{opcua.NewNodeID(2, 1001)}); err != nil {
		t.Fatalf("Read 应可用: %v", err)
	}
}

// TestV0300AutoRenewNoneNoop：None 通道 + ratio>0 → 循环不启动，行为
// 与 v0.29.0 None 一致（无令牌可续，无 panic）。
func TestV0300AutoRenewNoneNoop(t *testing.T) {
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithSeed(43))
	if err := sim.Start(); err != nil {
		t.Fatalf("sim 启动: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	endpoint := "opc.tcp://" + sim.Addr()

	c, err := opcua.OpenWithOptions(endpoint, 5*time.Second, opcua.OpenSecureChannelOptions{AutoRenewRatio: 0.75})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Read([]opcua.NodeId{opcua.NewNodeID(2, 1001)}); err != nil {
		t.Fatalf("None 通道 Read 应可用: %v", err)
	}
}

// TestV0300AutoRenewRatioValidation：AutoRenewRatio 越界拒绝。
func TestV0300AutoRenewRatioValidation(t *testing.T) {
	if _, err := opcua.OpenWithOptions("opc.tcp://127.0.0.1:1", time.Second, opcua.OpenSecureChannelOptions{AutoRenewRatio: 1.5}); err == nil {
		t.Fatal("ratio=1.5 应拒绝")
	}
	if _, err := opcua.OpenWithOptions("opc.tcp://127.0.0.1:1", time.Second, opcua.OpenSecureChannelOptions{AutoRenewRatio: -0.1}); err == nil {
		t.Fatal("ratio=-0.1 应拒绝")
	}
}
