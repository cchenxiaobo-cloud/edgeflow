package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	v1alpha1 "edgeflow/apis/edge/v1alpha1"
	"edgeflow/pkg/log"

	"edgeflow/cloud/pkg/etcdstore"
)

// 本文件实现注册表的 etcd 写穿包装（EtcdRegistry）：内存 Map 保留为
// 读写缓存，etcd 键空间 /edgeflow/registry/nodes/<nodeID> 为持久化事实源。
//
// 写穿顺序（设计文档 §1）：Register 先 Put etcd 成功、再更新内存；
// etcd 失败 → 返回 error、内存不动。心跳（UpdateHeartbeat）与离线标记
// （MarkOffline）是瞬态（§3/§5），仅内存、不进 etcd。GC 删除需要持久化：
// 被清理节点的 etcd 键由包装层同步删除（DeleteRange），nodeID 集合经
// TakeGCEvents 外露，供集成层级联删 /edgeflow/devicestatus/<nodeID>/ 子树。
//
// 本包只依赖消费面接口 KVStore（Go 结构化接口），具体实现由
// cloud/pkg/etcdstore 基础层提供（embed 客户端或外部 etcd 均满足）。

// KeyNodesPrefix 是节点注册元数据在 etcd 中的键空间前缀。
const KeyNodesPrefix = "/edgeflow/registry/nodes/"

// CleanupLoopIntervalDefault 是定期清理循环的默认间隔（对齐
// edge/pkg/metamanager/ledger.go 的 RunCleanupLoop 模式）。
const CleanupLoopIntervalDefault = time.Hour

// KVEntry 是键值对的通用形态（ListByPrefix 返回）。
// v0.4.0 集成期统一为 etcdstore 基础层类型别名：结构型接口要求方法签名
// 中的类型逐字一致，本地定义会破坏赋值兼容（etcdstore 不依赖本包，无循环）。
type KVEntry = etcdstore.KVEntry

// KVStore 是注册表包装消费的最小 KV 能力面（cf etcdstore.KVStore）：
// 由 cloud/pkg/etcdstore 基础层实现（embed clientv3 或未来外部 etcd 均
// 满足），本包仅按方法集消费，未来切外部 etcd 时业务层零改动。
type KVStore = etcdstore.KVStore

// nodeRecord 是节点注册元数据的持久化 DTO（JSON 小驼峰，字段与 NodeInfo
// 一致但显式不含 lastHeartbeatAt/status 两个瞬态字段，设计文档 §2/§5）。
// 任何能读 etcd 的客户端/etcdctl 都能直接人读。
type nodeRecord struct {
	NodeID          string `json:"nodeID"`
	NodeName        string `json:"nodeName"`
	Arch            string `json:"arch"`
	OS              string `json:"os"`
	EdgecoreVersion string `json:"edgecoreVersion"`
	CPU             int    `json:"cpu"`
	Memory          uint64 `json:"memory"`
	IP              string `json:"ip"`
	RegisteredAt    int64  `json:"registeredAt"`
}

// toNodeInfo 把持久化 DTO 还原为内存形态：瞬态字段按"重启后未知"语义
// 初始化（Status=Unknown、LastHeartbeatAt=0），由后续心跳/注册翻新。
func (r nodeRecord) toNodeInfo() NodeInfo {
	return NodeInfo{
		NodeID:          r.NodeID,
		NodeName:        r.NodeName,
		Arch:            r.Arch,
		OS:              r.OS,
		EdgecoreVersion: r.EdgecoreVersion,
		CPU:             r.CPU,
		Memory:          r.Memory,
		IP:              r.IP,
		RegisteredAt:    r.RegisteredAt,
		LastHeartbeatAt: 0,
		Status:          StatusUnknown,
	}
}

// fromNodeInfo 把内存形态收敛为持久化 DTO（丢弃瞬态字段）。
func fromNodeInfo(n NodeInfo) nodeRecord {
	return nodeRecord{
		NodeID:          n.NodeID,
		NodeName:        n.NodeName,
		Arch:            n.Arch,
		OS:              n.OS,
		EdgecoreVersion: n.EdgecoreVersion,
		CPU:             n.CPU,
		Memory:          n.Memory,
		IP:              n.IP,
		RegisteredAt:    n.RegisteredAt,
	}
}

// nodeKey 构造节点台账的 etcd 键；调用方须先经 validNodeID 校验。
func nodeKey(nodeID string) string {
	return KeyNodesPrefix + nodeID
}

// EtcdRegistry 是注册表的 etcd 写穿实现（实现 Store 接口）。
//
// 内部包含一个内存 Registry（读写缓存，含全部既有语义：排序、拷贝、
// 惰性 GC、TTL）；etcd 只同步"需要持久化"的写（注册、GC 删除）。
// GC 事件流：内存 GC 移除节点 → 包装层 sweepGC 同步删 etcd 键（失败
// 重入队、告警不阻断）→ nodeID 列表经 TakeGCEvents 外露给集成层做
// devicestatus 子树级联删除。
type EtcdRegistry struct {
	reg *Registry // 内存缓存（也是全部读路径的数据源）
	kv  KVStore   // 持久化事实源

	pmu           sync.Mutex
	pendingEvents []string // 待集成层取走的 GC 事件（取走后即清空）

	closeCh chan struct{}
	once    sync.Once
}

// 编译期断言：etcd 写穿实现满足 Store 接口。
var _ Store = (*EtcdRegistry)(nil)

// NewEtcdRegistry 创建注册表的 etcd 写穿包装。kv 为 KVStore 实现
// （通常来自 cloud/pkg/etcdstore 基础层）；opts 与 New 一致（如
// WithOfflineTTL，默认 OfflineTTLDefault=24h，由集成层按
// EDGEFLOW_CLOUDCORE_NODE_RETENTION 喂入，保证内存/etcd 同一口径）。
func NewEtcdRegistry(kv KVStore, opts ...Option) (*EtcdRegistry, error) {
	if kv == nil {
		return nil, fmt.Errorf("registry: NewEtcdRegistry 需要非 nil 的 KVStore")
	}
	e := &EtcdRegistry{
		reg:     New(opts...),
		kv:      kv,
		closeCh: make(chan struct{}),
	}
	return e, nil
}

// Register 写穿登记：先 Put etcd（含节点台账 DTO），成功后才更新内存缓存；
// etcd 失败 → 返回 error、内存不动（"写成功 = 已持久化"，设计文档 §1）。
// 本次写触发的惰性 GC 同步清理 etcd 键（sweepGC）。
func (e *EtcdRegistry) Register(info NodeInfo) error {
	if !validNodeID(info.NodeID) {
		return fmt.Errorf("registry: 拒绝非法 nodeID %q（允许 [A-Za-z0-9._-]）", info.NodeID)
	}
	data, err := json.Marshal(fromNodeInfo(info))
	if err != nil {
		return fmt.Errorf("registry: 序列化节点台账失败: %w", err)
	}
	key := nodeKey(info.NodeID)
	if err := e.kv.Put(context.Background(), key, data); err != nil {
		return fmt.Errorf("registry: etcd Put %s 失败: %w", key, err)
	}
	if err := e.reg.Register(info); err != nil {
		// 内存写入失败（理论上仅键校验，上面已拦截）：回滚 etcd 键，避免孤儿台账
		_ = e.kv.Delete(context.Background(), key)
		return err
	}
	e.sweepGC()
	return nil
}

// UpdateHeartbeat 刷新心跳：瞬态，仅内存、不进 etcd（设计文档 §3/§5）。
// 心跳触发的惰性 GC 仍会同步清理 etcd 键。
func (e *EtcdRegistry) UpdateHeartbeat(nodeID string, ts int64) error {
	if err := e.reg.UpdateHeartbeat(nodeID, ts); err != nil {
		return err
	}
	e.sweepGC()
	return nil
}

// MarkOffline 标记离线：瞬态，仅内存、不进 etcd。触发的惰性 GC 同步
// 清理 etcd 键。
func (e *EtcdRegistry) MarkOffline(nodeID string) error {
	if err := e.reg.MarkOffline(nodeID); err != nil {
		return err
	}
	e.sweepGC()
	return nil
}

// Seed 播种（启动加载用）：与内存 Seed 语义一致（Unknown 状态、offlineSince
// 以 since 起算），随即执行启动清理——过期节点从内存移除、etcd 键同步删除，
// 事件经 TakeGCEvents 外露。集成层用法：Load 失败降级时可用本方法播种空集。
func (e *EtcdRegistry) Seed(nodes []NodeInfo, since int64) error {
	if err := e.reg.Seed(nodes, since); err != nil {
		return err
	}
	e.sweepGC()
	return nil
}

// Load 从 etcd 全量加载节点台账并灌入内存缓存（每次启动调用一次）。
// 单键反序列化失败/键内容非法：跳过该键 + 告警（坏键不阻断全库，
// 设计文档 §6.5）。加载后随即执行启动清理（Seed 语义）。
func (e *EtcdRegistry) Load(ctx context.Context) error {
	entries, err := e.kv.ListByPrefix(ctx, KeyNodesPrefix)
	if err != nil {
		return fmt.Errorf("registry: ListByPrefix %s 失败: %w", KeyNodesPrefix, err)
	}
	nodes := make([]NodeInfo, 0, len(entries))
	skipped := 0
	for _, ent := range entries {
		var rec nodeRecord
		if err := json.Unmarshal(ent.Value, &rec); err != nil {
			skipped++
			log.Warnf("[EtcdRegistry] 跳过坏键 %s（反序列化失败: %v）", ent.Key, err)
			continue
		}
		if !validNodeID(rec.NodeID) {
			skipped++
			log.Warnf("[EtcdRegistry] 跳过坏键 %s（nodeID %q 非法）", ent.Key, rec.NodeID)
			continue
		}
		nodes = append(nodes, rec.toNodeInfo())
	}
	if skipped > 0 {
		log.Warnf("[EtcdRegistry] 加载完成：恢复 %d 个节点，跳过 %d 个坏键", len(nodes), skipped)
	}
	if err := e.reg.Seed(nodes, time.Now().UnixMilli()); err != nil {
		return err
	}
	e.sweepGC()
	return nil
}

// TakeGCEvents 返回被 GC 移除的 nodeID 列表并清空（排空语义）。供集成层
// 消费：对每个 nodeID 级联删除 /edgeflow/devicestatus/<nodeID>/ 子树。
// 清理失败不重入（集成层下一轮再取即可，删除是幂等操作；本包装层对
// 自身键空间的删除失败会重入 reg 事件，见 sweepGC）。
func (e *EtcdRegistry) TakeGCEvents() []string {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if len(e.pendingEvents) == 0 {
		return nil
	}
	out := append([]string(nil), e.pendingEvents...)
	e.pendingEvents = e.pendingEvents[:0]
	return out
}

// sweepGC 把内存 GC 事件落到 etcd：对每个被移除节点同步 DeleteRange 其
// 台账键（失败告警 + 重入队，下一轮重试；不阻断调用方），并把 nodeID
// 透传给集成层（pendingEvents，TakeGCEvents 取走）。
func (e *EtcdRegistry) sweepGC() {
	ctx := context.Background()
	for _, id := range e.reg.TakeGCEvents() {
		// 精确单键删除（设计 §2：节点键为 /edgeflow/registry/nodes/<nodeID>）；
		// 不能用 DeleteRange 前缀删除——"n1" 会连带命中 "n10"/"n1abc" 等
		// 前缀冲突节点的合法台账键（跨节点数据丢失，复核 🔴-2）。
		if err := e.kv.Delete(ctx, nodeKey(id)); err != nil {
			log.Warnf("[EtcdRegistry] 删除节点 %s 的 etcd 键失败: %v（v0.6.0 L7 修复：重入队，下一轮清理重试——内存已移除，重扫不再产生该事件，必须显式重入队）", id, err)
			// v0.6.0 L7 修复（设计 §4.1，embed 唯一行为改动）：启用既有 requeueGCEvent
			// 死代码——该函数本身已实现、幂等（gcEvents 去重由 sweepGC 排空语义保证），
			// 注释语义正是此用途，只是 v0.4.0 以来无人调用。行为差异：孤儿键现在必然
			// 在下轮重试被清（v0.5.0 是永久孤儿直到重启）；其余语义逐位不变。
			e.reg.requeueGCEvent(id)
		} else {
			e.pmu.Lock()
			e.pendingEvents = append(e.pendingEvents, id)
			e.pmu.Unlock()
		}
	}
}

// CleanupLoop 启动后台定期清理 goroutine（对齐 ledger.go RunCleanupLoop
// 模式，设计文档 §3.3）：每 interval 执行一次内存 GC 扫描 + 同步删 etcd
// 键；清理失败只告警不阻断（等下一轮）。ctx 取消或 Close() 时退出。
// interval <= 0 时用 CleanupLoopIntervalDefault（1h）。
func (e *EtcdRegistry) CleanupLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = CleanupLoopIntervalDefault
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.closeCh:
				return
			case <-t.C:
				// gcLocked 要求调用方已持写锁：这里先加锁再扫描（内存 GC），
				// 随后 sweepGC 同步删 etcd 键（TakeGCEvents 自带锁）。
				e.reg.mu.Lock()
				e.reg.gcLocked(time.Now().UnixMilli())
				e.reg.mu.Unlock()
				e.sweepGC()
			}
		}
	}()
}

// Close 停止清理循环并释放包装层资源。注意：不关闭 kv（底层 etcd 客户端/
// embed 实例的生命周期归 cloud/pkg/etcdstore 基础层所有，由集成层按
// 设计文档 §6.1 关停顺序在最后统一关闭，避免双重 Close）。
func (e *EtcdRegistry) Close() error {
	e.once.Do(func() { close(e.closeCh) })
	return nil
}

// 读路径全部走内存缓存（设计文档 §1：读不触 etcd），直接委托内部 Registry。
func (e *EtcdRegistry) Get(nodeID string) (NodeInfo, bool) { return e.reg.Get(nodeID) }

// List 返回全部节点信息的拷贝，按 NodeID 排序。
func (e *EtcdRegistry) List() []NodeInfo { return e.reg.List() }

// Count 返回注册表内节点数。
func (e *EtcdRegistry) Count() int { return e.reg.Count() }

// ToEdgeNode 把 NodeInfo 映射为 EdgeNode 视图。
func (e *EtcdRegistry) ToEdgeNode(info *NodeInfo) *v1alpha1.EdgeNode {
	return e.reg.ToEdgeNode(info)
}

// ListEdgeNodes 返回全部节点的 EdgeNode 视图（按 Name 排序）。
func (e *EtcdRegistry) ListEdgeNodes() []v1alpha1.EdgeNode { return e.reg.ListEdgeNodes() }
