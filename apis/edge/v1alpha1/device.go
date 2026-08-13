package v1alpha1

// Device 描述一个具体的设备实例（对标 KubeEdge Device，
// devices.kubeedge.io/v1alpha2）。
//
// Device 是数字孪生（Twin）机制的核心：
//
//   - Spec.Properties 里声明云端期望值（desired）
//   - Status.Twins 记录设备实际上报值（reported）
//
// 设备与型号、节点的关系：
//
//	Device --引用--> DeviceModel（模板：协议 + 属性定义）
//	Device --绑定--> EdgeNode（承载节点，NodeName）
type Device struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`

	// Spec 是设备的期望配置（云端声明）。
	Spec DeviceSpec `json:"spec"`
	// Status 是设备的运行时状态（设备上报，云端只读）。
	Status DeviceStatus `json:"status,omitempty"`
}

// DeviceSpec 是设备的期望配置。
type DeviceSpec struct {
	// DeviceModelRef 是引用的设备型号名称（必填，须与 DeviceModel 同命名空间）。
	DeviceModelRef string `json:"deviceModelRef"`
	// NodeName 是设备绑定的边缘节点名称（必填，对标 KubeEdge 的 spec.nodeName）。
	NodeName string `json:"nodeName"`
	// Protocol 是设备的连接信息（协议名 + 连接参数）。
	Protocol ProtocolConfig `json:"protocol,omitempty"`
	// Properties 是设备实例上各属性的期望值（属性名须在 DeviceModel 中定义）。
	Properties []DevicePropertySpec `json:"properties,omitempty"`
}

// ProtocolConfig 描述设备的连接信息。
type ProtocolConfig struct {
	// ProtocolName 是协议名，须与 DeviceModel.Spec.Protocol 一致。
	ProtocolName string `json:"protocolName,omitempty"`
	// Config 是协议连接参数（键值对），如串口波特率、MQTT 地址等。
	// 对标 KubeEdge 的 protocolConfig 结构（此处用扁平键值对简化）。
	Config map[string]string `json:"config,omitempty"`
}

// DevicePropertySpec 描述设备实例上某个属性的期望状态。
type DevicePropertySpec struct {
	// Name 是属性名（须在 DeviceModel.Spec.Properties 中定义）。
	Name string `json:"name"`
	// Desired 是云端下发的期望值（数字孪生 desired 侧）。
	Desired *PropertyValue `json:"desired,omitempty"`
}

// PropertyValue 表示一个属性值（值 + 附加元数据）。
type PropertyValue struct {
	// Value 是属性值（字符串形式，与 DeviceModel 声明的 DataType 对应）。
	Value string `json:"value"`
	// Metadata 是值的附加信息（如采集时间、单位），键值对。
	Metadata map[string]string `json:"metadata,omitempty"`
}

// DeviceStatus 是设备的运行时状态。
type DeviceStatus struct {
	// Twins 是设备的数字孪生属性列表（desired 与 reported 对照）。
	Twins []TwinProperty `json:"twins,omitempty"`
	// LastUpdatedTime 是最近一次状态更新的时间（RFC3339）。
	LastUpdatedTime string `json:"lastUpdatedTime,omitempty"`
}

// TwinProperty 是数字孪生中的一个属性（对标 KubeEdge DeviceStatus.Twins）。
type TwinProperty struct {
	// PropertyName 是属性名。
	PropertyName string `json:"propertyName"`
	// Desired 是云端下发的期望值。
	Desired *PropertyValue `json:"desired,omitempty"`
	// Reported 是设备实际上报的值。
	Reported *PropertyValue `json:"reported,omitempty"`
}
