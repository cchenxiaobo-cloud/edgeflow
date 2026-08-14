// cloudcore 是 EdgeFlow 的云端组件入口。
//
// 对标 KubeEdge 的 CloudCore：未来将在这里承载云边通信
// （WebSocket）、消息路由（NATS）、设备管理（CRD）等云端逻辑。
// 当前版本提供：加载配置、启动 HTTP 服务（/healthz 健康检查）、
// 启动 CloudHub（云边 WebSocket 通道，默认端口 10000）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	v1alpha1 "edgeflow/apis/edge/v1alpha1"
	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/podstatus"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/config"
	"edgeflow/pkg/httpx"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/version"
)

func main() {
	// run 返回进程退出码：非 0 表示启动/运行失败
	if code := run(os.Args[1:], os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// run 是 main 的可测试入口：解析命令行 → 加载配置 → 启动服务，
// 返回进程退出码（0 成功，1 失败）。
func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		// 写入 stderr 失败（如管道关闭）时无需额外处理，忽略即可
		_, _ = fmt.Fprintf(stderr, "参数解析失败: %v\n", err)
		return 1
	}

	// --version 打印版本信息后退出
	if opts.version {
		// 写入 stdout 失败时无需额外处理，忽略即可
		_, _ = fmt.Fprintln(stdout, version.Get().String())
		return 0
	}

	// 加载配置：--port > 环境变量 EDGEFLOW_CLOUDCORE_PORT > 配置文件 > 默认值
	cfg, err := config.Load(opts.config, opts.port, opts.portSet)
	if err != nil {
		log.Errorf("配置加载失败: %v", err)
		return 1
	}

	// 打印启动信息与生效配置（含端口来源，便于排查）
	info := version.Get()
	log.Infof("cloudcore starting, %s", info.String())
	log.Infof("生效配置: 端口 %d（来源: %s）", cfg.Port, cfg.PortSource)

	// 解析 CloudHub 端口：环境变量 EDGEFLOW_CLOUDCORE_HUB_PORT > 默认 10000
	hubPort, err := cloudhub.PortFromEnv()
	if err != nil {
		log.Errorf("CloudHub 端口配置无效: %v", err)
		return 1
	}
	hub := cloudhub.New(fmt.Sprintf(":%d", hubPort))

	// 节点注册表（内存态）与 CloudHub 事件桥接：
	// 节点注册/心跳/断开时实时维护节点元数据，供查询 API 使用。
	// 依赖注入（SetNodeEvents），CloudHub 不感知注册表实现。
	nodeReg := registry.New()
	hub.SetNodeEvents(registry.NewCloudHubAdapter(nodeReg))

	// Pod 状态存储（内存态，WBS 6.3）与 CloudHub 上报回调桥接：
	// 边侧上报的 PodStatus 消息经 CloudHub 校验后注入存储，供查询 API 使用。
	// 依赖注入（SetPodStatusHandler），CloudHub 不感知存储实现。
	podStore := podstatus.NewStore()
	hub.SetPodStatusHandler(func(nodeID string, ps cloudhub.PodStatusPayload) {
		// 回调运行在 CloudHub 读循环 goroutine 内：recover 兜底，
		// 防止单条异常数据导致整个连接处理崩溃（M2B 审查 P2-4）。
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("PodStatus handler panic（nodeID=%s）: %v", nodeID, r)
			}
		}()
		// phase 白名单校验（M2B 审查 P2-3）：未知阶段视为脏数据，丢弃并告警
		switch ps.Phase {
		case "Running", "Stopped", "Absent", "Error", "Unknown":
		default:
			log.Warnf("收到未知 PodStatus phase %q（node=%s pod=%s），丢弃", ps.Phase, nodeID, ps.PodName)
			return
		}
		// Absent = 边缘确认 Pod 已从期望集合删除：清除云端记录，
		// 列表不再显示已删除 Pod（与 K8s 语义一致；P1 修复配套）。
		if ps.Phase == "Absent" {
			podStore.Delete(nodeID, ps.Namespace, ps.PodName)
			return
		}
		podStore.Upsert(nodeID, podstatus.PodStatus{
			NodeID:          ps.NodeID,
			PodName:         ps.PodName,
			Namespace:       ps.Namespace,
			Phase:           ps.Phase,
			Message:         ps.Message,
			LastReconcileAt: ps.LastReconcileAt,
		})
	})

	// 注册路由：/healthz 健康检查 + 节点查询 API（两个视角并存）：
	//   /api/v1/nodes      → NodeInfo 视图（运行视角：CPU/内存/IP 等）
	//   /api/v1/edgenodes  → EdgeNode 视图（CRD 对象视角，对标 K8s Node）
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", httpx.Healthz())
	api := &nodeAPI{
		reg:          nodeReg,
		pods:         podStore,
		reliableSend: hub.ReliableSendContext,
	}
	mux.HandleFunc("GET /api/v1/nodes", api.listNodes)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}", api.getNode)
	mux.HandleFunc("GET /api/v1/edgenodes", api.listEdgeNodes)
	mux.HandleFunc("GET /api/v1/edgenodes/{nodeID}", api.getEdgeNode)
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/podsync", api.syncPod)
	mux.HandleFunc("GET /api/v1/pods", api.listPods)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}/pods", api.listNodePods)

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,  // 防止慢速连接长时间占用连接
		ReadTimeout:       10 * time.Second, // 防止慢速读取长时间占用连接
	}
	return serve(srv, hub)
}

// options 是命令行参数解析结果。
type options struct {
	// port 是 --port 的值（仅当 portSet 为 true 时生效）。
	port int
	// portSet 表示 --port 是否被显式指定。
	portSet bool
	// config 是 --config 指定的配置文件路径。
	config string
	// version 表示是否只打印版本信息后退出。
	version bool
}

// parseFlags 解析命令行参数（使用独立的 FlagSet，便于单元测试）。
func parseFlags(args []string, out io.Writer) (*options, error) {
	fs := flag.NewFlagSet("cloudcore", flag.ContinueOnError)
	fs.SetOutput(out)
	opts := &options{}
	fs.IntVar(&opts.port, "port", 0, "HTTP 服务监听端口（优先级高于环境变量与配置文件；不指定时回落到环境变量/配置文件/默认 8080）")
	fs.StringVar(&opts.config, "config", config.DefaultPath, "配置文件路径（JSON 格式，默认 config/cloudcore.json）")
	fs.BoolVar(&opts.version, "version", false, "打印版本信息后退出")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	// 记录 --port 是否被显式设置（flag.Visit 只遍历被设置的 flag）
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			opts.portSet = true
		}
	})
	return opts, nil
}

// serve 启动 HTTP 服务与 CloudHub，等待退出信号后一并优雅关闭，返回进程退出码。
// podSyncRequest 是云端下发 Pod 配置的请求体（M2 应用管理雏形）。
type podSyncRequest struct {
	Operation string  `json:"operation"` // add / update / delete
	Pod       podSpec `json:"pod"`
}

// podSpec 是 Pod 的最小规格描述（后续 M2 扩展为完整 PodSpec）。
type podSpec struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Replicas  int    `json:"replicas"`
}

// syncPod 通过可靠投递向指定边缘节点下发 PodSync 消息（WBS 4.6 端到端入口）。
// 响应语义：200=边缘已确认（Ack ok）；400=请求非法（JSON 解析失败/缺字段/
// operation 不在 {add,update,delete} 内）；404=节点未注册/离线；
// 502=边缘回 error Ack（消息已送达但被拒绝）；504=确认超时重试耗尽。
func (api *nodeAPI) syncPod(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")

	var req podSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json body"}`, http.StatusBadRequest)
		return
	}
	if req.Operation == "" || req.Pod.Name == "" {
		http.Error(w, `{"error":"operation and pod.name are required"}`, http.StatusBadRequest)
		return
	}
	// operation 白名单校验（WBS 4.6 P2：云端前置校验，避免非法值下发到边缘）
	switch req.Operation {
	case "add", "update", "delete":
	default:
		http.Error(w, `{"error":"invalid operation: must be add/update/delete"}`, http.StatusBadRequest)
		return
	}
	// 镜像校验：add/update 必须有 image（delete 不需要，按 name 删除）
	if req.Operation != "delete" && req.Pod.Image == "" {
		http.Error(w, `{"error":"pod.image is required for add/update"}`, http.StatusBadRequest)
		return
	}
	// 云端校验 operation 取值（P2-5）：非法值直接 400 拒绝，
	// 避免下发后等一轮可靠投递往返（最长 ~15s）才以 502 暴露。
	switch req.Operation {
	case "add", "update", "delete":
	default:
		http.Error(w, `{"error":"invalid operation: must be add, update or delete"}`, http.StatusBadRequest)
		return
	}

	msg, err := protocol.NewMessage(protocol.TypePodSync, "cloud", nodeID, req)
	if err != nil {
		http.Error(w, `{"error":"build message failed"}`, http.StatusInternalServerError)
		return
	}

	if err := api.reliableSend(r.Context(), nodeID, msg, cloudhub.ReliableOptions{}); err != nil {
		if errors.Is(err, cloudhub.ErrNodeOffline) {
			http.Error(w, `{"error":"node offline or not registered"}`, http.StatusNotFound)
			return
		}
		if errors.Is(err, cloudhub.ErrAckTimeout) {
			http.Error(w, `{"error":"ack timeout after retries"}`, http.StatusGatewayTimeout)
			return
		}
		if errors.Is(err, cloudhub.ErrAckFailed) {
			// 边缘明确回 error Ack（P2-2）：消息已送达但被拒绝，
			// 与「没送达」（404/504）语义不同，映射 502 Bad Gateway。
			http.Error(w, `{"error":"edge rejected ack"}`, http.StatusBadGateway)
			return
		}
		http.Error(w, `{"error":"send failed"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok","acked":true}`))
}

func serve(srv *http.Server, hub *cloudhub.Server) int {
	// 在独立 goroutine 中启动 HTTP 服务与 CloudHub，错误通过 channel 上报主流程
	errCh := make(chan error, 2)
	go func() {
		log.Infof("HTTP server listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		if err := hub.Start(); err != nil {
			errCh <- err
		}
	}()

	// 监听 SIGINT（Ctrl+C）/ SIGTERM（kill）信号，用于优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		// 任一服务启动/运行失败：尽力关闭两者后返回失败
		log.Errorf("服务异常退出: %v", err)
		shutdownAll(srv, hub)
		return 1
	case sig := <-quit:
		log.Infof("收到信号 %s，开始优雅退出...", sig)
	}

	// 给正在处理的请求最多 5 秒完成时间
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok := true
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("HTTP 服务优雅退出失败: %v", err)
		ok = false
	}
	if err := hub.Shutdown(ctx); err != nil {
		log.Errorf("CloudHub 优雅退出失败: %v", err)
		ok = false
	}
	if !ok {
		return 1
	}
	log.Infof("cloudcore exited")
	return 0
}

// shutdownAll 在服务异常退出时尽力关闭 HTTP 服务与 CloudHub（结果只记日志）。
func shutdownAll(srv *http.Server, hub *cloudhub.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("HTTP 服务关闭失败: %v", err)
	}
	if err := hub.Shutdown(ctx); err != nil {
		log.Errorf("CloudHub 关闭失败: %v", err)
	}
}

// nodeAPI 是节点查询 API（/api/v1/nodes）的处理器集合。
// 通过结构体字段注入注册表与 Pod 状态存储（依赖注入，避免全局变量）。
type nodeAPI struct {
	// reg 是节点注册表（与 CloudHub 事件桥接共享同一实例）。
	reg *registry.Registry
	// pods 是 Pod 状态存储（与 CloudHub 上报回调共享同一实例）。
	pods *podstatus.PodStatusStore
	// reliableSend 是可靠投递函数，默认指向 hub.ReliableSend（run 装配时注入）。
	// 独立成字段是为了让 syncPod 可测：测试注入 fake 即可覆盖各错误路径
	// （离线/超时/失败），无需真实 WebSocket 节点与 Ack 往返。
	reliableSend func(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error
}

// listNodes 处理 GET /api/v1/nodes：返回全部节点（JSON 数组，按 NodeID 排序）。
func (a *nodeAPI) listNodes(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 编码失败（如客户端已断开）时无需额外处理，忽略即可
	_ = json.NewEncoder(w).Encode(a.reg.List())
}

// getNode 处理 GET /api/v1/nodes/{nodeID}：返回单节点详情；节点不存在时 404。
func (a *nodeAPI) getNode(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	info, ok := a.reg.Get(nodeID)
	if !ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		// 编码失败（如客户端已断开）时无需额外处理，忽略即可
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 编码失败（如客户端已断开）时无需额外处理，忽略即可
	_ = json.NewEncoder(w).Encode(info)
}

// edgeNodeList 是 GET /api/v1/edgenodes 的响应形态。
//
// 选择 K8s List 风格（kind/apiVersion + items 数组）而非裸数组：
// 与 apiserver 的列表响应结构一致，后续接入真实 apiserver 时
// 客户端解析逻辑无需改动；items 里的元素就是完整 EdgeNode 对象
// （含 kind/apiVersion，可直接当作 CRD 对象消费）。
type edgeNodeList struct {
	Kind       string              `json:"kind"`
	APIVersion string              `json:"apiVersion"`
	Items      []v1alpha1.EdgeNode `json:"items"`
}

// listEdgeNodes 处理 GET /api/v1/edgenodes：返回全部节点的 EdgeNode
// 对象列表（按 Name 排序）。
func (a *nodeAPI) listEdgeNodes(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 编码失败（如客户端已断开）时无需额外处理，忽略即可
	_ = json.NewEncoder(w).Encode(edgeNodeList{
		Kind:       "EdgeNodeList",
		APIVersion: v1alpha1.SchemeGroupVersion.String(),
		Items:      a.reg.ListEdgeNodes(),
	})
}

// podStatusList 是 Pod 状态查询 API 的响应形态。
//
// 选择 K8s List 风格（kind/apiVersion + items 数组）而非裸数组：
// 与 edgenodes 接口的 edgeNodeList 形态一致，也与 apiserver 的列表响应
// 结构一致，后续接入真实 apiserver 时客户端解析逻辑无需改动。
// items 恒为非 nil（空数据编码为 [] 而非 null）。
type podStatusList struct {
	Kind       string                `json:"kind"`
	APIVersion string                `json:"apiVersion"`
	Items      []podstatus.PodStatus `json:"items"`
}

// listPods 处理 GET /api/v1/pods：返回全部节点的 Pod 状态
// （按 nodeID/namespace/podName 排序）；无数据时返回空数组而非 null。
func (a *nodeAPI) listPods(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 存储未注入（测试等场景）时按空列表处理，避免 nil 编码为 null
	items := make([]podstatus.PodStatus, 0)
	if a.pods != nil {
		items = a.pods.ListAll()
	}
	// 编码失败（如客户端已断开）时无需额外处理，忽略即可
	_ = json.NewEncoder(w).Encode(podStatusList{
		Kind:       "PodStatusList",
		APIVersion: "v1",
		Items:      items,
	})
}

// listNodePods 处理 GET /api/v1/nodes/{nodeID}/pods：返回单节点的 Pod 状态。
//
// 语义约定：
//   - 节点不存在（从未注册）→ 404（与 /api/v1/nodes/{nodeID} 的 404 语义一致）
//   - 节点存在但无 Pod → 200 + 空数组（不是 404："节点健康、只是还没 Pod"
//     与 "节点未知" 是两种语义，空数组让客户端可以无分支地遍历）
func (a *nodeAPI) listNodePods(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if a.reg == nil {
		w.WriteHeader(http.StatusNotFound)
		// 编码失败（如客户端已断开）时无需额外处理，忽略即可
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID})
		return
	}
	if _, ok := a.reg.Get(nodeID); !ok {
		w.WriteHeader(http.StatusNotFound)
		// 编码失败（如客户端已断开）时无需额外处理，忽略即可
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID})
		return
	}
	items := make([]podstatus.PodStatus, 0)
	if a.pods != nil {
		items = a.pods.ListByNode(nodeID)
	}
	// 编码失败（如客户端已断开）时无需额外处理，忽略即可
	_ = json.NewEncoder(w).Encode(podStatusList{
		Kind:       "PodStatusList",
		APIVersion: "v1",
		Items:      items,
	})
}

// getEdgeNode 处理 GET /api/v1/edgenodes/{nodeID}：返回单个 EdgeNode
// 对象；节点不存在时 404。
func (a *nodeAPI) getEdgeNode(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	info, ok := a.reg.Get(nodeID)
	if !ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		// 编码失败（如客户端已断开）时无需额外处理，忽略即可
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Get 返回的是拷贝，取地址安全；编码失败（如客户端已断开）时忽略即可
	_ = json.NewEncoder(w).Encode(a.reg.ToEdgeNode(&info))
}
