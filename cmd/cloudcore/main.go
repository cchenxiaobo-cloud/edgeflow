// cloudcore 是 EdgeFlow 的云端组件入口。
//
// 对标 KubeEdge 的 CloudCore：未来将在这里承载云边通信
// （WebSocket）、消息路由（NATS）、设备管理（CRD）等云端逻辑。
// 当前版本仅打印版本信息和启动日志后退出，业务逻辑由后续任务实现。
package main

import "fmt"

// 版本信息，后续可通过 -ldflags 在编译时注入。
const version = "v0.1.0"

func main() {
	fmt.Printf("cloudcore starting, version=%s\n", version)
	fmt.Println("cloudcore exited (skeleton build, business logic coming soon)")
}
