// Command load-test 是云边通道的并发注册 + 心跳压测工具（WBS 8.4 / G5）。
//
// 原理：模拟 N 个 edgecore 客户端并发接入 cloudcore——每个虚拟节点按真实
// 云边契约走一遍完整流程：
//   - WebSocket 拨号（ws://<cloud>/v1/edge，与 edgehub.Client 同路径）
//   - 发送 Register（负载与 RegisterPayload 契约一致），等待 RegisterAck
//     （按 CorrelationID 匹配），测量"注册延迟"
//   - 注册成功后连续发送 H 次 Heartbeat，等待各自 HeartbeatAck，
//     测量"心跳延迟"
//
// 统计口径：
//   - 注册成功率 = 收到 Accepted=true 的 RegisterAck 数 / 尝试数
//   - 注册延迟 = 拨号完成到收到 RegisterAck 的耗时（仅统计成功注册）
//   - 心跳延迟 = 发出 Heartbeat 到收到 HeartbeatAck 的耗时
//
// 输出：控制台中文摘要（stdout）+ JSON 结果（-out 指定文件，缺省追加到
// stdout 末尾，便于流水线直接消费）。
//
// 用法：
//
//	# 环境变量方式
//	LOAD_TEST_NODES=10 LOAD_TEST_HEARTBEATS=5 LOAD_TEST_CLOUD=ws://127.0.0.1:10000 \
//	    go run ./hack/load-test
//
//	# 参数方式（等价）
//	go run ./hack/load-test -cloud ws://127.0.0.1:10000 -nodes 10 -heartbeats 5 -out result.json
//
// 说明：本机资源限制下 N=10 可稳定运行；100 节点量级需在集群/独立压测环境
// 执行（见 docs/PERFORMANCE-BASELINE.md）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"edgeflow/pkg/protocol"
)

// 默认值（可被环境变量 / 命令行参数覆盖）。
const (
	defaultNodes      = 10 // 并发模拟节点数
	defaultHeartbeats = 5  // 每个节点注册成功后发送的心跳次数
	defaultCloud      = "ws://127.0.0.1:10000"
	registerTimeout   = 10 * time.Second // 等待 RegisterAck 超时（与 edgehub 契约一致）
	heartbeatTimeout  = 10 * time.Second // 等待 HeartbeatAck 超时
)

// 配置（优先级：命令行参数 > 环境变量 > 默认值）。
type config struct {
	cloud      string // cloudcore CloudHub 地址
	nodes      int    // 并发模拟节点数 N
	heartbeats int    // 每节点心跳次数 H
	out        string // JSON 输出文件（空 = 追加到 stdout）
}

func loadConfig() config {
	cfg := config{cloud: defaultCloud, nodes: defaultNodes, heartbeats: defaultHeartbeats}
	if v := os.Getenv("LOAD_TEST_CLOUD"); v != "" {
		cfg.cloud = v
	}
	if v := os.Getenv("LOAD_TEST_NODES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.nodes = n
		}
	}
	if v := os.Getenv("LOAD_TEST_HEARTBEATS"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			cfg.heartbeats = h
		}
	}
	flag.StringVar(&cfg.cloud, "cloud", cfg.cloud, "CloudHub 地址，如 ws://127.0.0.1:10000")
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "并发模拟节点数 N")
	flag.IntVar(&cfg.heartbeats, "heartbeats", cfg.heartbeats, "每节点心跳次数 H")
	flag.StringVar(&cfg.out, "out", "", "JSON 结果输出文件（空 = 追加到 stdout）")
	flag.Parse()
	// 参数合法性校验：nodes/heartbeats 必须 ≥ 1（否则除零 / 空结果，见审查 P2-10）
	if cfg.nodes < 1 {
		fmt.Fprintln(os.Stderr, "节点数必须 ≥ 1（-nodes 或 LOAD_TEST_NODES）")
		os.Exit(2)
	}
	if cfg.heartbeats < 1 {
		fmt.Fprintln(os.Stderr, "心跳次数必须 ≥ 1（-heartbeats 或 LOAD_TEST_HEARTBEATS）")
		os.Exit(2)
	}
	return cfg
}

// normalizeCloud 确保云地址带 ws:// 前缀与 /v1/edge 通道路径（与 edgehub
// 客户端行为一致：缺省自动补全）。
func normalizeCloud(addr string) (string, error) {
	if !strings.HasPrefix(addr, "ws://") && !strings.HasPrefix(addr, "wss://") {
		addr = "ws://" + addr
	}
	if !strings.Contains(addr, "/v1/edge") {
		addr = strings.TrimSuffix(addr, "/") + "/v1/edge"
	}
	return addr, nil
}

// ---- 契约负载（与 edge/pkg/edgehub 中定义一致，字段名不可改；
// 此处本地镜像，避免压测工具耦合应用包） ----

// registerPayload 是 Register 消息负载（CloudHub 契约）。
type registerPayload struct {
	NodeID          string `json:"nodeID"`
	Arch            string `json:"arch"`
	OS              string `json:"os"`
	EdgeCoreVersion string `json:"edgecoreVersion"`
	CPU             int    `json:"cpu"`
	Memory          uint64 `json:"memory"` // 单位：字节
}

// registerAckPayload 是 RegisterAck 消息负载（CloudHub 契约）。
type registerAckPayload struct {
	Accepted bool   `json:"accepted"`
	NodeName string `json:"nodeName"`
	Message  string `json:"message"`
}

// heartbeatPayload 是 Heartbeat 消息负载（CloudHub 契约）。
type heartbeatPayload struct {
	Timestamp int64 `json:"timestamp"` // 毫秒时间戳
}

// nodeResult 是单个虚拟节点的压测结果。
type nodeResult struct {
	NodeID     string    `json:"nodeID"`
	RegisterOK bool      `json:"registerOK"`
	RegisterMs float64   `json:"registerLatencyMs"` // 成功注册的耗时；失败为 0
	Heartbeat  []float64 `json:"heartbeatLatencyMs"`
	Err        string    `json:"error,omitempty"`
}

// runNode 模拟单个 edgecore：拨号 → 注册 → 心跳，返回该节点的测量结果。
func runNode(nodeID, cloud string, heartbeats int) nodeResult {
	res := nodeResult{NodeID: nodeID}
	dialer := websocket.Dialer{HandshakeTimeout: registerTimeout}
	start := time.Now()
	conn, _, err := dialer.Dial(cloud, nil)
	if err != nil {
		res.Err = "拨号失败: " + err.Error()
		return res
	}
	defer func() { _ = conn.Close() }()
	dialMs := float64(time.Since(start).Microseconds()) / 1000.0

	// ---- 注册：Register → RegisterAck（按 CorrelationID 匹配） ----
	regPayload := registerPayload{
		NodeID:          nodeID,
		Arch:            "arm64",
		OS:              "linux",
		EdgeCoreVersion: "load-test",
		CPU:             2,
		Memory:          4096,
	}
	regMsg, err := protocol.NewMessage(protocol.TypeRegister, nodeID, "cloud", regPayload)
	if err != nil {
		res.Err = "构造 Register 失败: " + err.Error()
		return res
	}
	regStart := time.Now()
	if err := conn.WriteJSON(regMsg); err != nil {
		res.Err = "发送 Register 失败: " + err.Error()
		return res
	}
	ack, err := waitAck(conn, regMsg.ID, registerTimeout)
	if err != nil {
		res.Err = "等待 RegisterAck 超时/失败: " + err.Error()
		return res
	}
	var regAck registerAckPayload
	if derr := ack.DecodePayload(&regAck); derr != nil {
		res.Err = "解析 RegisterAck 失败: " + derr.Error()
		return res
	}
	if !regAck.Accepted {
		res.Err = "注册被拒绝: " + regAck.Message
		return res
	}
	res.RegisterOK = true
	res.RegisterMs = float64(time.Since(regStart).Microseconds())/1000.0 + dialMs // 含拨号耗时

	// ---- 心跳：Heartbeat → HeartbeatAck（连续 H 次） ----
	for i := 0; i < heartbeats; i++ {
		hbPayload := heartbeatPayload{Timestamp: time.Now().UnixMilli()}
		hbMsg, err := protocol.NewMessage(protocol.TypeHeartbeat, nodeID, "cloud", hbPayload)
		if err != nil {
			res.Err = "构造 Heartbeat 失败: " + err.Error()
			break
		}
		hbStart := time.Now()
		if err := conn.WriteJSON(hbMsg); err != nil {
			res.Err = "发送 Heartbeat 失败: " + err.Error()
			break
		}
		if _, err := waitAck(conn, hbMsg.ID, heartbeatTimeout); err != nil {
			res.Err = "等待 HeartbeatAck 超时/失败: " + err.Error()
			break
		}
		res.Heartbeat = append(res.Heartbeat, float64(time.Since(hbStart).Microseconds())/1000.0)
	}
	return res
}

// waitAck 读取消息直到收到 CorrelationID 与 msgID 匹配的应答。
func waitAck(conn *websocket.Conn, msgID string, timeout time.Duration) (*protocol.Message, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		var m protocol.Message
		if err := conn.ReadJSON(&m); err != nil {
			return nil, err
		}
		if m.CorrelationID == msgID {
			return &m, nil
		}
		// 非匹配消息（理论上本连接只存在应答）直接跳过
	}
}

// latencyStats 是延迟分布的统计汇总（毫秒）。
type latencyStats struct {
	Mean float64 `json:"meanMs"`
	P50  float64 `json:"p50Ms"`
	P95  float64 `json:"p95Ms"`
	P99  float64 `json:"p99Ms"`
	Min  float64 `json:"minMs"`
	Max  float64 `json:"maxMs"`
}

// percentile 计算有序切片 p 分位（p ∈ [0,100]）。
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100.0)
	return sorted[idx]
}

func computeStats(values []float64) latencyStats {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return latencyStats{
		Mean: sum / float64(len(sorted)),
		P50:  percentile(sorted, 50),
		P95:  percentile(sorted, 95),
		P99:  percentile(sorted, 99),
		Min:  sorted[0],
		Max:  sorted[len(sorted)-1],
	}
}

// report 是压测的完整 JSON 结果。
type report struct {
	Cloud       string        `json:"cloud"`
	Nodes       int           `json:"nodes"`
	Heartbeats  int           `json:"heartbeatsPerNode"`
	StartedAt   string        `json:"startedAt"`
	DurationMs  float64       `json:"durationMs"`
	Register    registerStat  `json:"register"`
	Heartbeat   heartbeatStat `json:"heartbeat"`
	NodeDetails []nodeResult  `json:"nodeDetails"`
}

type registerStat struct {
	Attempted   int          `json:"attempted"`
	Succeeded   int          `json:"succeeded"`
	SuccessRate float64      `json:"successRate"`
	Latency     latencyStats `json:"latencyMs"`
}

type heartbeatStat struct {
	Sent      int          `json:"sent"`
	Succeeded int          `json:"succeeded"`
	Latency   latencyStats `json:"latencyMs"`
}

func main() {
	cfg := loadConfig()
	cloud, err := normalizeCloud(cfg.cloud)
	if err != nil {
		fmt.Fprintf(os.Stderr, "非法云地址 %q: %v\n", cfg.cloud, err)
		os.Exit(1)
	}

	fmt.Printf("=== EdgeFlow 云边通道压测 ===\n")
	fmt.Printf("目标: %s | 模拟节点: %d | 每节点心跳: %d\n", cloud, cfg.nodes, cfg.heartbeats)

	started := time.Now()
	results := make([]nodeResult, cfg.nodes)
	var wg sync.WaitGroup
	// 并发屏障：所有节点同时开始拨号，最大化注册并发
	barrier := make(chan struct{})
	for i := 0; i < cfg.nodes; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-barrier
			nodeID := fmt.Sprintf("load-node-%03d", idx+1)
			results[idx] = runNode(nodeID, cloud, cfg.heartbeats)
		}(i)
	}
	close(barrier)
	wg.Wait()
	duration := time.Since(started)

	// ---- 汇总 ----
	rep := report{
		Cloud:       cloud,
		Nodes:       cfg.nodes,
		Heartbeats:  cfg.heartbeats,
		StartedAt:   started.Format(time.RFC3339),
		DurationMs:  float64(duration.Microseconds()) / 1000.0,
		NodeDetails: results,
	}
	var regLat []float64
	var hbLat []float64
	for _, r := range results {
		if r.RegisterOK {
			regLat = append(regLat, r.RegisterMs)
		}
		rep.Heartbeat.Sent += len(r.Heartbeat)
		rep.Heartbeat.Succeeded += len(r.Heartbeat)
		hbLat = append(hbLat, r.Heartbeat...)
	}
	rep.Register.Attempted = cfg.nodes
	rep.Register.Succeeded = len(regLat)
	if cfg.nodes > 0 {
		rep.Register.SuccessRate = float64(len(regLat)) / float64(cfg.nodes)
	}
	rep.Register.Latency = computeStats(regLat)
	rep.Heartbeat.Latency = computeStats(hbLat)

	// ---- 控制台摘要 ----
	fmt.Printf("总耗时: %dms\n", duration.Milliseconds())
	fmt.Printf("注册: 成功 %d/%d（成功率 %.1f%%）| 延迟均值 %.2fms | P50 %.2fms | P95 %.2fms | P99 %.2fms\n",
		rep.Register.Succeeded, rep.Register.Attempted, rep.Register.SuccessRate*100,
		rep.Register.Latency.Mean, rep.Register.Latency.P50, rep.Register.Latency.P95, rep.Register.Latency.P99)
	fmt.Printf("心跳: 成功 %d/%d | 延迟均值 %.2fms | P50 %.2fms | P95 %.2fms | P99 %.2fms\n",
		rep.Heartbeat.Succeeded, rep.Heartbeat.Sent,
		rep.Heartbeat.Latency.Mean, rep.Heartbeat.Latency.P50, rep.Heartbeat.Latency.P95, rep.Heartbeat.Latency.P99)
	failed := 0
	for _, r := range results {
		if !r.RegisterOK {
			failed++
			fmt.Printf("  失败节点 %s: %s\n", r.NodeID, r.Err)
		}
	}
	if failed == 0 {
		fmt.Printf("全部节点注册成功 ✓\n")
	}

	// ---- JSON 输出 ----
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化结果失败: %v\n", err)
		os.Exit(1)
	}
	if cfg.out != "" {
		if werr := os.WriteFile(cfg.out, append(raw, '\n'), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", cfg.out, werr)
			os.Exit(1)
		}
		fmt.Printf("JSON 结果已写入 %s\n", cfg.out)
	} else {
		fmt.Printf("\n--- JSON ---\n%s\n", raw)
	}

	// 注册有失败视为压测不通过（exit 1），便于 CI 门禁
	if rep.Register.Succeeded < cfg.nodes {
		os.Exit(1)
	}
}
