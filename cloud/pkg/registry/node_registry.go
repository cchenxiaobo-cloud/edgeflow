// Package registry 实现云端节点注册表（内存态，WBS 2.3/2.6 的本地版）。
//
// 这是向"kubectl 可见边缘节点"推进的中间形态：当前以内存 Map 保存
// 节点元数据，通过 CloudHub 的节点事件（注册/心跳/断开）持续更新，
// 由 cloudcore 的 HTTP API（/api/v1/nodes）对外暴露查询。
//
// v0.4.0：本包在纯内存实现（Registry）之上新增 Store 接口与 etcd 写穿
// 包装（EtcdRegistry，见 etcd_registry.go）——内存 Map 保留为读写缓存，
// etcd 为持久化事实源，读写路径全部走内存缓存。集成层面向 Store 接口
// 装配，纯内存模式行为与 v0.3.x 完全一致。
//
// 待集群环境就绪后，本包将被 K8s apiserver 对接（Node 对象 + Informer）
// 取代，具体迁移点见文件末尾"后续对接 K8s apiserver"注释。
package registry

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	v1alpha1 "edgeflow/apis/edge/v1alpha1"
)

// NodeStatus 是节点状态（与 KubeEdge 语义对齐的简化版）。
type NodeStatus string

// 节点状态取值。
const (
	// StatusReady 表示节点在线且最近心跳正常。
	StatusReady NodeStatus = "Ready"
	// StatusUnknown 表示状态未知（预留：注册后尚未收到心跳等场景）。
	StatusUnknown NodeStatus = "Unknown"
	// StatusOffline 表示节点已断开连接（连接断开后保留元数据，仅标记离线）。
	StatusOffline NodeStatus = "Offline"
)

// NodeInfo 是节点的云端元数据快照，同时也是 HTTP API 的 JSON 形态。
//
// 时间戳统一为 Unix 毫秒（与心跳协议 payload 的毫秒约定一致），
// JSON 字段名与协议/API 约定保持一致（小驼峰）。
type NodeInfo struct {
	NodeID          string     `json:"nodeID"`          // 节点唯一 ID
	NodeName        string     `json:"nodeName"`        // 云端分配的节点名（M1 与 nodeID 一致）
	Arch            string     `json:"arch"`            // CPU 架构（如 arm64/amd64）
	OS              string     `json:"os"`              // 操作系统（如 linux）
	EdgecoreVersion string     `json:"edgecoreVersion"` // edgecore 版本号
	CPU             int        `json:"cpu"`             // CPU 核数
	Memory          uint64     `json:"memory"`          // 内存大小（字节）
	IP              string     `json:"ip"`              // 连接来源 IP
	RegisteredAt    int64      `json:"registeredAt"`    // 注册时间（毫秒时间戳）
	LastHeartbeatAt int64      `json:"lastHeartbeatAt"` // 最近心跳时间（毫秒时间戳）
	Status          NodeStatus `json:"status"`          // 节点状态（Ready/Unknown/Offline）
}

// Registry 是内存态节点注册表（读写缓存的实现基座）。
//
// 并发安全：所有方法内部加锁，可在任意 goroutine（CloudHub 连接处理、
// HTTP 请求处理）中并发调用。Get/List 返回值的拷贝，调用方修改不会
// 污染注册表内部状态。
//
// 节点保留策略：节点断开（MarkOffline）后元数据默认保留
// OfflineTTLDefault（24h），供查询 API 展示最近离线的节点；超过保留时长
// 的 Offline/Unknown 节点在后续任意写操作（Register/UpdateHeartbeat/
// MarkOffline/Seed）触发惰性 GC 时被移除，避免 nodes map 对"历史上连过
// 的一切 nodeID"只增不减（CODE-REVIEW-M1B P2-3）。Ready 节点即使有残留
// offlineSince 也不会被误删；保留时长可用 WithOfflineTTL 调整，传 0 禁用。
//
// GC 语义（v0.4.0 扩展）：被移除的 nodeID 记入 gcEvents，由 TakeGCEvents
// 取走；持久化包装（EtcdRegistry）据此同步删除 etcd 键并外露级联事件，
// 纯内存模式下取走即丢弃。Unknown 节点纳入 GC 范围仅为"启动清理"服务
// （Seed 播种后重建的节点均为 Unknown，见 Seed 注释）。
type Registry struct {
	mu           sync.RWMutex
	nodes        map[string]*NodeInfo // nodeID → 节点信息
	offlineSince map[string]int64     // nodeID → 标记离线的时间戳（毫秒）
	offlineTTL   time.Duration        // 节点保留时长；<=0 表示禁用 GC
	gcEvents     []string             // 被 GC 移除的 nodeID（待 TakeGCEvents 取走）
}

// Store 是节点注册表的存储接口（v0.4.0）：同一方法面由内存实现
// （Registry，读写缓存）与 etcd 写穿实现（EtcdRegistry）共同实现，
// 集成层面向本接口装配，未来接 K8s apiserver 时整体替换实现。
//
// 写路径方法（Register/UpdateHeartbeat/MarkOffline/Seed）返回 error：
// 内存实现除键校验外恒为 nil（Go 允许调用点忽略返回值，既有调用不变）；
// 读写路径（Get/List/Count/ToEdgeNode/ListEdgeNodes）只读内存缓存，不失败。
// Seed 用于启动加载播种；TakeGCEvents 外露被 GC 移除的 nodeID（排空语义）；
// Close 释放资源（纯内存实现为空操作）。
type Store interface {
	Register(info NodeInfo) error
	UpdateHeartbeat(nodeID string, ts int64) error
	MarkOffline(nodeID string) error
	Get(nodeID string) (NodeInfo, bool)
	List() []NodeInfo
	Count() int
	ToEdgeNode(info *NodeInfo) *v1alpha1.EdgeNode
	ListEdgeNodes() []v1alpha1.EdgeNode
	Seed(nodes []NodeInfo, since int64) error
	TakeGCEvents() []string
	// Load 从持久化后端全量加载并灌入内存缓存（etcd 实现：启动时调用一次；
	// 纯内存实现为空操作）。
	Load(ctx context.Context) error
	// CleanupLoop 定期清理（etcd 实现：内存 GC + 同步删 etcd 键，失败只告警
	// 不阻断；纯内存实现空转等待取消——GC 由写路径惰性触发 + NodeController
	// 心跳超时兜底）。
	CleanupLoop(ctx context.Context, interval time.Duration)
	Close() error
}

// 编译期断言：内存实现满足 Store 接口（锁住接口契约）。
var _ Store = (*Registry)(nil)

// nodeIDRe 是 nodeID 的合法性约束（v0.4.0 新增硬性约束）：nodeID 同时
// 用作 etcd 键路径段，含 '/' 会破坏前缀扫描与单键定位，故写入口统一拒绝。
// 现有边缘 nodeID 为 UUID/主机名形态，不受影响（设计文档 §2）。
var nodeIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validNodeID 报告 nodeID 是否满足键空间字符约束。
func validNodeID(id string) bool {
	return nodeIDRe.MatchString(id)
}

// OfflineTTLDefault 是 Offline 节点的默认保留时长（24h）：超过该时长未
// 重新上线的 Offline 节点会被惰性 GC 清理。
const OfflineTTLDefault = 24 * time.Hour

// Option 是 Registry 构造选项。
type Option func(*Registry)

// WithOfflineTTL 设置 Offline 节点的保留时长；ttl <= 0 表示禁用 TTL/GC
// （Offline 节点永久保留，等同旧行为）。
func WithOfflineTTL(ttl time.Duration) Option {
	return func(r *Registry) { r.offlineTTL = ttl }
}

// New 创建空注册表，默认启用 Offline TTL（OfflineTTLDefault）。
func New(opts ...Option) *Registry {
	r := &Registry{
		nodes:        make(map[string]*NodeInfo),
		offlineSince: make(map[string]int64),
		offlineTTL:   OfflineTTLDefault,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Register 登记节点（节点注册成功时由事件源调用）。
//
// 语义：
//   - 已存在同 nodeID 节点时更新元数据（重连场景），保留首次 RegisteredAt
//   - 状态置为 Ready，LastHeartbeatAt 置为当前时间（未显式指定时）
//   - nodeID 不满足键空间字符约束（含 '/' 等）时拒绝写入并返回 error
//     （v0.4.0 硬性约束，见 validNodeID）
func (r *Registry) Register(info NodeInfo) error {
	if !validNodeID(info.NodeID) {
		return fmt.Errorf("registry: 拒绝非法 nodeID %q（允许 [A-Za-z0-9._-]）", info.NodeID)
	}
	now := time.Now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.nodes[info.NodeID]; ok && old.RegisteredAt > 0 {
		// 重连不重置注册时间，保留首次注册时刻
		info.RegisteredAt = old.RegisteredAt
	} else if info.RegisteredAt == 0 {
		info.RegisteredAt = now
	}
	if info.LastHeartbeatAt == 0 {
		info.LastHeartbeatAt = now
	}
	info.Status = StatusReady
	r.nodes[info.NodeID] = &info
	// 重新上线：清除离线标记，避免残留的 offlineSince 在后续 GC 中被误判
	delete(r.offlineSince, info.NodeID)
	// 惰性 GC：写入路径顺带清理过期 Offline/Unknown 节点（新节点状态为 Ready，不受影响）
	r.gcLocked(now)
	return nil
}

// UpdateHeartbeat 刷新节点心跳时间并把状态置回 Ready；节点不存在时忽略。
// ts 为毫秒时间戳，传 0 表示用当前时间。心跳是瞬态（不落盘），内存实现
// 恒返回 nil。
func (r *Registry) UpdateHeartbeat(nodeID string, ts int64) error {
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	now := time.Now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok {
		n.LastHeartbeatAt = ts
		n.Status = StatusReady
		// 恢复在线：清除离线标记
		delete(r.offlineSince, nodeID)
	}
	r.gcLocked(now)
	return nil
}

// MarkOffline 标记节点离线（连接断开时由事件源调用），保留元数据供查询；
// 节点不存在时忽略。离线时刻被记录用于 TTL/GC。离线标记是瞬态（不落盘），
// 内存实现恒返回 nil。
//
// 决策：仅当节点首次从非离线状态转为离线时才记录 offlineSince——
// 已处于 Offline 的节点重复收到 MarkOffline（如心跳超时扫描与连接断开
// 双路径先后触发）不刷新离线时钟，TTL 自首次离线起算（复核 M1）。
func (r *Registry) MarkOffline(nodeID string) error {
	now := time.Now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok {
		n.Status = StatusOffline
		if _, existed := r.offlineSince[nodeID]; !existed {
			r.offlineSince[nodeID] = now
		}
	}
	r.gcLocked(now)
	return nil
}

// Seed 播种节点（启动加载用，v0.4.0）：节点以 Unknown 状态进入注册表
// （心跳/状态不落盘，重启后不显示陈旧 Ready），LastHeartbeatAt 清零；
// 全部节点的 offlineSince 以 since（启动时刻）起算——超过保留期的
// Unknown 节点在随即执行的 GC 扫描中被移除，这是"云边同死、节点永不
// 回归"场景的兜底（设计文档 §3.2/§5）。since <= 0 时取当前时间；
// 任一 nodeID 非法则整体拒绝（校验先行，不产生部分播种）。
func (r *Registry) Seed(nodes []NodeInfo, since int64) error {
	now := time.Now().UnixMilli()
	if since <= 0 {
		since = now
	}
	for _, n := range nodes {
		if !validNodeID(n.NodeID) {
			return fmt.Errorf("registry: Seed 拒绝非法 nodeID %q（允许 [A-Za-z0-9._-]）", n.NodeID)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range nodes {
		n.Status = StatusUnknown
		n.LastHeartbeatAt = 0
		if n.RegisteredAt <= 0 {
			n.RegisteredAt = since
		}
		r.nodes[n.NodeID] = &n
		r.offlineSince[n.NodeID] = since
	}
	r.gcLocked(now)
	return nil
}

// TakeGCEvents 取走并清空被 GC 移除的 nodeID 列表（排空语义）。
// 纯内存模式下取走即丢弃；持久化包装（EtcdRegistry）据此同步删除
// 对应 etcd 键，并向集成层外露级联事件（如级联删设备子树）。
// Load 纯内存实现：无可加载的持久化后端，空操作。
func (r *Registry) Load(ctx context.Context) error { return nil }

// CleanupLoop 纯内存实现：无独立清理循环（惰性 GC + NodeController 兜底），
// 仅空转等待取消信号，保持与 etcd 实现一致的调用方语义。
func (r *Registry) CleanupLoop(ctx context.Context, _ time.Duration) {
	<-ctx.Done()
}

func (r *Registry) TakeGCEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.gcEvents) == 0 {
		return nil
	}
	out := append([]string(nil), r.gcEvents...)
	r.gcEvents = r.gcEvents[:0]
	return out
}

// requeueGCEvent 把 GC 事件重新入队（持久化包装在 etcd 删除失败时调用，
// 下一轮清理重试；调用方需确认该节点确实已被移除）。
func (r *Registry) requeueGCEvent(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcEvents = append(r.gcEvents, nodeID)
}

// Close 释放资源：纯内存实现无可关闭资源，恒返回 nil（接口契约占位）。
func (r *Registry) Close() error {
	return nil
}

// gcLocked 惰性清理：删除保留时长 >= offlineTTL 的节点（调用方须已持写锁）。
// 处理 StatusOffline 与 StatusUnknown 节点（Unknown 仅出现在 Seed 播种后，
// 启动清理依赖此范围），Ready 节点即使 offlineSince 有残留也不受影响
// （防御：offlineSince 缺失的 Offline/Unknown 节点同样跳过，避免误删）。
// 被移除的 nodeID 记入 gcEvents（TakeGCEvents 取走）。ttl <= 0 表示禁用，
// 直接返回。
func (r *Registry) gcLocked(now int64) {
	if r.offlineTTL <= 0 {
		return
	}
	ttlMs := r.offlineTTL.Milliseconds()
	for id, n := range r.nodes {
		if n.Status != StatusOffline && n.Status != StatusUnknown {
			continue
		}
		since, ok := r.offlineSince[id]
		if !ok {
			continue // 无保留期记录：跳过，不猜删
		}
		if now-since >= ttlMs {
			delete(r.nodes, id)
			delete(r.offlineSince, id)
			r.gcEvents = append(r.gcEvents, id)
		}
	}
}

// Get 返回指定节点的信息拷贝；节点不存在时返回 false。
func (r *Registry) Get(nodeID string) (NodeInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[nodeID]
	if !ok {
		return NodeInfo{}, false
	}
	return *n, true
}

// List 返回全部节点信息的拷贝，按 NodeID 排序保证输出确定性。
func (r *Registry) List() []NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]NodeInfo, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// Count 返回注册表内节点数（含 Offline 节点）。
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.nodes)
}

// 后续对接 K8s apiserver 的迁移点：
//   - 本包内对 r.nodes 的读写替换为 informer 缓存（Node 对象 + Watch 回调）
//   - Register / UpdateHeartbeat / MarkOffline 对应 Node 的 Add / Update 事件
//   - Get / List 改为从 informer lister 读取；NodeInfo 的 JSON 字段与 K8s
//     Node 的 status.conditions / addresses 映射即可，HTTP API 层无需改动
