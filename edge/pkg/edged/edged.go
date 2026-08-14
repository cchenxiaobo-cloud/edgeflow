package edged

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
)

// DefaultReconcileInterval 是默认调谐周期：期望状态来自 MetaManager 落盘数据，
// 5s 轮询即可满足"断网自治"场景（M2 验收：断网 30min 容器持续运行）。
const DefaultReconcileInterval = 5 * time.Second

// DefaultRemovedRetention 是已删除 Pod 的 Absent 终态默认保留窗口（90s）：
// 覆盖 3 个默认上报周期（30s），保证删除终态至少被云端收到一次。
const DefaultRemovedRetention = 90 * time.Second

// CrashLoopBackOff 参数（P1-2 简化版：固定窗口，不做指数退避）：
//   - maxRestartBeforeBackoff：连续重启达到该次数后进入退避；
//   - backoffWindow：退避期内不再重启（跳过本轮）；
//   - stableWindow：副本稳定运行超过该时长后重置重启计数。
const (
	maxRestartBeforeBackoff = 3
	backoffWindow           = 30 * time.Second
	stableWindow            = 60 * time.Second
)

// restartRecord 是单个副本的重启历史。
type restartRecord struct {
	count       int       // 连续重启次数（稳定运行后归零）
	lastRestart time.Time // 最近一次重启时间
}

// PodStatus 是单个 Pod 最近一次调谐的结果记录（供日志与后续 Pod 状态上报 WBS 6.3）。
type PodStatus struct {
	State         RuntimeState // 最近一次调谐后的容器状态
	Err           error        // 最近一次调谐错误（nil 表示成功）
	LastReconcile time.Time    // 最近一次调谐时间
	RemovedAt     time.Time    // Pod 从期望集合删除的时间（零值=仍在期望集合）
	RestartCount  int          // 健康检查累计重启次数（WBS 6.5；M2 退避策略的输入）
}

// Edged 是 Pod 生命周期管理主循环（reconciler）。
//
// 工作原理（声明式调谐）：
//   - 期望状态：MetaManager 落盘的 Pod 集合（store.ListPods()），每个 Pod
//     的期望副本数为 Replicas（缺省 1）；
//   - 实际状态：运行时上的容器实例集合（rt.List()，按副本粒度）；
//   - 每轮把实际状态向期望状态收敛：期望副本缺失 → 补齐创建；多余副本 →
//     停止（从最大 index 开始）；期望副本存在但停止/未知 → 健康检查重启
//     （WBS 6.5 自愈）；本地存在但期望已删除 → EnsureStopped（孤儿清理）。
//
// 启动即先调谐一次，之后按 interval 定时调谐；Stop() 可随时退出循环
// （测试与集成装配需要）。
//
// 说明：
//   - 当前用轮询起步，不依赖 MetaManager 增量订阅；订阅就绪后可在
//     SavePod/DeletePod 回调里触发 Trigger() 立即调谐（接入点见 docs/EDGED-POC.md）；
//   - 健康检查重启无退避（POC 简化：每次调谐重试；M2 完整版加
//     CrashLoopBackOff 指数退避，见 docs/EDGED-POC.md §9）。
type Edged struct {
	store    *metamanager.Store
	rt       ContainerRuntime
	interval time.Duration

	reconcileMu sync.Mutex // 串行化调谐（loop 与测试直调不并发）

	// removedRetention 是已删除 Pod 的 Absent 终态保留窗口：
	// 删除后至少保留一个上报周期（默认 90s = 3×30s），确保 Absent
	// 终态能被上报到云端，之后条目才被清理（WBS 6.3/P2-1）。
	removedRetention time.Duration

	// restartRec 记录每个副本的重启历史（CrashLoopBackOff 输入，P1-2）。
	restartMu  sync.Mutex
	restartRec map[string]restartRecord

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
		store:            store,
		rt:               rt,
		interval:         interval,
		status:           make(map[string]PodStatus),
		triggerCh:        make(chan struct{}, 1),
		removedRetention: DefaultRemovedRetention,
		restartRec:       make(map[string]restartRecord),
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

	// 2. 实际集合：运行时上的容器实例（List 失败不中断：只警告，清理留到下一轮）
	local, listErr := e.rt.List()
	if listErr != nil {
		log.Warnf("Edged reconcile: 列出本地容器失败（本轮跳过孤儿清理）: %v", listErr)
		local = nil
	}

	desiredKeys := make(map[string]struct{}, len(desired))
	for _, p := range desired {
		desiredKeys[podKey(p.Namespace, p.Name)] = struct{}{}
	}

	running, stopped, errCount, removed, restarted := 0, 0, 0, 0, 0

	// 3. 期望存在 → 收敛副本数 + 健康检查自愈
	for _, pod := range desired {
		key := podKey(pod.Namespace, pod.Name)

		// 期望副本数：Replicas 缺省 1（云端未下发/历史数据无该字段）
		replicas := pod.Replicas
		if replicas <= 0 {
			replicas = 1
		}

		// 实际副本实例：runtime.List 按 Pod 过滤（按 index 升序，缺副本也收敛）
		actual := filterInstances(local, pod.Namespace, pod.Name)

		// 本 Pod 本轮最终状态（任一副本失败 → 以最近一次错误为准）
		podState, podErr := StateRunning, error(nil)

		// 3a. 收缩：实际 > 期望 → 停止多余副本（从最大 index 开始，
		//     保证缩容后剩余副本是 0..Replicas-1 的连续前缀）
		for i := len(actual) - 1; i >= replicas; i-- {
			inst := actual[i]
			if err := e.rt.EnsureStopped(inst.Pod(), inst.Index); err != nil {
				podState, podErr = StateUnknown, err
				log.Errorf("Edged reconcile: 收缩 Pod %s 多余副本 %d 失败: %v", key, inst.Index, err)
				errCount++
			}
		}

		// 3b. 补齐：实际 < 期望 → 创建缺失副本（index 从实际数开始；
		//     存在缺口时由 3c 的健康检查兜底补齐，EnsureRunning 幂等不重复创建）
		for idx := len(actual); idx < replicas; idx++ {
			if err := e.rt.EnsureRunning(pod, idx); err != nil {
				podState, podErr = StateUnknown, err
				log.Errorf("Edged reconcile: 创建 Pod %s 副本 %d 失败: %v", key, idx, err)
				errCount++
				continue
			}
		}

		// 3c. 健康检查自愈（WBS 6.5 + P1-2 CrashLoopBackOff）：逐个期望副本
		//     Inspect，非 Running 且无错误（Stopped/Unknown/Absent）→ 视为
		//     运行异常 → EnsureRunning 重启（幂等）。连续重启达到阈值后进入
		//     退避窗口（固定 30s，不做指数退避——POC 简化，注释标明演进方向）；
		//     副本稳定运行超过 stableWindow 后重置重启计数。
		//     镜像漂移重建（WBS 6.4 滚动更新）：StateRunning 时用
		//     ImageMatches 比对期望镜像，不一致 → EnsureRunning 重建
		//     （运行时内部先停再建）。滚动策略：按 index 顺序 0→N-1 逐副本
		//     处理，每轮最多重建 1 个漂移副本（批大小 1；无健康检查等待，
		//     下一轮调谐继续——简化版，跨 Pod 串行化留待增强）。
		restartedThisPod := 0
		driftRebuilt := false // 本轮是否已重建漂移副本（每轮最多 1 个）
		for idx := 0; idx < replicas; idx++ {
			st, ierr := e.rt.Inspect(pod, idx)
			if ierr != nil {
				podState, podErr = StateUnknown, ierr
				log.Errorf("Edged reconcile: 检查 Pod %s 副本 %d 状态失败: %v", key, idx, ierr)
				errCount++
				continue
			}
			instKey := instanceKey(pod.Namespace, pod.Name, idx)
			if st == StateRunning {
				running++
				// 稳定运行 → 重置重启计数（CrashLoopBackOff 恢复条件）
				e.restartMu.Lock()
				if rec, ok := e.restartRec[instKey]; ok && !rec.lastRestart.IsZero() &&
					now.Sub(rec.lastRestart) > stableWindow {
					delete(e.restartRec, instKey)
				}
				e.restartMu.Unlock()

				// 镜像漂移检测（WBS 6.4）：每轮最多重建 1 个漂移副本，
				// 保证"0→N-1 逐批滚动"的确定性顺序（批大小 1）。
				if driftRebuilt {
					continue
				}
				ok, ierr := e.rt.ImageMatches(pod, idx)
				if ierr != nil {
					podState, podErr = StateUnknown, ierr
					log.Errorf("Edged reconcile: 检查 Pod %s 副本 %d 镜像失败: %v", key, idx, ierr)
					errCount++
					continue
				}
				if !ok {
					// 镜像不一致 → 重建（EnsureRunning 内部先停旧容器再重建）。
					// 不计入 restartRec/RestartCount：这是期望变更驱动的主动
					// 重建，不是崩溃重启，不应触发 CrashLoopBackOff。
					if rerr := e.rt.EnsureRunning(pod, idx); rerr != nil {
						podState, podErr = StateUnknown, rerr
						log.Errorf("Edged reconcile: 镜像漂移重建 Pod %s 副本 %d 失败: %v", key, idx, rerr)
						errCount++
						continue
					}
					driftRebuilt = true
					restarted++ // 摘要统计：重建也是一种重启（RestartCount 不受影响）
					log.Infof("镜像漂移：Pod %s 副本 %d 已重建（滚动策略，每轮 1 个，顺序 0→N-1）", key, idx)
				}
				continue
			}
			// 运行异常（停止/未知/缺失）：先查退避状态
			e.restartMu.Lock()
			rec := e.restartRec[instKey]
			inBackoff := rec.count >= maxRestartBeforeBackoff &&
				now.Sub(rec.lastRestart) < backoffWindow
			e.restartMu.Unlock()
			if inBackoff {
				// 退避期内跳过重启（退出型镜像不再被每轮无限拉起）
				podState, podErr = StateStopped, fmt.Errorf(
					"CrashLoopBackOff: 副本 %d 连续重启 %d 次，退避至 %v",
					idx, rec.count, rec.lastRestart.Add(backoffWindow).Format("15:04:05"))
				log.Warnf("CrashLoopBackOff：Pod %s 副本 %d 进入退避（已重启 %d 次）",
					key, idx, rec.count)
				continue
			}
			// 退避期外 → 重启并记录
			if rerr := e.rt.EnsureRunning(pod, idx); rerr != nil {
				podState, podErr = StateUnknown, rerr
				log.Errorf("Edged reconcile: 健康检查重启 Pod %s 副本 %d 失败: %v", key, idx, rerr)
				errCount++
				continue
			}
			restarted++
			restartedThisPod++
			if st == StateStopped {
				stopped++ // 统计"发现时已停止"的副本数
			}
			e.restartMu.Lock()
			e.restartRec[instKey] = restartRecord{count: rec.count + 1, lastRestart: now}
			e.restartMu.Unlock()
			log.Infof("健康检查：容器 %s 已重启（累计 %d 次）",
				ContainerName(pod.Namespace, pod.Name, idx), rec.count+1)
		}

		// 汇总：重启先累加 RestartCount（addRestarts 置 Running），
		// 再以本轮最终状态覆盖（有失败则错误优先，RestartCount 不受影响）
		if restartedThisPod > 0 {
			e.addRestarts(key, restartedThisPod, now)
		}
		e.setStatus(key, podState, podErr, now)
	}

	// 3d. 旧命名实例迁移（M4A P1-1 修复）：Index<0 的容器是旧版无序号命名
	//     （edgeflow-<ns>-<name>），与新版 -0 槽位冲突，replicas≥2 时会
	//     每轮调谐创建/删除循环。无论是否在期望集合，一律优先移除——
	//     在期望集合中会被第 3 步以规范名重建，不在则等同孤儿清理。
	for _, inst := range local {
		if inst.Index >= 0 {
			continue
		}
		key := podKey(inst.Namespace, inst.Name)
		if err := e.rt.EnsureStopped(inst.Pod(), inst.Index); err != nil {
			e.setStatus(key, StateUnknown, err, now)
			log.Errorf("Edged reconcile: 清理旧命名容器 %s 失败: %v", key, err)
			errCount++
			continue
		}
		log.Infof("Edged reconcile: 已迁移旧命名容器 %s（重建为规范副本命名）", key)
		removed++
	}

	// 4. 孤儿清理：本地存在但期望集合已删除 → 确保停止（按副本实例逐个清理）
	for _, inst := range local {
		key := podKey(inst.Namespace, inst.Name)
		if _, ok := desiredKeys[key]; ok {
			continue // 期望中存在，交给第 3 步处理
		}
		if err := e.rt.EnsureStopped(inst.Pod(), inst.Index); err != nil {
			e.setStatus(key, StateUnknown, err, now)
			log.Errorf("Edged reconcile: 清理孤儿容器 %s 副本 %d 失败: %v", key, inst.Index, err)
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
		log.Warnf("reconcile: %d pods, %d running, %d stopped, %d restarted, %d error, %d removed",
			len(desired), running, stopped, restarted, errCount, removed)
	} else {
		log.Infof("reconcile: %d pods, %d running, %d stopped, %d restarted, %d error, %d removed",
			len(desired), running, stopped, restarted, errCount, removed)
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

// filterInstances 从 List 结果中筛出属于指定 Pod 的副本实例（按 index 升序）。
// 注意：List 返回的实例按 instanceKey 字典序（"#10" < "#2"），必须按数值重排，
// 否则"补齐从实际数开始"与"从最大 index 收缩"会错位。
func filterInstances(instances []InstanceRef, namespace, name string) []InstanceRef {
	out := make([]InstanceRef, 0, 2)
	for _, inst := range instances {
		if inst.Namespace == namespace && inst.Name == name {
			out = append(out, inst)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// setStatus 记录单个 Pod 的调谐结果（保留既有 RestartCount 与 RemovedAt，加锁写）。
func (e *Edged) setStatus(key string, state RuntimeState, err error, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	prev := e.status[key]
	e.status[key] = PodStatus{
		State:         state,
		Err:           err,
		LastReconcile: at,
		RemovedAt:     prev.RemovedAt,
		RestartCount:  prev.RestartCount,
	}
}

// addRestarts 把本轮健康检查重启次数累加到 Pod 的重启计数（WBS 6.5），
// 并把状态收敛为 Running（重启成功的副本已恢复运行）。
// 注意：调用方随后会以本轮最终状态覆盖，错误优先。
func (e *Edged) addRestarts(key string, n int, at time.Time) {
	if n <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.status[key]
	st.RestartCount += n
	st.State = StateRunning
	st.Err = nil
	st.LastReconcile = at
	e.status[key] = st
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
