// Package nodecontroller 实现云端节点心跳静默超时管理（WBS 2.4）。
//
// 职责边界（与 CloudHub 断开事件互补，不重复）：
//   - CloudHub 负责"连接级"失活：默认 90s 无任何消息即断开连接，
//     触发 OnNodeDisconnected 事件 → 注册表 MarkOffline。覆盖
//     "连接断了"的常规场景。
//   - NodeController 负责"心跳级"静默超时：定时扫描注册表，用
//     LastHeartbeatAt 判定节点心跳是否停滞——连接还活着但心跳异常、
//     或连接断开但事件未触发（事件丢失/回调异常）时，由云端定时
//     扫描兜底判 Offline。与 KubeEdge NodeController 职责对齐。
//
// 状态机（复用 registry 现有逻辑，不重构）：
//
//	Register/UpdateHeartbeat → Ready
//	MarkOffline（断开事件或本控制器扫描）→ Offline
//	Offline 节点重新心跳（UpdateHeartbeat）→ Ready（自动恢复）
//
// 本包只做"扫描 + 判定 + 标记"，状态转移全部走 registry 公开方法，
// 与 CloudHub 事件回调并发安全（registry 内部加锁）。
package nodecontroller

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/log"
)

// 环境变量名（cmd/cloudcore 装配时读取，覆盖默认值）。
const (
	// EnvScanInterval 覆盖扫描周期，如 "10s" 或 "10"（秒）。
	EnvScanInterval = "EDGEFLOW_CLOUDCORE_NODE_SCAN_INTERVAL"
	// EnvTimeout 覆盖心跳超时阈值，如 "3m" 或 "180"（秒）。
	EnvTimeout = "EDGEFLOW_CLOUDCORE_NODE_TIMEOUT"
)

// 默认配置。
const (
	// DefaultScanInterval 是默认扫描周期：30s。
	// 依据：CloudHub 连接失活阈值 90s、边侧心跳周期 30s；
	// 30s 扫描可在 1~2 个周期内感知 90s 级的心跳停滞，扫描开销可忽略。
	DefaultScanInterval = 30 * time.Second
	// DefaultTimeout 是默认心跳超时阈值：180s（约 6 个心跳周期）。
	// 依据：大于 CloudHub 的 90s 连接失活阈值——常规断开先由断开事件
	// 判 Offline，本控制器只兜底"静默停滞/事件丢失"，避免慢网络误伤。
	DefaultTimeout = 180 * time.Second
)

// Option 是 NodeController 的配置项（函数式选项）。
type Option func(*NodeController)

// WithInterval 设置扫描周期（默认 30s）。
func WithInterval(d time.Duration) Option {
	return func(c *NodeController) { c.interval = d }
}

// WithTimeout 设置心跳超时阈值（默认 180s）。
func WithTimeout(d time.Duration) Option {
	return func(c *NodeController) { c.timeout = d }
}

// WithNow 注入时钟（默认 time.Now，测试用）：
// scanOnce 用它计算"当前时间"，测试可注入假时钟模拟时间流逝，
// 避免真实等待超时阈值（详见 node_controller_test.go 说明）。
func WithNow(f func() time.Time) Option {
	return func(c *NodeController) { c.now = f }
}

// NodeController 定时扫描节点注册表，把心跳停滞的节点标记为 Offline。
//
// 并发安全：scanOnce 只调用 registry 的并发安全方法（List/MarkOffline），
// 可与 CloudHub 事件回调（注册/心跳/断开）并行运行；Start/Stop 的
// 状态与 channel 由内部互斥锁保护。
type NodeController struct {
	reg      *registry.Registry
	interval time.Duration    // 扫描周期
	timeout  time.Duration    // 心跳超时阈值
	now      func() time.Time // 可注入时钟

	mu      sync.Mutex
	started bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

// New 创建节点控制器，opts 可覆盖周期/阈值/时钟。
func New(reg *registry.Registry, opts ...Option) *NodeController {
	c := &NodeController{
		reg:      reg,
		interval: DefaultScanInterval,
		timeout:  DefaultTimeout,
		now:      time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Start 启动后台扫描循环（幂等：重复调用不重复启动）。
func (c *NodeController) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return
	}
	c.started = true
	c.stopCh = make(chan struct{})
	c.doneCh = make(chan struct{})
	go c.loop()
	log.Infof("[NodeController] 心跳超时扫描已启动（interval=%v, timeout=%v）", c.interval, c.timeout)
}

// Stop 停止扫描并等待循环退出（幂等）。
func (c *NodeController) Stop() {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return
	}
	c.started = false
	close(c.stopCh)
	c.mu.Unlock()
	<-c.doneCh
	log.Infof("[NodeController] 心跳超时扫描已停止")
}

// loop 是后台扫描循环：按 interval 周期执行 scanOnce，直到 Stop。
func (c *NodeController) loop() {
	defer close(c.doneCh)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.scanOnce()
		case <-c.stopCh:
			return
		}
	}
}

// scanOnce 执行一轮扫描：把 LastHeartbeatAt 距今超过 timeout 且
// 尚未 Offline 的节点标记为 Offline。
//
// 判定规则（注意单位）：LastHeartbeatAt 是毫秒时间戳，换算为
// time.Time 后与注入时钟比较，避免毫秒/纳秒单位混淆；
// 已 Offline 的节点跳过（状态机不允许重复标记，也不重复告警日志）。
func (c *NodeController) scanOnce() {
	now := c.now()
	for _, info := range c.reg.List() {
		// 已 Offline：跳过（不重复打日志）
		if info.Status == registry.StatusOffline {
			continue
		}
		// LastHeartbeatAt 为 0 说明从未心跳（防御性兜底）：
		// 按"未知"处理，不按超时误判
		if info.LastHeartbeatAt <= 0 {
			continue
		}
		last := time.UnixMilli(info.LastHeartbeatAt)
		if now.Sub(last) >= c.timeout {
			c.reg.MarkOffline(info.NodeID)
			log.Warnf("[NodeController] 节点 %s 心跳超时（lastHeartbeat=%v, timeout=%v），标记 Offline",
				info.NodeID, last.Format(time.RFC3339), c.timeout)
		}
	}
}

// DurationsFromEnv 读取扫描周期与超时阈值：
//   - 环境变量 EDGEFLOW_CLOUDCORE_NODE_SCAN_INTERVAL / EDGEFLOW_CLOUDCORE_NODE_TIMEOUT
//     支持 Go duration（"15s"、"1m30s"）或纯秒数（"15"）
//   - 未设置时用默认值（30s/180s）
//   - 非法值返回错误（与 CloudHub 端口环境变量同约定：装配期报错，不静默回退）
func DurationsFromEnv() (interval, timeout time.Duration, err error) {
	interval, timeout = DefaultScanInterval, DefaultTimeout
	if v := os.Getenv(EnvScanInterval); v != "" {
		interval, err = parseDurationEnv(EnvScanInterval, v)
		if err != nil {
			return 0, 0, err
		}
	}
	if v := os.Getenv(EnvTimeout); v != "" {
		timeout, err = parseDurationEnv(EnvTimeout, v)
		if err != nil {
			return 0, 0, err
		}
	}
	return interval, timeout, nil
}

// parseDurationEnv 解析时长环境变量：优先 Go duration，回退纯秒数；
// 非法或非正值返回错误。
func parseDurationEnv(name, v string) (time.Duration, error) {
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("环境变量 %s=%q 必须为正时长", name, v)
		}
		return d, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("环境变量 %s=%q 不是合法时长（支持 Go duration 如 \"15s\" 或秒数）", name, v)
}
