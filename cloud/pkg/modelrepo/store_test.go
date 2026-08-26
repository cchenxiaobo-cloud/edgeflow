package modelrepo

// MemoryModelStore 单测（设计 §10.1 S1 + 任务书 WBS-7/8/9 族）：
//
//	S1 内存 CRUD + 重复创建冲突 + 排序 + 删除前置：
//	   - CreateModel/GetModel/ListModels/UpdateModel/DeleteModel
//	     （DeleteModel 含 active 版本与在途发布两种前置拒绝）；
//	   - CreateVersion/GetVersion/ListVersions/DeleteVersion
//	     （draft/archived 可删、active 拒删）；
//	   - ActivateVersion/ArchiveVersion/ActiveVersion（版本三态语义）；
//	   - CreateRelease guard 语义（同模型在途 → ReleaseConflictError 带
//	     在途 ID；模型不存在 → ErrModelNotFound = D7 内存等价）；
//	   - CancelRelease / RequestRollback（含 **RWMutex 死锁回归锚点**：
//	     RequestRollback 为单临界区——持写锁闭包内不得再取 RLock，若改回
//	     经 UpdateReleaseHead + GetVersion 组合会自死锁，测试以超时兜底）；
//	   - SetNodeResult 转换合法性（首写 create-if-absent / 更新 CAS /
//	     禁止 →pending 回退 / 批序号与 StartedAt 不可变）；
//	   - SetDeployment/ListDeployments/DeleteModelDeployments；
//	   - 并发用例（-race）：同模型并发创建发布 → 恰一个成功其余冲突；
//	     并发读 + RequestRollback 不丢数据。
//
// 内存实现的语义对齐点（设计 §3.4）：单进程互斥锁串行 = 强一致，
// guard/激活序列在锁内天然原子，无 CAS 无补偿需求——本文件只测对外
// 契约，不测 etcd 层的重试/补偿（那是 etcd_store_test.go 的职责）。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fixedTime 是测试用固定时钟（内存存储 now 注入）。
var fixedTime = time.UnixMilli(1787000000000)

// fakeNow 是可推进时钟（列表排序确定性：CreatedAt 必须严格递增）。
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeNow(t time.Time) *fakeNow { return &fakeNow{t: t} }

func (f *fakeNow) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeNow) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// newMemFixture 构造模型 + 版本（v1 已归档、v2 active）的内存环境。
func newMemFixture(t *testing.T) *MemoryModelStore {
	t.Helper()
	s := NewMemoryModelStore()
	s.now = func() time.Time { return fixedTime }
	ctx := context.Background()
	if err := s.CreateModel(ctx, &Model{Name: "defect-detector", Type: "detection"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1.0.0", "v2.0.0"} {
		if err := s.CreateVersion(ctx, &ModelVersion{
			Model: "defect-detector", Version: v, Mirror: "reg/x:" + v,
			Sha256: "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ActivateVersion(ctx, "defect-detector", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateVersion(ctx, "defect-detector", "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	return s
}

// mustHead 把 release 头经 UpdateReleaseHead 打到指定状态（测试驱动状态机
// 用；存储层不校验转换合法性——合法性由 mutate 闭包/控制器承担）。
func mustHead(t *testing.T, s *MemoryModelStore, id string, mutate func(*ModelRelease) error) {
	t.Helper()
	if err := s.UpdateReleaseHead(context.Background(), id, mutate); err != nil {
		t.Fatal(err)
	}
}

// ── S1：模型 CRUD ──────────────────────────────────────────────────────

// TestMemCreateModel 创建 + 时间戳填充 + 重复创建 409。
func TestMemCreateModel(t *testing.T) {
	s := NewMemoryModelStore()
	s.now = func() time.Time { return fixedTime }
	ctx := context.Background()
	if err := s.CreateModel(ctx, &Model{Name: "m1", Description: "d", Type: "detection",
		Metadata: map[string]string{"owner": "qa"}}); err != nil {
		t.Fatal(err)
	}
	m, err := s.GetModel(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "m1" || m.Description != "d" || m.Type != "detection" {
		t.Fatalf("模型字段丢失: %+v", m)
	}
	if m.CreatedAt != fixedTime.UnixMilli() || m.UpdatedAt != fixedTime.UnixMilli() {
		t.Fatalf("创建/更新时间应被存储填充: %+v", m)
	}
	if m.Metadata["owner"] != "qa" {
		t.Fatalf("metadata 应完整保留: %+v", m.Metadata)
	}
	// 返回的是拷贝：修改返回值不得污染内部
	m.Description = "hacked"
	got, _ := s.GetModel(ctx, "m1")
	if got.Description != "d" {
		t.Fatalf("GetModel 应返回拷贝，got %q", got.Description)
	}
	// 重复创建 → ErrModelExists
	if err := s.CreateModel(ctx, &Model{Name: "m1"}); !errors.Is(err, ErrModelExists) {
		t.Fatalf("重复创建应 ErrModelExists，got %v", err)
	}
	// nil 防御
	if err := s.CreateModel(ctx, nil); err == nil {
		t.Fatal("nil 模型应报错")
	}
	// 不存在 → ErrModelNotFound
	if _, err := s.GetModel(ctx, "nope"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("不存在模型应 ErrModelNotFound，got %v", err)
	}
}

// TestMemListModels 列表按 name 排序；空列表返回空切片而非 nil。
func TestMemListModels(t *testing.T) {
	s := NewMemoryModelStore()
	ctx := context.Background()
	empty, err := s.ListModels(ctx)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空列表应为空切片，got %v err=%v", empty, err)
	}
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if err := s.CreateModel(ctx, &Model{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Name != "alpha" || all[1].Name != "mid" || all[2].Name != "zeta" {
		t.Fatalf("应按 name 排序: %+v", all)
	}
}

// TestMemUpdateModel patch 语义：成功 / patch 返回错误中止 / 不存在 404 /
// UpdatedAt 刷新。
func TestMemUpdateModel(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()
	err := s.UpdateModel(ctx, "defect-detector", func(m *Model) error {
		m.Description = "新描述"
		m.Type = "segmentation"
		m.Metadata = map[string]string{"k": "v"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := s.GetModel(ctx, "defect-detector")
	if m.Description != "新描述" || m.Type != "segmentation" || m.Metadata["k"] != "v" {
		t.Fatalf("patch 未生效: %+v", m)
	}
	if m.UpdatedAt != fixedTime.UnixMilli() {
		t.Fatalf("UpdatedAt 应刷新: %+v", m)
	}
	// patch 返回错误 → 中止不写
	before, _ := s.GetModel(ctx, "defect-detector")
	if err := s.UpdateModel(ctx, "defect-detector", func(*Model) error {
		return errors.New("stop")
	}); err == nil {
		t.Fatal("patch 错误应透传")
	}
	after, _ := s.GetModel(ctx, "defect-detector")
	if after.Description != before.Description {
		t.Fatalf("patch 错误后不应写入: %+v vs %+v", after, before)
	}
	// 不存在 → ErrModelNotFound
	if err := s.UpdateModel(ctx, "nope", func(*Model) error { return nil }); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("应 ErrModelNotFound，got %v", err)
	}
}

// TestMemDeleteModel 删除前置：active 版本 409、在途发布 409（带在途 ID）、
// 成功删除级联（versions/deploys/meta）。
func TestMemDeleteModel(t *testing.T) {
	ctx := context.Background()

	// 有 active 版本 → ErrModelHasActiveVersion
	s := newMemFixture(t)
	if err := s.DeleteModel(ctx, "defect-detector"); !errors.Is(err, ErrModelHasActiveVersion) {
		t.Fatalf("有 active 版本应拒绝删除，got %v", err)
	}

	// 有在途发布 → ReleaseConflictError 带在途 ID（pending 校验；无 active
	// 版本时 guard 前置生效——fixture 的 v2.0.0 若 active 会先撞 active 校验）
	s2 := NewMemoryModelStore()
	s2.now = func() time.Time { return fixedTime }
	ctx = context.Background()
	if err := s2.CreateModel(ctx, &Model{Name: "defect-detector"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v1.0.0", Mirror: "reg/x:v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v1.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	err := s2.DeleteModel(ctx, "defect-detector")
	var rce *ReleaseConflictError
	if !errors.As(err, &rce) || rce.InFlight != "rel-1" {
		t.Fatalf("在途发布应拒绝删除并带在途 ID，got %v", err)
	}

	// 成功删除：版本级联清空
	s3 := NewMemoryModelStore()
	s3.now = func() time.Time { return fixedTime }
	if err := s3.CreateModel(ctx, &Model{Name: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s3.CreateVersion(ctx, &ModelVersion{Model: "m", Version: "v1", Mirror: "reg/x:v1"}); err != nil {
		t.Fatal(err)
	}
	if err := s3.SetDeployment(ctx, "m", "n1", DeploymentState{Model: "m", NodeID: "n1", Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := s3.DeleteModel(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := s3.GetModel(ctx, "m"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("删除后模型应 404，got %v", err)
	}
	if vs, err := s3.ListVersions(ctx, "m"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("删除后版本列表应 404，got %v err=%v", vs, err)
	}
	if ds, _ := s3.ListDeployments(ctx, "m"); len(ds) != 0 {
		t.Fatalf("部署影子应清空: %+v", ds)
	}
	// 重复删除 → ErrModelNotFound
	if err := s3.DeleteModel(ctx, "m"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("重复删除应 404，got %v", err)
	}
}

// ── S1：版本 CRUD 与状态机 ─────────────────────────────────────────────

// TestMemVersionCreate 创建（强制 draft）+ 重复 409 + 模型缺失 404 +
// 排序列表。
func TestMemVersionCreate(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()

	// 强制初始 draft + 时间戳填充
	if err := s.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0",
		Mirror: "reg/x:v3.0.0", Archs: []string{"amd64", "arm64"}}); err != nil {
		t.Fatal(err)
	}
	v, err := s.GetVersion(ctx, "defect-detector", "v3.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != VersionStatusDraft {
		t.Fatalf("创建版本应强制 draft，got %s", v.Status)
	}
	if v.CreatedAt != fixedTime.UnixMilli() || v.UpdatedAt != fixedTime.UnixMilli() {
		t.Fatalf("版本时间戳应填充: %+v", v)
	}
	// 重复 → ErrVersionExists
	if err := s.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0", Mirror: "reg/x:v3.0.0"}); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("重复版本应 ErrVersionExists，got %v", err)
	}
	// 模型缺失 → ErrModelNotFound
	if err := s.CreateVersion(ctx, &ModelVersion{Model: "nope", Version: "v1"}); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("模型缺失应 ErrModelNotFound，got %v", err)
	}
	// 列表排序 + 拷贝语义
	vs, err := s.ListVersions(ctx, "defect-detector")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 || vs[0].Version != "v1.0.0" || vs[1].Version != "v2.0.0" || vs[2].Version != "v3.0.0" {
		t.Fatalf("版本应按 tag 排序: %+v", vs)
	}
	vs[0].Mirror = "hacked"
	got, _ := s.GetVersion(ctx, "defect-detector", "v1.0.0")
	if got.Mirror == "hacked" {
		t.Fatal("GetVersion 应返回拷贝")
	}
}

// TestMemVersionDelete 删除语义：draft 可删、archived 可删、active 拒删
// （409）、缺失 404、模型缺失 404。
func TestMemVersionDelete(t *testing.T) {
	ctx := context.Background()

	// active → ErrVersionActive
	s := newMemFixture(t)
	if err := s.DeleteVersion(ctx, "defect-detector", "v2.0.0"); !errors.Is(err, ErrVersionActive) {
		t.Fatalf("active 版本应拒删，got %v", err)
	}

	// draft 可删
	s2 := NewMemoryModelStore()
	s2.now = func() time.Time { return fixedTime }
	if err := s2.CreateModel(ctx, &Model{Name: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.CreateVersion(ctx, &ModelVersion{Model: "m", Version: "draft-v", Mirror: "reg/x:draft-v"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.DeleteVersion(ctx, "m", "draft-v"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.GetVersion(ctx, "m", "draft-v"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("删除后应 404，got %v", err)
	}

	// archived 可删（激活 v2 后 v1 自动归档）
	s3 := newMemFixture(t)
	if err := s3.DeleteVersion(ctx, "defect-detector", "v1.0.0"); err != nil {
		t.Fatalf("archived 版本应可删，got %v", err)
	}

	// 缺失 / 模型缺失 → 404
	if err := s2.DeleteVersion(ctx, "m", "nope"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("版本缺失应 404，got %v", err)
	}
	if err := s2.DeleteVersion(ctx, "nope", "v1"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("模型缺失应 404，got %v", err)
	}
}

// TestMemActivateArchive 激活/归档状态机：activate 自动降级旧 active、
// 非 draft 拒激活（409）、非 active 拒归档（409）、ActiveVersion 查询。
func TestMemActivateArchive(t *testing.T) {
	ctx := context.Background()
	s := newMemFixture(t)

	// 当前 active = v2.0.0（fixture 已激活）
	av, err := s.ActiveVersion(ctx, "defect-detector")
	if err != nil || av == nil || av.Version != "v2.0.0" {
		t.Fatalf("ActiveVersion 应为 v2.0.0，got %+v err=%v", av, err)
	}

	// 激活 draft v3 → v2 自动归档
	if err := s.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0", Mirror: "reg/x:v3.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateVersion(ctx, "defect-detector", "v3.0.0"); err != nil {
		t.Fatal(err)
	}
	old, _ := s.GetVersion(ctx, "defect-detector", "v2.0.0")
	if old.Status != VersionStatusArchived {
		t.Fatalf("旧 active 应自动归档，got %s", old.Status)
	}
	av, _ = s.ActiveVersion(ctx, "defect-detector")
	if av.Version != "v3.0.0" {
		t.Fatalf("active 应为 v3.0.0，got %s", av.Version)
	}

	// 已归档版本不可再激活 → ErrVersionNotDraft
	if err := s.ActivateVersion(ctx, "defect-detector", "v2.0.0"); !errors.Is(err, ErrVersionNotDraft) {
		t.Fatalf("archived 再激活应 ErrVersionNotDraft，got %v", err)
	}
	// active 再激活 → ErrVersionNotDraft
	if err := s.ActivateVersion(ctx, "defect-detector", "v3.0.0"); !errors.Is(err, ErrVersionNotDraft) {
		t.Fatalf("active 再激活应 ErrVersionNotDraft，got %v", err)
	}
	// 归档 active → archived
	if err := s.ArchiveVersion(ctx, "defect-detector", "v3.0.0"); err != nil {
		t.Fatal(err)
	}
	// 归档非 active → ErrVersionNotActive
	if err := s.ArchiveVersion(ctx, "defect-detector", "v3.0.0"); !errors.Is(err, ErrVersionNotActive) {
		t.Fatalf("archived 再归档应 ErrVersionNotActive，got %v", err)
	}
	// 缺失/模型缺失 → 404
	if err := s.ArchiveVersion(ctx, "defect-detector", "nope"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("版本缺失应 404，got %v", err)
	}
	if err := s.ActivateVersion(ctx, "nope", "v1.0.0"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("模型缺失应 404，got %v", err)
	}
}

// TestMemArchiveInFlightVersion 归档在途发布目标版本 → ReleaseConflictError
// 带在途 ID（设计 §2.4：防发布中归档目标）。
func TestMemArchiveInFlightVersion(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()
	if err := s.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	err := s.ArchiveVersion(ctx, "defect-detector", "v2.0.0")
	var rce *ReleaseConflictError
	if !errors.As(err, &rce) || rce.InFlight != "rel-1" {
		t.Fatalf("归档在途目标应 409 带在途 ID，got %v", err)
	}
	// 发布终态（canceled）后再归档 → 放行
	if err := s.CancelRelease(ctx, "rel-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveVersion(ctx, "defect-detector", "v2.0.0"); err != nil {
		t.Fatalf("在途解除后归档应放行，got %v", err)
	}
}

// ── S1：发布存储（guard / 状态机 / cancel / rollback）──────────────────

// TestMemCreateRelease 创建（pending、时间戳）+ 同模型在途 409 带在途 ID
// + 模型缺失 404（D7 内存等价）+ Get/List 排序 + nil 防御。
func TestMemCreateRelease(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()

	rel := &ModelRelease{ID: "rel-1", Model: "defect-detector", Version: "v2.0.0",
		Target:      ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{"n1"}},
		TargetNodes: []string{"n1"}, BatchSize: 2, PauseBetween: 30000,
		PrevActive: "v1.0.0"}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRelease(ctx, "rel-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ReleaseStatusPending {
		t.Fatalf("创建发布应 pending，got %s", got.Status)
	}
	if got.CreatedAt != fixedTime.UnixMilli() {
		t.Fatalf("CreatedAt 应填充: %+v", got)
	}
	if got.TargetNodes[0] != "n1" || got.BatchSize != 2 {
		t.Fatalf("发布字段应完整: %+v", got)
	}

	// 同模型在途 → ReleaseConflictError 带在途 ID（guard 409 语义）
	rel2 := &ModelRelease{ID: "rel-2", Model: "defect-detector", Version: "v2.0.0",
		BatchSize: 1, TargetNodes: []string{"n1"}}
	err = s.CreateRelease(ctx, rel2)
	var rce *ReleaseConflictError
	if !errors.As(err, &rce) || rce.InFlight != "rel-1" {
		t.Fatalf("在途发布应 409 带在途 ID，got %v", err)
	}

	// 模型缺失 → ErrModelNotFound
	if err := s.CreateRelease(ctx, &ModelRelease{ID: "rel-3", Model: "nope", Version: "v1", BatchSize: 1}); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("模型缺失应 404，got %v", err)
	}
	// nil / 空 ID 防御
	if err := s.CreateRelease(ctx, nil); err == nil {
		t.Fatal("nil release 应报错")
	}
	if err := s.CreateRelease(ctx, &ModelRelease{Model: "defect-detector", Version: "v2.0.0"}); err == nil {
		t.Fatal("空 release ID 应报错")
	}
	// 重复 ID（不同模型也冲突——UUID 全局唯一；同模型重复 ID 会先撞在途
	// guard，故用另一模型覆盖重复 ID 路径）→ ErrConcurrentConflict
	if err := s.CreateModel(ctx, &Model{Name: "other-model"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "other-model", Version: "v1", BatchSize: 1}); !errors.Is(err, ErrConcurrentConflict) {
		t.Fatalf("重复 release ID 应 ErrConcurrentConflict，got %v", err)
	}
	// List 按 createdAt 升序 + 模型过滤（用可推进时钟保证 CreatedAt 严格递增）
	clk := newFakeNow(fixedTime)
	s2 := NewMemoryModelStore()
	s2.now = clk.Now
	if err := s2.CreateModel(ctx, &Model{Name: "defect-detector"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector", Version: "v1", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Millisecond)
	if err := s2.CancelRelease(ctx, "rel-1"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Millisecond)
	if err := s2.CreateRelease(ctx, &ModelRelease{ID: "rel-4", Model: "defect-detector", Version: "v1", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	rels, err := s2.ListReleases(ctx, "defect-detector")
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 || rels[0].ID != "rel-1" || rels[1].ID != "rel-4" {
		t.Fatalf("应按 createdAt 升序: %+v", rels)
	}
	if all, _ := s.ListReleases(ctx, ""); len(all) != 1 || all[0].ID != "rel-1" {
		t.Fatalf("空 model 应返回全部（rel-4 在 s2 独立夹具中）: %+v", all)
	}
	if _, err := s.GetRelease(ctx, "nope"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("缺失发布应 404，got %v", err)
	}
}

// TestMemCancelRelease cancel 语义：pending/running → canceled + FinishedAt；
// 终态再 cancel → ErrReleaseTerminal。
func TestMemCancelRelease(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()
	if err := s.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelRelease(ctx, "rel-1"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetRelease(ctx, "rel-1")
	if r.Status != ReleaseStatusCanceled || r.FinishedAt != fixedTime.UnixMilli() {
		t.Fatalf("cancel 应置 canceled + FinishedAt: %+v", r)
	}
	// 终态再 cancel → 409（幂等语义：可安全重复调用，结果一致）
	if err := s.CancelRelease(ctx, "rel-1"); !errors.Is(err, ErrReleaseTerminal) {
		t.Fatalf("终态再 cancel 应 ErrReleaseTerminal，got %v", err)
	}
	// running 中 cancel 同样放行（控制器认领后 API 取消）
	mustHead(t, s, "rel-1", func(r *ModelRelease) error {
		r.Status = ReleaseStatusRunning
		return nil
	})
	if err := s.CancelRelease(ctx, "rel-1"); err != nil {
		t.Fatalf("running 中 cancel 应放行，got %v", err)
	}
}

// newRollbackFixture 构造可回滚发布：v1 曾 active（现 archived）、v2 active、
// release{Version: v2, PrevActive: v1} 已跑完（succeeded + perNode deployed）。
func newRollbackFixture(t *testing.T) (*MemoryModelStore, string) {
	t.Helper()
	s := newMemFixture(t) // v1 archived、v2 active
	ctx := context.Background()
	rel := &ModelRelease{ID: "rel-rb", Model: "defect-detector", Version: "v2.0.0",
		Target:      ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{"n1", "n2"}},
		TargetNodes: []string{"n1", "n2"}, BatchSize: 1, PrevActive: "v1.0.0"}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"n1", "n2"} {
		if err := s.SetNodeResult(ctx, rel.ID, n, &NodeReleaseResult{NodeID: n, Status: NodeRelDeployed, Version: "v2.0.0", Batch: 1}); err != nil {
			t.Fatal(err)
		}
	}
	mustHead(t, s, rel.ID, func(r *ModelRelease) error {
		if r.Status != ReleaseStatusPending {
			return errors.New("not pending")
		}
		r.Status = ReleaseStatusSucceeded
		r.FinishedAt = fixedTime.UnixMilli()
		return nil
	})
	return s, rel.ID
}

// TestMemRequestRollback_成功 回滚成功路径（单临界区直接读内部 map——
// 死锁回归锚点的覆盖路径之一）。
func TestMemRequestRollback_成功(t *testing.T) {
	s, relID := newRollbackFixture(t)
	if err := s.RequestRollback(context.Background(), relID); err != nil {
		t.Fatalf("回滚请求应成功，got %v", err)
	}
	r, _ := s.GetRelease(context.Background(), relID)
	if !r.RollbackRequested {
		t.Fatalf("RollbackRequested 应置位: %+v", r)
	}
	if r.Status != ReleaseStatusSucceeded {
		t.Fatalf("回滚请求不应改状态: %+v", r)
	}
	// 幂等：重复请求结果一致（设计 §8.3）
	if err := s.RequestRollback(context.Background(), relID); err != nil {
		t.Fatalf("重复回滚请求应幂等，got %v", err)
	}
}

// TestMemRequestRollback_死锁回归锚点 RequestRollback 必须在**单临界区**
// 内完成全部校验（直接访问内部 map）：若改回经 UpdateReleaseHead 闭包内
// 再调 GetVersion/ActiveVersion（各取 RLock），RWMutex 不可重入 → 自死锁。
// 本测试以 goroutine + 超时兜底暴露死锁（-race 下另捕获数据竞争）；并发
// 读者放大竞争面。
func TestMemRequestRollback_死锁回归锚点(t *testing.T) {
	s, relID := newRollbackFixture(t)
	ctx := context.Background()

	// 并发读者：GetVersion/ActiveVersion 取 RLock，与 RequestRollback 写锁
	// 交替——若实现引入重入，必在此处死锁或 -race 报错。
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = s.GetVersion(ctx, "defect-detector", "v1.0.0")
					_, _ = s.ActiveVersion(ctx, "defect-detector")
				}
			}
		}()
	}

	done := make(chan error, 1)
	go func() { done <- s.RequestRollback(ctx, relID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("回滚请求失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(stop)
		t.Fatal("RequestRollback 死锁：单临界区回归（持写锁闭包内不得再取 RLock）")
	}
	close(stop)
	wg.Wait()
}

// TestMemRequestRollback_前置守卫 全部拒绝路径：
// pending → 409；rolled_back → 409；无 PrevActive → 422；
// PrevActive 版本缺失 → 422；version ≠ 当前 active → 409（L26 守卫）。
func TestMemRequestRollback_前置守卫(t *testing.T) {
	ctx := context.Background()

	// pending → ErrReleaseTerminal
	s := newMemFixture(t)
	if err := s.CreateRelease(ctx, &ModelRelease{ID: "rel-p", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}, PrevActive: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestRollback(ctx, "rel-p"); !errors.Is(err, ErrReleaseTerminal) {
		t.Fatalf("pending 回滚应 409，got %v", err)
	}

	// rolled_back → ErrReleaseTerminal
	s, relID := newRollbackFixture(t)
	mustHead(t, s, relID, func(r *ModelRelease) error {
		r.Status = ReleaseStatusRolledBack
		r.FinishedAt = fixedTime.UnixMilli()
		return nil
	})
	if err := s.RequestRollback(ctx, relID); !errors.Is(err, ErrReleaseTerminal) {
		t.Fatalf("rolled_back 再回滚应 409，got %v", err)
	}

	// 无 PrevActive → ErrNoPrevActive（422）
	s2 := newMemFixture(t)
	if err := s2.CreateRelease(ctx, &ModelRelease{ID: "rel-np", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	mustHead(t, s2, "rel-np", func(r *ModelRelease) error {
		r.Status = ReleaseStatusSucceeded
		return nil
	})
	if err := s2.RequestRollback(ctx, "rel-np"); !errors.Is(err, ErrNoPrevActive) {
		t.Fatalf("无 PrevActive 应 422，got %v", err)
	}

	// PrevActive 版本被删 → ErrNoPrevActive（422；F-4 TOCTOU 的存储侧守卫）
	s3, relID3 := newRollbackFixture(t)
	if err := s3.DeleteVersion(ctx, "defect-detector", "v1.0.0"); err != nil {
		t.Fatal(err) // v1 archived 可删
	}
	if err := s3.RequestRollback(ctx, relID3); !errors.Is(err, ErrNoPrevActive) {
		t.Fatalf("PrevActive 缺失应 422，got %v", err)
	}

	// version ≠ 当前 active（被新版本接管）→ ErrVersionMismatch（409，L26）
	s4, relID4 := newRollbackFixture(t)
	if err := s4.CreateVersion(ctx, &ModelVersion{Model: "defect-detector", Version: "v3.0.0", Mirror: "reg/x:v3.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := s4.ActivateVersion(ctx, "defect-detector", "v3.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := s4.RequestRollback(ctx, relID4); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("被新版本接管应 409 ErrVersionMismatch，got %v", err)
	}
	// 无 active（模型 active 全被归档）→ 同样 ErrVersionMismatch
	s5, relID5 := newRollbackFixture(t)
	if err := s5.ArchiveVersion(ctx, "defect-detector", "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := s5.RequestRollback(ctx, relID5); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("无 active 应 ErrVersionMismatch，got %v", err)
	}

	// 缺失 → ErrReleaseNotFound
	if err := s.RequestRollback(ctx, "nope"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("缺失发布应 404，got %v", err)
	}
}

// ── S1：逐节点结果 ─────────────────────────────────────────────────────

// TestMemSetNodeResult 首写 create-if-absent（批序号/时间戳默认）+ CAS
// 更新（Batch/StartedAt 不可变）+ 转换合法性（→pending 回退拒绝）+ 缺失
// 发布 404。
func TestMemSetNodeResult(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()
	if err := s.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 2, TargetNodes: []string{"n1", "n2"}}); err != nil {
		t.Fatal(err)
	}

	// 首写：Batch 预分配 + StartedAt 填充；pending 不写 FinishedAt
	if err := s.SetNodeResult(ctx, "rel-1", "n1", &NodeReleaseResult{NodeID: "n1", Status: NodeRelPending, Batch: 1}); err != nil {
		t.Fatal(err)
	}
	nr, err := s.GetNodeResult(ctx, "rel-1", "n1")
	if err != nil || nr == nil {
		t.Fatalf("perNode 应已写，got %+v err=%v", nr, err)
	}
	if nr.Batch != 1 || nr.StartedAt != fixedTime.UnixMilli() || nr.FinishedAt != 0 {
		t.Fatalf("首写应填 Batch/StartedAt 且 pending 无 FinishedAt: %+v", nr)
	}
	// 无记录 → (nil, nil)
	if nr, _ := s.GetNodeResult(ctx, "rel-1", "nope"); nr != nil {
		t.Fatalf("无记录应 (nil, nil)，got %+v", nr)
	}

	// 更新：pending → deployed（合法）；Batch/StartedAt 保持、FinishedAt 填充
	if err := s.SetNodeResult(ctx, "rel-1", "n1", &NodeReleaseResult{NodeID: "n1", Status: NodeRelDeployed, Version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	nr, _ = s.GetNodeResult(ctx, "rel-1", "n1")
	if nr.Status != NodeRelDeployed || nr.Batch != 1 || nr.StartedAt != fixedTime.UnixMilli() || nr.FinishedAt != fixedTime.UnixMilli() {
		t.Fatalf("更新应保持 Batch/StartedAt 并填 FinishedAt: %+v", nr)
	}

	// 回滚更新：deployed → deployed + Version 改为 PrevActive（合法，设计 §2.4）
	if err := s.SetNodeResult(ctx, "rel-1", "n1", &NodeReleaseResult{NodeID: "n1", Status: NodeRelDeployed, Version: "v1.0.0"}); err != nil {
		t.Fatalf("终态→终态更新应合法，got %v", err)
	}
	nr, _ = s.GetNodeResult(ctx, "rel-1", "n1")
	if nr.Version != "v1.0.0" {
		t.Fatalf("回滚应更新 Version 字段: %+v", nr)
	}

	// 转换非法：deployed → pending 回退拒绝
	if err := s.SetNodeResult(ctx, "rel-1", "n1", &NodeReleaseResult{NodeID: "n1", Status: NodeRelPending}); err == nil {
		t.Fatal("→pending 回退应拒绝")
	}
	// skipped → deployed 恢复（回滚覆盖 skipped 节点，L23 收敛）合法
	if err := s.SetNodeResult(ctx, "rel-1", "n2", &NodeReleaseResult{NodeID: "n2", Status: NodeRelPending, Batch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeResult(ctx, "rel-1", "n2", &NodeReleaseResult{NodeID: "n2", Status: NodeRelSkipped}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeResult(ctx, "rel-1", "n2", &NodeReleaseResult{NodeID: "n2", Status: NodeRelDeployed, Version: "v1.0.0"}); err != nil {
		t.Fatalf("skipped→deployed 回滚恢复应合法，got %v", err)
	}

	// 列表按 NodeID 排序
	all, err := s.ListNodeResults(ctx, "rel-1")
	if err != nil || len(all) != 2 {
		t.Fatalf("应返回 2 条，got %+v err=%v", all, err)
	}
	if all[0].NodeID != "n1" || all[1].NodeID != "n2" {
		t.Fatalf("应按 NodeID 排序: %+v", all)
	}
	// 缺失发布 → 404
	if err := s.SetNodeResult(ctx, "nope", "n1", &NodeReleaseResult{NodeID: "n1", Status: NodeRelDeployed}); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("缺失发布应 404，got %v", err)
	}
	if _, err := s.ListNodeResults(ctx, "nope"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("缺失发布列表应 404，got %v", err)
	}
	// nil 防御
	if err := s.SetNodeResult(ctx, "rel-1", "n1", nil); err == nil {
		t.Fatal("nil 结果应报错")
	}
}

// ── S1：部署影子 ───────────────────────────────────────────────────────

// TestMemSetDeployment 影子整值覆盖 + 排序 + 模型缺失 404 + 级联删除。
func TestMemSetDeployment(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()

	if err := s.SetDeployment(ctx, "defect-detector", "n2", DeploymentState{Model: "x", NodeID: "y", ReleaseID: "r1", Version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeployment(ctx, "defect-detector", "n1", DeploymentState{ReleaseID: "r1", Version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	// Model/NodeID/UpdatedAt 由存储填充覆盖（调用方传了也以存储为准）
	ds, err := s.ListDeployments(ctx, "defect-detector")
	if err != nil || len(ds) != 2 {
		t.Fatalf("影子应 2 条，got %+v err=%v", ds, err)
	}
	if ds[0].NodeID != "n1" || ds[1].NodeID != "n2" {
		t.Fatalf("影子应按 NodeID 排序: %+v", ds)
	}
	if ds[0].Model != "defect-detector" || ds[0].UpdatedAt != fixedTime.UnixMilli() || ds[0].ReleaseID != "r1" {
		t.Fatalf("影子字段应被存储填充: %+v", ds[0])
	}
	// 整值覆盖（派生台账 last-writer-wins）
	if err := s.SetDeployment(ctx, "defect-detector", "n1", DeploymentState{ReleaseID: "r2", Version: "v3.0.0"}); err != nil {
		t.Fatal(err)
	}
	ds, _ = s.ListDeployments(ctx, "defect-detector")
	if ds[0].ReleaseID != "r2" || ds[0].Version != "v3.0.0" {
		t.Fatalf("影子应整值覆盖: %+v", ds[0])
	}
	// 模型缺失 → 404
	if err := s.SetDeployment(ctx, "nope", "n1", DeploymentState{}); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("模型缺失应 404，got %v", err)
	}
	if _, err := s.ListDeployments(ctx, "nope"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("列表模型缺失应 404，got %v", err)
	}
	// 级联删除
	if err := s.DeleteModelDeployments(ctx, "defect-detector"); err != nil {
		t.Fatal(err)
	}
	if ds, _ := s.ListDeployments(ctx, "defect-detector"); len(ds) != 0 {
		t.Fatalf("级联删除后应为空: %+v", ds)
	}
	// 内存 Store 其余接口占位行为
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if rev, err := s.LoadAnchored(ctx); err != nil || rev != 0 {
		t.Fatalf("内存 LoadAnchored 应 (0, nil)，got %d %v", rev, err)
	}
	s.StartWatch(ctx)
	if err := s.ReleaseGuard(ctx, "defect-detector"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// ── 并发（-race）───────────────────────────────────────────────────────

// TestMemConcurrentCreateRelease 同模型并发创建发布 → 恰一个成功、其余
// ReleaseConflictError（guard 语义的竞态锚点，设计 §10.1 H3 存储侧）。
func TestMemConcurrentCreateRelease(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, conflict := 0, 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.CreateRelease(ctx, &ModelRelease{
				ID: fmt.Sprintf("rel-%d", i), Model: "defect-detector", Version: "v2.0.0",
				BatchSize: 1, TargetNodes: []string{"n1"},
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrReleaseConflict):
				conflict++
			default:
				t.Errorf("意外错误: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || conflict != 5 {
		t.Fatalf("应恰 1 成功 5 冲突，got ok=%d conflict=%d", ok, conflict)
	}
	// guard 释放（cancel 后）→ 再次创建成功
	rels, err := s.ListReleases(ctx, "defect-detector")
	if err != nil || len(rels) != 1 {
		t.Fatalf("应只有 1 个发布，got %+v err=%v", rels, err)
	}
	if err := s.CancelRelease(ctx, rels[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRelease(ctx, &ModelRelease{ID: "rel-after", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatalf("终态后应可再次发布，got %v", err)
	}
}

// TestMemConcurrentHeadTransitions 并发 head 流转（模拟 cancel/控制器竞争）：
// 两个 goroutine 从 pending 出发——一个 cancel、一个置 running，
// 恰一个成功，另一个读-改-写冲突后重试读新状态（mutate 判决拒绝）。
func TestMemConcurrentHeadTransitions(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()
	if err := s.CreateRelease(ctx, &ModelRelease{ID: "rel-1", Model: "defect-detector",
		Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	// cancel 赢家将 release 置为 canceled；running 尝试方必须看到新状态后
	// 放弃（模拟控制器 claim 的 errNotPending 判决）
	canRun := errors.New("not pending anymore")
	runAttempt := func() error {
		return s.UpdateReleaseHead(ctx, "rel-1", func(r *ModelRelease) error {
			if r.Status != ReleaseStatusPending {
				return canRun
			}
			r.Status = ReleaseStatusRunning
			return nil
		})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	gotCancel, gotPending := false, false
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := s.CancelRelease(ctx, "rel-1")
		mu.Lock()
		defer mu.Unlock()
		gotCancel = err == nil
	}()
	go func() {
		defer wg.Done()
		err := runAttempt()
		mu.Lock()
		defer mu.Unlock()
		gotPending = errors.Is(err, canRun)
	}()
	wg.Wait()
	if !gotCancel && !gotPending {
		t.Fatalf("两个 transition 都失败：cancel=%v run=%v", gotCancel, gotPending)
	}
	r, _ := s.GetRelease(ctx, "rel-1")
	if r.Status != ReleaseStatusCanceled && r.Status != ReleaseStatusRunning {
		t.Fatalf("head 应为二者之一: %+v", r)
	}
}

// TestMemoryGCReleases 验证 v0.8.0（L28）终态发布 GC：保留最近 keep 条
// 终态、删除更旧的终态及其逐节点结果、非终态不删、keep<1 报错。
func TestMemoryGCReleases(t *testing.T) {
	ctx := context.Background()
	now := newFakeNow(fixedTime)
	s := NewMemoryModelStore()
	s.now = now.Now

	// 造一个模型
	if err := s.CreateModel(ctx, &Model{Name: "mnist"}); err != nil {
		t.Fatalf("创建模型失败: %v", err)
	}
	// 造 5 个终态发布（succeeded）+ 1 个在途（pending）。
	// 内存 CreateRelease 的在途检查 = 扫描非终态（guard 键是 etcd 概念，
	// 内存 ReleaseGuard 为 no-op），因此「创建 → 置终态」串行推进，每个
	// 创建时前序已终态、无在途冲突。
	createRelease := func(id string) {
		now.mu.Lock()
		now.t = now.t.Add(time.Second)
		now.mu.Unlock()
		if err := s.CreateRelease(ctx, &ModelRelease{
			ID: id, Model: "mnist", Version: "v1.0.0",
			Status: ReleaseStatusPending, TargetNodes: []string{"node-a"},
		}); err != nil {
			t.Fatalf("创建发布 %s 失败: %v", id, err)
		}
	}
	setStatus := func(id string, status ReleaseStatus) {
		if err := s.UpdateReleaseHead(ctx, id, func(cur *ModelRelease) error {
			cur.Status = status
			return nil
		}); err != nil {
			t.Fatalf("置 %s 状态 %s 失败: %v", id, status, err)
		}
	}
	for i := 1; i <= 5; i++ {
		createRelease(fmt.Sprintf("r%d", i))
		setStatus(fmt.Sprintf("r%d", i), ReleaseStatusSucceeded)
	}
	createRelease("inflight") // 保持在途（pending），验证 GC 不删非终态

	// 给 r1/r2 造逐节点结果（验证级联删除）
	_ = s.SetNodeResult(ctx, "r1", "node-a", &NodeReleaseResult{NodeID: "node-a", Status: NodeRelDeployed})
	_ = s.SetNodeResult(ctx, "r2", "node-a", &NodeReleaseResult{NodeID: "node-a", Status: NodeRelDeployed})

	// keep=3 → 应删 r1、r2（最早两条终态），保留 r3/r4/r5 + inflight + running
	removed, err := s.GCReleases(ctx, "mnist", 3)
	if err != nil {
		t.Fatalf("GCReleases 失败: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed=%d，期望 2", removed)
	}
	for _, gone := range []string{"r1", "r2"} {
		if _, err := s.GetRelease(ctx, gone); !errors.Is(err, ErrReleaseNotFound) {
			t.Errorf("%s 应被 GC，实际 err=%v", gone, err)
		}
	}
	for _, keep := range []string{"r3", "r4", "r5", "inflight"} {
		if _, err := s.GetRelease(ctx, keep); err != nil {
			t.Errorf("%s 应保留，实际 err=%v", keep, err)
		}
	}
	// 逐节点结果级联删除：release 已删 → ListNodeResults 返回 ErrReleaseNotFound
	// （或空列表），两者皆证明子键已随主键清理
	if rs, err := s.ListNodeResults(ctx, "r1"); !errors.Is(err, ErrReleaseNotFound) && len(rs) != 0 {
		t.Errorf("r1 逐节点结果应被级联删除，err=%v len=%d", err, len(rs))
	}

	// keep<1 报错
	if _, err := s.GCReleases(ctx, "mnist", 0); err == nil {
		t.Error("keep=0 应报错")
	}
	// 幂等：再跑一次（只剩 3 条终态）→ 0 删除
	if removed, err := s.GCReleases(ctx, "mnist", 3); err != nil || removed != 0 {
		t.Errorf("二次 GC 应 0 删除，removed=%d err=%v", removed, err)
	}
}

// ── v0.13.0（B）：DeleteModel 在 GC 开启下级联清理该模型全部终态发布 ──────

// TestMemDeleteModelReleaseGCOff 默认（GC-off）→ 删除模型保留终态发布（L31 审计口径回归）。
func TestMemDeleteModelReleaseGCOff(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryModelStore() // 缺省 = GC 关闭
	s.now = func() time.Time { return fixedTime }
	mustFinishRelease(t, s, "mnist", "rel-1", ReleaseStatusSucceeded)
	mustFinishRelease(t, s, "mnist", "rel-2", ReleaseStatusFailed)
	if err := s.DeleteModel(ctx, "mnist"); err != nil {
		t.Fatalf("删除模型失败: %v", err)
	}
	for _, id := range []string{"rel-1", "rel-2"} {
		if _, err := s.GetRelease(ctx, id); err != nil {
			t.Errorf("GC-off 下 %s 应保留（L31 审计口径），got err=%v", id, err)
		}
	}
	// 无关模型发布不受影响
	if _, err := s.GetRelease(ctx, "rel-1"); err != nil {
		t.Errorf("rel-1 应保留: %v", err)
	}
}

// TestMemDeleteModelReleaseGCOn GC 开启 → 删除模型全清该模型终态发布（头+逐节点）。
func TestMemDeleteModelReleaseGCOn(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryModelStore(WithReleaseGC(true, 100))
	s.now = func() time.Time { return fixedTime }
	mustFinishRelease(t, s, "mnist", "rel-1", ReleaseStatusSucceeded)
	mustFinishRelease(t, s, "mnist", "rel-2", ReleaseStatusFailed)
	_ = s.SetNodeResult(ctx, "rel-1", "node-a", &NodeReleaseResult{NodeID: "node-a", Status: NodeRelDeployed})
	if err := s.DeleteModel(ctx, "mnist"); err != nil {
		t.Fatalf("删除模型失败: %v", err)
	}
	for _, id := range []string{"rel-1", "rel-2"} {
		if _, err := s.GetRelease(ctx, id); !errors.Is(err, ErrReleaseNotFound) {
			t.Errorf("GC-on 下 %s 应被级联清理，got err=%v", id, err)
		}
		if _, err := s.GetNodeResult(ctx, id, "node-a"); !errors.Is(err, ErrReleaseNotFound) {
			t.Errorf("%s 逐节点结果应清空，got err=%v", id, err)
		}
	}
	// 模型本身已删
	if _, err := s.GetModel(ctx, "mnist"); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("模型应 404，got %v", err)
	}
}

// TestMemDeleteModelReleaseGCOnInFlight 在途发布仍 409 拒绝（GC-on 前置守卫不变）。
func TestMemDeleteModelReleaseGCOnInFlight(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryModelStore(WithReleaseGC(true, 100))
	s.now = func() time.Time { return fixedTime }
	if err := s.CreateModel(ctx, &Model{Name: "mnist"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateVersion(ctx, &ModelVersion{Model: "mnist", Version: "v1", Mirror: "reg/x:v1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRelease(ctx, &ModelRelease{ID: "rel-inflight", Model: "mnist",
		Version: "v1", BatchSize: 1, TargetNodes: []string{"n1"}}); err != nil {
		t.Fatal(err)
	}
	err := s.DeleteModel(ctx, "mnist")
	var rce *ReleaseConflictError
	if !errors.As(err, &rce) || rce.InFlight != "rel-inflight" {
		t.Fatalf("在途发布应拒绝删除并带在途 ID，got %v", err)
	}
}

// mustFinishRelease 造一个模型 + 一条终态发布（helper，v0.13.0 B 测试）。
func mustFinishRelease(t *testing.T, s *MemoryModelStore, model, id string, status ReleaseStatus) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetModel(ctx, model); err != nil {
		if err := s.CreateModel(ctx, &Model{Name: model}); err != nil {
			t.Fatalf("创建模型失败: %v", err)
		}
	}
	if _, err := s.GetVersion(ctx, model, "v1"); err != nil {
		if err := s.CreateVersion(ctx, &ModelVersion{Model: model, Version: "v1", Mirror: "reg/x:v1"}); err != nil {
			t.Fatalf("创建版本失败: %v", err)
		}
	}
	if err := s.CreateRelease(ctx, &ModelRelease{ID: id, Model: model,
		Version: "v1", BatchSize: 1, TargetNodes: []string{"node-a"}}); err != nil {
		t.Fatalf("创建发布 %s 失败: %v", id, err)
	}
	if err := s.UpdateReleaseHead(ctx, id, func(cur *ModelRelease) error {
		cur.Status = status
		return nil
	}); err != nil {
		t.Fatalf("置 %s 终态失败: %v", id, err)
	}
}
