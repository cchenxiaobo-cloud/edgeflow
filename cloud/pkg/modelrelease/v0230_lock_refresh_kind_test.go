// CLD-13 观测锚点测试：领跑锁刷新日志的 paused/running kind 标记。
// kind 标记出现在「刷新失败」与「被接管」两类日志行（刷新成功本就无日志，
// 续租静默是既有行为）。覆盖：running 发布刷新失败日志带 kind=running；
// paused 发布被接管日志带 kind=paused 且文案含「占锁保持」；控制流不变
// （kind 读取不阻断续租/接管处理）。分支语义经导出直调断言（读失败 →
// unknown）。既有测试见 controller_test.go 的 TestP5_锁刷新（不改一行）。
package modelrelease

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/pkg/log"
)

// countKindLogs 统计缓冲中带 kind=<v> 标记的锁刷新日志条数。
func countKindLogs(buf *bytes.Buffer, kind string) int {
	return strings.Count(buf.String(), "kind="+kind)
}

// TestRefreshLockLogKindRunning running 发布刷新失败日志带 kind=running。
func TestRefreshLockLogKindRunning(t *testing.T) {
	// 双节点 + batchSize=1：第一批完成后 release 仍在 running（批间暂停）且
	// active 保留（与 TestP5_锁刷新 同构的最小场景）。
	fx := newCtrlFixture(t, []string{"a", "b"}, 1, 60000, true)
	ctx := context.Background()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	fx.ctrl.scanOnce(ctx) // claim + 锁 + 批次 1（a）
	if r := fx.release(t); r.Status != modelrepo.ReleaseStatusRunning {
		t.Fatalf("批次 1 后应仍 running（暂停中），got %s", r.Status)
	}

	// 锁后端故障 → 刷新失败路径：日志带 kind=running，控制流不变（下轮重试）。
	fx.lock.fail = errors.New("etcd down")
	fx.ctrl.refreshActiveLocks(ctx)
	if n := countKindLogs(&buf, "running"); n != 1 {
		t.Fatalf("running 刷新失败日志应恰 1 条 kind=running，实得 %d（日志:\n%s）", n, buf.String())
	}
	if !strings.Contains(buf.String(), "下轮重试") {
		t.Fatalf("刷新失败日志应保留「下轮重试」语义（日志:\n%s）", buf.String())
	}
}

// TestRefreshLockLogKindPausedTakeover paused 发布刷新被接管：日志带
// kind=paused 且文案含「占锁保持」中断语义；接管处理控制流不变（active 移除）。
func TestRefreshLockLogKindPausedTakeover(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a", "b"}, 1, 60000, true)
	ctx := context.Background()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	fx.ctrl.scanOnce(ctx) // claim + 锁 + 批次 1（a）→ running（批间暂停中）
	if r := fx.release(t); r.Status != modelrepo.ReleaseStatusRunning {
		t.Fatalf("批次 1 后应仍 running（暂停中），got %s", r.Status)
	}
	before := fx.lock.grantCount()

	// 先验证 paused 静默续租（正常路径无日志，仅 grant 计数 +1）。
	fx.ctrl.refreshActiveLocks(ctx)
	if fx.lock.grantCount() != before+1 {
		t.Fatalf("刷新应重 grant（D5），got %d want %d", fx.lock.grantCount(), before+1)
	}

	// 暂停：running → paused；锁被「他副本」持有 → 刷新走接管路径：
	// 日志带 kind=paused + 「占锁保持中断」，active 移除（控制流不变）。
	if err := fx.store.PauseRelease(ctx, fx.relID); err != nil {
		t.Fatalf("PauseRelease: %v", err)
	}
	buf.Reset()
	lockKey := modelrepo.ReleaseLockKey(fx.relID)
	fx.lock.held[lockKey] = true
	fx.ctrl.refreshActiveLocks(ctx)
	if n := countKindLogs(&buf, "paused"); n != 1 {
		t.Fatalf("paused 刷新被接管日志应恰 1 条 kind=paused，实得 %d（日志:\n%s）", n, buf.String())
	}
	if !strings.Contains(buf.String(), "占锁保持中断") {
		t.Fatalf("接管日志应含「占锁保持中断」语义（日志:\n%s）", buf.String())
	}
	// 控制流不变：接管后 active 已移除（isActive=false）。
	if fx.ctrl.isActive(fx.relID) {
		t.Fatal("被接管后 active 应已移除（控制流不受 kind 读取影响）")
	}
}

// TestReleaseKindForLogBranches releaseKindForLog 分支语义：非 running/paused
// 状态（pending/终态）与读失败 → unknown；paused → paused。
func TestReleaseKindForLogBranches(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a"}, 1, 0, true)
	ctx := context.Background()

	if got := fx.ctrl.releaseKindForLog(ctx, fx.relID); got != "unknown" {
		t.Fatalf("pending（未认领）release → %q，期望 unknown（非 running/paused 状态）", got)
	}
	if got := fx.ctrl.releaseKindForLog(ctx, "no-such-release"); got != "unknown" {
		t.Fatalf("不存在的 release → %q，期望 unknown（读失败兜底）", got)
	}

	fx.ctrl.scanOnce(ctx) // 单节点首轮即终态（succeeded）
	if got := fx.ctrl.releaseKindForLog(ctx, fx.relID); got != "unknown" {
		t.Fatalf("终态 release → %q，期望 unknown", got)
	}

	// paused 分支的正向覆盖见 TestRefreshLockLogKindPausedTakeover
	// （running → PauseRelease → kind=paused 端到端断言）。
}
