// 设备影子 etcd 持久化包装（v0.4.0 存储改造，设计 §1/§2/§5）。
//
// 架构形态：内存 Map 保留为读写缓存，etcd 为持久化事实源，写穿
// write-through（etcd 先写、成功后才更新内存）。
//
//   - 读路径（Get/ListByNode/ListAll/Count）：恒走内存缓存，永不读 etcd；
//   - 写路径 SetDesired（云端指令落点）：同步写穿——先 etcd Put 成功再更新
//     内存；etcd 失败 → 返回 error、内存不动、调用方记日志；
//   - Upsert（边→云 reported 上报）：仅内存字段级合并（保 Desired），
//     **不落盘**——Properties/LastReportedAt 是周期上报瞬态；
//   - Delete / DeleteByNode：同步删 etcd 键后再删内存；
//   - Load：启动时对键空间前缀全量 Range → 反序列化 → 灌回内存缓存。
//
// 键空间（与 registry 同事的 KV 消费接口对齐，键值 JSON 小驼峰，etcdctl
// 可人读）：/edgeflow/devicestatus/<nodeID>/<ns>/<deviceName>，值为
// DeviceShadowRecord（仅 Desired 落盘）。节点 GC 时由 registry 包装层
// 调用 DeleteByNode 级联删除 /edgeflow/devicestatus/<nodeID>/ 整棵子树。
package devicestatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/pkg/log"
)

// KeyPrefixDeviceStatus 是设备影子在 etcd 键空间中的根前缀（设计 §2）。
// 完整键：<prefix>/<nodeID>/<ns>/<deviceName>。
const KeyPrefixDeviceStatus = "/edgeflow/devicestatus"

// KVEntry 是前缀扫描返回的单个键值对。
// v0.4.0 集成期统一为 etcdstore 基础层类型别名（见 KVStore 注释）。
type KVEntry = etcdstore.KVEntry

// KVStore 是本包对底层 KV 存储（etcd clientv3 KV 子集）的消费接口，
// 即 cloud/pkg/etcdstore 基础层合同（类型别名，非本地定义）：未来切外部
// etcd（v0.5+）时本接口与业务层零改动。
type KVStore = etcdstore.KVStore

// deviceShadowRecord 是设备影子的 etcd 持久化 DTO（设计 §2）。
// 显式**不含** properties / lastReportedAt（瞬态上报，不落盘）；
// 字段名与 DeviceStatus 的 JSON tag 一致（小驼峰）。
type deviceShadowRecord struct {
	NodeID     string             `json:"nodeID"`
	DeviceName string             `json:"deviceName"`
	Namespace  string             `json:"namespace"`
	Desired    map[string]float64 `json:"desired"`
}

// toDeviceShadowRecord 把内存态设备状态收缩为持久化 DTO（丢弃瞬态字段）。
func toDeviceShadowRecord(ds DeviceStatus) deviceShadowRecord {
	return deviceShadowRecord{
		NodeID:     ds.NodeID,
		DeviceName: ds.DeviceName,
		Namespace:  ds.Namespace,
		Desired:    ds.Desired,
	}
}

// toDeviceStatus 把持久化 DTO 还原为内存态设备状态，并做加载归一：
// Properties 恒为非 nil 空 map（JSON 编码为 {} 而非 null，与 API 层
// cloneDeviceStatus 惯例一致），LastReportedAt 归零（上报瞬态不落盘）。
func (r deviceShadowRecord) toDeviceStatus() DeviceStatus {
	return DeviceStatus{
		NodeID:     r.NodeID,
		DeviceName: r.DeviceName,
		Namespace:  normNamespace(r.Namespace),
		Properties: map[string]float64{},
		Desired:    r.Desired,
	}
}

// deviceStoreConfig 是 EtcdDeviceStore 的可选配置。
type deviceStoreConfig struct {
	prefix string // 键空间根前缀，默认 KeyPrefixDeviceStatus
}

// Option 是 EtcdDeviceStore 构造选项（对齐设计 §3 预留 WithLeaseTTL 的形态）。
type Option func(*deviceStoreConfig)

// WithKeyPrefix 覆盖键空间根前缀（默认 /edgeflow/devicestatus）。
// 主要供测试隔离与多租户场景使用；生产默认值即可。
func WithKeyPrefix(prefix string) Option {
	return func(c *deviceStoreConfig) { c.prefix = prefix }
}

// ErrDesiredConflict 是 SetDesired 的 CAS 冲突耗尽哨兵（v0.6.0 设计 §3.2）：
// 并发修改重试 ≤3 次后仍冲突，Desired 未更新。API 层语义不变（200 + 日志，
// 不翻转——指令已到边缘，运维凭日志重发）。
var ErrDesiredConflict = errors.New("devicestatus: SetDesired 写冲突（并发修改，重试耗尽），Desired 未更新")

// EtcdDeviceStore 是 DeviceStatusStore 的写穿 etcd 包装：
// 内存缓存（*DeviceStatusStore）+ 底层 KV。
//
// v0.6.0 多副本（设计 §3）：SetDesired 改 etcd modRevision CAS（读 etcd 基准
// → 合并 → CompareAndPut，冲突重试 ≤3，耗尽返回 ErrDesiredConflict）——
// 消灭 L5 整记录覆盖；两模式统一走 CAS（D4：embed 单副本无并发 → CAS 恒
// 成功，行为等价）。StartWatch/LoadAnchored 仅外部模式装配消费（非
// ExtendedKV 时 StartWatch no-op + Warn）。
//
// 并发安全：写穿关键区（SetDesired/Delete/DeleteByNode）由内部 mu 串行化，
// 保证"CAS/写穿 → 内存更新"原子，避免并发指令丢失更新；读方法直接委托
// 内存存储（自身加锁），不触碰 etcd。
type EtcdDeviceStore struct {
	mu     sync.Mutex
	mem    *DeviceStatusStore  // 读写缓存（读路径唯一事实源）
	kv     KVStore             // 持久化事实源（写穿目标）
	ext     etcdstore.ExtendedKV // 扩展面（CAS/watch；非扩展 KV 时为 nil，退化路径）
	prefix string

	watchRev atomic.Int64 // LoadAnchored 返回的锚点（watch 应用器重连/重放用）

	lastReload time.Time // 最近一次全量重放时刻（重放降频，s.mu 保护）
}

// NewEtcdDeviceStore 创建写穿包装。
//
// 错误：kv 为 nil、或前缀非法（空 / 不以 "/" 开头）时返回 error。
// 内存缓存从空开始，启动后调用方需先 Load(ctx) 灌回持久化数据再对外服务。
func NewEtcdDeviceStore(kv KVStore, opts ...Option) (*EtcdDeviceStore, error) {	if kv == nil {
		return nil, fmt.Errorf("devicestatus: NewEtcdDeviceStore 需要非 nil KVStore")
	}
	cfg := deviceStoreConfig{prefix: KeyPrefixDeviceStatus}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.prefix == "" || !strings.HasPrefix(cfg.prefix, "/") {
		return nil, fmt.Errorf("devicestatus: 非法键前缀 %q（须非空且以 '/' 开头）", cfg.prefix)
	}
	ext, _ := kv.(etcdstore.ExtendedKV) // 非扩展 KV（理论仅自定义实现）→ nil，CAS/watch 退化
	return &EtcdDeviceStore{
		mem:    NewStore(),
		kv:     kv,
		ext:     ext,
		prefix: strings.TrimSuffix(cfg.prefix, "/"),
	}, nil
}

// nodeIDRe 是 nodeID 的键约束（设计 §2 硬性约束：nodeID 含 '/' 会破坏
// 前缀扫描与单键定位，写入口统一校验，非法 → 拒绝写入并告警）。
var nodeIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validateKeyParts 校验键空间三段标识（namespace/deviceName 须已归一、
// 非空且不含 '/'；nodeID 匹配白名单）。返回描述性 error，非法即拒绝。
func validateKeyParts(nodeID, namespace, deviceName string) error {
	if !nodeIDRe.MatchString(nodeID) {
		return fmt.Errorf("devicestatus: 非法 nodeID %q（须匹配 ^[A-Za-z0-9._-]+$，不含 '/'）", nodeID)
	}
	if namespace == "" || strings.Contains(namespace, "/") {
		return fmt.Errorf("devicestatus: 非法 namespace %q（非空且不含 '/'）", namespace)
	}
	if deviceName == "" || strings.Contains(deviceName, "/") {
		return fmt.Errorf("devicestatus: 非法 deviceName %q（非空且不含 '/'）", deviceName)
	}
	return nil
}

// deviceKey 构造完整 etcd 键（namespace 须已归一）。
func (s *EtcdDeviceStore) deviceKey(nodeID, namespace, deviceName string) string {
	return s.prefix + "/" + nodeID + "/" + namespace + "/" + deviceName
}

// Upsert 写入（或更新）一台设备的已上报状态。
//
// v0.4.0 **不落盘**：reported 是周期上报瞬态，仅内存字段级合并
// （保 Desired 语义由内存实现保证，加载后依然成立）。恒返回 nil。
func (s *EtcdDeviceStore) Upsert(nodeID string, ds DeviceStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mem.Upsert(nodeID, ds)
}

// SetDesired 写入（或更新）一台设备的期望值（云端指令落点，写穿）。
//
// v0.6.0（设计 §3，D4）：**CAS 是唯一写穿路径**（两模式统一）——读 etcd 基准
// （非内存！任何副本的陈旧内存都不可能成为合并基准，L5 整记录覆盖消失）→
// 合并 → CompareAndPut（ModRevision 比较；基准不存在 → create-if-absent）→
// 冲突重试 ≤3 次，耗尽返回 ErrDesiredConflict（内存不动）。
// 非扩展 KV（理论仅自定义实现）退化为 v0.5.0 读内存合并写穿路径。
//
// 键约束：nodeID/namespace/deviceName 非法（含 '/' 等）→ 拒绝写入并
// 返回 error；空参数与内存实现一致忽略（返回 nil）。
func (s *EtcdDeviceStore) SetDesired(nodeID, namespace, deviceName, property string, value float64) error {
	if nodeID == "" || deviceName == "" || property == "" {
		return nil
	}
	ns := normNamespace(namespace)
	if err := validateKeyParts(nodeID, ns, deviceName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ext == nil {
		// 退化路径（非 ExtendedKV）：v0.5.0 读内存合并 → Put。
		merged, ok := s.mem.Get(nodeID, ns, deviceName)
		if !ok {
			merged = DeviceStatus{NodeID: nodeID, DeviceName: deviceName, Namespace: ns}
			merged = cloneDeviceStatus(merged)
		}
		merged.Desired[property] = value
		data, err := json.Marshal(toDeviceShadowRecord(merged))
		if err != nil {
			return err
		}
		if err := s.kv.Put(context.Background(), s.deviceKey(nodeID, ns, deviceName), data); err != nil {
			return fmt.Errorf("devicestatus: SetDesired 写穿失败（内存未更新）: %w", err)
		}
		return s.mem.SetDesired(nodeID, ns, deviceName, property, value)
	}

	key := s.deviceKey(nodeID, ns, deviceName)
	ctx := context.Background()
	for attempt := 0; attempt < 3; attempt++ {
		cur, modRev, err := s.ext.GetWithRev(ctx, key)
		if err != nil {
			return fmt.Errorf("devicestatus: SetDesired 读 etcd 基准失败（内存未更新）: %w", err)
		}
		var merged DeviceStatus
		if modRev > 0 {
			var rec deviceShadowRecord
			if err := json.Unmarshal(cur, &rec); err != nil {
				return fmt.Errorf("devicestatus: SetDesired 基准记录损坏（键 %s）: %w", key, err)
			}
			merged = rec.toDeviceStatus()
		} else {
			merged = DeviceStatus{
				NodeID:     nodeID,
				DeviceName: deviceName,
				Namespace:  ns,
				Properties: map[string]float64{},
			}
		}
		if merged.Desired == nil {
			merged.Desired = map[string]float64{}
		}
		merged.Desired[property] = value
		data, err := json.Marshal(toDeviceShadowRecord(merged))
		if err != nil {
			return err
		}
		ok, err := s.ext.CompareAndPut(ctx, key, data, modRev)
		if err != nil {
			return fmt.Errorf("devicestatus: SetDesired 写穿失败（内存未更新）: %w", err)
		}
		if ok {
			// CAS 成功：内存更新（保 reported，语义与 v0.5.0 逐值一致）
			return s.mem.SetDesired(nodeID, ns, deviceName, property, value)
		}
		// 冲突（他副本/他请求刚写过）→ 读最新基准再合并重试
	}
	return fmt.Errorf("%w（nodeID=%s device=%s property=%s）", ErrDesiredConflict, nodeID, deviceName, property)
}

// Delete 删除一台设备状态（写穿：先删 etcd 键，成功后再删内存）。
//
// 接口签名受现有调用点约束保持 bool（见 Store 接口注释）：etcd 删除
// 失败时无法以返回值上抛——记 Error 日志、内存不动（保持读写一致，
// 记录仍在），返回 false 表示"未删除"。幂等：etcd Delete 对不存在键
// 静默成功；内存不存在返回 false。非法键（写入路径已拒绝，理论上不
// 存在对应 etcd 键）仅走内存删除。
func (s *EtcdDeviceStore) Delete(nodeID, namespace, deviceName string) bool {
	if nodeID == "" || deviceName == "" {
		return false
	}
	ns := normNamespace(namespace)
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateKeyParts(nodeID, ns, deviceName); err == nil {
		if err := s.kv.Delete(context.Background(), s.deviceKey(nodeID, ns, deviceName)); err != nil {
			log.Errorf("devicestatus: Delete 写穿失败（内存未删除）: %v", err)
			return false
		}
	}
	return s.mem.Delete(nodeID, ns, deviceName)
}

// DeleteByNode 级联删除指定节点的全部设备记录（供 registry GC 调用，
// 设计 §3：节点生命周期是设备数据唯一正确的清理信号）。
//
// 先 DeleteRange /edgeflow/devicestatus/<nodeID>/ 整棵子树，成功后再清
// 内存中该节点的全部记录；etcd 失败 → 返回 error、内存不动。
// 空 nodeID 为幂等 no-op；nodeID 非法（含 '/'）返回 error。
func (s *EtcdDeviceStore) DeleteByNode(nodeID string) error {
	if nodeID == "" {
		return nil
	}
	if !nodeIDRe.MatchString(nodeID) {
		return fmt.Errorf("devicestatus: DeleteByNode 非法 nodeID %q", nodeID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.kv.DeleteRange(context.Background(), s.prefix+"/"+nodeID+"/"); err != nil {
		return fmt.Errorf("devicestatus: DeleteByNode 写穿失败（内存未清理）: %w", err)
	}
	s.mem.mu.Lock()
	delete(s.mem.devices, nodeID)
	s.mem.mu.Unlock()
	return nil
}

// Load 从 etcd 全量加载设备影子并灌回内存缓存（启动时调用，服务前完成，
// 语义与 v0.5.0 逐位不变——embed 路径回归锚点）。
func (s *EtcdDeviceStore) Load(ctx context.Context) error {
	entries, err := s.kv.ListByPrefix(ctx, s.prefix+"/")
	if err != nil {
		return fmt.Errorf("devicestatus: Load 前缀扫描失败: %w", err)
	}
	s.applyLoad(entries)
	return nil
}

// LoadAnchored 锚定加载（v0.6.0 设计 §2.1，外部模式装配用）：前缀扫描 +
// 返回响应头 revision 作为 watch 锚点。内存重建语义与 Load 完全一致。
// 非 ExtendedKV（无法锚定）→ 返回 error。
func (s *EtcdDeviceStore) LoadAnchored(ctx context.Context) (int64, error) {
	if s.ext == nil {
		return 0, fmt.Errorf("devicestatus: LoadAnchored 需要 ExtendedKV（当前 KV 不支持修订锚定）")
	}
	entries, rev, err := s.ext.ListByPrefixRev(ctx, s.prefix+"/")
	if err != nil {
		return 0, fmt.Errorf("devicestatus: LoadAnchored 前缀扫描失败: %w", err)
	}
	s.applyLoad(entries)
	s.watchRev.Store(rev)
	return rev, nil
}

// applyLoad 把扫描结果整体灌回内存缓存（Load/LoadAnchored 共用；锁序与
// v0.5.0 Load 一致：s.mu → s.mem.mu）。
func (s *EtcdDeviceStore) applyLoad(entries []KVEntry) {
	fresh := make(map[string]map[deviceKey]DeviceStatus, len(entries))
	skipped := 0
	for _, e := range entries {
		var rec deviceShadowRecord
		if err := json.Unmarshal(e.Value, &rec); err != nil {
			skipped++
			log.Warnf("devicestatus: Load 跳过坏键 %q（JSON 反序列化失败: %v）", e.Key, err)
			continue
		}
		if err := validateKeyParts(rec.NodeID, normNamespace(rec.Namespace), rec.DeviceName); err != nil {
			skipped++
			log.Warnf("devicestatus: Load 跳过非法记录键 %q（%v）", e.Key, err)
			continue
		}
		ns := normNamespace(rec.Namespace)
		byKey, ok := fresh[rec.NodeID]
		if !ok {
			byKey = make(map[deviceKey]DeviceStatus)
			fresh[rec.NodeID] = byKey
		}
		byKey[deviceKeyOf(ns, rec.DeviceName)] = rec.toDeviceStatus()
	}
	if skipped > 0 {
		log.Warnf("devicestatus: Load 完成，跳过 %d 条坏记录（共扫描 %d 键）", skipped, len(entries))
	}

	s.mu.Lock()
	s.mem.mu.Lock()
	s.mem.devices = fresh
	s.mem.mu.Unlock()
	s.mu.Unlock()
}

// StartWatch 启动 watch 应用器（v0.6.0 多副本读一致，设计 §2）：单 goroutine
// 单调应用 PUT/DELETE；断线/ErrCompacted → 全量重放（降频）→ 重锚定。
// 应用器只读铁律：只改内存（保留本地 reported/LastReportedAt），绝不发
// etcd 写（设计 §2.2②）。非 ExtendedKV → no-op + Warn。
func (s *EtcdDeviceStore) StartWatch(ctx context.Context) {
	if s.ext == nil {
		log.Warnf("devicestatus: 底层 KV 不支持 watch（非 ExtendedKV），watch 缓存同步停用（no-op）——读一致性降级为启动快照")
		return
	}
	go s.watchLoop(ctx)
}

// watchLoop 锚定增量应用主循环：watch 断开/Err（含 ErrCompacted）→ 全量
// 重放（逐记录合并，保留本地瞬态）→ 重锚定 → 重启 watch。
func (s *EtcdDeviceStore) watchLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		rev := s.watchRev.Load()
		ch := s.ext.WatchPrefix(ctx, s.prefix+"/", etcdstore.StartRevision(rev))
		for ev := range ch {
			switch ev.Type {
			case etcdstore.WatchEventPut:
				s.applyPut(ev.Key, ev.Value)
				s.watchRev.Store(ev.ModRevision) // 单调推进锚点：断线重放只补增量，不重放已应用事件（R2-3）
			case etcdstore.WatchEventDelete:
				s.applyDelete(ev.Key)
				s.watchRev.Store(ev.ModRevision)
			case etcdstore.WatchEventErr:
				if ctx.Err() != nil {
					return
				}
				s.reloadAll(ctx)
			}
		}
		if ctx.Err() != nil {
			return
		}
		s.reloadAll(ctx)
	}
}

// applyPut 应用设备影子 PUT（多副本合并语义：只覆盖 Desired 与身份字段，
// 保留本地 reported/LastReportedAt——reported 是各副本本地瞬态，不能被他
// 副本的持久化数据冲掉，设计 §2.2 表）。
func (s *EtcdDeviceStore) applyPut(key string, value []byte) {
	var rec deviceShadowRecord
	if err := json.Unmarshal(value, &rec); err != nil {
		log.Warnf("devicestatus: 忽略坏影子事件 %q（JSON 反序列化失败: %v）", key, err)
		return
	}
	if err := validateKeyParts(rec.NodeID, normNamespace(rec.Namespace), rec.DeviceName); err != nil {
		log.Warnf("devicestatus: 忽略非法影子事件 %q（%v）", key, err)
		return
	}
	s.mu.Lock()
	s.mem.mu.Lock()
	ns := normNamespace(rec.Namespace)
	byNode, ok := s.mem.devices[rec.NodeID]
	if !ok {
		byNode = make(map[deviceKey]DeviceStatus)
		s.mem.devices[rec.NodeID] = byNode
	}
	k := deviceKeyOf(ns, rec.DeviceName)
	if existing, ok := byNode[k]; ok {
		// 合并：Desired 以 etcd 为准，reported（Properties/LastReportedAt）保留本地
		existing.Desired = normalizeDesiredMap(rec.Desired)
		existing.NodeID = rec.NodeID
		existing.DeviceName = rec.DeviceName
		existing.Namespace = ns
		byNode[k] = existing
	} else {
		byNode[k] = rec.toDeviceStatus()
	}
	s.mem.mu.Unlock()
	s.mu.Unlock()
}

// applyDelete 应用设备影子 DELETE（单键移除）。
func (s *EtcdDeviceStore) applyDelete(key string) {
	parts := strings.Split(strings.TrimPrefix(key, s.prefix+"/"), "/")
	if len(parts) != 3 {
		log.Warnf("devicestatus: 忽略非法删除事件 %q（键段数 %d ≠ 3）", key, len(parts))
		return
	}
	s.mu.Lock()
	s.mem.Delete(parts[0], parts[1], parts[2])
	s.mu.Unlock()
}

// reloadAll 全量重放（断线/ErrCompacted）：ListByPrefixRev → 逐记录合并
// （不整体替换内存——保留本地 reported 与刚写入未回流的瞬态，设计 §2.3）
// → 移除 etcd 已不存在的记录 → 重锚定。重放降频：相邻重放间隔 ≥ 30s。
func (s *EtcdDeviceStore) reloadAll(ctx context.Context) {
	s.mu.Lock()
	now := time.Now()
	if !s.lastReload.IsZero() && now.Sub(s.lastReload) < watchReloadMinInterval {
		wait := watchReloadMinInterval - now.Sub(s.lastReload)
		s.mu.Unlock()
		log.Warnf("devicestatus: watch 重放过频，降频等待 %v 后全量重放", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	} else {
		s.mu.Unlock()
	}

	entries, rev, err := s.ext.ListByPrefixRev(ctx, s.prefix+"/")
	if err != nil {
		log.Warnf("devicestatus: 全量重放失败（保持旧内存，stale-but-consistent）: %v", err)
		return
	}
	s.mu.Lock()
	s.lastReload = time.Now()
	s.mu.Unlock()

	present := make(map[string]struct{}, len(entries))
	for _, ent := range entries {
		var rec deviceShadowRecord
		if err := json.Unmarshal(ent.Value, &rec); err != nil {
			continue
		}
		if err := validateKeyParts(rec.NodeID, normNamespace(rec.Namespace), rec.DeviceName); err != nil {
			continue
		}
		present[ent.Key] = struct{}{}
		s.applyPut(ent.Key, ent.Value)
	}
	// 内存有、etcd 无 → 已被删除（GC 级联/他副本）：移除
	s.mu.Lock()
	s.mem.mu.Lock()
	for nodeID, byNode := range s.mem.devices {
		for k := range byNode {
			full := s.prefix + "/" + nodeID + "/" + string(k)
			if _, ok := present[full]; !ok {
				delete(byNode, k)
			}
		}
		if len(byNode) == 0 {
			delete(s.mem.devices, nodeID)
		}
	}
	s.mem.mu.Unlock()
	s.mu.Unlock()
	s.watchRev.Store(rev)
}

// watchReloadMinInterval 是 watch 全量重放的最小间隔（防重连风暴，设计 §2.3；
// 对齐默认重扫周期 30s）。
const watchReloadMinInterval = 30 * time.Second

// normalizeDesiredMap 归一 Desired 为恒非 nil 的 map（JSON null → {}）。
func normalizeDesiredMap(m map[string]float64) map[string]float64 {
	if m == nil {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Close 关闭底层 KV 存储（集成层按关停顺序最后调用，见设计 §6.1）。
func (s *EtcdDeviceStore) Close() error {
	return s.kv.Close()
}

// Get 查询单台设备状态（读路径：恒走内存缓存）。
func (s *EtcdDeviceStore) Get(nodeID, namespace, deviceName string) (DeviceStatus, bool) {
	return s.mem.Get(nodeID, namespace, deviceName)
}

// ListByNode 返回指定节点的全部设备状态（读路径：恒走内存缓存）。
func (s *EtcdDeviceStore) ListByNode(nodeID string) []DeviceStatus {
	return s.mem.ListByNode(nodeID)
}

// ListAll 返回全部设备状态（读路径：恒走内存缓存）。
func (s *EtcdDeviceStore) ListAll() []DeviceStatus {
	return s.mem.ListAll()
}

// Count 返回记录总数（读路径：恒走内存缓存）。
func (s *EtcdDeviceStore) Count() int {
	return s.mem.Count()
}

// 编译期断言：EtcdDeviceStore 实现 Store 契约（与纯内存实现可互换装配）。
var _ Store = (*EtcdDeviceStore)(nil)

// 键值对按 Key 排序（fake KV 与未来 etcd 前缀扫描都保持确定性输出）。
func sortEntries(entries []KVEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
}
