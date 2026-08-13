package registry

import (
	"sort"
	"time"

	v1alpha1 "edgeflow/apis/edge/v1alpha1"
)

// 本文件实现 NodeInfo（运行视角）→ EdgeNode（CRD 对象视角）的映射，
// 让云端节点注册表同时提供两种视图：
//   - /api/v1/nodes：NodeInfo 视图（协议/运行细节，如 CPU、内存、IP）
//   - /api/v1/edgenodes：EdgeNode 视图（对标 K8s Node 的 CRD 对象，
//     后续对接真实 apiserver 时，本文件就是"本地注册表 → Node 对象"的
//     转换逻辑，可直接复用）
//
// 映射是"一次性快照"：每次转换都新建 map/slice，返回对象与注册表
// 内部状态完全独立，调用方任意修改不影响注册表。

// edgeNodeKind 是 EdgeNode 资源的 Kind（与 CRD 声明一致）。
const edgeNodeKind = "EdgeNode"

// ToEdgeNode 把运行视角的 NodeInfo 映射为 CRD 对象视角的 EdgeNode。
//
// 字段映射规则：
//   - Spec.NodeID ← NodeInfo.NodeID（云端唯一标识，对标 KubeEdge 的 spec.nodeID）
//   - Spec.Role ← 固定 "edge"（注册表只登记边缘节点）
//   - Spec.Addresses ← NodeInfo.IP（InternalIP 类型；为空则不填）
//   - Status.Phase ← NodeInfo.Status（Ready→Running，Offline→Offline，其余→Unknown）
//   - Status.Conditions ← 单一 Ready 条件（True/False/Unknown 与状态对应，
//     附 Reason/Message/LastTransitionTime）
//   - Status.HeartbeatTime / LastSeenTime ← LastHeartbeatAt（RFC3339）
//   - Status.Version ← EdgecoreVersion
//   - ObjectMeta.Name ← NodeName（为空时兜底用 NodeID）
//   - ObjectMeta.Labels ← nodeID/arch/os（edgeflow.io/* 前缀，便于标签选择）
//   - ObjectMeta.CreationTimestamp ← RegisteredAt（RFC3339）
func (r *Registry) ToEdgeNode(info *NodeInfo) *v1alpha1.EdgeNode {
	if info == nil {
		return nil
	}
	hb := msToRFC3339(info.LastHeartbeatAt)
	node := &v1alpha1.EdgeNode{
		TypeMeta: v1alpha1.TypeMeta{
			Kind:       edgeNodeKind,
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              info.NodeName,
			CreationTimestamp: msToRFC3339(info.RegisteredAt),
			Labels: map[string]string{
				"edgeflow.io/node-id": info.NodeID,
			},
		},
		Spec: v1alpha1.EdgeNodeSpec{
			NodeID: info.NodeID,
			Role:   v1alpha1.NodeRoleEdge,
		},
		Status: v1alpha1.EdgeNodeStatus{
			Phase:         phaseFromStatus(info.Status),
			HeartbeatTime: hb,
			LastSeenTime:  hb,
			Version:       info.EdgecoreVersion,
			Conditions:    []v1alpha1.NodeCondition{readyCondition(info.Status, info.LastHeartbeatAt)},
		},
	}
	if node.Name == "" {
		// M1 阶段 NodeName 与 NodeID 一致；兜底保证 Name 非空
		node.Name = info.NodeID
	}
	if info.Arch != "" {
		node.Labels["edgeflow.io/arch"] = info.Arch
	}
	if info.OS != "" {
		node.Labels["edgeflow.io/os"] = info.OS
	}
	if info.IP != "" {
		node.Spec.Addresses = []v1alpha1.NodeAddress{
			{Type: v1alpha1.NodeAddressTypeInternalIP, Address: info.IP},
		}
	}
	return node
}

// ListEdgeNodes 返回全部节点的 EdgeNode 视图（按 Name 排序）。
//
// 并发安全：整个转换在读锁内完成——转换过程只读内部指针，产出的是
// 全新对象（含新分配的 map/slice），不返回内部指针，调用方修改结果
// 不会污染注册表。
func (r *Registry) ListEdgeNodes() []v1alpha1.EdgeNode {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]v1alpha1.EdgeNode, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, *r.ToEdgeNode(n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// phaseFromStatus 把注册表状态映射为 EdgeNode 生命周期阶段：
// Ready→Running（在线且心跳正常），Offline→Offline，其余（含 Unknown）→Unknown。
func phaseFromStatus(s NodeStatus) string {
	switch s {
	case StatusReady:
		return v1alpha1.NodePhaseRunning
	case StatusOffline:
		return v1alpha1.NodePhaseOffline
	default:
		return v1alpha1.NodePhaseUnknown
	}
}

// 条件状态取值（对标 corev1.ConditionStatus；apis 包未提供常量，此处本地定义）。
const (
	// condTrue 表示条件成立。
	condTrue = "True"
	// condFalse 表示条件不成立。
	condFalse = "False"
	// condUnknown 表示条件状态未知。
	condUnknown = "Unknown"
)

// readyCondition 构造 Ready 条件：条件状态与注册表状态对应，
// Reason 为机器可读原因（英文），Message 为人类可读说明（中文）。
func readyCondition(s NodeStatus, ts int64) v1alpha1.NodeCondition {
	cond := v1alpha1.NodeCondition{
		Type:               v1alpha1.NodeConditionReady,
		LastTransitionTime: msToRFC3339(ts),
	}
	switch s {
	case StatusReady:
		cond.Status = condTrue
		cond.Reason = "HeartbeatReceived"
		cond.Message = "节点在线，心跳正常"
	case StatusOffline:
		cond.Status = condFalse
		cond.Reason = "HeartbeatTimeout"
		cond.Message = "节点离线：心跳超时或连接断开"
	default:
		cond.Status = condUnknown
		cond.Reason = "NodeNotConnected"
		cond.Message = "节点状态未知"
	}
	return cond
}

// msToRFC3339 把 Unix 毫秒时间戳转成 RFC3339 字符串（EdgeNode 的
// 时间字段约定格式）；ts<=0 时返回空串。
func msToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
