package edged

import "edgeflow/edge/pkg/metamanager"

// RuntimeState 是容器运行时状态的归一化枚举：
// 各运行时实现（Mock / Docker）把真实状态映射到这四态，屏蔽底层差异。
type RuntimeState int

const (
	// StateAbsent 容器不存在（从未创建或已删除）。
	StateAbsent RuntimeState = iota
	// StateRunning 容器存在且运行中。
	StateRunning
	// StateStopped 容器存在但已停止（docker: Exited/created 未运行）。
	StateStopped
	// StateUnknown 状态未知（运行时不可用、解析失败等）。
	StateUnknown
)

// String 返回状态的可读名称（日志/状态上报用）。
func (s RuntimeState) String() string {
	switch s {
	case StateAbsent:
		return "absent"
	case StateRunning:
		return "running"
	case StateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// ContainerRuntime 是容器运行时抽象（方案 A 的核心接口）。
//
// 设计要点：
//   - 以"Pod 副本实例"为操作单位：Pod 的期望副本数由 Replicas 字段决定
//     （缺省 1），每个副本是独立容器（命名 edgeflow-<ns>-<name>-<index>，
//     index 0..Replicas-1）。reconciler 按副本粒度补齐/收缩/健康检查，
//     运行时只需保证"第 index 个副本"的幂等收敛；
//   - 所有方法必须幂等：重复调用不产生副作用（reconcile 循环天然会重复调用）；
//   - 错误必须可诊断：运行时不可用（如 docker daemon 未启动）要返回带
//     明确前缀的错误，便于排查与告警。
type ContainerRuntime interface {
	// EnsureRunning 确保 Pod 第 index 个副本的容器存在且运行：
	//   - 不存在 → 创建并启动；
	//   - 存在但停止 → 启动；
	//   - 已运行 → no-op（幂等）。
	EnsureRunning(pod metamanager.Pod, index int) error

	// EnsureStopped 确保 Pod 第 index 个副本的容器被停止并移除：
	//   - 存在 → 强制停止并删除；
	//   - 不存在 → no-op（幂等）。
	EnsureStopped(pod metamanager.Pod, index int) error

	// Inspect 查询 Pod 第 index 个副本容器的当前状态
	// （Running/Stopped/Absent/Unknown）。
	Inspect(pod metamanager.Pod, index int) (RuntimeState, error)

	// List 列出本运行时上由 Edged 管理的全部容器实例（含已停止的）。
	// reconciler 用它发现"本地存在但期望集合已删除"的孤儿容器，触发清理；
	// 也用它按 Pod 统计实际副本数（多副本收敛的依据）。
	List() ([]InstanceRef, error)

	// Close 释放运行时资源（如长连接）；无状态实现可返回 nil。
	Close() error
}
