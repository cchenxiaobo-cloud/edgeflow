// Command modbus-sim 是高保真 Modbus TCP 模拟器入口（WBS 5.2）。
//
// 用途：在无真实 Modbus 设备时，验证 Modbus Mapper 的读写链路与操作台账。
// 协议实现见 pkg/modbussim（自实现 MBAP + PDU，不引第三方 Modbus 库）。
//
// 用法：
//
//	go run ./hack/modbus-sim            # 默认监听 :15020
//	MODBUS_SIM_PORT=15021 go run ./hack/modbus-sim
//
// 启动后打印寄存器表说明；Ctrl+C 退出。
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"edgeflow/pkg/log"
	"edgeflow/pkg/modbussim"
)

func main() {
	port := modbussim.DefaultPort
	if v := os.Getenv(modbussim.EnvPort); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			log.Errorf("环境变量 %s=%q 非法，使用默认端口 %d", modbussim.EnvPort, v, modbussim.DefaultPort)
		} else {
			port = p
		}
	}
	addr := fmt.Sprintf(":%d", port)

	sim := modbussim.New(addr)
	if err := sim.Start(); err != nil {
		log.Errorf("Modbus 模拟器启动失败: %v", err)
		os.Exit(1)
	}
	log.Infof("Modbus TCP 模拟器已启动：%s（unit ID 1）", sim.Addr())
	fmt.Println(sim.RegisterTable())
	log.Infof("按 Ctrl+C 退出")

	// 等待 SIGINT/SIGTERM 优雅退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = sim.Stop()
	log.Infof("Modbus 模拟器已停止")
}
