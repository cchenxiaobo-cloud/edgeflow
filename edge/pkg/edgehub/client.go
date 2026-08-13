// Package edgehub 实现 EdgeCore 的云边通信客户端（WBS 3.1）。
//
// 对标 KubeEdge 的 EdgeHub：作为边缘侧与云端 CloudHub 的唯一 WebSocket
// 通道，负责连接建立、节点注册、心跳保活与断线重连，并把云端下发的
// 非应答类消息（如未来的 PodSync）分发给上层模块（MetaManager/Edged）。
//
// 与 CloudHub 的通道契约（WBS 4.1-4.6）：
//   - 通道：WebSocket，路径 /v1/edge，端口 10000（云端地址可配置）
//   - 边缘主动连接云端；连接建立后第一条消息是 Register
//   - 心跳：每 30s 发 Heartbeat，云回 HeartbeatAck
//   - 断线重连：指数退避 1s/2s/4s/…上限 60s，注册成功后重置退避
package edgehub

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/version"

	"github.com/gorilla/websocket"
)

// 通道契约常量。
const (
	// EnvNodeID 指定边缘节点 ID 的环境变量。
	EnvNodeID = "EDGEFLOW_EDGECORE_NODE_ID"
	// EnvCloudAddr 指定云端 CloudHub 地址的环境变量。
	EnvCloudAddr = "EDGEFLOW_EDGECORE_CLOUD_ADDR"

	// DefaultCloudAddr 是云端地址的默认值（本机开发/单机部署）。
	DefaultCloudAddr = "ws://127.0.0.1:10000"
	// channelPath 是云边 WebSocket 通道路径（与 CloudHub 契约一致）。
	channelPath = "/v1/edge"
	// targetCloud 是消息 Target 的固定值（契约：云侧标识为 "cloud"）。
	targetCloud = "cloud"

	// DefaultHeartbeatInterval 是心跳发送间隔（契约 30s）。
	DefaultHeartbeatInterval = 30 * time.Second
	// DefaultRegisterTimeout 是发出 Register 后等待 RegisterAck 的超时。
	DefaultRegisterTimeout = 10 * time.Second
	// DefaultBackoffBase 是重连指数退避的初始间隔（契约 1s）。
	DefaultBackoffBase = 1 * time.Second
	// DefaultBackoffMax 是重连退避的上限（契约 60s）。
	DefaultBackoffMax = 60 * time.Second
	// DefaultWriteTimeout 是单次 WebSocket 写超时（防止写死连接时永久阻塞）。
	DefaultWriteTimeout = 10 * time.Second

	// maxReadBytes 是单条入站消息的大小上限（1 MiB，与 CloudHub 的
	// maxMessageBytes 对称）：云端被攻破/异常时防止超大消息灌入导致内存膨胀。
	maxReadBytes = 1 << 20
)

// DefaultNodeID 解析默认节点 ID，优先级：
//  1. 环境变量 EDGEFLOW_EDGECORE_NODE_ID；
//  2. "edge-" + 主机名（与 KubeEdge 的 nodeID 命名习惯一致）；
//  3. 兜底 "edge-local"（主机名获取失败时）。
func DefaultNodeID() string {
	if v := os.Getenv(EnvNodeID); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return "edge-" + h
	}
	return "edge-local"
}

// DefaultCloudAddrFromEnv 解析云端地址：环境变量 EDGEFLOW_EDGECORE_CLOUD_ADDR
// 优先，缺省本机 10000 端口（通道路径 /v1/edge 由 New 自动补全）。
func DefaultCloudAddrFromEnv() string {
	if v := os.Getenv(EnvCloudAddr); v != "" {
		return v
	}
	return DefaultCloudAddr
}

// ensureChannelPath 确保云端地址包含通道路径 /v1/edge：
// 地址未显式携带路径时自动补全（契约：通道路径固定为 /v1/edge）；
// 显式给出路径（如经网关转发）时保持原样。非法地址原样返回，
// 交给拨号器报错。
func ensureChannelPath(addr string) string {
	u, err := url.Parse(addr)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return addr
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = channelPath
	}
	return u.String()
}

// RegisterPayload 是 Register 消息的负载（字段名与 CloudHub 契约一致，不可改）。
type RegisterPayload struct {
	NodeID          string `json:"nodeID"`
	Arch            string `json:"arch"`
	OS              string `json:"os"`
	EdgeCoreVersion string `json:"edgecoreVersion"`
	CPU             int    `json:"cpu"`
	Memory          uint64 `json:"memory"` // 单位：字节
}

// RegisterAckPayload 是 RegisterAck 消息的负载（与 CloudHub 契约一致）。
type RegisterAckPayload struct {
	Accepted bool   `json:"accepted"`
	NodeName string `json:"nodeName"`
	Message  string `json:"message"`
}

// HeartbeatPayload 是 Heartbeat 消息的负载（与 CloudHub 契约一致）。
type HeartbeatPayload struct {
	Timestamp int64 `json:"timestamp"` // 毫秒时间戳
}

// HeartbeatAckPayload 是 HeartbeatAck 消息的负载（与 CloudHub 契约一致）。
type HeartbeatAckPayload struct {
	NodeStatus string `json:"nodeStatus"`
}

// Dialer 抽象 WebSocket 拨号能力：*websocket.Dialer 天然满足，
// 抽象出来便于测试注入（计数/伪造拨号）与将来替换传输层。
type Dialer interface {
	Dial(urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error)
}

// Options 是 EdgeHub 客户端的配置项；零值字段由 New 填充默认值。
type Options struct {
	// CloudAddr 是云端 CloudHub 地址，如 ws://127.0.0.1:10000（缺省自动补 /v1/edge）。
	CloudAddr string
	// NodeID 是边缘节点 ID，注册与消息 Source 使用。
	NodeID string
	// HeartbeatInterval 是心跳发送间隔。
	HeartbeatInterval time.Duration
	// RegisterTimeout 是发出 Register 后等待 RegisterAck 的超时。
	RegisterTimeout time.Duration
	// BackoffBase 是重连指数退避的初始间隔。
	BackoffBase time.Duration
	// BackoffMax 是重连退避上限。
	BackoffMax time.Duration
	// Dialer 是自定义拨号器（测试注入用）；nil 时使用内置默认拨号器。
	Dialer Dialer
}

// Client 是 EdgeHub WebSocket 客户端。
//
// 并发模型（gorilla/websocket 不允许并发写，必须串行化）：
//   - 一个 run goroutine 负责"连接→注册→心跳→退避重连"主循环；
//   - 每个连接一个 readLoop goroutine 负责读消息与消息分发；
//   - 所有写操作经 writeMu 串行化。
type Client struct {
	opts Options

	mu        sync.Mutex
	conn      *websocket.Conn // 当前连接（可能为 nil）
	connected bool            // 是否已连接且注册成功
	nodeName  string          // 云端在 RegisterAck 中分配的节点名
	lastAck   time.Time       // 最近一次收到 HeartbeatAck 的时间

	statusHandler func(connected bool)              // 连接状态回调（状态切换时触发）
	msgHandler    func(msg *protocol.Message) error // 非应答类消息回调（可返回错误，供自动 Ack 用）

	// 幂等去重缓存（自动 Ack 用）：记录已成功处理的消息 ID，
	// 云端重发同 ID 时直接回 Ack 不重复执行（WBS 4.6 QoS 1）。
	procMu     sync.Mutex
	processed  map[string]struct{} // ID → 已处理标记
	processedQ []string            // FIFO 顺序，超上限时淘汰最旧

	// downlinkMu 串行化 handleDownlink 的「检查→执行→标记」全流程（P2-4）：
	// 重连窗口内两条连接的 readLoop 可能短暂并存，防止同 ID 消息并发执行。
	downlinkMu sync.Mutex

	writeMu sync.Mutex // 串行化所有 WebSocket 写

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup // run goroutine
	connWG   sync.WaitGroup // 各连接的 readLoop goroutine
}

// New 创建 EdgeHub 客户端，零值 Options 字段用默认值补齐。
func New(opts Options) *Client {
	if opts.CloudAddr == "" {
		opts.CloudAddr = DefaultCloudAddrFromEnv()
	}
	opts.CloudAddr = ensureChannelPath(opts.CloudAddr)
	if opts.NodeID == "" {
		opts.NodeID = DefaultNodeID()
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.RegisterTimeout <= 0 {
		opts.RegisterTimeout = DefaultRegisterTimeout
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = DefaultBackoffBase
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = DefaultBackoffMax
	}
	if opts.BackoffBase > opts.BackoffMax {
		opts.BackoffBase = opts.BackoffMax // 初始间隔不超过上限
	}
	if opts.Dialer == nil {
		// 默认拨号器：握手超时 10s，避免对不可达地址无限阻塞
		opts.Dialer = &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	}
	return &Client{
		opts:   opts,
		stopCh: make(chan struct{}),
	}
}

// IsConnected 返回是否已连接且注册成功（供 MetaManager/Edged 订阅连接状态）。
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Address 返回实际连接的云端地址（含自动补全的通道路径），供日志展示。
func (c *Client) Address() string {
	return c.opts.CloudAddr
}

// NodeName 返回云端在 RegisterAck 中分配的节点名；未注册成功时为空。
func (c *Client) NodeName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nodeName
}

// LastHeartbeatAckTime 返回最近一次收到 HeartbeatAck 的时间（测试与诊断用）。
// 从未收到过时返回零值 time.Time。
func (c *Client) LastHeartbeatAckTime() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastAck
}

// SetStatusHandler 注册连接状态回调：connected=true 表示"已连接且注册成功"。
// 回调在内部 goroutine 中调用，应快速返回、不要阻塞。
func (c *Client) SetStatusHandler(h func(connected bool)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statusHandler = h
}

// SetMessageHandler 注册消息回调：收到下发类消息（非 Ack 且非注册/心跳类，
// 如 PodSync/ConfigSync/DeviceCommand）时回调，处理完成后 EdgeHub 自动回
// Ack（code=ok）。回调在内部读 goroutine 中调用，上层不应阻塞。
//
// 兼容旧签名：回调不返回错误；需要报告处理失败（回 Ack code=error）时
// 请用 SetMessageHandlerFunc。
func (c *Client) SetMessageHandler(h func(msg *protocol.Message)) {
	c.SetMessageHandlerFunc(func(msg *protocol.Message) error {
		if h != nil {
			h(msg)
		}
		return nil
	})
}

// SetMessageHandlerFunc 注册可返回错误的消息回调（推荐用法）：回调返回
// nil → 自动回 Ack code=ok；返回 error → 自动回 Ack code=error（云端可据
// 此重试，见 WBS 4.6 可靠投递契约）。回调在内部读 goroutine 中调用，
// 上层不应阻塞（需要异步处理时自行拷贝数据）。
func (c *Client) SetMessageHandlerFunc(h func(msg *protocol.Message) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgHandler = h
}

// Send 向云端发送一条消息（并发安全，供上层与自动 Ack 使用）。
// 内部与心跳/注册共用 writeMu 串行化写；当前无活动连接
// （未连接/已断开）时返回错误。
func (c *Client) Send(msg *protocol.Message) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("未连接到云端")
	}
	return c.write(conn, msg)
}

// Start 启动客户端：立即开始连接，随后在后台持续运行
// （连接→注册→心跳→断线重连），直到 Stop 被调用。
func (c *Client) Start() {
	c.wg.Add(1)
	go c.run()
}

// Stop 优雅关闭：停止重连与心跳，关闭当前连接，等待内部 goroutine 全部退出。
func (c *Client) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
		c.closeConn() // 唤醒阻塞中的读写 goroutine
	})
	c.wg.Wait()     // run 退出后不会再启动新的 readLoop
	c.connWG.Wait() // 等待所有 readLoop 退出
}

// run 是主循环：连接→注册→（心跳会话）→断线→退避→重连。
func (c *Client) run() {
	defer c.wg.Done()
	backoff := c.opts.BackoffBase
	for {
		if c.isStopped() {
			return
		}
		conn, closed, err := c.connectAndRegister()
		if err != nil {
			// 连接失败或注册失败：退避后重试
			log.Errorf("EdgeHub 连接/注册失败（%s）: %v", c.opts.CloudAddr, err)
			if !c.sleep(backoff) {
				return
			}
			backoff = nextBackoff(backoff, c.opts.BackoffMax)
			continue
		}
		// 注册成功：退避重置（契约：连接成功并完成注册后重置）
		backoff = c.opts.BackoffBase
		log.Infof("EdgeHub 已连接并注册到云端 %s（nodeID=%s, nodeName=%s）",
			c.opts.CloudAddr, c.opts.NodeID, c.NodeName())

		// 会话阶段：心跳保活，直到连接断开或收到停止信号
		err = c.session(conn, closed)
		// 无论何种原因退出会话（断线/停止），都先清理连接状态；
		// 断开期间 Send 应报“未连接”而不是写入已关闭的旧连接
		c.setConnected(false)
		_ = conn.Close()
		c.setConn(nil)
		if c.isStopped() {
			return
		}
		log.Warnf("EdgeHub 连接断开（%v），等待退避后重连", err)
		if !c.sleep(backoff) {
			return
		}
		backoff = nextBackoff(backoff, c.opts.BackoffMax)
	}
}

// connectAndRegister 建立 WebSocket 连接并完成注册。
// 返回当前连接与"连接已关闭"信号；连接失败/注册失败（含被拒绝、超时）
// 时关闭连接并返回错误。
func (c *Client) connectAndRegister() (*websocket.Conn, <-chan struct{}, error) {
	conn, _, err := c.opts.Dialer.Dial(c.opts.CloudAddr, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("拨号 %s 失败: %w", c.opts.CloudAddr, err)
	}
	// 与云端对称的入站消息大小限制（1 MiB）：
	// 超过限制的消息会触发读错误并断开连接，而不是被完整读入内存。
	conn.SetReadLimit(maxReadBytes)

	// 每个连接独立的信号通道：注册应答（缓冲 1，防阻塞）、连接错误、关闭通知
	closed := make(chan struct{})
	regAckCh := make(chan *protocol.Message, 1)
	connErrCh := make(chan error, 1)
	c.setConn(conn)
	c.connWG.Add(1)
	go c.readLoop(conn, closed, regAckCh, connErrCh)

	if err := c.register(conn, closed, regAckCh, connErrCh); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	c.setConnected(true)
	return conn, closed, nil
}

// register 发送 Register 消息并等待 RegisterAck。
//
// 决策：accepted=false 时记录错误并视为本次注册失败，由主循环按退避重试
// （不针对 message 提示做特殊分支——M1 阶段云端拒绝的原因如重复节点、
// 版本不兼容等统一走重试路径，后续如需定向处理可在上层扩展）。
func (c *Client) register(conn *websocket.Conn, closed <-chan struct{}, regAckCh <-chan *protocol.Message, connErrCh <-chan error) error {
	payload := RegisterPayload{
		NodeID:          c.opts.NodeID,
		Arch:            runtime.GOARCH,
		OS:              runtime.GOOS,
		EdgeCoreVersion: version.Get().String(),
		CPU:             runtime.NumCPU(),
		Memory:          totalMemoryBytes(),
	}
	msg, err := protocol.NewMessage(protocol.TypeRegister, c.opts.NodeID, targetCloud, payload)
	if err != nil {
		return fmt.Errorf("构造 Register 消息失败: %w", err)
	}
	if err := c.write(conn, msg); err != nil {
		return fmt.Errorf("发送 Register 失败: %w", err)
	}

	select {
	case ack := <-regAckCh:
		var ackPayload RegisterAckPayload
		if err := ack.DecodePayload(&ackPayload); err != nil {
			return fmt.Errorf("解析 RegisterAck 负载失败: %w", err)
		}
		if !ackPayload.Accepted {
			return fmt.Errorf("注册被云端拒绝（nodeName=%q, message=%q）", ackPayload.NodeName, ackPayload.Message)
		}
		if ackPayload.NodeName == "" {
			ackPayload.NodeName = c.opts.NodeID // 云端未分配时回落为 nodeID
		}
		c.mu.Lock()
		c.nodeName = ackPayload.NodeName
		c.mu.Unlock()
		return nil
	case err := <-connErrCh:
		return fmt.Errorf("等待 RegisterAck 期间连接出错: %w", err)
	case <-closed:
		return errors.New("等待 RegisterAck 期间连接关闭")
	case <-time.After(c.opts.RegisterTimeout):
		return errors.New("等待 RegisterAck 超时")
	case <-c.stopCh:
		return errors.New("客户端已停止")
	}
}

// readLoop 持续读消息直到连接关闭或出错，并做消息分发：
//   - RegisterAck → 投递给注册等待方（regAckCh）；
//   - HeartbeatAck → 更新 lastAck；
//   - Ack → 云端对边侧上报消息的确认，暂不处理（后续可靠上报用）；
//   - 其余类型（PodSync 等）→ 自动 Ack + 幂等去重后回调 msgHandler。
//
// 退出时关闭 closed 并上报错误（若有），让主循环感知断线。
func (c *Client) readLoop(conn *websocket.Conn, closed chan struct{}, regAckCh chan<- *protocol.Message, connErrCh chan<- error) {
	defer c.connWG.Done()
	defer close(closed)
	for {
		// 读超时保护：心跳应答间隔远小于该值，正常连接不会被误杀；
		// 一旦静默半开连接（对端无任何消息），读会在此超时报错触发重连。
		_ = conn.SetReadDeadline(time.Now().Add(3 * c.opts.HeartbeatInterval))
		_, data, err := conn.ReadMessage()
		if err != nil {
			select {
			case connErrCh <- err:
			default:
			}
			return
		}
		msg, err := protocol.Decode(data)
		if err != nil {
			log.Warnf("EdgeHub 收到无法解析的消息: %v", err)
			continue
		}
		switch msg.Type {
		case protocol.TypeRegisterAck:
			// 缓冲 1：注册等待方未就绪（如超时后迟到的应答）时直接丢弃，不阻塞读循环
			select {
			case regAckCh <- msg:
			default:
			}
		case protocol.TypeHeartbeatAck:
			c.mu.Lock()
			c.lastAck = time.Now()
			c.mu.Unlock()
		case protocol.TypeAck:
			// 云端对边侧上报消息（PodStatus 等）的确认；可靠上报在 M3 实现，
			// 当前仅记录日志，不参与自动 Ack（避免 Ack 套 Ack）
			log.Infof("EdgeHub 收到云端 Ack（correlationID=%s）", msg.CorrelationID)
		default:
			// 下发类消息：自动 Ack + 幂等去重后分发给上层（WBS 4.6）
			c.handleDownlink(msg)
		}
	}
}

// session 在连接存活期间驱动心跳，直到连接断开或收到停止信号。
func (c *Client) session(conn *websocket.Conn, closed <-chan struct{}) error {
	ticker := time.NewTicker(c.opts.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return nil
		case <-closed:
			return errors.New("连接已关闭")
		case <-ticker.C:
			if err := c.sendHeartbeat(conn); err != nil {
				return fmt.Errorf("发送心跳失败: %w", err)
			}
		}
	}
}

// sendHeartbeat 发送一条 Heartbeat 消息（负载带毫秒时间戳，与契约一致）。
func (c *Client) sendHeartbeat(conn *websocket.Conn) error {
	msg, err := protocol.NewMessage(protocol.TypeHeartbeat, c.opts.NodeID, targetCloud, HeartbeatPayload{
		Timestamp: time.Now().UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("构造心跳消息失败: %w", err)
	}
	return c.write(conn, msg)
}

// write 串行化地写一条消息（gorilla/websocket 不允许并发写）。
func (c *Client) write(conn *websocket.Conn, msg *protocol.Message) error {
	data, err := protocol.Encode(msg)
	if err != nil {
		return fmt.Errorf("编码消息失败: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(DefaultWriteTimeout))
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("写消息失败: %w", err)
	}
	return nil
}

// nextBackoff 计算下一次退避间隔：当前值翻倍，封顶 max。
// 默认参数下序列为 1s,2s,4s,8s,16s,32s,60s,60s,...（契约）。
func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		next = max
	}
	return next
}

// sleep 休眠 d 时长；收到停止信号时立即返回 false（调用方应退出）。
func (c *Client) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-c.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

// isStopped 返回是否已收到停止信号。
func (c *Client) isStopped() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// setConn 记录当前连接。
func (c *Client) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

// closeConn 关闭当前连接（nil 安全；gorilla/websocket 允许并发 Close）。
func (c *Client) closeConn() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// setConnected 更新连接状态；状态发生变化时在锁外通知回调，避免死锁。
func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	changed := c.connected != v
	c.connected = v
	h := c.statusHandler
	c.mu.Unlock()
	if changed && h != nil {
		h(v)
	}
}

// totalMemoryBytes 尽力获取节点总内存（字节）。M1 仅在 Linux 上采集
// /proc/meminfo；其余平台返回 0（边缘节点目标平台为 Linux，后续可扩展，
// 不影响注册流程）。
func totalMemoryBytes() uint64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				return kb * 1024 // MemTotal 单位是 kB
			}
		}
		return 0
	}
	return 0
}
