package e2e

// OPC-UA 设备链路端到端测试（WBS 5.2 第二阶段验收，v0.14.0）：
//   自研 OPC-UA 模拟器 → edgecore OPC-UA Mapper 采集上报 → 云端
//   /api/v1/devices 可见 opcua-device-01 属性 → 下发 device-command
//   写 setpoint → Desired 出现 → Properties 收敛。
//
// 链路（真实装配路径，无需 Docker）：
//   测试进程内起 pkg/opcuasim（127.0.0.1:0）→ edgecore 装配
//   EDGEFLOW_OPCUA_ENDPOINT/NODES → collectMapperReports 采集 →
//   DeviceReport 上报 → devicestatus → GET /api/v1/devices 可见 →
//   POST device-command（setpoint=200）→ Mapper HandleCommand 写点 →
//   回读验证 → 影子更新 → 周期上报 Properties 收敛到 200。
//
// 运行：go test -v -timeout 20m ./tests/e2e/ -run TestOPCUADeviceE2E
import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"edgeflow/pkg/opcuasim"
)

// TestOPCUADeviceE2E 验证 OPC-UA 设备从模拟器到云端属性的完整闭环。
func TestOPCUADeviceE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	// 1. 测试进程内起 OPC-UA 模拟器（空闲端口）
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithStep(100*time.Millisecond), opcuasim.WithSeed(1))
	if err := sim.Start(); err != nil {
		t.Fatalf("OPC-UA 模拟器启动失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	endpoint := "opc.tcp://" + sim.Addr()
	t.Logf("OPC-UA 模拟器就绪（%s）", endpoint)

	// 2. 启动 cloudcore
	cloud, httpPort, hubPort := startCloudcore(t, root)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	// 3. 启动 edgecore（追加 OPC-UA Mapper 装配 env）
	nodeID := "e2e-opcua-1"
	dbPath := filepath.Join(t.TempDir(), "edgecore.db")
	cloudAddr := "ws://127.0.0.1:" + strconv.Itoa(hubPort)
	env := append(edgeEnv(nodeID, cloudAddr, dbPath),
		"EDGEFLOW_OPCUA_ENDPOINT="+endpoint,
		"EDGEFLOW_OPCUA_NODES=temperature=ns=2;i=1001,humidity=ns=2;i=1002,pressure=ns=2;i=1003,setpoint=ns=2;i=3001",
	)
	startProcess(t, "edgecore-opcua", filepath.Join(binDir, "edgecore"), nil, env)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册", nodeID)

	// 4. 等待 opcua-device-01 在 /api/v1/devices 可见且 Properties 含配置点位
	const deviceName = "opcua-device-01"
	deadline := time.Now().Add(90 * time.Second)
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
		if found != nil && len(found.Properties) >= 4 {
			break
		}
		found = nil
		time.Sleep(500 * time.Millisecond)
	}
	if found == nil {
		t.Fatalf("等待 OPC-UA 设备 %s 上报超时（90s）：当前设备列表 %v",
			deviceName, listDeviceNames(t, base))
	}
	t.Logf("OPC-UA 设备已同步到云端：%v", found.Properties)
	for _, p := range []string{"temperature", "humidity", "pressure", "setpoint"} {
		if _, ok := found.Properties[p]; !ok {
			t.Errorf("Properties 缺少点位 %s: %v", p, found.Properties)
		}
	}

	// 5. 下发 device-command 写 setpoint=200（边缘 Ack）
	const property = "setpoint"
	const wantValue = 200.0
	url := fmt.Sprintf("%s/api/v1/nodes/%s/device-command", base, nodeID)
	postJSON(t, url, map[string]any{
		"deviceName": deviceName,
		"namespace":  "default",
		"property":   property,
		"value":      wantValue,
	})
	t.Logf("设备指令已下发：%s.%s=%v（边缘已 Ack）", deviceName, property, wantValue)

	// 6. 等待 Desired 含指令值 + Properties 收敛到 200
	deadline = time.Now().Add(90 * time.Second)
	var final *deviceStatusItem
	for time.Now().Before(deadline) {
		var list deviceStatusList
		getJSON(t, base+"/api/v1/devices", &list)
		for i := range list.Items {
			if list.Items[i].NodeID == nodeID && list.Items[i].DeviceName == deviceName {
				final = &list.Items[i]
				break
			}
		}
		if final != nil && final.Desired[property] == wantValue && final.Properties[property] == wantValue {
			break
		}
		final = nil
		time.Sleep(500 * time.Millisecond)
	}
	if final == nil {
		t.Fatalf("等待指令生效超时（90s）：Desired=%v Properties=%v",
			desiredOf(t, base, nodeID, deviceName), propsOf(t, base, nodeID, deviceName))
	}
	if got := final.Desired[property]; got != wantValue {
		t.Errorf("Desired[%s] = %v，期望 %v", property, got, wantValue)
	}
	if got := final.Properties[property]; got != wantValue {
		t.Errorf("Properties[%s] = %v，期望 %v（回读验证应收敛）", property, got, wantValue)
	}
	// 模拟器侧确认 setpoint 已写入
	if sim.Setpoint() != wantValue {
		t.Errorf("模拟器 setpoint = %v，期望 %v", sim.Setpoint(), wantValue)
	}
	t.Logf("OPC-UA 指令闭环完成：Desired=%v Properties=%v", final.Desired, final.Properties)
}

// listDeviceNames 返回当前云端设备名列表（失败诊断用）。
func listDeviceNames(t *testing.T, base string) []string {
	t.Helper()
	var list deviceStatusList
	getJSON(t, base+"/api/v1/devices", &list)
	names := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		names = append(names, it.NodeID+"/"+it.DeviceName)
	}
	return names
}
