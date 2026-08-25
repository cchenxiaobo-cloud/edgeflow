package modelrelease

// 存储路径定向用例（设计 §10.1 S4 的跨包对拍 + 审稿 P8 部分项；用
// fakeKV + EtcdModelStore 验证控制器消费到的存储语义）：
//
//   - P8-① guard 自愈（主线裁决 D3）：孤儿 guard（指向不存在的 release）
//     → CreateRelease 重试自愈；指向在途 release → ErrReleaseConflict
//     带在途 ID；指向终态 release（防御）→ 自愈清理；
//   - P8-③ cancel-before-claim 的 guard 真实删除（fakeKV 可断言键）；
//   - P8-⑤ 崩溃点②（head 已 succeeded、guard 未删）→ ≤1 扫描周期 guard
//     清除，同模型可再次发布。
//
// 控制器 / 存储职责边界：D3/D7 自愈为存储层实现（modelrepo.CreateRelease），
// 本文件以黑盒行为锁定，防止未来 refactor 回归。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"edgeflow/cloud/pkg/modelrepo"
)

// newEtcdFixture 构造 fakeKV + EtcdModelStore + 模型/版本（v1 archived、
// v2 active），返回可供控制器使用的完整环境。
type etcdFixture struct {
	kv     *fakeKV
	store  *modelrepo.EtcdModelStore
	model  string
	relID  string
	clk    *fakeClock
	deploy *fakeDeployer
	ctrl   *Controller
}

func newEtcdFixture(t *testing.T, nodes []string, batchSize int, pause int64, failFast bool) *etcdFixture {
	t.Helper()
	kv := newFakeKV()
	store, err := modelrepo.NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateModel(ctx, &modelrepo.Model{Name: "defect-detector", Type: "detection"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1.0.0", "v2.0.0"} {
		if err := store.CreateVersion(ctx, &modelrepo.ModelVersion{
			Model: "defect-detector", Version: v, Mirror: "reg/x:" + v,
			Sha256: "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ActivateVersion(ctx, "defect-detector", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateVersion(ctx, "defect-detector", "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	rel := &modelrepo.ModelRelease{
		ID: "rel-1", Model: "defect-detector", Version: "v2.0.0",
		Target:      modelrepo.ReleaseTarget{Type: "nodeIDs", NodeIDs: nodes},
		TargetNodes: nodes, BatchSize: batchSize, PauseBetween: pause,
		FailFast: failFast, PrevActive: "v1.0.0",
	}
	if err := store.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	for i, n := range nodes {
		if err := store.SetNodeResult(ctx, rel.ID, n, &modelrepo.NodeReleaseResult{
			NodeID: n, Status: modelrepo.NodeRelPending, Batch: BatchNumber(i, batchSize),
		}); err != nil {
			t.Fatal(err)
		}
	}
	clk := newFakeClock(time.UnixMilli(1787000000000))
	deploy := &fakeDeployer{errs: make(map[string]error)}
	ctrl, err := NewController(store, deploy, newFakeLock(), Options{
		ScanInterval: time.Second, LockTTL: 60 * time.Second, Now: clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &etcdFixture{kv: kv, store: store, model: "defect-detector", relID: rel.ID,
		clk: clk, deploy: deploy, ctrl: ctrl}
}

// TestP8_guard自愈_孤儿（D3） 孤儿 guard（guard 写后崩溃、release 键从未
// 写入）→ CreateRelease 自愈：按值删 guard → 重试创建成功。
func TestP8_guard自愈_孤儿(t *testing.T) {
	kv := newFakeKV()
	store, err := modelrepo.NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateModel(ctx, &modelrepo.Model{Name: "m2"}); err != nil {
		t.Fatal(err)
	}
	// 模拟孤儿 guard：值指向不存在的 release（lock 键从未创建——设计
	// §5.4 的"lock 过期"陈旧判据在此不自洽，D3 裁决改为读 release 键）
	guardKey := modelrepo.KeyGuardsPrefix + "m2"
	if err := kv.Put(ctx, guardKey, []byte("orphan-id")); err != nil {
		t.Fatal(err)
	}
	rel := &modelrepo.ModelRelease{ID: "rel-3", Model: "m2", Version: "v1", BatchSize: 1, TargetNodes: []string{"a"}}
	if err := store.CreateRelease(ctx, rel); err != nil {
		t.Fatalf("孤儿 guard 应自愈后创建成功，got %v", err)
	}
	// guard 已被新发布持有
	if v, _ := kv.Get(ctx, guardKey); string(v) != "rel-3" {
		t.Fatalf("guard 应指向 rel-3，got %q", v)
	}
	// release 键已写入
	if v, _ := kv.Get(ctx, modelrepo.KeyReleasesPrefix+"rel-3"); v == nil {
		t.Fatal("release 头键应已写入")
	}
}

// TestP8_guard_指向在途_409 guard 指向有效在途 release → ErrReleaseConflict
// 带在途 ID（409 响应含 releaseID）。
func TestP8_guard_指向在途_409(t *testing.T) {
	fx := newEtcdFixture(t, []string{"a"}, 1, 0, true)
	ctx := context.Background()
	rel2 := &modelrepo.ModelRelease{ID: "rel-2", Model: fx.model, Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"a"}}
	err := fx.store.CreateRelease(ctx, rel2)
	var rce *modelrepo.ReleaseConflictError
	if !errors.As(err, &rce) || rce.InFlight != "rel-1" {
		t.Fatalf("应返回 ReleaseConflictError 带在途 ID rel-1，got %v", err)
	}
	if !errors.Is(err, modelrepo.ErrReleaseConflict) {
		t.Fatalf("errors.Is(ErrReleaseConflict) 应成立，got %v", err)
	}
}

// TestP8_guard自愈_终态残留（防御） guard 指向已终态 release（终态后 guard
// 未清）→ 自愈清理重试创建。
func TestP8_guard自愈_终态残留(t *testing.T) {
	kv := newFakeKV()
	store, err := modelrepo.NewEtcdModelStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateModel(ctx, &modelrepo.Model{Name: "m3"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRelease(ctx, &modelrepo.ModelRelease{ID: "rel-4", Model: "m3", Version: "v1", BatchSize: 1, TargetNodes: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	// 手工把 head 置为终态（模拟"head succeeded 但 guard 未删"的残留）
	raw, err := kv.Get(ctx, modelrepo.KeyReleasesPrefix+"rel-4")
	if err != nil || raw == nil {
		t.Fatal("release 头键缺失")
	}
	var hdr modelrepo.ModelRelease
	if err := json.Unmarshal(raw, &hdr); err != nil {
		t.Fatal(err)
	}
	hdr.Status = modelrepo.ReleaseStatusSucceeded
	data, _ := json.Marshal(&hdr)
	if err := kv.Put(ctx, modelrepo.KeyReleasesPrefix+"rel-4", data); err != nil {
		t.Fatal(err)
	}
	// 新创建应自愈（终态残留 guard 视为陈旧）
	if err := store.CreateRelease(ctx, &modelrepo.ModelRelease{ID: "rel-5", Model: "m3", Version: "v1", BatchSize: 1, TargetNodes: []string{"a"}}); err != nil {
		t.Fatalf("终态残留 guard 应自愈，got %v", err)
	}
	if v, _ := kv.Get(ctx, modelrepo.KeyGuardsPrefix+"m3"); string(v) != "rel-5" {
		t.Fatalf("guard 应指向 rel-5，got %q", v)
	}
}

// TestP8_cancel_before_claim_guard删除（etcd 路径） pending 时 cancel →
// ≤1 扫描周期 perNode 全 skipped + guard 键真实删除（同模型可再次发布）。
func TestP8_cancel_before_claim_guard删除(t *testing.T) {
	fx := newEtcdFixture(t, []string{"a", "b"}, 1, 0, true)
	ctx := context.Background()
	if err := fx.store.CancelRelease(ctx, fx.relID); err != nil {
		t.Fatal(err)
	}
	guardKey := modelrepo.KeyGuardsPrefix + fx.model
	if !fx.kv.hasKey(guardKey) {
		t.Fatal("cancel 前 guard 应存在")
	}
	fx.ctrl.scanOnce(ctx) // 轮 1：skipped 补齐
	for _, n := range []string{"a", "b"} {
		nr, err := fx.store.GetNodeResult(ctx, fx.relID, n)
		if err != nil || nr == nil || nr.Status != modelrepo.NodeRelSkipped {
			t.Fatalf("%s 应 skipped，got %+v err=%v", n, nr, err)
		}
	}
	if fx.kv.hasKey(guardKey) {
		t.Fatal("cancel 收敛后 guard 仍存在（轮 1 内应已释放——终态判定在每轮扫描）")
	}
	// 同模型可再次发布（fakeKV 断言 guard 已被删）
	rel2 := &modelrepo.ModelRelease{ID: "rel-2", Model: fx.model, Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"a"}}
	if err := fx.store.CreateRelease(ctx, rel2); err != nil {
		t.Fatalf("cancel 终态后同模型应可再次发布，got %v", err)
	}
}

// TestP8_崩溃点_head后guard前（P8-⑤②） head 已 succeeded、guard 未删 →
// 下一轮扫描 guard 清除；同模型可再次发布。
func TestP8_崩溃点_head后guard前(t *testing.T) {
	fx := newEtcdFixture(t, []string{"a", "b"}, 1, 0, true)
	ctx := context.Background()
	// 跑到 succeeded（guard 尚未删除——releaseGuard 在终态**下一轮**执行）
	for i := 0; i < 10; i++ {
		fx.ctrl.scanOnce(ctx)
		if r, _ := fx.store.GetRelease(ctx, fx.relID); r.Status == modelrepo.ReleaseStatusSucceeded {
			break
		}
	}
	guardKey := modelrepo.KeyGuardsPrefix + fx.model
	if !fx.kv.hasKey(guardKey) {
		t.Fatal("succeeded 后 guard 应先于释放流程仍然存在（崩溃点② 前提）")
	}
	// 崩溃：新副本接管 → 终态释放 guard（≤1 扫描周期）
	fx.ctrl.scanOnce(ctx)
	if fx.kv.hasKey(guardKey) {
		t.Fatal("succeeded 后 guard 应在 ≤1 扫描周期内被清除")
	}
	if err := fx.store.CreateRelease(ctx, &modelrepo.ModelRelease{
		ID: "rel-2", Model: fx.model, Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"a"},
	}); err != nil {
		t.Fatalf("guard 清除后同模型应可再次发布，got %v", err)
	}
}
