package modelrepo

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

// EtcdModelStore 是模型仓库的 etcd 写穿实现（设计 §3，embed/外部模式共用
// 同一实现——embed 的 kvStore 同样实现 AtomicKV，单副本无并发 = CAS 恒成功，
// 行为等价，D4 口径）。
//
// 键空间（设计 §3.1，前缀 /edgeflow/models/ 与 registry/devicestatus 完全隔离）：
//
//	/edgeflow/models/meta/<modelName>                    # Model JSON（写穿/CAS）
//	/edgeflow/models/versions/<modelName>/<version>      # ModelVersion JSON（写穿/CAS）
//	/edgeflow/models/guards/<modelName>                  # 在途发布守卫（create-if-absent；终态删除）
//	/edgeflow/models/releases/<releaseID>                # release 头（状态机 CAS 键）
//	/edgeflow/models/releases/<releaseID>/nodes/<nodeID> # 逐节点结果（独立键，CAS）
//	/edgeflow/models/releases/<releaseID>/lock           # 领跑锁（租约键，控制器经 LockKV 消费）
//	/edgeflow/models/deployments/<modelName>/<nodeID>    # 部署影子（普通 Put 整值覆盖）
//
// 并发策略（设计 §3.3）：全部业务写 = AtomicKV CAS（create-if-absent /
// modRevision），冲突重试 ≤3，耗尽 → ErrConcurrentConflict；
// 写穿顺序 = 先写 etcd 成功、再更新内存缓存，失败内存不动。
//
// 已知限制登记（设计 §13.1，KNOWN-ISSUES L21-L29 代码侧口径）：
//   - N-1/L25 族：终态 release 头与 perNode 键**永久保留作审计痕迹**
//     （主线裁决 D9：不随模型删除级联，etcdctl 可手动清理；长期运行累积，
//     与 L28 无分页同族）；
//   - L25：模型删除为非事务级联（versions+deployments+meta 顺序删除），
//     中途崩溃留孤儿键——启动加载（Load/LoadAnchored）只认 meta 存在的前缀，
//     孤儿键不可见不加载；
//   - L22：纯内存模式重启丢失（本实现不适用；MemoryModelStore 明示）。
//   - watch 应用器只写内存、永不发 etcd 写（防回写环，S3 零写断言）。
type EtcdModelStore struct {
	kv     etcdstore.KVStore  // 持久化事实源（普通读写面）
	atomic etcdstore.AtomicKV // CAS 面（构造时断言；embed/外部均满足）
	ext    etcdstore.WatchKV  // watch 面（外部模式；nil → StartWatch no-op）

	mu         sync.RWMutex
	models     map[string]*Model
	version    map[string]map[string]*ModelVersion
	release    map[string]*ModelRelease
	nodes      map[string]map[string]*NodeReleaseResult
	deploys    map[string]map[string]DeploymentState
	watchRev   atomic.Int64
	lastReload time.Time

	closeCh chan struct{}
	once    sync.Once
	now     func() time.Time
}

// 编译期断言：etcd 写穿实现满足 ModelStore 接口。
var _ ModelStore = (*EtcdModelStore)(nil)

// NewEtcdModelStore 创建模型仓库 etcd 写穿包装。kv 必须满足 AtomicKV
// （embed kvStore 与外部 clientv3 均满足；不满足 → fail-fast，对齐
// NewEtcdRegistry 的 nil 校验风格）。watch 面可选（外部模式装配使用）。
func NewEtcdModelStore(kv etcdstore.KVStore) (*EtcdModelStore, error) {
	if kv == nil {
		return nil, fmt.Errorf("modelrepo: NewEtcdModelStore 需要非 nil 的 KVStore")
	}
	ak, ok := kv.(etcdstore.AtomicKV)
	if !ok {
		return nil, fmt.Errorf("modelrepo: NewEtcdModelStore 需要 AtomicKV（当前 KV 不满足 CAS 扩展面）")
	}
	wk, _ := kv.(etcdstore.WatchKV)
	return &EtcdModelStore{
		kv:      kv,
		atomic:  ak,
		ext:     wk,
		models:  make(map[string]*Model),
		version: make(map[string]map[string]*ModelVersion),
		release: make(map[string]*ModelRelease),
		nodes:   make(map[string]map[string]*NodeReleaseResult),
		deploys: make(map[string]map[string]DeploymentState),
		closeCh: make(chan struct{}),
		now:     time.Now,
	}, nil
}

func (s *EtcdModelStore) nowMs() int64 { return s.now().UnixMilli() }

// ── 键构造（设计 §3.1；调用方须先经校验函数，模型名/版本禁 '/'）────────

func modelKey(name string) string { return KeyMetaPrefix + name }
func versionKey(model, version string) string {
	return KeyVersionsPrefix + model + "/" + version
}
func guardKey(model string) string { return KeyGuardsPrefix + model }
func releaseKey(id string) string  { return KeyReleasesPrefix + id }
func releaseNodeKey(id, nodeID string) string {
	return KeyReleasesPrefix + id + "/nodes/" + nodeID
}
func deploymentKey(model, nodeID string) string {
	return KeyDeploymentsPrefix + model + "/" + nodeID
}

// ── 写穿路径（etcd 先写成功，再更新内存；失败内存不动）─────────────────

// CreateModel 实现 ModelStore（create-if-absent CAS）。
func (s *EtcdModelStore) CreateModel(ctx context.Context, m *Model) error {
	if m == nil {
		return fmt.Errorf("modelrepo: nil model")
	}
	cp := copyModel(m)
	now := s.nowMs()
	cp.CreatedAt = now
	cp.UpdatedAt = now
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("modelrepo: 序列化模型失败: %w", err)
	}
	key := modelKey(m.Name)
	ok, err := s.atomic.CompareAndPut(ctx, key, data, 0)
	if err != nil {
		return fmt.Errorf("modelrepo: CreateModel CAS %s 失败: %w", key, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrModelExists, m.Name)
	}
	s.mu.Lock()
	s.models[m.Name] = copyModel(cp)
	s.mu.Unlock()
	return nil
}

func (s *EtcdModelStore) GetModel(_ context.Context, name string) (*Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.models[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, name)
	}
	return copyModel(m), nil
}

func (s *EtcdModelStore) ListModels(_ context.Context) ([]Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Model, 0, len(s.models))
	for _, m := range s.models {
		out = append(out, *copyModel(m))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UpdateModel 实现 ModelStore（CAS 读-改-写，冲突重试 ≤3，耗尽 409）。
func (s *EtcdModelStore) UpdateModel(ctx context.Context, name string, patch func(*Model) error) error {
	key := modelKey(name)
	for attempt := 0; attempt < 3; attempt++ {
		cur, rev, err := s.atomic.GetWithRev(ctx, key)
		if err != nil {
			return fmt.Errorf("modelrepo: UpdateModel 读取基准失败: %w", err)
		}
		if rev == 0 {
			return fmt.Errorf("%w: %s", ErrModelNotFound, name)
		}
		var m Model
		if err := json.Unmarshal(cur, &m); err != nil {
			return fmt.Errorf("modelrepo: UpdateModel 反序列化失败: %w", err)
		}
		cp := copyModel(&m)
		if err := patch(cp); err != nil {
			return err
		}
		cp.UpdatedAt = s.nowMs()
		data, err := json.Marshal(cp)
		if err != nil {
			return fmt.Errorf("modelrepo: UpdateModel 序列化失败: %w", err)
		}
		ok, err := s.atomic.CompareAndPut(ctx, key, data, rev)
		if err != nil {
			return fmt.Errorf("modelrepo: UpdateModel CAS 失败: %w", err)
		}
		if ok {
			s.mu.Lock()
			s.models[name] = copyModel(cp)
			s.mu.Unlock()
			return nil
		}
		// 冲突：他写者已提交 → 读最新基准再合并重试（≤3）
	}
	return fmt.Errorf("%w: update model %s", ErrConcurrentConflict, name)
}

// DeleteModel 实现 ModelStore（前置校验 + 非事务级联，L25 登记见文件头）。
func (s *EtcdModelStore) DeleteModel(ctx context.Context, name string) error {
	if _, err := s.kv.Get(ctx, modelKey(name)); err != nil {
		return fmt.Errorf("modelrepo: DeleteModel 读取失败: %w", err)
	}
	s.mu.RLock()
	_, metaOK := s.models[name]
	for _, v := range s.version[name] {
		if v.Status == VersionStatusActive {
			s.mu.RUnlock()
			return fmt.Errorf("%w: %s", ErrModelHasActiveVersion, name)
		}
	}
	var inFlight string
	for id, r := range s.release {
		if r.Model == name && r.Status.InFlight() {
			inFlight = id
			break
		}
	}
	s.mu.RUnlock()
	if !metaOK {
		return fmt.Errorf("%w: %s", ErrModelNotFound, name)
	}
	if inFlight != "" {
		return &ReleaseConflictError{InFlight: inFlight}
	}
	if err := s.kv.Delete(ctx, guardKey(name)); err != nil {
		log.Warnf("[modelrepo] DeleteModel 清 guard 失败（best-effort，孤儿 guard 由 CreateRelease 自愈 D3）: %v", err)
	}
	if err := s.kv.DeleteRange(ctx, KeyVersionsPrefix+name+"/"); err != nil {
		return fmt.Errorf("modelrepo: DeleteModel 级联删版本失败: %w", err)
	}
	if err := s.kv.DeleteRange(ctx, KeyDeploymentsPrefix+name+"/"); err != nil {
		return fmt.Errorf("modelrepo: DeleteModel 级联删部署影子失败: %w", err)
	}
	if err := s.kv.Delete(ctx, modelKey(name)); err != nil {
		return fmt.Errorf("modelrepo: DeleteModel 删 meta 失败: %w", err)
	}
	s.mu.Lock()
	delete(s.version, name)
	delete(s.deploys, name)
	delete(s.models, name)
	s.mu.Unlock()
	return nil
}

// CreateVersion 实现 ModelStore（create-if-absent CAS；初始 draft）。
func (s *EtcdModelStore) CreateVersion(ctx context.Context, v *ModelVersion) error {
	if v == nil {
		return fmt.Errorf("modelrepo: nil version")
	}
	if _, err := s.GetModel(ctx, v.Model); err != nil {
		return err
	}
	cp := copyVersion(v)
	cp.Status = VersionStatusDraft
	now := s.nowMs()
	cp.CreatedAt = now
	cp.UpdatedAt = now
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("modelrepo: 序列化版本失败: %w", err)
	}
	key := versionKey(v.Model, v.Version)
	ok, err := s.atomic.CompareAndPut(ctx, key, data, 0)
	if err != nil {
		return fmt.Errorf("modelrepo: CreateVersion CAS %s 失败: %w", key, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s/%s", ErrVersionExists, v.Model, v.Version)
	}
	s.mu.Lock()
	if s.version[v.Model] == nil {
		s.version[v.Model] = make(map[string]*ModelVersion)
	}
	s.version[v.Model][v.Version] = copyVersion(cp)
	s.mu.Unlock()
	return nil
}

func (s *EtcdModelStore) GetVersion(_ context.Context, model, version string) (*ModelVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.models[model]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	v, ok := s.version[model][version]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrVersionNotFound, model, version)
	}
	return copyVersion(v), nil
}

func (s *EtcdModelStore) ListVersions(_ context.Context, model string) ([]ModelVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.models[model]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	out := make([]ModelVersion, 0, len(s.version[model]))
	for _, v := range s.version[model] {
		out = append(out, *copyVersion(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// DeleteVersion 实现 ModelStore（CompareAndDelete rev 守卫，冲突重试 ≤3）。
func (s *EtcdModelStore) DeleteVersion(ctx context.Context, model, version string) error {
	key := versionKey(model, version)
	for attempt := 0; attempt < 3; attempt++ {
		cur, rev, err := s.atomic.GetWithRev(ctx, key)
		if err != nil {
			return fmt.Errorf("modelrepo: DeleteVersion 读取失败: %w", err)
		}
		if rev == 0 {
			return fmt.Errorf("%w: %s/%s", ErrVersionNotFound, model, version)
		}
		var v ModelVersion
		if err := json.Unmarshal(cur, &v); err != nil {
			return fmt.Errorf("modelrepo: DeleteVersion 反序列化失败: %w", err)
		}
		if v.Status == VersionStatusActive {
			return fmt.Errorf("%w: %s/%s", ErrVersionActive, model, version)
		}
		deleted, err := s.atomic.CompareAndDelete(ctx, key, rev)
		if err != nil {
			return fmt.Errorf("modelrepo: DeleteVersion CAS 失败: %w", err)
		}
		if deleted {
			s.mu.Lock()
			delete(s.version[model], version)
			if len(s.version[model]) == 0 {
				delete(s.version, model)
			}
			s.mu.Unlock()
			return nil
		}
		// rev 已被改写 → 刷新重试
	}
	return fmt.Errorf("%w: delete version %s/%s", ErrConcurrentConflict, model, version)
}

// ActivateVersion 实现 ModelStore（双键 CAS 序列 + ②失败补偿，设计 §3.3）：
//
//	① CAS 旧 active（active→archived）；② CAS 目标（draft→active）。
//	①失败（rev 冲突）→ 刷新重试（≤3）；②失败 → 补偿：尽力把 ① 的旧 active
//	恢复为 active（archived→active CAS），补偿失败 → 返回 409 + Warn（"无
//	active" 瞬态窗口 <1s，重试 activate 即收敛，设计 §3.3 文档化）。
func (s *EtcdModelStore) ActivateVersion(ctx context.Context, model, version string) error {
	if _, err := s.GetModel(ctx, model); err != nil {
		return err
	}
	targetKey := versionKey(model, version)
	now := s.nowMs()
	for attempt := 0; attempt < 3; attempt++ {
		tCur, tRev, err := s.atomic.GetWithRev(ctx, targetKey)
		if err != nil {
			return fmt.Errorf("modelrepo: ActivateVersion 读取目标失败: %w", err)
		}
		if tRev == 0 {
			return fmt.Errorf("%w: %s/%s", ErrVersionNotFound, model, version)
		}
		var target ModelVersion
		if err := json.Unmarshal(tCur, &target); err != nil {
			return fmt.Errorf("modelrepo: ActivateVersion 反序列化失败: %w", err)
		}
		if target.Status != VersionStatusDraft {
			return fmt.Errorf("%w: %s/%s (status=%s)", ErrVersionNotDraft, model, version, target.Status)
		}
		oldKey, oldRev, oldActive, err := s.findActiveWithRev(ctx, model)
		if err != nil {
			return err
		}
		if oldRev > 0 && oldActive.Version != version {
			// ① 旧 active → archived（CAS 防双 active 瞬态）
			oldArchived := oldActive
			oldArchived.Status = VersionStatusArchived
			oldArchived.UpdatedAt = now
			archData, err := json.Marshal(&oldArchived)
			if err != nil {
				return err
			}
			ok, err := s.atomic.CompareAndPut(ctx, oldKey, archData, oldRev)
			if err != nil {
				return fmt.Errorf("modelrepo: ActivateVersion ①降级旧 active 失败: %w", err)
			}
			if !ok {
				continue // ①冲突：他写者已动 → 刷新重试
			}
		}
		// ② 目标 draft → active（PrevActive 记被降级版本，回滚目标；
		// 无旧 active 时保持原值——首次激活无回滚目标）
		active := target
		active.Status = VersionStatusActive
		active.UpdatedAt = now
		if oldRev > 0 && oldActive.Version != version {
			active.PrevActive = oldActive.Version
		}
		actData, err := json.Marshal(&active)
		if err != nil {
			return err
		}
		ok, err := s.atomic.CompareAndPut(ctx, targetKey, actData, tRev)
		if err != nil {
			return fmt.Errorf("modelrepo: ActivateVersion ②置位失败: %w", err)
		}
		if ok {
			s.mu.Lock()
			if oldRev > 0 && oldActive.Version != version && s.version[model] != nil {
				if old, ok := s.version[model][oldActive.Version]; ok {
					cp := copyVersion(old)
					cp.Status = VersionStatusArchived
					cp.UpdatedAt = now
					s.version[model][oldActive.Version] = cp
				}
			}
			cp := copyVersion(&active)
			if s.version[model] == nil {
				s.version[model] = make(map[string]*ModelVersion)
			}
			s.version[model][version] = cp
			s.mu.Unlock()
			return nil
		}
		// ②失败 → 补偿：尽力恢复 ① 降级的旧 active
		if oldRev > 0 && oldActive.Version != version {
			s.compensateActivate(ctx, oldKey, oldActive, now)
		}
		return fmt.Errorf("%w: activate %s/%s（②置位冲突）", ErrConcurrentConflict, model, version)
	}
	return fmt.Errorf("%w: activate %s/%s", ErrConcurrentConflict, model, version)
}

// findActiveWithRev 从 etcd 前缀扫描定位当前 active 版本及其 ModRevision
// （跨副本权威；内存可能滞后）。无 active → (key, 0, nil)。
func (s *EtcdModelStore) findActiveWithRev(ctx context.Context, model string) (string, int64, *ModelVersion, error) {
	entries, err := s.kv.ListByPrefix(ctx, KeyVersionsPrefix+model+"/")
	if err != nil {
		return "", 0, nil, fmt.Errorf("modelrepo: 扫描版本前缀失败: %w", err)
	}
	for _, ent := range entries {
		var v ModelVersion
		if err := json.Unmarshal(ent.Value, &v); err != nil {
			continue
		}
		if v.Status == VersionStatusActive {
			return ent.Key, ent.Revision, &v, nil
		}
	}
	return "", 0, nil, nil
}

// compensateActivate 补偿 ① 的降级：仅在旧 active 仍是"我们刚降级的那份"
// （archived 且 Version 匹配且 rev 未被重写）时恢复为 active；否则说明
// 他写者已接管（并发 activate/delete），放弃并 Warn（设计 §3.3 瞬态窗口）。
func (s *EtcdModelStore) compensateActivate(ctx context.Context, oldKey string, oldActive *ModelVersion, now int64) {
	cur, rev, err := s.atomic.GetWithRev(ctx, oldKey)
	if err != nil || rev == 0 {
		log.Warnf("[modelrepo] ActivateVersion 补偿失败：旧 active 键不可读（%v）——可能已删除，'无 active' 瞬态窗口由重试 activate 收敛", err)
		return
	}
	var v ModelVersion
	if err := json.Unmarshal(cur, &v); err != nil || v.Status != VersionStatusArchived || v.Version != oldActive.Version {
		log.Warnf("[modelrepo] ActivateVersion 补偿跳过：旧 active %s 已被他写者改动（可能是并发 activate/删除）——保持现状，窗口收敛", oldActive.Version)
		return
	}
	v.Status = VersionStatusActive
	v.UpdatedAt = now
	data, err := json.Marshal(&v)
	if err != nil {
		return
	}
	if ok, err := s.atomic.CompareAndPut(ctx, oldKey, data, rev); err != nil || !ok {
		log.Warnf("[modelrepo] ActivateVersion 补偿 CAS 失败（ok=%v err=%v）——'无 active' 瞬态窗口 <1s，重试 activate 即收敛（设计 §3.3）", ok, err)
	}
}

// ArchiveVersion 实现 ModelStore（单键 CAS；防发布中归档目标见接口注释）。
func (s *EtcdModelStore) ArchiveVersion(ctx context.Context, model, version string) error {
	if _, err := s.GetModel(ctx, model); err != nil {
		return err
	}
	key := versionKey(model, version)
	for attempt := 0; attempt < 3; attempt++ {
		cur, rev, err := s.atomic.GetWithRev(ctx, key)
		if err != nil {
			return fmt.Errorf("modelrepo: ArchiveVersion 读取失败: %w", err)
		}
		if rev == 0 {
			return fmt.Errorf("%w: %s/%s", ErrVersionNotFound, model, version)
		}
		var v ModelVersion
		if err := json.Unmarshal(cur, &v); err != nil {
			return fmt.Errorf("modelrepo: ArchiveVersion 反序列化失败: %w", err)
		}
		if v.Status != VersionStatusActive {
			return fmt.Errorf("%w: %s/%s (status=%s)", ErrVersionNotActive, model, version, v.Status)
		}
		// 防发布中归档目标（设计 §2.4）：存在指向该版本的 pending/running 发布 → 409
		if id := s.inFlightForVersion(model, version); id != "" {
			return &ReleaseConflictError{InFlight: id}
		}
		v.Status = VersionStatusArchived
		v.UpdatedAt = s.nowMs()
		data, err := json.Marshal(&v)
		if err != nil {
			return err
		}
		ok, err := s.atomic.CompareAndPut(ctx, key, data, rev)
		if err != nil {
			return fmt.Errorf("modelrepo: ArchiveVersion CAS 失败: %w", err)
		}
		if ok {
			s.mu.Lock()
			if old, ok := s.version[model][version]; ok {
				cp := copyVersion(old)
				cp.Status = VersionStatusArchived
				cp.UpdatedAt = v.UpdatedAt
				s.version[model][version] = cp
			}
			s.mu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("%w: archive %s/%s", ErrConcurrentConflict, model, version)
}

// inFlightForVersion 返回指向该版本的 in-flight 发布 ID（内存扫描；无则 ""）。
func (s *EtcdModelStore) inFlightForVersion(model, version string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, r := range s.release {
		if r.Model == model && r.Version == version && r.Status.InFlight() {
			return id
		}
	}
	return ""
}

func (s *EtcdModelStore) ActiveVersion(_ context.Context, model string) (*ModelVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.models[model]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	for _, v := range s.version[model] {
		if v.Status == VersionStatusActive {
			return copyVersion(v), nil
		}
	}
	return nil, nil
}

// ── 发布（guard 自愈 D3 + meta 复查 D7 + 头键 CAS）─────────────────────

// CreateRelease 实现 ModelStore（设计 §3.2/§3.3 + 主线裁决 D3/D7）：
//
//  1. guard CAS（create-if-absent）；冲突 → 孤儿 guard 自愈（D3）：
//     读 guard 值 → 读 release 键 → 不存在（release 键从未写入 = 写后崩溃
//     的孤儿；或已终态 = 防御性清理）→ 按值 CompareAndDelete（防误删
//     新 guard）→ 重试一次；仍冲突 → *ReleaseConflictError 带在途 ID；
//     （废弃 "lock 过期" 陈旧判据：孤儿场景 lock 键从未创建，判据不自洽——
//     审稿 F-3 修正；控制器不承担兜底，看不见孤儿 guard）
//  2. 模型 meta 复查（D7）：guard CAS 成功后复查模型键存在（封堵"模型
//     删除×发布创建"竞态）；缺失 → 删 guard（best-effort）+ ErrModelNotFound；
//  3. release 头键 CAS（create-if-absent；UUID 碰撞理论不可达）；
//  4. 内存更新。逐节点 pending 预写由调用方经 SetNodeResult 完成。
func (s *EtcdModelStore) CreateRelease(ctx context.Context, r *ModelRelease) error {
	if r == nil {
		return fmt.Errorf("modelrepo: nil release")
	}
	if r.ID == "" {
		return fmt.Errorf("modelrepo: release id is required")
	}
	cp := copyRelease(r)
	cp.Status = ReleaseStatusPending
	if cp.CreatedAt == 0 {
		cp.CreatedAt = s.nowMs()
	}
	head, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("modelrepo: 序列化 release 失败: %w", err)
	}

	gk := guardKey(r.Model)
	ok, err := s.atomic.CompareAndPut(ctx, gk, []byte(r.ID), 0)
	if err != nil {
		return fmt.Errorf("modelrepo: CreateRelease guard CAS 失败: %w", err)
	}
	if !ok {
		// D3 自愈：guard 冲突 → 判定陈旧 → 按值删 guard → 重试一次
		stale, inFlight, err := s.guardIsStale(ctx, r.Model)
		if err != nil {
			return err
		}
		if !stale {
			return &ReleaseConflictError{InFlight: inFlight}
		}
		if err := s.deleteGuardByValue(ctx, r.Model); err != nil {
			return err
		}
		ok, err = s.atomic.CompareAndPut(ctx, gk, []byte(r.ID), 0)
		if err != nil {
			return fmt.Errorf("modelrepo: CreateRelease guard CAS 重试失败: %w", err)
		}
		if !ok {
			_, inFlight, err := s.guardIsStale(ctx, r.Model)
			if err != nil {
				return err
			}
			return &ReleaseConflictError{InFlight: inFlight}
		}
	}
	// D7：guard 已持有 → 复查模型 meta（防与 DeleteModel 竞态）。
	// 注意：KVStore.Get 对缺失键返回 (nil, nil)——存在性必须显式判定，
	// 只判 err 会把"模型已删"当成"存在"（单测 TestEtcdCreateRelease_D7模型meta复查
	// 锁定；实施登记：修复前实现只捕错误不捕缺失，D7 语义未真正生效）。
	mv, err := s.kv.Get(ctx, modelKey(r.Model))
	if err != nil {
		_ = s.deleteGuardByValue(ctx, r.Model)
		return fmt.Errorf("modelrepo: CreateRelease 复查模型失败: %w", err)
	}
	if mv == nil {
		_ = s.deleteGuardByValue(ctx, r.Model)
		return fmt.Errorf("%w: %s", ErrModelNotFound, r.Model)
	}
	ok, err = s.atomic.CompareAndPut(ctx, releaseKey(r.ID), head, 0)
	if err != nil {
		_ = s.deleteGuardByValue(ctx, r.Model)
		return fmt.Errorf("modelrepo: CreateRelease 头键写失败: %w", err)
	}
	if !ok {
		_ = s.deleteGuardByValue(ctx, r.Model)
		return fmt.Errorf("%w: duplicate release id %s（理论不可达）", ErrConcurrentConflict, r.ID)
	}
	s.mu.Lock()
	s.release[r.ID] = copyRelease(cp)
	s.mu.Unlock()
	return nil
}

// guardIsStale 判定 guard 是否陈旧（D3）：guard 指向的 release 键不存在
// （孤儿：guard 写后、release 键写前崩溃）或已终态（防御：终态后 guard
// 未清）。陈旧 → (true, "", nil)；有效在途 → (false, inFlightID, nil)。
func (s *EtcdModelStore) guardIsStale(ctx context.Context, model string) (bool, string, error) {
	gv, err := s.kv.Get(ctx, guardKey(model))
	if err != nil {
		return false, "", fmt.Errorf("modelrepo: 自愈读取 guard 失败: %w", err)
	}
	if gv == nil {
		return true, "", nil // guard 刚被释放（终态清理）→ 视为可重试
	}
	gid := string(gv)
	rv, err := s.kv.Get(ctx, releaseKey(gid))
	if err != nil {
		return false, "", fmt.Errorf("modelrepo: 自愈读取 release 失败: %w", err)
	}
	if rv == nil {
		return true, "", nil // 孤儿：release 键从未写入
	}
	var hdr ModelRelease
	if err := json.Unmarshal(rv, &hdr); err == nil && hdr.Status.IsTerminal() {
		return true, "", nil // 终态残留 guard（防御性清理）
	}
	return false, gid, nil
}

// deleteGuardByValue 按值 CAS 删除 guard（防误删新 guard，D3）：读当前 rev →
// CompareAndDelete；失败（rev 被重写 = 新 guard 已建立）→ 不动，返回 nil
// （调用方下一轮 CAS 自然冲突）。删除失败 → error（调用方记日志）。
func (s *EtcdModelStore) deleteGuardByValue(ctx context.Context, model string) error {
	_, rev, err := s.atomic.GetWithRev(ctx, guardKey(model))
	if err != nil {
		return fmt.Errorf("modelrepo: 删除 guard 读取失败: %w", err)
	}
	if rev == 0 {
		return nil
	}
	deleted, err := s.atomic.CompareAndDelete(ctx, guardKey(model), rev)
	if err != nil {
		return fmt.Errorf("modelrepo: 删除 guard CAS 失败: %w", err)
	}
	if !deleted {
		log.Warnf("[modelrepo] guard %s 已被重写（新发布建立），放弃自愈删除", guardKey(model))
	}
	return nil
}

func (s *EtcdModelStore) GetRelease(_ context.Context, id string) (*ModelRelease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.release[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrReleaseNotFound, id)
	}
	return copyRelease(r), nil
}

func (s *EtcdModelStore) ListReleases(_ context.Context, model string) ([]ModelRelease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ModelRelease, 0, len(s.release))
	for _, r := range s.release {
		if model != "" && r.Model != model {
			continue
		}
		out = append(out, *copyRelease(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// UpdateReleaseHead 实现 ModelStore（CAS 读-改-写，冲突重试 ≤3，耗尽 409）。
func (s *EtcdModelStore) UpdateReleaseHead(ctx context.Context, id string, mutate func(*ModelRelease) error) error {
	key := releaseKey(id)
	for attempt := 0; attempt < 3; attempt++ {
		cur, rev, err := s.atomic.GetWithRev(ctx, key)
		if err != nil {
			return fmt.Errorf("modelrepo: UpdateReleaseHead 读取失败: %w", err)
		}
		if rev == 0 {
			return fmt.Errorf("%w: %s", ErrReleaseNotFound, id)
		}
		var r ModelRelease
		if err := json.Unmarshal(cur, &r); err != nil {
			return fmt.Errorf("modelrepo: UpdateReleaseHead 反序列化失败: %w", err)
		}
		cp := copyRelease(&r)
		if err := mutate(cp); err != nil {
			return err
		}
		data, err := json.Marshal(cp)
		if err != nil {
			return err
		}
		ok, err := s.atomic.CompareAndPut(ctx, key, data, rev)
		if err != nil {
			return fmt.Errorf("modelrepo: UpdateReleaseHead CAS 失败: %w", err)
		}
		if ok {
			s.mu.Lock()
			s.release[id] = copyRelease(cp)
			s.mu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("%w: update release head %s", ErrConcurrentConflict, id)
}

func (s *EtcdModelStore) CancelRelease(ctx context.Context, id string) error {
	return s.UpdateReleaseHead(ctx, id, func(r *ModelRelease) error {
		switch r.Status {
		case ReleaseStatusPending, ReleaseStatusRunning:
		default:
			return fmt.Errorf("%w: release already %s", ErrReleaseTerminal, r.Status)
		}
		r.Status = ReleaseStatusCanceled
		r.FinishedAt = s.nowMs()
		return nil
	})
}

// RequestRollback 实现 ModelStore（设计 §5.5 前置守卫全量；见接口注释）。
func (s *EtcdModelStore) RequestRollback(ctx context.Context, id string) error {
	return s.UpdateReleaseHead(ctx, id, func(r *ModelRelease) error {
		switch r.Status {
		case ReleaseStatusRunning, ReleaseStatusSucceeded, ReleaseStatusFailed, ReleaseStatusCanceled:
		case ReleaseStatusPending:
			return fmt.Errorf("%w: release pending (not yet started)", ErrReleaseTerminal)
		case ReleaseStatusRolledBack:
			return fmt.Errorf("%w: release already rolled back", ErrReleaseTerminal)
		}
		if r.RollbackRequested {
			return nil // 幂等：已置位，重复请求结果一致（设计 §8.3）
		}
		if r.PrevActive == "" {
			return fmt.Errorf("%w: release %s has no previous active version", ErrNoPrevActive, id)
		}
		prev, err := s.GetVersion(ctx, r.Model, r.PrevActive)
		if err != nil || prev == nil {
			return fmt.Errorf("%w: previous active version %s/%s missing", ErrNoPrevActive, r.Model, r.PrevActive)
		}
		cur, err := s.ActiveVersion(ctx, r.Model)
		if err != nil {
			return err
		}
		if cur == nil || cur.Version != r.Version {
			active := "none"
			if cur != nil {
				active = cur.Version
			}
			return fmt.Errorf("%w: release version %s, current active %s（先显式 activate 目标旧版本或发起新发布，设计 L26）", ErrVersionMismatch, r.Version, active)
		}
		r.RollbackRequested = true
		return nil
	})
}

// SetNodeResult 实现 ModelStore（首写 create-if-absent + CAS 更新 ≤3）。
func (s *EtcdModelStore) SetNodeResult(ctx context.Context, releaseID, nodeID string, nr *NodeReleaseResult) error {
	if nr == nil {
		return fmt.Errorf("modelrepo: nil node result")
	}
	if _, err := s.GetRelease(ctx, releaseID); err != nil {
		return err
	}
	key := releaseNodeKey(releaseID, nodeID)
	now := s.nowMs()
	for attempt := 0; attempt < 3; attempt++ {
		cur, rev, err := s.atomic.GetWithRev(ctx, key)
		if err != nil {
			return fmt.Errorf("modelrepo: SetNodeResult 读取失败: %w", err)
		}
		if rev == 0 {
			// 首写：create-if-absent
			cp := *nr
			if cp.Batch < 1 {
				cp.Batch = 1
			}
			cp.StartedAt = now
			if cp.Status != NodeRelPending {
				cp.FinishedAt = now
			}
			data, err := json.Marshal(&cp)
			if err != nil {
				return err
			}
			ok, err := s.atomic.CompareAndPut(ctx, key, data, 0)
			if err != nil {
				return fmt.Errorf("modelrepo: SetNodeResult 首写失败: %w", err)
			}
			if ok {
				s.upsertNodeResult(releaseID, nodeID, &cp)
				return nil
			}
			continue // 他写者刚建键 → 走更新分支
		}
		var old NodeReleaseResult
		if err := json.Unmarshal(cur, &old); err != nil {
			return fmt.Errorf("modelrepo: SetNodeResult 反序列化失败: %w", err)
		}
		if !CanTransitionNodeResult(old.Status, nr.Status) {
			return fmt.Errorf("modelrepo: illegal node result transition %s → %s (node %s)",
				old.Status, nr.Status, nodeID)
		}
		cp := *nr
		cp.Batch = old.Batch // 批序号创建时预分配，不可变
		cp.StartedAt = old.StartedAt
		if cp.Status != NodeRelPending {
			cp.FinishedAt = now
		}
		data, err := json.Marshal(&cp)
		if err != nil {
			return err
		}
		ok, err := s.atomic.CompareAndPut(ctx, key, data, rev)
		if err != nil {
			return fmt.Errorf("modelrepo: SetNodeResult CAS 失败: %w", err)
		}
		if ok {
			s.upsertNodeResult(releaseID, nodeID, &cp)
			return nil
		}
	}
	return fmt.Errorf("%w: set node result %s/%s", ErrConcurrentConflict, releaseID, nodeID)
}

func (s *EtcdModelStore) upsertNodeResult(releaseID, nodeID string, r *NodeReleaseResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodes[releaseID] == nil {
		s.nodes[releaseID] = make(map[string]*NodeReleaseResult)
	}
	cp := *r
	s.nodes[releaseID][nodeID] = &cp
}

func (s *EtcdModelStore) GetNodeResult(_ context.Context, releaseID, nodeID string) (*NodeReleaseResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.release[releaseID]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrReleaseNotFound, releaseID)
	}
	r, ok := s.nodes[releaseID][nodeID]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (s *EtcdModelStore) ListNodeResults(_ context.Context, releaseID string) ([]NodeReleaseResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.release[releaseID]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrReleaseNotFound, releaseID)
	}
	out := make([]NodeReleaseResult, 0, len(s.nodes[releaseID]))
	for _, r := range s.nodes[releaseID] {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// SetDeployment 实现 ModelStore（普通 Put 整值覆盖——派生台账非权威指令，
// 无 CAS 需求；v0.6.0 审稿线索 3 裁定，与 Desired 的差异为有意设计）。
// 前置校验模型存在（与内存实现语义一致；模型删除与回滚发布的竞态由
// D2 执行期复查封堵）。
func (s *EtcdModelStore) SetDeployment(ctx context.Context, model, nodeID string, d DeploymentState) error {
	if _, err := s.GetModel(ctx, model); err != nil {
		return err
	}
	d.Model = model
	d.NodeID = nodeID
	d.UpdatedAt = s.nowMs()
	data, err := json.Marshal(&d)
	if err != nil {
		return err
	}
	key := deploymentKey(model, nodeID)
	if err := s.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("modelrepo: SetDeployment 写穿失败: %w", err)
	}
	s.mu.Lock()
	if s.deploys[model] == nil {
		s.deploys[model] = make(map[string]DeploymentState)
	}
	s.deploys[model][nodeID] = d
	s.mu.Unlock()
	return nil
}

func (s *EtcdModelStore) ListDeployments(_ context.Context, model string) ([]DeploymentState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.models[model]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	out := make([]DeploymentState, 0, len(s.deploys[model]))
	for _, d := range s.deploys[model] {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

func (s *EtcdModelStore) DeleteModelDeployments(ctx context.Context, model string) error {
	if err := s.kv.DeleteRange(ctx, KeyDeploymentsPrefix+model+"/"); err != nil {
		return fmt.Errorf("modelrepo: DeleteModelDeployments 写穿失败: %w", err)
	}
	s.mu.Lock()
	delete(s.deploys, model)
	s.mu.Unlock()
	return nil
}

// ReleaseGuard 实现 ModelStore（幂等删除 guard 键；失败返回 error，控制器
// 下一轮重试，对齐 sweepGC 模式）。内存缓存无守卫概念，无需联动。
func (s *EtcdModelStore) ReleaseGuard(ctx context.Context, model string) error {
	if err := s.kv.Delete(ctx, guardKey(model)); err != nil {
		return fmt.Errorf("modelrepo: ReleaseGuard 删除 %s 失败: %w", guardKey(model), err)
	}
	return nil
}

// ── 加载与 watch（外部模式多副本读一致；设计 §3.5）─────────────────────

// classifyKey 把键分类为 (kind, parts)：kind ∈ {model, version, releaseHead,
// releaseNode, deployment, ignore}；parts 为键段（用于回填校验）。
func classifyKey(key string) (kind string, parts []string) {
	rest, ok := strings.CutPrefix(key, KeyNamePrefix)
	if !ok {
		return "ignore", nil
	}
	switch {
	case strings.HasPrefix(rest, "meta/"):
		return "model", []string{strings.TrimPrefix(rest, "meta/")}
	case strings.HasPrefix(rest, "versions/"):
		parts := strings.Split(strings.TrimPrefix(rest, "versions/"), "/")
		if len(parts) != 2 {
			return "ignore", nil
		}
		return "version", parts
	case strings.HasPrefix(rest, "releases/"):
		parts := strings.Split(strings.TrimPrefix(rest, "releases/"), "/")
		switch len(parts) {
		case 1:
			return "releaseHead", parts
		case 3:
			if parts[1] == "nodes" {
				return "releaseNode", []string{parts[0], parts[2]}
			}
			return "ignore", nil // releases/<id>/lock 等租约键
		}
		return "ignore", nil
	case strings.HasPrefix(rest, "deployments/"):
		parts := strings.Split(strings.TrimPrefix(rest, "deployments/"), "/")
		if len(parts) != 2 {
			return "ignore", nil
		}
		return "deployment", parts
	}
	return "ignore", nil
}

// Load 从 etcd 全量加载并重建内存缓存（embed 路径启动时调用一次；L25 孤儿
// 过滤：只载入 meta 存在前缀下的 models/versions/deployments 与 head 存在的
// releases/nodes；坏键跳过 + 告警不阻断，对齐 EtcdRegistry.Load 惯例）。
func (s *EtcdModelStore) Load(ctx context.Context) error {
	entries, err := s.kv.ListByPrefix(ctx, KeyNamePrefix)
	if err != nil {
		return fmt.Errorf("modelrepo: Load 前缀扫描失败: %w", err)
	}
	s.buildMemory(entries)
	return nil
}

// LoadAnchored 锚定加载（外部模式装配用）：全量重建 + 返回响应头 revision
// 作为 watch 锚点。底层 KV 不支持修订锚定 → error。
func (s *EtcdModelStore) LoadAnchored(ctx context.Context) (int64, error) {
	if s.ext == nil {
		return 0, fmt.Errorf("modelrepo: LoadAnchored 需要 WatchKV（当前 KV 不支持修订锚定）")
	}
	entries, rev, err := s.ext.ListByPrefixRev(ctx, KeyNamePrefix)
	if err != nil {
		return 0, fmt.Errorf("modelrepo: LoadAnchored 前缀扫描失败: %w", err)
	}
	s.buildMemory(entries)
	s.watchRev.Store(rev)
	return rev, nil
}

// buildMemory 整体重建内存缓存（Load/LoadAnchored/reloadAll 共用）。
// L25 孤儿过滤：meta 缺失的模型前缀下版本/部署不加载；head 缺失的发布
// perNode 不加载；按护卫顺序两遍扫描。
func (s *EtcdModelStore) buildMemory(entries []etcdstore.KVEntry) {
	models := make(map[string]*Model)
	version := make(map[string]map[string]*ModelVersion)
	release := make(map[string]*ModelRelease)
	nodes := make(map[string]map[string]*NodeReleaseResult)
	deploys := make(map[string]map[string]DeploymentState)
	skipped := 0

	// 第一遍：模型台账（决定孤儿过滤的"meta 存在"集合）
	for _, ent := range entries {
		kind, parts := classifyKey(ent.Key)
		switch kind {
		case "model":
			var m Model
			if err := json.Unmarshal(ent.Value, &m); err != nil || m.Name != parts[0] {
				skipped++
				log.Warnf("[modelrepo] Load 跳过坏模型键 %s: %v", ent.Key, err)
				continue
			}
			models[m.Name] = copyModel(&m)
		}
	}
	// 第二遍：其余子空间（孤儿过滤）
	for _, ent := range entries {
		kind, parts := classifyKey(ent.Key)
		switch kind {
		case "version":
			if _, ok := models[parts[0]]; !ok {
				skipped++ // L25：孤儿版本键（模型已被级联删除但残留）不可见
				continue
			}
			var v ModelVersion
			if err := json.Unmarshal(ent.Value, &v); err != nil || v.Model != parts[0] || v.Version != parts[1] {
				skipped++
				log.Warnf("[modelrepo] Load 跳过坏版本键 %s: %v", ent.Key, err)
				continue
			}
			if version[parts[0]] == nil {
				version[parts[0]] = make(map[string]*ModelVersion)
			}
			version[parts[0]][parts[1]] = copyVersion(&v)
		case "releaseHead":
			var r ModelRelease
			if err := json.Unmarshal(ent.Value, &r); err != nil || r.ID != parts[0] {
				skipped++
				log.Warnf("[modelrepo] Load 跳过坏发布键 %s: %v", ent.Key, err)
				continue
			}
			if _, ok := models[r.Model]; !ok {
				skipped++ // L25：孤儿发布（模型已删）不可见
				continue
			}
			release[r.ID] = copyRelease(&r)
		case "releaseNode":
			if _, ok := release[parts[0]]; !ok {
				skipped++ // 孤儿 perNode（head 缺失）不可见
				continue
			}
			var nr NodeReleaseResult
			if err := json.Unmarshal(ent.Value, &nr); err != nil || nr.NodeID != parts[1] {
				skipped++
				continue
			}
			if nodes[parts[0]] == nil {
				nodes[parts[0]] = make(map[string]*NodeReleaseResult)
			}
			cp := nr
			nodes[parts[0]][parts[1]] = &cp
		case "deployment":
			if _, ok := models[parts[0]]; !ok {
				skipped++
				continue
			}
			var d DeploymentState
			if err := json.Unmarshal(ent.Value, &d); err != nil || d.Model != parts[0] || d.NodeID != parts[1] {
				skipped++
				continue
			}
			if deploys[parts[0]] == nil {
				deploys[parts[0]] = make(map[string]DeploymentState)
			}
			deploys[parts[0]][parts[1]] = d
		}
	}
	if skipped > 0 {
		log.Warnf("[modelrepo] 加载完成：恢复模型 %d/版本 %d/发布 %d/部署影子 %d，跳过 %d 个坏键/孤儿键",
			len(models), len(version), len(release), len(deploys), skipped)
	}
	s.mu.Lock()
	s.models = models
	s.version = version
	s.release = release
	s.nodes = nodes
	s.deploys = deploys
	s.mu.Unlock()
}

// StartWatch 启动 watch 应用器（外部模式多副本读一致，设计 §3.5）：
// 单 goroutine 单调应用 PUT/DELETE；断线/ErrCompacted → 全量重放（降频
// ≥30s）→ 重锚定。应用器**只写内存、永不发 etcd 写**（防回写环，S3 零写
// 断言）。非 ExtendedKV → no-op + Warn。Close() 停止。
func (s *EtcdModelStore) StartWatch(ctx context.Context) {
	if s.ext == nil {
		log.Warnf("modelrepo: 底层 KV 不支持 watch（非 ExtendedKV），watch 缓存同步停用（no-op）——读一致性降级为启动快照")
		return
	}
	innerCtx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-s.closeCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	go s.watchLoop(innerCtx)
}

// watchLoop 锚定增量应用主循环（对齐 devicestatus.EtcdDeviceStore.watchLoop
// 模式）：watch 断开/Err（含 ErrCompacted）→ 全量重放（降频）→ 重锚定。
func (s *EtcdModelStore) watchLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		rev := s.watchRev.Load()
		ch := s.ext.WatchPrefix(ctx, KeyNamePrefix, etcdstore.StartRevision(rev))
		for ev := range ch {
			switch ev.Type {
			case etcdstore.WatchEventPut:
				s.applyPut(ev.Key, ev.Value)
				s.watchRev.Store(ev.ModRevision) // 单调推进锚点：断线重放只补增量
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

// applyPut 应用 PUT 事件（只写内存；幂等合并——本副本自己写穿的键也会经
// watch 回流，重复应用同值无害）。
func (s *EtcdModelStore) applyPut(key string, value []byte) {
	kind, parts := classifyKey(key)
	switch kind {
	case "model":
		var m Model
		if err := json.Unmarshal(value, &m); err != nil || m.Name != parts[0] {
			log.Warnf("[modelrepo] 忽略坏模型事件 %s: %v", key, err)
			return
		}
		s.mu.Lock()
		s.models[m.Name] = copyModel(&m)
		s.mu.Unlock()
	case "version":
		var v ModelVersion
		if err := json.Unmarshal(value, &v); err != nil || v.Model != parts[0] || v.Version != parts[1] {
			log.Warnf("[modelrepo] 忽略坏版本事件 %s: %v", key, err)
			return
		}
		s.mu.RLock()
		_, ok := s.models[parts[0]]
		s.mu.RUnlock()
		if !ok {
			log.Warnf("[modelrepo] 忽略孤儿版本事件 %s（模型不存在，L25 口径）", key)
			return
		}
		s.mu.Lock()
		if s.version[parts[0]] == nil {
			s.version[parts[0]] = make(map[string]*ModelVersion)
		}
		s.version[parts[0]][parts[1]] = copyVersion(&v)
		s.mu.Unlock()
	case "releaseHead":
		var r ModelRelease
		if err := json.Unmarshal(value, &r); err != nil || r.ID != parts[0] {
			log.Warnf("[modelrepo] 忽略坏发布事件 %s: %v", key, err)
			return
		}
		s.mu.Lock()
		s.release[r.ID] = copyRelease(&r)
		s.mu.Unlock()
	case "releaseNode":
		var nr NodeReleaseResult
		if err := json.Unmarshal(value, &nr); err != nil || nr.NodeID != parts[1] {
			log.Warnf("[modelrepo] 忽略坏 perNode 事件 %s: %v", key, err)
			return
		}
		s.mu.RLock()
		_, ok := s.release[parts[0]]
		s.mu.RUnlock()
		if !ok {
			log.Warnf("[modelrepo] 忽略孤儿 perNode 事件 %s（head 缺失）", key)
			return
		}
		s.mu.Lock()
		if s.nodes[parts[0]] == nil {
			s.nodes[parts[0]] = make(map[string]*NodeReleaseResult)
		}
		cp := nr
		s.nodes[parts[0]][parts[1]] = &cp
		s.mu.Unlock()
	case "deployment":
		var d DeploymentState
		if err := json.Unmarshal(value, &d); err != nil || d.Model != parts[0] || d.NodeID != parts[1] {
			log.Warnf("[modelrepo] 忽略坏部署事件 %s: %v", key, err)
			return
		}
		s.mu.RLock()
		_, ok := s.models[parts[0]]
		s.mu.RUnlock()
		if !ok {
			return
		}
		s.mu.Lock()
		if s.deploys[parts[0]] == nil {
			s.deploys[parts[0]] = make(map[string]DeploymentState)
		}
		s.deploys[parts[0]][parts[1]] = d
		s.mu.Unlock()
	}
	// guards/lock/未知键：忽略（内存无守卫概念；锁归控制器经 LockKV 消费）
}

// applyDelete 应用 DELETE 事件（只写内存）。
func (s *EtcdModelStore) applyDelete(key string) {
	kind, parts := classifyKey(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "model":
		delete(s.models, parts[0])
		delete(s.version, parts[0])
		delete(s.deploys, parts[0])
	case "version":
		if m, ok := s.version[parts[0]]; ok {
			delete(m, parts[1])
			if len(m) == 0 {
				delete(s.version, parts[0])
			}
		}
	case "releaseHead":
		// 终态 release 键永久保留（D9/N-1），删除事件仅来自模型级联清理等
		// 运维操作：同步清理 head 与 perNode。
		delete(s.release, parts[0])
		delete(s.nodes, parts[0])
	case "releaseNode":
		if m, ok := s.nodes[parts[0]]; ok {
			delete(m, parts[1])
		}
	case "deployment":
		if m, ok := s.deploys[parts[0]]; ok {
			delete(m, parts[1])
		}
	}
}

// reloadAll 全量重放（断线/ErrCompacted/通道关闭）：ListByPrefixRev →
// buildMemory 整体重建（模型存储无"本地私有瞬态"，写穿保证 etcd 即全量，
// 重建无损）→ 重锚定。重放降频：相邻间隔 ≥30s（防重连风暴，设计 §3.5）。
func (s *EtcdModelStore) reloadAll(ctx context.Context) {
	s.mu.Lock()
	now := time.Now()
	if !s.lastReload.IsZero() && now.Sub(s.lastReload) < watchReloadMinInterval {
		wait := watchReloadMinInterval - now.Sub(s.lastReload)
		s.mu.Unlock()
		log.Warnf("modelrepo: watch 重放过频，降频等待 %v 后全量重放", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	} else {
		s.mu.Unlock()
	}
	entries, rev, err := s.ext.ListByPrefixRev(ctx, KeyNamePrefix)
	if err != nil {
		log.Warnf("modelrepo: 全量重放失败（保持旧内存，stale-but-consistent；周期重扫兜底）: %v", err)
		return
	}
	s.mu.Lock()
	s.lastReload = time.Now()
	s.mu.Unlock()
	s.buildMemory(entries)
	s.watchRev.Store(rev)
}

// watchReloadMinInterval 是 watch 全量重放的最小间隔（防重连风暴）。
const watchReloadMinInterval = 30 * time.Second

// Close 停止 watch 应用器（幂等）。注意：不关闭底层 kv——kv 生命周期归
// 存储装配层所有（对齐 EtcdRegistry.Close 契约，避免双重 Close）。
func (s *EtcdModelStore) Close() error {
	s.once.Do(func() { close(s.closeCh) })
	return nil
}
