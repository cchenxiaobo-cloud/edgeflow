package modelrelease

// 控制器单测（设计 §10.1 A3/C1/C2/C3 + 审稿 P8 定向用例，WBS-9）：
//
//	A3 调度推进（fake clock + fake deployer）：正常批次 → NextBatchAt =
//	    now+pause；failFast=true 单失败中止（剩余 skipped、head failed、
//	    guard 释放）；failFast=false 全量跑完有失败 → failed；全部成功 →
//	    succeeded（D8 断言通过）。
//	C1 认领：pending→running CAS；锁被占 → 跳过；锁到期 → 接管（perNode
//	    deployed 跳过、从首 pending 批次继续、NextBatchAt 尊重）；崩溃点
//	    续跑（perNode 全写后 head 前 → 接管后直接 succeeded 不重发）。
//	C2 cancel：running 中取消 → 批次边界停止 → 剩余 skipped；终态 cancel
//	    → ErrReleaseTerminal（409 语义）。
//	C3 rollback：逆序批次（批序倒置、批内顺序不变、忽略 pause）；部分
//	    失败仍 rolled_back（L24）；无 PrevActive → 422；version≠active
//	    → 409。
//	P8 定向：① guard 自愈（etcd 存储路径，controller 无关——放
//	    storekit 相关测试）；② 回滚 TOCTOU×2（执行期 active 被接管 /
//	    PrevActive 被删 → 中止终态）；③ cancel-before-claim（pending 时
//	    cancel → guard 释放 + perNode 全 skipped）；⑤ 崩溃点 head 后 guard
//	    前（guard ≤1 扫描周期清除）。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/modelrepo"
)

// ── fakes ──────────────────────────────────────────────────────────────

// fakeClock 是可注入时钟（推进 pause 计时；NewController Options.Now）。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// deployCall 是一次部署调用的记录。
type deployCall struct {
	nodeID    string
	releaseID string
	version   string
}

// fakeDeployer 是部署执行器 fake：记录调用、按 nodeID 或前 N 次可控失败。
type fakeDeployer struct {
	mu        sync.Mutex
	calls     []deployCall
	errs      map[string]error // nodeID → 部署错误（前向/回滚共用；测试按阶段清空）
	failFirst int              // >0：前 N 次调用失败（模拟回滚中部分失败）
}

func (f *fakeDeployer) DeployVersion(_ context.Context, nodeID, releaseID string, ver *modelrepo.ModelVersion) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, deployCall{nodeID: nodeID, releaseID: releaseID, version: ver.Version})
	if f.failFirst > 0 {
		f.failFirst--
		return errors.New("simulated deploy failure")
	}
	if err, ok := f.errs[nodeID]; ok {
		return err
	}
	return nil
}

func (f *fakeDeployer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDeployer) versions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.version)
	}
	return out
}

func (f *fakeDeployer) nodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.nodeID)
	}
	return out
}

// fakeLock 是领跑锁 fake：held=他副本持有；fail=后端故障；grants=成功锁键。
type fakeLock struct {
	mu     sync.Mutex
	held   map[string]bool
	fail   error
	grants []string
}

func newFakeLock() *fakeLock { return &fakeLock{held: make(map[string]bool)} }

func (f *fakeLock) TryAcquire(_ context.Context, key string, _ []byte, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return false, f.fail
	}
	if f.held[key] {
		return false, nil
	}
	f.grants = append(f.grants, key)
	return true, nil
}

func (f *fakeLock) grantCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.grants)
}

// ── fixture ────────────────────────────────────────────────────────────

// ctrlFixture 是一次控制器测试的最小世界：内存 store + 模型/版本/发布
// （含 perNode pending 预写，模拟 API 创建流程第 6 步）。
type ctrlFixture struct {
	store  modelrepo.ModelStore
	deploy *fakeDeployer
	lock   *fakeLock
	clk    *fakeClock
	ctrl   *Controller
	model  string
	vPrev  string
	vNew   string
	relID  string
}

const testModel = "defect-detector"

// newCtrlFixture 创建模型（v1 曾 active、v2 当前 active）与发布。
func newCtrlFixture(t *testing.T, targetNodes []string, batchSize int, pause int64, failFast bool) *ctrlFixture {
	t.Helper()
	store := modelrepo.NewMemoryModelStore()
	ctx := context.Background()
	if err := store.CreateModel(ctx, &modelrepo.Model{Name: testModel, Type: "detection"}); err != nil {
		t.Fatal(err)
	}
	mkVer := func(v string) *modelrepo.ModelVersion {
		return &modelrepo.ModelVersion{
			Model: testModel, Version: v,
			Mirror: "registry.example.com/models/" + testModel + ":" + v,
			Sha256: "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		}
	}
	if err := store.CreateVersion(ctx, mkVer("v1.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateVersion(ctx, testModel, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateVersion(ctx, mkVer("v2.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateVersion(ctx, testModel, "v2.0.0"); err != nil {
		t.Fatal(err)
	}

	rel := &modelrepo.ModelRelease{
		ID:           "rel-1",
		Model:        testModel,
		Version:      "v2.0.0",
		Target:       modelrepo.ReleaseTarget{Type: "nodeIDs", NodeIDs: targetNodes},
		TargetNodes:  targetNodes,
		BatchSize:    batchSize,
		PauseBetween: pause,
		FailFast:     failFast,
		PrevActive:   "v1.0.0",
	}
	if err := store.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	// perNode pending 预写（含 Batch 序号预分配，设计 §5.2 step6）
	for i, n := range targetNodes {
		if err := store.SetNodeResult(ctx, rel.ID, n, &modelrepo.NodeReleaseResult{
			NodeID: n, Status: modelrepo.NodeRelPending, Batch: BatchNumber(i, batchSize),
		}); err != nil {
			t.Fatal(err)
		}
	}

	fx := &ctrlFixture{
		store:  store,
		deploy: &fakeDeployer{errs: make(map[string]error)},
		lock:   newFakeLock(),
		clk:    newFakeClock(time.UnixMilli(1787000000000)),
		model:  testModel,
		vPrev:  "v1.0.0",
		vNew:   "v2.0.0",
		relID:  rel.ID,
	}
	ctrl, err := NewController(store, fx.deploy, fx.lock, Options{
		ScanInterval: time.Second,
		LockTTL:      60 * time.Second,
		Now:          fx.clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.ctrl = ctrl
	return fx
}

// release 便捷读当前 head。
func (fx *ctrlFixture) release(t *testing.T) *modelrepo.ModelRelease {
	t.Helper()
	r, err := fx.store.GetRelease(context.Background(), fx.relID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// nodeResult 便捷读 perNode。
func (fx *ctrlFixture) nodeResult(t *testing.T, nodeID string) *modelrepo.NodeReleaseResult {
	t.Helper()
	nr, err := fx.store.GetNodeResult(context.Background(), fx.relID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if nr == nil {
		t.Fatalf("perNode %s 缺失", nodeID)
	}
	return nr
}

// runToSucceeded 全速跑到 succeeded（每次扫提前推进时钟，覆盖 pause
// ≤60s 的 fixture——pause=0 时一轮一批且 Advance 无副作用）。
func (fx *ctrlFixture) runToSucceeded(t *testing.T) {
	t.Helper()
	for i := 0; i < 20; i++ {
		fx.clk.Advance(60 * time.Second)
		fx.ctrl.scanOnce(context.Background())
		if r := fx.release(t); r.Status == modelrepo.ReleaseStatusSucceeded {
			return
		}
	}
	t.Fatalf("20 轮内未到 succeeded（head=%s）", fx.release(t).Status)
}

// ── A3：调度推进 ───────────────────────────────────────────────────────

// TestA3_正常批次_NextBatchAt与succeeded 批间节奏：批完成 → NextBatchAt=
// now+pause（跨轮生效）；全部完成 → succeeded（D8）。
func TestA3_正常批次_NextBatchAt与succeeded(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b", "c"}, 1, 1000, true)
	base := fx.clk.Now().UnixMilli()

	fx.ctrl.scanOnce(context.Background()) // 认领 + 批次 1
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusRunning {
		t.Fatalf("第 1 轮后应 running，got %s", r.Status)
	}
	if r.NextBatchAt != base+1000 {
		t.Fatalf("NextBatchAt = %d, want %d（now+pause）", r.NextBatchAt, base+1000)
	}
	if r.StartedAt != base {
		t.Fatalf("StartedAt 应写入认领时刻，got %d", r.StartedAt)
	}
	if fx.deploy.count() != 1 || fx.deploy.nodes()[0] != "a" {
		t.Fatalf("批次 1 应只部署 a，got %v", fx.deploy.nodes())
	}

	fx.ctrl.scanOnce(context.Background()) // 未到 NextBatchAt：暂停中
	if fx.deploy.count() != 1 {
		t.Fatalf("暂停期间不应推进批次，got %d 次部署", fx.deploy.count())
	}

	fx.clk.Advance(1000 * time.Millisecond)
	fx.ctrl.scanOnce(context.Background()) // 批次 2
	if fx.deploy.count() != 2 || fx.deploy.nodes()[1] != "b" {
		t.Fatalf("第 2 批应部署 b，got %v", fx.deploy.nodes())
	}

	fx.clk.Advance(1000 * time.Millisecond)
	fx.ctrl.scanOnce(context.Background()) // 批次 3（最后一批）→ succeeded
	r = fx.release(t)
	if r.Status != modelrepo.ReleaseStatusSucceeded {
		t.Fatalf("全部 deployed 后应 succeeded，got %s（reason=%q）", r.Status, r.FailureReason)
	}
	if r.FinishedAt == 0 {
		t.Fatal("succeeded 应写 FinishedAt")
	}
	if fx.deploy.count() != 3 {
		t.Fatalf("应恰好 3 次部署，got %d", fx.deploy.count())
	}
	for _, n := range []string{"a", "b", "c"} {
		if nr := fx.nodeResult(t, n); nr.Status != modelrepo.NodeRelDeployed || nr.Version != "v2.0.0" {
			t.Fatalf("perNode %s = %+v, want deployed/v2.0.0", n, nr)
		}
	}
}

// TestA3_failFast中止 单节点失败立即中止：本批剩余与后续全部 skipped、
// head=failed（含 reason）、guard 下一轮释放。
func TestA3_failFast中止(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b", "c", "d"}, 2, 0, true)
	fx.deploy.errs["b"] = cloudhub.ErrNodeOffline

	fx.ctrl.scanOnce(context.Background()) // 批次 1：[a,b] → a 成功、b 失败 → fail-fast
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusFailed {
		t.Fatalf("fail-fast 应置 failed，got %s", r.Status)
	}
	if !strings.Contains(r.FailureReason, "fail fast: node b") {
		t.Fatalf("FailureReason = %q, want 含 fail fast: node b", r.FailureReason)
	}
	if nr := fx.nodeResult(t, "a"); nr.Status != modelrepo.NodeRelDeployed {
		t.Fatalf("a 应 deployed，got %s", nr.Status)
	}
	if nr := fx.nodeResult(t, "b"); nr.Status != modelrepo.NodeRelFailed || nr.Reason != "node offline or not registered" {
		t.Fatalf("b 应 failed + 机器可读 reason，got %+v", nr)
	}
	for _, n := range []string{"c", "d"} {
		if nr := fx.nodeResult(t, n); nr.Status != modelrepo.NodeRelSkipped {
			t.Fatalf("%s 应 skipped，got %s", n, nr.Status)
		}
	}
	// 终态后：同模型可再次创建发布（guard 已释放——内存实现 inFlight 检查）
	fx.ctrl.scanOnce(context.Background()) // 终态 → releaseGuard（内存空操作）
	if err := fx.store.CreateRelease(context.Background(), &modelrepo.ModelRelease{
		ID: "rel-2", Model: testModel, Version: "v2.0.0", BatchSize: 1,
		TargetNodes: []string{"a"},
	}); err != nil {
		t.Fatalf("fail-fast 终态后同模型应可再次发布，got %v", err)
	}
}

// TestA3_failFastFalse_有失败终态 不中止跑完全部批次，有失败 → failed。
func TestA3_failFastFalse_有失败终态(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b", "c"}, 1, 0, false)
	fx.deploy.errs["b"] = cloudhub.ErrAckTimeout

	fx.ctrl.scanOnce(context.Background()) // a
	fx.ctrl.scanOnce(context.Background()) // b 失败（继续）
	fx.ctrl.scanOnce(context.Background()) // c
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusFailed {
		t.Fatalf("failFast=false 有失败应 failed，got %s", r.Status)
	}
	if !strings.Contains(r.FailureReason, "1 node(s) failed") {
		t.Fatalf("FailureReason = %q", r.FailureReason)
	}
	if fx.deploy.count() != 3 {
		t.Fatalf("failFast=false 应跑完全部节点，got %d 次部署", fx.deploy.count())
	}
	if nr := fx.nodeResult(t, "b"); nr.Status != modelrepo.NodeRelFailed || nr.Reason != "ack timeout after retries" {
		t.Fatalf("b 应 failed + timeout 文案，got %+v", nr)
	}
	if nr := fx.nodeResult(t, "c"); nr.Status != modelrepo.NodeRelDeployed {
		t.Fatalf("c 应 deployed，got %s", nr.Status)
	}
}

// TestA3_批内串行D6 批次内逐节点串行（batchSize 不提供并发度；D6 口径
// 由部署调用有序性断言——单 goroutine 天然串行，防回归并发改造）。
func TestA3_批内串行D6(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b"}, 2, 0, true)
	fx.ctrl.scanOnce(context.Background())
	if got := fx.deploy.nodes(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("批内应按 TargetNodes 顺序串行部署，got %v", got)
	}
}

// ── C1：认领与接管 ─────────────────────────────────────────────────────

// TestC1_认领与锁被占 锁被占用 → 不推进（仅认领 CAS）；释放后 → 执行。
func TestC1_认领与锁被占(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b"}, 1, 0, true)
	lockKey := modelrepo.ReleaseLockKey(fx.relID)
	fx.lock.held[lockKey] = true

	fx.ctrl.scanOnce(context.Background())
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusRunning {
		t.Fatalf("认领 CAS 应把 pending→running（他副本可接管执行），got %s", r.Status)
	}
	if fx.deploy.count() != 0 {
		t.Fatalf("锁被占时本副本不应推进，got %d 次部署", fx.deploy.count())
	}

	delete(fx.lock.held, lockKey) // 他副本完成/退出 → 本副本可推进
	fx.ctrl.scanOnce(context.Background())
	if fx.deploy.count() != 1 {
		t.Fatalf("锁释放后应执行，got %d 次部署", fx.deploy.count())
	}
	// fakeLock 只在成功时记录 grant：第一次（held）失败不计，第二次成功
	// 计 1 次 → grants=1
	if fx.lock.grantCount() != 1 {
		t.Fatalf("grant 次数 = %d, want 1（仅释放后成功的那次）", fx.lock.grantCount())
	}
}

// TestC1_锁后端故障 后端错误 → 本轮跳过（不推进、不 panic），下轮恢复。
func TestC1_锁后端故障(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a"}, 1, 0, true)
	fx.lock.fail = errors.New("etcd unreachable")
	fx.ctrl.scanOnce(context.Background())
	if fx.deploy.count() != 0 {
		t.Fatalf("锁故障时不应推进，got %d 次部署", fx.deploy.count())
	}
	fx.lock.fail = nil
	fx.ctrl.scanOnce(context.Background())
	if fx.deploy.count() != 1 {
		t.Fatalf("锁恢复后应推进，got %d 次部署", fx.deploy.count())
	}
}

// TestC1_接管续跑 新副本（无 active 状态）认领 running release：已 deployed
// 批次跳过、从首个 pending 批次继续、NextBatchAt 尊重（不重复计 pause）。
func TestC1_接管续跑(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b", "c", "d", "e"}, 2, 1000, true)
	// 副本 A：批次 1、2 完成（a,b,c,d deployed；NextBatchAt 已持久化）
	fx.ctrl.scanOnce(context.Background())
	fx.clk.Advance(1000 * time.Millisecond)
	fx.ctrl.scanOnce(context.Background())
	if fx.deploy.count() != 4 {
		t.Fatalf("副本 A 应部署 4 节点，got %d", fx.deploy.count())
	}
	// 副本 A 崩溃（销毁 controller；锁租约到期由 fakeLock 无状态模拟：
	// 新副本直接 grant 即接管）
	ctrlB, err := NewController(fx.store, &fakeDeployer{errs: make(map[string]error)}, newFakeLock(), Options{
		ScanInterval: time.Second, LockTTL: 60 * time.Second, Now: fx.clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	deployB := ctrlB.deploy.(*fakeDeployer)
	// 未到 NextBatchAt：接管后尊重节奏，不提前开跑
	ctrlB.scanOnce(context.Background())
	if deployB.count() != 0 {
		t.Fatalf("接管后未到 NextBatchAt 不应推进（pause 尊重），got %d", deployB.count())
	}
	// 到达节奏：从批次 3（首 pending）继续，只部署 e
	fx.clk.Advance(1000 * time.Millisecond)
	ctrlB.scanOnce(context.Background())
	if got := deployB.nodes(); !reflect.DeepEqual(got, []string{"e"}) {
		t.Fatalf("接管应只部署 e（deployed 跳过），got %v", got)
	}
	r, _ := fx.store.GetRelease(context.Background(), fx.relID)
	if r.Status != modelrepo.ReleaseStatusSucceeded {
		t.Fatalf("接管完成后应 succeeded，got %s", r.Status)
	}
}

// TestC1_崩溃点续跑_perNode全写后head前 精确崩溃点（P8-⑤ ①）：perNode
// 全部 deployed、head 仍 running → 接管后直接 succeeded 且不重发。
func TestC1_崩溃点续跑_perNode全写后head前(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b"}, 1, 0, true)
	ctx := context.Background()
	// 模拟崩溃前状态：perNode 全部 deployed（手工写穿），head 仍 running
	if err := fx.store.SetNodeResult(ctx, fx.relID, "a", &modelrepo.NodeReleaseResult{NodeID: "a", Status: modelrepo.NodeRelDeployed, Version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := fx.store.SetNodeResult(ctx, fx.relID, "b", &modelrepo.NodeReleaseResult{NodeID: "b", Status: modelrepo.NodeRelDeployed, Version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	// 新副本接管
	ctrlB, err := NewController(fx.store, &fakeDeployer{errs: make(map[string]error)}, newFakeLock(), Options{
		ScanInterval: time.Second, LockTTL: 60 * time.Second, Now: fx.clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctrlB.scanOnce(context.Background())
	r, _ := fx.store.GetRelease(context.Background(), fx.relID)
	if r.Status != modelrepo.ReleaseStatusSucceeded {
		t.Fatalf("perNode 全写后接管应直接 succeeded 且不重发（got %s reason=%q）", r.Status, r.FailureReason)
	}
	if deployB := ctrlB.deploy.(*fakeDeployer); deployB.count() != 0 {
		t.Fatalf("接管不应重发任何部署，got %v", deployB.nodes())
	}
}

// ── C2：cancel ─────────────────────────────────────────────────────────

// TestC2_cancel_批次边界停止 暂停中取消 → 批次边界停止（本轮不再推），
// 剩余 pending 补写 skipped（≤1 扫描周期，L27）。
func TestC2_cancel_批次边界停止(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b", "c", "d", "e"}, 2, 60000, true)
	fx.ctrl.scanOnce(context.Background()) // 批次 1：[a,b] deployed；NextBatchAt=now+60s
	if fx.deploy.count() != 2 {
		t.Fatalf("批次 1 应部署 2 节点，got %d", fx.deploy.count())
	}
	fx.clk.Advance(5000 * time.Millisecond)
	fx.ctrl.scanOnce(context.Background()) // 暂停中 → 无推进
	if fx.deploy.count() != 2 {
		t.Fatalf("暂停中不应推进，got %d", fx.deploy.count())
	}
	if err := fx.store.CancelRelease(context.Background(), fx.relID); err != nil {
		t.Fatal(err)
	}
	fx.clk.Advance(60000 * time.Millisecond)
	fx.ctrl.scanOnce(context.Background()) // 已到节奏点但 head=canceled → 取消生效 + skipped 补齐
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusCanceled {
		t.Fatalf("head 应保持 canceled，got %s", r.Status)
	}
	if fx.deploy.count() != 2 {
		t.Fatalf("取消后不应再部署，got %d", fx.deploy.count())
	}
	for _, n := range []string{"a", "b"} {
		if nr := fx.nodeResult(t, n); nr.Status != modelrepo.NodeRelDeployed {
			t.Fatalf("已部署节点 %s 应保持 deployed，got %s", n, nr.Status)
		}
	}
	for _, n := range []string{"c", "d", "e"} {
		if nr := fx.nodeResult(t, n); nr.Status != modelrepo.NodeRelSkipped {
			t.Fatalf("剩余节点 %s 应 skipped，got %s", n, nr.Status)
		}
	}
}

// TestC2_终态cancel_409 终态 release cancel → ErrReleaseTerminal（API 409
// 语义；store 层实现，控制器不承担）。
func TestC2_终态cancel_409(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a"}, 1, 0, true)
	fx.runToSucceeded(t)
	err := fx.store.CancelRelease(context.Background(), fx.relID)
	if !errors.Is(err, modelrepo.ErrReleaseTerminal) {
		t.Fatalf("终态 cancel 应 ErrReleaseTerminal，got %v", err)
	}
}

// ── C3：rollback ───────────────────────────────────────────────────────

// TestC3_回滚逆序批次 succeeded 后回滚：逆序逐批（批序倒置、批内顺序
// 不变）、批间不暂停（忽略 PauseBetween=60s）、perNode.Version=PrevActive、
// head=rolled_back（RollbackRequested 清零）。
func TestC3_回滚逆序批次(t *testing.T) {
	fx := newCtrlFixture(t, []string{"n1", "n2", "n3", "n4", "n5", "n6"}, 2, 60000, true)
	fx.runToSucceeded(t)
	if err := fx.store.RequestRollback(context.Background(), fx.relID); err != nil {
		t.Fatal(err)
	}

	fx.ctrl.scanOnce(context.Background()) // 回滚批次 3：[n5,n6]
	fx.ctrl.scanOnce(context.Background()) // 回滚批次 2：[n3,n4]
	fx.ctrl.scanOnce(context.Background()) // 回滚批次 1：[n1,n2] → rolled_back
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusRolledBack {
		t.Fatalf("回滚完成应 rolled_back，got %s（reason=%q）", r.Status, r.FailureReason)
	}
	if r.RollbackRequested {
		t.Fatal("rolled_back 后 RollbackRequested 必须清零（防下轮重跑）")
	}
	// 逆序断言：最后发布的批次先回滚、批内顺序不变
	rollbackCalls := fx.deploy.nodes()[6:] // 前 6 次是前向部署
	want := []string{"n5", "n6", "n3", "n4", "n1", "n2"}
	if !reflect.DeepEqual(rollbackCalls, want) {
		t.Fatalf("回滚逆序批次 = %v, want %v", rollbackCalls, want)
	}
	for _, v := range fx.deploy.versions()[6:] {
		if v != "v1.0.0" {
			t.Fatalf("回滚部署版本应为 PrevActive v1.0.0，got %v", fx.deploy.versions()[6:])
		}
	}
	for _, n := range []string{"n1", "n2", "n3", "n4", "n5", "n6"} {
		nr := fx.nodeResult(t, n)
		if nr.Status != modelrepo.NodeRelDeployed || nr.Version != "v1.0.0" {
			t.Fatalf("perNode %s = %+v, want deployed/v1.0.0", n, nr)
		}
	}
}

// TestC3_回滚部分失败仍rolled_back（L24） 回滚失败节点 perNode=failed +
// reason（Version=PrevActive），head 仍 rolled_back。
func TestC3_回滚部分失败仍rolled_back(t *testing.T) {
	fx := newCtrlFixture(t, []string{"n1", "n2"}, 1, 0, true)
	fx.runToSucceeded(t)
	if err := fx.store.RequestRollback(context.Background(), fx.relID); err != nil {
		t.Fatal(err)
	}
	fx.deploy.failFirst = 1 // 第一次回滚部署（逆序：n2）失败
	fx.ctrl.scanOnce(context.Background())
	fx.ctrl.scanOnce(context.Background())
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusRolledBack {
		t.Fatalf("部分失败仍应 rolled_back（L24），got %s", r.Status)
	}
	if nr := fx.nodeResult(t, "n2"); nr.Status != modelrepo.NodeRelFailed || nr.Version != "v1.0.0" || nr.Reason == "" {
		t.Fatalf("回滚失败节点应 failed + Version=PrevActive + reason，got %+v", nr)
	}
	if nr := fx.nodeResult(t, "n1"); nr.Status != modelrepo.NodeRelDeployed || nr.Version != "v1.0.0" {
		t.Fatalf("回滚成功节点应 deployed/v1.0.0，got %+v", nr)
	}
}

// TestC3_无PrevActive_422 release 无 PrevActive → RequestRollback 422 语义。
func TestC3_无PrevActive_422(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a"}, 1, 0, true)
	ctx := context.Background()
	// 先让 fixture 的 rel-1 终态（guard 释放），否则同模型 CreateRelease 409
	fx.runToSucceeded(t)
	rel := &modelrepo.ModelRelease{ID: "rel-no-prev", Model: testModel, Version: "v2.0.0",
		BatchSize: 1, TargetNodes: []string{"a"}} // PrevActive=""
	if err := fx.store.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if err := fx.store.SetNodeResult(ctx, rel.ID, "a", &modelrepo.NodeReleaseResult{NodeID: "a", Status: modelrepo.NodeRelPending}); err != nil {
		t.Fatal(err)
	}
	// 先跑到 succeeded（保证状态合法）
	ctrl2, err := NewController(fx.store, &fakeDeployer{errs: make(map[string]error)}, newFakeLock(), Options{
		ScanInterval: time.Second, LockTTL: 60 * time.Second, Now: fx.clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		ctrl2.scanOnce(ctx)
		if r, _ := fx.store.GetRelease(ctx, rel.ID); r.Status == modelrepo.ReleaseStatusSucceeded {
			break
		}
	}
	err = fx.store.RequestRollback(ctx, rel.ID)
	if !errors.Is(err, modelrepo.ErrNoPrevActive) {
		t.Fatalf("无 PrevActive 应 ErrNoPrevActive（422），got %v", err)
	}
}

// TestC3_版本被接管_409 release.version ≠ 当前 active → ErrVersionMismatch
// （409，L26 守卫：先显式 activate 或发起新发布）。
func TestC3_版本被接管_409(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a"}, 1, 0, true)
	ctx := context.Background()
	// 先让 rel-1 终态（RequestRollback 仅接受 running/终态，pending 会先
	// 撞 ErrReleaseTerminal；succeeded 后才能测试版本被接管分支）
	fx.runToSucceeded(t)
	// 新版本 v3.0.0 激活（自动归档 v2.0.0）——模拟"回滚前已被新发布接管"
	if err := fx.store.CreateVersion(ctx, &modelrepo.ModelVersion{
		Model: testModel, Version: "v3.0.0", Mirror: "reg/x:v3", Sha256: "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fx.store.ActivateVersion(ctx, testModel, "v3.0.0"); err != nil {
		t.Fatal(err)
	}
	err := fx.store.RequestRollback(ctx, fx.relID)
	if !errors.Is(err, modelrepo.ErrVersionMismatch) {
		t.Fatalf("version≠active 应 ErrVersionMismatch（409），got %v", err)
	}
}

// TestC3_pending回滚_409 pending 中 rollback → ErrReleaseTerminal（409）。
func TestC3_pending回滚_409(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a"}, 1, 0, true)
	err := fx.store.RequestRollback(context.Background(), fx.relID)
	if !errors.Is(err, modelrepo.ErrReleaseTerminal) {
		t.Fatalf("pending rollback 应 ErrReleaseTerminal，got %v", err)
	}
}

// ── P8 定向用例 ────────────────────────────────────────────────────────

// TestP8_回滚TOCTOU_active被接管 回滚请求后被新版本激活接管 → 执行期复查
// 中止（D2）：head=failed + reason 明确 + RollbackRequested 清除（防活锁）
// + 未执行 perNode 标 skipped。
func TestP8_回滚TOCTOU_active被接管(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b"}, 1, 0, true)
	ctx := context.Background()
	fx.runToSucceeded(t)
	if err := fx.store.RequestRollback(ctx, fx.relID); err != nil {
		t.Fatal(err)
	}
	// 回滚执行前（控制器下一轮尚未跑）：新版本激活接管
	if err := fx.store.CreateVersion(ctx, &modelrepo.ModelVersion{
		Model: testModel, Version: "v3.0.0", Mirror: "reg/x:v3", Sha256: "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fx.store.ActivateVersion(ctx, testModel, "v3.0.0"); err != nil {
		t.Fatal(err)
	}
	fx.ctrl.scanOnce(ctx)
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusFailed {
		t.Fatalf("被接管回滚应中止为 failed，got %s", r.Status)
	}
	if !strings.Contains(r.FailureReason, "rollback superseded by active v3.0.0") {
		t.Fatalf("FailureReason = %q, want 含 superseded by active v3.0.0", r.FailureReason)
	}
	if r.RollbackRequested {
		t.Fatal("中止后 RollbackRequested 必须清除（防活锁）")
	}
	if fx.deploy.count() != 2 {
		t.Fatalf("中止前不应执行任何回滚部署，got %d 次", fx.deploy.count())
	}
	// 未执行 perNode 标 skipped：原 succeeded 的节点已 deployed（无 pending），
	// 保持现状（cancelRemainder 只动 pending）
	if nr := fx.nodeResult(t, "a"); nr.Status != modelrepo.NodeRelDeployed {
		t.Fatalf("已部署节点应保持 deployed，got %s", nr.Status)
	}
}

// TestP8_回滚TOCTOU_PrevActive被删 回滚请求后 PrevActive 版本被删（archived
// 可删）→ 执行期复查中止（D4）：head=failed + reason 含 deleted。
func TestP8_回滚TOCTOU_PrevActive被删(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a"}, 1, 0, true)
	ctx := context.Background()
	fx.runToSucceeded(t)
	if err := fx.store.RequestRollback(ctx, fx.relID); err != nil {
		t.Fatal(err)
	}
	if err := fx.store.DeleteVersion(ctx, testModel, "v1.0.0"); err != nil { // archived 可删
		t.Fatal(err)
	}
	fx.ctrl.scanOnce(ctx)
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusFailed {
		t.Fatalf("PrevActive 被删回滚应中止为 failed，got %s", r.Status)
	}
	if !strings.Contains(r.FailureReason, "previous active version v1.0.0 deleted") {
		t.Fatalf("FailureReason = %q, want 含 deleted", r.FailureReason)
	}
	if r.RollbackRequested {
		t.Fatal("中止后 RollbackRequested 必须清除")
	}
	if fx.deploy.count() != 1 {
		t.Fatalf("中止前不应执行回滚部署，got %d 次", fx.deploy.count())
	}
}

// TestP8_cancel_before_claim pending 时 cancel（从未被认领）→ ≤1 扫描周期
// perNode 全 skipped + guard 释放（同模型可再次发布）。perNode 补齐与
// guard 释放各需 ≤1 轮。
func TestP8_cancel_before_claim(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b"}, 1, 0, true)
	ctx := context.Background()
	if err := fx.store.CancelRelease(ctx, fx.relID); err != nil {
		t.Fatal(err)
	}
	fx.ctrl.scanOnce(ctx) // 轮 1：cancelRemainder → skipped
	for _, n := range []string{"a", "b"} {
		if nr := fx.nodeResult(t, n); nr.Status != modelrepo.NodeRelSkipped {
			t.Fatalf("%s 应 skipped，got %s", n, nr.Status)
		}
	}
	if fx.deploy.count() != 0 || fx.lock.grantCount() != 0 {
		t.Fatalf("cancel-before-claim 不应认领/部署（deploy=%d lock=%d）", fx.deploy.count(), fx.lock.grantCount())
	}
	fx.ctrl.scanOnce(ctx) // 轮 2：终态 → guard 释放（内存实现 = inFlight 检查放行）
	if err := fx.store.CreateRelease(ctx, &modelrepo.ModelRelease{
		ID: "rel-2", Model: testModel, Version: "v2.0.0", BatchSize: 1, TargetNodes: []string{"a"},
	}); err != nil {
		t.Fatalf("cancel 终态后同模型应可再次发布（guard 释放），got %v", err)
	}
}

// TestP5_锁刷新 刷新循环只对 active release 重绑租约；被接管 → 停止执行
// （active 移除，不再推进）。refreshActiveLocks 导出直调（D5：独立定时器）。
func TestP5_锁刷新(t *testing.T) {
	// 双节点 + batchSize=1：第一批完成后 release 仍在 running（暂停中）
	// 且 active 集合保留——刷新才有对象。单节点会在首轮直接终态。
	fx := newCtrlFixture(t, []string{"a", "b"}, 1, 60000, true)
	before := fx.lock.grantCount()
	fx.ctrl.scanOnce(context.Background()) // claim + 锁 + 批次 1（a）
	if fx.lock.grantCount() != before+1 {
		t.Fatal("claim 应 grant 一次")
	}
	if r := fx.release(t); r.Status != modelrepo.ReleaseStatusRunning {
		t.Fatalf("批次 1 后应仍 running（暂停中），got %s", r.Status)
	}
	// 刷新：active release 重绑租约
	fx.ctrl.refreshActiveLocks(context.Background())
	if fx.lock.grantCount() != before+2 {
		t.Fatalf("刷新应重 grant（D5），got %d want %d", fx.lock.grantCount(), before+2)
	}
	// 锁被他副本接管 → 本副本停止执行
	lockKey := modelrepo.ReleaseLockKey(fx.relID)
	fx.lock.held[lockKey] = true
	fx.ctrl.refreshActiveLocks(context.Background())
	fx.clk.Advance(60000 * time.Millisecond)
	fx.ctrl.scanOnce(context.Background())
	// 暂停中（NextBatchAt=now+60s）即使仍在 active 也不推进——此处断言：
	// 接管后 active 已移除，任何后续扫描都不应再部署
	fx.clk.Advance(60000 * time.Millisecond)
	fx.ctrl.scanOnce(context.Background())
	if fx.deploy.count() != 1 {
		t.Fatalf("锁被接管后本副本不应再推进，got %d 次部署", fx.deploy.count())
	}
}

// TestD8_不动式_实现缺陷暴露 目标节点 perNode 缺失 → finish 置 failed
// （invariant 文案）而非 succeeded（防御：实现缺陷不得静默错置）。
func TestD8_不动式_实现缺陷暴露(t *testing.T) {
	// 手工构造：head 先置 running（finish 的 setTerminal 仅接受 running，
	// CAS 拒绝 pending→failed，测试此前遗漏此前置），再注入"全部批次 deployed
	// 但 perNode 缺失"的崩溃态（等价于 TestC1_崩溃点续跑 反例）：
	fx := newCtrlFixture(t, []string{"a", "b", "c"}, 1, 0, true)
	ctx := context.Background()
	if err := fx.store.UpdateReleaseHead(ctx, fx.relID, func(h *modelrepo.ModelRelease) error {
		if h.Status != modelrepo.ReleaseStatusPending {
			return fmt.Errorf("head 应为 pending，got %s", h.Status)
		}
		h.Status = modelrepo.ReleaseStatusRunning
		h.StartedAt = fx.clk.Now().UnixMilli()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := fx.store.SetNodeResult(ctx, fx.relID, "a", &modelrepo.NodeReleaseResult{NodeID: "a", Status: modelrepo.NodeRelDeployed, Version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := fx.store.SetNodeResult(ctx, fx.relID, "b", &modelrepo.NodeReleaseResult{NodeID: "b", Status: modelrepo.NodeRelDeployed, Version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	// c 缺失：直接调用 finish 模拟"all Nodes 视为已执行"的缺陷路径
	// （正常控制器不会走到：firstUnexecutedBatch 会选中 c；这里单测不变式本身）
	r, err := fx.store.GetRelease(ctx, fx.relID)
	if err != nil {
		t.Fatal(err)
	}
	results, err := fx.store.ListNodeResults(ctx, fx.relID)
	if err != nil {
		t.Fatal(err)
	}
	fx.ctrl.finish(ctx, r, results) // 2 条结果、3 个目标 → D8 失败分支
	r2, _ := fx.store.GetRelease(ctx, fx.relID)
	if r2.Status != modelrepo.ReleaseStatusFailed {
		t.Fatalf("D8 不动式失败应置 failed，got %s", r2.Status)
	}
	if !strings.Contains(r2.FailureReason, "invariant") {
		t.Fatalf("FailureReason = %q, want 含 invariant", r2.FailureReason)
	}
}

// TestNewController 校验：nil 依赖 → error；LockTTL < 15s → error（D5 下限）。
func TestNewController(t *testing.T) {
	store := modelrepo.NewMemoryModelStore()
	dep := &fakeDeployer{errs: make(map[string]error)}
	lock := newFakeLock()
	if _, err := NewController(nil, dep, lock, Options{}); err == nil {
		t.Fatal("nil store 应报错")
	}
	if _, err := NewController(store, nil, lock, Options{}); err == nil {
		t.Fatal("nil deployer 应报错")
	}
	if _, err := NewController(store, dep, nil, Options{}); err == nil {
		t.Fatal("nil lock 应报错")
	}
	if _, err := NewController(store, dep, lock, Options{LockTTL: 10 * time.Second}); err == nil {
		t.Fatal("LockTTL < 15s 应报错（D5）")
	}
	// 默认值生效
	ctrl, err := NewController(store, dep, lock, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ctrl.opts.ScanInterval != DefaultScanInterval || ctrl.opts.LockTTL != DefaultLockTTL {
		t.Fatalf("默认参数错误: %+v", ctrl.opts)
	}
}
