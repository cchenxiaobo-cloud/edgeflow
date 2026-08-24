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
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

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

// EtcdDeviceStore 是 DeviceStatusStore 的写穿 etcd 包装：
// 内存缓存（*DeviceStatusStore）+ 底层 KV。
//
// 并发安全：写穿关键区（SetDesired/Delete/DeleteByNode）由内部 mu 串行化，
// 保证"读内存合并 → etcd Put → 内存更新"原子，避免并发指令丢失更新；
// 读方法直接委托内存存储（自身加锁），不触碰 etcd。
type EtcdDeviceStore struct {
	mu     sync.Mutex
	mem    *DeviceStatusStore // 读写缓存（读路径唯一事实源）
	kv     KVStore            // 持久化事实源（写穿目标）
	prefix string
}

// NewEtcdDeviceStore 创建写穿包装。
//
// 错误：kv 为 nil、或前缀非法（空 / 不以 "/" 开头）时返回 error。
// 内存缓存从空开始，启动后调用方需先 Load(ctx) 灌回持久化数据再对外服务。
func NewEtcdDeviceStore(kv KVStore, opts ...Option) (*EtcdDeviceStore, error) {
	if kv == nil {
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
	return &EtcdDeviceStore{
		mem:    NewStore(),
		kv:     kv,
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
// 顺序：读内存合并已有 Desired → etcd Put 成功 → 内存更新。etcd 失败
// → 返回 error、内存不动（"写成功 = 已持久化"）。设备记录不存在时
// 自动创建并落盘（指令即声明）。
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

	// 字段级合并：保留已有期望值（Get 返回拷贝，不污染存储）。
	merged, ok := s.mem.Get(nodeID, ns, deviceName)
	if !ok {
		merged = DeviceStatus{NodeID: nodeID, DeviceName: deviceName, Namespace: ns}
		merged = cloneDeviceStatus(merged) // nil map 归一为空 map
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

// Load 从 etcd 全量加载设备影子并灌回内存缓存（启动时调用，服务前完成）。
//
// 语义（设计 §2/§5/§6.5）：
//   - 前缀扫描 <prefix>/ 全部键，反序列化 DeviceShadowRecord；
//   - 加载归一：Properties 为空 map {}、LastReportedAt=0（不落盘字段）；
//   - 单键 JSON 损坏或键字段非法：跳过该键 + 告警，不阻断全库；
//   - kv 层失败（ListByPrefix error）：整体失败返回 error（由集成层按
//     降级策略处理，见设计 §6.5）。
//
// 加载完成后内存态 Upsert 合并保 Desired 的语义照常成立（单测覆盖）。
func (s *EtcdDeviceStore) Load(ctx context.Context) error {
	entries, err := s.kv.ListByPrefix(ctx, s.prefix+"/")
	if err != nil {
		return fmt.Errorf("devicestatus: Load 前缀扫描失败: %w", err)
	}

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
	return nil
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
