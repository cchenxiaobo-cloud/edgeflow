// Package cloudhub 实现 CloudHub：云边通信的 WebSocket 服务端（WBS 2.1）。
//
// 职责（M1 基础版）：
//   - 监听云边通道（路径 /v1/edge，端口默认 10000，与 EdgeHub 契约一致）
//   - 处理边缘节点注册（Register → RegisterAck）与心跳（Heartbeat → HeartbeatAck）
//   - 同一 nodeID 只允许一个活跃连接，重复注册时踢掉旧连接
//   - 心跳超时（默认 90s 无消息）断开连接
//   - 维护节点注册表，供 EdgeController 等后续模块查询
//
// 认证说明：M1 基础版不校验连接来源（CheckOrigin 全放行）。
// mTLS（WBS 7.4）已落地：EDGEFLOW_CLOUDCORE_TLS=on 时经 WithTLS 注入
// tls.Config，监听层强制要求并验证客户端证书（RequireAndVerifyClientCert），
// CheckOrigin 仍全放行（TLS 层已认证身份，应用层不重复校验）。
package cloudhub

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"

	"github.com/gorilla/websocket"
)

// 云边通道约定（与 EdgeHub 契约一致，勿单独修改）。
const (
	// DefaultHubPort 是 CloudHub 默认监听端口。
	DefaultHubPort = 10000
	// EnvHubPort 是覆盖 CloudHub 监听端口的环境变量名。
	EnvHubPort = "EDGEFLOW_CLOUDCORE_HUB_PORT"
	// PathEdge 是云边 WebSocket 通道的 HTTP 路径。
	PathEdge = "/v1/edge"
	// HeartbeatTimeout 是连接失活判定阈值：超过该时长未收到任何消息即断开。
	HeartbeatTimeout = 90 * time.Second
)

// 通用 Ack 错误码（与 EdgeHub 约定）。
const (
	// CodeConflict 表示同 nodeID 重复注册，旧连接被抢占。
	CodeConflict = "conflict"
	// CodeInvalidMessage 表示消息格式非法（缺少必填字段/JSON 解析失败）。
	CodeInvalidMessage = "invalid_message"
	// CodeNotRegistered 表示节点未注册就发送了需要注册态的消息。
	CodeNotRegistered = "not_registered"
)

// 内部实现参数。
const (
	// sendBufferSize 是每个连接的发送缓冲（消息数），防慢客户端阻塞写方。
	sendBufferSize = 64
	// writeTimeout 是单条消息的写出超时。
	writeTimeout = 10 * time.Second
	// maxMessageBytes 是单条消息的大小上限（1 MiB），防内存耗尽。
	maxMessageBytes = 1 << 20
	// readHeaderTimeout 是升级握手前的读请求头超时。
	readHeaderTimeout = 10 * time.Second
	// connGoroutineWaitTimeout 是 Shutdown 等待连接 goroutine 退出的超时
	// 上限（P2-1 兜底）：正常路径下连接被 closeAllConns 关闭后，读循环/
	// 写循环/心跳监控立即退出，无需等待这么久；超时只是防御性兜底，
	// 防止任何未预期路径把 Shutdown 卡死。
	connGoroutineWaitTimeout = 5 * time.Second
)

// RegisterPayload 是 Register 消息的负载（边→云）。
// Compression 是新增可选字段（WBS 4.4 压缩协商，旧云端忽略未知字段）：
// 边缘声明 "gzip" 表示支持通道压缩；云端仅在 RegisterAck 回带同值时
// 才启用双向压缩（见 server.go encodeFor 与 compress.go 文件头注释）。
type RegisterPayload struct {
	NodeID          string `json:"nodeID"`          // 节点唯一 ID
	Arch            string `json:"arch"`            // CPU 架构（如 arm64/amd64）
	OS              string `json:"os"`              // 操作系统（如 linux）
	EdgecoreVersion string `json:"edgecoreVersion"` // edgecore 版本号
	CPU             int    `json:"cpu"`             // CPU 核数
	Memory          uint64 `json:"memory"`          // 内存大小（字节），契约与 EdgeHub 一致为 uint64
	// Token 是接入令牌（WBS 7.3 设备认证）：edgecore 注册时携带，
	// 服务端 nodeToken 非空时校验，不匹配拒绝注册（accepted=false）。
	Token string `json:"token"`
	// Compression 是压缩能力声明（可选）："gzip" 表示支持云边通道
	// gzip 压缩（WBS 4.4）。旧版本（v1.0）不发送该字段，云端保持明文下发。
	Compression string `json:"compression,omitempty"`
}

// RegisterAckPayload 是 RegisterAck 消息的负载（云→边）。
// Compression 与 RegisterPayload.Compression 对应：云端启用压缩时回带
// "gzip"，边缘据此启用上行压缩（协商闭环，见 compress.go 文件头注释）。
type RegisterAckPayload struct {
	Accepted bool   `json:"accepted"` // 是否接受注册
	NodeName string `json:"nodeName"` // 云端分配的节点名（M1 与 nodeID 一致）
	Message  string `json:"message"`  // 附加说明（拒绝原因 / 欢迎语）
	// Compression 是压缩能力确认（可选）："gzip" 表示云端已启用通道压缩。
	Compression string `json:"compression,omitempty"`
}

// HeartbeatPayload 是 Heartbeat 消息的负载（边→云）。
type HeartbeatPayload struct {
	Timestamp int64 `json:"timestamp"` // 边侧发送时间（毫秒）
}

// PodStatusPayload 是 PodStatus 消息的负载（边→云，WBS 6.3 契约）。
// 字段与协议契约一致，勿单独修改；存储侧对应的快照类型见
// edgeflow/cloud/pkg/podstatus.PodStatus（由装配层做字段映射）。
type PodStatusPayload struct {
	NodeID          string `json:"nodeID"`          // 节点唯一 ID（应与消息 Source 一致）
	PodName         string `json:"podName"`         // Pod 名称
	Namespace       string `json:"namespace"`       // 命名空间（缺省按 default 处理）
	Phase           string `json:"phase"`           // 阶段：Running/Stopped/Absent/Error
	Message         string `json:"message"`         // 附加说明（如错误原因）
	LastReconcileAt int64  `json:"lastReconcileAt"` // 最近一次协调时间（毫秒时间戳）
}

// HeartbeatAckPayload 是 HeartbeatAck 消息的负载（云→边）。
type HeartbeatAckPayload struct {
	NodeStatus string `json:"nodeStatus"` // 节点状态，M1 固定 "Ready"
}

// AckPayload 是通用 Ack 消息的负载（双向）。
type AckPayload struct {
	Code    string `json:"code"`    // 错误码（见 Code* 常量）
	Message string `json:"message"` // 人类可读说明
}

// NodeInfo 是已注册节点的信息快照（供 EdgeController 等后续模块查询）。
type NodeInfo struct {
	NodeID          string    // 节点 ID
	Arch            string    // CPU 架构
	OS              string    // 操作系统
	EdgecoreVersion string    // edgecore 版本
	CPU             int       // CPU 核数
	Memory          uint64    // 内存字节数（uint64，与 RegisterPayload 契约一致）
	RemoteIP        string    // 连接来源 IP
	RegisteredAt    time.Time // 注册时间
}

// NodeEvents 是节点生命周期事件回调接口，供注册表/控制器等外部模块订阅
// （依赖注入，避免 CloudHub 直接耦合具体注册表实现）。
//
// 并发与性能约定：
//   - 回调在 CloudHub 内部锁之外、连接处理 goroutine 中同步调用
//   - 实现方应尽快返回、不得执行阻塞操作，也不得反向调用 CloudHub 的方法（防止死锁）
type NodeEvents interface {
	// OnNodeRegistered 在节点注册成功时调用，info 是注册信息快照。
	OnNodeRegistered(info NodeInfo)
	// OnNodeHeartbeat 在收到已注册节点的心跳时调用。
	OnNodeHeartbeat(nodeID string)
	// OnNodeDisconnected 在节点连接断开并从注册表移除时调用。
	OnNodeDisconnected(nodeID string)
}

// PodStatusHandler 处理边侧上报的 PodStatus 消息（依赖注入，WBS 6.3）。
// 与 NodeEvents 同风格：CloudHub 不感知具体存储实现，由 cmd/cloudcore
// 装配时注入 store.Upsert 的适配函数。
//
// 并发与性能约定（与 NodeEvents 一致）：
//   - 回调在 CloudHub 内部锁之外、连接处理 goroutine 中同步调用
//   - 实现方应尽快返回、不得执行阻塞操作，也不得反向调用 CloudHub 的方法（防止死锁）
type PodStatusHandler func(nodeID string, ps PodStatusPayload)

// Server 是 CloudHub WebSocket 服务端。
//
// 并发安全说明：registry/nodes 由 mu 保护；连接集合由 connsMu 保护；
// 监听状态由 stateMu 保护；连接内部状态见 conn 结构体注释。
type Server struct {
	// addr 是监听地址（如 ":10000"）。
	addr string
	// upgrader 负责把 HTTP 连接升级为 WebSocket。
	upgrader websocket.Upgrader
	// tlsConfig 非 nil 时监听层启用 mTLS（WBS 7.4）：
	// 服务端证书 + 强制要求并验证客户端证书。由装配层经 WithTLS 注入。
	tlsConfig *tls.Config
	// heartbeatTimeout 是连接失活判定阈值（可经 Option 覆盖，测试用）。
	heartbeatTimeout time.Duration
	// monitorInterval 是心跳监控的扫描周期。
	monitorInterval time.Duration
	// nodeToken 是节点接入令牌（WBS 7.3 设备认证）；非空时注册必须携带
	// 相同 Token，否则拒绝注册。空值向后兼容（不校验）。
	nodeToken string
	// compressEnabled 是云边通道压缩开关（WBS 4.4）：默认开启；关闭后
	// 不向边缘回带压缩能力（RegisterAck 无 compression 字段），边缘
	// 经协商自动保持明文——单开关控制双向（见 pkg/protocol/compress.go）。
	compressEnabled bool

	// stateMu 保护以下启动/停止相关字段。
	stateMu sync.Mutex
	ln      net.Listener
	httpSrv *http.Server
	started bool
	// shuttingDown 标记已进入关闭流程，防止 Start/Shutdown 交错。
	shuttingDown atomic.Bool
	// serveDone 在 Serve 返回后关闭，Shutdown 据此等待退出。
	serveDone chan struct{}

	// mu 保护注册表 registry、节点信息 nodes 与事件回调
	// nodeEvents/podStatusHandler/deviceReportHandler。
	mu                  sync.RWMutex
	registry            map[string]*conn     // nodeID → 活跃连接
	nodes               map[string]*NodeInfo // nodeID → 节点信息
	nodeEvents          NodeEvents           // 节点生命周期事件回调（nil 表示未订阅）
	podStatusHandler    PodStatusHandler     // PodStatus 消息回调（nil 表示未订阅）
	deviceReportHandler DeviceReportHandler  // DeviceReport 消息回调（nil 表示未订阅，WBS 5.3）

	// connsMu 保护活跃连接集合 conns（含未注册连接，供 Shutdown 统一关闭）。
	connsMu sync.Mutex
	conns   map[*conn]struct{}
	// wg 跟踪所有连接相关 goroutine（读循环/写循环/心跳监控）。
	wg sync.WaitGroup

	// ackMu 保护待确认表 pending（可靠投递，WBS 4.6）。
	ackMu sync.Mutex
	// pending 是在途可靠消息表：msgID → 等待项（ReliableSend 注册、handleAck 匹配）。
	pending map[string]*pendingEntry
}

// Option 是 Server 的构造选项。
type Option func(*Server)

// WithHeartbeatTimeout 覆盖连接失活阈值（默认 90s，测试中缩短用）。
func WithHeartbeatTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.heartbeatTimeout = d
		}
	}
}

// WithNodeToken 注入节点接入令牌（WBS 7.3 设备认证）。
// 非空时：edgecore 注册必须携带相同 Token（常数时间比较），否则拒绝注册
// （RegisterAck accepted=false）。空值（默认）保持向后兼容：不校验。
func WithNodeToken(token string) Option {
	return func(s *Server) { s.nodeToken = token }
}

// WithTLS 为云边通道启用 mTLS（WBS 7.1/7.4）：
//   - 传 nil 等价于不调用（保持纯 WebSocket 行为，向后兼容）；
//   - 传入由 certs.LoadTLSConfig 构造的 tls.Config（服务端形态：
//     携带 cloudcore 证书 + ClientCAs + RequireAndVerifyClientCert）后，
//     Start 时监听层自动启用 TLS，边缘侧必须携带 CA 签发的客户端证书
//     才能完成握手。
//
// 实现选择：Start 内用 tls.NewListener 包装底层监听器再交给
// http.Server.Serve（与 net/http 的 ListenAndServeTLS 内部同构）：
// 证书来自内存 tls.Config 而非文件路径，握手错误只影响单条连接，
// Shutdown 的关闭语义与纯 TCP 监听完全一致。
func WithTLS(tlsConfig *tls.Config) Option {
	return func(s *Server) {
		s.tlsConfig = tlsConfig
	}
}

// WithCompress 设置云边通道压缩开关（WBS 4.4）：默认开启，传 false 关闭。
// 关闭语义：云端不再向边缘确认压缩能力（RegisterAck 不带 compression
// 字段），已协商连接之外的边缘经协商自动回落明文——单开关控双向，
// 与旧版本（v1.0）互操作不受影响（协商式兼容，见 pkg/protocol/compress.go）。
// 注意：配置变更需重启生效（与 hubPort 同策略，热重载时保持旧值）。
func WithCompress(enabled bool) Option {
	return func(s *Server) {
		s.compressEnabled = enabled
	}
}

// New 创建 CloudHub 服务端，addr 形如 ":10000"。
func New(addr string, opts ...Option) *Server {
	s := &Server{
		addr: addr,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// M1 基础版不做来源校验；WBS 4.5 引入 Token/mTLS 后收紧
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
		heartbeatTimeout: HeartbeatTimeout,
		serveDone:        make(chan struct{}),
		registry:         make(map[string]*conn),
		nodes:            make(map[string]*NodeInfo),
		conns:            make(map[*conn]struct{}),
		pending:          make(map[string]*pendingEntry),
		// 压缩默认开启（WBS 4.4）；经 WithCompress(false) 关闭。
		// 默认开启的兼容性由协商机制保证：旧边缘不声明能力 → 明文；
		// 旧云端不回带能力 → 新边缘保持明文。
		compressEnabled: true,
	}
	for _, opt := range opts {
		opt(s)
	}
	// 心跳监控扫描周期：不超过 1s（90s 阈值下可及时感知），也不小于 50ms
	s.monitorInterval = s.heartbeatTimeout / 3
	if s.monitorInterval > time.Second {
		s.monitorInterval = time.Second
	}
	if s.monitorInterval < 50*time.Millisecond {
		s.monitorInterval = 50 * time.Millisecond
	}
	return s
}

// PortFromEnv 读取 CloudHub 监听端口：优先环境变量 EDGEFLOW_CLOUDCORE_HUB_PORT，
// 未设置时返回默认值 10000。0 表示随机可用端口（测试用）。
func PortFromEnv() (int, error) {
	v := os.Getenv(EnvHubPort)
	if v == "" {
		return DefaultHubPort, nil
	}
	p, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("环境变量 %s=%q 不是合法端口", EnvHubPort, v)
	}
	if p < 0 || p > 65535 {
		return 0, fmt.Errorf("环境变量 %s=%q 超出端口范围 0-65535", EnvHubPort, v)
	}
	return p, nil
}

// Start 启动 CloudHub 并阻塞，直到 Shutdown 被调用或监听出错。
// 正常关闭（Shutdown）后返回 nil；启动/运行错误返回 error。
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("CloudHub 监听 %s 失败: %w", s.addr, err)
	}
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	// mTLS：WithTLS 注入 tls.Config 时用 tls.NewListener 包装监听器。
	// 握手失败由 net/http 在单连接上处理（写回错误并关闭），不影响其他连接；
	// 握手成功的连接 r.TLS 会被填充（serveWS 据此记录对端证书身份）。
	if s.tlsConfig != nil {
		srv.TLSConfig = s.tlsConfig
		ln = tls.NewListener(ln, s.tlsConfig)
	}
	s.stateMu.Lock()
	if s.started {
		s.stateMu.Unlock()
		_ = ln.Close()
		return errors.New("CloudHub 已启动，勿重复调用 Start")
	}
	if s.shuttingDown.Load() {
		// Shutdown 先于 Start 完成初始化：放弃本次启动
		s.stateMu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.ln, s.httpSrv, s.started = ln, srv, true
	s.stateMu.Unlock()
	defer close(s.serveDone)

	log.Infof("CloudHub listening on %s", ln.Addr())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Addr 返回实际监听地址（Start 完成初始化后可用；未启动时为空串）。
func (s *Server) Addr() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Shutdown 优雅关闭：停止接受新连接 → 等待进行中的请求收尾 →
// 关闭所有 WebSocket 连接并等待连接 goroutine 退出。
// 未启动的 Server 调用 Shutdown 是安全的（幂等）。
func (s *Server) Shutdown(ctx context.Context) error {
	s.shuttingDown.Store(true)
	s.stateMu.Lock()
	srv, ln := s.httpSrv, s.ln
	s.stateMu.Unlock()

	var firstErr error
	switch {
	case srv != nil:
		// 已正常启动：先关监听器，再等 Serve 退出
		if err := srv.Shutdown(ctx); err != nil {
			firstErr = err
		}
		select {
		case <-s.serveDone:
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
		}
	case ln != nil:
		// Start 尚在初始化：关闭监听器，Serve 会立即返回
		_ = ln.Close()
		select {
		case <-s.serveDone:
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
		}
	}

	// 关闭所有 WebSocket 连接（含未注册的），等待连接 goroutine 退出
	s.closeAllConns()
	// 终止全部在途可靠投递等待（P2-1）：连接已关，Ack 不会再到达，
	// 直接关闭 pending 通道唤醒等待者返回 ErrShuttingDown，避免 Shutdown
	// 期间 HTTP 处理器（如 syncPod）空等至单次超时（默认 5s）。
	s.failAllPending()
	// 等待连接 goroutine 退出，带超时兜底（P2-1）：trackConn 已保证
	// 不漏关连接、wg 计数守恒，理论上 Wait 必然快速返回；超时仅防止
	// 未预期路径把 Shutdown 无限阻塞。
	if !s.waitConnections(connGoroutineWaitTimeout) {
		log.Warnf("CloudHub Shutdown：等待连接 goroutine 退出超时（%v），继续关闭流程", connGoroutineWaitTimeout)
	}
	return firstErr
}

// handler 返回 CloudHub 的 HTTP 路由（云边通道升级入口）。
// 单独抽出便于测试用 httptest 启动。
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PathEdge, s.serveWS)
	return mux
}

// serveWS 处理 /v1/edge 的 WebSocket 升级，并为每个连接
// 启动读循环、写循环和心跳监控三个 goroutine。
//
// 竞态说明（P2-1）：http.Server.Shutdown 不等待已 hijack 的连接，
// 若 Shutdown 恰在 Upgrade 与 trackConn 之间执行，closeAllConns 的
// 快照会漏掉本连接。trackConn 在 connsMu 内复查 shuttingDown 并完成
// wg.Add(3)：被快照漏掉的连接由本路径立即关闭（不启动 goroutine），
// 且 wg.Add 与 Shutdown 的 wg.Wait 互斥于 connsMu，不存在 WaitGroup
// 禁止的「零计数时 Add 与 Wait 并发」。Shutdown 侧另有
// waitConnections 超时兜底，任何漏网连接都不会让 Shutdown 无限阻塞。
func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	// mTLS 身份旁证：TLS 握手成功后 r.TLS 携带对端证书链（WBS 7.4）。
	// 记录客户端证书 CN（edgeflow-<nodeID>）便于审计与排查。
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		peerCN := r.TLS.PeerCertificates[0].Subject.CommonName
		log.Infof("mTLS 连接已认证（peer CN=%s, 来源 %s）", peerCN, r.RemoteAddr)
	}
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warnf("WebSocket 升级失败（来源 %s）: %v", r.RemoteAddr, err)
		return
	}
	ws.SetReadLimit(maxMessageBytes)
	c := newConn(ws, remoteIP(r.RemoteAddr))
	if !s.trackConn(c) {
		// Shutdown 已开始：本连接不在 closeAllConns 快照内，
		// 立即关闭，不启动任何连接 goroutine（wg 计数未被占用）。
		c.close()
		return
	}
	go func() {
		defer s.wg.Done()
		c.writeLoop()
	}()
	go func() {
		defer s.wg.Done()
		s.monitor(c)
	}()
	go func() {
		defer s.wg.Done()
		s.readLoop(c)
	}()
}

// readLoop 是连接的读循环：读消息 → 更新 lastSeen → 分发处理。
// 连接断开或读错误时清理注册信息并关闭连接。
func (s *Server) readLoop(c *conn) {
	defer func() {
		s.untrackConn(c)
		s.unregister(c)
		c.close()
	}()
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return // 连接关闭或读错误，结束
		}
		// 任何消息都视为活跃，刷新心跳时间
		c.lastSeen.Store(time.Now().UnixMilli())
		s.dispatch(c, data)
	}
}

// writeLoop 是连接的写循环：从缓冲 channel 取消息写出。
// 写失败（对端关闭/超时）时关闭连接，防止 goroutine 泄漏。
func (c *conn) writeLoop() {
	for {
		select {
		case data := <-c.send:
			if err := c.write(data); err != nil {
				c.close()
				return
			}
		case <-c.closed:
			return
		}
	}
}

// monitor 是心跳监控：超过 heartbeatTimeout 未收到任何消息则断开连接。
// 未注册的连接同样受监控，防止死连接长期占用资源。
func (s *Server) monitor(c *conn) {
	ticker := time.NewTicker(s.monitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			last := time.UnixMilli(c.lastSeen.Load())
			if time.Since(last) >= s.heartbeatTimeout {
				nodeID, _ := c.nodeID.Load().(string)
				log.Warnf("连接心跳超时（%s 无消息），断开: nodeID=%s ip=%s",
					s.heartbeatTimeout, nodeID, c.remoteIP)
				c.close()
				return
			}
		case <-c.closed:
			return
		}
	}
}

// dispatch 解码并分发消息：Register/Heartbeat/Ack 之外的类型 M1 暂不支持。
// 解码走 DecodeCompressed：带压缩 magic 的帧自动解压，明文帧回落原路径
// （旧版本互操作，见 pkg/protocol/compress.go）。
func (s *Server) dispatch(c *conn, data []byte) {
	m, err := protocol.DecodeCompressed(data)
	if err != nil {
		s.rejectInvalid(c, data, err)
		return
	}
	switch m.Type {
	case protocol.TypeRegister:
		s.handleRegister(c, m)
	case protocol.TypeHeartbeat:
		s.handleHeartbeat(c, m)
	case protocol.TypePodStatus:
		s.handlePodStatus(c, m)
	case protocol.TypeDeviceReport:
		s.handleDeviceReport(c, m)
	case protocol.TypeAck:
		s.handleAck(c, m)
	default:
		log.Warnf("暂不支持的消息类型 %q（source=%s），忽略", m.Type, m.Source)
	}
}

// rejectInvalid 处理无法通过协议校验的消息：尽力回发 Ack 说明原因，不中断连接。
// 压缩帧解码失败时（损坏/超限），先用 Decompress 提取明文再做元信息解析：
// 解压本身失败（帧损坏）则无法拿到 source，仅记录日志（与明文同路径）。
func (s *Server) rejectInvalid(c *conn, data []byte, cause error) {
	log.Warnf("收到非法消息（%s），来源 %s", cause, c.remoteIP)
	// 解码失败时无法拿到完整信封，尽量从原始数据提取 source 用于回 Ack
	var meta struct {
		Source        string `json:"source"`
		CorrelationID string `json:"correlationId"`
	}
	if protocol.IsCompressed(data) {
		if plain, err := protocol.Decompress(data); err == nil {
			_ = json.Unmarshal(plain, &meta) // 尽力提取；解压后仍非法则放弃
		}
	} else if err := json.Unmarshal(data, &meta); err != nil || meta.Source == "" {
		// 连 source 都没有：无法回 Ack（Ack 信封要求 Target 非空），仅记录日志
		return
	}
	if meta.Source == "" {
		return
	}
	s.sendTo(c, meta.Source, protocol.TypeAck, meta.CorrelationID,
		AckPayload{Code: CodeInvalidMessage, Message: cause.Error()})
}

// handleRegister 处理节点注册：
//   - 校验 payload（nodeID 必填且与消息 Source 一致）
//   - 同 nodeID 已有连接时踢掉旧连接（发送 conflict Ack 后关闭）
//   - 记录节点信息并回 RegisterAck（accepted=true）
func (s *Server) handleRegister(c *conn, m *protocol.Message) {
	if c.dead.Load() {
		// 已被新连接抢占，拒绝后续注册，避免死连接反复抢占
		return
	}
	var reg RegisterPayload
	if err := m.DecodePayload(&reg); err != nil {
		s.rejectRegister(c, m, fmt.Sprintf("注册 payload 解析失败: %v", err))
		return
	}
	if reg.NodeID == "" {
		s.rejectRegister(c, m, "注册 payload 缺少 nodeID")
		return
	}
	if reg.NodeID != m.Source {
		s.rejectRegister(c, m, fmt.Sprintf("消息 Source=%q 与 payload.nodeID=%q 不一致", m.Source, reg.NodeID))
		return
	}
	// WBS 7.3 设备认证：nodeToken 非空时校验注册令牌（常数时间比较防时序侧信道）。
	// 不匹配 → 拒绝注册；nodeToken 为空时向后兼容（不校验，M1-M3 行为）。
	if s.nodeToken != "" && subtle.ConstantTimeCompare([]byte(reg.Token), []byte(s.nodeToken)) != 1 {
		s.rejectRegister(c, m, "接入令牌校验失败（token 缺失或不匹配）")
		return
	}
	// WBS 4.4 压缩协商：边缘声明 gzip 且云端开关开启 → 本连接启用压缩。
	// 协商结果同时决定下发编码（encodeFor）与 RegisterAck 回带字段。
	c.compression.Store(s.compressEnabled && reg.Compression == "gzip")

	info := &NodeInfo{
		NodeID:          reg.NodeID,
		Arch:            reg.Arch,
		OS:              reg.OS,
		EdgecoreVersion: reg.EdgecoreVersion,
		CPU:             reg.CPU,
		Memory:          reg.Memory,
		RemoteIP:        c.remoteIP,
		RegisteredAt:    time.Now(),
	}

	// evicted 记录因本连接更换 nodeID 而被清理的旧节点，锁外通知事件回调
	var evicted string
	s.mu.Lock()
	// 同一连接更换 nodeID 重注册时，先清理旧注册项，避免孤儿映射
	if oldID, _ := c.nodeID.Load().(string); oldID != "" && oldID != reg.NodeID {
		if s.registry[oldID] == c {
			delete(s.registry, oldID)
			delete(s.nodes, oldID)
			evicted = oldID
		}
	}
	if old, exists := s.registry[reg.NodeID]; exists && old != c {
		// 同 nodeID 重复注册：在锁内先标记旧连接死亡并完成换新，
		// 防止旧连接后续消息（如再次 Register）反过来抢占新连接
		old.dead.Store(true)
		s.registry[reg.NodeID] = c
		s.nodes[reg.NodeID] = info
		c.nodeID.Store(reg.NodeID)
		c.registered.Store(true)
		s.mu.Unlock()
		s.kick(old, "同 nodeID 重复注册，新连接抢占")
	} else {
		s.registry[reg.NodeID] = c
		s.nodes[reg.NodeID] = info
		c.nodeID.Store(reg.NodeID)
		c.registered.Store(true)
		s.mu.Unlock()
	}

	// 节点生命周期事件（锁外调用，见 notifyNodeEvent 注释）
	if evicted != "" {
		s.notifyNodeEvent(func(h NodeEvents) { h.OnNodeDisconnected(evicted) })
	}
	s.notifyNodeEvent(func(h NodeEvents) { h.OnNodeRegistered(*info) })

	log.Infof("节点 %s 注册成功（ip=%s arch=%s os=%s edgecore=%s）",
		reg.NodeID, c.remoteIP, reg.Arch, reg.OS, reg.EdgecoreVersion)
	s.sendTo(c, reg.NodeID, protocol.TypeRegisterAck, m.ID,
		RegisterAckPayload{Accepted: true, NodeName: reg.NodeID, Message: "注册成功",
			Compression: s.ackCompression(c)})
}

// ackCompression 返回 RegisterAck 应回带的压缩能力：仅当本连接已协商
// gzip（云端开关开启 && 边缘声明 gzip，见 handleRegister 的 Store）时回
// "gzip"，否则为空串。回带值即本连接协商结果——旧边缘不声明 → 空串
// （保持明文）；云端关闭 → 空串（边缘不上行压缩）。边缘侧仅在收到
// "gzip" 后才启用上行压缩。
func (s *Server) ackCompression(c *conn) string {
	if c.compression.Load() {
		return "gzip"
	}
	return ""
}

// rejectRegister 回发 RegisterAck（accepted=false）并记录日志。
func (s *Server) rejectRegister(c *conn, m *protocol.Message, reason string) {
	log.Warnf("节点 %s 注册被拒绝: %s", m.Source, reason)
	s.sendTo(c, m.Source, protocol.TypeRegisterAck, m.ID,
		RegisterAckPayload{Accepted: false, Message: reason})
}

// handleHeartbeat 处理心跳：更新已由读循环完成的 lastSeen，回 HeartbeatAck。
// 未注册连接发来的心跳会被拒绝。
func (s *Server) handleHeartbeat(c *conn, m *protocol.Message) {
	if !c.registered.Load() {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeNotRegistered, Message: "节点未注册，拒绝心跳"})
		return
	}
	var hb HeartbeatPayload
	if err := m.DecodePayload(&hb); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "心跳 payload 解析失败: " + err.Error()})
		return
	}
	s.sendTo(c, m.Source, protocol.TypeHeartbeatAck, m.ID,
		HeartbeatAckPayload{NodeStatus: "Ready"})
	// 通知事件回调（锁外调用）：刷新注册表心跳时间与状态
	s.notifyNodeEvent(func(h NodeEvents) { h.OnNodeHeartbeat(m.Source) })
}

// handlePodStatus 处理边侧上报的 PodStatus 消息：
//   - 未注册连接上报 → 回 not_registered Ack 拒绝
//   - payload 解析失败 / 缺少 podName / payload.nodeID 与 Source 不一致 → 回 invalid_message Ack
//   - 校验通过 → 调用注入的 PodStatusHandler 回调（锁外调用），不另行回 Ack
//     （上报是单向流式语义，契约未约定应答；边缘侧无需等待确认）
func (s *Server) handlePodStatus(c *conn, m *protocol.Message) {
	if !c.registered.Load() {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeNotRegistered, Message: "节点未注册，拒绝 PodStatus"})
		return
	}
	var ps PodStatusPayload
	if err := m.DecodePayload(&ps); err != nil {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "PodStatus payload 解析失败: " + err.Error()})
		return
	}
	if ps.PodName == "" {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "PodStatus payload 缺少 podName"})
		return
	}
	if ps.NodeID != "" && ps.NodeID != m.Source {
		s.sendTo(c, m.Source, protocol.TypeAck, m.CorrelationID,
			AckPayload{Code: CodeInvalidMessage, Message: "PodStatus payload.nodeID 与消息 Source 不一致"})
		return
	}
	// payload 缺省/空 nodeID 时以消息 Source 为准（消息来源即权威）
	ps.NodeID = m.Source
	s.notifyPodStatus(ps.NodeID, ps)
	log.Infof("收到节点 %s 的 PodStatus: %s/%s phase=%s", ps.NodeID, ps.Namespace, ps.PodName, ps.Phase)
}

// handleAck 处理边侧返回的通用 Ack：记录日志，并匹配可靠投递的在途等待者
// （WBS 4.6，按 CorrelationID == 在途 msg.ID 匹配，见 resolvePending）。
// payload 不可解析不影响确认：确认语义由信封的 CorrelationID 决定。
func (s *Server) handleAck(c *conn, m *protocol.Message) {
	var ack AckPayload
	if err := m.DecodePayload(&ack); err != nil {
		log.Infof("收到节点 %s 的 Ack（payload 不可解析: %v）", m.Source, err)
	} else {
		log.Infof("收到节点 %s 的 Ack: code=%s message=%q", m.Source, ack.Code, ack.Message)
	}
	s.resolvePending(m)
}

// kick 踢掉旧连接：先尽力送达 conflict Ack，再关闭连接。
func (s *Server) kick(c *conn, reason string) {
	target, _ := c.nodeID.Load().(string)
	log.Warnf("踢掉旧连接: nodeID=%s ip=%s（%s）", target, c.remoteIP, reason)
	if target != "" {
		m, err := protocol.NewMessage(protocol.TypeAck, "cloud", target,
			AckPayload{Code: CodeConflict, Message: reason})
		if err == nil {
			// 按被踢连接的协商结果编码（旧边缘未协商 → 明文）
			if data, err := s.encodeFor(c, m); err == nil {
				if err := c.write(data); err != nil {
					log.Warnf("向被踢连接 %s 发送冲突通知失败: %v", target, err)
				}
			}
		}
	}
	c.close()
}

// sendTo 构造云→边消息（Source=cloud，Target=target）并投递到连接的发送缓冲。
// 投递失败（连接已关闭或缓冲已满）时关闭连接，防止慢客户端阻塞写方。
func (s *Server) sendTo(c *conn, target, msgType, correlationID string, payload any) {
	m, err := protocol.NewMessage(msgType, "cloud", target, payload)
	if err != nil {
		log.Errorf("构造 %s 消息失败: %v", msgType, err)
		return
	}
	m.CorrelationID = correlationID
	data, err := s.encodeFor(c, m)
	if err != nil {
		log.Errorf("编码 %s 消息失败: %v", msgType, err)
		return
	}
	if !c.trySend(data) {
		log.Warnf("向 %s 投递 %s 失败：连接已关闭或发送缓冲已满", target, msgType)
	}
}

// encodeFor 按连接压缩协商结果编码下发消息：
//   - 未协商（旧边缘 / 压缩关闭）或 RegisterAck（协商信道）→ 明文 Encode；
//   - 已协商 gzip → EncodeCompressed（内含小消息跳过：心跳等小消息
//     仍走明文，对旧路径完全透明）。
//
// Register/RegisterAck 恒为明文是协商式兼容的基石（见
// pkg/protocol/compress.go 文件头注释）：即使未来压缩实现出问题，
// 注册/应答这条协商信道也不受影响。
func (s *Server) encodeFor(c *conn, m *protocol.Message) ([]byte, error) {
	if c.compression.Load() && m.Type != protocol.TypeRegisterAck {
		return protocol.EncodeCompressed(m)
	}
	return protocol.Encode(m)
}

// unregister 在连接断开时从注册表移除节点（仅当注册表仍指向该连接时才删除，
// 避免误删已被新连接接管的注册项），并通知节点断开事件。
func (s *Server) unregister(c *conn) {
	s.mu.Lock()
	nodeID, _ := c.nodeID.Load().(string)
	removed := nodeID != "" && s.registry[nodeID] == c
	if removed {
		delete(s.registry, nodeID)
		delete(s.nodes, nodeID)
	}
	c.registered.Store(false)
	s.mu.Unlock()

	if removed {
		log.Infof("节点 %s 已断开（ip=%s），从注册表移除", nodeID, c.remoteIP)
		s.notifyNodeEvent(func(h NodeEvents) { h.OnNodeDisconnected(nodeID) })
	}
}

// trackConn / untrackConn 维护活跃连接集合（Shutdown 时统一关闭）。
//
// trackConn 返回 false 表示 Shutdown 已开始：连接未被纳入集合（也不应
// 再纳入，closeAllConns 的快照可能已执行完毕），调用方应立即关闭连接。
// 在 connsMu 内复查 shuttingDown 可保证：返回 true 的连接必然出现在
// 之后任意一次 closeAllConns 的快照中（二者互斥于同一把锁）。
//
// wg.Add(3) 同样在 connsMu 内完成（读循环/写循环/心跳监控三个 goroutine
// 的计数）：与 Shutdown 的 wg.Wait 之间满足「零计数时 Add 先于 Wait」——
// 要么本连接的 Add 先于 closeAllConns 的快照完成（Wait 时计数 >0，
// 必然被等待），要么 trackConn 看到 shuttingDown 已置位而返回 false
// （不 Add）。两种情况下都不存在 WaitGroup 禁止的 Add(正值) 与 Wait 并发。
func (s *Server) trackConn(c *conn) bool {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.shuttingDown.Load() {
		return false
	}
	s.conns[c] = struct{}{}
	s.wg.Add(3)
	return true
}

func (s *Server) untrackConn(c *conn) {
	s.connsMu.Lock()
	delete(s.conns, c)
	s.connsMu.Unlock()
}

// closeAllConns 关闭所有活跃 WebSocket 连接。
func (s *Server) closeAllConns() {
	s.connsMu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.connsMu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

// waitConnections 等待全部连接 goroutine 退出，带超时兜底（P2-1）。
// 返回 true 表示全部退出；超时返回 false（调用方记录日志后继续，
// 不阻塞 Shutdown 主流程）。超时后后台等待 goroutine 不会泄漏：
// 连接最终关闭、wg 计数归零后自行退出。
func (s *Server) waitConnections(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// SetNodeEvents 注册节点生命周期事件回调（nil 表示取消）。可在任意时刻调用。
func (s *Server) SetNodeEvents(h NodeEvents) {
	s.mu.Lock()
	s.nodeEvents = h
	s.mu.Unlock()
}

// SetPodStatusHandler 注册 PodStatus 消息回调（nil 表示取消）。可在任意时刻调用。
// 与 SetNodeEvents 并存：NodeEvents 管节点生命周期，PodStatusHandler 管
// Pod 状态上报，二者互不影响（向后兼容：未注册回调时 PodStatus 仅记日志）。
func (s *Server) SetPodStatusHandler(h PodStatusHandler) {
	s.mu.Lock()
	s.podStatusHandler = h
	s.mu.Unlock()
}

// notifyPodStatus 在锁外安全地调用 PodStatus 回调：先在锁内取回调快照，
// 再在锁外执行，保证回调执行期间不持有任何 CloudHub 锁（防止回调
// 反向调用 CloudHub 方法时死锁，与 notifyNodeEvent 同约定）。
func (s *Server) notifyPodStatus(nodeID string, ps PodStatusPayload) {
	s.mu.RLock()
	h := s.podStatusHandler
	s.mu.RUnlock()
	if h != nil {
		h(nodeID, ps)
	}
}

// notifyNodeEvent 在锁外安全地调用事件回调：先在锁内取回调快照，
// 再在锁外执行，保证回调执行期间不持有任何 CloudHub 锁（防止回调
// 反向调用 CloudHub 方法时死锁）。
func (s *Server) notifyNodeEvent(call func(NodeEvents)) {
	s.mu.RLock()
	h := s.nodeEvents
	s.mu.RUnlock()
	if h != nil {
		call(h)
	}
}

// NodeCount 返回当前已注册的节点数。
func (s *Server) NodeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.registry)
}

// ConnCount 返回当前活跃 WebSocket 连接数（含已连接未注册的节点），
// 供可观测性指标（edgeflow_cloudcore_active_connections）使用。
func (s *Server) ConnCount() int {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	return len(s.conns)
}

// NodeIDs 返回已注册节点 ID 列表（排序后，保证输出确定性）。
func (s *Server) NodeIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.registry))
	for id := range s.registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// NodeInfo 返回指定节点的注册信息；节点不存在时返回 false。
func (s *Server) NodeInfo(nodeID string) (NodeInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info, ok := s.nodes[nodeID]
	if !ok {
		return NodeInfo{}, false
	}
	return *info, true
}

// conn 是一条 WebSocket 连接的服务端视图。
//
// 并发安全说明：
//   - lastSeen/registered/dead 为原子变量，任意 goroutine 可读写
//   - nodeID 由注册流程写入（持有 s.mu），读取方同样在 s.mu 保护下进行
//   - 所有 WebSocket 写入经 writeMu 串行化（写循环与踢连接直写互斥）
//   - closed 通道 + closeOnce 保证只关闭一次
type conn struct {
	ws       *websocket.Conn
	remoteIP string
	// send 是发送缓冲 channel，写循环从中取消息写出。
	send chan []byte
	// closed 在连接关闭时关闭，用于唤醒写循环/心跳监控。
	closed chan struct{}
	// closeOnce 保证 close 只执行一次。
	closeOnce sync.Once
	// writeMu 串行化所有 WebSocket 写入。
	writeMu sync.Mutex
	// lastSeen 是最后一次收到消息的时间（毫秒时间戳）。
	lastSeen atomic.Int64
	// nodeID 是注册后分配的节点 ID（未注册为空串）。
	nodeID atomic.Value
	// registered 表示是否已完成注册。
	registered atomic.Bool
	// dead 表示已被新连接抢占，禁止再注册。
	dead atomic.Bool
	// compression 表示本连接是否已协商 gzip 压缩（WBS 4.4）：
	// 注册时由 handleRegister 写入（云端开关开启 && 边缘声明 gzip），
	// 决定下发编码走 EncodeCompressed 还是明文 Encode。
	compression atomic.Bool
}

// newConn 创建连接视图，并初始化 lastSeen 为当前时间。
func newConn(ws *websocket.Conn, remoteIP string) *conn {
	c := &conn{
		ws:       ws,
		remoteIP: remoteIP,
		send:     make(chan []byte, sendBufferSize),
		closed:   make(chan struct{}),
	}
	c.nodeID.Store("")
	c.lastSeen.Store(time.Now().UnixMilli())
	return c
}

// close 关闭连接：通知写循环/心跳监控退出，并关闭底层 WebSocket。
func (c *conn) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.ws.Close()
	})
}

// write 同步写出一条消息（带超时），调用方需保证不与其他写入并发（内部已加锁）。
func (c *conn) write(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

// trySend 非阻塞投递消息到发送缓冲：
//   - 缓冲未满：入队成功
//   - 连接已关闭：返回 false
//   - 缓冲已满：视为慢客户端，关闭连接并返回 false
func (c *conn) trySend(data []byte) bool {
	select {
	case <-c.closed:
		return false
	case c.send <- data:
		return true
	default:
		// 对端消费过慢，发送缓冲已满：丢弃连接，避免阻塞写方
		c.close()
		return false
	}
}

// remoteIP 从 "ip:port" 形式的地址中提取 IP。
func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
