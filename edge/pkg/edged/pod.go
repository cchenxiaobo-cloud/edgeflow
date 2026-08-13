// Package edged 实现 EdgeCore 的 Edged 模块（WBS 3.2）：轻量容器生命周期管理。
//
// 本包是"方案 A（自研精简 kubelet）"的 POC 产物（docs/EDGED-POC.md）：
//   - 容器运行时抽象（ContainerRuntime）+ 双实现（Mock / Docker CLI）；
//   - Edged 主循环（reconciler）：定时读取 MetaManager 中的期望 Pod 集合，
//     对差异执行创建/启动/删除，收敛到期望状态；
//   - 状态记录（Status()）为后续 Pod 状态上报（WBS 6.3）预留。
package edged

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"edgeflow/edge/pkg/metamanager"
)

// PodSpec 是 Edged 消费的 Pod 规格。
//
// 决策：直接复用 metamanager.Pod（类型别名），不重复定义——
//   - 避免两份定义字段漂移（POC 阶段 Pod 只有 name/namespace/image/replicas 四字段，
//     与云端 PodSync 契约一致）；
//   - 后续 M2 扩展字段时只需改 metamanager.Pod 一处。
//
// 别名在编译期与 metamanager.Pod 完全等价，接口签名用 PodSpec 仅为可读性。
type PodSpec = metamanager.Pod

// 容器命名与标签常量。
const (
	// containerNamePrefix 是容器名前缀，完整命名规范：
	// edgeflow-<namespace>-<name>，namespace 缺省 "default"。
	containerNamePrefix = "edgeflow-"

	// labelPod / labelNamespace 是打到容器上的标签键：
	// Edged 通过 label=edgeflow.pod 过滤出自己管理的容器（List），
	// 避免误动其他工具创建的容器。
	labelPod       = "edgeflow.pod"
	labelNamespace = "edgeflow.namespace"

	// maxContainerNameLen 容器名长度上限。docker 限制 255 字符，
	// 留余量取 200，超长时截断并追加 hash 后缀防碰撞。
	maxContainerNameLen = 200
)

// ContainerName 派生容器名：edgeflow-<namespace>-<name>。
//
// 合法化处理（docker 名要求 [a-zA-Z0-9][a-zA-Z0-9_.-]*）：
//   - namespace 缺省 "default"（与 metamanager key 派生规则一致）；
//   - 非法字符替换为 '-'，整体转小写；
//   - 超长（>200）时截断并追加原始名的 sha256 前 8 位 hex 后缀，防止不同
//     名字截断后碰撞。
func ContainerName(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	raw := containerNamePrefix + namespace + "-" + name
	n := sanitizeDockerName(raw)
	if len(n) <= maxContainerNameLen {
		return n
	}
	sum := sha256.Sum256([]byte(raw))
	suffix := "-" + hex.EncodeToString(sum[:4])
	return n[:maxContainerNameLen-len(suffix)] + suffix
}

// sanitizeDockerName 把任意字符串清洗成合法 docker 容器名：
// 转小写、非法字符替换为 '-'、去掉首部非字母数字字符、空结果兜底 "pod"。
func sanitizeDockerName(s string) string {
	lower := strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(lower))
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	out := b.String()
	// docker 名称首字符必须是字母或数字
	for len(out) > 0 && !isAlphaNum(out[0]) {
		out = out[1:]
	}
	if out == "" {
		return "pod"
	}
	return out
}

// isAlphaNum 判断字符是否为字母或数字。
func isAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// podKey 是调谐与状态记录的键：<namespace>/<name>。
// namespace 缺省 "default"，与 metamanager 存储 key 规则（pods/<namespace>/<name>）
// 对齐，保证"本地容器集合"与"期望 Pod 集合"可比对。
func podKey(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	return namespace + "/" + name
}
