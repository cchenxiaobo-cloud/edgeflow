// Command modbus-e2e 是 Modbus Mapper 端到端验证程序（WBS 5.2 验收）。
//
// 完整链路：起模拟器（自实现协议帧）→ Mapper（goburrow 客户端）读写 →
// 操作台账（SQLite 持久化）→ 按条件查询输出。
//
// 用法：
//
//	go run ./hack/modbus-e2e
//
// 环境变量：
//   - EDGEFLOW_EDGECORE_DB_PATH  台账 DB 路径（默认 data/modbus-e2e.db）
//   - MODBUS_SIM_PORT            模拟器端口（默认 15020）
//   - EDGEFLOW_MODBUS_ADDR       Mapper 设备地址（默认 127.0.0.1:15020）
//
// 结束后可用 sqlite3 直接查台账：
//
//	sqlite3 data/modbus-e2e.db "SELECT id,ts,device_id,direction,reg_addr,value,result FROM op_ledger ORDER BY id;"
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
	"edgeflow/pkg/modbussim"

	modbusmapper "edgeflow/mappers/modbus"
)

// envOr 读取环境变量，未设置时返回默认值。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	port := modbussim.DefaultPort
	if v := os.Getenv(modbussim.EnvPort); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 && p < 65536 {
			port = p
		}
	}
	dbPath := envOr(metamanager.EnvDBPath, filepath.Join("data", "modbus-e2e.db"))
	devAddr := envOr(modbusmapper.EnvAddr, fmt.Sprintf("127.0.0.1:%d", port))

	fmt.Println("========== Modbus Mapper 端到端验证（WBS 5.2）==========")
	fmt.Printf("1) 启动模拟器（: %d）\n", port)
	sim := modbussim.New(fmt.Sprintf(":%d", port), modbussim.WithStep(500*time.Millisecond))
	if err := sim.Start(); err != nil {
		log.Errorf("模拟器启动失败: %v", err)
		os.Exit(1)
	}
	defer func() { _ = sim.Stop() }()
	fmt.Println(sim.RegisterTable())

	fmt.Printf("2) 打开台账（%s）\n", dbPath)
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
	if n, _ := ledger.CleanupOps(metamanager.LedgerRetentionDays * 24 * time.Hour); n > 0 {
		fmt.Printf("   启动清理：已删除 %d 条超过 %d 天的旧记录\n", n, metamanager.LedgerRetentionDays)
	}

	fmt.Printf("3) 启动 Mapper（addr=%s）并采集（读 0x0000-0x0001）\n", devAddr)
	m := modbusmapper.New(devAddr, modbusmapper.WithLedger(ledger), modbusmapper.WithTimeout(5*time.Second))
	if err := m.Start(context.Background()); err != nil {
		log.Errorf("Mapper 启动失败: %v", err)
		os.Exit(1)
	}
	defer func() { _ = m.Stop() }()

	props, err := m.Collect()
	if err != nil {
		log.Errorf("Collect 失败: %v", err)
		os.Exit(1)
	}
	fmt.Printf("   Collect 结果: temperature=%.1f°C humidity=%.1f%%\n",
		props["temperature"], props["humidity"])

	fmt.Println("4) 下发指令 targetTemp=25.5（写 0x0010）")
	report, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "mb-sensor-01", Namespace: "default",
		Property: "targetTemp", Value: 25.5,
	})
	if err != nil {
		log.Errorf("写目标温度失败: %v", err)
		os.Exit(1)
	}
	fmt.Printf("   指令执行后快照: temperature=%.1f°C humidity=%.1f%%\n",
		report.Properties["temperature"], report.Properties["humidity"])

	fmt.Println("5) 下发指令 coil0=1 / coil3=1（写线圈 0x0020、0x0023）")
	for _, c := range []mapper.DeviceCommand{
		{DeviceName: "mb-sensor-01", Property: "coil0", Value: 1},
		{DeviceName: "mb-sensor-01", Property: "coil3", Value: 1},
	} {
		if _, err := m.HandleCommand(c); err != nil {
			log.Errorf("写 %s 失败: %v", c.Property, err)
			os.Exit(1)
		}
	}
	fmt.Println("   coil0/coil3 写入成功（已回读验证）")

	fmt.Println("6) 等待温度向目标收敛（2s）后再次采集")
	time.Sleep(2 * time.Second)
	props, err = m.Collect()
	if err != nil {
		log.Errorf("Collect 失败: %v", err)
		os.Exit(1)
	}
	fmt.Printf("   Collect 结果: temperature=%.1f°C（向 25.5°C 收敛中） humidity=%.1f%%\n",
		props["temperature"], props["humidity"])

	fmt.Println("7) 台账查询")
	all, err := ledger.ListOps(metamanager.OpFilter{})
	if err != nil {
		log.Errorf("ListOps 失败: %v", err)
		os.Exit(1)
	}
	fmt.Printf("   全量 %d 条（时间倒序）:\n", len(all))
	for _, op := range all {
		fmt.Printf("     [%s] %s %-14s addr=%-12s value=%-6s result=%-5s %s\n",
			time.UnixMilli(op.Ts).Format("15:04:05.000"),
			op.Direction, op.DeviceID, op.RegAddr, op.Value, op.Result, op.Message)
	}
	downs, err := ledger.ListOps(metamanager.OpFilter{Direction: metamanager.DirDown})
	if err != nil {
		log.Errorf("ListOps(down) 失败: %v", err)
		os.Exit(1)
	}
	fmt.Printf("   条件查询（direction=down）%d 条:\n", len(downs))
	for _, op := range downs {
		fmt.Printf("     [%s] %s addr=%-12s value=%-6s result=%-5s %s\n",
			time.UnixMilli(op.Ts).Format("15:04:05.000"),
			op.Direction, op.RegAddr, op.Value, op.Result, op.Message)
	}

	fmt.Println("========== 端到端验证通过 ==========")
	fmt.Printf("台账持久化于 %s，可用 sqlite3 直接查询：\n", dbPath)
	fmt.Printf("  sqlite3 %q \"SELECT id,ts,device_id,direction,reg_addr,value,result,message FROM op_ledger ORDER BY id;\"\n", dbPath)
}
