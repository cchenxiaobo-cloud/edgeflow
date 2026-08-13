package v1alpha1

// DeviceModel 描述一类设备的"模板"（对标 KubeEdge DeviceModel，
// devices.kubeedge.io/v1alpha2）。
//
// DeviceModel 只描述"这类设备长什么样"（协议家族 + 属性定义），
// 不涉及具体设备实例；实例由 Device 描述并引用本模板。
type DeviceModel struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`

	// Spec 是设备型号的定义（协议类型 + 属性列表）。
	Spec DeviceModelSpec `json:"spec"`
}

// DeviceModelSpec 定义设备型号的协议类型与属性集合。
type DeviceModelSpec struct {
	// Protocol 是设备使用的通信协议家族，
	// 如 "modbus"、"opcua"、"bluetooth"、"mqtt"。
	// 具体连接参数在 Device.Spec.Protocol 中配置。
	Protocol string `json:"protocol,omitempty"`
	// Properties 是该型号支持的属性列表。
	Properties []DeviceProperty `json:"properties,omitempty"`
}

// 属性访问模式常量。
const (
	// AccessModeReadOnly 表示属性只读：设备上报，云端不可下发期望值。
	AccessModeReadOnly = "ReadOnly"
	// AccessModeReadWrite 表示属性可读写：设备上报，云端可下发期望值（默认）。
	AccessModeReadWrite = "ReadWrite"
)

// DeviceProperty 描述设备型号上的一个属性。
type DeviceProperty struct {
	// Name 是属性名（同一型号内唯一）。
	Name string `json:"name"`
	// Description 是属性的人类可读说明。
	Description string `json:"description,omitempty"`
	// DataType 是属性数据类型：int / string / double / float / boolean。
	DataType string `json:"dataType"`
	// AccessMode 是访问模式：ReadOnly / ReadWrite（默认 ReadWrite）。
	AccessMode string `json:"accessMode,omitempty"`
	// DefaultValue 是属性的默认值（字符串形式，与 DataType 对应）。
	DefaultValue string `json:"defaultValue,omitempty"`
	// Minimum 是数值型属性的最小值（字符串形式）。
	Minimum string `json:"minimum,omitempty"`
	// Maximum 是数值型属性的最大值（字符串形式）。
	Maximum string `json:"maximum,omitempty"`
	// Unit 是属性的计量单位，如 "celsius"。
	Unit string `json:"unit,omitempty"`
}
