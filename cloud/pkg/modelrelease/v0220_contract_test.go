package modelrelease

// v0.22.0 T-06 回归测试（CLD-01/02/03 契约漂移点锚点；既有测试文件不改
// 一行，本文件纯增量，复用 ctrlFixture/fakeDeployer 基建）：
//
//   - CLD-01 终态写点状态机断言：assertReleaseTransition 表映射与哨兵
//     错误、succeeded/failed 快乐路径穿越断言、rolled_back/autopause
//     写点、abortRollback 终态豁免面、违例观测面上报（含 panic 隔离）；
//   - CLD-02 digest 复核失败：head 时间线事件（EventDigestMismatch）+
//     失败预算同源判定（digest 失败节点经 SummarizeNodes 触发 autopause）；
//   - CLD-03 failFast 解耦：deployBatchNodes/deployBatchNode 只认 advance
//     流程快照参数，head 指针参数（含 PATCH 运行中改参的陈旧快照）不
//     参与中止判定。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"edgeflow/cloud/pkg/modelrepo"
)

const testMirrorDigest = "sha256:aaaa"

// setDigestOpts 在 fixture 控制器上注入 digest 校验（测试同包直注；
// n1 上报错 digest，其余节点上报正确 digest）。
func setDigestOpts(fx *ctrlFixture) {
	fx.ctrl.opts.DigestLookup = func(nodeID string) string {
		if nodeID == "n1" {
			return "sha256:bbbb"
		}
		return testMirrorDigest
	}
}

// setMirrorAndBudget 把 head 置 MirrorDigest + FailureBudget（模拟 API
// 创建发布时携带镜像 digest 与预算）。
func setMirrorAndBudget(t *testing.T, fx *ctrlFixture, budget int) {
	t.Helper()
	if err := fx.store.UpdateReleaseHead(context.Background(), fx.relID, func(h *modelrepo.ModelRelease) error {
		h.MirrorDigest = testMirrorDigest
		h.FailureBudget = budget
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// runToTerminal 反复扫描至目标终态（对齐 v0160 ctrl 测试模式）。
func runToTerminal(t *testing.T, fx *ctrlFixture, want modelrepo.ReleaseStatus) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if fx.release(t).Status == want {
			return
		}
		fx.ctrl.scanOnce(ctx)
	}
	if fx.release(t).Status != want {
		t.Fatalf("扫描 20 轮未达 %s，got %s", want, fx.release(t).Status)
	}
}

// TestV0220AssertReleaseTransition 表映射与哨兵错误（CLD-01 断言面单测）：
// 表内合法转换放行、自转换放行（幂等收敛）、表外非法转换拒绝且
// errors.Is 命中 errIllegalTransition 哨兵。
func TestV0220AssertReleaseTransition(t *testing.T) {
	legal := [][2]modelrepo.ReleaseStatus{
		{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusSucceeded},
		{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusFailed},
		{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusPaused},
		{modelrepo.ReleaseStatusSucceeded, modelrepo.ReleaseStatusRolledBack},
		{modelrepo.ReleaseStatusFailed, modelrepo.ReleaseStatusRolledBack},
	}
	for _, tr := range legal {
		if err := assertReleaseTransition(tr[0], tr[1]); err != nil {
			t.Fatalf("合法转换 %s→%s 被断言拒绝: %v", tr[0], tr[1], err)
		}
	}
	illegal := [][2]modelrepo.ReleaseStatus{
		{modelrepo.ReleaseStatusPending, modelrepo.ReleaseStatusSucceeded},
		{modelrepo.ReleaseStatusPending, modelrepo.ReleaseStatusFailed},
		{modelrepo.ReleaseStatusSucceeded, modelrepo.ReleaseStatusFailed},
		{modelrepo.ReleaseStatusFailed, modelrepo.ReleaseStatusSucceeded},
		{modelrepo.ReleaseStatusRolledBack, modelrepo.ReleaseStatusRunning},
	}
	for _, tr := range illegal {
		err := assertReleaseTransition(tr[0], tr[1])
		if err == nil || !errors.Is(err, errIllegalTransition) {
			t.Fatalf("非法转换 %s→%s 未被拒绝（哨兵不符）: %v", tr[0], tr[1], err)
		}
	}
	// 自转换放行（幂等收敛写点：同终态重入不视为违例）
	if err := assertReleaseTransition(modelrepo.ReleaseStatusSucceeded, modelrepo.ReleaseStatusSucceeded); err != nil {
		t.Fatalf("自转换应放行: %v", err)
	}
}

// TestV0220TerminalWritePoints_HappyPath succeeded/failed 终态写点穿越
// 断言（CLD-01）：合法路径零拦截，终态事件随写点落台账。
func TestV0220TerminalWritePoints_HappyPath(t *testing.T) {
	// succeeded 写点：全部部署成功 → succeeded（running→succeeded 穿越断言）
	fx := newCtrlFixture(t, []string{"n1", "n2"}, 1, 0, true)
	runToTerminal(t, fx, modelrepo.ReleaseStatusSucceeded)
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusSucceeded || r.FinishedAt == 0 {
		t.Fatalf("应收敛 succeeded，got %s", r.Status)
	}
	if len(r.Events) == 0 || r.Events[len(r.Events)-1].Kind != modelrepo.EventTerminal {
		t.Fatalf("终态写点应落 terminal 事件: %+v", r.Events)
	}

	// failed 写点：fail-fast 中止 → failed（running→failed 穿越断言）
	fx2 := newCtrlFixture(t, []string{"n1", "n2"}, 1, 0, true)
	fx2.deploy.errs["n1"] = errors.New("boom")
	runToTerminal(t, fx2, modelrepo.ReleaseStatusFailed)
	r2 := fx2.release(t)
	if r2.Status != modelrepo.ReleaseStatusFailed || r2.FailureReason == "" {
		t.Fatalf("应收敛 failed 且带 reason，got %s/%q", r2.Status, r2.FailureReason)
	}
	if n1, _ := fx2.store.GetNodeResult(context.Background(), fx2.relID, "n1"); n1 == nil || n1.Status != modelrepo.NodeRelFailed {
		t.Fatalf("n1 应为 failed，got %+v", n1)
	}
}

// TestV0220TerminalObserver_Report 违例观测面：上报回调收数 + panic 隔离
// （观测面故障不影响发布主流程）。
func TestV0220TerminalObserver_Report(t *testing.T) {
	fx := newCtrlFixture(t, []string{"n1"}, 1, 0, true)
	var got []terminalWriteDetail
	fx.ctrl.SetTerminalObserver(func(d terminalWriteDetail) {
		got = append(got, d)
	})
	// 直接注入一次违例上报（断言拦截触发面单测——生产违例即缺陷信号）
	fx.ctrl.reportIllegalTransition(fx.relID, modelrepo.ReleaseStatusSucceeded, modelrepo.ReleaseStatusFailed, "unit")
	if len(got) != 1 || got[0].ReleaseID != fx.relID || got[0].From != modelrepo.ReleaseStatusSucceeded || got[0].To != modelrepo.ReleaseStatusFailed {
		t.Fatalf("观测回调未收到违例明细: %+v", got)
	}
	// panic 隔离：回调 panic 不外泄
	fx.ctrl.SetTerminalObserver(func(terminalWriteDetail) { panic("observer boom") })
	fx.ctrl.reportIllegalTransition(fx.relID, modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusPending, "unit-panic")
}

// TestV0220DigestMismatch_WritesHeadEvent digest 复核失败写 perNode failed
// + head 时间线事件（CLD-02）；三跳过路径回归锚点不误报。
func TestV0220DigestMismatch_WritesHeadEvent(t *testing.T) {
	fx := newCtrlFixture(t, []string{"n1"}, 1, 0, true)
	setDigestOpts(fx)
	setMirrorAndBudget(t, fx, 0)
	ctx := context.Background()
	h := fx.release(t)
	ok, reason := fx.ctrl.digestMismatch(ctx, fx.relID, h, "n1")
	if !ok || reason == "" {
		t.Fatalf("n1 digest 不一致应命中，got ok=%v reason=%q", ok, reason)
	}
	// perNode failed（与批次失败同源的事实源）
	nr, err := fx.store.GetNodeResult(ctx, fx.relID, "n1")
	if err != nil || nr.Status != modelrepo.NodeRelFailed {
		t.Fatalf("digest 失败应写 perNode failed，got %+v err=%v", nr, err)
	}
	// head 台账事件（CLD-02 核心断言）
	h2 := fx.release(t)
	if len(h2.Events) == 0 || h2.Events[len(h2.Events)-1].Kind != modelrepo.EventDigestMismatch {
		t.Fatalf("digest 复核失败应写 head 事件，got %+v", h2.Events)
	}
	if want := "node n1: " + reason; h2.Events[len(h2.Events)-1].Detail != want {
		t.Fatalf("事件 Detail=%q，want %q", h2.Events[len(h2.Events)-1].Detail, want)
	}

	// 跳过路径回归锚点：Lookup 未注入 / MirrorDigest 空 / 上报空 → 不误报
	fx.ctrl.opts.DigestLookup = nil
	if ok, _ := fx.ctrl.digestMismatch(ctx, fx.relID, h2, "n1"); ok {
		t.Fatal("Lookup 未注入应整体关闭")
	}
	hNoDigest := *h2
	hNoDigest.MirrorDigest = ""
	if ok, _ := fx.ctrl.digestMismatch(ctx, fx.relID, &hNoDigest, "n1"); ok {
		t.Fatal("MirrorDigest 空应跳过")
	}
	fx.ctrl.opts.DigestLookup = func(string) string { return "" }
	if ok, _ := fx.ctrl.digestMismatch(ctx, fx.relID, h2, "n1"); ok {
		t.Fatal("节点上报空 digest 应跳过（老边缘不误伤）")
	}
}

// TestV0220DigestFailure_SameSourceBudgetAutoPause digest 失败与批次失败
// 同源计入失败预算（CLD-02）：n1 部署成功但 digest 复核失败（无任何部署
// 失败），budget=1 → 与批次失败同一 SummarizeNodes 源触发自动暂停。
func TestV0220DigestFailure_SameSourceBudgetAutoPause(t *testing.T) {
	fx := newCtrlFixture(t, []string{"n1", "n2"}, 1, 0, false) // failFast=false：预算模式
	setDigestOpts(fx)                                          // n1 部署成功后即时 digest 检查失败
	setMirrorAndBudget(t, fx, 1)
	fx.ctrl.scanOnce(context.Background()) // 认领 → 批1（n1 digest 失败不入中止）→ 预算检查

	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusPaused || r.AutoPausedAt == 0 {
		t.Fatalf("digest 失败应同源触发预算自动暂停，got status=%s autoPausedAt=%d", r.Status, r.AutoPausedAt)
	}
	// 同源证据：唯一失败节点来自 digest 复核（部署零失败）
	if got := len(fx.deploy.errs); got != 0 {
		t.Fatalf("不应有部署失败注入，got %d", got)
	}
	n1, _ := fx.store.GetNodeResult(context.Background(), fx.relID, "n1")
	if n1 == nil || n1.Status != modelrepo.NodeRelFailed || !strings.HasPrefix(n1.Reason, "digest-mismatch") {
		t.Fatalf("失败源应为 digest 复核，got %+v", n1)
	}
	// 时间线：digest_mismatch 与 autopause 事件俱在（环形顺序）
	kinds := map[string]bool{}
	for _, ev := range r.Events {
		kinds[ev.Kind] = true
	}
	if !kinds[modelrepo.EventDigestMismatch] || !kinds[modelrepo.EventAutoPause] {
		t.Fatalf("时间线缺 digest_mismatch/autopause 事件: %+v", r.Events)
	}
}

// TestV0220FailFast_DecoupledFromHeadParam CLD-03：deployBatchNodes 中止
// 判定只认 advance 流程快照参数——head 指针 FailFast=true（陈旧快照）但
// 参数=false 时批内继续不中止；参数=true 时首败即返回（串行路径不触达
// 后续节点）。失败返回不改 head 状态。
func TestV0220FailFast_DecoupledFromHeadParam(t *testing.T) {
	// 场景 A：head 快照 FailFast=true（陈旧），流程参数=false → 不中止
	fx := newCtrlFixture(t, []string{"n1", "n2"}, 2, 0, true) // 创建时 FailFast=true
	ctx := context.Background()
	h := fx.release(t) // h.FailFast=true（陈旧指针参数）；发布未认领，head=pending
	statusBefore := h.Status
	if !h.FailFast {
		t.Fatal("fixture 前置：head 快照应为 FailFast=true")
	}
	results, _ := fx.store.ListNodeResults(ctx, fx.relID)
	byNode := map[string]modelrepo.NodeReleaseResult{}
	for _, nr := range results {
		byNode[nr.NodeID] = nr
	}
	fx.deploy.errs["n1"] = errors.New("boom")
	failNode, _ := fx.ctrl.deployBatchNodes(ctx, fx.relID, h, []string{"n1", "n2"}, byNode, false)
	if failNode != "" {
		t.Fatalf("参数=false 应不中止（首败继续），got failNode=%s", failNode)
	}
	n1, _ := fx.store.GetNodeResult(ctx, fx.relID, "n1")
	n2, _ := fx.store.GetNodeResult(ctx, fx.relID, "n2")
	if n1.Status != modelrepo.NodeRelFailed || n2.Status != modelrepo.NodeRelDeployed {
		t.Fatalf("批内应继续：n1=failed n2=deployed，got n1=%s n2=%s", n1.Status, n2.Status)
	}
	if r := fx.release(t); r.Status != statusBefore {
		t.Fatalf("失败返回不得改 head 状态，before=%s got %s", statusBefore, r.Status)
	}

	// 场景 B：流程参数=true → 首败即中止判定（返回首败节点，串行不触达 n2）
	fx2 := newCtrlFixture(t, []string{"n1", "n2"}, 2, 0, false) // head=false，参数说了算
	h2 := fx2.release(t)
	statusBefore2 := h2.Status
	results2, _ := fx2.store.ListNodeResults(ctx, fx2.relID)
	byNode2 := map[string]modelrepo.NodeReleaseResult{}
	for _, nr := range results2 {
		byNode2[nr.NodeID] = nr
	}
	fx2.deploy.errs["n1"] = errors.New("boom")
	failNode2, reason2 := fx2.ctrl.deployBatchNodes(ctx, fx2.relID, h2, []string{"n1", "n2"}, byNode2, true)
	if failNode2 != "n1" || reason2 == "" {
		t.Fatalf("参数=true 应首败返回 n1，got %s/%q", failNode2, reason2)
	}
	if got := len(fx2.deploy.nodes()); got != 1 {
		t.Fatalf("串行中止后不应触达 n2，部署调用数=%d", got)
	}
	if r := fx2.release(t); r.Status != statusBefore2 {
		t.Fatalf("deployBatchNodes 只返回决策不改 head，before=%s got %s", statusBefore2, r.Status)
	}
}

// TestV0220RollbackDone_WritePointAssertAndEvent rolled_back 写点断言 +
// 回滚完成事件（CLD-01 补齐台账：该写点此前不写事件）。
func TestV0220RollbackDone_WritePointAssertAndEvent(t *testing.T) {
	fx := newCtrlFixture(t, []string{"n1", "n2"}, 1, 0, true)
	runToTerminal(t, fx, modelrepo.ReleaseStatusSucceeded)
	ctx := context.Background()
	if err := fx.store.RequestRollback(ctx, fx.relID); err != nil {
		t.Fatal(err)
	}
	runToTerminal(t, fx, modelrepo.ReleaseStatusRolledBack)
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusRolledBack || r.RollbackRequested {
		t.Fatalf("应收敛 rolled_back 且清零请求位，got %s requested=%v", r.Status, r.RollbackRequested)
	}
	found := false
	for _, ev := range r.Events {
		if ev.Kind == modelrepo.EventRollbackDone {
			found = true
		}
	}
	if !found {
		t.Fatalf("回滚完成写点应落 rollback_done 事件: %+v", r.Events)
	}
	for _, n := range []string{"n1", "n2"} {
		nr, _ := fx.store.GetNodeResult(ctx, fx.relID, n)
		if nr == nil || nr.Version != "v1.0.0" {
			t.Fatalf("节点 %s 应回滚到 v1.0.0，got %+v", n, nr)
		}
	}
}

// TestV0220RollbackAbort_TerminalExemption abortRollback 对 RequestRollback
// 准入终态（succeeded）的跨终态收敛豁免（CLD-01 登记：succeeded→failed
// 不在表内，硬拦截会形成台账矛盾；豁免面回归锚点）。
func TestV0220RollbackAbort_TerminalExemption(t *testing.T) {
	fx := newCtrlFixture(t, []string{"n1"}, 1, 0, true)
	runToTerminal(t, fx, modelrepo.ReleaseStatusSucceeded)
	ctx := context.Background()
	if err := fx.store.RequestRollback(ctx, fx.relID); err != nil {
		t.Fatal(err)
	}
	// 制造 D2 中止条件：active 被新版本接管（v3 激活）
	if err := fx.store.CreateVersion(ctx, &modelrepo.ModelVersion{
		Model: testModel, Version: "v3.0.0",
		Mirror: "registry.example.com/models/" + testModel + ":v3.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fx.store.ActivateVersion(ctx, testModel, "v3.0.0"); err != nil {
		t.Fatal(err)
	}
	fx.clk.Advance(time.Second)
	fx.ctrl.scanOnce(context.Background())
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusFailed || r.RollbackRequested {
		t.Fatalf("回滚中止应收敛 failed 且清零请求位，got %s requested=%v", r.Status, r.RollbackRequested)
	}
	if r.FailureReason == "" {
		t.Fatal("中止 reason 不应为空")
	}
}
