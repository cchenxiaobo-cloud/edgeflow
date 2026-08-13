package v1alpha1

// GroupName 是 EdgeFlow CRD 的 API 分组名。
//
// 对标 KubeEdge 的 devices.kubeedge.io；EdgeFlow 统一使用 edgeflow.io，
// 保证所有资源（EdgeNode / DeviceModel / Device）在同一分组下。
const GroupName = "edgeflow.io"

// Version 是当前 API 版本（v1alpha1：初版，后续可能演进到 v1）。
const Version = "v1alpha1"

// GroupVersion 表示一个 API 分组 + 版本组合。
type GroupVersion struct {
	// Group 是 API 分组名，如 "edgeflow.io"。
	Group string
	// Version 是版本号，如 "v1alpha1"。
	Version string
}

// SchemeGroupVersion 是本包资源的默认 GroupVersion。
var SchemeGroupVersion = GroupVersion{Group: GroupName, Version: Version}

// String 返回 apiVersion 字符串，如 "edgeflow.io/v1alpha1"。
func (gv GroupVersion) String() string {
	return gv.Group + "/" + gv.Version
}
