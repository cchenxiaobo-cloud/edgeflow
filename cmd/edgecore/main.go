// edgecore 是 EdgeFlow 的边缘端组件入口。
//
// 对标 KubeEdge 的 EdgeCore：未来将在这里实现边缘侧与云端的
// WebSocket 通信、消息收发、设备数据上报等逻辑。
// 当前版本仅打印版本信息后退出，业务逻辑由后续任务实现。
package main

import (
	"flag"
	"fmt"

	"edgeflow/pkg/version"
)

func main() {
	// 命令行参数：--version 打印版本信息后退出
	showVersion := flag.Bool("version", false, "打印版本信息后退出")
	flag.Parse()

	info := version.Get()
	if *showVersion {
		fmt.Println(info.String())
		return
	}
	fmt.Printf("edgecore starting, %s\n", info.String())
	fmt.Println("edgecore exited (skeleton build, business logic coming soon)")
}
