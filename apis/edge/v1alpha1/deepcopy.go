package v1alpha1

// 本文件手工实现各类型的深拷贝方法（DeepCopy / DeepCopyInto）。
//
// 深拷贝语义：拷贝得到的对象与原对象完全独立——修改副本的 map、slice、
// 指针字段等，不影响原对象。
//
// 说明：接入 Kubernetes 时，需要引入 k8s.io/apimachinery 并用
// controller-gen 等工具生成符合 runtime.Object 接口的实现
// （DeepCopyObject() runtime.Object），届时可整体替换本文件。
// 当前零依赖阶段先手写，保证"修改副本不影响原对象"的语义可用。

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *TypeMeta) DeepCopyInto(out *TypeMeta) {
	*out = *in
}

// DeepCopy 返回 TypeMeta 的深拷贝。
func (in *TypeMeta) DeepCopy() *TypeMeta {
	if in == nil {
		return nil
	}
	out := new(TypeMeta)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out（含 Labels / Annotations map）。
func (in *ObjectMeta) DeepCopyInto(out *ObjectMeta) {
	*out = *in
	if in.Labels != nil {
		out.Labels = make(map[string]string, len(in.Labels))
		for k, v := range in.Labels {
			out.Labels[k] = v
		}
	}
	if in.Annotations != nil {
		out.Annotations = make(map[string]string, len(in.Annotations))
		for k, v := range in.Annotations {
			out.Annotations[k] = v
		}
	}
}

// DeepCopy 返回 ObjectMeta 的深拷贝。
func (in *ObjectMeta) DeepCopy() *ObjectMeta {
	if in == nil {
		return nil
	}
	out := new(ObjectMeta)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *EdgeNode) DeepCopyInto(out *EdgeNode) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy 返回 EdgeNode 的深拷贝。
func (in *EdgeNode) DeepCopy() *EdgeNode {
	if in == nil {
		return nil
	}
	out := new(EdgeNode)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *EdgeNodeSpec) DeepCopyInto(out *EdgeNodeSpec) {
	*out = *in
	if in.Addresses != nil {
		out.Addresses = make([]NodeAddress, len(in.Addresses))
		copy(out.Addresses, in.Addresses)
	}
}

// DeepCopy 返回 EdgeNodeSpec 的深拷贝。
func (in *EdgeNodeSpec) DeepCopy() *EdgeNodeSpec {
	if in == nil {
		return nil
	}
	out := new(EdgeNodeSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *NodeAddress) DeepCopyInto(out *NodeAddress) {
	*out = *in
}

// DeepCopy 返回 NodeAddress 的深拷贝。
func (in *NodeAddress) DeepCopy() *NodeAddress {
	if in == nil {
		return nil
	}
	out := new(NodeAddress)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *EdgeNodeStatus) DeepCopyInto(out *EdgeNodeStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]NodeCondition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}

// DeepCopy 返回 EdgeNodeStatus 的深拷贝。
func (in *EdgeNodeStatus) DeepCopy() *EdgeNodeStatus {
	if in == nil {
		return nil
	}
	out := new(EdgeNodeStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *NodeCondition) DeepCopyInto(out *NodeCondition) {
	*out = *in
}

// DeepCopy 返回 NodeCondition 的深拷贝。
func (in *NodeCondition) DeepCopy() *NodeCondition {
	if in == nil {
		return nil
	}
	out := new(NodeCondition)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *DeviceModel) DeepCopyInto(out *DeviceModel) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

// DeepCopy 返回 DeviceModel 的深拷贝。
func (in *DeviceModel) DeepCopy() *DeviceModel {
	if in == nil {
		return nil
	}
	out := new(DeviceModel)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *DeviceModelSpec) DeepCopyInto(out *DeviceModelSpec) {
	*out = *in
	if in.Properties != nil {
		out.Properties = make([]DeviceProperty, len(in.Properties))
		copy(out.Properties, in.Properties)
	}
}

// DeepCopy 返回 DeviceModelSpec 的深拷贝。
func (in *DeviceModelSpec) DeepCopy() *DeviceModelSpec {
	if in == nil {
		return nil
	}
	out := new(DeviceModelSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *DeviceProperty) DeepCopyInto(out *DeviceProperty) {
	*out = *in
}

// DeepCopy 返回 DeviceProperty 的深拷贝。
func (in *DeviceProperty) DeepCopy() *DeviceProperty {
	if in == nil {
		return nil
	}
	out := new(DeviceProperty)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *Device) DeepCopyInto(out *Device) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy 返回 Device 的深拷贝。
func (in *Device) DeepCopy() *Device {
	if in == nil {
		return nil
	}
	out := new(Device)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *DeviceSpec) DeepCopyInto(out *DeviceSpec) {
	*out = *in
	in.Protocol.DeepCopyInto(&out.Protocol)
	if in.Properties != nil {
		out.Properties = make([]DevicePropertySpec, len(in.Properties))
		for i := range in.Properties {
			in.Properties[i].DeepCopyInto(&out.Properties[i])
		}
	}
}

// DeepCopy 返回 DeviceSpec 的深拷贝。
func (in *DeviceSpec) DeepCopy() *DeviceSpec {
	if in == nil {
		return nil
	}
	out := new(DeviceSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *ProtocolConfig) DeepCopyInto(out *ProtocolConfig) {
	*out = *in
	if in.Config != nil {
		out.Config = make(map[string]string, len(in.Config))
		for k, v := range in.Config {
			out.Config[k] = v
		}
	}
}

// DeepCopy 返回 ProtocolConfig 的深拷贝。
func (in *ProtocolConfig) DeepCopy() *ProtocolConfig {
	if in == nil {
		return nil
	}
	out := new(ProtocolConfig)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *DevicePropertySpec) DeepCopyInto(out *DevicePropertySpec) {
	*out = *in
	if in.Desired != nil {
		out.Desired = new(PropertyValue)
		in.Desired.DeepCopyInto(out.Desired)
	}
}

// DeepCopy 返回 DevicePropertySpec 的深拷贝。
func (in *DevicePropertySpec) DeepCopy() *DevicePropertySpec {
	if in == nil {
		return nil
	}
	out := new(DevicePropertySpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *PropertyValue) DeepCopyInto(out *PropertyValue) {
	*out = *in
	if in.Metadata != nil {
		out.Metadata = make(map[string]string, len(in.Metadata))
		for k, v := range in.Metadata {
			out.Metadata[k] = v
		}
	}
}

// DeepCopy 返回 PropertyValue 的深拷贝。
func (in *PropertyValue) DeepCopy() *PropertyValue {
	if in == nil {
		return nil
	}
	out := new(PropertyValue)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *DeviceStatus) DeepCopyInto(out *DeviceStatus) {
	*out = *in
	if in.Twins != nil {
		out.Twins = make([]TwinProperty, len(in.Twins))
		for i := range in.Twins {
			in.Twins[i].DeepCopyInto(&out.Twins[i])
		}
	}
}

// DeepCopy 返回 DeviceStatus 的深拷贝。
func (in *DeviceStatus) DeepCopy() *DeviceStatus {
	if in == nil {
		return nil
	}
	out := new(DeviceStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 将 in 的内容深拷贝到 out。
func (in *TwinProperty) DeepCopyInto(out *TwinProperty) {
	*out = *in
	if in.Desired != nil {
		out.Desired = new(PropertyValue)
		in.Desired.DeepCopyInto(out.Desired)
	}
	if in.Reported != nil {
		out.Reported = new(PropertyValue)
		in.Reported.DeepCopyInto(out.Reported)
	}
}

// DeepCopy 返回 TwinProperty 的深拷贝。
func (in *TwinProperty) DeepCopy() *TwinProperty {
	if in == nil {
		return nil
	}
	out := new(TwinProperty)
	in.DeepCopyInto(out)
	return out
}
