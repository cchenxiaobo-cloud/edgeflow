// edgecore 是 EdgeFlow 的边缘端组件入口。
//
// 对标 KubeEdge 的 EdgeCore：未来将在这里实现边缘侧与云端的
// WebSocket 通信、消息收发、设备数据上报等逻辑。
// 当前版本仅打印版本信息和启动日志后退出，业务逻辑由后续任务实现。
package main

import "fmt"

// 版本信息，后续可通过 -ldflags 在编译时注入。
const version = "v0.1.0"

func main() {
	fmt.Printf("edgecore starting, version=%s\n", version)
	fmt.Println("edgecore exited (skeleton build, business logic coming soon)")
}
