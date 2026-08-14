package edged

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
)

// DefaultReconcileInterval 是默认调谐周期：期望状态来自 MetaManager 落盘数据，
// 5s 轮询即可满足"断网自治"场景（M2 验收：断网 30min 容器持续运行）。
const DefaultReconcileInterval = 5 * time.Second

// PodStatus 是单个 Pod 最近一次调谐的结果记录（供日志与后续 Pod 状态上报 WBS 6.3）。
type PodStatus struct {
	State         RuntimeState // 最近一次调谐后的容器状态
	Err           error        // 最近一次调谐错误（nil 表示成功）
	LastReconcile time.Time    // 最近一次调谐时间
}

// Edged 是 Pod 生命周期管理主循环（reconciler）。
//
// 工作原理（声明式调谐）：
//   - 期望状态：MetaManager 落盘的 Pod 集合（store.ListPods()）；
//   - 实际状态：运行时上的容器集合（rt.List()）；
//   - 每轮把实际状态向期望状态收敛：期望中存在 → EnsureRunning；
//     本地存在但期望已删除 → EnsureStopped（孤儿清理）。
//
// 启动即先调谐一次，之后按 interval 定时调谐；Stop() 可随时退出循环
// （测试与集成装配需要）。
//
// 说明：
//   - 当前用轮询起步，不依赖 MetaManager 增量订阅；订阅就绪后可在
//     SavePod/DeletePod 回调里触发 Notify() 立即调谐（接入点见 docs/EDGED-POC.md）；
//   - Pod.Replicas 在 POC 阶段按单副本处理（多副本展开属 M2 范围）。
type Edged struct {
	store    *metamanager.Store
	rt       ContainerRuntime
	interval time.Duration

	reconcileMu sync.Mutex // 串行化调谐（loop 与测试直调不并发）

	mu     sync.RWMutex // 保护 status 与生命周期字段
	status map[string]PodStatus

	triggerCh chan struct{} // 增量订阅触发信号（非阻塞，合并重复触发）

	stopCh chan struct{}
	doneCh chan struct{}
}

// New 创建 Edged。interval <= 0 时用 DefaultReconcileInterval。
func New(store *metamanager.Store, rt ContainerRuntime, interval time.Duration) *Edged {
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	return &Edged{
		store:     store,
		rt:        rt,
		interval:  interval,
		status:    make(map[string]PodStatus),
		triggerCh: make(chan struct{}, 1),
	}
}

// Start 启动调谐循环（goroutine），立即执行一轮调谐。
// 重复 Start 是 no-op（已启动时直接返回）。
func (e *Edged) Start() {
	e.mu.Lock()
	if e.stopCh != nil {
		e.mu.Unlock()
		return
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	e.stopCh, e.doneCh = stopCh, doneCh
	e.mu.Unlock()

	// 通道以参数传入 loop：loop 只读写本地参数，不触碰共享字段，
	// 避免与 Stop() 的 stopCh=nil 写入产生数据竞争（-race 可证）。
	go e.loop(stopCh, doneCh)
}

// loop 是调谐循环主体：启动即调谐一次，之后按周期调谐，直到 Stop() 关闭 stopCh。
func (e *Edged) loop(stopCh <-chan struct{}, doneCh chan<- struct{}) {
	defer close(doneCh)

	// 启动即先调谐一次（不等第一个 tick）；错误已记录在 Status 并输出日志
	_ = e.reconcileOnce()

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// 错误已记录在 Status 并输出日志，循环不因单轮失败退出
			_ = e.reconcileOnce()
		case <-e.triggerCh:
			// 增量订阅触发：MetaManager 有 Pod 变更时立即调谐一次（不等下个周期）
			_ = e.reconcileOnce()
		case <-stopCh:
			log.Infof("Edged 调谐循环已停止")
			return
		}
	}
}

// Trigger 请求立即执行一轮调谐（非阻塞；已有一轮在跑则合并）。
// 供 MetaManager 增量订阅等事件源调用，减少轮询延迟。
func (e *Edged) Trigger() {
	select {
	case e.triggerCh <- struct{}{}:
	default: // 已有待处理触发信号，合并
	}
}

// Stop 停止调谐循环并等待退出。可重复调用（第二次起 no-op）。
// Stop 后可用 Start 重新启动（测试场景）。
func (e *Edged) Stop() {
	e.mu.Lock()
	if e.stopCh == nil {
		e.mu.Unlock()
		return
	}
	ch := e.stopCh
	e.stopCh = nil
	e.mu.Unlock()

	close(ch)
	<-e.doneCh
}

// reconcileOnce 执行一轮完整调谐，返回错误（读取期望集合失败等全局错误；
// 单 Pod 失败记录在 Status 中，不中断本轮其他 Pod）。
// 导出给测试直调；循环内串行化，并发调用安全。
func (e *Edged) reconcileOnce() error {
	e.reconcileMu.Lock()
	defer e.reconcileMu.Unlock()

	now := time.Now()

	// 1. 期望集合：MetaManager 落盘的 Pod
	desired, err := e.desiredPods()
	if err != nil {
		log.Errorf("Edged reconcile: 读取期望 Pod 集合失败: %v", err)
		return err
	}

	// 2. 实际集合：运行时上的容器（List 失败不中断：只警告，清理留到下一轮）
	local, listErr := e.rt.List()
	if listErr != nil {
		log.Warnf("Edged reconcile: 列出本地容器失败（本轮跳过孤儿清理）: %v", listErr)
		local = nil
	}

	desiredKeys := make(map[string]struct{}, len(desired))
	for _, p := range desired {
		desiredKeys[podKey(p.Namespace, p.Name)] = struct{}{}
	}

	running, stopped, errCount, removed := 0, 0, 0, 0

	// 3. 期望存在 → 确保运行
	for _, pod := range desired {
		key := podKey(pod.Namespace, pod.Name)
		if err := e.rt.EnsureRunning(pod); err != nil {
			e.setStatus(key, StateUnknown, err, now)
			log.Errorf("Edged reconcile: 确保 Pod %s 运行失败: %v", key, err)
			errCount++
			continue
		}
		// 拉取真实状态（容器可能刚创建即退出，如一次性任务镜像）
		st, ierr := e.rt.Inspect(pod)
		if ierr != nil {
			e.setStatus(key, StateUnknown, ierr, now)
			errCount++
			continue
		}
		e.setStatus(key, st, nil, now)
		switch st {
		case StateRunning:
			running++
		case StateStopped:
			stopped++
		}
	}

	// 4. 孤儿清理：本地存在但期望集合已删除 → 确保停止
	for _, pod := range local {
		key := podKey(pod.Namespace, pod.Name)
		if _, ok := desiredKeys[key]; ok {
			continue // 期望中存在，交给第 3 步处理
		}
		if err := e.rt.EnsureStopped(pod); err != nil {
			e.setStatus(key, StateUnknown, err, now)
			log.Errorf("Edged reconcile: 清理孤儿容器 %s 失败: %v", key, err)
			errCount++
			continue
		}
		e.setStatus(key, StateAbsent, nil, now)
		removed++
	}

	// 5. 状态表清理（P2-1）：移除已不在期望集合的残留条目（策略见 cleanupStatus）
	e.cleanupStatus(desiredKeys, local, listErr != nil)

	// 6. 摘要日志
	if errCount > 0 {
		log.Warnf("reconcile: %d pods, %d running, %d stopped, %d error, %d removed",
			len(desired), running, stopped, errCount, removed)
	} else {
		log.Infof("reconcile: %d pods, %d running, %d stopped, %d error, %d removed",
			len(desired), running, stopped, errCount, removed)
	}
	return nil
}

// desiredPods 从 Store 读取并解析期望 Pod 集合。
// 单条 JSON 解析失败：记警告并跳过（不因脏数据中断整体调谐）。
func (e *Edged) desiredPods() ([]metamanager.Pod, error) {
	raw, err := e.store.ListPods()
	if err != nil {
		return nil, fmt.Errorf("ListPods 失败: %w", err)
	}
	pods := make([]metamanager.Pod, 0, len(raw))
	for _, j := range raw {
		var pod metamanager.Pod
		if err := json.Unmarshal([]byte(j), &pod); err != nil {
			log.Warnf("Edged reconcile: 跳过无法解析的 Pod 元数据: %v", err)
			continue
		}
		if pod.Name == "" {
			log.Warnf("Edged reconcile: 跳过缺少 name 的 Pod 元数据")
			continue
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

// setStatus 记录单个 Pod 的调谐结果（加锁写）。
func (e *Edged) setStatus(key string, state RuntimeState, err error, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status[key] = PodStatus{State: state, Err: err, LastReconcile: at}
}

// Status 返回全部 Pod 的最近调谐结果副本（调用方安全迭代）。
func (e *Edged) Status() map[string]PodStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]PodStatus, len(e.status))
	for k, v := range e.status {
		out[k] = v
	}
	return out
}
