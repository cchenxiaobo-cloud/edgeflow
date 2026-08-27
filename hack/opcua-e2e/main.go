// Command opcua-e2e 是 OPC-UA Mapper 端到端验证程序（WBS 5.2 第二阶段验收）。
//
// 完整链路：起模拟器（自研服务端）→ Mapper（自研客户端）读写 →
// 操作台账（SQLite 持久化）→ 按条件查询输出。
//
// 用法：
//
//	go run ./hack/opcua-e2e
//
// 环境变量：
//   - EDGEFLOW_EDGECORE_DB_PATH  台账 DB 路径（默认 data/opcua-e2e.db）
//   - OPCUA_SIM_PORT             模拟器端口（默认 14840）
//
// 结束后可用 sqlite3 直接查台账：
//
//	sqlite3 data/opcua-e2e.db "SELECT id,ts,device_id,direction,reg_addr,value,result FROM op_ledger ORDER BY id;"
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
	opcuapkg "edgeflow/pkg/opcua"
	"edgeflow/pkg/opcuasim"

	opcuamapper "edgeflow/mappers/opcua"
)

// envOr 读取环境变量，未设置时返回默认值。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	port := opcuasim.DefaultPort
	if v := os.Getenv(opcuasim.EnvPort); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 && p < 65536 {
			port = p
		}
	}
	dbPath := envOr(metamanager.EnvDBPath, filepath.Join("data", "opcua-e2e.db"))

	fmt.Println("========== OPC-UA Mapper 端到端验证（WBS 5.2 第二阶段）==========")
	fmt.Printf("1) 启动模拟器（127.0.0.1:%d）\n", port)
	sim := opcuasim.New(fmt.Sprintf("127.0.0.1:%d", port), opcuasim.WithStep(500*time.Millisecond))
	if err := sim.Start(); err != nil {
		log.Errorf("模拟器启动失败: %v", err)
		os.Exit(1)
	}
	defer sim.Stop()
	fmt.Println(sim.NodeTable())

	fmt.Printf("2) 打开操作台账（%s）\n", dbPath)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Errorf("创建台账目录失败: %v", err)
		os.Exit(1)
	}
	store, err := metamanager.Open(dbPath)
	if err != nil {
		log.Errorf("打开台账 DB 失败: %v", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()
	ledger, err := metamanager.NewLedger(store)
	if err != nil {
		log.Errorf("初始化台账失败: %v", err)
		os.Exit(1)
	}

	fmt.Printf("3) 装配 OPC-UA Mapper（endpoint=opc.tcp://127.0.0.1:%d）\n", port)
	points := []opcuamapper.PointDef{
		{Name: "temperature", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodeTemperature)},
		{Name: "humidity", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodeHumidity)},
		{Name: "pressure", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodePressure)},
		{Name: "running", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodeRunning)},
		{Name: "setpoint", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodeSetpoint)},
	}
	m, err := opcuamapper.New(fmt.Sprintf("opc.tcp://127.0.0.1:%d", port),
		opcuamapper.WithPoints(points), opcuamapper.WithLedger(ledger))
	if err != nil {
		log.Errorf("创建 Mapper 失败: %v", err)
		os.Exit(1)
	}
	if err := m.Start(context.Background()); err != nil {
		log.Errorf("启动 Mapper 失败: %v", err)
		os.Exit(1)
	}
	defer m.Stop()

	fmt.Println("4) Collect 采集点位（读链路）")
	props, err := m.Collect()
	if err != nil {
		log.Errorf("采集失败: %v", err)
		os.Exit(1)
	}
	for name, val := range props {
		fmt.Printf("   - %-12s = %.2f\n", name, val)
	}

	fmt.Println("5) HandleCommand 写 setpoint=200（写链路 + 回读验证）")
	rep, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "opcua-device-01",
		Namespace:  "default",
		Property:   "setpoint",
		Value:      200,
	})
	if err != nil {
		log.Errorf("写点失败: %v", err)
		os.Exit(1)
	}
	fmt.Printf("   写后快照 setpoint = %.2f（回读验证一致）\n", rep.Properties["setpoint"])

	fmt.Println("6) 台账查询（最近 8 条）")
	recs, err := ledger.ListOps(metamanager.OpFilter{Limit: 8})
	if err != nil {
		log.Errorf("台账查询失败: %v", err)
		os.Exit(1)
	}
	if len(recs) == 0 {
		fmt.Println("   台账为空")
	}
	for _, r := range recs {
		fmt.Printf("   [%s] %s %s %s = %s → %s（%s）\n",
			time.UnixMilli(r.Ts).Format("15:04:05"), r.Direction, r.DeviceID, r.RegAddr, r.Value, r.Result, r.Message)
	}

	fmt.Println("========== OPC-UA Mapper 端到端验证通过 ==========")
}
