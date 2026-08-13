// Package v1alpha1 定义 EdgeFlow 边缘计算平台的核心 CRD 类型。
//
// 对标 KubeEdge 的 Device / DeviceModel 与 Node 相关资源，包含三类资源：
//
//   - EdgeNode：边缘节点（资源承载者，设备挂载在节点上）
//   - DeviceModel：设备型号（设备的"模板"：协议类型 + 属性定义）
//   - Device：设备实例（引用 DeviceModel，绑定到边缘节点，含数字孪生状态）
//
// 设计约定：
//
//   - 零第三方依赖：仅使用 Go 标准库，不引入 k8s.io/apimachinery 等；
//     当前仅提供手写的 DeepCopy/DeepCopyInto 方法，尚未实现
//     runtime.Object 接口（接入 Kubernetes 时补充）。
//   - API 分组：Group = edgeflow.io，Version = v1alpha1，
//     apiVersion 形如 "edgeflow.io/v1alpha1"。
//   - 序列化：所有结构体带 json 标签，可直接用于 JSON 持久化
//     （后续可无缝迁移到 CRD YAML）。
//
// 字段与 KubeEdge 的对应关系、推断字段说明见 docs/API-SPEC.md。
package v1alpha1
