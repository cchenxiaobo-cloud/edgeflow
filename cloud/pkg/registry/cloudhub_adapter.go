package registry

import (
	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/pkg/log"
)

// CloudHubAdapter 把 CloudHub 的节点生命周期事件桥接进注册表
// （依赖注入：由 cmd/cloudcore 装配时通过 hub.SetNodeEvents 注册）。
//
// 职责映射：
//   - 注册成功 → Store.Register（携带节点元数据；失败记 error 日志，
//     etcd 写穿模式下意味着"未持久化 + 未入内存"）
//   - 心跳 → Store.UpdateHeartbeat（瞬态，仅内存）
//   - 断开 → Store.MarkOffline（瞬态，仅内存；保留元数据，仅标记离线）
//
// v0.4.0：构造参数由 *Registry 放宽为 Store 接口，纯内存与 etcd 写穿
// 两种装配均可注入。
type CloudHubAdapter struct {
	reg Store
}

// NewCloudHubAdapter 创建桥接器。
func NewCloudHubAdapter(reg Store) *CloudHubAdapter {
	return &CloudHubAdapter{reg: reg}
}

// OnNodeRegistered 实现 cloudhub.NodeEvents：节点注册成功。
func (a *CloudHubAdapter) OnNodeRegistered(info cloudhub.NodeInfo) {
	err := a.reg.Register(NodeInfo{
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
	if err != nil {
		log.Errorf("[CloudHubAdapter] 节点 %s 注册失败（未持久化、未入内存）: %v", info.NodeID, err)
	}
}

// OnNodeHeartbeat 实现 cloudhub.NodeEvents：收到节点心跳。
func (a *CloudHubAdapter) OnNodeHeartbeat(nodeID string) {
	a.reg.UpdateHeartbeat(nodeID, 0) // ts=0 表示用当前时间；心跳仅内存，恒 nil
}

// OnNodeDisconnected 实现 cloudhub.NodeEvents：节点连接断开。
func (a *CloudHubAdapter) OnNodeDisconnected(nodeID string) {
	a.reg.MarkOffline(nodeID) // 瞬态标记，仅内存，恒 nil
}
