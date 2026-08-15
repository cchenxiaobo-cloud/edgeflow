// cloudcore 是 EdgeFlow 的云端组件入口。
//
// 对标 KubeEdge 的 CloudCore：未来将在这里承载云边通信
// （WebSocket）、消息路由（NATS）、设备管理（CRD）等云端逻辑。
// 当前版本提供：加载配置、启动 HTTP 服务（/healthz 健康检查）、
// 启动 CloudHub（云边 WebSocket 通道，默认端口 10000）。
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	v1alpha1 "edgeflow/apis/edge/v1alpha1"
	"edgeflow/cloud/pkg/audit"
	"edgeflow/cloud/pkg/auth"
	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/devicestatus"
	"edgeflow/cloud/pkg/metrics"
	"edgeflow/cloud/pkg/nodecontroller"
	"edgeflow/cloud/pkg/podstatus"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/certs"
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

	// 云边通道 mTLS（WBS 7.1 证书管理 + 7.4 云边认证）：
	// EDGEFLOW_CLOUDCORE_TLS=on 时启用 TLS 监听，并要求边缘侧携带
	// 本 CA 签发的客户端证书（双向认证）。
	//   - 证书目录：EDGEFLOW_CLOUDCORE_CERT_DIR（默认 data/certs/）
	//   - 首次运行自动生成 CA 与 cloudcore 服务端证书；已存在则加载（幂等）
	//   - 未开启时行为与之前完全一致（纯 ws://，向后兼容）
	var hubTLS *tls.Config
	if os.Getenv("EDGEFLOW_CLOUDCORE_TLS") == "on" {
		certDir := os.Getenv("EDGEFLOW_CLOUDCORE_CERT_DIR")
		if certDir == "" {
			certDir = certs.DefaultCertDir
		}
		if _, err := certs.EnsureCA(certDir); err != nil {
			log.Errorf("CA 初始化失败（certDir=%s）: %v", certDir, err)
			return 1
		}
		// SAN 注入（M4B P1-4）：EDGEFLOW_CLOUDCORE_TLS_SAN 支持
		// "IP:1.2.3.4,DNS:cloudcore.svc" 逗号分隔列表；未设置时
		// 回退默认（127.0.0.1/localhost/cloudcore，仅本机可用）。
		// M4C P2-① 修复：非法条目从"Warn 跳过"改为"报错退出"（fail-fast）——
		// 配错的 SAN 若被静默跳过，证书只覆盖默认 SAN（仅回环），边缘节点
		// mTLS 会全部握手失败且难以定位，必须在启动阶段就暴露配置错误。
		var ips []net.IP
		var dnsNames []string
		if san := os.Getenv("EDGEFLOW_CLOUDCORE_TLS_SAN"); san != "" {
			ips, dnsNames, err = parseSANList(san)
			if err != nil {
				log.Errorf("EDGEFLOW_CLOUDCORE_TLS_SAN 配置无效: %v", err)
				return 1
			}
		}
		if _, err := certs.EnsureServerCertWithSANs(certDir, "cloudcore", ips, dnsNames); err != nil {
			log.Errorf("cloudcore 服务端证书初始化失败: %v", err)
			return 1
		}
		if hubTLS, err = certs.LoadTLSConfig(certDir, true); err != nil {
			log.Errorf("加载 TLS 配置失败: %v", err)
			return 1
		}
		log.Infof("CloudHub TLS 已启用（certDir=%s, mTLS: 强制要求并验证客户端证书）", certDir)
	}
	hub := cloudhub.New(fmt.Sprintf(":%d", hubPort), cloudhub.WithTLS(hubTLS),
		// WBS 7.3 设备认证：EDGEFLOW_CLOUDCORE_NODE_TOKEN 非空时启用节点接入
		// 令牌校验（edgecore 注册必须携带相同 token）；未设置保持向后兼容。
		cloudhub.WithNodeToken(os.Getenv("EDGEFLOW_CLOUDCORE_NODE_TOKEN")))

	// 节点注册表（内存态）与 CloudHub 事件桥接：
	// 节点注册/心跳/断开时实时维护节点元数据，供查询 API 使用。
	// 依赖注入（SetNodeEvents），CloudHub 不感知注册表实现。
	nodeReg := registry.New()
	hub.SetNodeEvents(registry.NewCloudHubAdapter(nodeReg))

	// 节点心跳静默超时管理（WBS 2.4）：定时扫描注册表，把心跳停滞
	// （LastHeartbeatAt 超过 timeout 未更新）的节点标记 Offline。
	// 与 CloudHub 断开事件互补：断开事件覆盖"连接断了"的常规场景，
	// 本控制器兜底"连接活着但心跳停滞 / 断开事件丢失"的场景
	// （对标 KubeEdge NodeController）。
	// 周期/阈值环境变量覆盖：EDGEFLOW_CLOUDCORE_NODE_SCAN_INTERVAL /
	// EDGEFLOW_CLOUDCORE_NODE_TIMEOUT（默认 30s / 180s，支持 "15s" 或秒数）。
	scanInterval, nodeTimeout, err := nodecontroller.DurationsFromEnv()
	if err != nil {
		log.Errorf("NodeController 配置无效: %v", err)
		return 1
	}
	nc := nodecontroller.New(nodeReg,
		nodecontroller.WithInterval(scanInterval),
		nodecontroller.WithTimeout(nodeTimeout),
	)
	nc.Start()
	log.Infof("NodeController 装配完成: 扫描周期 %v, 心跳超时 %v", scanInterval, nodeTimeout)

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

	// 设备状态存储（内存态，WBS 5.3）与 CloudHub 上报回调桥接：
	// 边侧上报的 DeviceReport 消息经 CloudHub 校验后注入存储，供查询 API 使用。
	// 依赖注入（SetDeviceReportHandler），CloudHub 不感知存储实现。
	deviceStore := devicestatus.NewStore()
	hub.SetDeviceReportHandler(func(nodeID string, dr cloudhub.DeviceReportPayload) {
		// 回调运行在 CloudHub 读循环 goroutine 内：recover 兜底，
		// 防止单条异常数据导致整个连接处理崩溃（与 PodStatus 回调同约定）。
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("DeviceReport handler panic（nodeID=%s）: %v", nodeID, r)
			}
		}()
		deviceStore.Upsert(nodeID, devicestatus.DeviceStatus{
			NodeID:         nodeID,
			DeviceName:     dr.DeviceName,
			Namespace:      dr.Namespace,
			Properties:     dr.Properties,
			LastReportedAt: dr.ReportedAt,
		})
	})

	// 审计台账（WBS 7.5）：JSONL 追加写，记录每次管理 API 调用。
	// 路径可用环境变量 EDGEFLOW_CLOUDCORE_AUDIT_PATH 覆盖（默认 data/audit-ledger.jsonl）；
	// 初始化失败直接拒绝启动（审计是安全控制，静默降级会掩盖问题）。
	auditPath := os.Getenv(audit.EnvPath)
	if auditPath == "" {
		auditPath = audit.DefaultPath
	}
	ledger, err := audit.NewLedger(auditPath)
	if err != nil {
		log.Errorf("审计台账初始化失败: %v", err)
		return 1
	}
	defer func() { _ = ledger.Close() }()
	log.Infof("审计台账就绪（%s，JSONL 追加写）", auditPath)

	// API Token 认证（WBS 7.2）：默认关闭（向后兼容），
	// EDGEFLOW_CLOUDCORE_AUTH=on 时启用，令牌来自 EDGEFLOW_CLOUDCORE_API_TOKEN。
	// 开启但未配置令牌 → 拒绝启动（fail-fast，与 TLS SAN 校验同约定）。
	authEnabled := auth.EnabledFromEnv()
	var apiToken string
	if authEnabled {
		apiToken = auth.TokenFromEnv()
		if apiToken == "" {
			log.Errorf("%s=on 但 %s 未设置，拒绝启动（认证开启必须配置令牌）", auth.EnvAuth, auth.EnvToken)
			return 1
		}
		log.Infof("API 认证已启用：/api/v1/* 需携带 Authorization: Bearer <token>")
	} else {
		log.Infof("API 认证未启用（默认关闭，向后兼容；%s=on 开启）", auth.EnvAuth)
	}

	// 注册路由：/healthz 健康检查 + 节点查询 API（两个视角并存）：
	//   /api/v1/nodes      → NodeInfo 视图（运行视角：CPU/内存/IP 等）
	//   /api/v1/edgenodes  → EdgeNode 视图（CRD 对象视角，对标 K8s Node）
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", httpx.Healthz())
	api := &nodeAPI{
		reg:          nodeReg,
		pods:         podStore,
		devices:      deviceStore,
		reliableSend: hub.ReliableSendContext,
	}
	// 管理 API 全部注册在独立 apiMux 上，统一挂认证 + 审计中间件链
	// （WBS 7.5/7.2）：
	//   - 审计在外层：请求 context 挂身份槽，未认证请求（401）同样留痕，
	//     安全审计不丢未授权访问尝试；
	//   - 认证在内层：校验通过后把身份写入身份槽（audit.SetIdentity），
	//     审计落盘 operator=token。
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /api/v1/nodes", api.listNodes)
	apiMux.HandleFunc("GET /api/v1/nodes/{nodeID}", api.getNode)
	apiMux.HandleFunc("GET /api/v1/edgenodes", api.listEdgeNodes)
	apiMux.HandleFunc("GET /api/v1/edgenodes/{nodeID}", api.getEdgeNode)
	apiMux.HandleFunc("POST /api/v1/nodes/{nodeID}/podsync", api.syncPod)
	apiMux.HandleFunc("POST /api/v1/nodes/{nodeID}/config-sync", api.syncConfig)
	apiMux.HandleFunc("GET /api/v1/pods", api.listPods)
	apiMux.HandleFunc("GET /api/v1/nodes/{nodeID}/pods", api.listNodePods)
	// 设备链路 API（WBS 5.3/2.2）：设备状态查询 + 设备指令下发
	apiMux.HandleFunc("GET /api/v1/devices", api.listDevices)
	apiMux.HandleFunc("GET /api/v1/nodes/{nodeID}/devices", api.listNodeDevices)
	apiMux.HandleFunc("POST /api/v1/nodes/{nodeID}/device-command", api.sendDeviceCommand)

	var apiHandler http.Handler = apiMux
	if authEnabled {
		apiHandler = auth.Middleware(apiToken)(apiHandler)
	}
	apiHandler = ledger.Middleware(apiHandler)
	mux.Handle("/api/v1/", apiHandler)

	// 可观测性（WBS 10.1）：/metrics 端点输出 Prometheus 文本格式指标。
	// gauge 取值函数由装配层注入（依赖倒置，metrics 包不感知各存储实现）。
	m := metrics.New(metrics.Providers{
		Nodes:             nodeReg.Count,
		Pods:              podStore.Count,
		Devices:           deviceStore.Count,
		ActiveConnections: hub.ConnCount,
	})
	mux.HandleFunc("GET /metrics", m.Handler())

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           m.Middleware(mux), // 最外层：统计全部 HTTP 请求（含 /healthz、/metrics）
		ReadHeaderTimeout: 5 * time.Second,   // 防止慢速连接长时间占用连接
		ReadTimeout:       10 * time.Second,  // 防止慢速读取长时间占用连接
	}
	return serve(srv, hub, nc)
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

// parseSANList 解析 EDGEFLOW_CLOUDCORE_TLS_SAN 逗号分隔列表
// （如 "IP:1.2.3.4,DNS:cloudcore.svc"），返回 IP 与 DNS 名列表。
// 语法校验（M4C P2-①）：无法识别的前缀、IP 解析失败、空 DNS 名、
// 空条目（多余逗号）一律返回错误——配置错误必须在启动阶段暴露，
// 而不是静默跳过导致证书 SAN 不完整。
func parseSANList(san string) ([]net.IP, []string, error) {
	var ips []net.IP
	var dnsNames []string
	for _, item := range strings.Split(san, ",") {
		item = strings.TrimSpace(item)
		switch {
		case item == "":
			return nil, nil, fmt.Errorf("含空条目（检查是否有多余逗号）")
		case strings.HasPrefix(item, "IP:"):
			ip := net.ParseIP(strings.TrimPrefix(item, "IP:"))
			if ip == nil {
				return nil, nil, fmt.Errorf("条目 %q 不是合法 IP（格式: IP:1.2.3.4）", item)
			}
			ips = append(ips, ip)
		case strings.HasPrefix(item, "DNS:"):
			name := strings.TrimPrefix(item, "DNS:")
			if name == "" {
				return nil, nil, fmt.Errorf("条目 %q 的 DNS 名为空（格式: DNS:host.example.com）", item)
			}
			dnsNames = append(dnsNames, name)
		default:
			return nil, nil, fmt.Errorf("条目 %q 无法识别（仅支持 IP: 与 DNS: 前缀）", item)
		}
	}
	return ips, dnsNames, nil
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

// configSyncRequest 是云端下发 ConfigMap/Secret 配置的请求体（M2：WBS 6.2）。
// 与 PodSync 的 podSyncRequest 同构：operation + 配置对象。
// config 字段与边缘 ConfigSyncPayload.config 契约一致，原样下发。
type configSyncRequest struct {
	Operation string     `json:"operation"` // add / update / delete
	Config    configSpec `json:"config"`    // 配置对象（含 name/namespace/kind/data）
}

// configSpec 是配置的最小规格描述。
// Data 是 map[string]string：ConfigMap 的 value 为原始字符串；Secret 的
// value 按 base64 编码语义（云端负责编码，边缘原样存储）。
type configSpec struct {
	Name      string            `json:"name"`      // 配置名称（必填）
	Namespace string            `json:"namespace"` // 命名空间（缺省 "default"）
	Kind      string            `json:"kind"`      // 类型：ConfigMap / Secret（必填）
	Data      map[string]string `json:"data"`      // 键值数据（add/update 必填）
}

// syncConfig 通过可靠投递向指定边缘节点下发 ConfigSync 消息（WBS 6.2 端到端入口）。
// 校验规则：operation 白名单 {add,update,delete}、config.name 必填、
// kind 白名单 {ConfigMap,Secret}、add/update 时 data 非空。
// 响应语义与 syncPod 五态一致：200=边缘已确认（Ack ok）；400=请求非法；
// 404=节点未注册/离线；502=边缘回 error Ack（消息已送达但被拒绝）；
// 504=确认超时重试耗尽；500=其他发送失败。
func (api *nodeAPI) syncConfig(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")

	var req configSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json body"}`, http.StatusBadRequest)
		return
	}
	if req.Operation == "" || req.Config.Name == "" {
		http.Error(w, `{"error":"operation and config.name are required"}`, http.StatusBadRequest)
		return
	}
	// operation 白名单校验（与 syncPod 同约定：云端前置校验，
	// 避免非法值下发到边缘，省一轮可靠投递往返）
	switch req.Operation {
	case "add", "update", "delete":
	default:
		http.Error(w, `{"error":"invalid operation: must be add/update/delete"}`, http.StatusBadRequest)
		return
	}
	// kind 白名单校验：ConfigMap/Secret 之外的类型直接 400 拒绝
	// （契约约束，防止未知类型数据落到边缘）
	switch req.Config.Kind {
	case "ConfigMap", "Secret":
	default:
		http.Error(w, `{"error":"invalid kind: must be ConfigMap/Secret"}`, http.StatusBadRequest)
		return
	}
	// data 校验：add/update 必须有 data（delete 不需要，按 name 删除）
	if req.Operation != "delete" && len(req.Config.Data) == 0 {
		http.Error(w, `{"error":"config.data is required for add/update"}`, http.StatusBadRequest)
		return
	}

	msg, err := protocol.NewMessage(protocol.TypeConfigSync, "cloud", nodeID, req)
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
			// 边缘明确回 error Ack：消息已送达但被拒绝，
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

func serve(srv *http.Server, hub *cloudhub.Server, nc *nodecontroller.NodeController) int {
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
		// 任一服务启动/运行失败：尽力关闭全部后返回失败
		log.Errorf("服务异常退出: %v", err)
		shutdownAll(srv, hub, nc)
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
	// NodeController 扫描循环随之停止（优雅退出的一部分）
	nc.Stop()
	if !ok {
		return 1
	}
	log.Infof("cloudcore exited")
	return 0
}

// shutdownAll 在服务异常退出时尽力关闭 HTTP 服务与 CloudHub（结果只记日志）。
func shutdownAll(srv *http.Server, hub *cloudhub.Server, nc *nodecontroller.NodeController) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("HTTP 服务关闭失败: %v", err)
	}
	if err := hub.Shutdown(ctx); err != nil {
		log.Errorf("CloudHub 关闭失败: %v", err)
	}
	nc.Stop()
}

// nodeAPI 是节点查询 API（/api/v1/nodes）的处理器集合。
// 通过结构体字段注入注册表、Pod 状态存储与设备状态存储
// （依赖注入，避免全局变量）。
type nodeAPI struct {
	// reg 是节点注册表（与 CloudHub 事件桥接共享同一实例）。
	reg *registry.Registry
	// pods 是 Pod 状态存储（与 CloudHub 上报回调共享同一实例）。
	pods *podstatus.PodStatusStore
	// devices 是设备状态存储（与 CloudHub 上报回调共享同一实例，WBS 5.3）。
	devices *devicestatus.DeviceStatusStore
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
