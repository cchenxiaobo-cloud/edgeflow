package modelrepo

// v0.16.0 新增能力单测：定时窗口字段校验 / paused 状态机 / Pause·Resume 存储
// 语义（Memory 与 Etcd 契约同构用例）/ 导出快照结构合法性。
// 既有 store_test.go 不改一行；本文件为纯增量。

import (
	"context"
	"testing"
	"time"
)

func TestV160CanTransitionReleasePaused(t *testing.T) {
	if !CanTransitionRelease(ReleaseStatusRunning, ReleaseStatusPaused) {
		t.Fatal("running → paused 应合法（v0.16.0 pause API）")
	}
	if !CanTransitionRelease(ReleaseStatusPaused, ReleaseStatusRunning) {
		t.Fatal("paused → running 应合法（v0.16.0 resume API）")
	}
	if !CanTransitionRelease(ReleaseStatusPaused, ReleaseStatusCanceled) {
		t.Fatal("paused → canceled 应合法（暂停中可直接取消）")
	}
	for _, bad := range []ReleaseStatus{ReleaseStatusSucceeded, ReleaseStatusFailed, ReleaseStatusRolledBack} {
		if CanTransitionRelease(ReleaseStatusPaused, bad) {
			t.Fatalf("paused → %s 不应合法（终态经控制器推进，不直跳）", bad)
		}
	}
	if CanTransitionRelease(ReleaseStatusPending, ReleaseStatusPaused) {
		t.Fatal("pending → paused 不应合法（未启动发布无可暂停）")
	}
}

func TestV160PausedInFlight(t *testing.T) {
	p := ReleaseStatusPaused
	if !p.IsValid() || !p.InFlight() || p.IsTerminal() {
		t.Fatal("paused 必须是合法非终态且占用 guard（InFlight=true）")
	}
}

func TestV160ValidateCreateNotBeforeMs(t *testing.T) {
	r := &ModelRelease{Model: "defect-detector", Version: "v2.0.0",
		Target: ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{"n1"}}, BatchSize: 1}
	if err := r.ValidateCreate(); err != nil {
		t.Fatalf("零值 NotBeforeMs 应通过: %v", err)
	}
	r.NotBeforeMs = -1
	if err := r.ValidateCreate(); err == nil {
		t.Fatal("负数 notBeforeMs 应拒绝")
	}
	r.NotBeforeMs = time.Now().UnixMilli() + 60_000
	if err := r.ValidateCreate(); err != nil {
		t.Fatalf("未来窗口应通过: %v", err)
	}
}

func TestV160MemPauseResumeLifecycle(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()
	mustCreateRelease := func(id string) {
		t.Helper()
		err := s.CreateRelease(ctx, &ModelRelease{
			ID: id, Model: "defect-detector", Version: "v2.0.0",
			BatchSize: 1, TargetNodes: []string{"n1"},
			Target: ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{"n1"}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	mustHead := func(id string, st ReleaseStatus) {
		t.Helper()
		if err := s.UpdateReleaseHead(ctx, id, func(h *ModelRelease) error {
			h.Status = st
			if st == ReleaseStatusRunning && h.StartedAt == 0 {
				h.StartedAt = s.nowMs()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	mustCreateRelease("rel-pause")
	// pending 暂停/恢复均 409 族
	if err := s.PauseRelease(ctx, "rel-pause"); err == nil {
		t.Fatal("pending 发布 pause 应拒绝")
	}
	if err := s.ResumeRelease(ctx, "rel-pause"); err == nil {
		t.Fatal("pending 发布 resume 应拒绝")
	}
	mustHead("rel-pause", ReleaseStatusRunning)
	// 正常生命周期：pause → paused + PausedAt 落值；重复幂等；resume 回 running
	if err := s.PauseRelease(ctx, "rel-pause"); err != nil {
		t.Fatalf("running pause 失败: %v", err)
	}
	h, _ := s.GetRelease(ctx, "rel-pause")
	if h.Status != ReleaseStatusPaused || h.PausedAt == 0 {
		t.Fatalf("pause 后应 paused 且 PausedAt>0，got status=%s pausedAt=%d", h.Status, h.PausedAt)
	}
	if err := s.PauseRelease(ctx, "rel-pause"); err != nil {
		t.Fatalf("重复 pause 应幂等: %v", err)
	}
	// paused 下请求回滚应拒绝（先 resume 或 cancel）
	if err := s.RequestRollback(ctx, "rel-pause"); err == nil {
		t.Fatal("paused 发布 rollback 应拒绝")
	}
	if err := s.ResumeRelease(ctx, "rel-pause"); err != nil {
		t.Fatalf("resume 失败: %v", err)
	}
	h, _ = s.GetRelease(ctx, "rel-pause")
	if h.Status != ReleaseStatusRunning {
		t.Fatalf("resume 后应 running，got %s", h.Status)
	}
	if err := s.ResumeRelease(ctx, "rel-pause"); err == nil {
		t.Fatal("running 态 resume 应拒绝")
	}
}

// TestV160MemCancelFromPaused paused 状态下 cancel 直达终态（独立用例，
// 避免与前段共享 release 的 guard 互斥）。
func TestV160MemCancelFromPaused(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()
	err := s.CreateRelease(ctx, &ModelRelease{
		ID: "rel-pc", Model: "defect-detector", Version: "v2.0.0",
		BatchSize: 1, TargetNodes: []string{"n1"},
		Target: ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{"n1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateReleaseHead(ctx, "rel-pc", func(h *ModelRelease) error {
		h.Status = ReleaseStatusRunning
		h.StartedAt = s.nowMs()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseRelease(ctx, "rel-pc"); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelRelease(ctx, "rel-pc"); err != nil {
		t.Fatalf("paused → cancel 应成功: %v", err)
	}
	h, _ := s.GetRelease(ctx, "rel-pc")
	if h.Status != ReleaseStatusCanceled || !h.Status.IsTerminal() {
		t.Fatalf("cancel 后应 canceled 终态，got %s", h.Status)
	}
}

func TestV160MemScheduledKeepsGuardAndNotStarted(t *testing.T) {
	s := newMemFixture(t)
	ctx := context.Background()
	future := time.Now().Add(1 * time.Hour).UnixMilli()
	err := s.CreateRelease(ctx, &ModelRelease{
		ID: "rel-win", Model: "defect-detector", Version: "v2.0.0",
		BatchSize: 1, TargetNodes: []string{"n1"},
		Target:      ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{"n1"}},
		NotBeforeMs: future,
	})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := s.GetRelease(ctx, "rel-win")
	if h.NotBeforeMs != future {
		t.Fatalf("创建后 NotBeforeMs 应持久化，got %d want %d", h.NotBeforeMs, future)
	}
	if h.StartedAt != 0 || h.Status != ReleaseStatusPending {
		t.Fatalf("窗口发布创建即 pending 未启动，got %s startedAt=%d", h.Status, h.StartedAt)
	}
	// InFlight 判定守 guard：窗口期不得再发第二单
	if !h.Status.InFlight() {
		t.Fatal("pending 必须 InFlight=true（含窗口期），否则同模型可并发发布")
	}
	err = s.CreateRelease(ctx, &ModelRelease{
		ID: "rel-win-2", Model: "defect-detector", Version: "v2.0.0",
		BatchSize: 1, TargetNodes: []string{"n1"},
		Target: ReleaseTarget{Type: "nodeIDs", NodeIDs: []string{"n1"}},
	})
	if err == nil {
		t.Fatal("窗口期内第二单应被 guard 拒绝（ErrReleaseConflict）")
	}
}
