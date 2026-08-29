package e2e

// MQTT 设备链路端到端测试（v0.24.0，F-3 接线验收）：
//   mqttsim 测试 broker → edgecore MQTT Mapper（订阅型采集）→ 云端
//   /api/v1/devices 可见 mqtt-device-01 属性 → 下发 device-command →
//   Mapper 发布到 cmd 主题（sim.Received 可见）→ 模拟设备回发新状态 →
//   Properties 收敛（完整闭环）。
//
// 链路（真实装配路径，无需 Docker，与 opcua e2e 同构）：
//   测试进程内起 pkg/mqttsim（127.0.0.1:0）→ edgecore 进程经环境变量
//   装配 EDGEFLOW_MQTT_BROKER/TOPICS/CMD_TOPIC（Mapper 注册开关=broker
//   非空，与 EDGEFLOW_MODBUS_ADDR / EDGEFLOW_OPCUA_ENDPOINT 同款 opt-in）
//   → broker.Publish 模拟设备上报 → 订阅回调合并快照 →
//   collectMapperReports 周期汇入影子 → DeviceReport 上报 → 云端可见 →
//   POST device-command → HandleCommand 发布指令 → sim.Received() 断言
//   → 模拟设备回发 → Properties 收敛。
//
// 说明：Mapper 的存活探针每 2s 向 edgeflow/mqtt/health 发布空消息，
// sim.Received() 会含这些记录——断言一律按主题过滤，不受探针噪声影响。
//
// 运行：go test -v -timeout 20m ./tests/e2e/ -run TestMQTTDeviceE2E
import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"edgeflow/pkg/mqttsim"
)

// MQTT e2e 固定主题/设备名（与 edgecore 装配 env 一致）。
const (
	mqttE2EDevice     = "mqtt-device-01"
	mqttE2EStateTopic = "devices/mqtt-dev-01/state"
	mqttE2ECmdTopic   = "devices/mqtt-dev-01/cmd"
)

// TestMQTTDeviceE2E 验证 MQTT 设备从模拟上报到云端属性、指令下发、
// 状态收敛的完整闭环。
func TestMQTTDeviceE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	// 1. 测试进程内起 MQTT broker（空闲端口）
	sim, err := mqttsim.NewBroker()
	if err != nil {
		t.Fatalf("MQTT 测试 broker 启动失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })
	t.Logf("MQTT broker 就绪（%s）", sim.Addr())

	// 2. 启动 cloudcore
	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	// 3. 启动 edgecore（追加 MQTT Mapper 装配 env——真实装配路径）
	nodeID := "e2e-mqtt-1"
	dbPath := filepath.Join(t.TempDir(), "edgecore.db")
	cloudAddr := "ws://127.0.0.1:" + strconv.Itoa(hubPort)
	env := append(edgeEnv(nodeID, cloudAddr, dbPath),
		"EDGEFLOW_MQTT_BROKER="+sim.Addr(),
		"EDGEFLOW_MQTT_TOPICS=devices/+/state",
		"EDGEFLOW_MQTT_DEVICE_NAME="+mqttE2EDevice,
		"EDGEFLOW_MQTT_CMD_TOPIC="+mqttE2ECmdTopic,
	)
	startProcess(t, "edgecore-mqtt", filepath.Join(binDir, "edgecore"), nil, env)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册", nodeID)

	// 4. broker.Publish 模拟设备上报 → 属性到达云端
	first := map[string]float64{"temperature": 25.5, "humidity": 60}
	mqttE2EPublish(t, sim, first)
	t.Logf("已模拟设备上报：%v → %s", first, mqttE2EStateTopic)
	waitMQTTProps(t, base, nodeID, mqttE2EDevice, first, 60*time.Second)
	t.Logf("MQTT 设备已同步到云端：%v", first)

	// 5. 下发 device-command（边缘 Ack = Mapper HandleCommand 已发布 cmd）
	const property = "setpoint"
	const wantValue = 42.0
	url := fmt.Sprintf("%s/api/v1/nodes/%s/device-command", base, nodeID)
	postJSON(t, url, map[string]any{
		"deviceName": mqttE2EDevice,
		"namespace":  "default",
		"property":   property,
		"value":      wantValue,
	})
	t.Logf("设备指令已下发：%s.%s=%v（边缘已 Ack）", mqttE2EDevice, property, wantValue)

	// 6. 断言指令已发布到 cmd 主题（HandleCommand → Publish 的对端证据）
	waitMQTTPublished(t, sim, mqttE2ECmdTopic, map[string]any{
		"deviceName": mqttE2EDevice,
		"property":   property,
		"value":      wantValue,
	}, 10*time.Second)
	t.Logf("指令已到达 cmd 主题 %s", mqttE2ECmdTopic)

	// 7. Desired 收敛（指令值已写入影子 Desired）
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

	// 8. 模拟设备执行指令后回发新状态 → Properties 收敛（闭环收尾）
	second := map[string]float64{"temperature": 26.5, "humidity": 55, "setpoint": wantValue}
	mqttE2EPublish(t, sim, second)
	final := waitMQTTProps(t, base, nodeID, mqttE2EDevice, second, 60*time.Second)
	t.Logf("MQTT 指令闭环完成：Properties=%v", final.Properties)
}

// mqttE2EPublish 用 broker 服务端推送模拟一次设备上报（JSON 属性）。
func mqttE2EPublish(t *testing.T, sim *mqttsim.Broker, props map[string]float64) {
	t.Helper()
	payload, err := json.Marshal(props)
	if err != nil {
		t.Fatalf("序列化上报 payload 失败: %v", err)
	}
	if err := sim.Publish(mqttE2EStateTopic, payload); err != nil {
		t.Fatalf("向 %s 上报失败: %v", mqttE2EStateTopic, err)
	}
}

// waitMQTTProps 轮询云端设备列表，直到目标设备 Properties 覆盖 want
// （键存在且值相等）后返回该条目；超时 Fatal（附当前值辅助诊断）。
func waitMQTTProps(t *testing.T, base, nodeID, deviceName string, want map[string]float64, timeout time.Duration) deviceStatusItem {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var item *deviceStatusItem
	for time.Now().Before(deadline) {
		var list deviceStatusList
		getJSON(t, base+"/api/v1/devices", &list)
		item = nil
		for i := range list.Items {
			if list.Items[i].NodeID == nodeID && list.Items[i].DeviceName == deviceName {
				item = &list.Items[i]
				break
			}
		}
		if item != nil && mqttPropsMatch(item.Properties, want) {
			return *item
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待设备 %s 属性 %v 超时（%s）：当前 %v",
		deviceName, want, timeout, item)
	return deviceStatusItem{}
}

// mqttPropsMatch 判断 got 是否覆盖 want（键存在且值相等）。
func mqttPropsMatch(got, want map[string]float64) bool {
	if got == nil {
		return false
	}
	for k, wantV := range want {
		if gotV, ok := got[k]; !ok || gotV != wantV {
			return false
		}
	}
	return true
}

// waitMQTTPublished 断言 sim.Received() 中出现发往 topic 的客户端
// PUBLISH 且 payload JSON 含 expect 的全部键值（Mapper HandleCommand
// 发布结果的对端证据；探针 edgeflow/mqtt/health 与其他主题被过滤）。
func waitMQTTPublished(t *testing.T, sim *mqttsim.Broker, topic string, expect map[string]any, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, p := range sim.Received() {
			if p.Topic != topic {
				continue
			}
			var got map[string]any
			if err := json.Unmarshal(p.Payload, &got); err != nil {
				continue
			}
			match := true
			for k, wantV := range expect {
				if gv, ok := got[k]; !ok || fmt.Sprint(gv) != fmt.Sprint(wantV) {
					match = false
					break
				}
			}
			if match {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待 %s 上的指令发布超时（%s）", topic, timeout)
}
