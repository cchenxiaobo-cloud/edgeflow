package v1alpha1

// 本文件实现各资源的默认值填充（SetDefaults）。
//
// 对标 Kubernetes 的默认值机制：资源创建/更新前把未显式填写的字段
// 补上合理默认值。当前是零依赖实现（直接调用方法即可）；接入
// Kubernetes 后，可迁移为 CRD schema 的 default 或 mutating webhook。

// SetDefaults 填充 EdgeNode 的默认值：
//   - Spec.Role 为空时设为 "edge"（大多数节点是边缘节点）
//   - Status.Phase 为空时设为 "Pending"（新节点尚未上线）
func (in *EdgeNode) SetDefaults() {
	in.Spec.SetDefaults()
	if in.Status.Phase == "" {
		in.Status.Phase = NodePhasePending
	}
}

// SetDefaults 填充 EdgeNodeSpec 的默认值：Role 为空时设为 "edge"。
func (in *EdgeNodeSpec) SetDefaults() {
	if in.Role == "" {
		in.Role = NodeRoleEdge
	}
}

// SetDefaults 填充 DeviceModel 的默认值：
// 属性未声明 AccessMode 时设为 ReadWrite（默认允许云端下发期望值）。
func (in *DeviceModel) SetDefaults() {
	for i := range in.Spec.Properties {
		if in.Spec.Properties[i].AccessMode == "" {
			in.Spec.Properties[i].AccessMode = AccessModeReadWrite
		}
	}
}
