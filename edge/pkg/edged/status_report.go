// Pod 状态上报（WBS 6.3 边缘侧）与状态表过期条目清理（P2-1）。
//
// 本文件提供：BuildStatusPayload 从 Status() 生成上报负载（v0.12.0 起为
// 只读采样——digest 采集失败降级空串，不阻塞上报，无副作用）；digestOfImageRef
// 声明式 digest 解析纯函数；cleanupStatus 在每轮调谐末尾清理已不在期望集合
// 的残留条目。上报循环本体（周期、发送、优雅退出）在 cmd/edgecore/
// status_report.go，与调谐解耦（见该文件注释）。
package edged

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
)

// PodStatusPayload 是 Pod 状态上报消息（TypePodStatus）的负载，
// 与云端契约一致（字段名与语义不可改）：
// 一条消息对应一个 Pod 的状态快照；Phase 取值见 phaseOf。
type PodStatusPayload struct {
	NodeID          string `json:"nodeID"`          // 边缘节点 ID（与消息 Source 一致）
	PodName         string `json:"podName"`         // Pod 名称
	Namespace       string `json:"namespace"`       // 命名空间（缺省 "default"）
	Phase           string `json:"phase"`           // 阶段：Running/Stopped/Absent/Unknown/Error
	Message         string `json:"message"`         // 错误文本（Phase=Error 时非空，其余为空串）
	LastReconcileAt int64  `json:"lastReconcileAt"` // 最近一次调谐时间（毫秒时间戳）
	// ImageDigest 镜像 manifest digest（v0.11.0 字段，v0.12.0 R-1++ 真实
	// 填充）；可选。采集为只读采样（声明式优先、运行时兜底），失败降级
	// 空串——不阻塞上报，云端对空 digest 跳过比对（老边缘/无采集等效 off）。
	ImageDigest string `json:"imageDigest,omitempty"`
}

// phaseOf 把（状态，错误）映射为上报阶段，与云端契约一致：
//   - Err != nil → "Error"（错误优先于状态枚举，Message 填错误文本）；
//   - 否则按状态映射：Running/Stopped/Absent/Unknown。
func phaseOf(state RuntimeState, err error) (phase, message string) {
	if err != nil {
		return "Error", err.Error()
	}
	switch state {
	case StateRunning:
		return "Running", ""
	case StateStopped:
		return "Stopped", ""
	case StateAbsent:
		return "Absent", ""
	default:
		return "Unknown", ""
	}
}

// BuildStatusPayload 从 Status() 生成上报负载数组（每条对应一个 Pod）。
// 输出按 podKey 升序排列：同一轮上报内容稳定有序，便于云端对账与日志比对。
// 只读采样、可失败降级（v0.12.0）：digest 采集失败仅跳过该字段，不阻塞
// 上报；节点 ID 由调用方传入（edgecore 用自身 nodeID）。
//
// digest 采集（R-1++ 双通道，仅 StateRunning 上报）：① 声明式——期望
// Pod 镜像引用含 "@sha256:"（digestOfImageRef 解析，pin 语义保证运行镜像
// 与 digest 一致，零运行时依赖）；② 运行时——无声明式时查 index 0 副本
// 的 RepoDigests（DockerRuntime.ImageDigest，拉取后真实 digest）。两者皆
// 空/失败 → 空串（omitempty 不写键，云端跳过比对，保持老行为）。
func (e *Edged) BuildStatusPayload(nodeID string) []PodStatusPayload {
	status := e.Status()
	// v0.12.0 R-1++：一轮读一次期望集合（ListPods），构建 key→image 表供
	// 声明式 digest 解析。读失败退化为空表——仅影响 digest（跳过），不阻塞上报。
	desiredImages := map[string]string{}
	if raw, err := e.store.ListPods(); err == nil {
		for _, j := range raw {
			var pod metamanager.Pod
			if json.Unmarshal([]byte(j), &pod) == nil && pod.Name != "" {
				desiredImages[podKey(pod.Namespace, pod.Name)] = pod.Image
			}
		}
	} else {
		log.Warnf("Edged 上报: 读取期望 Pod 集合失败（本轮 digest 跳过）: %v", err)
	}

	keys := make([]string, 0, len(status))
	for k := range status {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]PodStatusPayload, 0, len(keys))
	for _, k := range keys {
		st := status[k]
		p := parsePodKey(k)
		// 已删除 Pod（RemovedAt 非零）：上报 Absent 终态，时间戳用删除时刻
		// （P1 修复：Absent 必须在保留窗口内进入上报，云端据此清除陈旧记录）。
		if !st.RemovedAt.IsZero() {
			out = append(out, PodStatusPayload{
				NodeID:          nodeID,
				PodName:         p.Name,
				Namespace:       p.Namespace,
				Phase:           "Absent",
				Message:         "pod removed from desired set",
				LastReconcileAt: st.RemovedAt.UnixMilli(),
			})
			continue
		}
		phase, msg := phaseOf(st.State, st.Err)
		payload := PodStatusPayload{
			NodeID:          nodeID,
			PodName:         p.Name,
			Namespace:       p.Namespace,
			Phase:           phase,
			Message:         msg,
			LastReconcileAt: st.LastReconcile.UnixMilli(),
		}
		// v0.12.0 R-1++：仅 Running 态上报 digest（停掉/报错/Absent 的 Pod
		// 不应「声称」在跑该镜像，防误导云端 nodeDigestOf）。
		if st.State == StateRunning {
			payload.ImageDigest = e.digestOf(p, desiredImages[podKey(p.Namespace, p.Name)])
		}
		out = append(out, payload)
	}
	return out
}

// digestOfImageRef 从镜像引用提取 manifest digest（sha256:<64hex>，统一
// 小写）。规则：取最后一个 '@' 之后的部分；必须匹配 ^sha256:[0-9a-f]{64}$
// 才返回，否则返回 ""（未 pin digest → 调用方走运行时通道）。小写归一与
// 云端 Docker-Content-Digest 头形态对齐，避免 spec 大写 pin 比对误伤。
func digestOfImageRef(ref string) string {
	i := strings.LastIndex(ref, "@")
	if i < 0 {
		return ""
	}
	d := ref[i+1:]
	if !strings.HasPrefix(d, "sha256:") {
		return ""
	}
	hexpart := d[len("sha256:"):]
	if len(hexpart) != 64 {
		return ""
	}
	for k := 0; k < len(hexpart); k++ {
		c := hexpart[k]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
	}
	return "sha256:" + strings.ToLower(hexpart)
}

// digestOf 计算 Pod 上报 digest（v0.12.0，R-1++）：声明式（期望镜像引用含
// @sha256:）优先；否则运行时查 index 0 副本（多副本同镜像）；两者皆空/失败
// → ""（云端跳过比对，保持老行为）。运行时失败记 Warn 后降级空串，不阻塞。
func (e *Edged) digestOf(p metamanager.Pod, image string) string {
	if d := digestOfImageRef(image); d != "" {
		return d
	}
	d, err := e.rt.ImageDigest(p, 0)
	if err != nil {
		log.Warnf("Edged digest 采集失败（%s/%s）: %v", p.Namespace, p.Name, err)
		return ""
	}
	return d
}

// cleanupStatus 清理 status map 中已不在期望集合的残留条目（P2-1）。
//
// 背景：Pod 从期望集合删除后，孤儿清理（EnsureStopped）成功会把状态置为
// StateAbsent，但条目本身仍留在 map 中——长期运行（频繁创建/删除 Pod）下
// status map 会无界增长（内存泄漏），且过期条目会被后续轮次误上报给云端。
//
// 策略（期望集合是唯一权威，在每轮 reconcile 末尾执行）：
//   - key 仍在期望集合 → 保留；
//   - key 不在期望集合：
//   - State==StateAbsent 且 Err==nil（容器已确认移除，如孤儿清理成功）
//     → 进入 Absent 终态保留：首次设置 RemovedAt，保留 removedRetention
//     窗口（默认 90s，覆盖 3 个上报周期）让 Absent 终态上报到云端，
//     窗口期满后删除条目（P1 修复：避免云端删除 Pod 后列表永久陈旧）；
//   - 容器仍存在（孤儿清理失败等异常，State!=Absent 或带错误）→ 保留
//     条目（记录错误供排查与上报），直至容器确认移除——下一轮孤儿清理
//     成功后状态转 Absent，随即被删除；
//   - 容器已不存在（异常路径：如被外部删除，List 已不含它）→ 删除条目；
//   - List 失败（运行时不可用）时无法确认容器是否还在 → 保守处理：
//     只删除已确认 Absent 的条目，其余保留，避免误删排查信息。
//
// 说明：Absent 终态必须在上报循环的采样窗口内可见（保留 90s），否则云端
// 删除 Pod 后 /api/v1/pods 会永久显示陈旧状态（M2B 审查 P1 修复）。
//
// local 是运行时返回的容器实例（含副本序号）；只取 Pod 级 key 判断存在性。
func (e *Edged) cleanupStatus(desiredKeys map[string]struct{}, local []InstanceRef, listFailed bool) {
	localKeys := make(map[string]struct{}, len(local))
	for _, inst := range local {
		localKeys[podKey(inst.Namespace, inst.Name)] = struct{}{}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for key, st := range e.status {
		if _, ok := desiredKeys[key]; ok {
			continue // 期望集合内：保留
		}
		if st.State == StateAbsent && st.Err == nil {
			if st.RemovedAt.IsZero() {
				// 进入 Absent 终态保留：记录删除时刻，等待窗口期满
				st.RemovedAt = now
				e.status[key] = st
			} else if now.Sub(st.RemovedAt) >= e.removedRetention {
				delete(e.status, key) // 终态已保留足够窗口：清理
			}
			continue
		}
		if listFailed {
			continue // 无法确认容器是否还在：保守保留
		}
		if _, ok := localKeys[key]; !ok {
			delete(e.status, key) // 容器已不存在：条目无意义
		}
		// 容器还在（孤儿清理失败/待清理）：保留错误条目，下一轮收敛后删除
	}
}
