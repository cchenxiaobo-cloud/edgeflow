package edged

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"edgeflow/edge/pkg/metamanager"
)

// MockRuntime 是 ContainerRuntime 的内存实现（测试/开发用）。
//
// 设计：
//   - 内部用 map[podKey]RuntimeState 模拟容器状态，sync.Mutex 保证线程安全
//     （reconcile 循环与测试断言并发访问）；
//   - 记录每次调用的调用计数，供测试断言幂等性（createCalls 只在
//     Absent→Running 时自增，ensureRunningCalls 每次自增）；
//   - 支持按 pod 注入失败（fail map），模拟"容器异常"验证 reconcile 重试不 panic。
type MockRuntime struct {
	mu sync.Mutex

	// states 是当前容器状态表：key 为 podKey，value 为状态。
	// 只有 Running/Stopped 的 key 算"容器存在"（List 返回）。
	states map[string]RuntimeState

	// 调用计数（测试断言用）
	ensureRunningCalls map[string]int
	ensureStoppedCalls map[string]int
	createCalls        map[string]int // Absent→Running 的创建次数（幂等断言）
	removeCalls        map[string]int // 存在→Absent 的删除次数（幂等断言）

	// fail 是注入的失败：podKey → error。命中时 EnsureRunning/EnsureStopped 直接返回该错误。
	fail map[string]error

	// listErr 注入 List 错误（模拟运行时不可用）。
	listErr error

	closed bool
}

// NewMockRuntime 创建空的 MockRuntime。
func NewMockRuntime() *MockRuntime {
	return &MockRuntime{
		states:             make(map[string]RuntimeState),
		ensureRunningCalls: make(map[string]int),
		ensureStoppedCalls: make(map[string]int),
		createCalls:        make(map[string]int),
		removeCalls:        make(map[string]int),
		fail:               make(map[string]error),
	}
}

// EnsureRunning 把 pod 状态置为 Running；已 Running 则 no-op（幂等）。
func (m *MockRuntime) EnsureRunning(pod metamanager.Pod) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := podKey(pod.Namespace, pod.Name)
	if err := m.fail[key]; err != nil {
		return err
	}
	m.ensureRunningCalls[key]++
	// 只有 Absent→Running 算"创建"；Stopped→Running 是"重新启动"（幂等语义：不重建容器）
	if m.states[key] == StateAbsent {
		m.createCalls[key]++
	}
	m.states[key] = StateRunning
	return nil
}

// EnsureStopped 把 pod 状态置为 Absent（删除）；已 Absent 则 no-op（幂等）。
func (m *MockRuntime) EnsureStopped(pod metamanager.Pod) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := podKey(pod.Namespace, pod.Name)
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

// Inspect 返回 pod 的当前状态。
func (m *MockRuntime) Inspect(pod metamanager.Pod) (RuntimeState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return StateUnknown, fmt.Errorf("mock runtime 已关闭")
	}
	st, ok := m.states[podKey(pod.Namespace, pod.Name)]
	if !ok {
		return StateAbsent, nil
	}
	return st, nil
}

// List 返回所有"容器存在"（Running/Stopped）的 pod，按 key 升序。
func (m *MockRuntime) List() ([]metamanager.Pod, error) {
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

	pods := make([]metamanager.Pod, 0, len(keys))
	for _, k := range keys {
		pods = append(pods, parsePodKey(k))
	}
	return pods, nil
}

// Close 标记关闭：之后 Inspect/List 报错，Ensure 类操作不受影响（POC 简化）。
func (m *MockRuntime) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// SetFail 注入失败：pod 的 Ensure 操作将返回 err；err 为 nil 时清除注入。
// failAll 传 nil；按 pod 注入用 podKey(namespace, name)。
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

// State 返回 pod 当前状态（测试断言）。
func (m *MockRuntime) State(key string) RuntimeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[key]
	if !ok {
		return StateAbsent
	}
	return st
}

// parsePodKey 把 podKey（<namespace>/<name>）拆回 Pod。
func parsePodKey(key string) metamanager.Pod {
	ns, name := "default", key
	if i := strings.IndexByte(key, '/'); i >= 0 {
		ns, name = key[:i], key[i+1:]
	}
	return metamanager.Pod{Namespace: ns, Name: name}
}
