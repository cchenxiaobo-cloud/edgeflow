// 节点镜像 digest 偏好查询（v0.12.0，D-1：从 cmd/cloudcore/nodeDigestOf 提升
// 为包级函数，供发布控制器 DigestLookup 与发布 digest 复核端点共用同一闭包
// 实例——保证两者口径一致：优先模型部署 Pod，否则任意非空，全部为空 → ""）。
package podstatus

import "strings"

// NodeDigestOf 返回节点最近上报的镜像 digest 偏好查询闭包：
//   - 优先模型部署 Pod（edgeflow-model-* 前缀）的 imageDigest；
//   - 无则取该节点任意非空 imageDigest；
//   - 全部为空 / store 为 nil → ""（跳过比对，老边缘不误伤）。
//
// v0.11.0 起作为发布控制器 DigestLookup 装配；v0.12.0 提升为包级函数，
// 发布 digest 复核端点（GET .../releases/{releaseID}/digest）复用同一
// 闭包实例，回答"控制器现在会看到什么"。
func NodeDigestOf(store Store) func(nodeID string) string {
	return func(nodeID string) string {
		if store == nil {
			return ""
		}
		for _, ps := range store.ListByNode(nodeID) {
			if strings.HasPrefix(ps.PodName, "edgeflow-model-") && ps.ImageDigest != "" {
				return ps.ImageDigest
			}
		}
		for _, ps := range store.ListByNode(nodeID) {
			if ps.ImageDigest != "" {
				return ps.ImageDigest
			}
		}
		return ""
	}
}
