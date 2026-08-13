// edgecore 是 EdgeFlow 的边缘端组件入口。
//
// 对标 KubeEdge 的 EdgeCore：当前版本启动 EdgeHub（云边通信 WebSocket
// 客户端，WBS 3.1）并常驻运行——连接云端 CloudHub、节点注册、心跳保活、
// 断线重连；收到 SIGINT/SIGTERM 时优雅退出。未来将在此基础上接入
// MetaManager/Edged 等边缘模块。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"edgeflow/edge/pkg/edgehub"
	"edgeflow/pkg/log"
	"edgeflow/pkg/version"
)

func main() {
	// 退出信号：SIGINT/SIGTERM 触发优雅关闭
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	if code := run(os.Args[1:], os.Stdout, os.Stderr, sigCh); code != 0 {
		os.Exit(code)
	}
}

// run 是可测试入口：解析命令行 → 加载配置 → 启动 EdgeHub → 等待退出信号。
// 返回进程退出码（0 成功，1 参数错误）。
func run(args []string, stdout, stderr io.Writer, sigCh <-chan os.Signal) int {
	fs := flag.NewFlagSet("edgecore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// 命令行参数：--version 打印版本信息后退出
	showVersion := fs.Bool("version", false, "打印版本信息后退出")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	info := version.Get()
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, info.String())
		return 0
	}

	// 加载配置（环境变量 EDGEFLOW_EDGECORE_NODE_ID / EDGEFLOW_EDGECORE_CLOUD_ADDR 可覆盖默认值）
	opts := edgehub.Options{
		CloudAddr: edgehub.DefaultCloudAddrFromEnv(),
		NodeID:    edgehub.DefaultNodeID(),
	}
	client := edgehub.New(opts)

	log.Infof("EdgeHub connecting to %s as %s", client.Address(), opts.NodeID)
	client.Start()

	// 常驻：等待退出信号后优雅关闭
	sig := <-sigCh
	log.Infof("收到信号 %v，正在优雅关闭 EdgeHub...", sig)
	client.Stop()
	log.Infof("edgecore exited")
	return 0
}
