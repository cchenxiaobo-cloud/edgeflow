package edged

import (
	"fmt"
	"sort"
	"sync"

	"edgeflow/edge/pkg/metamanager"
)

// MockRuntime 是 ContainerRuntime 的内存实现（测试/开发用）。
//
// 设计：
//   - 内部用 map[instanceKey]RuntimeState 模拟容器状态（键形如
//     "default/nginx#0"，# 后是副本序号），sync.Mutex 保证线程安全
//     （reconcile 循环与测试断言并发访问）；
//   - 记录每次调用的调用计数，供测试断言幂等性（createCalls 只在
//     Absent→Running 时自增，ensureRunningCalls 每次自增）；
//   - 支持按实例注入失败（fail map），模拟"容器异常"验证 reconcile 重试不 panic；
//   - SetState/DeleteState 供测试直接改写状态表，模拟容器被外部停止/删除；
//   - 记录每个实例"创建时"的镜像（images map），EnsureRunning 在已运行
//     时比对期望镜像（WBS 6.4 镜像漂移）：不一致 → 先停再建（重建）。
//     SetImage 供测试注入漂移（模拟外部把容器镜像改掉）。
type MockRuntime struct {
	mu sync.Mutex

	// states 是当前容器状态表：key 为 instanceKey，value 为状态。
	// 只有 Running/Stopped 的 key 算"容器存在"（List 返回）。
	states map[string]RuntimeState

	// images 是每个实例当前持有的镜像（key 同 states）：创建/重建时写入
	// pod.Image。SetImage 可改写（模拟镜像漂移）；未记录的 key 在
	// EnsureRunning 漂移检查中视为与期望一致（兼容旧测试语义：
	// 直接 SetState 注入状态、未记录镜像的实例不误重建）。
	images map[string]string

	// 调用计数（测试断言用）
	ensureRunningCalls map[string]int
	ensureStoppedCalls map[string]int
	createCalls        map[string]int // Absent→Running 的创建次数（幂等断言）
	removeCalls        map[string]int // 存在→Absent 的删除次数（幂等断言）

	// fail 是注入的失败：instanceKey → error。命中时 EnsureRunning/EnsureStopped 直接返回该错误。
	fail map[string]error

	// listErr 注入 List 错误（模拟运行时不可用）。
	listErr error

	closed bool
}

// NewMockRuntime 创建空的 MockRuntime。
func NewMockRuntime() *MockRuntime {
	return &MockRuntime{
		states:             make(map[string]RuntimeState),
		images:             make(map[string]string),
		ensureRunningCalls: make(map[string]int),
		ensureStoppedCalls: make(map[string]int),
		createCalls:        make(map[string]int),
		removeCalls:        make(map[string]int),
		fail:               make(map[string]error),
	}
}

// EnsureRunning 把 pod 第 index 个副本的状态置为 Running；已 Running 且
// 镜像与期望一致则 no-op（幂等）。镜像漂移处理（WBS 6.4）：已 Running 但
// 镜像与期望不一致 → 视为需要重建——先记一次 EnsureStopped/remove，再记
// 一次 create，镜像更新为期望值（与 DockerRuntime 的"先停再建"语义一致）。
func (m *MockRuntime) EnsureRunning(pod metamanager.Pod, index int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := instanceKey(pod.Namespace, pod.Name, index)
	if err := m.fail[key]; err != nil {
		return err
	}
	m.ensureRunningCalls[key]++
	st := m.states[key]
	// Absent→Running 算"创建"；Stopped→Running 是"重新启动"（幂等语义：不重建容器）
	if st == StateAbsent {
		m.createCalls[key]++
		m.images[key] = pod.Image
		m.states[key] = StateRunning
		return nil
	}
	// 镜像漂移重建（WBS 6.4）：已运行但镜像与期望不一致 → 先停再建。
	// images 未记录（空串）视为一致，不误重建（兼容 SetState 直改状态表的旧测试）。
	if st == StateRunning && m.images[key] != "" && m.images[key] != pod.Image {
		m.ensureStoppedCalls[key]++
		if m.states[key] != StateAbsent {
			m.removeCalls[key]++
		}
		m.createCalls[key]++
		m.images[key] = pod.Image
		m.states[key] = StateRunning
		return nil
	}
	m.states[key] = StateRunning
	return nil
}

// EnsureStopped 把 pod 第 index 个副本的状态置为 Absent（删除）；已 Absent 则 no-op（幂等）。
func (m *MockRuntime) EnsureStopped(pod metamanager.Pod, index int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := instanceKey(pod.Namespace, pod.Name, index)
	if err := m.fail[key]; err != nil {
		return err
	}
	m.ensureStoppedCalls[key]++
	if m.states[key] != StateAbsent {
		m.removeCalls[key]++
	}
	m.states[key] = StateAbsent
	return nil
}

// Inspect 返回 pod 第 index 个副本的当前状态。
func (m *MockRuntime) Inspect(pod metamanager.Pod, index int) (RuntimeState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return StateUnknown, fmt.Errorf("mock runtime 已关闭")
	}
	st, ok := m.states[instanceKey(pod.Namespace, pod.Name, index)]
	if !ok {
		return StateAbsent, nil
	}
	return st, nil
}

// List 返回所有"容器存在"（Running/Stopped）的实例，按 instanceKey 升序。
func (m *MockRuntime) List() ([]InstanceRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, fmt.Errorf("mock runtime 已关闭")
	}
	if m.listErr != nil {
		return nil, m.listErr
	}
	keys := make([]string, 0, len(m.states))
	for k, st := range m.states {
		if st != StateAbsent {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	insts := make([]InstanceRef, 0, len(keys))
	for _, k := range keys {
		insts = append(insts, parseInstanceKey(k))
	}
	return insts, nil
}

// Close 标记关闭：之后 Inspect/List 报错，Ensure 类操作不受影响（POC 简化）。
func (m *MockRuntime) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// SetState 直接设置某实例的状态（测试模拟外部变化，如容器被外部停止）。
// key 为 instanceKey（如 "default/nginx#0"）。
func (m *MockRuntime) SetState(key string, st RuntimeState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[key] = st
}

// DeleteState 删除某实例（测试模拟容器被外部删除）。
// key 为 instanceKey（如 "default/nginx#0"）。
func (m *MockRuntime) DeleteState(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, key)
}

// SetFail 注入失败：实例的 Ensure 操作将返回 err；err 为 nil 时清除注入。
// 按实例注入用 instanceKey(namespace, name, index)（如 "default/nginx#0"）。
func (m *MockRuntime) SetFail(key string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.fail, key)
		return
	}
	m.fail[key] = err
}

// SetListErr 注入 List 错误（模拟运行时不可用）；传 nil 清除。
func (m *MockRuntime) SetListErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listErr = err
}

// SetImage 设置实例当前镜像（测试模拟镜像漂移：创建后外部把容器镜像改掉）。
// key 为 instanceKey（如 "default/nginx#0"）；image 为空串表示清除记录
// （恢复"未记录即视为一致"语义）。
func (m *MockRuntime) SetImage(key, image string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.images[key] = image
}

// Image 返回实例当前记录的镜像（测试断言；未记录返回 ""）。
func (m *MockRuntime) Image(key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.images[key]
}

// ImageMatches 返回实例镜像是否与期望一致（WBS 6.4）：
// 未记录镜像（images 无此 key）视为一致；已记录则精确比对。
// 模拟"容器不存在"时也返回 (false, nil)（与 DockerRuntime 语义对齐，
// 容器不存在不算漂移）。
func (m *MockRuntime) ImageMatches(pod metamanager.Pod, index int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return false, fmt.Errorf("mock runtime 已关闭")
	}
	key := instanceKey(pod.Namespace, pod.Name, index)
	img, ok := m.images[key]
	if !ok || img == "" {
		return true, nil // 未记录：视为一致（不误报漂移）
	}
	return img == pod.Image, nil
}

// EnsureRunningCount 返回 EnsureRunning 的累计调用次数（测试断言）。
func (m *MockRuntime) EnsureRunningCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureRunningCalls[key]
}

// CreateCount 返回 Absent→Running 的创建次数（幂等断言：连续 reconcile 后应为 1）。
func (m *MockRuntime) CreateCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createCalls[key]
}

// EnsureStoppedCount 返回 EnsureStopped 的累计调用次数（测试断言）。
func (m *MockRuntime) EnsureStoppedCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureStoppedCalls[key]
}

// State 返回实例当前状态（测试断言）。
func (m *MockRuntime) State(key string) RuntimeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[key]
	if !ok {
		return StateAbsent
	}
	return st
}
