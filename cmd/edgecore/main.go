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
	"errors"
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
	"edgeflow/pkg/protocol"
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

	// 消息处理回调（WBS 4.6）：云端下发类消息（PodSync 等）→ MetaManager
	// 落盘；处理结果由 EdgeHub 自动回 Ack（成功 code=ok / 失败 code=error）。
	client.SetMessageHandlerFunc(func(msg *protocol.Message) error {
		switch msg.Type {
		case protocol.TypePodSync:
			return handlePodSync(store, msg)
		default:
			// 未知下发类型（ConfigSync 等 M2 消息）：暂不处理但回 ok，
			// 避免云端视为失败无限重试；后续模块接入时在此扩展
			log.Warnf("EdgeHub 收到未注册处理器的消息类型 %s，忽略", msg.Type)
			return nil
		}
	})

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

// PodSyncPayload 是 PodSync 消息的负载（与云端契约一致，字段不可改）：
// operation 取 add/update/delete，pod 是 Pod 的 JSON 对象（原样交给
// MetaManager 落盘，不做字段裁剪）。
type PodSyncPayload struct {
	Operation string          `json:"operation"` // add / update / delete
	Pod       json.RawMessage `json:"pod"`       // Pod 的 JSON 表示
}

// handlePodSync 处理一条 PodSync 下发消息：
//   - add/update → MetaManager.SavePod（Pod JSON 原样落盘）；
//   - delete → 从 pod 对象提取 name 后 MetaManager.DeletePod；
//   - 解析/存储失败返回 error，EdgeHub 自动回 Ack code=error。
func handlePodSync(store *metamanager.Store, msg *protocol.Message) error {
	var payload PodSyncPayload
	if err := msg.DecodePayload(&payload); err != nil {
		return fmt.Errorf("解析 PodSync 负载失败: %w", err)
	}
	podJSON := string(payload.Pod)
	switch payload.Operation {
	case "add", "update":
		if err := store.SavePod(podJSON); err != nil {
			return fmt.Errorf("保存 Pod 元数据失败: %w", err)
		}
		log.Infof("MetaManager 已保存 Pod 元数据（operation=%s, pod=%s）",
			payload.Operation, podJSON)
		return nil
	case "delete":
		// delete 时 pod 对象通常只携带 name；提取后按名删除
		var pod struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(podJSON), &pod); err != nil {
			return fmt.Errorf("解析 delete 操作的 Pod 信息失败: %w", err)
		}
		if pod.Name == "" {
			return errors.New("delete 操作的 Pod 缺少 name 字段")
		}
		if err := store.DeletePod(pod.Name); err != nil {
			return fmt.Errorf("删除 Pod 元数据失败: %w", err)
		}
		log.Infof("MetaManager 已删除 Pod 元数据（pod=%s）", pod.Name)
		return nil
	default:
		return fmt.Errorf("未知的 PodSync operation: %q", payload.Operation)
	}
}
