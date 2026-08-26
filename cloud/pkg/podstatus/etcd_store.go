// EtcdPodStore：云端 Pod 状态写穿持久化实现（v0.9.0，KNOWN-ISSUES §3 ③ 闭环）。
//
// 背景：v0.4.0 起 registry/devicestatus 三存储写穿 etcd，但 Pod 状态被排除在
// 写穿范围外（整表不落盘，cloudcore 重启后 pods 列表短暂为空，≤1 上报周期
// 自愈）。v0.9.0 补全：Upsert/Delete 写穿 etcd（键空间 /edgeflow/podstatus/
// 于 v0.4.0 预留），重启后 Pod 列表直接可见。
//
// 语义（对齐 devicestatus.EtcdDeviceStore 结构，差异点已注明）：
//   - 写路径：先 etcd 成功再更新内存缓存（"写成功 = 已持久化"）；失败返回
//     error、内存不动（与 devicestatus.Upsert 仅写内存不同——Pod 状态无
//     Desired 类 CAS 路径，Upsert 即唯一写入口，因此本层写穿）。
//   - 读路径：全走内存缓存（Get/ListByNode/ListAll/Count 无失败路径）。
//   - 多副本：LoadAnchored 锚定加载 + StartWatch 增量应用（外部模式消费）；
//     watch 应用器只写内存、永不发 etcd 写（防回写环）。
//   - 键布局：<prefix>/<nodeID>/<ns>/<podName>，nodeID/ns/podName 非法
//     （含 '/' 等）→ 拒绝写入（对齐 validateKeyParts 语义）。
package podstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/pkg/log"
)

// KeyPrefixPodStatus 是 Pod 状态在 etcd 键空间中的根前缀（v0.4.0 预留，
// v0.9.0 启用；与 devicestatus.KeyPrefixDeviceStatus 同级布局）。
const KeyPrefixPodStatus = "/edgeflow/podstatus"

// podStoreConfig 是 EtcdPodStore 构造选项（对齐 devicestatus 模式）。
type podStoreConfig struct {
	prefix string
}

// Option 是 EtcdPodStore 构造选项函数。
type Option func(*podStoreConfig)

// WithKeyPrefix 覆盖键空间根前缀（默认 KeyPrefixPodStatus；测试用）。
func WithKeyPrefix(prefix string) Option {
	return func(c *podStoreConfig) { c.prefix = prefix }
}

// EtcdPodStore 是 Pod 状态存储的写穿 etcd 实现。
//
// 并发安全：所有方法内部加锁；读路径（Get/List*/Count）与内存版语义一致
// 返回拷贝。写路径（Upsert/Delete）先 etcd 后内存，etcd 失败内存不动。
type EtcdPodStore struct {
	mu     sync.Mutex
	mem    *PodStatusStore   // 读写缓存（读路径唯一事实源）
	kv     etcdstore.KVStore // 持久化事实源（写穿目标）
	ext    etcdstore.ExtendedKV // 扩展面（watch；非扩展 KV 时为 nil，退化路径）
	prefix string

	watchRev   atomic.Int64 // LoadAnchored 返回的锚点（watch 应用器重连/重放用）
	lastReload time.Time    // 最近一次全量重放时刻（重放降频，s.mu 保护）
}

// 编译期断言：EtcdPodStore 满足 Store 契约。
var _ Store = (*EtcdPodStore)(nil)

// NewEtcdPodStore 构造写穿实现。kv 必须非 nil。内存缓存从空开始，启动后
// 调用方需先 Load(ctx) 灌回持久化数据再对外服务（对齐 EtcdDeviceStore）。
func NewEtcdPodStore(kv etcdstore.KVStore, opts ...Option) (*EtcdPodStore, error) {
	if kv == nil {
		return nil, fmt.Errorf("podstatus: NewEtcdPodStore 需要非 nil KVStore")
	}
	cfg := podStoreConfig{prefix: KeyPrefixPodStatus}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.prefix == "" || !strings.HasPrefix(cfg.prefix, "/") {
		return nil, fmt.Errorf("podstatus: 非法键前缀 %q（须非空且以 '/' 开头）", cfg.prefix)
	}
	ext, _ := kv.(etcdstore.ExtendedKV) // 非扩展 KV → nil，watch 退化
	return &EtcdPodStore{
		mem:    NewStore(),
		kv:     kv,
		ext:    ext,
		prefix: strings.TrimSuffix(cfg.prefix, "/"),
	}, nil
}

// podKeyValue 是键值的 JSON 形态（即 PodStatus 本身；键路径已含 nodeID/ns/pod）。
type podKeyValue = PodStatus

// podKey 构造完整键：<prefix>/<nodeID>/<ns>/<podName>。
// nodeID/ns/podName 含 '/' → 拒绝（validatePodKeyParts 已拦）。
func (s *EtcdPodStore) podKey(nodeID, namespace, podName string) string {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return fmt.Sprintf("%s/%s/%s/%s", s.prefix, nodeID, namespace, podName)
}

// validatePodKeyParts 校验键段合法性：nodeID/namespace/podName 均不得含 '/'
// 或为空（nodeID/podName 必填；namespace 缺省补 default 后再校验）。
func validatePodKeyParts(nodeID, namespace, podName string) error {
	if nodeID == "" {
		return fmt.Errorf("podstatus: nodeID 为空")
	}
	if podName == "" {
		return fmt.Errorf("podstatus: podName 为空")
	}
	if namespace == "" {
		namespace = DefaultNamespace
	}
	for _, part := range []struct{ name, v string }{
		{"nodeID", nodeID}, {"namespace", namespace}, {"podName", podName},
	} {
		if strings.Contains(part.v, "/") {
			return fmt.Errorf("podstatus: %s=%q 含 '/'，拒绝写入（键路径分隔符冲突）", part.name, part.v)
		}
	}
	return nil
}

// Upsert 写穿：先 etcd Put（5s 超时），成功后再更新内存缓存；
// etcd 失败返回 error、内存不动（对齐 registry 写穿语义）。
func (s *EtcdPodStore) Upsert(nodeID string, ps PodStatus) error {
	if nodeID == "" || ps.PodName == "" {
		return nil // 与内存版一致：无法定位存储键，忽略
	}
	ns := ps.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	if err := validatePodKeyParts(nodeID, ns, ps.PodName); err != nil {
		return err
	}
	payload, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("podstatus: Upsert 编码失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = s.kv.Put(ctx, s.podKey(nodeID, ns, ps.PodName), payload)
	cancel()
	if err != nil {
		return fmt.Errorf("podstatus: Upsert 写穿 etcd 失败（node=%s pod=%s，内存不动）: %w", nodeID, ps.PodName, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mem.Upsert(nodeID, ps)
}

// Delete 写穿：先 etcd Delete，成功后再删内存缓存；返回是否存在且被删除。
func (s *EtcdPodStore) Delete(nodeID, namespace, podName string) bool {
	if nodeID == "" || podName == "" {
		return false
	}
	if err := validatePodKeyParts(nodeID, namespace, podName); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := s.kv.Delete(ctx, s.podKey(nodeID, namespace, podName))
	cancel()
	if err != nil {
		log.Warnf("[podstatus] Delete 写穿 etcd 失败（node=%s pod=%s，内存不动，下轮上报收敛）: %v", nodeID, podName, err)
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mem.Delete(nodeID, namespace, podName)
}

// Get 查询单条 Pod 状态（读路径走内存缓存）。
func (s *EtcdPodStore) Get(nodeID, namespace, podName string) (PodStatus, bool) {
	return s.mem.Get(nodeID, namespace, podName)
}

// ListByNode 返回指定节点的全部 Pod 状态（读路径走内存缓存）。
func (s *EtcdPodStore) ListByNode(nodeID string) []PodStatus {
	return s.mem.ListByNode(nodeID)
}

// ListAll 返回全部 Pod 状态（读路径走内存缓存）。
func (s *EtcdPodStore) ListAll() []PodStatus {
	return s.mem.ListAll()
}

// Count 返回记录总数（读路径走内存缓存）。
func (s *EtcdPodStore) Count() int {
	return s.mem.Count()
}

// Load 全量加载持久化后端并灌入内存缓存（embed/纯内存路径；纯内存 = 空操作）。
// 键约束：非法键跳过 + 告警，不阻断加载（对齐 EtcdDeviceStore.applyLoad）。
func (s *EtcdPodStore) Load(ctx context.Context) error {
	entries, err := s.kv.ListByPrefix(ctx, s.prefix+"/")
	if err != nil {
		return fmt.Errorf("podstatus: Load 前缀扫描失败: %w", err)
	}
	s.applyLoad(entries)
	return nil
}

// LoadAnchored 锚定加载（外部模式）：全量加载 + 返回响应头 revision 作为
// watch 锚点。内存重建语义与 Load 完全一致。
func (s *EtcdPodStore) LoadAnchored(ctx context.Context) (int64, error) {
	if s.ext == nil {
		return 0, fmt.Errorf("podstatus: LoadAnchored 需要 WatchKV（外部模式装配使用）")
	}
	entries, rev, err := s.ext.ListByPrefixRev(ctx, s.prefix+"/")
	if err != nil {
		return 0, fmt.Errorf("podstatus: LoadAnchored 前缀扫描失败: %w", err)
	}
	s.applyLoad(entries)
	s.watchRev.Store(rev)
	return rev, nil
}

// applyLoad 用全量扫描结果重建内存缓存（排序确定性；坏键跳过 + Warn）。
func (s *EtcdPodStore) applyLoad(entries []etcdstore.KVEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	loaded := make(map[string]map[podKey]PodStatus)
	for _, e := range entries {
		var ps PodStatus
		if err := json.Unmarshal(e.Value, &ps); err != nil {
			log.Warnf("[podstatus] Load 跳过坏值（key=%s）: %v", e.Key, err)
			continue
		}
		rest := strings.TrimPrefix(e.Key, s.prefix+"/")
		parts := strings.Split(rest, "/")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			log.Warnf("[podstatus] Load 跳过非法键（key=%s）", e.Key)
			continue
		}
		nodeID, ns, podName := parts[0], parts[1], parts[2]
		ps.NodeID, ps.PodName, ps.Namespace = nodeID, podName, ns
		if _, ok := loaded[nodeID]; !ok {
			loaded[nodeID] = make(map[podKey]PodStatus)
		}
		loaded[nodeID][podKeyOf(ns, podName)] = ps
	}
	s.mu.Lock()
	s.mem = &PodStatusStore{pods: loaded}
	s.lastReload = time.Now()
	s.mu.Unlock()
}

// StartWatch 启动 watch 应用器（外部模式多副本读一致；应用器只写内存、
// 永不发 etcd 写——防回写环；断线/ErrCompacted → 全量重放，降频 ≥30s）。
// 内存/embed = no-op。
func (s *EtcdPodStore) StartWatch(ctx context.Context) {
	if s.ext == nil {
		return // 非扩展 KV：无 watch 能力，退化路径
	}
	go s.watchLoop(ctx)
}

// watchLoop 锚定增量应用主循环（对齐 EtcdDeviceStore.watchLoop）：watch
// 断开/Err（含 ErrCompacted）→ 全量重放（降频 ≥30s）→ 重锚定 → 重启 watch。
func (s *EtcdPodStore) watchLoop(ctx context.Context) {
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
				s.watchRev.Store(ev.ModRevision)
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

// reloadAll 全量重放（watch 断线/ErrCompacted；降频 ≥30s）。
func (s *EtcdPodStore) reloadAll(ctx context.Context) {
	s.mu.Lock()
	recent := time.Since(s.lastReload) < 30*time.Second
	s.mu.Unlock()
	if recent {
		return
	}
	if s.ext == nil {
		return
	}
	entries, rev, err := s.ext.ListByPrefixRev(ctx, s.prefix+"/")
	if err != nil {
		log.Warnf("[podstatus] watch 断线全量重放失败: %v", err)
		return
	}
	s.applyLoad(entries)
	s.watchRev.Store(rev)
}

// applyPut 应用一条 watch 增量写（只写内存，不回写 etcd）。
func (s *EtcdPodStore) applyPut(key string, value []byte) {
	rest := strings.TrimPrefix(key, s.prefix+"/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		log.Warnf("[podstatus] watch 跳过非法键（key=%s）", key)
		return
	}
	var ps PodStatus
	if err := json.Unmarshal(value, &ps); err != nil {
		log.Warnf("[podstatus] watch 跳过坏值（key=%s）: %v", key, err)
		return
	}
	ps.NodeID, ps.PodName, ps.Namespace = parts[0], parts[2], parts[1]
	s.mu.Lock()
	_ = s.mem.Upsert(parts[0], ps)
	s.mu.Unlock()
}

// applyDelete 应用一条 watch 增量删（只写内存，不回写 etcd）。
func (s *EtcdPodStore) applyDelete(key string) {
	rest := strings.TrimPrefix(key, s.prefix+"/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		log.Warnf("[podstatus] watch 跳过非法键（key=%s）", key)
		return
	}
	s.mu.Lock()
	s.mem.Delete(parts[0], parts[1], parts[2])
	s.mu.Unlock()
}

// Close 关闭存储（无可关闭资源；接口契约占位，供集成层统一关停链调用）。
func (s *EtcdPodStore) Close() error { return nil }
