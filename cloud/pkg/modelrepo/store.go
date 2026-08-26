package modelrepo

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ModelStore 是模型仓库的存储接口（设计 WBS-2）：同一方法面由内存实现
// （MemoryModelStore，互斥锁串行 = 强一致）与 etcd 写穿实现
// （EtcdModelStore，写穿 + CAS + watch，embed/外部共用）共同实现，
// 集成层面向本接口装配；控制器与 API 处理器只依赖本接口。
//
// 契约语义（三模式一致的对外承诺）：
//   - 写路径返回 error：内存实现除前置校验外恒为 nil；etcd 实现
//     "写成功 = 已持久化"（先写 etcd、成功后才更新内存缓存，失败内存不动）；
//   - 读路径（Get/List）恒走内存缓存（HTTP 热路径永不读 etcd）；
//   - CreateRelease 的 guard 语义（同模型并发发布互斥）：etcd 实现 =
//     create-if-absent CAS + **孤儿 guard 自愈**（主线裁决 D3：guard CAS
//     冲突 → 读 guard 指向的 release 键 → 不存在或已终态 → 按值 CAS 删
//     guard → 重试一次；仍冲突 → ErrReleaseConflict 带在途 ID）+ **模型
//     meta 复查**（裁决 D7：guard CAS 成功后复查模型键存在，缺失 → 删除
//     guard + ErrModelNotFound，封堵"模型删除×发布创建"竞态）；内存实现
//     以互斥锁等价实现两语义（单进程内无孤儿窗口）；
//   - 发布状态流转（UpdateReleaseHead）为 CAS 读-改-写，冲突重试 ≤3，
//     耗尽返回 ErrConcurrentConflict；
//   - Load（embed/内存无需/全量加载）/ LoadAnchored + StartWatch（外部
//     模式多副本读一致；watch 应用器只写内存、永不发 etcd 写）。
type ModelStore interface {
	// CreateModel 创建模型；已存在 → ErrModelExists。CreatedAt/UpdatedAt 由
	// 实现填充覆盖（调用方可不设）。
	CreateModel(ctx context.Context, m *Model) error
	// GetModel 返回模型拷贝；不存在 → ErrModelNotFound。
	GetModel(ctx context.Context, name string) (*Model, error)
	// ListModels 返回全部模型，按 name 排序（拷贝）。
	ListModels(ctx context.Context) ([]Model, error)
	// UpdateModel CAS 读-改-写更新模型（description/type/metadata 整表替换，
	// 由 patch 闭包决定）；内部重试 ≤3，耗尽 → ErrConcurrentConflict；
	// 不存在 → ErrModelNotFound。patch 返回错误时中止且不写。
	UpdateModel(ctx context.Context, name string, patch func(*Model) error) error
	// DeleteModel 删除模型：前置校验（无 active 版本 → 否则
	// ErrModelHasActiveVersion；无在途发布 → 否则 ErrReleaseConflict）后
	// 级联删除 versions+deployments+meta+guard（设计 §3.3，非事务级联，
	// L25：中途崩溃留孤儿键，不可见，启动加载只认 meta 存在的前缀）。
	DeleteModel(ctx context.Context, name string) error

	// CreateVersion 创建版本（初始 draft；状态由实现强制为 draft）；
	// 模型不存在 → ErrModelNotFound；版本已存在 → ErrVersionExists。
	CreateVersion(ctx context.Context, v *ModelVersion) error
	// GetVersion 返回版本拷贝；不存在 → ErrVersionNotFound。
	GetVersion(ctx context.Context, model, version string) (*ModelVersion, error)
	// ListVersions 返回模型下全部版本，按 version 排序；模型不存在 → ErrModelNotFound。
	ListVersions(ctx context.Context, model string) ([]ModelVersion, error)
	// DeleteVersion 删除版本（仅 draft/archived；active → ErrVersionActive）；
	// 不存在 → ErrVersionNotFound。etcd 实现为 CompareAndDelete（rev 守卫），
	// 冲突重试 ≤3。
	DeleteVersion(ctx context.Context, model, version string) error
	// ActivateVersion 激活版本（draft→active；自动把当前 active 置为 archived；
	// 双键 CAS 序列 + ②失败补偿，设计 §3.3）：目标不存在 → ErrVersionNotFound；
	// 非 draft → ErrVersionNotDraft；冲突重试 ≤3 / 补偿失败 → ErrConcurrentConflict。
	ActivateVersion(ctx context.Context, model, version string) error
	// ArchiveVersion 归档版本（active→archived）：目标不存在 → ErrVersionNotFound；
	// 非 active → ErrVersionNotActive；存在指向该版本的 pending/running 发布 →
	// 409（wrap ErrReleaseConflict 带在途 ID，防发布中归档目标）。
	ArchiveVersion(ctx context.Context, model, version string) error
	// ActiveVersion 返回模型当前 active 版本拷贝；无 active → (nil, nil)。
	ActiveVersion(ctx context.Context, model string) (*ModelVersion, error)

	// CreateRelease 创建发布：guard CAS（create-if-absent，冲突 → 孤儿自愈
	// D3 后重试一次，仍冲突 → *ReleaseConflictError 带在途 ID）+ 模型 meta
	// 复查（D7，缺失 → 删 guard + ErrModelNotFound）+ release 头键 CAS。
	// CreatedAt/Status 由实现填充覆盖。逐节点 pending 预写由调用方经
	// SetNodeResult 完成（handler 创建流程第 6 步，设计 §5.2）。
	CreateRelease(ctx context.Context, r *ModelRelease) error
	// GetRelease 返回发布头拷贝；不存在 → ErrReleaseNotFound。
	GetRelease(ctx context.Context, id string) (*ModelRelease, error)
	// ListReleases 返回发布列表（model 非空过滤；"" = 全部），按 createdAt
	// 升序。控制器扫描全部在途发布用 ""。
	ListReleases(ctx context.Context, model string) ([]ModelRelease, error)
	// UpdateReleaseHead CAS 读-改-写发布头（状态机流转键）；mutate 返回错误
	// 时中止不写；冲突重试 ≤3，耗尽 → ErrConcurrentConflict；
	// 不存在 → ErrReleaseNotFound。
	UpdateReleaseHead(ctx context.Context, id string, mutate func(*ModelRelease) error) error
	// CancelRelease 取消发布（pending/running → canceled + FinishedAt；
	// 已终态 → ErrReleaseTerminal）。perNode skipped 补齐由控制器 ≤1 扫描
	// 周期完成（L27）。
	CancelRelease(ctx context.Context, id string) error
	// RequestRollback 请求回滚（设计 §5.5 前置守卫全量落这里）：
	//  1) status ∈ {running, succeeded, failed, canceled}（pending/rolled_back
	//     → ErrReleaseTerminal 409；重复请求幂等返回 nil）；
	//  2) PrevActive != "" 且该版本存在（否则 ErrNoPrevActive → 422）；
	//  3) release.Version == 模型当前 active（否则 ErrVersionMismatch → 409，
	//     设计 L26 守卫）；
	// 通过 → RollbackRequested=true（CAS 写 head），控制器下一轮异步执行。
	RequestRollback(ctx context.Context, id string) error

	// SetNodeResult 写逐节点结果：首写 create-if-absent（批序号预分配）；
	// 之后 CAS 更新（回滚只更新 Version 字段，禁止 →pending 回退）。
	// release 不存在 → ErrReleaseNotFound。
	SetNodeResult(ctx context.Context, releaseID, nodeID string, r *NodeReleaseResult) error
	// GetNodeResult 返回逐节点结果；无记录 → (nil, nil)。
	GetNodeResult(ctx context.Context, releaseID, nodeID string) (*NodeReleaseResult, error)
	// ListNodeResults 返回发布下全部逐节点结果（按 NodeID 排序）；release
	// 不存在 → ErrReleaseNotFound。
	ListNodeResults(ctx context.Context, releaseID string) ([]NodeReleaseResult, error)

	// SetDeployment 写部署影子（普通 Put 整值覆盖，幂等；派生台账非权威指令，
	// 无 CAS 需求——v0.6.0 审稿线索 3 裁定，见 types.go DeploymentState 注释）。
	SetDeployment(ctx context.Context, model, nodeID string, d DeploymentState) error
	// ListDeployments 返回模型下全部部署影子（按 NodeID 排序）。
	ListDeployments(ctx context.Context, model string) ([]DeploymentState, error)
	// DeleteModelDeployments 级联删除模型下全部部署影子（模型删除流程内调用）。
	DeleteModelDeployments(ctx context.Context, model string) error

	// ReleaseGuard 释放同模型在途发布守卫（终端态发布由控制器调用；幂等；
	// 失败返回 error，控制器下一轮重试，对齐 sweepGC 模式）。内存实现为
	// 空操作（guard 语义由互斥锁等价实现，无持久化守卫键）。
	ReleaseGuard(ctx context.Context, model string) error
	// GCReleases 清理终态发布（v0.8.0，L28）：按 CreatedAt 升序保留最近
	// keep 条终态（succeeded/failed/canceled/rolled_back），删除更旧的
	// 终态及其逐节点结果；非终态/在途发布绝不删除。返回删除条数。
	// keep < 1 → 报错（调用方负责校验）。默认关闭（L31 审计口径：终态键
	// 永久保留），由 EDGEFLOW_CLOUDCORE_RELEASE_GC_ENABLED=1 显式开启。
	GCReleases(ctx context.Context, model string, keep int) (int, error)

	// Load 全量加载持久化后端并灌入内存缓存（embed/纯内存路径；纯内存 = 空操作）。
	Load(ctx context.Context) error
	// LoadAnchored 锚定加载（外部模式）：全量加载 + 返回响应头 revision 作为
	// watch 锚点。内存/embed 用 Load（无锚定能力时返回 error）。
	LoadAnchored(ctx context.Context) (int64, error)
	// StartWatch 启动 watch 应用器（外部模式多副本读一致；应用器只写内存、
	// 永不发 etcd 写——防回写环，S3 单测零写断言；断线/ErrCompacted → 全量
	// 重放，降频 ≥30s）。内存/embed = no-op。
	StartWatch(ctx context.Context)
	// Close 释放资源（etcd 实现停止 watch 循环；不关底层 kv——kv 生命周期归
	// 存储装配层所有；纯内存实现空操作）。
	Close() error
}

// ── 纯内存实现（设计 §3.4：sync.Mutex 串行化全部写；guard 语义由互斥锁
//    等价实现，无 CAS；重启丢失是明示行为 L22）────────────────────────────

// MemoryModelStore 是 ModelStore 的内存实现（纯内存模式装配；重启后
// 模型/版本/发布/部署影子全部丢失为明示行为，设计 §9.1 矩阵 + L22 登记）。
//
// 并发安全：全部方法内部加锁；读写路径返回拷贝。
// 与 etcd 实现的语义对齐点：
//   - CreateRelease：同模型在途发布（pending/running）→ ReleaseConflictError
//     带在途 ID；模型不存在 → ErrModelNotFound（D7 的内存等价——同锁内检查，
//     无竞态窗口）；
//   - 双键激活序列在单进程内天然原子（锁内完成旧 active 降级 + 目标置位），
//     无补偿需求（对应设计 §3.4：单副本无并发 = CAS 恒成功，行为等价）。
type MemoryModelStore struct {
	mu      sync.RWMutex
	models  map[string]*Model
	version map[string]map[string]*ModelVersion      // model → version → 台账
	release map[string]*ModelRelease                 // id → 头
	nodes   map[string]map[string]*NodeReleaseResult // releaseID → nodeID → 结果
	deploys map[string]map[string]DeploymentState    // model → nodeID → 影子
	now     func() time.Time
}

// 编译期断言：内存实现满足 ModelStore 接口。
var _ ModelStore = (*MemoryModelStore)(nil)

// NewMemoryModelStore 创建空的内存模型存储（默认时钟 time.Now）。
func NewMemoryModelStore() *MemoryModelStore {
	return &MemoryModelStore{
		models:  make(map[string]*Model),
		version: make(map[string]map[string]*ModelVersion),
		release: make(map[string]*ModelRelease),
		nodes:   make(map[string]map[string]*NodeReleaseResult),
		deploys: make(map[string]map[string]DeploymentState),
		now:     time.Now,
	}
}

func (s *MemoryModelStore) nowMs() int64 { return s.now().UnixMilli() }

func copyModel(m *Model) *Model {
	if m == nil {
		return nil
	}
	out := *m
	if m.Metadata != nil {
		out.Metadata = make(map[string]string, len(m.Metadata))
		for k, v := range m.Metadata {
			out.Metadata[k] = v
		}
	}
	return &out
}

func copyVersion(v *ModelVersion) *ModelVersion {
	if v == nil {
		return nil
	}
	out := *v
	out.Archs = append([]string(nil), v.Archs...)
	if v.Metadata != nil {
		out.Metadata = make(map[string]string, len(v.Metadata))
		for k, val := range v.Metadata {
			out.Metadata[k] = val
		}
	}
	return &out
}

func copyRelease(r *ModelRelease) *ModelRelease {
	if r == nil {
		return nil
	}
	out := *r
	out.Target.NodeIDs = append([]string(nil), r.Target.NodeIDs...)
	out.TargetNodes = append([]string(nil), r.TargetNodes...)
	return &out
}

func copyDeployment(d *DeploymentState) DeploymentState {
	return *d
}

// inFlightRelease 返回同模型在途发布（pending/running）的 ID（无则 ""）。
func (s *MemoryModelStore) inFlightRelease(model string) string {
	for _, r := range s.release {
		if r.Model == model && r.Status.InFlight() {
			return r.ID
		}
	}
	return ""
}

// CreateModel 实现 ModelStore。
func (s *MemoryModelStore) CreateModel(_ context.Context, m *Model) error {
	if m == nil {
		return fmt.Errorf("modelrepo: nil model")
	}
	now := s.nowMs()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[m.Name]; ok {
		return ErrModelExists
	}
	cp := copyModel(m)
	cp.CreatedAt = now
	cp.UpdatedAt = now
	s.models[m.Name] = cp
	return nil
}

func (s *MemoryModelStore) GetModel(_ context.Context, name string) (*Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.models[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, name)
	}
	return copyModel(m), nil
}

func (s *MemoryModelStore) ListModels(_ context.Context) ([]Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Model, 0, len(s.models))
	for _, m := range s.models {
		out = append(out, *copyModel(m))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *MemoryModelStore) UpdateModel(_ context.Context, name string, patch func(*Model) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrModelNotFound, name)
	}
	cp := copyModel(m)
	if err := patch(cp); err != nil {
		return err
	}
	cp.UpdatedAt = s.nowMs()
	s.models[name] = cp
	return nil
}

func (s *MemoryModelStore) DeleteModel(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[name]; !ok {
		return fmt.Errorf("%w: %s", ErrModelNotFound, name)
	}
	for _, v := range s.version[name] {
		if v.Status == VersionStatusActive {
			return fmt.Errorf("%w: %s", ErrModelHasActiveVersion, name)
		}
	}
	if id := s.inFlightReleaseLocked(name); id != "" {
		return &ReleaseConflictError{InFlight: id}
	}
	delete(s.version, name)
	delete(s.deploys, name)
	delete(s.models, name)
	return nil
}

// inFlightReleaseLocked 同 inFlightRelease，要求调用方已持写锁。
func (s *MemoryModelStore) inFlightReleaseLocked(model string) string {
	for id, r := range s.release {
		if r.Model == model && r.Status.InFlight() {
			return id
		}
	}
	return ""
}

func (s *MemoryModelStore) CreateVersion(_ context.Context, v *ModelVersion) error {
	if v == nil {
		return fmt.Errorf("modelrepo: nil version")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[v.Model]; !ok {
		return fmt.Errorf("%w: %s", ErrModelNotFound, v.Model)
	}
	if _, ok := s.version[v.Model]; !ok {
		s.version[v.Model] = make(map[string]*ModelVersion)
	}
	if _, ok := s.version[v.Model][v.Version]; ok {
		return fmt.Errorf("%w: %s/%s", ErrVersionExists, v.Model, v.Version)
	}
	now := s.nowMs()
	cp := copyVersion(v)
	cp.Status = VersionStatusDraft // 初始 draft（设计 §4.2：POST 不激活）
	cp.CreatedAt = now
	cp.UpdatedAt = now
	s.version[v.Model][v.Version] = cp
	return nil
}

func (s *MemoryModelStore) GetVersion(_ context.Context, model, version string) (*ModelVersion, error) {
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

func (s *MemoryModelStore) ListVersions(_ context.Context, model string) ([]ModelVersion, error) {
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

func (s *MemoryModelStore) DeleteVersion(_ context.Context, model, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[model]; !ok {
		return fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	v, ok := s.version[model][version]
	if !ok {
		return fmt.Errorf("%w: %s/%s", ErrVersionNotFound, model, version)
	}
	if v.Status == VersionStatusActive {
		return fmt.Errorf("%w: %s/%s", ErrVersionActive, model, version)
	}
	delete(s.version[model], version)
	if len(s.version[model]) == 0 {
		delete(s.version, model)
	}
	return nil
}

func (s *MemoryModelStore) ActivateVersion(_ context.Context, model, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[model]; !ok {
		return fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	target, ok := s.version[model][version]
	if !ok {
		return fmt.Errorf("%w: %s/%s", ErrVersionNotFound, model, version)
	}
	if target.Status != VersionStatusDraft {
		return fmt.Errorf("%w: %s/%s (status=%s)", ErrVersionNotDraft, model, version, target.Status)
	}
	now := s.nowMs()
	// 旧 active → archived（内存模式锁内原子，无补偿需求）；被降级版本记入
	// target.PrevActive（回滚目标，API 链路 prevActive 来源）
	var prevActive string
	for v, vv := range s.version[model] {
		if vv.Status == VersionStatusActive && v != version {
			cp := copyVersion(vv)
			cp.Status = VersionStatusArchived
			cp.UpdatedAt = now
			s.version[model][v] = cp
			prevActive = v
		}
	}
	cp := copyVersion(target)
	cp.Status = VersionStatusActive
	cp.UpdatedAt = now
	cp.PrevActive = prevActive
	s.version[model][version] = cp
	return nil
}

func (s *MemoryModelStore) ArchiveVersion(_ context.Context, model, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[model]; !ok {
		return fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	target, ok := s.version[model][version]
	if !ok {
		return fmt.Errorf("%w: %s/%s", ErrVersionNotFound, model, version)
	}
	if target.Status != VersionStatusActive {
		return fmt.Errorf("%w: %s/%s (status=%s)", ErrVersionNotActive, model, version, target.Status)
	}
	// 防发布中归档目标：存在指向该版本的 pending/running 发布 → 409（带在途 ID）
	for id, r := range s.release {
		if r.Model == model && r.Version == version && r.Status.InFlight() {
			return &ReleaseConflictError{InFlight: id}
		}
	}
	cp := copyVersion(target)
	cp.Status = VersionStatusArchived
	cp.UpdatedAt = s.nowMs()
	s.version[model][version] = cp
	return nil
}

func (s *MemoryModelStore) ActiveVersion(_ context.Context, model string) (*ModelVersion, error) {
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

func (s *MemoryModelStore) CreateRelease(_ context.Context, r *ModelRelease) error {
	if r == nil {
		return fmt.Errorf("modelrepo: nil release")
	}
	if r.ID == "" {
		return fmt.Errorf("modelrepo: release id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[r.Model]; !ok {
		return fmt.Errorf("%w: %s", ErrModelNotFound, r.Model)
	}
	// guard 语义（互斥锁等价实现，设计 §3.4）：同模型在途发布 → 冲突带在途 ID
	if id := s.inFlightReleaseLocked(r.Model); id != "" {
		return &ReleaseConflictError{InFlight: id}
	}
	if _, ok := s.release[r.ID]; ok {
		return fmt.Errorf("%w: duplicate release id %s", ErrConcurrentConflict, r.ID)
	}
	cp := copyRelease(r)
	cp.Status = ReleaseStatusPending
	if cp.CreatedAt == 0 {
		cp.CreatedAt = s.nowMs()
	}
	s.release[r.ID] = cp
	return nil
}

func (s *MemoryModelStore) GetRelease(_ context.Context, id string) (*ModelRelease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.release[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrReleaseNotFound, id)
	}
	return copyRelease(r), nil
}

func (s *MemoryModelStore) ListReleases(_ context.Context, model string) ([]ModelRelease, error) {
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

func (s *MemoryModelStore) UpdateReleaseHead(_ context.Context, id string, mutate func(*ModelRelease) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.release[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrReleaseNotFound, id)
	}
	cp := copyRelease(r)
	if err := mutate(cp); err != nil {
		return err
	}
	s.release[id] = cp
	return nil
}

func (s *MemoryModelStore) CancelRelease(ctx context.Context, id string) error {
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

func (s *MemoryModelStore) RequestRollback(ctx context.Context, id string) error {
	// 注意：不能经 UpdateReleaseHead 闭包再调 GetVersion/ActiveVersion——
	// 闭包在持写锁时运行，RWMutex 不可重入会自死锁（测试暴露）。
	// 因此本方法为单临界区：锁内直接访问内部 map。
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.release[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrReleaseNotFound, id)
	}
	cp := copyRelease(r)
	switch cp.Status {
	case ReleaseStatusRunning, ReleaseStatusSucceeded, ReleaseStatusFailed, ReleaseStatusCanceled:
	case ReleaseStatusPending:
		return fmt.Errorf("%w: release pending (not yet started)", ErrReleaseTerminal)
	case ReleaseStatusRolledBack:
		return fmt.Errorf("%w: release already rolled back", ErrReleaseTerminal)
	}
	if cp.RollbackRequested {
		return nil // 幂等：已置位，重复请求结果一致（设计 §8.3）
	}
	if cp.PrevActive == "" {
		return fmt.Errorf("%w: release %s has no previous active version", ErrNoPrevActive, id)
	}
	var prev *ModelVersion
	if vs, ok := s.version[cp.Model]; ok {
		if v, ok := vs[cp.PrevActive]; ok {
			prev = copyVersion(v)
		}
	}
	if prev == nil {
		return fmt.Errorf("%w: previous active version %s/%s missing", ErrNoPrevActive, cp.Model, cp.PrevActive)
	}
	var cur *ModelVersion
	if vs, ok := s.version[cp.Model]; ok {
		for _, v := range vs {
			if v.Status == VersionStatusActive {
				cur = copyVersion(v)
				break
			}
		}
	}
	if cur == nil || cur.Version != cp.Version {
		active := "none"
		if cur != nil {
			active = cur.Version
		}
		return fmt.Errorf("%w: release version %s, current active %s（先显式 activate 目标旧版本或发起新发布，设计 L26）", ErrVersionMismatch, cp.Version, active)
	}
	cp.RollbackRequested = true
	s.release[id] = cp
	return nil
}

func (s *MemoryModelStore) SetNodeResult(_ context.Context, releaseID, nodeID string, r *NodeReleaseResult) error {
	if r == nil {
		return fmt.Errorf("modelrepo: nil node result")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.release[releaseID]; !ok {
		return fmt.Errorf("%w: %s", ErrReleaseNotFound, releaseID)
	}
	byNode, ok := s.nodes[releaseID]
	if !ok {
		byNode = make(map[string]*NodeReleaseResult)
		s.nodes[releaseID] = byNode
	}
	now := s.nowMs()
	if old, ok := byNode[nodeID]; ok {
		if !CanTransitionNodeResult(old.Status, r.Status) {
			return fmt.Errorf("modelrepo: illegal node result transition %s → %s (node %s)",
				old.Status, r.Status, nodeID)
		}
		cp := *r
		cp.Batch = old.Batch // 批序号创建时预分配，不可变（设计 §2.3）
		cp.StartedAt = old.StartedAt
		if cp.Status != NodeRelPending {
			cp.FinishedAt = now
		}
		byNode[nodeID] = &cp
		return nil
	}
	cp := *r
	cp.Batch = r.Batch
	if cp.Batch < 1 {
		cp.Batch = 1
	}
	cp.StartedAt = now
	if cp.Status != NodeRelPending {
		cp.FinishedAt = now
	}
	byNode[nodeID] = &cp
	return nil
}

func (s *MemoryModelStore) GetNodeResult(_ context.Context, releaseID, nodeID string) (*NodeReleaseResult, error) {
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

func (s *MemoryModelStore) ListNodeResults(_ context.Context, releaseID string) ([]NodeReleaseResult, error) {
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

func (s *MemoryModelStore) SetDeployment(_ context.Context, model, nodeID string, d DeploymentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[model]; !ok {
		return fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	byNode, ok := s.deploys[model]
	if !ok {
		byNode = make(map[string]DeploymentState)
		s.deploys[model] = byNode
	}
	d.Model = model
	d.NodeID = nodeID
	d.UpdatedAt = s.nowMs()
	byNode[nodeID] = d
	return nil
}

func (s *MemoryModelStore) ListDeployments(_ context.Context, model string) ([]DeploymentState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.models[model]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	out := make([]DeploymentState, 0, len(s.deploys[model]))
	for _, d := range s.deploys[model] {
		out = append(out, copyDeployment(&d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

func (s *MemoryModelStore) DeleteModelDeployments(_ context.Context, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deploys, model)
	return nil
}

// ReleaseGuard 内存实现：空操作（guard 语义由互斥锁等价实现，无持久化守卫键；
// 接口契约占位，与 etcd 实现保持方法面一致）。
func (s *MemoryModelStore) ReleaseGuard(_ context.Context, _ string) error { return nil }

// GCReleases 内存实现（v0.8.0，L28）：保留最近 keep 条终态，删除更旧的
// 终态头与其逐节点结果（防长运行内存线性增长，N-4）。
func (s *MemoryModelStore) GCReleases(_ context.Context, model string, keep int) (int, error) {
	if keep < 1 {
		return 0, fmt.Errorf("modelrepo: GCReleases keep=%d 必须 ≥1", keep)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var terminals []*ModelRelease
	for _, r := range s.release {
		if model != "" && r.Model != model {
			continue
		}
		if r.Status.IsTerminal() {
			terminals = append(terminals, r)
		}
	}
	sort.Slice(terminals, func(i, j int) bool { return terminals[i].CreatedAt < terminals[j].CreatedAt })
	removed := 0
	for i := 0; i < len(terminals)-keep; i++ {
		id := terminals[i].ID
		delete(s.release, id)
		delete(s.nodes, id)
		removed++
	}
	return removed, nil
}

// Load 纯内存实现：无可加载的持久化后端，空操作。
func (s *MemoryModelStore) Load(_ context.Context) error { return nil }

// LoadAnchored 纯内存实现：无锚定能力，返回 0, nil（外部模式装配不会选内存）。
func (s *MemoryModelStore) LoadAnchored(_ context.Context) (int64, error) { return 0, nil }

// StartWatch 纯内存实现：no-op（无跨副本写者，无 watch 需求）。
func (s *MemoryModelStore) StartWatch(_ context.Context) {}

// Close 纯内存实现：无可关闭资源，恒返回 nil（接口契约占位）。
func (s *MemoryModelStore) Close() error { return nil }
