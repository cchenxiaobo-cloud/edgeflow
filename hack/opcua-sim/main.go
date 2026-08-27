// Command opcua-sim 是 OPC-UA 模拟服务器入口（WBS 5.2 第二阶段验收）。
//
// 启动自研 OPC-UA 模拟器（SecurityPolicy None，匿名会话）并打印点位表，
// 供 edgecore（EDGEFLOW_OPCUA_ENDPOINT 指向本模拟器）或真实设备联调。
//
// 用法：
//
//	go run ./hack/opcua-sim
//
// 环境变量：
//   - OPCUA_SIM_PORT  模拟器端口（默认 14840）
//
// 安全：默认只绑 127.0.0.1，明文无认证，仅限本机/可信隔离网络。
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"edgeflow/pkg/opcuasim"
)

func main() {
	port := opcuasim.DefaultPort
	if v := os.Getenv(opcuasim.EnvPort); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 && p < 65536 {
			port = p
		}
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	sim := opcuasim.New(addr)
	if err := sim.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "模拟器启动失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("========== OPC-UA 模拟器已启动（%s）==========\n", addr)
	fmt.Println(sim.NodeTable())
	fmt.Printf("\n端点：%s\n", sim.EndpointURL())
	fmt.Println("按 Ctrl+C 停止")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = sim.Stop()
	_ = time.Now() // 保持 time 导入（生命周期由信号驱动）
	fmt.Println("\n模拟器已停止")
}
