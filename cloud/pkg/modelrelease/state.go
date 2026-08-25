package modelrelease

// 状态机合法性表（设计 §2.4，纯函数；WBS-5）。
//
// 权威判定在 modelrepo.types.go（CanTransitionVersion/CanTransitionRelease/
// CanTransitionNodeResult——创建发布/激活/归档/取消/回滚的 CAS mutate 闭包
// 全部复用）；本文件 = 控制器/API 侧的共享复用面：
//
//   - 表驱动数据形态：VersionTransitions / ReleaseTransitions /
//     NodeResultTransitions 全量合法转换对，供测试对拍、文档与审计共享
//     （一致性由 state_test.go 与 modelrepo 判定函数逐对对拍锁定；
//     新增转换对必须同时更新两处，对拍测试会失败）；
//   - 包装函数：CanTransitVersion / CanTransitRelease / CanTransitNodeResult，
//     委托 modelrepo 单一事实源，防两处实现漂移；
//   - 控制器不变式与派生汇总（供 API 的 summary 现算字段复用）：
//     AllNodesDeployed（D8：succeeded 转换前断言全部 TargetNodes perNode
//     终态 deployed 且无 failed，内存计数 O(1)）、SummarizeNodes。

import "edgeflow/cloud/pkg/modelrepo"

// VersionTransitions 是版本三态（draft/active/archived）全量合法转换对
// （设计 §2.4：draft─activate▶active─archive▶archived；archived 不可再
// 激活——需新版本号；delete 仅限 draft/archived 由 DeleteVersion 前置
// 校验承担，不在此表）。
var VersionTransitions = [][2]modelrepo.VersionStatus{
	{modelrepo.VersionStatusDraft, modelrepo.VersionStatusActive},    // activate
	{modelrepo.VersionStatusActive, modelrepo.VersionStatusArchived}, // archive
}

// ReleaseTransitions 是发布六态全量合法转换对（设计 §2.4，配合主线裁决）：
//
//	pending → running（控制器认领）/ canceled（cancel API）
//	running → succeeded（全部 deployed，D8 不变式）/ failed（fail-fast
//	          中止或跑完存在失败）/ canceled（批次边界）/ rolled_back
//	succeeded|failed|canceled → rolled_back（回滚执行，逆序逐批）
//	rolled_back → 无（终态；API 再回滚 → 409）
var ReleaseTransitions = [][2]modelrepo.ReleaseStatus{
	{modelrepo.ReleaseStatusPending, modelrepo.ReleaseStatusRunning},      // 认领（控制器）
	{modelrepo.ReleaseStatusPending, modelrepo.ReleaseStatusCanceled},     // cancel API
	{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusSucceeded},    // 全部 deployed（D8）
	{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusFailed},       // fail-fast 中止/跑完有失败
	{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusCanceled},     // cancel（批次边界生效）
	{modelrepo.ReleaseStatusRunning, modelrepo.ReleaseStatusRolledBack},   // rollback 执行完成
	{modelrepo.ReleaseStatusSucceeded, modelrepo.ReleaseStatusRolledBack}, // 回滚（终态可回滚）
	{modelrepo.ReleaseStatusFailed, modelrepo.ReleaseStatusRolledBack},    // 回滚（终态可回滚）
	{modelrepo.ReleaseStatusCanceled, modelrepo.ReleaseStatusRolledBack},  // 回滚（终态可回滚）
}

// NodeResultTransitions 是逐节点结果全量合法转换对（设计 §2.4：单向终态，
// 回退 pending 禁止；回滚只更新 Version 字段，状态保持 deployed/failed——
// 本表允许任意终态→终态，由调用方按语义写 Version）：
//
//	pending → deployed|failed|skipped（首写）
//	终态 → 终态（回滚执行：failed→deployed 恢复、deployed 保持改 Version、
//	 skipped→deployed 等）
var NodeResultTransitions = [][2]modelrepo.NodeRelStatus{
	{modelrepo.NodeRelPending, modelrepo.NodeRelDeployed},
	{modelrepo.NodeRelPending, modelrepo.NodeRelFailed},
	{modelrepo.NodeRelPending, modelrepo.NodeRelSkipped},
	{modelrepo.NodeRelDeployed, modelrepo.NodeRelDeployed},
	{modelrepo.NodeRelDeployed, modelrepo.NodeRelFailed},
	{modelrepo.NodeRelDeployed, modelrepo.NodeRelSkipped},
	{modelrepo.NodeRelFailed, modelrepo.NodeRelDeployed},
	{modelrepo.NodeRelFailed, modelrepo.NodeRelFailed},
	{modelrepo.NodeRelFailed, modelrepo.NodeRelSkipped},
	{modelrepo.NodeRelSkipped, modelrepo.NodeRelDeployed},
	{modelrepo.NodeRelSkipped, modelrepo.NodeRelFailed},
	{modelrepo.NodeRelSkipped, modelrepo.NodeRelSkipped},
}

// CanTransitVersion 报告版本状态机转换是否合法（委托 modelrepo 权威判定）。
func CanTransitVersion(from, to modelrepo.VersionStatus) bool {
	return modelrepo.CanTransitionVersion(from, to)
}

// CanTransitRelease 报告发布状态机转换是否合法（委托 modelrepo 权威判定）。
func CanTransitRelease(from, to modelrepo.ReleaseStatus) bool {
	return modelrepo.CanTransitionRelease(from, to)
}

// CanTransitNodeResult 报告逐节点结果转换是否合法（委托 modelrepo 权威判定）。
func CanTransitNodeResult(from, to modelrepo.NodeRelStatus) bool {
	return modelrepo.CanTransitionNodeResult(from, to)
}

// ── 控制器/API 共享的不变式与派生汇总（纯函数）────────────────────────

// AllNodesDeployed 断言 D8 防御性不变式：**全部 TargetNodes 的 perNode
// 终态 deployed 且无 failed**（内存计数 O(1)；主线裁决 D8——succeeded 转换
// 前必须通过，否则实现缺陷会以 failed/invariant 终态暴露而不是静默错置
// succeeded）。返回 (true, "") 或 (false, 违规节点 ID)。
//
// 注：failed 节点天然使断言失败（其 Status != deployed）；perNode 缺失同判。
func AllNodesDeployed(targetNodes []string, results []modelrepo.NodeReleaseResult) (bool, string) {
	byNode := make(map[string]modelrepo.NodeReleaseResult, len(results))
	for _, nr := range results {
		byNode[nr.NodeID] = nr
	}
	for _, id := range targetNodes {
		nr, ok := byNode[id]
		if !ok || nr.Status != modelrepo.NodeRelDeployed {
			return false, id
		}
	}
	return true, ""
}

// NodeSummary 是发布详情接口的 perNode 汇总（设计 §4.2：现算，非冗余存储）。
// JSON 形态（设计 §4.2 summary 字段表）：total/deployed/failed/pending/skipped。
type NodeSummary struct {
	Total    int `json:"total"`
	Deployed int `json:"deployed"`
	Failed   int `json:"failed"`
	Pending  int `json:"pending"`
	Skipped  int `json:"skipped"`
}

// SummarizeNodes 现算逐节点结果汇总（控制器 finish 判定失败计数与
// API summary 字段共用；只有合法状态参与计数，未知状态忽略并计入 Total）。
func SummarizeNodes(results []modelrepo.NodeReleaseResult) NodeSummary {
	var s NodeSummary
	for _, nr := range results {
		s.Total++
		switch nr.Status {
		case modelrepo.NodeRelDeployed:
			s.Deployed++
		case modelrepo.NodeRelFailed:
			s.Failed++
		case modelrepo.NodeRelPending:
			s.Pending++
		case modelrepo.NodeRelSkipped:
			s.Skipped++
		}
	}
	return s
}
