package modelrelease

// v0.16.0 控制器层时序单测：定时窗口门控（窗口未到不认领/不占领跑锁、
// 到点即启动）与暂停恢复（paused 不推进批次、保锁、resume 后续跑至终态）。
// 既有 controller_test.go 不改一行；本文件纯增量，复用 ctrlFixture。

import (
	"context"
	"testing"
	"time"

	"edgeflow/cloud/pkg/modelrepo"
)

// newScheduledFixture 在 ctrlFixture 基础上补一个带 NotBeforeMs 的发布
// （独立 releaseID="rel-win"，避免与 rel-1 的 guard 互斥——用第二个模型）。
func newWindowCtrlFixture(t *testing.T, notBeforeMs int64) *ctrlFixture {
	t.Helper()
	fx := newCtrlFixture(t, []string{"n1", "n2"}, 1, 0, true)
	// 用同模型创建第二单会撞 guard（rel-1 pending 占位）→ 先把 rel-1 置为
	// canceled 终态释放 guard，再建窗口单。
	err := fx.store.CancelRelease(context.Background(), fx.relID)
	if err != nil {
		t.Fatal(err)
	}
	fx.clk.Advance(time.Second)
	fx.ctrl.scanOnce(context.Background()) // cancelRemainder + guard 释放

	ctx := context.Background()
	rel := &modelrepo.ModelRelease{
		ID: "rel-win", Model: testModel, Version: "v2.0.0",
		Target:      modelrepo.ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{"n1", "n2"}},
		TargetNodes: []string{"n1", "n2"}, BatchSize: 1,
		FailFast: true, PrevActive: "v1.0.0",
		NotBeforeMs: notBeforeMs,
	}
	if err := fx.store.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"n1", "n2"} {
		if err := fx.store.SetNodeResult(ctx, rel.ID, n, &modelrepo.NodeReleaseResult{
			NodeID: n, Status: modelrepo.NodeRelPending, Batch: BatchNumber(0, 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

func TestV160WindowDelaysClaim(t *testing.T) {
	now := time.UnixMilli(1787000000000)
	fx := newWindowCtrlFixture(t, now.Add(10*time.Minute).UnixMilli())
	ctx := context.Background()

	get := func() *modelrepo.ModelRelease {
		r, err := fx.store.GetRelease(ctx, "rel-win")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// 窗口未到：连续多轮扫描 → 保持 pending、零部署调用、零锁请求
	for i := 0; i < 3; i++ {
		fx.ctrl.scanOnce(ctx)
	}
	if r := get(); r.Status != modelrepo.ReleaseStatusPending || r.StartedAt != 0 {
		t.Fatalf("窗口未到应保持 pending 未启动，got %s startedAt=%d", r.Status, r.StartedAt)
	}
	if got := len(fx.deploy.calls); got != 0 {
		t.Fatalf("窗口未到不应有任何部署调用，got %d", got)
	}

	// 推进到窗口之后：认领启动并推至终态
	fx.clk.Advance(11 * time.Minute)
	for i := 0; i < 20 && get().Status != modelrepo.ReleaseStatusSucceeded; i++ {
		fx.ctrl.scanOnce(ctx)
	}
	if r := get(); r.Status != modelrepo.ReleaseStatusSucceeded {
		t.Fatalf("到点后应跑完 succeeded，got %s", r.Status)
	}
	if r := get(); r.StartedAt < now.Add(10*time.Minute).UnixMilli() {
		t.Fatalf("StartedAt=%d 不应早于窗口起点", r.StartedAt)
	}
}

func TestV160PauseResumeRunToTerminal(t *testing.T) {
	fx := newCtrlFixture(t, []string{"n1", "n2"}, 1, 0, true)
	ctx := context.Background()
	get := func() *modelrepo.ModelRelease { return fx.release(t) }

	fx.ctrl.scanOnce(ctx) // 认领 + 批次 1（n1）
	if r := get(); r.Status != modelrepo.ReleaseStatusRunning {
		t.Fatalf("首轮后应 running，got %s", r.Status)
	}

	// 暂停：后续扫描不再推进批次（n2 不部署），状态保持 paused
	if err := fx.store.PauseRelease(ctx, fx.relID); err != nil {
		t.Fatal(err)
	}
	fx.clk.Advance(60 * time.Second)
	fx.ctrl.scanOnce(ctx)
	fx.ctrl.scanOnce(ctx)
	if r := get(); r.Status != modelrepo.ReleaseStatusPaused {
		t.Fatalf("暂停期应保持 paused，got %s", r.Status)
	}
	if nr := fx.nodeResult(t, "n2"); nr.Status != modelrepo.NodeRelPending {
		t.Fatalf("暂停期 n2 应保持 pending，got %s", nr.Status)
	}

	// 恢复：续跑剩余批至 succeeded
	if err := fx.store.ResumeRelease(ctx, fx.relID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20 && get().Status != modelrepo.ReleaseStatusSucceeded; i++ {
		fx.clk.Advance(60 * time.Second)
		fx.ctrl.scanOnce(ctx)
	}
	if r := get(); r.Status != modelrepo.ReleaseStatusSucceeded {
		t.Fatalf("resume 后应跑到 succeeded，got %s", r.Status)
	}
	if nr := fx.nodeResult(t, "n2"); nr.Status != modelrepo.NodeRelDeployed {
		t.Fatalf("resume 后 n2 应 deployed，got %s", nr.Status)
	}
}

func TestV160PausedReleaseKeepsGuardOverSecondRelease(t *testing.T) {
	// 两节点两批：首轮只消费 n1（n2 留待下一轮），确保 pause 落在 running 中途
	fx := newCtrlFixture(t, []string{"n1", "n2"}, 1, 60_000, true)
	ctx := context.Background()

	fx.ctrl.scanOnce(ctx) // 认领 + 批次 1（n1）
	if err := fx.store.PauseRelease(ctx, fx.relID); err != nil {
		t.Fatal(err)
	}
	// paused 是 InFlight：第二单必须被 guard 拒绝（暂停不腾出发布通道）
	err := fx.store.CreateRelease(ctx, &modelrepo.ModelRelease{
		ID: "rel-2", Model: testModel, Version: "v2.0.0",
		Target:      modelrepo.ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{"n1"}},
		TargetNodes: []string{"n1"}, BatchSize: 1, FailFast: true,
	})
	if err == nil {
		t.Fatal("paused 发布占 guard：第二单应拒绝")
	}
}
