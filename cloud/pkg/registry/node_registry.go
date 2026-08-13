// Package registry 实现云端节点注册表（内存态，WBS 2.3/2.6 的本地版）。
//
// 这是向"kubectl 可见边缘节点"推进的中间形态：当前以内存 Map 保存
// 节点元数据，通过 CloudHub 的节点事件（注册/心跳/断开）持续更新，
// 由 cloudcore 的 HTTP API（/api/v1/nodes）对外暴露查询。
// 待集群环境就绪后，本包将被 K8s apiserver 对接（Node 对象 + Informer）
// 取代，具体迁移点见文件末尾"后续对接 K8s apiserver"注释。
package registry

import (
	"sort"
	"sync"
	"time"
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

// Registry 是内存态节点注册表。
//
// 并发安全：所有方法内部加锁，可在任意 goroutine（CloudHub 连接处理、
// HTTP 请求处理）中并发调用。Get/List 返回值的拷贝，调用方修改不会
// 污染注册表内部状态。
type Registry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeInfo // nodeID → 节点信息
}

// New 创建空注册表。
func New() *Registry {
	return &Registry{nodes: make(map[string]*NodeInfo)}
}

// Register 登记节点（节点注册成功时由事件源调用）。
//
// 语义：
//   - 已存在同 nodeID 节点时更新元数据（重连场景），保留首次 RegisteredAt
//   - 状态置为 Ready，LastHeartbeatAt 置为当前时间（未显式指定时）
func (r *Registry) Register(info NodeInfo) {
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
}

// UpdateHeartbeat 刷新节点心跳时间并把状态置回 Ready；节点不存在时忽略。
// ts 为毫秒时间戳，传 0 表示用当前时间。
func (r *Registry) UpdateHeartbeat(nodeID string, ts int64) {
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok {
		n.LastHeartbeatAt = ts
		n.Status = StatusReady
	}
}

// MarkOffline 标记节点离线（连接断开时由事件源调用），保留元数据供查询；
// 节点不存在时忽略。
func (r *Registry) MarkOffline(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok {
		n.Status = StatusOffline
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
