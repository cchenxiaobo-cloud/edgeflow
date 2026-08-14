// Package edged 实现 EdgeCore 的 Edged 模块（WBS 3.2）：轻量容器生命周期管理。
//
// 本包是"方案 A（自研精简 kubelet）"的 POC 产物（docs/EDGED-POC.md）：
//   - 容器运行时抽象（ContainerRuntime）+ 双实现（Mock / Docker CLI）；
//   - Edged 主循环（reconciler）：定时读取 MetaManager 中的期望 Pod 集合，
//     对差异执行创建/启动/删除，收敛到期望状态（含多副本展开 WBS 6.4 与
//     健康检查自愈 WBS 6.5）；
//   - 状态记录（Status()）为后续 Pod 状态上报（WBS 6.3）预留。
package edged

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"edgeflow/edge/pkg/metamanager"
)

// PodSpec 是 Edged 消费的 Pod 规格。
//
// 决策：直接复用 metamanager.Pod（类型别名），不重复定义——
//   - 避免两份定义字段漂移（Pod 有 name/namespace/image/replicas 字段，
//     与云端 PodSync 契约一致）；
//   - 后续扩展字段时只需改 metamanager.Pod 一处。
//
// 别名在编译期与 metamanager.Pod 完全等价，接口签名用 PodSpec 仅为可读性。
type PodSpec = metamanager.Pod

// 容器命名与标签常量。
const (
	// containerNamePrefix 是容器名前缀，完整命名规范：
	// edgeflow-<namespace>-<name>-<index>，namespace 缺省 "default"；
	// index 是副本序号（0..Replicas-1，单副本时恒为 0）。
	containerNamePrefix = "edgeflow-"

	// labelPod / labelNamespace 是打到容器上的标签键：
	// Edged 通过 label=edgeflow.pod 过滤出自己管理的容器（List），
	// 避免误动其他工具创建的容器。
	labelPod       = "edgeflow.pod"
	labelNamespace = "edgeflow.namespace"

	// maxContainerNameLen 容器名长度上限。docker 限制 255 字符，
	// 留余量取 200，超长时截断并追加 hash 后缀防碰撞。
	maxContainerNameLen = 200

	// maxNameBaseLen 基础名（不含 -<index> 副本序号）长度上限：
	// 序号始终追加在末尾，保证 List 能按"末段数字"反解 index
	// （超长名截断只作用于基础名，不吞掉副本序号）。
	maxNameBaseLen = 190
)

// ContainerName 派生容器名：edgeflow-<namespace>-<name>-<index>。
//
// index 是副本序号：一个 Pod 的 Replicas 个副本分别命名
// edgeflow-<ns>-<name>-0 .. -<Replicas-1>；单副本（缺省 1）时恒为 -0。
//
// 合法化处理（docker 名要求 [a-zA-Z0-9][a-zA-Z0-9_.-]*）：
//   - namespace 缺省 "default"（与 metamanager key 派生规则一致）；
//   - 非法字符替换为 '-'，整体转小写；
//   - 基础名超长（>maxNameBaseLen）时截断并追加原始名的 sha256 前 8 位
//     hex 后缀（防止不同名字截断后碰撞），副本序号不受截断影响。
func ContainerName(namespace, name string, index int) string {
	if namespace == "" {
		namespace = "default"
	}
	if index < 0 {
		index = 0 // 防御：非法序号兜底为 0
	}
	raw := containerNamePrefix + namespace + "-" + name
	base := sanitizeDockerName(raw)
	if len(base) > maxNameBaseLen {
		sum := sha256.Sum256([]byte(raw))
		suffix := "-" + hex.EncodeToString(sum[:4])
		base = base[:maxNameBaseLen-len(suffix)] + suffix
	}
	return fmt.Sprintf("%s-%d", base, index)
}

// sanitizeDockerName 把任意字符串清洗成合法 docker 容器名：
// 转小写、非法字符替换为 '-'、去掉首部非字母数字字符、空结果兜底 "pod"。
// LegacyContainerName 返回旧版无序号容器名（edgeflow-<ns>-<name>，
// M4A P1-1 迁移用：仅 EnsureStopped(index=-1) 删除路径使用）。
func LegacyContainerName(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	return sanitizeDockerName(containerNamePrefix + namespace + "-" + name)
}

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

// InstanceRef 是运行时上一个具体容器实例的引用：Pod 标识 + 副本序号。
// ContainerRuntime.List() 返回它，让 reconciler 能按副本粒度收敛
// （补齐创建/收缩停止/逐副本健康检查）。
type InstanceRef struct {
	Namespace string
	Name      string
	Index     int
}

// Pod 返回该实例所属的 Pod 标识（不含 Image/Replicas——停止/检查用不到）。
func (r InstanceRef) Pod() metamanager.Pod {
	return metamanager.Pod{Namespace: r.Namespace, Name: r.Name}
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

// instanceKey 是副本实例的键：<namespace>/<name>#<index>。
// MockRuntime 的状态表与调用计数用它区分同一 Pod 的不同副本。
func instanceKey(namespace, name string, index int) string {
	return podKey(namespace, name) + "#" + strconv.Itoa(index)
}

// parseInstanceKey 把 instanceKey 拆回 InstanceRef；无 #index 时兜底副本 0
// （兼容旧式键，如升级前的测试数据）。
func parseInstanceKey(key string) InstanceRef {
	if i := strings.LastIndexByte(key, '#'); i >= 0 {
		idx, err := strconv.Atoi(key[i+1:])
		if err == nil {
			// 允许负 index：-1 表示旧式无序号命名实例（M4A P1-1），
			// 与 docker_runtime.parseIndexFromName 的语义保持一致。
			p := parsePodKey(key[:i])
			return InstanceRef{Namespace: p.Namespace, Name: p.Name, Index: idx}
		}
	}
	p := parsePodKey(key)
	return InstanceRef{Namespace: p.Namespace, Name: p.Name}
}

// parsePodKey 把 podKey（<namespace>/<name>）拆回 Pod。
func parsePodKey(key string) metamanager.Pod {
	ns, name := "default", key
	if i := strings.IndexByte(key, '/'); i >= 0 {
		ns, name = key[:i], key[i+1:]
	}
	return metamanager.Pod{Namespace: ns, Name: name}
}
