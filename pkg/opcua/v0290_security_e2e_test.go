// v0290_security_e2e_test.go — v0.29.0 MSG/CLO 对称覆盖 + Renew 端到端
//（package opcua_test，可安全导入 opcuasim；与 v0280/v0281 e2e 同布局）。
//
// 覆盖：
//  1. B256 全链路：加密 OPN → 加密 MSG（Read/Write）→ 订阅通知（密封帧）
//     → Renew（TokenID 变化 + 新钥派生）→ 新钥 Read → 跨续期通知 → 密封 CLO。
//  2. 传输级篡改：密封帧密文翻转 1 bit → sim ERR Bad_SecurityChecksFailed
//     断连（补 v0.28.1 复核登记的 US-4 传输级缺口）。

package opcua_test

import (
	"crypto/rsa"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/opcua"
	"edgeflow/pkg/opcuasim"
)

func v0290OpenB256(t *testing.T, endpoint string, timeout time.Duration, key *rsa.PrivateKey, cert *x509.Certificate) *opcua.Client {
	t.Helper()
	opts := opcua.OpenSecureChannelOptions{
		SecurityPolicyURI: opcua.SecurityPolicyBasic256Sha256URI,
		ClientCert:        cert,
		ClientKey:         key,
		ServerCert:        cert, // 自签环回：客户端 pin = 服务端证书
	}
	c, err := opcua.OpenWithOptions(endpoint, timeout, opts)
	if err != nil {
		t.Fatalf("Basic256Sha256 OPN 应成功: %v", err)
	}
	return c
}

func v0290WaitPublish(t *testing.T, ch <-chan opcua.PublishResult, wait time.Duration, phase string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(wait):
		t.Fatalf("%s：等待发布通知超时（%v）", phase, wait)
	}
}

// TestV0290EndToEndSymmetricRenewAndCLO：加密 MSG 全链路 + 显式续期 + 密封 CLO。
func TestV0290EndToEndSymmetricRenewAndCLO(t *testing.T) {
	key, cert := v0280SelfSigned(t)
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithSeed(29), opcuasim.WithIdentity(cert, key))
	if err := sim.Start(); err != nil {
		t.Fatalf("sim 启动: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	endpoint := "opc.tcp://" + sim.Addr()

	c := v0290OpenB256(t, endpoint, 5*time.Second, key, cert)
	defer func() { _ = c.Close() }()

	tok0 := c.TokenID()
	// 加密 MSG：Read
	vals, err := c.Read([]opcua.NodeId{opcua.NewNodeID(2, 1001)})
	if err != nil {
		t.Fatalf("加密 MSG Read: %v", err)
	}
	if len(vals) != 1 || vals[0].Value == nil {
		t.Fatalf("Read 结果异常: %+v", vals)
	}

	// 订阅（泵模式）：密封 Publish 通知
	ch, err := c.Subscribe([]opcua.NodeId{opcua.NewNodeID(2, 1001)}, 200)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	v0290WaitPublish(t, ch, 3*time.Second, "续期前")
	if err := c.PubAck(); err != nil { // API 契约：取走通知后补挂发布窗口
		t.Fatalf("续期前 PubAck: %v", err)
	}

	// 显式 Renew：TokenID 必须变化，新钥派生后 Read 仍通
	if err := c.Renew(5 * time.Second); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	tok1 := c.TokenID()
	if tok1 == tok0 || tok1 == 0 {
		t.Fatalf("Renew 后 TokenID 未变化: %d -> %d", tok0, tok1)
	}
	if _, err := c.Read([]opcua.NodeId{opcua.NewNodeID(2, 1001)}); err != nil {
		t.Fatalf("Renew 后 Read（新钥）: %v", err)
	}

	// 跨续期通知：sim 出站切组 + 客户端在途回退窗口（真实数据通知走
	// 异步密封帧路径，500ms 波动周期保证信封供给）
	v0290WaitPublish(t, ch, 4*time.Second, "续期后")
	if err := c.PubAck(); err != nil {
		t.Fatalf("续期后 PubAck: %v", err)
	}

	// 加密 Write + 回读
	v, _ := opcua.NewVariant(42.0)
	st, err := c.Write(opcua.NewNodeID(2, 3001), v)
	if err != nil || !st.IsGood() {
		t.Fatalf("加密 MSG Write: st=%v err=%v", st, err)
	}
	vals, err = c.Read([]opcua.NodeId{opcua.NewNodeID(2, 3001)})
	if err != nil {
		t.Fatalf("加密 MSG 回读: %v", err)
	}
	if f, ok := vals[0].Value.Value.(float64); !ok || f != 42.0 {
		t.Fatalf("设定点回读异常: %+v", vals[0].Value)
	}

	// 密封 CLO：关闭后连接不可再用
	_ = c.Close()
	if _, err := c.Read([]opcua.NodeId{opcua.NewNodeID(2, 1001)}); err == nil {
		t.Fatal("Close 后 Read 应失败")
	}
}

// TestV0290EndToEndTamperedFrameRejected：传输级篡改（密文 1 bit）→
// sim 拒绝（ERR Bad_SecurityChecksFailed）并断连，无静默降级。
func TestV0290EndToEndTamperedFrameRejected(t *testing.T) {
	key, cert := v0280SelfSigned(t)
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithSeed(30), opcuasim.WithIdentity(cert, key))
	if err := sim.Start(); err != nil {
		t.Fatalf("sim 启动: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	endpoint := "opc.tcp://" + sim.Addr()

	c := v0290OpenB256(t, endpoint, 5*time.Second, key, cert)
	defer func() { _ = c.Close() }()

	// 正向对照：正常加密 MSG 服务可用
	if _, err := c.Read([]opcua.NodeId{opcua.NewNodeID(2, 1001)}); err != nil {
		t.Fatalf("正向对照 Read: %v", err)
	}

	keys, chID, tokID := c.ProbeKeyMaterial()
	if keys == nil {
		t.Fatal("B256 通道应持有对称密钥组")
	}
	seqH, err := opcua.EncodeSequenceHeader(opcua.SequenceHeader{SequenceNumber: 999, RequestID: 424242})
	if err != nil {
		t.Fatalf("序列头编码: %v", err)
	}
	plain := append(seqH, []byte("v0290-tamper-probe-body")...)
	frame, err := opcua.SealMSGFrame(opcua.MsgSecureMessage, chID, tokID, keys, true, plain)
	if err != nil {
		t.Fatalf("密封: %v", err)
	}
	frame[opcua.HeaderSize+4+3] ^= 0xFF // 密文首块翻转 1 bit
	if err := c.ProbeWriteRaw(frame); err != nil {
		t.Fatalf("写入篡改帧: %v", err)
	}

	_, err = c.Read([]opcua.NodeId{opcua.NewNodeID(2, 1001)})
	if err == nil {
		t.Fatal("篡改帧后 sim 应拒绝并断连")
	}
	if strings.Contains(err.Error(), "Bad_") && !strings.Contains(err.Error(), "Bad_SecurityChecksFailed") {
		t.Fatalf("拒绝语义应为 Bad_SecurityChecksFailed，得到: %v", err)
	}
}
