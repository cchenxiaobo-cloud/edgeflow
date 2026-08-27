package modelrelease

// 发布控制器（设计 §5.4/§5.5，WBS-6/release.go；A3/C1/C2/C3 + P8 用例）。
//
// 每轮扫描（scanOnce，ticker 默认 5s，可注入时钟）：
//
//	1. 认领：内存中 pending 的 release → CAS pending→running（写 StartedAt）
//	   → 获取领跑锁（grant-per-claim，TTL 默认 60s 可配，D5 刷新绑定）→ 执行；
//	2. 推进（锁持有者）：按 NextBatchAt 与批次边界，批内逐节点**串行**
//	   DeployVersion（D6 维持串行；batchSize 控制批粒度与暂停节奏）；
//	   节点失败 → FailFast=true 立即中止（本批剩余+后续全部 skipped、
//	   head=failed、guard 释放）；FailFast=false 跑完全部批次后按失败计数
//	   判定 failed/succeeded；批完成 → NextBatchAt = now + pauseBetween
//	   写 head（持久化，跨接管保留节奏）；
//	3. canceled 收敛：head=canceled 且存在 pending perNode → 该轮补写
//	   skipped（幂等，≤1 扫描周期补齐，L27）；
//	4. 回滚执行：head.RollbackRequested → runRollback（逆序批次 + pause=0 +
//	   执行期复查 D2/D4 回滚 TOCTOU）；
//	5. 释放：终态 release → 删 guard 键（幂等；失败重入队下一轮重试）；
//	   lock 键到期自散（租约）。
//
// succeeded 前不变式（D8）：全部 TargetNodes 的 perNode 终态 deployed 且无
// failed（seeAllNodesDeployed）；不满足 → head=failed 暴露实现缺陷而非静默
// succeeded。
//
// 领跑锁：仅外部模式有意义（embed/内存单实例恒成功，锁逻辑空转无害——
// 装配注入 NoopLockKV）。接管语义：锁过期（领跑者崩溃）→ ≤TTL 后他副本
// 重新 grant → 从 perNode 首个 pending 批次继续（deployed 跳过），
// NextBatchAt 已持久化，节奏接续（L21 登记接管延迟上界）。
//
// 复杂度：单 release 串行；多 release 并行（每 release 一把锁互不阻塞）；
// 每轮 O(活跃 release × 批内节点)。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/pkg/log"
)

// 默认控制器参数（设计 §9.3；环境变量解析归装配层 cmd/cloudcore）：
const (
	// DefaultScanInterval 是发布控制器扫描周期默认值（env
	// EDGEFLOW_CLOUDCORE_RELEASE_SCAN_INTERVAL 默认 5s）。
	DefaultScanInterval = 5 * time.Second
	// DefaultLockTTL 是领跑锁租约 TTL 默认值（env
	// EDGEFLOW_CLOUDCORE_RELEASE_LOCK_TTL 默认 60s）。
	DefaultLockTTL = 60 * time.Second
	// MinLockTTL 是领跑锁 TTL 下限（>=15s，非法 fail-fast，设计 §9.3；
	// D5：TTL ≥ 3×refresh 护栏在此下限上恒成立）。
	MinLockTTL = 15 * time.Second
	// DefaultGCKeep 是终态发布 GC 保留条数默认值（v0.8.0，L28，env
	// EDGEFLOW_CLOUDCORE_RELEASE_GC_KEEP 默认 100；仅 GC 开启时消费）。
	DefaultGCKeep = 100
)

// RefreshPeriod 计算锁刷新周期（主线裁决 D5）：refresh = max(5s, TTL/3)。
// 校验 `TTL ≥ 3×refresh`：TTL=15s → refresh=5s 自洽；默认 TTL=60s →
// refresh=20s 保持原节奏。刷新走独立定时器（refreshLoop），与批内节点
// 部署解耦（防慢节点饿死刷新）。
func RefreshPeriod(ttl time.Duration) time.Duration {
	r := ttl / 3
	if r < 5*time.Second {
		r = 5 * time.Second
	}
	return r
}

// Options 是控制器构造选项（设计 WBS-6）：ScanInterval/LockTTL 可配；
// Now 可注入 fake 时钟（测试推进 pause 计时），nil → time.Now。
type Options struct {
	ScanInterval time.Duration
	LockTTL      time.Duration
	Now          func() time.Time
	// GCEnabled 终态发布 GC 开关（v0.8.0，L28；默认 false——L31 审计口径：
	// 终态 release 键永久保留。开启后按 GCKeep 保留最近 N 条终态，旧终态
	// 及其逐节点结果被清理，审计痕迹以 ops 台账为准）。
	GCEnabled bool
	// GCKeep 终态保留条数（默认 100，≥1；仅 GCEnabled 时消费）。
	GCKeep int
	// DigestLookup 返回节点最近上报的镜像 digest（v0.11.0，R-1+）；nil =
	// 关闭 digest 校验（回归锚点）。配合发布头 MirrorDigest：期望非空且
	// lookup 非空且不等 → perNode failed（reason=digest-mismatch）。
	DigestLookup func(nodeID string) string
	// BatchParallel 批内并行部署度（v0.10.0，D6）：默认 1 = 逐节点串行
	// （与 v0.7.0-v0.9.0 行为逐字节一致）；>1 时批内节点并行 DeployVersion
	// （信号量限流，min(BatchParallel, 批大小)）。failFast 语义：并发下
	// 本批在途节点跑完、后续批次中止（与串行的"失败即停"语义收敛为同一
	// 终态 head=failed + 未部署节点 skipped）。
	BatchParallel int
}

// withDefaults 填充零值（ScanInterval 默认 5s；LockTTL 默认 60s）。
func (o Options) withDefaults() Options {
	if o.ScanInterval <= 0 {
		o.ScanInterval = DefaultScanInterval
	}
	if o.LockTTL <= 0 {
		o.LockTTL = DefaultLockTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// errNotPending 是认领 mutate 的守卫哨兵：release 已非 pending（他副本
// 已认领 / API 已取消）→ 静默跳过。
var errNotPending = errors.New("modelrelease: release not pending")

// errHeadChanged 是 head mutate 的守卫哨兵：并发写者（cancel/rollback/
// 他副本）已改动 head → 放弃本次提交，下轮按新状态收敛。
var errHeadChanged = errors.New("modelrelease: release head changed concurrently")

// Controller 是灰度发布控制器（三模式通用实现；锁能力按模式注入）。
type Controller struct {
	store   modelrepo.ModelStore
	deploy  VersionDeployer
	locks   LockKV
	opts    Options
	refresh time.Duration // D5：refresh = max(5s, LockTTL/3)

	mu     sync.Mutex
	active map[string]struct{} // 本副本正在执行（已持有领跑锁）的 release ID
}

// NewController 构造控制器。store/deploy/locks 均必填（nil → error，
// fail-fast 对齐既有构造风格）；opts 零值用默认；LockTTL < MinLockTTL →
// error（D5 护栏）。
func NewController(store modelrepo.ModelStore, deploy VersionDeployer, locks LockKV, opts Options) (*Controller, error) {
	if store == nil {
		return nil, fmt.Errorf("modelrelease: NewController 需要非 nil 的 ModelStore")
	}
	if deploy == nil {
		return nil, fmt.Errorf("modelrelease: NewController 需要非 nil 的 VersionDeployer")
	}
	if locks == nil {
		return nil, fmt.Errorf("modelrelease: NewController 需要非 nil 的 LockKV")
	}
	opts = opts.withDefaults()
	if opts.LockTTL < MinLockTTL {
		return nil, fmt.Errorf("modelrelease: LockTTL 必须 >= %v（D5：TTL ≥ 3×refresh 护栏），收到 %v", MinLockTTL, opts.LockTTL)
	}
	return &Controller{
		store:   store,
		deploy:  deploy,
		locks:   locks,
		opts:    opts,
		refresh: RefreshPeriod(opts.LockTTL),
		active:  make(map[string]struct{}),
	}, nil
}

// nowMs 是控制器时钟的毫秒形态。
func (c *Controller) nowMs() int64 { return c.opts.Now().UnixMilli() }

// ── 主循环 ─────────────────────────────────────────────────────────────

// Run 启动控制器：扫描 ticker（默认 5s）+ 锁刷新独立定时器（D5，周期
// max(5s, LockTTL/3)）。ctx 取消即退出（幂等）。装配层以 goroutine 调用。
func (c *Controller) Run(ctx context.Context) {
	go c.refreshLoop(ctx)
	log.Infof("[modelrelease] 发布控制器启动：scan=%v lockTTL=%v refresh=%v", c.opts.ScanInterval, c.opts.LockTTL, c.refresh)
	t := time.NewTicker(c.opts.ScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Infof("[modelrelease] 发布控制器退出（ctx 取消）")
			return
		case <-t.C:
			c.scanOnce(ctx)
		}
	}
}

// refreshLoop 是领跑锁刷新独立定时器（D5：与批内节点部署解耦，防慢节点
// 饿死刷新——批内单节点最坏 ~10s+ 超时也不会中断锁刷新节奏）。
func (c *Controller) refreshLoop(ctx context.Context) {
	t := time.NewTicker(c.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refreshActiveLocks(ctx)
		}
	}
}

// refreshActiveLocks 对全部 active release 重绑租约（grant-per-claim）。
// 返回 false（锁被他副本接管）→ 本副本停止执行该 release（R-2 双执行者
// 窗口收敛：部署幂等 + perNode CAS，正确性不受损）。
func (c *Controller) refreshActiveLocks(ctx context.Context) {
	for _, id := range c.activeSnapshot() {
		ok, err := c.locks.TryAcquire(ctx, modelrepo.ReleaseLockKey(id), []byte(id), c.opts.LockTTL)
		if err != nil {
			log.Warnf("[modelrelease] 锁刷新失败（release %s，下轮重试）: %v", id, err)
			continue
		}
		if !ok {
			log.Warnf("[modelrelease] 领跑锁被接管（release %s），本副本停止执行", id)
			c.removeActive(id)
		}
	}
}

// scanOnce 执行一轮扫描（导出形态供测试直调；ticker 循环调用）。
func (c *Controller) scanOnce(ctx context.Context) {
	releases, err := c.store.ListReleases(ctx, "")
	if err != nil {
		log.Warnf("[modelrelease] 扫描 release 列表失败（本轮跳过）: %v", err)
		return
	}
	for i := range releases {
		r := &releases[i]
		if r.RollbackRequested {
			// 回滚优先（同一控制器循环内互斥：开始回滚后不再推进正常批次，
			// 设计 §5.5；仅锁持有者执行）
			if c.ensureActive(ctx, r.ID) {
				c.runRollback(ctx, r.ID)
			}
			continue
		}
		switch r.Status {
		case modelrepo.ReleaseStatusPending:
			// v0.16.0 定时维护窗口：未到点的发布不认领、不占领跑锁
			// （NotBeforeMs=0 恒立即，缺省行为与 v0.15.0 逐字节一致）。
			if r.NotBeforeMs > 0 && r.NotBeforeMs > c.nowMs() {
				continue
			}
			c.claim(ctx, r)
		case modelrepo.ReleaseStatusRunning:
			if c.isActive(r.ID) {
				c.advance(ctx, r.ID)
			} else if c.ensureActive(ctx, r.ID) {
				c.advance(ctx, r.ID) // 接管：锁到期后他副本重新 grant 续跑
			}
		case modelrepo.ReleaseStatusPaused:
			// v0.16.0 暂停态：非终态仍占 guard/锁语义；本副本若持锁则
			// 保留 active 身份（refresh 循环续租），但不推进批次——恢复
			// 由 resume API 置回 running 后按正常路径接管。
			if !c.isActive(r.ID) {
				_ = c.ensureActive(ctx, r.ID)
			}
		case modelrepo.ReleaseStatusCanceled:
			c.cancelRemainder(ctx, r.ID)
			// canceled 亦是终态：轮内即释放同模型 guard（P8①：≤1 扫描
			// 周期 guard 释放，同模型可再次发布）
			c.releaseGuard(ctx, r)
		default: // 终态（succeeded/failed/rolled_back）
			c.releaseGuard(ctx, r)
			c.gcIfEnabled(ctx, r)
		}
	}
}

// ── 认领与锁 ───────────────────────────────────────────────────────────

// claim 认领 release（设计 §5.4 step1）：CAS pending→running（写
// StartedAt）；冲突（他副本已认领/API 已取消）→ 静默跳过；成功后获取
// 领跑锁并入 active，接着推进第一批。
func (c *Controller) claim(ctx context.Context, r *modelrepo.ModelRelease) {
	now := c.nowMs()
	err := c.store.UpdateReleaseHead(ctx, r.ID, func(h *modelrepo.ModelRelease) error {
		if h.Status != modelrepo.ReleaseStatusPending {
			return errNotPending
		}
		h.Status = modelrepo.ReleaseStatusRunning
		h.StartedAt = now
		// v0.18.0：认领即启动事件（首个非 created 事件；created 由 API 层
		// 创建时写入。直置 store 的老数据无 created 也无妨——开放集合容忍缺序）。
		h.AppendEvent(modelrepo.EventCreated, "claimed by controller", now)
		return nil
	})
	if err != nil {
		if !errors.Is(err, errNotPending) {
			log.Warnf("[modelrelease] 认领 release %s 失败（下轮重试）: %v", r.ID, err)
		}
		return
	}
	log.Infof("[modelrelease] 认领 release %s（model=%s version=%s target=%d nodes batchSize=%d）", r.ID, r.Model, r.Version, len(r.TargetNodes), r.BatchSize)
	if c.ensureActive(ctx, r.ID) {
		c.advance(ctx, r.ID)
	}
}

// ensureActive 确保本副本持有该 release 的领跑锁并加入 active 集合：
// 已 active → true；TryAcquire 失败（他副本持有）或后端错误 → false
// （本副本不推进；下轮重试）。
func (c *Controller) ensureActive(ctx context.Context, id string) bool {
	if c.isActive(id) {
		return true
	}
	ok, err := c.locks.TryAcquire(ctx, modelrepo.ReleaseLockKey(id), []byte(id), c.opts.LockTTL)
	if err != nil {
		log.Warnf("[modelrelease] 获取领跑锁失败（release %s，下轮重试）: %v", id, err)
		return false
	}
	if !ok {
		log.Infof("[modelrelease] 领跑锁被占用（release %s），本副本不推进", id)
		return false
	}
	c.mu.Lock()
	c.active[id] = struct{}{}
	c.mu.Unlock()
	log.Infof("[modelrelease] 获取领跑锁 release %s（grant-per-claim，TTL=%v）", id, c.opts.LockTTL)
	return true
}

func (c *Controller) isActive(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.active[id]
	return ok
}

func (c *Controller) removeActive(id string) {
	c.mu.Lock()
	delete(c.active, id)
	c.mu.Unlock()
}

func (c *Controller) activeSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.active))
	for id := range c.active {
		out = append(out, id)
	}
	return out
}

// ── 批调度推进（设计 §5.4 step2）───────────────────────────────────────

// advance 推进一个批次（锁持有者调用；每轮至多一个批次，跨轮推进节奏 =
// scan ticker；批间暂停经 NextBatchAt 生效）。
func (c *Controller) advance(ctx context.Context, id string) {
	h, err := c.store.GetRelease(ctx, id)
	if err != nil {
		if errors.Is(err, modelrepo.ErrReleaseNotFound) {
			c.removeActive(id)
		}
		return
	}
	if h.Status != modelrepo.ReleaseStatusRunning {
		if h.Status == modelrepo.ReleaseStatusPaused {
			// v0.16.0 暂停中：保留 active 身份（领跑锁刷新循环继续续租，
			// 多副本接管竞争不变），仅停止推进批次；resume 置回 running
			// 后下一扫描周期从原节奏继续。
			return
		}
		c.removeActive(id) // 已终态/已取消：停止推进（cancelRemainder 等路径收敛）
		return
	}
	if h.NextBatchAt > c.nowMs() {
		return // 批间暂停中（pause 跨接管持久于 NextBatchAt，设计 §5.4）
	}
	results, err := c.store.ListNodeResults(ctx, id)
	if err != nil {
		log.Warnf("[modelrelease] 读取 perNode 失败（release %s，本轮跳过）: %v", id, err)
		return
	}
	byNode := make(map[string]modelrepo.NodeReleaseResult, len(results))
	for _, nr := range results {
		byNode[nr.NodeID] = nr
	}
	// 推进期 digest 复查（v0.11.0，R-1+ 接入点②）：对已 deployed 节点复核
	// 上报 digest 与发布头期望一致；mismatch 已由 digestMismatch 写 perNode
	// failed——failFast 下中止（后续批次 skipped），否则继续（终态按失败
	// 计数判定）。
	for _, nodeID := range h.TargetNodes {
		nr, ok := byNode[nodeID]
		if !ok || nr.Status != modelrepo.NodeRelDeployed {
			continue
		}
		if ok, reason := c.digestMismatch(ctx, id, h, nodeID); ok {
			log.Warnf("[modelrelease] 推进期 digest 复核失败（release %s, node %s）: %s", id, nodeID, reason)
			if h.FailFast {
				c.abortFailFast(ctx, id, h, nodeID, reason, byNode)
				return
			}
		}
	}
	batches := BuildBatches(h.TargetNodes, h.BatchSize)
	bi := firstUnexecutedBatch(batches, byNode)
	if bi < 0 {
		c.finish(ctx, h, results) // 全部批次已执行 → 判定终态（D8 不变式）
		return
	}
	// 批次边界复查（cancel 语义 = 批次边界生效，设计 §5.4/§2.4：暂停期间
	// 被 cancel → 本处读取到 canceled → 取消生效，剩余 pending 由
	// cancelRemainder 补写 skipped）
	cur, err := c.store.GetRelease(ctx, id)
	if err != nil {
		return
	}
	if cur.Status != modelrepo.ReleaseStatusRunning {
		if cur.Status != modelrepo.ReleaseStatusPaused {
			c.removeActive(id)
		}
		// v0.16.0：部署下一批前复查到 paused → 本批不发车（批边界生效）；
		// 保留 active 身份同上。
		return
	}

	batch := batches[bi]
	now := c.nowMs()
	failNode, failReason := c.deployBatchNodes(ctx, id, h, batch, byNode)

	if failNode != "" {
		// fail-fast：本批剩余与后续全部标 skipped，head 置 failed（§5.4）
		c.abortFailFast(ctx, id, h, failNode, failReason, byNode)
		return
	}
	// v0.18.0：批次完成事件（时间线，环形截断随 AppendEvent；无失败时
	// Detail 记成功计数）。事件写入失败不阻断发布（尽力而为）。
	if err := c.store.UpdateReleaseHead(ctx, id, func(hh *modelrepo.ModelRelease) error {
		if hh.Status != modelrepo.ReleaseStatusRunning {
			return errHeadChanged
		}
		hh.AppendEvent(modelrepo.EventBatchDone, fmt.Sprintf("batch %d/%d done", bi+1, len(batches)), now)
		return nil
	}); err != nil && !errors.Is(err, errHeadChanged) {
		log.Warnf("[modelrelease] 写 batch_done 事件失败（release %s）: %v", id, err)
	}
	if bi == len(batches)-1 {
		// 最后一批完成（无失败或 failFast=false 已排除中止）：读最新
		// perNode（含本轮写入）判定终态
		done, err := c.store.ListNodeResults(ctx, id)
		if err != nil {
			return
		}
		c.finish(ctx, h, done)
		return
	}
	// v0.18.0：失败预算检查（failFast=false 跑完判定模式下有累计窗口；
	// 预算禁用时零开销——Len(results)==0 同义跳过）。达标且未终态 → 自动
	// 暂停（复用 paused 状态机，人可介入排查后 resume 续跑）；置 AutoPausedAt
	// + autopause 事件。预算派生自 perNode 事实源（ ListNodeResults 实时读），
	// 不持久化计数避免双写漂移。
	if h.FailureBudget >= 1 {
		latest, err := c.store.ListNodeResults(ctx, id)
		if err == nil && SummarizeNodes(latest).Failed >= h.FailureBudget {
			if perr := c.store.UpdateReleaseHead(ctx, id, func(hh *modelrepo.ModelRelease) error {
				if hh.Status != modelrepo.ReleaseStatusRunning || hh.RollbackRequested {
					return errHeadChanged
				}
				nowAP := c.nowMs()
				hh.Status = modelrepo.ReleaseStatusPaused
				hh.AutoPausedAt = nowAP
				hh.PausedAt = nowAP
				nap := SummarizeNodes(latest).Failed
				hh.AppendEvent(modelrepo.EventAutoPause,
					fmt.Sprintf("failure budget %d reached (failed=%d); manual resume to continue", hh.FailureBudget, nap), nowAP)
				return nil
			}); perr != nil && !errors.Is(perr, errHeadChanged) {
				log.Warnf("[modelrelease] 失败预算自动暂停写 head 失败（release %s，下轮重查）: %v", id, perr)
			} else if perr == nil {
				log.Infof("[modelrelease] release %s 触发失败预算自动暂停（budget=%d），等待人工 resume", id, h.FailureBudget)
				return // 本轮终止推进；resume 后继续剩余批次
			}
		}
	}
	// 批间节奏持久化（跨接管保留；N-3 登记：本写与批完成之间存在崩溃窗口，
	// 崩溃后接管读到旧 NextBatchAt（过去时刻）→ 下一批立即开跑，该次
	// pause 丢失，影响轻微，不改设计）
	next := now + h.PauseBetween
	if err := c.store.UpdateReleaseHead(ctx, id, func(hh *modelrepo.ModelRelease) error {
		if hh.Status != modelrepo.ReleaseStatusRunning || hh.RollbackRequested {
			return errHeadChanged
		}
		hh.NextBatchAt = next
		return nil
	}); err != nil && !errors.Is(err, errHeadChanged) {
		log.Warnf("[modelrelease] 写 NextBatchAt 失败（release %s，下轮重试）: %v", id, err)
	}
	if h.PauseBetween > 0 {
		log.Infof("[modelrelease] 批次 %d 完成（release %s），暂停 %dms（NextBatchAt=%d）", bi+1, id, h.PauseBetween, next)
	}
}

// firstUnexecutedBatch 返回第一个含未执行（missing 或 pending）节点的
// 批次下标；全部已执行 → -1。接管语义：从首个含 pending 的批次继续，
// 已 deployed 批次整体跳过（设计 §5.4 接管续跑）。
func firstUnexecutedBatch(batches [][]string, byNode map[string]modelrepo.NodeReleaseResult) int {
	for i, b := range batches {
		for _, nodeID := range b {
			nr, ok := byNode[nodeID]
			if !ok || nr.Status == modelrepo.NodeRelPending {
				return i
			}
		}
	}
	return -1
}

// finish 判定发布终态（设计 §5.4 + 主线裁决 D8）：
//   - 存在 failed 节点（failFast=false 跑完全部批次）→ failed；
//   - 否则 D8 不变式前置断言：全部 TargetNodes perNode 终态 deployed 且
//     无 failed——不满足 → head=failed（暴露实现缺陷，绝不静默 succeeded）；
//   - 通过 → succeeded。
func (c *Controller) finish(ctx context.Context, h *modelrepo.ModelRelease, results []modelrepo.NodeReleaseResult) {
	// digest 终态复核（v0.11.0，R-1+ 接入点③）：对入参 results 中 deployed
	// 节点逐一比对上报 digest；mismatch 写 perNode failed（幂等：置 failed
	// 后不再被复核查到）后重新读取最新 perNode 再走既有判定。
	for _, nr := range results {
		if nr.Status != modelrepo.NodeRelDeployed {
			continue
		}
		if ok, reason := c.digestMismatch(ctx, h.ID, h, nr.NodeID); ok {
			log.Warnf("[modelrelease] 终态 digest 复核失败（release %s, node %s）: %s", h.ID, nr.NodeID, reason)
		}
	}
	if latest, err := c.store.ListNodeResults(ctx, h.ID); err == nil {
		results = latest // 刷新为复核后的最新 perNode（v0.12.0 F-1：修复 v0.11.0 shadow 自赋值，终态判定不再基于陈旧快照）
	} else {
		log.Warnf("[modelrelease] 终态复核后读 perNode 失败（release %s，按入参判定）: %v", h.ID, err)
	}
	sum := SummarizeNodes(results)
	if sum.Failed > 0 {
		c.setTerminal(ctx, h.ID, modelrepo.ReleaseStatusFailed, fmt.Sprintf("%d node(s) failed", sum.Failed))
		return
	}
	if ok, node := AllNodesDeployed(h.TargetNodes, results); !ok {
		log.Errorf("[modelrelease] D8 不变式失败：node %s 缺少 deployed perNode（release %s）——head 置 failed 暴露实现缺陷", node, h.ID)
		c.setTerminal(ctx, h.ID, modelrepo.ReleaseStatusFailed, fmt.Sprintf("invariant check failed: node %s not deployed", node))
		return
	}
	c.setTerminal(ctx, h.ID, modelrepo.ReleaseStatusSucceeded, "")
}

// setTerminal 把 running head 置为终态（FinishedAt + FailureReason）。
// CAS 守卫：仅 running 且未置回滚的 head 可被推进（防 cancel/rollback
// 并发覆盖——cancel API 先写 canceled，本处冲突后放弃，下轮按 canceled
// 收敛）。终态后从 active 移除（锁停止刷新，租约到期自散）。
func (c *Controller) setTerminal(ctx context.Context, id string, st modelrepo.ReleaseStatus, reason string) {
	err := c.store.UpdateReleaseHead(ctx, id, func(h *modelrepo.ModelRelease) error {
		if h.Status != modelrepo.ReleaseStatusRunning || h.RollbackRequested {
			return errHeadChanged
		}
		h.Status = st
		h.FailureReason = reason
		h.FinishedAt = c.nowMs()
		// v0.18.0：终态事件（reason 直带入 Detail，含 D8 不变式失败文本）
		detail := reason
		if detail == "" {
			detail = string(st)
		}
		h.AppendEvent(modelrepo.EventTerminal, detail, h.FinishedAt)
		return nil
	})
	if err != nil {
		if !errors.Is(err, errHeadChanged) {
			log.Warnf("[modelrelease] 置终态失败（release %s，下轮重试）: %v", id, err)
		}
		return
	}
	c.removeActive(id)
	log.Infof("[modelrelease] release %s → %s（%s）", id, st, reason)
}

// abortFailFast 执行 fail-fast 中止（设计 §5.4）：本批剩余与后续全部
// pending 节点标 skipped（幂等；已 deployed/failed 不动），head 置 failed
// （perNode 先写、head 后写——写序 crash-safe）。
//
// 注意：byNode 是 advance 进入本批前的快照，**不包含本批内刚部署成功
// 的节点**（SetNodeResult 在循环内写库，快照不随之更新）——因此跳过
// 判据只看"快照中已非 pending"；本批成功节点因不在快照中不会被误标，
// 但为防御并发写（他副本接管窗口），标 skipped 前先读最新 perNode，
// 仍 pending/缺失才标（missing 表示创建期预写缺失，等同 pending）。
// deployBatchNodes 部署一批节点（v0.10.0，D6：批内可并行）。
// parallel = min(BatchParallel, 批大小)；=1 时为逐节点串行（与旧版逐字节
// 一致）。返回 (首个失败节点, 失败原因)；failFast 下并发版本批在途节点
// 全部执行完（后续批次由调用方 abortFailFast 中止）。
func (c *Controller) deployBatchNodes(ctx context.Context, id string, h *modelrepo.ModelRelease, batch []string, byNode map[string]modelrepo.NodeReleaseResult) (string, string) {
	parallel := c.opts.BatchParallel
	if parallel < 1 {
		parallel = 1
	}
	if parallel > len(batch) {
		parallel = len(batch)
	}
	if parallel == 1 {
		// 串行路径（回归锚点：与 v0.7.0-v0.9.0 逐字节一致）
		for _, nodeID := range batch {
			failNode, failReason := c.deployBatchNode(ctx, id, h, nodeID, byNode)
			if failNode != "" {
				if h.FailFast {
					return failNode, failReason
				}
				// failFast=false：记录首个失败继续（与旧版一致，最终按失败计数判定）
				if failReason != "" {
					continue
				}
			}
		}
		return "", ""
	}
	// 并行路径：信号量限流 + WaitGroup；failFast 下本批在途全部执行完
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	firstNode, firstReason := "", ""
	recordFail := func(nodeID, reason string) {
		mu.Lock()
		if firstNode == "" {
			firstNode, firstReason = nodeID, reason
		}
		mu.Unlock()
	}
	for _, nodeID := range batch {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			failNode, failReason := c.deployBatchNode(ctx, id, h, n, byNode)
			if failNode != "" && h.FailFast {
				recordFail(failNode, failReason)
			}
		}(nodeID)
	}
	wg.Wait()
	return firstNode, firstReason
}

// digestMismatch 校验节点上报 digest 与发布头期望 digest（v0.11.0，
// R-1+）。三跳过（任一成立返回 false, ""）：
//   - 控制器 DigestLookup 未注入（nil）→ 整体关闭（回归锚点）；
//   - 发布头 MirrorDigest 空（off 模式/HEAD 缺头/warn 失败）→ 全链路跳过；
//   - 节点上报 digest 空（老边缘/未上报）→ 该节点跳过，不误伤不阻塞。
//
// 不一致时写 perNode failed（Reason="digest-mismatch: expected <exp> got
// <got>"）并返回 (true, reason)。幂等：节点已 failed 后不再被复核查到。
func (c *Controller) digestMismatch(ctx context.Context, id string, h *modelrepo.ModelRelease, nodeID string) (bool, string) {
	if c.opts.DigestLookup == nil || h.MirrorDigest == "" {
		return false, ""
	}
	got := c.opts.DigestLookup(nodeID)
	if got == "" {
		return false, ""
	}
	if got == h.MirrorDigest {
		return false, ""
	}
	reason := fmt.Sprintf("digest-mismatch: expected %s got %s", h.MirrorDigest, got)
	_ = c.store.SetNodeResult(ctx, id, nodeID, &modelrepo.NodeReleaseResult{
		NodeID: nodeID, Status: modelrepo.NodeRelFailed, Version: h.Version, Reason: reason,
	})
	return true, reason
}

// deployBatchNode 部署单节点（返回失败节点与原因；成功返回 "", ""）。
func (c *Controller) deployBatchNode(ctx context.Context, id string, h *modelrepo.ModelRelease, nodeID string, byNode map[string]modelrepo.NodeReleaseResult) (string, string) {
	// 接管续跑：已 deployed 跳过（6.1 重试语义：对 failed 节点不自动
	// 重试——人工决策重新发布或回滚；deployed 判据在 byNode 快照内）
	if nr, ok := byNode[nodeID]; ok && nr.Status != modelrepo.NodeRelPending {
		return "", ""
	}
	ver, err := c.store.GetVersion(ctx, h.Model, h.Version)
	if err != nil {
		// 目标版本运行期不可用（被删等异常）：按节点失败处理
		reason := fmt.Sprintf("target version %s unavailable: %v", h.Version, err)
		_ = c.store.SetNodeResult(ctx, id, nodeID, &modelrepo.NodeReleaseResult{
			NodeID: nodeID, Status: modelrepo.NodeRelFailed, Version: h.Version, Reason: reason,
		})
		log.Warnf("[modelrelease] node %s 部署失败（release %s）: %s", nodeID, id, reason)
		if h.FailFast {
			return nodeID, reason
		}
		return "", ""
	}
	if err := c.deploy.DeployVersion(ctx, nodeID, id, ver); err != nil {
		reason := DeployReason(err)
		_ = c.store.SetNodeResult(ctx, id, nodeID, &modelrepo.NodeReleaseResult{
			NodeID: nodeID, Status: modelrepo.NodeRelFailed, Version: h.Version, Reason: reason,
		})
		log.Warnf("[modelrelease] node %s 部署失败（release %s）: %s", nodeID, id, reason)
		if h.FailFast {
			return nodeID, reason
		}
		return "", ""
	}
	// 部署即时 digest 检查（v0.11.0，R-1+ 接入点①）：上报已可见且不一致
	// → 当轮按节点失败处理（failFast 立即可中止；SetNodeResult 已由
	// digestMismatch 写成 failed）。
	if ok, reason := c.digestMismatch(ctx, id, h, nodeID); ok {
		log.Warnf("[modelrelease] node %s 部署即时 digest 检查失败（release %s）: %s", nodeID, id, reason)
		if h.FailFast {
			return nodeID, reason
		}
		return "", ""
	}
	_ = c.store.SetNodeResult(ctx, id, nodeID, &modelrepo.NodeReleaseResult{
		NodeID: nodeID, Status: modelrepo.NodeRelDeployed, Version: h.Version,
	})
	return "", ""
}

func (c *Controller) abortFailFast(ctx context.Context, id string, h *modelrepo.ModelRelease, failedNode, reason string, byNode map[string]modelrepo.NodeReleaseResult) {
	skipped := 0
	for _, nodeID := range h.TargetNodes {
		if nodeID == failedNode {
			continue
		}
		if nr, ok := byNode[nodeID]; ok && nr.Status != modelrepo.NodeRelPending {
			continue // 快照中已 deployed/failed 不动
		}
		// 最新读：本批刚部署成功的节点 → 不标（防旧快照误标）
		if latest, err := c.store.GetNodeResult(ctx, id, nodeID); err == nil && latest != nil && latest.Status != modelrepo.NodeRelPending {
			continue
		}
		if err := c.store.SetNodeResult(ctx, id, nodeID, &modelrepo.NodeReleaseResult{NodeID: nodeID, Status: modelrepo.NodeRelSkipped}); err == nil {
			skipped++
		}
	}
	c.setTerminal(ctx, id, modelrepo.ReleaseStatusFailed, fmt.Sprintf("fail fast: node %s: %s", failedNode, reason))
	log.Warnf("[modelrelease] fail-fast 中止 release %s：node %s 失败（%s；%d 个未执行节点标 skipped）", id, failedNode, reason, skipped)
}

// ── cancel 收敛（设计 §5.4 step3；L27）─────────────────────────────────

// cancelRemainder 把 canceled release 的剩余 pending perNode 补写 skipped
// （幂等；≤1 扫描周期补齐，L27 登记）。任何副本可执行（CAS 幂等收敛）。
func (c *Controller) cancelRemainder(ctx context.Context, id string) {
	results, err := c.store.ListNodeResults(ctx, id)
	if err != nil {
		return
	}
	written := 0
	for _, nr := range results {
		if nr.Status != modelrepo.NodeRelPending {
			continue
		}
		if err := c.store.SetNodeResult(ctx, id, nr.NodeID, &modelrepo.NodeReleaseResult{NodeID: nr.NodeID, Status: modelrepo.NodeRelSkipped}); err != nil {
			log.Warnf("[modelrelease] cancel 补写 skipped 失败（release %s node %s）: %v", id, nr.NodeID, err)
			continue
		}
		written++
	}
	if written > 0 {
		log.Infof("[modelrelease] cancel 收敛 release %s：%d 个 pending 节点标 skipped（L27 ≤1 扫描周期）", id, written)
	}
}

// ── 回滚执行（设计 §5.5 + 主线裁决 D2/D4）──────────────────────────────

// runRollback 执行回滚（锁持有者；每轮一个逆序批次，批间不暂停）：
//
//   - 执行期复查（D2 + D4 回滚 TOCTOU，每轮起始重查——防"回滚请求后、
//     执行中"被新发布激活接管 active / PrevActive 版本被删）：
//     ① release.Version == 模型当前 active（被新版本接管 → 中止）；
//     ② PrevActive 版本存在（被删 → 中止）；
//     失败 → head=failed + 明确 reason + **清除 RollbackRequested**（防
//     活锁，P4）+ 未执行 perNode 标 skipped；
//   - 逆序逐批：最后发布的批次先回滚，批内节点顺序不变；批间不暂停
//     （pause=0，快速恢复优先——直接忽略 h.PauseBetween）；
//   - 失败策略：不回滚中止（fail-fast 不适用，L24：能回多少回多少）——
//     失败节点 perNode.Status=failed + reason（Version 标 PrevActive，
//     下轮视为已完成不自动重试）；
//   - 完成：head → rolled_back（RollbackRequested 清零防重跑；
//     FinishedAt）；存在失败节点 → 同样 rolled_back + Warn（L24）。
func (c *Controller) runRollback(ctx context.Context, id string) {
	h, err := c.store.GetRelease(ctx, id)
	if err != nil {
		return
	}
	if abort, reason := c.rollbackGuard(ctx, h); abort {
		c.abortRollback(ctx, id, reason)
		return
	}
	prev, err := c.store.GetVersion(ctx, h.Model, h.PrevActive)
	if err != nil {
		c.abortRollback(ctx, id, fmt.Sprintf("previous active version %s deleted", h.PrevActive))
		return
	}
	results, err := c.store.ListNodeResults(ctx, id)
	if err != nil {
		log.Warnf("[modelrelease] 回滚读取 perNode 失败（release %s，本轮跳过）: %v", id, err)
		return
	}
	byNode := make(map[string]modelrepo.NodeReleaseResult, len(results))
	for _, nr := range results {
		byNode[nr.NodeID] = nr
	}
	// 推进期 digest 复查（v0.11.0，R-1+ 接入点②）：对已 deployed 节点复核
	// 上报 digest 与发布头期望一致；mismatch 已由 digestMismatch 写 perNode
	// failed——failFast 下中止（后续批次 skipped），否则继续（终态按失败
	// 计数判定）。
	for _, nodeID := range h.TargetNodes {
		nr, ok := byNode[nodeID]
		if !ok || nr.Status != modelrepo.NodeRelDeployed {
			continue
		}
		if ok, reason := c.digestMismatch(ctx, id, h, nodeID); ok {
			log.Warnf("[modelrelease] 推进期 digest 复核失败（release %s, node %s）: %s", id, nodeID, reason)
			if h.FailFast {
				c.abortFailFast(ctx, id, h, nodeID, reason, byNode)
				return
			}
		}
	}
	batches := BuildBatches(h.TargetNodes, h.BatchSize)

	// 逆序找第一个未回滚完的批次（接管续跑：已完成批次整体跳过）
	for bi := len(batches) - 1; bi >= 0; bi-- {
		batch := batches[bi]
		if rollbackBatchDone(batch, byNode, prev.Version) {
			continue
		}
		for _, nodeID := range batch {
			if nr, ok := byNode[nodeID]; ok && rollbackNodeDone(nr, prev.Version) {
				continue // 已完成（deployed/failed 且 Version=PrevActive）
			}
			if err := c.deploy.DeployVersion(ctx, nodeID, id, prev); err != nil {
				reason := DeployReason(err)
				if serr := c.store.SetNodeResult(ctx, id, nodeID, &modelrepo.NodeReleaseResult{
					NodeID: nodeID, Status: modelrepo.NodeRelFailed, Version: prev.Version, Reason: reason,
				}); serr == nil {
					byNode[nodeID] = modelrepo.NodeReleaseResult{NodeID: nodeID, Status: modelrepo.NodeRelFailed, Version: prev.Version}
				}
				log.Warnf("[modelrelease] 回滚节点 %s 失败（release %s，不回滚中止 L24）: %s", nodeID, id, reason)
				continue
			}
			if serr := c.store.SetNodeResult(ctx, id, nodeID, &modelrepo.NodeReleaseResult{
				NodeID: nodeID, Status: modelrepo.NodeRelDeployed, Version: prev.Version,
			}); serr == nil {
				byNode[nodeID] = modelrepo.NodeReleaseResult{NodeID: nodeID, Status: modelrepo.NodeRelDeployed, Version: prev.Version}
			}
		}
		break // 每轮一个批次；下轮从更前一批次继续（pause=0 不等待）
	}

	// 完成判定：全部目标节点已回滚（终态且 Version=PrevActive）
	done, failedCount := rollbackAllDone(h.TargetNodes, byNode, prev.Version)
	if !done {
		return
	}
	err = c.store.UpdateReleaseHead(ctx, id, func(hh *modelrepo.ModelRelease) error {
		if !hh.RollbackRequested {
			return errHeadChanged
		}
		hh.Status = modelrepo.ReleaseStatusRolledBack
		hh.RollbackRequested = false // 必须清零：防下轮按再次回滚重跑
		hh.FinishedAt = c.nowMs()
		return nil
	})
	if err != nil {
		if !errors.Is(err, errHeadChanged) {
			log.Warnf("[modelrelease] 回滚置终态失败（release %s，下轮重试）: %v", id, err)
		}
		return
	}
	c.removeActive(id)
	if failedCount > 0 {
		log.Warnf("[modelrelease] 回滚完成 release %s → rolled_back（%d 个节点回滚失败，明细见 perNode，L24）", id, failedCount)
	} else {
		log.Infof("[modelrelease] 回滚完成 release %s → rolled_back（全部节点回到 %s）", id, h.PrevActive)
	}
}

// rollbackGuard 是回滚执行期复查（主线裁决 D2/D4，每轮起始重查）：
//
//	① release.Version == 模型当前 active（被新版本激活接管 → 中止；
//	   active=none/模型已删 → 中止）；
//	② PrevActive 版本存在（被删 → 中止）。
//
// 返回 (true, reason) = 中止；读失败（网络等瞬态）→ (false, "") 本轮
// 跳过、下轮重试（不误杀）。
func (c *Controller) rollbackGuard(ctx context.Context, h *modelrepo.ModelRelease) (bool, string) {
	cur, err := c.store.ActiveVersion(ctx, h.Model)
	if err != nil {
		if errors.Is(err, modelrepo.ErrModelNotFound) {
			return true, "rollback superseded: model deleted"
		}
		return false, "" // 瞬态读失败：本轮跳过，下轮重试
	}
	if cur == nil || cur.Version != h.Version {
		active := "none"
		if cur != nil {
			active = cur.Version
		}
		return true, fmt.Sprintf("rollback superseded by active %s", active)
	}
	if _, err := c.store.GetVersion(ctx, h.Model, h.PrevActive); err != nil {
		return true, fmt.Sprintf("previous active version %s deleted", h.PrevActive)
	}
	return false, ""
}

// abortRollback 中止回滚（D2/D4 执行期复查失败）：head=failed + 明确
// reason + **清除 RollbackRequested**（防活锁：不清除则每轮重进
// runRollback 又中止）+ 未执行 perNode 标 skipped（deployed 保持现状）。
func (c *Controller) abortRollback(ctx context.Context, id, reason string) {
	err := c.store.UpdateReleaseHead(ctx, id, func(h *modelrepo.ModelRelease) error {
		if !h.RollbackRequested {
			return errHeadChanged
		}
		h.Status = modelrepo.ReleaseStatusFailed
		h.RollbackRequested = false
		h.FailureReason = reason
		h.FinishedAt = c.nowMs()
		return nil
	})
	if err != nil {
		if !errors.Is(err, errHeadChanged) {
			log.Warnf("[modelrelease] 回滚中止置终态失败（release %s，下轮重试）: %v", id, err)
		}
		return
	}
	c.cancelRemainder(ctx, id) // 未执行 perNode → skipped
	c.removeActive(id)
	log.Warnf("[modelrelease] 回滚中止 release %s：%s", id, reason)
}

// rollbackNodeDone 报告节点回滚是否已完成：perNode 终态（deployed 或
// failed）且 Version==PrevActive。规则：
//   - 原任务 deployed（Version=目标版本）→ 未完成 → 回滚部署；
//   - 原任务 failed/skipped（Version≠PrevActive）→ 未完成 → 回滚尝试
//     （failed→deployed 恢复，L23 收敛）；
//   - 回滚中 failed（Version=PrevActive）→ 已完成（不回滚中止 L24：
//     不自动重试，人工复核）。
func rollbackNodeDone(nr modelrepo.NodeReleaseResult, prevVersion string) bool {
	return nr.Status != modelrepo.NodeRelPending && nr.Version == prevVersion
}

// rollbackBatchDone 报告批次内全部节点回滚已完成。
func rollbackBatchDone(batch []string, byNode map[string]modelrepo.NodeReleaseResult, prevVersion string) bool {
	for _, nodeID := range batch {
		nr, ok := byNode[nodeID]
		if !ok || !rollbackNodeDone(nr, prevVersion) {
			return false
		}
	}
	return true
}

// rollbackAllDone 报告全部目标节点回滚已完成，并返回回滚失败计数（L24
// Warn 用）。
func rollbackAllDone(targetNodes []string, byNode map[string]modelrepo.NodeReleaseResult, prevVersion string) (bool, int) {
	failed := 0
	for _, nodeID := range targetNodes {
		nr, ok := byNode[nodeID]
		if !ok {
			return false, 0
		}
		if !rollbackNodeDone(nr, prevVersion) {
			return false, 0
		}
		if nr.Status == modelrepo.NodeRelFailed {
			failed++
		}
	}
	return true, failed
}

// ── guard 释放（设计 §5.4 step5）───────────────────────────────────────

// releaseGuard 释放同模型在途发布守卫（终态 release；幂等；失败重入队
// 下一轮重试，对齐 sweepGC 模式）。
//
// D9/N-1 登记：终态 release 头与 perNode 键**永久保留作审计痕迹**——本
// 操作只删 guard 键，不删 release 键（模型删除也不级联 release，L25 族；
// 长期运行累积由 etcdctl 手动清理）；N-4：终态 release 常驻内存无 GC，
// 与 KNOWN-ISSUES L28（无分页）同族登记。
func (c *Controller) releaseGuard(ctx context.Context, r *modelrepo.ModelRelease) {
	if err := c.store.ReleaseGuard(ctx, r.Model); err != nil {
		log.Warnf("[modelrelease] 释放 guard 失败（model=%s release=%s，下轮重试）: %v", r.Model, r.ID, err)
		return
	}
	log.Infof("[modelrelease] 已释放 guard（model=%s release=%s 终态）——同模型可再次发布", r.Model, r.ID)
}

// gcIfEnabled 在终态发布释放 guard 后按配置执行终态 GC（v0.8.0，L28）：
// 默认关闭（L31 审计口径）；开启后按 GCKeep 保留最近 N 条终态（含本次），
// 旧终态及其逐节点结果由 store.GCReleases 清理。失败仅告警（下个终态
// 事件重试），不影响发布主流程。
func (c *Controller) gcIfEnabled(ctx context.Context, r *modelrepo.ModelRelease) {
	if !c.opts.GCEnabled {
		return
	}
	removed, err := c.store.GCReleases(ctx, r.Model, c.opts.GCKeep)
	if err != nil {
		log.Warnf("[modelrelease] 终态发布 GC 失败（model=%s，下个终态事件重试）: %v", r.Model, err)
		return
	}
	if removed > 0 {
		log.Infof("[modelrelease] 终态发布 GC：model=%s 清理 %d 条旧终态（保留最近 %d 条）", r.Model, removed, c.opts.GCKeep)
	}
}
