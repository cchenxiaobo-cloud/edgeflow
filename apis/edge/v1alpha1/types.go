package v1alpha1

// TypeMeta 描述资源自身的类型与 API 版本。
//
// 对标 k8s.io/apimachinery 的 metav1.TypeMeta（最小子集）。
// 序列化时内联展开：JSON 顶层直接出现 kind / apiVersion 字段。
type TypeMeta struct {
	// Kind 是资源种类，如 "EdgeNode"。
	Kind string `json:"kind,omitempty"`
	// APIVersion 是 API 版本字符串，如 "edgeflow.io/v1alpha1"。
	APIVersion string `json:"apiVersion,omitempty"`
}

// ObjectMeta 是资源的通用元数据。
//
// 对标 metav1.ObjectMeta 的最小子集：只保留当前业务需要且可
// 手写深拷贝的字段。后续接入 Kubernetes 时替换为 metav1.ObjectMeta。
type ObjectMeta struct {
	// Name 是资源名称（在同一命名空间内唯一）。
	Name string `json:"name"`
	// Namespace 是资源所属命名空间（空表示默认命名空间）。
	Namespace string `json:"namespace,omitempty"`
	// Labels 是资源的标签（键值对，用于选择与分组）。
	Labels map[string]string `json:"labels,omitempty"`
	// Annotations 是资源的注解（键值对，用于附加说明信息）。
	Annotations map[string]string `json:"annotations,omitempty"`
	// UID 是资源的全局唯一标识（接入 Kubernetes 后由 apiserver 分配）。
	UID string `json:"uid,omitempty"`
	// ResourceVersion 是资源的版本号（接入 Kubernetes 后由 apiserver 维护）。
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// CreationTimestamp 是资源的创建时间（RFC3339 格式字符串）。
	CreationTimestamp string `json:"creationTimestamp,omitempty"`
}
