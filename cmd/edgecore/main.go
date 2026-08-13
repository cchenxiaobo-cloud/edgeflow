// edgecore 是 EdgeFlow 的边缘端组件入口。
//
// 对标 KubeEdge 的 EdgeCore：当前版本启动 EdgeHub（云边通信 WebSocket
// 客户端，WBS 3.1）与 MetaManager（SQLite 元数据存储，WBS 3.3）并常驻运行
// ——连接云端 CloudHub、节点注册、心跳保活、断线重连，注册成功后把节点
// 元数据持久化到本地 SQLite（重启后数据仍在）；收到 SIGINT/SIGTERM 时优雅退出。
// 未来将在此基础上接入 Edged 等边缘模块。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"edgeflow/edge/pkg/edgehub"
	"edgeflow/edge/pkg/metamanager"
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

	// 启动 MetaManager：打开 SQLite 元数据存储（目录不存在自动创建，
	// 路径可用环境变量 EDGEFLOW_EDGECORE_DB_PATH 覆盖）。
	// 打开失败视为致命错误：元数据持久化是 M1 验收项，缺失时不应继续。
	store, err := metamanager.Open(metamanager.DefaultDBPathFromEnv())
	if err != nil {
		log.Errorf("MetaManager 打开失败: %v", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	log.Infof("MetaManager opened: %s", store.Path())

	// 启动日志：展示已持久化的节点元数据条数——重启后数据仍在的直观证明
	if infos, err := store.ListNodes(); err != nil {
		log.Warnf("MetaManager 读取节点元数据失败: %v", err)
	} else if len(infos) > 0 {
		log.Infof("MetaManager 已加载 %d 条节点元数据（上次运行保留）", len(infos))
	} else {
		log.Infof("MetaManager 已加载 0 条节点元数据（首次运行）")
	}

	client := edgehub.New(opts)

	// 连接状态回调：注册成功（收到 RegisterAck）后把节点信息落盘。
	// 回调在 EdgeHub 内部 goroutine 中触发，且此时 NodeName 已赋值，
	// 直接读取并写入 SQLite（单条写，耗时微秒级，不会阻塞主循环）。
	client.SetStatusHandler(func(connected bool) {
		if !connected {
			return // 断线/重连中不更新，保持最后一次成功注册的记录
		}
		info := metamanager.NodeInfo{
			NodeID:          opts.NodeID,
			NodeName:        client.NodeName(),
			CloudAddr:       client.Address(),
			Arch:            runtime.GOARCH,
			OS:              runtime.GOOS,
			EdgeCoreVersion: version.Get().String(),
			RegisteredAt:    time.Now().UnixMilli(),
		}
		data, err := json.Marshal(info)
		if err != nil {
			log.Errorf("MetaManager 序列化节点信息失败: %v", err)
			return
		}
		if err := store.SaveNodeInfo(opts.NodeID, string(data)); err != nil {
			log.Errorf("MetaManager 保存节点信息失败: %v", err)
			return
		}
		log.Infof("MetaManager 已保存节点注册信息（nodeID=%s, nodeName=%s）",
			info.NodeID, info.NodeName)
	})

	log.Infof("EdgeHub connecting to %s as %s", client.Address(), opts.NodeID)
	client.Start()

	// 常驻：等待退出信号后优雅关闭（先停 EdgeHub，再关 Store，
	// 保证回调不再触发后才会关闭数据库连接）
	sig := <-sigCh
	log.Infof("收到信号 %v，正在优雅关闭 EdgeHub...", sig)
	client.Stop()
	log.Infof("edgecore exited")
	return 0
}
