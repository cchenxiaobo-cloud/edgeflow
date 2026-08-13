package registry

import (
	"edgeflow/cloud/pkg/cloudhub"
)

// CloudHubAdapter 把 CloudHub 的节点生命周期事件桥接进 Registry
// （依赖注入：由 cmd/cloudcore 装配时通过 hub.SetNodeEvents 注册）。
//
// 职责映射：
//   - 注册成功 → Registry.Register（携带节点元数据）
//   - 心跳 → Registry.UpdateHeartbeat
//   - 断开 → Registry.MarkOffline（保留元数据，仅标记离线，供查询 API 展示）
type CloudHubAdapter struct {
	reg *Registry
}

// NewCloudHubAdapter 创建桥接器。
func NewCloudHubAdapter(reg *Registry) *CloudHubAdapter {
	return &CloudHubAdapter{reg: reg}
}

// OnNodeRegistered 实现 cloudhub.NodeEvents：节点注册成功。
func (a *CloudHubAdapter) OnNodeRegistered(info cloudhub.NodeInfo) {
	a.reg.Register(NodeInfo{
		NodeID: info.NodeID,
		// M1 策略：云端分配的 nodeName 与 nodeID 一致（见 RegisterAck 注释）；
		// 将来引入命名策略后，在此改为取 Register payload 中的 nodeName
		NodeName:        info.NodeID,
		Arch:            info.Arch,
		OS:              info.OS,
		EdgecoreVersion: info.EdgecoreVersion,
		CPU:             info.CPU,
		Memory:          info.Memory,
		IP:              info.RemoteIP,
		RegisteredAt:    info.RegisteredAt.UnixMilli(),
	})
}

// OnNodeHeartbeat 实现 cloudhub.NodeEvents：收到节点心跳。
func (a *CloudHubAdapter) OnNodeHeartbeat(nodeID string) {
	a.reg.UpdateHeartbeat(nodeID, 0) // ts=0 表示用当前时间
}

// OnNodeDisconnected 实现 cloudhub.NodeEvents：节点连接断开。
func (a *CloudHubAdapter) OnNodeDisconnected(nodeID string) {
	a.reg.MarkOffline(nodeID)
}
