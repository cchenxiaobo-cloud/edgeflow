// digest 校验（v0.11.0，R-1+）控制器测试：三接入点（部署即时检查 /
// 推进期复查 / 终态复核）+ 三跳过（expected 空 / 边缘 digest 空 /
// DigestLookup nil）。
package modelrelease

import (
	"context"
	"strings"
	"testing"
	"time"

	"edgeflow/cloud/pkg/modelrepo"
)

// newDigestFixture 在 newCtrlFixture 基础上注入 MirrorDigest 与
// DigestLookup（lookup 为 map 引用——测试可中途改写模拟边缘上报变化）。
func newDigestFixture(t *testing.T, targetNodes []string, batchSize int, pause int64, failFast bool, expectedDigest string, lookup map[string]string) *ctrlFixture {
	t.Helper()
	fx := newCtrlFixture(t, targetNodes, batchSize, pause, failFast)
	ctx := context.Background()
	if err := fx.store.UpdateReleaseHead(ctx, fx.relID, func(h *modelrepo.ModelRelease) error {
		h.MirrorDigest = expectedDigest
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fx.ctrl.opts.DigestLookup = func(nodeID string) string {
		return lookup[nodeID]
	}
	return fx
}

func digestReason(t *testing.T, fx *ctrlFixture, nodeID string) string {
	t.Helper()
	nr := fx.nodeResult(t, nodeID)
	if nr.Status != modelrepo.NodeRelFailed {
		return ""
	}
	return nr.Reason
}

// 部署即时检查：lookup 预置 mismatch → 部署成功但当轮判 failed → failFast
// 中止（head failed + 其余 skipped + reason=digest-mismatch）。
func TestReleaseDigestMismatchFailFast(t *testing.T) {
	fx := newDigestFixture(t, []string{"a", "b", "c"}, 1, 0, true,
		"sha256:exp", map[string]string{"a": "sha256:exp", "b": "sha256:other"})
	fx.ctrl.scanOnce(context.Background()) // 批 1：a 部署+即时检查 match
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusRunning {
		t.Fatalf("批 1 后应 running，got %s", r.Status)
	}
	fx.clk.Advance(time.Millisecond)
	fx.ctrl.scanOnce(context.Background()) // 批 2：b 部署成功 → 即时 digest 检查 mismatch → fail-fast
	r = fx.release(t)
	if r.Status != modelrepo.ReleaseStatusFailed {
		t.Fatalf("应 failed，got %s（reason=%q）", r.Status, r.FailureReason)
	}
	if !strings.Contains(r.FailureReason, "digest-mismatch") {
		t.Fatalf("FailureReason 应含 digest-mismatch，got %q", r.FailureReason)
	}
	if nr := fx.nodeResult(t, "b"); nr.Status != modelrepo.NodeRelFailed || !strings.Contains(nr.Reason, "digest-mismatch: expected sha256:exp got sha256:other") {
		t.Fatalf("perNode b = %+v，期望 failed+digest-mismatch reason", nr)
	}
	if nr := fx.nodeResult(t, "c"); nr.Status != modelrepo.NodeRelSkipped {
		t.Fatalf("fail-fast 后 c 应 skipped，got %s", nr.Status)
	}
}

// failFast=false：mismatch 不中止，全部批次执行完 → 终态 failed。
func TestReleaseDigestMismatchNoFailFast(t *testing.T) {
	fx := newDigestFixture(t, []string{"a", "b"}, 1, 0, false,
		"sha256:exp", map[string]string{"a": "sha256:exp", "b": "sha256:other"})
	fx.ctrl.scanOnce(context.Background())
	fx.clk.Advance(time.Millisecond)
	fx.ctrl.scanOnce(context.Background()) // b 部署即时检查 mismatch（failFast=false 继续）
	fx.clk.Advance(time.Millisecond)
	fx.ctrl.scanOnce(context.Background()) // 全部批次完成 → finish 复核 → failed
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusFailed {
		t.Fatalf("应 failed，got %s（reason=%q）", r.Status, r.FailureReason)
	}
	if fx.deploy.count() != 2 {
		t.Fatalf("failFast=false 应部署全部节点，got %d", fx.deploy.count())
	}
}

// 推进期复查：批 1 部署时 match，批间边缘上报变 mismatch → 下一轮 advance
// 复查 catch → failFast 中止。
func TestReleaseDigestMismatchAdvanceRecheck(t *testing.T) {
	lookup := map[string]string{"a": "sha256:exp", "b": "sha256:exp"}
	fx := newDigestFixture(t, []string{"a", "b"}, 1, 1000, true,
		"sha256:exp", lookup)
	fx.ctrl.scanOnce(context.Background()) // 批 1：a deployed（match）
	if nr := fx.nodeResult(t, "a"); nr.Status != modelrepo.NodeRelDeployed {
		t.Fatalf("a 应 deployed，got %s", nr.Status)
	}
	lookup["a"] = "sha256:evil" // 边缘上报 digest 变化（模拟 tag 被重指后边缘拉到新镜像）
	fx.clk.Advance(1000 * time.Millisecond)
	fx.ctrl.scanOnce(context.Background()) // 推进期复查 catch a → failFast 中止
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusFailed {
		t.Fatalf("推进期复查应 failed，got %s（reason=%q）", r.Status, r.FailureReason)
	}
	if !strings.Contains(r.FailureReason, "digest-mismatch") {
		t.Fatalf("reason 应含 digest-mismatch，got %q", r.FailureReason)
	}
	if nr := fx.nodeResult(t, "b"); nr.Status != modelrepo.NodeRelSkipped {
		t.Fatalf("b 应 skipped，got %s", nr.Status)
	}
}

// 终态复核：failFast=false、mismatch 在批间出现 → 全部部署后 finish 复核
// catch → head failed（失败计数路径）。
func TestReleaseDigestMismatchAtFinish(t *testing.T) {
	lookup := map[string]string{"a": "sha256:exp", "b": "sha256:exp"}
	fx := newDigestFixture(t, []string{"a", "b"}, 1, 1000, false,
		"sha256:exp", lookup)
	fx.ctrl.scanOnce(context.Background()) // a deployed
	lookup["a"] = "sha256:evil"
	fx.clk.Advance(1000 * time.Millisecond)
	fx.ctrl.scanOnce(context.Background()) // 推进期复查 catch a（failFast=false 继续）→ b 部署
	fx.clk.Advance(1000 * time.Millisecond)
	fx.ctrl.scanOnce(context.Background()) // finish 复核 → failed
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusFailed {
		t.Fatalf("应 failed，got %s（reason=%q）", r.Status, r.FailureReason)
	}
	if nr := fx.nodeResult(t, "a"); nr.Status != modelrepo.NodeRelFailed || !strings.Contains(nr.Reason, "digest-mismatch") {
		t.Fatalf("a 应 failed+digest-mismatch，got %+v", nr)
	}
}

// match：全部一致 → succeeded（并验证 perNode deployed）。
func TestReleaseDigestMatchSucceeds(t *testing.T) {
	fx := newDigestFixture(t, []string{"a", "b"}, 1, 0, true,
		"sha256:exp", map[string]string{"a": "sha256:exp", "b": "sha256:exp"})
	fx.ctrl.scanOnce(context.Background())
	fx.clk.Advance(time.Millisecond)
	fx.ctrl.scanOnce(context.Background())
	fx.clk.Advance(time.Millisecond)
	fx.ctrl.scanOnce(context.Background())
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusSucceeded {
		t.Fatalf("应 succeeded，got %s（reason=%q）", r.Status, r.FailureReason)
	}
}

// 三跳过①：expected 空（off 模式）——即使边缘上报 digest 也不比对。
func TestReleaseDigestExpectedEmptySkips(t *testing.T) {
	fx := newDigestFixture(t, []string{"a"}, 1, 0, true, "",
		map[string]string{"a": "sha256:whatever"})
	fx.ctrl.scanOnce(context.Background())
	fx.clk.Advance(time.Millisecond)
	fx.ctrl.scanOnce(context.Background())
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusSucceeded {
		t.Fatalf("expected 空应跳过比对 succeeded，got %s（reason=%q）", r.Status, r.FailureReason)
	}
}

// 三跳过②：边缘 digest 空（老边缘 v0.9.0/v0.10.0 不报 imageDigest）——
// 不误伤不阻塞。
func TestReleaseDigestEdgeEmptySkips(t *testing.T) {
	fx := newDigestFixture(t, []string{"a"}, 1, 0, true, "sha256:exp",
		map[string]string{"a": ""})
	fx.ctrl.scanOnce(context.Background())
	fx.clk.Advance(time.Millisecond)
	fx.ctrl.scanOnce(context.Background())
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusSucceeded {
		t.Fatalf("边缘 digest 空应跳过比对 succeeded，got %s（reason=%q）", r.Status, r.FailureReason)
	}
}

// 三跳过③：DigestLookup nil（控制器未注入）→ 与 v0.10.0 行为逐字节一致。
func TestReleaseDigestLookupNilRegression(t *testing.T) {
	fx := newCtrlFixture(t, []string{"a"}, 1, 0, true)
	ctx := context.Background()
	if err := fx.store.UpdateReleaseHead(ctx, fx.relID, func(h *modelrepo.ModelRelease) error {
		h.MirrorDigest = "sha256:exp"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fx.ctrl.scanOnce(context.Background())
	fx.clk.Advance(time.Millisecond)
	fx.ctrl.scanOnce(context.Background())
	r := fx.release(t)
	if r.Status != modelrepo.ReleaseStatusSucceeded {
		t.Fatalf("DigestLookup nil 应关闭比对 succeeded，got %s（reason=%q）", r.Status, r.FailureReason)
	}
}
