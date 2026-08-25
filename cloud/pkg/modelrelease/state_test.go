package modelrelease

// 状态机表与不变式单测（设计 §10.1 V2 的控制器侧复用面；WBS-9）。
//
//   - 表驱动合法性表（VersionTransitions/ReleaseTransitions/
//     NodeResultTransitions）与 modelrepo 权威判定函数逐对对拍——新增
//     转换对必须两处同步更新，本测试保证不漂移；
//   - AllNodesDeployed（D8 防御性不变式）与 SummarizeNodes（API summary
//     现算复用）。

import (
	"reflect"
	"testing"

	"edgeflow/cloud/pkg/modelrepo"
)

// TestVersionTransitions_对拍 modelrepo 判定：表内全部合法、表外全部非法。
func TestVersionTransitions_对拍(t *testing.T) {
	states := []modelrepo.VersionStatus{
		modelrepo.VersionStatusDraft,
		modelrepo.VersionStatusActive,
		modelrepo.VersionStatusArchived,
	}
	inTable := map[[2]modelrepo.VersionStatus]bool{}
	for _, tr := range VersionTransitions {
		inTable[tr] = true
		if !CanTransitVersion(tr[0], tr[1]) {
			t.Fatalf("表内转换 %s→%s 与 modelrepo 判定不一致", tr[0], tr[1])
		}
	}
	for _, from := range states {
		for _, to := range states {
			ok := modelrepo.CanTransitionVersion(from, to)
			if ok != inTable[[2]modelrepo.VersionStatus{from, to}] {
				t.Fatalf("版本转换 %s→%s：modelrepo=%v 表=%v 不一致", from, to, ok, !ok)
			}
		}
	}
	// 非法状态防呆
	if CanTransitVersion("bogus", modelrepo.VersionStatusActive) {
		t.Fatal("非法状态不应可转换")
	}
}

// TestReleaseTransitions_对拍 覆盖设计 §2.4 全部合法/非法转换对：
// pending→running 合法、pending→succeeded 非法、canceled→rolled_back
// 合法、rolled_back→任何 非法。
func TestReleaseTransitions_对拍(t *testing.T) {
	states := []modelrepo.ReleaseStatus{
		modelrepo.ReleaseStatusPending,
		modelrepo.ReleaseStatusRunning,
		modelrepo.ReleaseStatusSucceeded,
		modelrepo.ReleaseStatusFailed,
		modelrepo.ReleaseStatusCanceled,
		modelrepo.ReleaseStatusRolledBack,
	}
	inTable := map[[2]modelrepo.ReleaseStatus]bool{}
	for _, tr := range ReleaseTransitions {
		inTable[tr] = true
		if !CanTransitRelease(tr[0], tr[1]) {
			t.Fatalf("表内转换 %s→%s 与 modelrepo 判定不一致", tr[0], tr[1])
		}
	}
	for _, from := range states {
		for _, to := range states {
			ok := modelrepo.CanTransitionRelease(from, to)
			if ok != inTable[[2]modelrepo.ReleaseStatus{from, to}] {
				t.Fatalf("发布转换 %s→%s：modelrepo=%v 表=%v 不一致", from, to, ok, !ok)
			}
		}
	}
	// 关键语义抽查（设计 §2.4 表 + 裁决 D8）
	illegal := [][2]modelrepo.ReleaseStatus{
		{modelrepo.ReleaseStatusPending, modelrepo.ReleaseStatusSucceeded},
		{modelrepo.ReleaseStatusPending, modelrepo.ReleaseStatusFailed},
		{modelrepo.ReleaseStatusPending, modelrepo.ReleaseStatusRolledBack},
		{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusPending},
		{modelrepo.ReleaseStatusRolledBack, modelrepo.ReleaseStatusRunning},
		{modelrepo.ReleaseStatusRolledBack, modelrepo.ReleaseStatusSucceeded},
		{modelrepo.ReleaseStatusSucceeded, modelrepo.ReleaseStatusSucceeded},
	}
	for _, pr := range illegal {
		if CanTransitRelease(pr[0], pr[1]) {
			t.Fatalf("非法转换 %s→%s 被放行", pr[0], pr[1])
		}
	}
	legal := [][2]modelrepo.ReleaseStatus{
		{modelrepo.ReleaseStatusPending, modelrepo.ReleaseStatusRunning},
		{modelrepo.ReleaseStatusPending, modelrepo.ReleaseStatusCanceled},
		{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusSucceeded},
		{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusFailed},
		{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusCanceled},
		{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusRolledBack},
		{modelrepo.ReleaseStatusCanceled, modelrepo.ReleaseStatusRolledBack},
		{modelrepo.ReleaseStatusFailed, modelrepo.ReleaseStatusRolledBack},
		{modelrepo.ReleaseStatusSucceeded, modelrepo.ReleaseStatusRolledBack},
	}
	for _, pr := range legal {
		if !CanTransitRelease(pr[0], pr[1]) {
			t.Fatalf("合法转换 %s→%s 被拒绝", pr[0], pr[1])
		}
	}
}

// TestNodeResultTransitions_对拍：单向终态不回退（任意 →pending 非法）；
// 终态→终态合法（回滚只更新 Version 字段，状态保持 deployed/failed）。
func TestNodeResultTransitions_对拍(t *testing.T) {
	states := []modelrepo.NodeRelStatus{
		modelrepo.NodeRelPending,
		modelrepo.NodeRelDeployed,
		modelrepo.NodeRelFailed,
		modelrepo.NodeRelSkipped,
	}
	inTable := map[[2]modelrepo.NodeRelStatus]bool{}
	for _, tr := range NodeResultTransitions {
		inTable[tr] = true
		if !CanTransitNodeResult(tr[0], tr[1]) {
			t.Fatalf("表内转换 %s→%s 与 modelrepo 判定不一致", tr[0], tr[1])
		}
	}
	for _, from := range states {
		for _, to := range states {
			ok := modelrepo.CanTransitionNodeResult(from, to)
			if ok != inTable[[2]modelrepo.NodeRelStatus{from, to}] {
				t.Fatalf("perNode 转换 %s→%s：modelrepo=%v 表=%v 不一致", from, to, ok, !ok)
			}
		}
	}
	// 回退 pending 一律非法
	for _, from := range states {
		if CanTransitNodeResult(from, modelrepo.NodeRelPending) {
			t.Fatalf("%s→pending 应非法", from)
		}
	}
}

// TestNumTransitions 表规模守卫：版本 2 条、发布 9 条、perNode 12 条
// （全矩阵 4×3=12，防误删）。任何收缩都需主线知会（状态机是契约面）。
func TestNumTransitions(t *testing.T) {
	if len(VersionTransitions) != 2 {
		t.Fatalf("VersionTransitions 应有 2 条，got %d", len(VersionTransitions))
	}
	if len(ReleaseTransitions) != 9 {
		t.Fatalf("ReleaseTransitions 应有 9 条，got %d", len(ReleaseTransitions))
	}
	if len(NodeResultTransitions) != 12 {
		t.Fatalf("NodeResultTransitions 应有 12 条，got %d", len(NodeResultTransitions))
	}
}

// ── D8 不变式与汇总 ─────────────────────────────────────────────────────

func TestAllNodesDeployed(t *testing.T) {
	results := func(nrs ...modelrepo.NodeReleaseResult) []modelrepo.NodeReleaseResult {
		return nrs
	}
	targets := []string{"a", "b", "c"}
	// 全部 deployed → ok
	ok, node := AllNodesDeployed(targets, results(
		modelrepo.NodeReleaseResult{NodeID: "a", Status: modelrepo.NodeRelDeployed},
		modelrepo.NodeReleaseResult{NodeID: "b", Status: modelrepo.NodeRelDeployed},
		modelrepo.NodeReleaseResult{NodeID: "c", Status: modelrepo.NodeRelDeployed},
	))
	if !ok {
		t.Fatalf("全部 deployed 应通过 D8（node=%s）", node)
	}
	// 任一 failed → 失败（无 failed 亦要求——deployed 之外的终态即违规）
	if ok, node := AllNodesDeployed(targets, results(
		modelrepo.NodeReleaseResult{NodeID: "a", Status: modelrepo.NodeRelDeployed},
		modelrepo.NodeReleaseResult{NodeID: "b", Status: modelrepo.NodeRelFailed},
		modelrepo.NodeReleaseResult{NodeID: "c", Status: modelrepo.NodeRelDeployed},
	)); ok || node != "b" {
		t.Fatalf("failed 节点应使 D8 失败并指出违规节点（got ok=%v node=%s）", ok, node)
	}
	// pending → 失败
	if ok, node := AllNodesDeployed(targets, results(
		modelrepo.NodeReleaseResult{NodeID: "a", Status: modelrepo.NodeRelDeployed},
		modelrepo.NodeReleaseResult{NodeID: "b", Status: modelrepo.NodeRelPending},
		modelrepo.NodeReleaseResult{NodeID: "c", Status: modelrepo.NodeRelDeployed},
	)); ok || node != "b" {
		t.Fatalf("pending 节点应使 D8 失败（got ok=%v node=%s）", ok, node)
	}
	// perNode 缺失 → 失败
	if ok, node := AllNodesDeployed(targets, results(
		modelrepo.NodeReleaseResult{NodeID: "a", Status: modelrepo.NodeRelDeployed},
		modelrepo.NodeReleaseResult{NodeID: "b", Status: modelrepo.NodeRelDeployed},
	)); ok || node != "c" {
		t.Fatalf("perNode 缺失应使 D8 失败（got ok=%v node=%s）", ok, node)
	}
	// skipped → 失败；无目标节点 → 通过（空发布防御）
	if ok, _ := AllNodesDeployed(targets, results(
		modelrepo.NodeReleaseResult{NodeID: "a", Status: modelrepo.NodeRelDeployed},
		modelrepo.NodeReleaseResult{NodeID: "b", Status: modelrepo.NodeRelDeployed},
		modelrepo.NodeReleaseResult{NodeID: "c", Status: modelrepo.NodeRelSkipped},
	)); ok {
		t.Fatal("skipped 节点应使 D8 失败")
	}
	if ok, _ := AllNodesDeployed(nil, nil); !ok {
		t.Fatal("空目标应通过 D8")
	}
}

func TestSummarizeNodes(t *testing.T) {
	results := []modelrepo.NodeReleaseResult{
		{NodeID: "a", Status: modelrepo.NodeRelDeployed},
		{NodeID: "b", Status: modelrepo.NodeRelDeployed},
		{NodeID: "c", Status: modelrepo.NodeRelFailed},
		{NodeID: "d", Status: modelrepo.NodeRelPending},
		{NodeID: "e", Status: modelrepo.NodeRelSkipped},
		{NodeID: "f", Status: modelrepo.NodeRelSkipped},
	}
	sum := SummarizeNodes(results)
	want := NodeSummary{Total: 6, Deployed: 2, Failed: 1, Pending: 1, Skipped: 2}
	if !reflect.DeepEqual(sum, want) {
		t.Fatalf("SummarizeNodes = %+v, want %+v", sum, want)
	}
	if got := SummarizeNodes(nil); got.Total != 0 {
		t.Fatalf("空输入应全零，got %+v", got)
	}
}
