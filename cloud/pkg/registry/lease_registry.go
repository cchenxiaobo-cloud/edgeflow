// v0.6.0「真多活」外部模式注册表：LeaseEtcdRegistry。
//
// 判活 = etcd 视角的 hb 键存在性（设计定稿 §1，主线裁决 D1/D2）：
//   - 心跳落盘：每次心跳 Grant+Put（grant-per-heartbeat），心跳键
//     /edgeflow/registry/heartbeats/<id> 绑租约，值 {"lastSeen": <UnixMs>}；
//     租约到期 → etcd 自动删键 → 判离线；台账键/Desired 绝不绑租约（§1.1）；
//   - 写路径：Register = CAS upsert（保留 RegisteredAt）；心跳 = 内存即时 +
//     异步续约队列（cap 4096 × 4 workers，hub 读循环零阻塞）；
//   - 读一致：LoadAnchored（ListByPrefixRev 台账+心跳双空间锚定）→ watch
//     增量（单 goroutine 应用器，只读铁律）→ 周期重扫兜底（§1.3 源 1/2/3）；
//   - 删除：两阶段 GC（内存 pending → etcd 确认），守卫删除
//     （hb 键在 → 活节点撤销；rev 不匹配 → 重注册撤销；网络错误 → 重试
//     带最新 rev），级联事件只在确认删除后产生（R4-1/R4-2）；
//   - 仅外部模式装配（embed 路径完全不受影响——EtcdRegistry 不变）。
//
// 应用器只读铁律（设计 §2.2②）：watch 应用器只改内存，绝不发 etcd 写；
// 「修复性重写」（本副本在服务该节点时 hb 键被删）经续约队列由 worker
// 执行——应用器本身零写，用 fakeKV 写计数单测锁定。
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1alpha1 "edgeflow/apis/edge/v1alpha1"
	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/pkg/log"
)

// 心跳键与注册表键空间前缀（主线裁决 R8-2：心跳键独立**复数**前缀，
// 与台账键分离；v0.5.0 二进制读不到心跳键，回滚零脏键）。
const (
	// KeyHeartbeatsPrefix 是心跳键独立前缀（租约只绑本前缀下键）。
	KeyHeartbeatsPrefix = "/edgeflow/registry/heartbeats/"
	// KeyRegistryPrefix 是注册表总前缀（台账+心跳双子空间；LoadAnchored /
	// Watch 用一个前缀覆盖，应用器按子前缀分派，未知键忽略 + Warn）。
	KeyRegistryPrefix = "/edgeflow/registry/"
)

// 续约流水线与重扫周期常量（设计 §1.2/§1.5）。
const (
	renewQueueCapacity  = 4096             // 续约队列容量（满则丢弃 + Warn，下次心跳自然重入队）
	defaultRenewWorkers = 4                // 续约 worker 数
	renewBackoffBase    = 5 * time.Second  // 续约失败退避基值
	renewBackoffCap     = 60 * time.Second // 退避上限
	minScanInterval     = 30 * time.Second // 重扫/GC 周期下限（max(30s, NODE_SCAN_INTERVAL)）
	defaultScanInterval = 30 * time.Second // NODE_SCAN_INTERVAL 默认值（外部模式语义迁移）
)

// hbValue 是心跳键的持久化值（etcdctl 可人读；键存在即判活，值解析失败
// 仅丢 lastSeen 展示精度——防御规则 L17，绝不 fail-closed 判死）。
type hbValue struct {
	LastSeen int64 `json:"lastSeen"`
}

// parseHBValue 解析心跳键值（容错：失败返回 0，调用方遵守「键在即活」）。
func parseHBValue(v []byte) (int64, error) {
	var hb hbValue
	if err := json.Unmarshal(v, &hb); err != nil {
		return 0, err
	}
	return hb.LastSeen, nil
}

// LeaseRegOptions 是 LeaseEtcdRegistry 构造选项（设计 §12.2）。
type LeaseRegOptions struct {
	OfflineTTL   time.Duration // 保留期（NODE_RETENTION，默认 24h）
	LeaseTTL     time.Duration // 心跳租约 TTL（EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL，默认 300s）
	ScanInterval time.Duration // 重扫/watch 重放冷却/GC 周期（NODE_SCAN_INTERVAL，默认 30s）
	RenewWorkers int           // 续约 worker 数（默认 4；0 → 默认）

	// 以下为测试注入口（生产零设置，不导出文档承诺）。
	now              func() time.Time
	renewBackoffBase time.Duration
	renewBackoffCap  time.Duration
}

// LeaseEtcdRegistry 实现 Store（外部模式多副本注册表，判活 = hb 键存在性）。
// renewRequest 是续约队列元素：nodeID 目标节点；repair=true 表示修复性
// 重写（v0.11.0，L12+）——本副本仍服务但 hb 键被删/缺失，grant 成功时
// 计入 hbRebuilds。
type renewRequest struct {
	nodeID string
	repair bool
}

type LeaseEtcdRegistry struct {
	reg  *Registry            // 内存缓存（读路径唯一事实源；OfflineTTL=0，GC 由本层两阶段接管）
	kv   etcdstore.ExtendedKV // 扩展 KV 面（CAS/watch/lease）
	opts LeaseRegOptions

	// lm 保护以下注册表自身的并发结构（锁序：绝不嵌套持有 reg.mu 再取 lm；
	// 唯一嵌套方向 reg.mu → lm 也不允许，读取 aliveHB 一律先取 lm 再取 reg.mu）。
	lm            sync.Mutex
	localHB       map[string]int64 // nodeID → 本地最近心跳 ts（本副本在服务 = 连接活着）
	aliveHB       map[string]int64 // etcd 视角 hb 键集合：nodeID → lastSeen
	pending       map[string]struct{}
	pendingEvents []string // 待集成层取走的 GC 级联事件
	retrying      map[string]struct{}
	lastQueueWarn time.Time
	lastReload    time.Time

	renewCh  chan renewRequest
	watchRev atomic.Int64

	// renewalFailures 是心跳租约续约失败累计计数（v0.8.0，L12）：
	// renewWorker 每次 Grant+Put 失败即自增，供 /metrics
	// edgeflow_cloudcore_lease_renewal_failures_total 消费（监控告警：
	// 持续增长 = etcd 侧异常或网络分区，见 KNOWN-ISSUES L12）。
	renewalFailures atomic.Uint64

	// hbRebuilds 是 hb 键修复性重建累计计数（v0.11.0，L12+）：本副本仍
	// 在服务（servingGrace 窗内）但 hb 键被删/缺失 → 续约 worker 经
	// enqueueRepairRenew 标记并**成功重建**（grant 成功）时自增；正常心跳
	// 续约不计。供 /metrics edgeflow_cloudcore_lease_hb_rebuilds_total
	// 消费（持续增长 = 租约抖动/键被外部删除，与 renewalFailures 互补）。
	hbRebuilds atomic.Uint64

	contactMu   sync.Mutex
	lastContact time.Time // 最近一次成功的 etcd 接触（healthz 多副本模式用）

	closeCh chan struct{}
	once    sync.Once
}

// 编译期断言：LeaseEtcdRegistry 满足 Store 接口。
var _ Store = (*LeaseEtcdRegistry)(nil)

// NewLeaseEtcdRegistry 创建外部模式注册表。kv 必须满足 ExtendedKV
// （装配层 AsExtended 已断言；本构造再防御校验 nil）。
func NewLeaseEtcdRegistry(kv etcdstore.ExtendedKV, opts LeaseRegOptions) (*LeaseEtcdRegistry, error) {
	if kv == nil {
		return nil, fmt.Errorf("registry: NewLeaseEtcdRegistry 需要非 nil ExtendedKV")
	}
	if opts.OfflineTTL <= 0 {
		opts.OfflineTTL = OfflineTTLDefault
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = etcdstore.DefaultLeaseTTL
	}
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = defaultScanInterval
	}
	if opts.RenewWorkers == 0 {
		opts.RenewWorkers = defaultRenewWorkers
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.renewBackoffBase <= 0 {
		opts.renewBackoffBase = renewBackoffBase
	}
	if opts.renewBackoffCap <= 0 {
		opts.renewBackoffCap = renewBackoffCap
	}
	return &LeaseEtcdRegistry{
		reg:      New(WithOfflineTTL(0)), // 内存 GC 停用：两阶段 GC 由本层接管（pending 期间节点保留可读）
		kv:       kv,
		opts:     opts,
		localHB:  make(map[string]int64),
		aliveHB:  make(map[string]int64),
		pending:  make(map[string]struct{}),
		retrying: make(map[string]struct{}),
		renewCh:  make(chan renewRequest, renewQueueCapacity),
		closeCh:  make(chan struct{}),
	}, nil
}

// ── 心跳键 ──────────────────────────────────────────────────────────────

func heartbeatKey(nodeID string) string { return KeyHeartbeatsPrefix + nodeID }

// markContact 记录一次成功的 etcd 接触（续约成功/扫描成功/watch 事件/
// CAS 成功）。多副本模式 /healthz 据此判 etcd 连接（失联 > TTL → 503，D3）。
func (r *LeaseEtcdRegistry) markContact() {
	r.contactMu.Lock()
	r.lastContact = r.opts.now()
	r.contactMu.Unlock()
}

// EtcdHealthyWithin 报告最近一次成功接触是否在 d 之内（/healthz 用）。
func (r *LeaseEtcdRegistry) EtcdHealthyWithin(d time.Duration) bool {
	r.contactMu.Lock()
	defer r.contactMu.Unlock()
	return time.Since(r.lastContact) <= d
}

// RenewalFailures 返回心跳租约续约失败累计计数（v0.8.0，L12，/metrics 用）。
func (r *LeaseEtcdRegistry) RenewalFailures() uint64 {
	return r.renewalFailures.Load()
}

// HBRebuildsCount 返回 hb 键修复性重建累计计数（v0.11.0，L12+，供
// /metrics edgeflow_cloudcore_lease_hb_rebuilds_total 消费）。
func (r *LeaseEtcdRegistry) HBRebuildsCount() uint64 {
	return r.hbRebuilds.Load()
}

// servingGrace 是「本副本正在服务该节点」的判定窗 = leaseTTL/2：
//   - 正常断连：hb 到期删除事件距最后一次本地心跳 ≈ TTL > TTL/2 → 判离线 ✓
//   - 租约抖动（etcd 故障恢复）：本地心跳持续刷新（≤30s）< TTL/2 → 修复性重写 ✓
//
// 依赖护栏：生产 TTL ≥ 90s（<90s 启动 Warn），grace ≥ 45s > 心跳周期 30s，
// 抖动窗口内不会误判离线（E12）。
func (r *LeaseEtcdRegistry) servingGrace() time.Duration { return r.opts.LeaseTTL / 2 }

// locallyServing 报告本副本是否正在服务该节点（本地心跳 fresh）。
func (r *LeaseEtcdRegistry) locallyServing(nodeID string) bool {
	r.lm.Lock()
	defer r.lm.Unlock()
	t, ok := r.localHB[nodeID]
	if !ok {
		return false
	}
	return r.opts.now().UnixMilli()-t < r.servingGrace().Milliseconds()
}

// ── 续约流水线（grant-per-heartbeat，D1）────────────────────────────────

// enqueueRenew 非阻塞入队普通续约请求（hub 读循环零阻塞：队列满则丢弃 +
// 降频 Warn，受影响的节点租约到期后由下一次心跳自然重建，自愈）。
func (r *LeaseEtcdRegistry) enqueueRenew(nodeID string) {
	r.enqueue(renewRequest{nodeID: nodeID})
}

// enqueueRepairRenew 非阻塞入队修复性重写请求（v0.11.0，L12+）：本副本仍
// 在服务但 hb 键被删/缺失（applyDelete/rescanOnce/gcSweepOne 守卫 0 三处
// 修复性入口）；worker grant 成功时计入 hbRebuilds。
func (r *LeaseEtcdRegistry) enqueueRepairRenew(nodeID string) {
	r.enqueue(renewRequest{nodeID: nodeID, repair: true})
}

// enqueue 入队统一实现。
func (r *LeaseEtcdRegistry) enqueue(req renewRequest) {
	select {
	case r.renewCh <- req:
	default:
		r.lm.Lock()
		now := r.opts.now()
		if now.Sub(r.lastQueueWarn) >= 30*time.Second {
			r.lastQueueWarn = now
			log.Warnf("[LeaseEtcdRegistry] 心跳续约队列已满（cap=%d），丢弃续约请求——下次心跳自然重入队，受影响节点租约到期后自动重建", renewQueueCapacity)
		}
		r.lm.Unlock()
	}
}

// grantHeartbeat 执行一次 GrantHeartbeatLease（失败不影响内存态——节点仍
// 显示 Ready 直至租约到期，设计 §1.2 错误语义）。
func (r *LeaseEtcdRegistry) grantHeartbeat(nodeID string) error {
	data, err := json.Marshal(hbValue{LastSeen: r.opts.now().UnixMilli()})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.kv.GrantHeartbeatLease(ctx, heartbeatKey(nodeID), data, r.opts.LeaseTTL); err != nil {
		return err
	}
	r.markContact()
	return nil
}

// renewWorker 消费续约队列：grant-per-heartbeat——每个 worker 对每个入队
// nodeID 做 Grant+Put；失败 → 指数退避重试（窗口 ≤ lease TTL）；节点离开
// 本副本服务集（localHB 不 fresh）→ 停续（R1-3②）。
func (r *LeaseEtcdRegistry) renewWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.closeCh:
			return
		case req := <-r.renewCh:
			if !r.locallyServing(req.nodeID) {
				continue // 断开即停续：节点已不在本副本服务集
			}
			if err := r.grantHeartbeat(req.nodeID); err != nil {
				log.Warnf("[LeaseEtcdRegistry] 心跳租约续约失败（nodeID=%s，重试窗口 ≤ %v，不影响内存态）: %v", req.nodeID, r.opts.LeaseTTL, err)
				r.renewalFailures.Add(1)
				r.scheduleRetry(ctx, req.nodeID, req.repair)
			} else if req.repair {
				// 修复性重写成功（v0.11.0，L12+）：hb 键已重建，计数一次
				r.hbRebuilds.Add(1)
			}
		}
	}
}

// scheduleRetry 启动单节点单在途的退避重试：指数退避（基值 5s，上限 60s），
// 总重试窗口 ≤ lease TTL；每次重试前复查本地服务状态（断开即放弃）。
// repair 透传：重试成功且为修复性重写 → 计入 hbRebuilds（只计一次）。
func (r *LeaseEtcdRegistry) scheduleRetry(ctx context.Context, nodeID string, repair bool) {
	r.lm.Lock()
	if _, dup := r.retrying[nodeID]; dup {
		r.lm.Unlock()
		return
	}
	r.retrying[nodeID] = struct{}{}
	r.lm.Unlock()

	go func() {
		defer func() {
			r.lm.Lock()
			delete(r.retrying, nodeID)
			r.lm.Unlock()
		}()
		delay := r.opts.renewBackoffBase
		elapsed := time.Duration(0)
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.closeCh:
				return
			case <-time.After(delay):
			}
			elapsed += delay
			if elapsed >= r.opts.LeaseTTL {
				return // 重试预算耗尽（≤TTL）：租约已到期，等节点下一次心跳重建
			}
			if !r.locallyServing(nodeID) {
				return // 断开即停续
			}
			if err := r.grantHeartbeat(nodeID); err == nil {
				if repair {
					r.hbRebuilds.Add(1)
				}
				return
			}
			delay *= 2
			if delay > r.opts.renewBackoffCap {
				delay = r.opts.renewBackoffCap
			}
		}
	}()
}

// ── 写路径 ─────────────────────────────────────────────────────────────

// Register 写穿登记（外部模式 = CAS upsert，保留首见 RegisteredAt——D4 仅
// 外部模式；embed 的 EtcdRegistry 保持裸 Put 逐位不变）。冲突重试 ≤3 次，
// 耗尽返回 error（未持久化、内存不动）。成功后同步建 hb 键（注册建活，
// 设计 §1.3 三态 + §9 U7）。
func (r *LeaseEtcdRegistry) Register(info NodeInfo) error {
	if !validNodeID(info.NodeID) {
		return fmt.Errorf("registry: 拒绝非法 nodeID %q（允许 [A-Za-z0-9._-]）", info.NodeID)
	}
	key := nodeKey(info.NodeID)
	now := r.opts.now().UnixMilli()
	rec := fromNodeInfo(info)
	if rec.RegisteredAt == 0 {
		rec.RegisteredAt = now
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("registry: 序列化节点台账失败: %w", err)
	}
	ctx := context.Background()
	var ok bool
	for attempt := 0; attempt < 3; attempt++ {
		cur, modRev, err := r.kv.GetWithRev(ctx, key)
		if err != nil {
			return fmt.Errorf("registry: Register 读取 etcd 基准失败（未持久化、内存不动）: %w", err)
		}
		if modRev > 0 {
			var existing nodeRecord
			if err := json.Unmarshal(cur, &existing); err == nil && existing.RegisteredAt > 0 {
				rec.RegisteredAt = existing.RegisteredAt // 保留首见（CAS 保证并发安全）
			}
		}
		ok, err = r.kv.CompareAndPut(ctx, key, data, modRev)
		if err != nil {
			return fmt.Errorf("registry: Register CAS 失败（未持久化、内存不动）: %w", err)
		}
		if ok {
			break
		}
		// 冲突：他副本/他请求刚写过 → 读最新基准再合并重试
	}
	if !ok {
		return fmt.Errorf("registry: Register 并发冲突重试耗尽（nodeID=%s），未持久化——边缘侧会重试注册", info.NodeID)
	}
	if err := r.reg.Register(info); err != nil {
		// 内存写入失败理论不可达（键校验上游已拦截）；etcd 已写入，错误上抛由
		// 调用方记日志，重试时 CAS upsert 幂等收敛。
		return err
	}
	r.lm.Lock()
	r.localHB[info.NodeID] = now
	delete(r.pending, info.NodeID)
	r.lm.Unlock()
	r.enqueueRenew(info.NodeID) // 注册即建 hb 键（U7）
	r.markContact()
	return nil
}

// UpdateHeartbeat 刷新心跳：内存即时更新（同步返回，hub 读循环零阻塞）+
// 非阻塞入队续约；值跨副本经 hb 键 lastSeen 同步（watch PUT）。
func (r *LeaseEtcdRegistry) UpdateHeartbeat(nodeID string, ts int64) error {
	if err := r.reg.UpdateHeartbeat(nodeID, ts); err != nil {
		return err
	}
	if ts == 0 {
		ts = r.opts.now().UnixMilli()
	}
	r.lm.Lock()
	r.localHB[nodeID] = ts
	delete(r.pending, nodeID) // 有新鲜心跳 = 活节点，撤销任何 GC pending
	r.lm.Unlock()
	r.enqueueRenew(nodeID)
	return nil
}

// MarkOffline 标记离线（连接断开事件）：瞬态仅内存 + 从本地服务集移除
// （断开即停续 R1-3②）。不主动 Revoke 租约（R4-3：跨副本离线由租约到期
// 送达，revoke 反而引入「删键瞬间重注册丢失窗口」）。
func (r *LeaseEtcdRegistry) MarkOffline(nodeID string) error {
	if err := r.reg.MarkOffline(nodeID); err != nil {
		return err
	}
	r.lm.Lock()
	delete(r.localHB, nodeID)
	r.lm.Unlock()
	return nil
}

// Seed 播种（启动加载降级用）：与内存 Seed 语义一致（Unknown + offlineSince
// 起算）。外部模式正常装配走 LoadAnchored，本方法仅满足 Store 接口面。
func (r *LeaseEtcdRegistry) Seed(nodes []NodeInfo, since int64) error {
	return r.reg.Seed(nodes, since)
}

// Load 实现 Store 接口：等价 LoadAnchored（结果一致，锚点丢弃）。
func (r *LeaseEtcdRegistry) Load(ctx context.Context) error {
	_, err := r.LoadAnchored(ctx)
	return err
}

// LoadAnchored 锚定加载（设计 §2.1）：单前缀扫描台账+心跳双子空间 →
// 内存重建（hb 键在 → Ready+lastSeen；无 hb → Seed 语义 Unknown）→
// 返回响应头 revision 作为 watch 锚点。
func (r *LeaseEtcdRegistry) LoadAnchored(ctx context.Context) (int64, error) {
	entries, rev, err := r.kv.ListByPrefixRev(ctx, KeyRegistryPrefix)
	if err != nil {
		return 0, fmt.Errorf("registry: LoadAnchored 前缀扫描 %s 失败: %w", KeyRegistryPrefix, err)
	}
	// 两遍扫描（显式不依赖键字典序——真实 etcd 按字节序返回 hb<nodes，
	// 但该排序是隐式契约，map 类实现/未来键空间变化会破坏；先收集
	// alive 集合，再重建台账）：
	//   第一遍：hb 键 → alive（键在即活，值仅影响 lastSeen 精度）；
	//   第二遍：台账键 → NodeInfo，状态以 hb 视角收敛（Ready/Unknown）。
	nodes := make([]NodeInfo, 0, len(entries))
	alive := make(map[string]int64, len(entries))
	skipped := 0
	for _, ent := range entries {
		if !strings.HasPrefix(ent.Key, KeyHeartbeatsPrefix) {
			continue
		}
		id := strings.TrimPrefix(ent.Key, KeyHeartbeatsPrefix)
		if !validNodeID(id) {
			skipped++
			log.Warnf("[LeaseEtcdRegistry] 跳过非法心跳键 %s", ent.Key)
			continue
		}
		lastSeen, err := parseHBValue(ent.Value)
		if err != nil {
			log.Warnf("[LeaseEtcdRegistry] 心跳键 %s 值解析失败（键在即活，仅丢 lastSeen 精度）: %v", ent.Key, err)
		}
		alive[id] = lastSeen
	}
	for _, ent := range entries {
		if !strings.HasPrefix(ent.Key, KeyNodesPrefix) {
			continue
		}
		id := strings.TrimPrefix(ent.Key, KeyNodesPrefix)
		var rec nodeRecord
		if err := json.Unmarshal(ent.Value, &rec); err != nil {
			skipped++
			log.Warnf("[LeaseEtcdRegistry] 跳过坏台账键 %s（反序列化失败: %v）", ent.Key, err)
			continue
		}
		if !validNodeID(id) || rec.NodeID != id {
			skipped++
			log.Warnf("[LeaseEtcdRegistry] 跳过非法台账键 %s（nodeID %q）", ent.Key, rec.NodeID)
			continue
		}
		info := rec.toNodeInfo()
		if lastSeen, ok := alive[rec.NodeID]; ok {
			info.Status = StatusReady
			info.LastHeartbeatAt = lastSeen
		}
		nodes = append(nodes, info)
	}
	// 第三遍：注册表前缀下的未知键（未来新键空间，默认忽略 + Warn）
	for _, ent := range entries {
		if !strings.HasPrefix(ent.Key, KeyNodesPrefix) && !strings.HasPrefix(ent.Key, KeyHeartbeatsPrefix) {
			skipped++
			log.Warnf("[LeaseEtcdRegistry] 忽略注册表前缀下的未知键 %s（未来新键空间，默认忽略）", ent.Key)
		}
	}
	if skipped > 0 {
		log.Warnf("[LeaseEtcdRegistry] 锚定加载完成：恢复 %d 个节点，跳过 %d 个坏键", len(nodes), skipped)
	}
	r.seedAnchored(nodes, alive)
	r.watchRev.Store(rev)
	r.markContact()
	return rev, nil
}

// seedAnchored 整体重建内存（hb 视角三态）：无 hb 的节点保持 Seed 语义
// （Unknown、LastHeartbeatAt=0、offlineSince=now）。
func (r *LeaseEtcdRegistry) seedAnchored(nodes []NodeInfo, alive map[string]int64) {
	since := r.opts.now().UnixMilli()
	r.reg.mu.Lock()
	fresh := make(map[string]*NodeInfo, len(nodes))
	offline := make(map[string]int64, len(nodes))
	for i := range nodes {
		n := nodes[i]
		if _, ok := alive[n.NodeID]; !ok {
			n.Status = StatusUnknown
			n.LastHeartbeatAt = 0
			offline[n.NodeID] = since
		}
		fresh[n.NodeID] = &n
	}
	r.reg.nodes = fresh
	r.reg.offlineSince = offline
	r.reg.gcEvents = nil // 内层 GC 停用（offlineTTL=0），事件面由本层 pendingEvents 承担
	r.reg.mu.Unlock()
	r.lm.Lock()
	r.aliveHB = alive
	r.lm.Unlock()
}

// ── watch 应用器（只读铁律：只改内存，绝不发 etcd 写）─────────────────

// StartWatch 启动 watch 应用器 + 周期重扫 + 心跳续约 worker（设计 §12.2）：
// ctx 取消或 Close() 后全部停止（幂等）。调用顺序：New → LoadAnchored →
// StartWatch。
func (r *LeaseEtcdRegistry) StartWatch(ctx context.Context) {
	go r.watchLoop(ctx)
	go r.rescanLoop(ctx)
	for i := 0; i < r.opts.RenewWorkers; i++ {
		go r.renewWorker(ctx)
	}
}

// watchLoop 锚定增量应用主循环：watch 断开/Err（含 ErrCompacted）→ 全量
// 重放（降频）→ 重锚定 → 重启 watch。
func (r *LeaseEtcdRegistry) watchLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		rev := r.watchRev.Load()
		ch := r.kv.WatchPrefix(ctx, KeyRegistryPrefix, etcdstore.StartRevision(rev))
		for ev := range ch {
			switch ev.Type {
			case etcdstore.WatchEventPut:
				r.markContact()
				r.applyPut(ev.Key, ev.Value)
				r.watchRev.Store(ev.ModRevision) // 单调推进锚点：断线重放只补增量（R2-3）
			case etcdstore.WatchEventDelete:
				r.markContact()
				r.applyDelete(ev.Key)
				r.watchRev.Store(ev.ModRevision)
			case etcdstore.WatchEventErr:
				// ErrCompacted / 连接断开：全量重放（ctx 取消不重放，直接退出）
				if ctx.Err() != nil {
					return
				}
				r.reloadAll(ctx)
			}
		}
		// 通道关闭（断开信号）：重放后重连
		if ctx.Err() != nil {
			return
		}
		r.reloadAll(ctx)
	}
}

// applyPut 应用 hb PUT / 台账 PUT（应用器只读：仅内存 + 续约队列入队）。
func (r *LeaseEtcdRegistry) applyPut(key string, value []byte) {
	switch {
	case strings.HasPrefix(key, KeyHeartbeatsPrefix):
		id := strings.TrimPrefix(key, KeyHeartbeatsPrefix)
		if !validNodeID(id) {
			log.Warnf("[LeaseEtcdRegistry] 忽略非法心跳键事件 %s", key)
			return
		}
		lastSeen, err := parseHBValue(value)
		if err != nil {
			log.Warnf("[LeaseEtcdRegistry] 心跳键 %s 值解析失败（键在即活，仅丢 lastSeen 精度 L17）: %v", key, err)
		}
		_ = r.reg.UpdateHeartbeat(id, lastSeen) // Ready + 清 offlineSince
		r.lm.Lock()
		r.aliveHB[id] = lastSeen
		delete(r.pending, id) // 活节点撤销 GC pending
		r.lm.Unlock()
	case strings.HasPrefix(key, KeyNodesPrefix):
		id := strings.TrimPrefix(key, KeyNodesPrefix)
		var rec nodeRecord
		if err := json.Unmarshal(value, &rec); err != nil {
			log.Warnf("[LeaseEtcdRegistry] 忽略坏台账事件 %s（反序列化失败: %v）", key, err)
			return
		}
		if !validNodeID(id) || rec.NodeID != id {
			log.Warnf("[LeaseEtcdRegistry] 忽略非法台账事件 %s（nodeID %q）", key, rec.NodeID)
			return
		}
		r.upsertFromRecord(rec)
	default:
		log.Warnf("[LeaseEtcdRegistry] 忽略注册表前缀下的未知键事件 %s（未来新键空间，默认忽略）", key)
	}
}

// applyDelete 应用 hb DELETE / 台账 DELETE。
func (r *LeaseEtcdRegistry) applyDelete(key string) {
	switch {
	case strings.HasPrefix(key, KeyHeartbeatsPrefix):
		id := strings.TrimPrefix(key, KeyHeartbeatsPrefix)
		if !validNodeID(id) {
			return
		}
		if r.locallyServing(id) {
			// 本副本正在服务该节点（租约抖动/断连事件丢失）：忽略 + 修复性
			// 重写（经续约队列由 worker 写，应用器本身零写——只读铁律）。
			log.Warnf("[LeaseEtcdRegistry] hb 键 %s 被删但本副本仍服务该节点（lastSeen 新鲜）——忽略并修复性重写", key)
			r.lm.Lock()
			delete(r.pending, id)
			r.lm.Unlock()
			r.enqueueRepairRenew(id)
			return
		}
		_ = r.reg.MarkOffline(id) // 租约到期 = 判离线（offlineSince=事件时刻，近似精确）
		r.lm.Lock()
		delete(r.aliveHB, id)
		r.lm.Unlock()
	case strings.HasPrefix(key, KeyNodesPrefix):
		id := strings.TrimPrefix(key, KeyNodesPrefix)
		if !validNodeID(id) {
			return
		}
		// 他副本 GC 确认（或级联了本键）：内存移除 + 级联事件（DeleteByNode
		// 幂等，他副本也删过一遍——设计 §2.2 表）。
		r.removeMemory(id)
		r.lm.Lock()
		r.pendingEvents = append(r.pendingEvents, id)
		r.lm.Unlock()
	default:
		log.Warnf("[LeaseEtcdRegistry] 忽略注册表前缀下的未知键删除事件 %s", key)
	}
}

// upsertFromRecord 台账 upsert（watch 应用器/重放用）：已有节点只更新元数据
// 字段（RegisteredAt 以 etcd 为准——CAS 保证首见保留），状态字段以 hb 视角
// 为准；新节点按 aliveHB 判定 Ready/Unknown 插入。
func (r *LeaseEtcdRegistry) upsertFromRecord(rec nodeRecord) {
	r.lm.Lock()
	lastSeen, alive := r.aliveHB[rec.NodeID]
	r.lm.Unlock()

	r.reg.mu.Lock()
	defer r.reg.mu.Unlock()
	if old, ok := r.reg.nodes[rec.NodeID]; ok {
		old.NodeName = rec.NodeName
		old.Arch = rec.Arch
		old.OS = rec.OS
		old.EdgecoreVersion = rec.EdgecoreVersion
		old.CPU = rec.CPU
		old.Memory = rec.Memory
		old.IP = rec.IP
		if rec.RegisteredAt > 0 {
			old.RegisteredAt = rec.RegisteredAt
		}
		return
	}
	info := rec.toNodeInfo()
	if alive {
		info.Status = StatusReady
		info.LastHeartbeatAt = lastSeen
	}
	r.reg.nodes[rec.NodeID] = &info
	if alive {
		delete(r.reg.offlineSince, rec.NodeID)
	} else {
		r.reg.offlineSince[rec.NodeID] = r.opts.now().UnixMilli()
	}
}

// reloadAll 全量重放（断线/ErrCompacted）：ListByPrefixRev → 逐记录合并
// （台账 upsert + hb 视角收敛 + 移除 etcd 已不存在的内存节点 + 级联事件）→
// 重新锚定。重放降频：相邻重放间隔 ≥ scanInterval（风暴防护，设计 §2.3）。
func (r *LeaseEtcdRegistry) reloadAll(ctx context.Context) {
	r.lm.Lock()
	now := r.opts.now()
	if !r.lastReload.IsZero() && now.Sub(r.lastReload) < r.opts.ScanInterval {
		wait := r.opts.ScanInterval - now.Sub(r.lastReload)
		r.lm.Unlock()
		log.Warnf("[LeaseEtcdRegistry] watch 重放过频，降频等待 %v 后全量重放", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	} else {
		r.lm.Unlock()
	}

	entries, rev, err := r.kv.ListByPrefixRev(ctx, KeyRegistryPrefix)
	if err != nil {
		log.Warnf("[LeaseEtcdRegistry] 全量重放失败（watch 断开但周期重扫兜底仍运行，判活正确性不依赖 watch）: %v", err)
		return
	}
	r.lm.Lock()
	r.lastReload = r.opts.now()
	r.lm.Unlock()
	r.markContact()

	alive := make(map[string]int64, len(entries))
	etcdNodes := make(map[string]struct{}, len(entries))
	for _, ent := range entries {
		switch {
		case strings.HasPrefix(ent.Key, KeyHeartbeatsPrefix):
			id := strings.TrimPrefix(ent.Key, KeyHeartbeatsPrefix)
			if !validNodeID(id) {
				continue
			}
			lastSeen, err := parseHBValue(ent.Value)
			if err != nil {
				log.Warnf("[LeaseEtcdRegistry] 心跳键 %s 值解析失败（键在即活）: %v", ent.Key, err)
			}
			alive[id] = lastSeen
		case strings.HasPrefix(ent.Key, KeyNodesPrefix):
			id := strings.TrimPrefix(ent.Key, KeyNodesPrefix)
			var rec nodeRecord
			if err := json.Unmarshal(ent.Value, &rec); err != nil || rec.NodeID != id {
				continue
			}
			if !validNodeID(id) {
				continue
			}
			etcdNodes[id] = struct{}{}
			r.upsertFromRecord(rec)
		default:
			// 忽略未知键
		}
	}
	r.lm.Lock()
	r.aliveHB = alive
	r.lm.Unlock()

	// 内存有、etcd 无 → 已被删除（GC 确认/他副本）：移除 + 级联事件（幂等）
	for _, info := range r.reg.List() {
		if _, ok := etcdNodes[info.NodeID]; ok {
			continue
		}
		r.removeMemory(info.NodeID)
		r.lm.Lock()
		r.pendingEvents = append(r.pendingEvents, info.NodeID)
		r.lm.Unlock()
	}
	// hb 视角收敛：hb 在但内存非 Ready → 恢复 Ready + 撤销 pending
	for id := range alive {
		if info, ok := r.reg.Get(id); ok && info.Status != StatusReady {
			_ = r.reg.UpdateHeartbeat(id, alive[id])
		}
		r.lm.Lock()
		delete(r.pending, id)
		r.lm.Unlock()
	}
	r.watchRev.Store(rev)
}

// ── 周期重扫（判活兜底源 2）────────────────────────────────────────────

// rescanLoop 按 scanInterval（下限 30s）周期重扫 hb 键空间，与内存节点集
// 对账（设计 §1.3 源 2）：有台账无 hb → MarkOffline；hb 在 + 内存 Offline →
// 恢复 Ready；hb 在但内存 Ready → 不动。判活正确性不依赖 watch。
func (r *LeaseEtcdRegistry) rescanLoop(ctx context.Context) {
	interval := r.opts.ScanInterval
	if interval < minScanInterval {
		interval = minScanInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.closeCh:
			return
		case <-t.C:
			r.rescanOnce(ctx)
		}
	}
}

// rescanOnce 执行一轮重扫对账（导出形态供测试直调）。
func (r *LeaseEtcdRegistry) rescanOnce(ctx context.Context) {
	entries, _, err := r.kv.ListByPrefixRev(ctx, KeyHeartbeatsPrefix)
	if err != nil {
		// etcd 抖动：本轮跳过，内存保持 stale-but-consistent（v0.5.0 语义）
		log.Warnf("[LeaseEtcdRegistry] 心跳重扫失败（内存暂不回退）: %v", err)
		return
	}
	r.markContact()
	alive := make(map[string]int64, len(entries))
	for _, ent := range entries {
		id := strings.TrimPrefix(ent.Key, KeyHeartbeatsPrefix)
		if !validNodeID(id) {
			continue
		}
		lastSeen, err := parseHBValue(ent.Value)
		if err != nil {
			log.Warnf("[LeaseEtcdRegistry] 心跳键 %s 值解析失败（键在即活）: %v", ent.Key, err)
		}
		alive[id] = lastSeen
	}
	r.lm.Lock()
	r.aliveHB = alive
	r.lm.Unlock()

	for _, info := range r.reg.List() {
		_, hbAlive := alive[info.NodeID]
		if hbAlive {
			if info.Status != StatusReady {
				_ = r.reg.UpdateHeartbeat(info.NodeID, alive[info.NodeID]) // 恢复 Ready
			}
			r.lm.Lock()
			delete(r.pending, info.NodeID)
			r.lm.Unlock()
			continue
		}
		if info.Status == StatusReady && r.locallyServing(info.NodeID) {
			// 本副本连接还活着但 hb 键缺失（续约失败窗口）：修复性重入队，不判离线
			r.enqueueRepairRenew(info.NodeID)
			continue
		}
		if info.Status != StatusOffline {
			_ = r.reg.MarkOffline(info.NodeID)
		}
	}
}

// ── 两阶段 GC（pending → etcd 确认删除 + 守卫）─────────────────────────

// CleanupLoop 启动两阶段 GC（外部模式周期 = max(30s, NODE_SCAN_INTERVAL)，
// 不再用 1h——重试粒度 30s，设计 §1.5）。ctx 取消或 Close() 退出。
func (r *LeaseEtcdRegistry) CleanupLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = r.opts.ScanInterval
	}
	if interval < minScanInterval {
		interval = minScanInterval
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.closeCh:
				return
			case <-t.C:
				r.gcOnce(ctx)
			}
		}
	}()
}

// gcOnce 执行一轮两阶段 GC（导出形态供测试直调）。
//   - 阶段一（内存）：保留期已到且仍 Offline/Unknown 的节点 → 标记 pending
//     （节点**保留在内存**、仍可被 API 读到、状态保持 Offline——v0.5.0 在
//     保留期后立即消失，现多出 ≤1 扫描周期的「过期但可见」窗口，无感知差异）；
//   - 阶段二（sweepPending）：对每个 pending 节点做守卫删除（见 gcSweepOne）。
func (r *LeaseEtcdRegistry) gcOnce(ctx context.Context) {
	now := r.opts.now().UnixMilli()
	ttlMs := r.opts.OfflineTTL.Milliseconds()
	if ttlMs <= 0 {
		return // GC 禁用（OfflineTTL<=0 防御）
	}
	for _, info := range r.reg.List() {
		if info.Status != StatusOffline && info.Status != StatusUnknown {
			continue
		}
		r.reg.mu.RLock()
		since, ok := r.reg.offlineSince[info.NodeID]
		r.reg.mu.RUnlock()
		if !ok || now-since < ttlMs {
			continue
		}
		r.lm.Lock()
		if _, dup := r.pending[info.NodeID]; !dup {
			r.pending[info.NodeID] = struct{}{}
		}
		r.lm.Unlock()
	}
	for _, id := range r.pendingSnapshot() {
		r.gcSweepOne(ctx, id)
	}
}

// gcSweepOne 对单个 pending 节点执行守卫删除（三态三分支，设计 §1.5/R4-1）：
//   - hb 键在（活节点，含 watch 滞后窗口）→ 撤销删除 + 恢复 Ready，零级联事件；
//   - rev 不匹配（重注册/他副本重写）→ 放弃删除 + 恢复，零级联事件；
//   - 网络错误 → 保持 pending，下轮重试（重试自带最新 rev，绝不删错版）；
//   - CompareAndDelete 成功 → 内存移除 + pendingEvents（级联严格排在其后，R4-2）。
func (r *LeaseEtcdRegistry) gcSweepOne(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// 守卫 0：本副本正在服务（心跳刚发生、hb 键异步重建中）→ 跳过，等续约恢复
	if r.locallyServing(id) {
		r.enqueueRepairRenew(id)
		r.markContact()
		return
	}
	// 守卫 1：hb 键仍存在 = 节点其实活着（watch 滞后/他副本刚续约）→ 恢复 Ready
	hb, err := r.kv.Get(ctx, heartbeatKey(id))
	if err != nil {
		log.Warnf("[LeaseEtcdRegistry] GC 守卫检查 hb 键失败（nodeID=%s，pending 保留下轮重试）: %v", id, err)
		return
	}
	if hb != nil {
		lastSeen, _ := parseHBValue(hb)
		_ = r.reg.UpdateHeartbeat(id, lastSeen)
		r.lm.Lock()
		delete(r.pending, id)
		delete(r.aliveHB, id)
		r.aliveHB[id] = lastSeen
		r.lm.Unlock()
		r.markContact()
		log.Warnf("[LeaseEtcdRegistry] GC 守卫拦截：节点 %s 的 hb 键仍存在（活节点），撤销删除并恢复 Ready", id)
		return
	}

	// 守卫 2：台账键版本（rev 不匹配 = 重注册 → 放弃；网络错误 → 重试带最新 rev）
	_, rev, err := r.kv.GetWithRev(ctx, nodeKey(id))
	if err != nil {
		log.Warnf("[LeaseEtcdRegistry] GC 读取台账版本失败（nodeID=%s，pending 保留下轮重试）: %v", id, err)
		return
	}
	if rev == 0 {
		// 台账已不存在（他副本已删的先删情形）：仅清 pending，不产生级联事件
		// （防级联误删重注册节点的 Desired；节点已彻底消失，watch 已驱动级联）。
		r.dropPending(id)
		r.removeMemory(id)
		r.markContact()
		return
	}
	deleted, err := r.kv.CompareAndDelete(ctx, nodeKey(id), rev)
	if err != nil {
		log.Warnf("[LeaseEtcdRegistry] GC 守卫删除失败（nodeID=%s，pending 下轮重试带最新 rev）: %v", id, err)
		return
	}
	r.markContact()
	if deleted {
		// 删除成功 → 级联严格排在其后（R4-2）：内存移除 + 级联事件
		r.dropPending(id)
		r.removeMemory(id)
		r.lm.Lock()
		r.pendingEvents = append(r.pendingEvents, id)
		r.lm.Unlock()
		return
	}
	// rev 不匹配 = 节点已重注册/他副本刚写过：放弃删除 + 恢复（重入队由
	// 注册路径负责；此处按台账现状恢复内存视图）
	cur, rev2, err := r.kv.GetWithRev(ctx, nodeKey(id))
	if err != nil {
		log.Warnf("[LeaseEtcdRegistry] GC 撤销恢复读取失败（nodeID=%s，pending 保留下轮重试）: %v", id, err)
		return
	}
	r.dropPending(id)
	if rev2 == 0 {
		r.removeMemory(id) // 已彻底消失：不产生事件
		return
	}
	var rec nodeRecord
	if err := json.Unmarshal(cur, &rec); err == nil {
		r.upsertFromRecord(rec)
		// 恢复为 Ready：重注册的节点按注册语义就绪（hb 键由注册/续约路径异步重建）
		nowMs := r.opts.now().UnixMilli()
		r.reg.mu.Lock()
		if old, ok := r.reg.nodes[id]; ok {
			old.Status = StatusReady
			old.LastHeartbeatAt = nowMs
			delete(r.reg.offlineSince, id)
		}
		r.reg.mu.Unlock()
		r.enqueueRenew(id)
	}
	log.Warnf("[LeaseEtcdRegistry] GC 守卫拦截：节点 %s 台账已重写（rev 不匹配），放弃删除并恢复", id)
}

// dropPending 从 pending 集合移除（幂等）。
func (r *LeaseEtcdRegistry) dropPending(id string) {
	r.lm.Lock()
	delete(r.pending, id)
	r.lm.Unlock()
}

// pendingSnapshot 返回 pending 集合快照（排序确定性输出）。
func (r *LeaseEtcdRegistry) pendingSnapshot() []string {
	r.lm.Lock()
	defer r.lm.Unlock()
	out := make([]string, 0, len(r.pending))
	for id := range r.pending {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// removeMemory 直接从内存移除节点（etcd 已确认删除后）。
func (r *LeaseEtcdRegistry) removeMemory(id string) {
	r.reg.mu.Lock()
	delete(r.reg.nodes, id)
	delete(r.reg.offlineSince, id)
	r.reg.mu.Unlock()
}

// ── 读路径 + 接口面 ────────────────────────────────────────────────────

// TakeGCEvents 返回被 GC 移除（etcd 已确认删除）的 nodeID 并清空（排空
// 语义）；供集成层级联删除 /edgeflow/devicestatus/<nodeID>/ 子树。
func (r *LeaseEtcdRegistry) TakeGCEvents() []string {
	r.lm.Lock()
	defer r.lm.Unlock()
	if len(r.pendingEvents) == 0 {
		return nil
	}
	out := append([]string(nil), r.pendingEvents...)
	r.pendingEvents = r.pendingEvents[:0]
	return out
}

// Close 停止后台循环（watch/重扫/续约/GC 随 ctx 或 closeCh 退出；不关 kv
// ——底层连接归 etcdstore 基础层，由装配层按关停顺序最后关闭）。
func (r *LeaseEtcdRegistry) Close() error {
	r.once.Do(func() { close(r.closeCh) })
	return nil
}

// 读路径全部走内存缓存（与 EtcdRegistry 同构），直接委托内部 Registry。
func (r *LeaseEtcdRegistry) Get(nodeID string) (NodeInfo, bool) { return r.reg.Get(nodeID) }
func (r *LeaseEtcdRegistry) List() []NodeInfo                   { return r.reg.List() }
func (r *LeaseEtcdRegistry) Count() int                         { return r.reg.Count() }
func (r *LeaseEtcdRegistry) ToEdgeNode(info *NodeInfo) *v1alpha1.EdgeNode {
	return r.reg.ToEdgeNode(info)
}
func (r *LeaseEtcdRegistry) ListEdgeNodes() []v1alpha1.EdgeNode { return r.reg.ListEdgeNodes() }
