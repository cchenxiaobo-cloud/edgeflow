// v0281_security_e2e_test.go — v0.28.1 OPN 体加密端到端（package opcua_test，
// 可安全导入 opcuasim；与 v0280 e2e 同布局）。
//
// 覆盖：
//  1. WithIdentity 注入服务端身份后：客户端 Basic256Sha256 OPN（加密体+签名）
//     与 sim（解封+验签+加密响应）端到端互通，MSG 服务可用。
//  2. 未注入身份：保持 v0.28.0 语义，非 None 策略显式拒绝（冻结兼容）。

package opcua_test

import (
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/opcua"
	"edgeflow/pkg/opcuasim"
)

// TestV0281EndToEndBasic256Sha256：加密 OPN 双向互通 + 通道可用。
func TestV0281EndToEndBasic256Sha256(t *testing.T) {
	key, cert := v0280SelfSigned(t)
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithIdentity(cert, key))
	if err := sim.Start(); err != nil {
		t.Fatalf("sim 启动: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	endpoint := "opc.tcp://" + sim.Addr()

	opts := opcua.OpenSecureChannelOptions{
		SecurityPolicyURI: opcua.SecurityPolicyBasic256Sha256URI,
		ClientCert:        cert,
		ClientKey:         key,
		ServerCert:        cert, // 自签环回：客户端 pin = 服务端证书
	}
	c, err := opcua.OpenWithOptions(endpoint, 5*time.Second, opts)
	if err != nil {
		t.Fatalf("Basic256Sha256 OPN 应端到端成功: %v", err)
	}
	defer c.Close()
	// 通道就绪探针：MSG 服务（PubAck）在加密协商后的通道上照常工作；
	// 连接建立本身已证明 channelId/tokenId 协商成功（否则 OPN 阶段已报错）。
	if err := c.PubAck(); err != nil {
		t.Fatalf("B256 通道 MSG 探针失败: %v", err)
	}
}

// TestV0281SimWithoutIdentityStillRejects：未注入身份 = v0.28.0 显式拒绝。
func TestV0281SimWithoutIdentityStillRejects(t *testing.T) {
	sim := opcuasim.New("127.0.0.1:0")
	if err := sim.Start(); err != nil {
		t.Fatalf("sim 启动: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	endpoint := "opc.tcp://" + sim.Addr()

	key, cert := v0280SelfSigned(t)
	opts := opcua.OpenSecureChannelOptions{
		SecurityPolicyURI: opcua.SecurityPolicyBasic256Sha256URI,
		ClientCert:        cert,
		ClientKey:         key,
		ServerCert:        cert,
	}
	_, err := opcua.OpenWithOptions(endpoint, 3*time.Second, opts)
	if err == nil {
		t.Fatal("无身份 sim 应显式拒绝 Basic256Sha256")
	}
	if !strings.Contains(err.Error(), "Bad_SecurityPolicyRejected") {
		t.Fatalf("应保持 v0.28.0 拒绝语义，得到: %v", err)
	}
}
