package v1alpha1

// EdgeNode 描述一个边缘节点（对标 KubeEdge 的 Node 相关资源）。
//
// 边缘节点是 EdgeFlow 的资源承载者：设备（Device）通过 NodeName 绑定到
// 节点，云端通过 EdgeNode.Status 感知节点在线状态，并据此调度任务与
// 下发配置。节点由 edgecore 注册（后续实现），注册后周期性上报心跳。
type EdgeNode struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`

	// Spec 是节点的期望配置（云端声明）。
	Spec EdgeNodeSpec `json:"spec"`
	// Status 是节点的运行时状态（edgecore 上报，云端只读）。
	Status EdgeNodeStatus `json:"status,omitempty"`
}

// 节点角色常量。
const (
	// NodeRoleEdge 表示边缘节点（默认角色）。
	NodeRoleEdge = "edge"
	// NodeRoleCloud 表示云端节点。
	NodeRoleCloud = "cloud"
)

// EdgeNodeSpec 是边缘节点的期望配置。
type EdgeNodeSpec struct {
	// NodeID 是节点唯一标识（对标 KubeEdge Node 的 spec.nodeID）。
	// 由节点注册时生成（如 keadm join），全局唯一，用于云边通信鉴权。
	NodeID string `json:"nodeID,omitempty"`
	// Role 是节点角色：edge 或 cloud（默认 edge，见 SetDefaults）。
	Role string `json:"role,omitempty"`
	// Addresses 是节点的网络地址列表（内网 IP、域名等）。
	Addresses []NodeAddress `json:"addresses,omitempty"`
}

// 常用节点地址类型（对标 Kubernetes corev1.NodeAddressType）。
const (
	// NodeAddressTypeInternalIP 表示内网 IP 地址。
	NodeAddressTypeInternalIP = "InternalIP"
	// NodeAddressTypeHostname 表示主机名。
	NodeAddressTypeHostname = "Hostname"
	// NodeAddressTypeDNS 表示 DNS 域名。
	NodeAddressTypeDNS = "DNS"
)

// NodeAddress 描述节点的一个网络地址。
type NodeAddress struct {
	// Type 是地址类型，如 InternalIP / Hostname / DNS。
	Type string `json:"type"`
	// Address 是地址值，如 "192.168.1.10"。
	Address string `json:"address"`
}

// EdgeNodeStatus 是边缘节点的运行时状态。
type EdgeNodeStatus struct {
	// Phase 是节点生命周期阶段（见下方 NodePhase* 常量）。
	Phase string `json:"phase,omitempty"`
	// HeartbeatTime 是最近一次心跳时间（RFC3339，edgecore 周期性上报）。
	HeartbeatTime string `json:"heartbeatTime,omitempty"`
	// LastSeenTime 是云端最后一次观测到节点的时间（RFC3339）。
	LastSeenTime string `json:"lastSeenTime,omitempty"`
	// Conditions 是节点的健康条件列表（如 Ready）。
	Conditions []NodeCondition `json:"conditions,omitempty"`
	// Version 是 edgecore 版本号，便于云端识别旧版本节点。
	Version string `json:"version,omitempty"`
}

// 节点生命周期阶段常量。
const (
	// NodePhasePending 表示节点已注册但尚未与云端建立连接。
	NodePhasePending = "Pending"
	// NodePhaseRunning 表示节点在线且心跳正常。
	NodePhaseRunning = "Running"
	// NodePhaseOffline 表示节点掉线（心跳超时）。
	NodePhaseOffline = "Offline"
	// NodePhaseUnknown 表示节点状态未知。
	NodePhaseUnknown = "Unknown"
)

// NodeConditionReady 是节点就绪条件名。
const NodeConditionReady = "Ready"

// NodeCondition 描述节点的一个健康条件（对标 corev1.NodeCondition 最小集）。
type NodeCondition struct {
	// Type 是条件类型，如 "Ready"。
	Type string `json:"type"`
	// Status 是条件状态：True / False / Unknown。
	Status string `json:"status"`
	// Reason 是条件状态的机器可读原因（如 "HeartbeatTimeout"）。
	Reason string `json:"reason,omitempty"`
	// Message 是条件状态的人类可读说明。
	Message string `json:"message,omitempty"`
	// LastTransitionTime 是条件最近一次状态变化的时间（RFC3339）。
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}
