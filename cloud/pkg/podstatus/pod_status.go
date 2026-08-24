// Package podstatus 实现云端 Pod 状态存储（内存态，WBS 6.3 云端侧）。
//
// 这是云边 Pod 状态回传链路的云端落点：边缘侧周期性上报 PodStatus 消息，
// CloudHub 收到后经回调注入本存储，再由 cloudcore 的 HTTP API
// （/api/v1/pods、/api/v1/nodes/{nodeID}/pods）对外提供查询。
//
// 与 registry 包同级的定位：当前为内存 Map 形态，待集群环境就绪后，
// 本包将被 K8s apiserver 对接（Pod 对象 + Informer/Watch）取代，
// 具体迁移点见文件末尾"后续对接 K8s apiserver"注释。
package podstatus

import (
	"sort"
	"sync"
)

// Store 是 Pod 状态存储的读写契约（v0.4.0 引入）。
//
// v0.4.0 键空间预留：/edgeflow/podstatus/<nodeID>/<ns>/<podName>。
// 当前版本暂不落盘：Pod 状态是边缘 reported 周期上报的瞬态数据，
// cloudcore 重启后 pods 列表短暂为空（≤1 个上报周期即自愈），
// 落盘只会造成写放大 + 重启后服务陈旧快照误导客户端。
// 未来如需持久化，键空间不变，直接新增 etcd 包装实现即可。
//
// 错误约定：仅 Upsert 返回 error（v0.4.0 纯内存实现恒返回 nil；未来
// 写穿 etcd 后失败即返回 error、内存不动）。Delete/Get/ListByNode/
// ListAll/Count 保持原签名——现有调用点（单测、HTTP API）在条件/多值
// 绑定上下文中使用，Go 不允许忽略多余返回值（设计 §8.1 的"调用点可
// 忽略返回值"仅对语句调用成立）；读路径恒走内存缓存，本就不存在
// 失败路径（见存储改造设计 §1）。
type Store interface {
	// Upsert 写入（或更新）一条 Pod 状态。
	Upsert(nodeID string, ps PodStatus) error
	// Delete 删除一条 Pod 状态；返回是否存在且被删除。
	Delete(nodeID, namespace, podName string) bool
	// Get 查询单条 Pod 状态。
	Get(nodeID, namespace, podName string) (PodStatus, bool)
	// ListByNode 返回指定节点的全部 Pod 状态（排序确定）。
	ListByNode(nodeID string) []PodStatus
	// ListAll 返回全部 Pod 状态（排序确定）。
	ListAll() []PodStatus
	// Count 返回记录总数。
	Count() int
}

// 编译期断言：PodStatusStore 实现 Store 契约。
var _ Store = (*PodStatusStore)(nil)

// 阶段取值（与边缘侧 PodStatus 契约一致，勿单独修改）。
const (
	// PhaseRunning 表示 Pod 运行中。
	PhaseRunning = "Running"
	// PhaseStopped 表示 Pod 已停止。
	PhaseStopped = "Stopped"
	// PhaseAbsent 表示 Pod 不存在（边缘侧曾管理、现已删除）。
	PhaseAbsent = "Absent"
	// PhaseError 表示 Pod 处于错误状态。
	PhaseError = "Error"
)

// PodStatus 是单条 Pod 状态的云端快照，字段与边→云 PodStatus
// 消息契约一致，同时也是 HTTP API 的 JSON 形态。
//
// 时间戳统一为 Unix 毫秒（与协议 payload 的毫秒约定一致），
// JSON 字段名与协议/API 约定保持一致（小驼峰）。
type PodStatus struct {
	NodeID          string `json:"nodeID"`          // 节点唯一 ID（来自消息 Source）
	PodName         string `json:"podName"`         // Pod 名称
	Namespace       string `json:"namespace"`       // 命名空间（缺省按 "default" 处理）
	Phase           string `json:"phase"`           // 阶段（Running/Stopped/Absent/Error）
	Message         string `json:"message"`         // 附加说明（如错误原因）
	LastReconcileAt int64  `json:"lastReconcileAt"` // 最近一次协调时间（毫秒时间戳）
}

// DefaultNamespace 是契约中 namespace 缺省时的取值（与 K8s 默认命名空间一致）。
const DefaultNamespace = "default"

// podKey 是节点内 Pod 的唯一键：namespace + "/" + podName。
// Pod 可能跨 namespace 重名（如 default/web-demo 与 kube-system/web-demo），
// 仅用 podName 作 key 会互相覆盖，因此键必须带 namespace。
type podKey string

// podKeyOf 构造 Pod 键（namespace 缺省补 "default"）。
func podKeyOf(namespace, podName string) podKey {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return podKey(namespace + "/" + podName)
}

// PodStatusStore 是内存态 Pod 状态存储。
//
// 并发安全：所有方法内部加锁，可在任意 goroutine（CloudHub 连接处理、
// HTTP 请求处理）中并发调用。Get/List 返回值的拷贝，调用方修改不会
// 污染存储内部状态。
//
// 数据保留策略：节点断开时不清空其 Pod 状态（与 registry 保留离线节点
// 元数据的策略一致），便于查询 API 展示节点离线前的最后状态；
// 待后续接入 apiserver 后由 TTL/GC 策略取代。
type PodStatusStore struct {
	mu   sync.RWMutex
	pods map[string]map[podKey]PodStatus // nodeID → (podKey → PodStatus)
}

// NewStore 创建空存储。
func NewStore() *PodStatusStore {
	return &PodStatusStore{pods: make(map[string]map[podKey]PodStatus)}
}

// Upsert 写入（或更新）一条 Pod 状态。
//
// 语义：
//   - nodeID 为空、或 ps.PodName 为空时忽略（无法定位存储键）
//   - ps.NodeID 以参数 nodeID 为准（消息来源即权威，防 payload 伪造错位）
//   - namespace 缺省补 "default"
//   - 已存在同键记录时整体覆盖（以最新上报为准）
//
// v0.4.0 纯内存实现恒返回 nil（不落盘，见 Store 接口注释）。
func (s *PodStatusStore) Upsert(nodeID string, ps PodStatus) error {
	if nodeID == "" || ps.PodName == "" {
		return nil
	}
	ps.NodeID = nodeID
	if ps.Namespace == "" {
		ps.Namespace = DefaultNamespace
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byNode, ok := s.pods[nodeID]
	if !ok {
		byNode = make(map[podKey]PodStatus)
		s.pods[nodeID] = byNode
	}
	byNode[podKeyOf(ps.Namespace, ps.PodName)] = ps
	return nil
}

// Count 返回存储内 Pod 状态记录总数（供 /metrics 的 pods_total 指标使用）。
func (s *PodStatusStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, byKey := range s.pods {
		n += len(byKey)
	}
	return n
}

// Get 查询单条 Pod 状态。
//
// 返回值是拷贝，调用方修改不会污染存储。
// namespace 缺省补 "default"；不存在时返回 false。
func (s *PodStatusStore) Get(nodeID, namespace, podName string) (PodStatus, bool) {
	if nodeID == "" || podName == "" {
		return PodStatus{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps, ok := s.pods[nodeID][podKeyOf(namespace, podName)]
	return ps, ok
}

// Delete 删除一条 Pod 状态；不存在时静默成功（幂等）。
// namespace 缺省补 "default"；返回是否存在且被删除。
//
// 保持 bool 签名：现有调用点（单测条件、main.go 语句）约束，见 Store 注释。
func (s *PodStatusStore) Delete(nodeID, namespace, podName string) bool {
	if nodeID == "" || podName == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byNode, ok := s.pods[nodeID]
	if !ok {
		return false
	}
	key := podKeyOf(namespace, podName)
	if _, ok := byNode[key]; !ok {
		return false
	}
	delete(byNode, key)
	// 节点下已无 Pod 时清理空 map，避免空壳节点长期驻留
	if len(byNode) == 0 {
		delete(s.pods, nodeID)
	}
	return true
}

// ListByNode 返回指定节点的全部 Pod 状态（按 namespace/podName 排序，
// 保证输出确定性）；节点无 Pod 时返回空切片（非 nil，JSON 编码为 []）。
func (s *PodStatusStore) ListByNode(nodeID string) []PodStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byNode := s.pods[nodeID]
	out := make([]PodStatus, 0, len(byNode))
	for _, ps := range byNode {
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].PodName < out[j].PodName
	})
	return out
}

// ListAll 返回全部 Pod 状态（按 nodeID → namespace/podName 排序，
// 保证输出确定性）；无数据时返回空切片（非 nil，JSON 编码为 []）。
func (s *PodStatusStore) ListAll() []PodStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PodStatus, 0, len(s.pods))
	for _, byNode := range s.pods {
		for _, ps := range byNode {
			out = append(out, ps)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].PodName < out[j].PodName
	})
	return out
}

// 后续对接 K8s apiserver 的迁移点：
//   - 本存储的 map[nodeID]map[podKey]PodStatus 将由 apiserver 的
//     Pod 对象 + Informer 缓存取代，PodStatus 结构体保留为 API 响应 DTO
//   - Upsert 对应 apiserver 的 Pod create/update（含 status 子资源）
//   - Delete 对应 Pod delete（或 status.phase=Absent 的最终态由边缘收敛）
//   - ListAll/ListByNode 对应 apiserver 的 List（按 nodeName 字段过滤）
//   - 节点断开清理策略届时改为 apiserver 侧 TTL/驱逐逻辑
