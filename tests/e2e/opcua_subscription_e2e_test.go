package e2e

// OPC-UA 订阅模式端到端测试（WBS 5.2 第三阶段验收，v0.15.0）：
//   自研 OPC-UA 模拟器 → edgecore OPC-UA Mapper（EDGEFLOW_OPCUA_SUBSCRIPTION=on
//   订阅采集模式）→ 云端 /api/v1/devices 可见点位属性 → 下发 device-command
//   写 setpoint → Desired 出现 → Properties 经订阅推送收敛。
//
// 与 TestOPCUADeviceE2E 的差异：Mapper 不再轮询批量 Read，而是经 OPC-UA
// Subscription 接收服务端数据变更推送，Collect() 返回推送缓存快照。
//
// 运行：go test -v -timeout 20m ./tests/e2e/ -run TestOPCUASubscriptionE2E

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"edgeflow/pkg/opcuasim"
)

// TestOPCUASubscriptionE2E 验证订阅模式下设备采集与指令写点的云边闭环。
func TestOPCUASubscriptionE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	// 1. 测试进程内起 OPC-UA 模拟器（空闲端口，快步进便于快速推送）
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithStep(100*time.Millisecond), opcuasim.WithSeed(7))
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

	// 3. 启动 edgecore（OPC-UA Mapper 装配 env + 订阅模式开关）
	nodeID := "e2e-opcua-sub-1"
	dbPath := filepath.Join(t.TempDir(), "edgecore.db")
	cloudAddr := "ws://127.0.0.1:" + strconv.Itoa(hubPort)
	env := append(edgeEnv(nodeID, cloudAddr, dbPath),
		"EDGEFLOW_OPCUA_ENDPOINT="+endpoint,
		"EDGEFLOW_OPCUA_NODES=temperature=ns=2;i=1001,humidity=ns=2;i=1002,setpoint=ns=2;i=3001",
		"EDGEFLOW_OPCUA_SUBSCRIPTION=on",
	)
	startProcess(t, "edgecore-opcua-sub", filepath.Join(binDir, "edgecore"), nil, env)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册", nodeID)

	// 4. 等待 opcua-device-01 经订阅推送在云端可见且含全部点位
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
		if found != nil && len(found.Properties) >= 3 {
			break
		}
		found = nil
		time.Sleep(500 * time.Millisecond)
	}
	if found == nil {
		t.Fatalf("等待 OPC-UA 设备 %s 上报超时（90s）：当前设备列表 %v",
			deviceName, listDeviceNames(t, base))
	}
	t.Logf("OPC-UA 设备已同步到云端（订阅模式）：%v", found.Properties)

	// 5. 下发 device-command 写 setpoint=200
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

	// 6. 等待 Desired 含指令值 + Properties 收敛到 200（订阅推送链路）
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
	t.Logf("OPC-UA 订阅模式指令闭环完成：Desired=%v Properties=%v", final.Desired, final.Properties)
}
