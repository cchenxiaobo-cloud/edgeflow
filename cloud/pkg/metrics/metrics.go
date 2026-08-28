// Package metrics 实现 cloudcore 的可观测性指标（WBS 10.1）。
//
// 设计约束：零第三方依赖（不引 Prometheus client 库），纯标准库实现。
// 指标以 Prometheus 文本格式（# HELP / # TYPE / name value）经 /metrics
// 端点暴露，兼容 Prometheus 文本采集器（prometheus.yml 直接 scrape）。
//
// 指标清单：
//   - edgeflow_cloudcore_nodes_total（gauge）：已注册边缘节点总数（含离线）
//   - edgeflow_cloudcore_pods_total（gauge）：云端 Pod 状态记录总数
//   - edgeflow_cloudcore_devices_total（gauge）：云端设备状态记录总数
//   - edgeflow_cloudcore_active_connections（gauge）：CloudHub 活跃连接数
//   - edgeflow_cloudcore_http_requests_total（counter，按 路由模式+状态码 分桶）
//
// 取值方式：gauge 通过 Provider 函数注入（依赖倒置）——metrics 包不感知
// 注册表/存储/CloudHub 的实现，由装配层（cloudcore main）把真实计数函数
// 传进来；counter 由 Middleware 在请求完成时自增。
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Providers 是各 gauge 指标的取值函数集合（由装配层注入）。
// 任一字段为 nil 时，对应指标行不出现在 /metrics 输出中。
type Providers struct {
	// Nodes 返回已注册节点总数（registry.Count）。
	Nodes func() int
	// Pods 返回 Pod 状态记录总数（podstatus.PodStatusStore.Count）。
	Pods func() int
	// Devices 返回设备状态记录总数（devicestatus.DeviceStatusStore.Count）。
	Devices func() int
	// ActiveConnections 返回 CloudHub 当前活跃连接数（cloudhub.Server.ConnCount）。
	ActiveConnections func() int
	// LeaseRenewalFailures 返回外部 etcd 模式心跳租约续约失败累计计数
	// （v0.8.0，L12；lease_registry.RenewalFailures）。仅外部多副本形态注入，
	// 其余形态 nil → 指标行不输出。
	LeaseRenewalFailures func() uint64
	// LeaseHBRebuilds 返回外部 etcd 模式 hb 键修复性重建累计计数
	// （v0.11.0，L12+；lease_registry.HBRe builds）。仅外部多副本形态注入，
	// 其余形态 nil → 指标行不输出。
	LeaseHBRebuilds func() uint64
	// HubSendBufferBytes 返回全部活跃连接发送缓冲在途字节合计
	// （CHN-02，v0.22.0；cloudhub.Server.BroadcastBytesInView）：广播 N 节点
	// 内存峰值可观测。nil → 指标行不输出；字节计量关闭（配额<=0）时恒 0。
	HubSendBufferBytes func() int64
}

// Metrics 是云端指标注册表：持有 gauge Provider 与请求计数，负责渲染
// Prometheus 文本格式。并发安全：计数与渲染均加锁，可被多个 goroutine
// （HTTP 处理、CloudHub 回调）并发使用。
type Metrics struct {
	mu        sync.Mutex
	providers Providers
	// requests 是 http_requests_total 的分桶计数：路由模式|状态码 → 次数。
	requests map[requestKey]uint64
}

// requestKey 是请求计数的分桶键：路由模式 + 状态码。
// 路由模式（如 /api/v1/nodes/{nodeID}）而非实际路径，避免高基数
// （nodeID 变化会让指标维度爆炸）。
type requestKey struct {
	path string
	code int
}

// New 创建指标注册表（初始为空计数）。
func New(p Providers) *Metrics {
	return &Metrics{
		providers: p,
		requests:  make(map[requestKey]uint64),
	}
}

// gaugeDefs 是全部 gauge 指标的定义（与 Providers 字段一一对应）。
var gaugeDefs = []struct {
	label    string // 指标名
	help     string // 指标说明（# HELP 文案）
	provider func(*Metrics) func() int
}{
	{
		label: "edgeflow_cloudcore_nodes_total",
		help:  "已注册边缘节点总数（含离线节点）。",
		provider: func(m *Metrics) func() int {
			return m.providers.Nodes
		},
	},
	{
		label: "edgeflow_cloudcore_pods_total",
		help:  "云端 Pod 状态记录总数。",
		provider: func(m *Metrics) func() int {
			return m.providers.Pods
		},
	},
	{
		label: "edgeflow_cloudcore_devices_total",
		help:  "云端设备状态记录总数。",
		provider: func(m *Metrics) func() int {
			return m.providers.Devices
		},
	},
	{
		label: "edgeflow_cloudcore_active_connections",
		help:  "CloudHub 当前活跃连接数（含已连接未注册的节点）。",
		provider: func(m *Metrics) func() int {
			return m.providers.ActiveConnections
		},
	},
}

// IncRequest 自增一条请求计数（路由模式 + 状态码）。
// 供 Middleware 在请求完成后调用；独立暴露便于测试与特殊路径接入。
func (m *Metrics) IncRequest(path string, code int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestKey{path: path, code: code}]++
}

// routePattern 提取请求的路由模式（低基数标签）：
// 优先用 r.Pattern（Go 1.23+ ServeMux 匹配时写入），未匹配（如 404）
// 时回退实际路径；Pattern 可能带方法前缀（"GET /api/v1/nodes"），
// 指标按路径分桶，去掉方法前缀避免 GET/POST 同路由分裂成两个桶。
func routePattern(r *http.Request) string {
	pat := r.Pattern
	if pat == "" {
		pat = r.URL.Path
	}
	if i := strings.IndexByte(pat, ' '); i >= 0 {
		pat = pat[i+1:]
	}
	return pat
}

// Middleware 包装任意 HTTP handler：请求完成后按 路由模式+状态码 计数。
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		m.IncRequest(routePattern(r), rec.statusCode())
	})
}

// Handler 返回 /metrics 端点处理器：输出 Prometheus 文本格式指标。
func (m *Metrics) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		// 先渲染到 buffer 再一次性写出：渲染期间持锁（快照计数），
		// 避免慢速客户端写响应时长时间阻塞计数更新。
		body := m.render()
		_, _ = w.Write(body)
	}
}

// render 渲染全部指标（Prometheus 文本格式），输出确定性排序：
// gauge 按指标名排序，counter 按 (路径, 状态码) 排序。
func (m *Metrics) render() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	// gauge：Provider 为 nil 时跳过该指标（装配层未注入则无意义）
	for _, g := range gaugeDefs {
		fn := g.provider(m)
		if fn == nil {
			continue
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n",
			g.label, g.help, g.label, g.label, fn())
	}

	// counter：按 (路径, 状态码) 排序保证输出确定性
	if len(m.requests) > 0 {
		fmt.Fprintf(&b, "# HELP edgeflow_cloudcore_http_requests_total 云端 HTTP 请求累计数（按路由模式与状态码分桶）。\n# TYPE edgeflow_cloudcore_http_requests_total counter\n")
		keys := make([]requestKey, 0, len(m.requests))
		for k := range m.requests {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].path != keys[j].path {
				return keys[i].path < keys[j].path
			}
			return keys[i].code < keys[j].code
		})
		for _, k := range keys {
			fmt.Fprintf(&b, "edgeflow_cloudcore_http_requests_total{path=%q,code=%q} %d\n",
				escapeLabel(k.path), strconv.Itoa(k.code), m.requests[k])
		}
	}

	// 续约失败计数（v0.8.0，L12）：Provider 注入时始终输出（0 值也有监控
	// 意义——面板可基于增长率告警，见 KNOWN-ISSUES L12 建议）。
	if m.providers.LeaseRenewalFailures != nil {
		fmt.Fprintf(&b, "# HELP edgeflow_cloudcore_lease_renewal_failures_total 外部 etcd 心跳租约续约失败累计数（持续增长 = etcd 侧异常/网络分区，告警阈值参考判活 TTL）。\n# TYPE edgeflow_cloudcore_lease_renewal_failures_total counter\n%s %d\n",
			"edgeflow_cloudcore_lease_renewal_failures_total", m.providers.LeaseRenewalFailures())
	}
	// hb 键修复性重建计数（v0.11.0，L12+）：Provider 注入时始终输出（0 值
	// 也有监控意义——面板可基于增长率告警；持续增长 = 租约抖动/键被外部删除）。
	if m.providers.LeaseHBRebuilds != nil {
		fmt.Fprintf(&b, "# HELP edgeflow_cloudcore_lease_hb_rebuilds_total 外部 etcd 心跳键修复性重建累计数（本副本在服务但 hb 键被删/缺失后由续约 worker 成功重建的次数；持续增长 = 租约抖动/键被外部删除）。\n# TYPE edgeflow_cloudcore_lease_hb_rebuilds_total counter\n%s %d\n",
			"edgeflow_cloudcore_lease_hb_rebuilds_total", m.providers.LeaseHBRebuilds())
	}
	// 发送缓冲在途字节（CHN-02，v0.22.0）：Provider 注入时始终输出。
	// 广播 N 节点的内存峰值≈该值；逼近配额（默认 64MiB/连接）说明存在慢客户端。
	if m.providers.HubSendBufferBytes != nil {
		fmt.Fprintf(&b, "# HELP edgeflow_cloudcore_hub_send_buffer_bytes 全部活跃连接发送缓冲在途字节合计（慢客户端积压观测：广播 N 节点内存峰值≈该值；单连接配额默认 64MiB，逼近配额 = 存在慢客户端）。\n# TYPE edgeflow_cloudcore_hub_send_buffer_bytes gauge\n%s %d\n",
			"edgeflow_cloudcore_hub_send_buffer_bytes", m.providers.HubSendBufferBytes())
	}
	return []byte(b.String())
}

// escapeLabel 转义 Prometheus 标签值中的特殊字符（\ " \n）。
// 路径中一般不含这些字符，但防御性处理避免输出非法文本。
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// statusRecorder 包装 ResponseWriter，记录响应状态码（供计数/审计使用）。
// 语义与 net/http 一致：未显式 WriteHeader 时按 200 计；重复 WriteHeader 忽略。
type statusRecorder struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

// code 返回记录的响应状态码（未写过头则默认为 200）。
func (r *statusRecorder) statusCode() int {
	if !r.wroteHeader {
		return http.StatusOK
	}
	return r.code
}

// WriteHeader 记录状态码并透传；重复调用忽略（与 net/http 行为一致）。
func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.code = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

// Write 未显式 WriteHeader 时按 200 计，然后透传写入。
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.code = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}
