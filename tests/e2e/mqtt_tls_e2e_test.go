package e2e

// MQTT TLS 设备链路端到端测试（v0.25.0，TLS 轮验收）：
// 与 TestMQTTDeviceE2E 同构的完整闭环，唯一差异是 MQTT 链路全程走 TLS：
//   mqttsim TLS broker（测试内生成自签证书）→ edgecore MQTT Mapper 经
//   EDGEFLOW_MQTT_TLS_CA 指向 CA 文件注入 RootCAs → TLS Dial → 订阅采集
//   → 云端属性可见 → device-command 下发 → cmd 主题（密钥通道）→
//   Desired 收敛 → 回发新状态 → Properties 收敛。
//
// 运行：go test -v -timeout 20m ./tests/e2e/ -run TestMQTTTLSDeviceE2E
import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"edgeflow/pkg/mqttsim"
)

// mqttTLSCert 生成自签服务器证书（ECDSA P-256，IsCA 便于直接充当测试
// RootCA，SAN 含 127.0.0.1 与 localhost），返回 (certPEM, keyPEM)。
func mqttTLSCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "edgeflow-e2e-tls"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("序列化私钥失败: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// TestMQTTTLSDeviceE2E 验证 MQTT 设备闭环全程走 TLS：上报、指令下发、
// 状态收敛与明文版语义一致，差异仅在传输加密。
func TestMQTTTLSDeviceE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	// 1. 测试进程内起 MQTT TLS broker（空闲端口 + 测试证书）
	certPEM, keyPEM := mqttTLSCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("加载证书对失败: %v", err)
	}
	sim, err := mqttsim.NewBrokerTLS(&tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("MQTT TLS 测试 broker 启动失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })
	caPath := filepath.Join(t.TempDir(), "mqtt-ca.pem")
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatalf("写 CA 文件失败: %v", err)
	}
	t.Logf("MQTT TLS broker 就绪（%s，CA=%s）", sim.Addr(), caPath)

	// 2. 启动 cloudcore
	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	// 3. 启动 edgecore（MQTT Mapper 装配 env + TLS CA——真实装配路径）
	nodeID := "e2e-mqtt-tls-1"
	dbPath := filepath.Join(t.TempDir(), "edgecore.db")
	cloudAddr := "ws://127.0.0.1:" + strconv.Itoa(hubPort)
	env := append(edgeEnv(nodeID, cloudAddr, dbPath),
		"EDGEFLOW_MQTT_BROKER="+sim.Addr(),
		"EDGEFLOW_MQTT_TOPICS=devices/+/state",
		"EDGEFLOW_MQTT_DEVICE_NAME="+mqttE2EDevice,
		"EDGEFLOW_MQTT_CMD_TOPIC="+mqttE2ECmdTopic,
		"EDGEFLOW_MQTT_TLS_CA="+caPath,
	)
	startProcess(t, "edgecore-mqtt-tls", filepath.Join(binDir, "edgecore"), nil, env)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册", nodeID)

	// 4. broker.Publish 模拟设备上报（TLS 密钥通道）→ 属性到达云端
	first := map[string]float64{"temperature": 27.5, "humidity": 58}
	mqttE2EPublish(t, sim, first)
	waitMQTTProps(t, base, nodeID, mqttE2EDevice, first, 60*time.Second)
	t.Logf("TLS 链路设备上报已同步到云端：%v", first)

	// 5. 下发 device-command（边缘 Ack = Mapper HandleCommand 已发布 cmd）
	const property = "setpoint"
	const wantValue = 43.0
	url := fmt.Sprintf("%s/api/v1/nodes/%s/device-command", base, nodeID)
	postJSON(t, url, map[string]any{
		"deviceName": mqttE2EDevice,
		"namespace":  "default",
		"property":   property,
		"value":      wantValue,
	})
	t.Logf("设备指令已下发：%s.%s=%v（边缘已 Ack）", mqttE2EDevice, property, wantValue)

	// 6. 断言指令经 TLS 通道发布到 cmd 主题
	waitMQTTPublished(t, sim, mqttE2ECmdTopic, map[string]any{
		"deviceName": mqttE2EDevice,
		"property":   property,
		"value":      wantValue,
	}, 10*time.Second)
	t.Logf("指令已到达 cmd 主题 %s（TLS）", mqttE2ECmdTopic)

	// 7. Desired 收敛
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if got := desiredOf(t, base, nodeID, mqttE2EDevice); got[property] == wantValue {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if got := desiredOf(t, base, nodeID, mqttE2EDevice); got[property] != wantValue {
		t.Fatalf("Desired[%s] = %v，期望 %v", property, got[property], wantValue)
	}

	// 8. 模拟设备回发新状态 → Properties 收敛（TLS 闭环收尾）
	second := map[string]float64{"temperature": 28.5, "humidity": 52, "setpoint": wantValue}
	mqttE2EPublish(t, sim, second)
	final := waitMQTTProps(t, base, nodeID, mqttE2EDevice, second, 60*time.Second)
	t.Logf("MQTT TLS 指令闭环完成：Properties=%v", final.Properties)
}
