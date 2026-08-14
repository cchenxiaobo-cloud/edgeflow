// edgecore 是 EdgeFlow 的边缘端组件入口。
//
// 对标 KubeEdge 的 EdgeCore：当前版本启动 EdgeHub（云边通信 WebSocket
// 客户端，WBS 3.1）与 MetaManager（SQLite 元数据存储，WBS 3.3）并常驻运行
// ——连接云端 CloudHub、节点注册、心跳保活、断线重连，注册成功后把节点
// 元数据持久化到本地 SQLite（重启后数据仍在）；收到 SIGINT/SIGTERM 时优雅退出。
// 未来将在此基础上接入 Edged 等边缘模块。
package main

import (
	"context"
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

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/edge/pkg/edged"
	"edgeflow/edge/pkg/edgehub"
	"edgeflow/edge/pkg/eventbus"
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

	// 启动 EventBus（WBS 3.6：边缘设备 MQTT 数据面接入点）。
	// 地址默认 tcp://127.0.0.1:1883，环境变量 EDGEFLOW_EDGECORE_MQTT_ADDR 覆盖。
	// Connect 失败（超时/拒绝）→ 告警并降级：Mapper 保持纯本地模式继续运行，
	// 云边设备链路（DeviceCommand/DeviceReport）不受影响，edgecore 不退出。
	// 决策依据：MQTT 数据面是设备接入的可选增强，管理面可用即可交付；
	// 注意：paho 在 Connect 超时后仍会在后台重试（broker 可能晚于 edgecore
	// 启动），但 Mapper 不会自动切换模式——降级是装配期决策，
	// 重启 edgecore（broker 就绪后）即可恢复 MQTT 数据面。
	bus := eventbus.New(eventbus.DefaultBrokerAddrFromEnv())
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), mqttConnectTimeout())
	connectErr := bus.Connect(connectCtx)
	cancelConnect()
	if connectErr != nil {
		log.Warnf("EventBus 连接失败: %v", connectErr)
		log.Warnf("Mapper 降级为纯本地模式：设备数据不走 MQTT 数据面（云边设备链路不受影响，edgecore 继续运行）")
		bus = nil
	} else {
		log.Infof("EventBus 已连接（%s），Mapper 启用 MQTT 数据面", bus.BrokerAddr())
	}

	// 设备链路（WBS 3.5/5.3）：设备影子存储 + DeviceCommand 下发处理。
	// 设备指令执行器由 Mapper 框架（edge/pkg/mapper，WBS 5.1）提供：
	// 装配层把 Mapper 注册表适配成 devicetwin.DeviceCommandExecutor
	// 注入（见 device_mapper.go），指令按 deviceName 路由到具体 Mapper 执行。
	twinStore := devicetwin.NewStore()
	mapperReg := buildMapperRegistry(bus)
	deviceExec := &mapperCommandExecutor{reg: mapperReg, twins: twinStore}
	// Mapper 生命周期随 edgecore 启停：启动采集循环（内置模拟传感器每 2s
	// 波动一次，由上报循环周期汇入影子）；启动失败只告警，不阻断主流程。
	mapperCtx, mapperCancel := context.WithCancel(context.Background())
	if err := mapperReg.StartAll(mapperCtx); err != nil {
		log.Warnf("Mapper 启动部分失败: %v", err)
	}

	// 消息处理回调（WBS 4.6）：云端下发类消息（PodSync/DeviceCommand 等）→
	// MetaManager 落盘 / 设备影子更新；处理结果由 EdgeHub 自动回 Ack
	// （成功 code=ok / 失败 code=error）。
	client.SetMessageHandlerFunc(func(msg *protocol.Message) error {
		switch msg.Type {
		case protocol.TypePodSync:
			return handlePodSync(store, msg)
		case protocol.TypeConfigSync:
			return handleConfigSync(store, msg)
		case protocol.TypeDeviceCommand:
			return handleDeviceCommand(twinStore, deviceExec, msg)
		default:
			// 未知下发类型：暂不处理但回 ok，
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

	// 启动 Edged（WBS 3.2 方案 A POC）：声明式调谐循环 + Docker 容器运行时。
	// 期望状态来自 MetaManager 的 Pod 元数据；每 5s 轮询 + 增量订阅触发。
	// 调谐周期可用环境变量 EDGEFLOW_EDGECORE_RECONCILE_INTERVAL 覆盖（默认 5s）
	reconcileInterval := durationFromEnv("EDGEFLOW_EDGECORE_RECONCILE_INTERVAL", 5*time.Second)
	edgedSvc := edged.New(store, edged.NewDockerRuntime(), reconcileInterval)
	edgedSvc.Start()
	log.Infof("Edged started（方案 A POC：DockerRuntime + %s 调谐周期）", reconcileInterval)

	// MetaManager 增量订阅：Pod 变更（upsert/delete）→ 触发 Edged 立即调谐。
	// 背压策略：订阅缓冲满时丢弃事件（reconcile 是声明式的，下一轮轮询会收敛）。
	subID, eventCh, err := store.Subscribe(metamanager.SubscribeOptions{})
	if err != nil {
		log.Warnf("MetaManager 订阅失败（Edged 仍按轮询调谐）: %v", err)
	} else {
		go func() {
			for ev := range eventCh {
				if ev.Type == metamanager.EventPodUpsert || ev.Type == metamanager.EventPodDelete {
					edgedSvc.Trigger()
				}
			}
		}()
		log.Infof("MetaManager 增量订阅已启用（subID=%d），Pod 变更将即时触发 Edged 调谐", subID)
	}

	// Pod 状态上报循环（WBS 6.3）：周期读取 Edged 状态表并上报云端。
	// 与调谐解耦：固定周期（默认 30s，环境变量 EDGEFLOW_EDGECORE_REPORT_INTERVAL
	// 可覆盖），启动即上报一轮。退出机制与主流程一致：独立 stopCh，收到退出
	// 信号后先停上报再停 Edged/EdgeHub（上报循环是 EdgeHub 通道的消费者）。
	reportInterval := durationFromEnv("EDGEFLOW_EDGECORE_REPORT_INTERVAL", defaultReportInterval)
	reportStopCh := make(chan struct{})
	reportDone := make(chan struct{})
	go func() {
		defer close(reportDone)
		runStatusReportLoop(client, edgedSvc, opts.NodeID, reportInterval, reportStopCh)
	}()
	log.Infof("Pod 状态上报循环已启动（周期 %s，nodeID=%s）", reportInterval, opts.NodeID)

	// 设备数据上报循环（WBS 5.3 边缘侧）：周期从 Twin 快照生成
	// DeviceReport 消息上报云端。与 Pod 上报循环同构（独立 stopCh、
	// 启动即上报一轮）；周期默认 30s，环境变量
	// EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL 可覆盖（复用 durationFromEnv
	// 的上下限校验）。
	deviceReportInterval := durationFromEnv(deviceReportIntervalEnv, defaultReportInterval)
	deviceReportStopCh := make(chan struct{})
	deviceReportDone := make(chan struct{})
	go func() {
		defer close(deviceReportDone)
		runDeviceReportLoop(client, mapperReg, twinStore, opts.NodeID, deviceReportInterval, deviceReportStopCh)
	}()
	log.Infof("设备上报循环已启动（周期 %s，nodeID=%s）", deviceReportInterval, opts.NodeID)

	// 常驻：等待退出信号后优雅关闭（先停 EdgeHub，再关 Store，
	// 保证回调不再触发后才会关闭数据库连接）
	sig := <-sigCh
	log.Infof("收到信号 %v，正在优雅关闭 EdgeHub...", sig)
	close(reportStopCh)
	<-reportDone // Pod 上报循环退出后不再有新消息写入通道
	close(deviceReportStopCh)
	<-deviceReportDone // 设备上报循环退出后不再有新消息写入通道
	mapperCancel()
	if err := mapperReg.StopAll(); err != nil {
		log.Warnf("Mapper 停止部分失败: %v", err)
	}
	// 优雅退出顺序：先停 Mapper 再断 EventBus——保证断线窗口内没有
	// 采集循环再向总线发布/订阅（避免发布失败告警刷屏与幽灵订阅）。
	if bus != nil {
		bus.Disconnect()
	}
	edgedSvc.Stop()
	client.Stop()
	if subID != 0 {
		store.Unsubscribe(subID)
	}
	log.Infof("edgecore exited")
	return 0
}

// mqttConnectTimeout 是 EventBus 建连等待上限：默认与 eventbus 一致（5s），
// 可用环境变量 EDGEFLOW_EDGECORE_MQTT_CONNECT_TIMEOUT 覆盖
// （复用 durationFromEnv 的 1s~10min 上下限校验）。
// 设小可让"broker 缺席"的部署快速进入降级路径（测试/CI 常用）。
func mqttConnectTimeout() time.Duration {
	return durationFromEnv("EDGEFLOW_EDGECORE_MQTT_CONNECT_TIMEOUT", eventbus.DefaultConnectTimeout)
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
//   - delete → 从 pod 对象提取 namespace/name 后 MetaManager.DeletePod；
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
		// delete 时 pod 对象通常只携带 name/namespace；提取后按命名空间+名称删除
		var pod struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}
		if err := json.Unmarshal([]byte(podJSON), &pod); err != nil {
			return fmt.Errorf("解析 delete 操作的 Pod 信息失败: %w", err)
		}
		if pod.Name == "" {
			return errors.New("delete 操作的 Pod 缺少 name 字段")
		}
		if err := store.DeletePod(pod.Namespace, pod.Name); err != nil {
			return fmt.Errorf("删除 Pod 元数据失败: %w", err)
		}
		log.Infof("MetaManager 已删除 Pod 元数据（namespace=%s, pod=%s）", pod.Namespace, pod.Name)
		return nil
	default:
		return fmt.Errorf("未知的 PodSync operation: %q", payload.Operation)
	}
}
