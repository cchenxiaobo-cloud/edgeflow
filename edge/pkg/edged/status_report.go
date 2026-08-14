// Pod 状态上报（WBS 6.3 边缘侧）与状态表过期条目清理（P2-1）。
//
// 本文件只提供纯函数与状态表操作：BuildStatusPayload 从 Status() 生成
// 上报负载；cleanupStatus 在每轮调谐末尾清理已不在期望集合的残留条目。
// 上报循环本体（周期、发送、优雅退出）在 cmd/edgecore/status_report.go，
// 与调谐解耦（见该文件注释）。
package edged

import (
	"sort"

	"edgeflow/edge/pkg/metamanager"
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
// 纯读取、无副作用；节点 ID 由调用方传入（edgecore 用自身 nodeID）。
func (e *Edged) BuildStatusPayload(nodeID string) []PodStatusPayload {
	status := e.Status()
	keys := make([]string, 0, len(status))
	for k := range status {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]PodStatusPayload, 0, len(keys))
	for _, k := range keys {
		st := status[k]
		phase, msg := phaseOf(st.State, st.Err)
		p := parsePodKey(k)
		out = append(out, PodStatusPayload{
			NodeID:          nodeID,
			PodName:         p.Name,
			Namespace:       p.Namespace,
			Phase:           phase,
			Message:         msg,
			LastReconcileAt: st.LastReconcile.UnixMilli(),
		})
	}
	return out
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
//     → 直接删除条目，不保留历史；
//   - 容器仍存在（孤儿清理失败等异常，State!=Absent 或带错误）→ 保留
//     条目（记录错误供排查与上报），直至容器确认移除——下一轮孤儿清理
//     成功后状态转 Absent，随即被删除；
//   - 容器已不存在（异常路径：如被外部删除，List 已不含它）→ 删除条目；
//   - List 失败（运行时不可用）时无法确认容器是否还在 → 保守处理：
//     只删除已确认 Absent 的条目，其余保留，避免误删排查信息。
//
// 说明：被删除 Pod 的 "Absent" 阶段只在孤儿清理成功到本函数执行之间的
// 极短窗口内存在，不会进入上报——云端删除 Pod 时本就知晓删除动作，
// 无需边侧回补 Absent 历史（若未来需要删除确认，可在此改为延迟删除）。
func (e *Edged) cleanupStatus(desiredKeys map[string]struct{}, local []metamanager.Pod, listFailed bool) {
	localKeys := make(map[string]struct{}, len(local))
	for _, p := range local {
		localKeys[podKey(p.Namespace, p.Name)] = struct{}{}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for key, st := range e.status {
		if _, ok := desiredKeys[key]; ok {
			continue // 期望集合内：保留
		}
		if st.State == StateAbsent && st.Err == nil {
			delete(e.status, key) // 容器已确认移除：不保留历史
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
