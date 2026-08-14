package e2e

// 设备链路端到端测试（WBS 8.3 验收缺口 G1）：
//   下发 DeviceCommand（云端 API）→ 边缘执行并写入影子 → 设备状态上报
//   → 云端 /api/v1/devices 可见（含指令期望值 Desired 与实际上报值
//   Properties）。
//
// 链路（真实装配路径，无需 Docker）：
//   POST /api/v1/nodes/{nodeID}/device-command
//     → CloudHub 可靠投递 DeviceCommand → EdgeHub 回调 handleDeviceCommand
//     → Mapper（mock_sensor，sensor-01）执行 → 影子 Twin 更新
//     → 设备上报循环（300ms 周期）发送 DeviceReport
//     → CloudHub → devicestatus 存储 → GET /api/v1/devices 可见
//
// 运行：go test -v -timeout 20m ./tests/e2e/ -run TestDeviceE2E
import (
	"fmt"
	"testing"
	"time"
)

// deviceStatusItem 是 /api/v1/devices 的元素（云端 DeviceStatus 子集）。
type deviceStatusItem struct {
	NodeID         string             `json:"nodeID"`
	DeviceName     string             `json:"deviceName"`
	Namespace      string             `json:"namespace"`
	Properties     map[string]float64 `json:"properties"`
	Desired        map[string]float64 `json:"desired"`
	LastReportedAt int64              `json:"lastReportedAt"`
}

// deviceStatusList 是 /api/v1/devices 的响应形态。
type deviceStatusList struct {
	Items []deviceStatusItem `json:"items"`
}

// TestDeviceCommandReportE2E 验证设备指令下发 → 状态上报 → 云端可见的闭环。
func TestDeviceCommandReportE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	// 1. 启动 cloudcore + edgecore
	cloud, httpPort, hubPort := startCloudcore(t, root)
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID := "e2e-dev-1"
	startEdgecore(t, root, nodeID, hubPort)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册", nodeID)

	// 2. 下发设备指令（mock_sensor 支持 targetTemp，见 mappers/mock_sensor）
	//    云端 200 = 边缘已 Ack（指令送达并处理成功）
	const deviceName, property = "sensor-01", "targetTemp"
	const wantValue = 26.5
	url := fmt.Sprintf("%s/api/v1/nodes/%s/device-command", base, nodeID)
	postJSON(t, url, map[string]any{
		"deviceName": deviceName,
		"namespace":  "default",
		"property":   property,
		"value":      wantValue,
	})
	t.Logf("设备指令已下发：%s.%s=%v（边缘已 Ack）", deviceName, property, wantValue)

	// 3. 云端设备状态可见：Desired 含指令期望值（下发成功即写入），
	//    Properties 含实际上报值（上报循环 300ms 周期，含首次立即上报）
	deadline := time.Now().Add(60 * time.Second)
	var found *deviceStatusItem
	for time.Now().Before(deadline) {
		var list deviceStatusList
		getJSON(t, base+"/api/v1/devices", &list)
		for i := range list.Items {
			if list.Items[i].NodeID == nodeID && list.Items[i].DeviceName == deviceName {
				found = &list.Items[i]
				break
			}
		}
		if found != nil && found.Desired[property] == wantValue && len(found.Properties) > 0 {
			break
		}
		found = nil
		time.Sleep(500 * time.Millisecond)
	}
	if found == nil {
		t.Fatalf("等待设备 %s 状态上报超时（60s）：Desired=%+v Properties=%+v",
			deviceName, desiredOf(t, base, nodeID, deviceName), propsOf(t, base, nodeID, deviceName))
	}
	if got := found.Desired[property]; got != wantValue {
		t.Errorf("Desired[%s] = %v，期望 %v", property, got, wantValue)
	}
	if len(found.Properties) == 0 {
		t.Errorf("Properties 为空，期望含温度/湿度上报值")
	}
	t.Logf("设备状态已同步到云端：%s Desired=%v Properties=%v",
		deviceName, found.Desired, found.Properties)

	// 4. 单节点视角同样可见（/api/v1/nodes/{nodeID}/devices）
	var nodeList deviceStatusList
	getJSON(t, base+"/api/v1/nodes/"+nodeID+"/devices", &nodeList)
	if len(nodeList.Items) != 1 || nodeList.Items[0].DeviceName != deviceName {
		t.Errorf("单节点设备列表 = %+v，期望只含 %s", nodeList.Items, deviceName)
	}
	_ = cloud
}

// desiredOf 查询设备当前 Desired 字段（失败诊断用）。
func desiredOf(t *testing.T, base, nodeID, deviceName string) map[string]float64 {
	t.Helper()
	var list deviceStatusList
	getJSON(t, base+"/api/v1/devices", &list)
	for _, it := range list.Items {
		if it.NodeID == nodeID && it.DeviceName == deviceName {
			return it.Desired
		}
	}
	return nil
}

// propsOf 查询设备当前 Properties 字段（失败诊断用）。
func propsOf(t *testing.T, base, nodeID, deviceName string) map[string]float64 {
	t.Helper()
	var list deviceStatusList
	getJSON(t, base+"/api/v1/devices", &list)
	for _, it := range list.Items {
		if it.NodeID == nodeID && it.DeviceName == deviceName {
			return it.Properties
		}
	}
	return nil
}
