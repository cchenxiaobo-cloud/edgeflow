package main

// v0.17.0 发布任务运维深化：PATCH 运行中可调参数 / 列表 status 过滤 /
// dryRun 预检。全部改动收敛在 API 层（存储接口零变化——控制器 BuildBatches
// 每轮按 TargetNodes+BatchSize 重新切片，运行中改 batchSize 下一批边界
// 自然生效；perNode.Batch 为创建时预分配元信息，不受影响）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/pkg/log"
)

// updateReleaseRequest 是 PATCH .../releases/{releaseID} 的请求体：
// 全字段指针 = 未提供的字段保持不变（部分更新语义）；显式提供即整值替换。
// 身份段（Version/Target/TargetNodes/Model）不可变——不在此结构中，天然不可改。
type updateReleaseRequest struct {
	BatchSize    *int   `json:"batchSize"`
	PauseBetween *int64 `json:"pauseBetween"`
	FailFast     *bool  `json:"failFast"`
}

// dryRunPreview 是 dryRun 创建的响应体：只读预检结论，**非承诺语义**
// （预检通过到真实创建之间资源状态可能变化——guard 竞态为可接受 TOCTOU，
// 见 design §2）。
type dryRunPreview struct {
	Kind         string                  `json:"kind"` // "DryRunPreview"
	APIVersion   string                  `json:"apiVersion"`
	Model        string                  `json:"model"`
	Version      string                  `json:"version"`
	WouldCreate  bool                    `json:"wouldCreate"`
	BlockReason  string                  `json:"blockReason,omitempty"` // wouldCreate=false 的机器可读原因
	Target       modelrepo.ReleaseTarget `json:"target"`
	TargetNodes  []string                `json:"targetNodes"`                 // 物化结果预览（与真实创建同一物化函数）
	PrevActive   string                  `json:"prevActive,omitempty"`        // 回滚链预览
	MirrorDigest string                  `json:"mirrorDigest,omitempty"`      // 探活开启时的期望 digest 预览
	InFlightID   string                  `json:"inFlightReleaseId,omitempty"` // blockReason=in-flight-release 时带出
	CheckedAt    int64                   `json:"checkedAt"`
}

// validReleaseStatuses 是 status 过滤的合法取值集合。
var validReleaseStatuses = map[modelrepo.ReleaseStatus]bool{
	modelrepo.ReleaseStatusPending: true, modelrepo.ReleaseStatusRunning: true,
	modelrepo.ReleaseStatusPaused: true, modelrepo.ReleaseStatusSucceeded: true,
	modelrepo.ReleaseStatusFailed: true, modelrepo.ReleaseStatusCanceled: true,
	modelrepo.ReleaseStatusRolledBack: true,
}

// parseStatusFilter 解析逗号分隔 status 过滤串 → 去重集合；空串 = 不过滤。
func parseStatusFilter(raw string) (map[modelrepo.ReleaseStatus]bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	set := make(map[modelrepo.ReleaseStatus]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		st := modelrepo.ReleaseStatus(part)
		if !st.IsValid() || !validReleaseStatuses[st] {
			return nil, &statusFilterError{value: part}
		}
		set[st] = true
	}
	return set, nil
}

type statusFilterError struct{ value string }

func (e *statusFilterError) Error() string {
	return "invalid status filter value: " + e.value + " (valid: pending,running,paused,succeeded,failed,canceled,rolled_back)"
}

// updateRelease 处理 PATCH /api/v1/models/{modelName}/releases/{releaseID}
// （v0.17.0）：发布任务运行中可调执行参数。规则：
//   - 仅非终态可改（终态 → 409 ErrReleaseTerminal 族——任务已完结，调参无意义
//     且具误导性）；pending/running/paused 均可（观察→调整→resume 运维动线）；
//   - 提供的每个字段先经值校验（batchSize≥1 / pauseBetween≥0），任一非法
//     → 400 整体拒绝（不做部分应用）；400 早于 404/409（语法错误优先）；
//   - CAS 经 store.UpdateReleaseHead（冲突重试 ≤3，耗尽 → 409）；控制器侧
//     每轮扫描重读 head 并 BuildBatches 重切批次 → 新参数下一批边界生效，
//     不中断在途批、无追溯歧义（failFast 改动同理只影响后续失败判定）。
func (a *modelAPI) updateRelease(w http.ResponseWriter, r *http.Request) {
	var req updateReleaseRequest
	if err := decodeWriteBody(w, r, &req); err != nil {
		badRequest(w, "invalid json body")
		return
	}
	if req.BatchSize == nil && req.PauseBetween == nil && req.FailFast == nil {
		badRequest(w, "empty patch: at least one of batchSize/pauseBetween/failFast is required")
		return
	}
	if req.BatchSize != nil && *req.BatchSize < 1 {
		badRequest(w, "batchSize must be >= 1, got %d", *req.BatchSize)
		return
	}
	if req.PauseBetween != nil && *req.PauseBetween < 0 {
		badRequest(w, "pauseBetween must be >= 0, got %d", *req.PauseBetween)
		return
	}
	releaseID := r.PathValue("releaseID")
	err := a.store.UpdateReleaseHead(r.Context(), releaseID, func(h *modelrepo.ModelRelease) error {
		if h.Status.IsTerminal() {
			return modelrepo.ErrReleaseTerminal
		}
		if req.BatchSize != nil {
			h.BatchSize = *req.BatchSize
		}
		if req.PauseBetween != nil {
			h.PauseBetween = *req.PauseBetween
		}
		if req.FailFast != nil {
			h.FailFast = *req.FailFast
		}
		return nil
	})
	if err != nil {
		modelError(w, err) // ErrReleaseNotFound→404 / ErrReleaseTerminal·ErrConcurrentConflict→409
		return
	}
	release, err := a.store.GetRelease(r.Context(), releaseID)
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, releaseResponse{
		ModelRelease: *release,
		Summary:      a.summaryOf(r.Context(), releaseID),
	})
}

// filterReleasesByStatus 内存过滤后再分页（与 limit/offset 正交，
// X-Total-Count 报过滤后总数）。allow 空 = 不过滤。
func filterReleasesByStatus(items []modelrepo.ModelRelease, allow map[modelrepo.ReleaseStatus]bool) []modelrepo.ModelRelease {
	if len(allow) == 0 {
		return items
	}
	out := make([]modelrepo.ModelRelease, 0, len(items))
	for i := range items {
		if allow[items[i].Status] {
			out = append(out, items[i])
		}
	}
	return out
}

// previewGuardConflict 是 dryRun 的 guard 等价只读判定：扫描同模型在途
// 发布（InFlight 三态），返回在途 ID 或空串。**不创建任何键**——etcd 实现的
// CreateRelease guard CAS 会落盘，dryRun 绝不能走它；本判定是尽力而为的
// TOCTOU 快照（响应为非承诺语义，真实创建以 CreateRelease CAS 为准兜底）。
func (a *modelAPI) previewGuardConflict(ctx context.Context, model string) string {
	releases, err := a.store.ListReleases(ctx, model)
	if err != nil {
		return "" // 预检尽力而为：列表失败视为无在途（真实创建仍有 guard CAS 兜底）
	}
	for i := range releases {
		if releases[i].Status.InFlight() {
			return releases[i].ID
		}
	}
	return ""
}

// checkedAtNowMs 便于测试注入的时间桩位（当前直通）。
var checkedAtNowMs = func() int64 { return time.Now().UnixMilli() }

// dryRunCreateRelease 处理 dryRun=true 的创建请求（v0.17.0）：与真实创建
// 同源复用校验链（模型存在/内容校验/窗口钟漂护栏/版本 active/镜像探活/
// 节点物化），加 guard 等价只读判定；错误响应码与真实创建完全一致
// （400/404/422 族同因同报），预检通过 → 200 + wouldCreate=true 摘要。
// 绝不调用 CreateRelease/SetNodeResult（零落盘、零 guard 键）。
func (a *modelAPI) dryRunCreateRelease(w http.ResponseWriter, r *http.Request, modelName string, req *createReleaseRequest) {
	// C-4 链序同源（设计稿裁定）：GetModel(404) 先于 ValidateCreate(400)，
	// 与真实创建路径逐字对齐——叠加错误场景下两链路同因同码。
	if _, err := a.store.GetModel(r.Context(), modelName); err != nil {
		modelError(w, err)
		return
	}
	pre := &modelrepo.ModelRelease{Model: modelName, Version: req.Version,
		Target: req.Target, BatchSize: req.BatchSize, PauseBetween: req.PauseBetween,
		NotBeforeMs: req.NotBeforeMs, FailureBudget: req.FailureBudget}
	if err := pre.ValidateCreate(); err != nil {
		badRequest(w, "%v", err)
		return
	}

	if req.NotBeforeMs > 0 && req.NotBeforeMs < time.Now().UnixMilli()-5*60*1000 {
		badRequest(w, "notBeforeMs is too far in the past (clock drift guard; use 0 for immediate)")
		return
	}
	version, err := a.store.GetVersion(r.Context(), modelName, req.Version)
	if err != nil {
		modelError(w, err)
		return
	}
	preview := dryRunPreview{
		Kind: "DryRunPreview", APIVersion: "v1",
		Model: modelName, Version: req.Version,
		Target: req.Target, CheckedAt: checkedAtNowMs(),
	}
	if version.Status != modelrepo.VersionStatusActive {
		preview.WouldCreate = false
		preview.BlockReason = fmt.Sprintf("target version %s/%s not active (status=%s)", modelName, req.Version, version.Status)
		writeJSON(w, http.StatusOK, preview)
		return
	}
	digest, block, err := a.mirrorCheck.check(r.Context(), version.Mirror)
	if err != nil && block {
		preview.WouldCreate = false
		preview.BlockReason = fmt.Sprintf("mirror check would fail: %v", err)
		writeJSON(w, http.StatusOK, preview)
		return
	}
	preview.MirrorDigest = digest
	targetNodes, err := a.materializeTargetNodes(req.Target, version)
	if err != nil {
		var une *modelrepo.UnknownNodesError
		switch {
		case errors.As(err, &une):
			preview.WouldCreate = false
			preview.BlockReason = "unknown nodes in whitelist"
			preview.TargetNodes = une.Nodes // 预检场景下改携未知节点清单，帮助排障
		case errors.Is(err, modelrepo.ErrNoReadyNodes):
			preview.WouldCreate = false
			preview.BlockReason = "no ready nodes for percentage target"
		default:
			badRequest(w, "%v", err)
			return
		}
		preview.CheckedAt = checkedAtNowMs()
		writeJSON(w, http.StatusOK, preview)
		return
	}
	preview.TargetNodes = targetNodes
	prevActive := version.PrevActive
	if prevActive == "" {
		if cur, err := a.store.ActiveVersion(r.Context(), modelName); err == nil && cur != nil && cur.Version != req.Version {
			prevActive = cur.Version
		}
	}
	preview.PrevActive = prevActive
	if inFlight := a.previewGuardConflict(r.Context(), modelName); inFlight != "" {
		preview.WouldCreate = false
		preview.BlockReason = "in-flight release exists (guard conflict; TOCTOU snapshot — real create may succeed if it resolves)"
		preview.InFlightID = inFlight
	} else {
		preview.WouldCreate = true
	}
	log.Infof("[modelAPI] dryRun 预检（model=%s version=%s target=%s nodes=%d wouldCreate=%v blockReason=%q）",
		modelName, req.Version, req.Target.Type, len(preview.TargetNodes), preview.WouldCreate, preview.BlockReason)
	writeJSON(w, http.StatusOK, preview)
}

// appendEvent 尽力而为地在发布头追加快径事件（v0.18.0 时间线；CAS 闭包内
// 追加不丢并发事件，终态 head 追加会被状态守卫拒绝——静默跳过不影响主流程）。
func (a *modelAPI) appendEvent(r *http.Request, releaseID, kind, detail string) {
	err := a.store.UpdateReleaseHead(r.Context(), releaseID, func(h *modelrepo.ModelRelease) error {
		if h.Status.IsTerminal() {
			return errSilent
		}
		h.AppendEvent(kind, detail, time.Now().UnixMilli())
		return nil
	})
	if err != nil && !errors.Is(err, errSilent) {
		log.Warnf("[modelAPI] 写事件 %s 失败（release %s）: %v", kind, releaseID, err)
	}
}

// errSilent 是 appendEvent 的静默终止哨兵（事件丢失可容忍：时间线是
// 尽力而为审计面，非权威台账）。
var errSilent = errors.New("modelapi: silent skip")

// deploymentFilterList 是全局部署影子列表响应形态。
type deploymentFilterList struct {
	Kind       string                      `json:"kind"`
	APIVersion string                      `json:"apiVersion"`
	Items      []modelrepo.DeploymentState `json:"items"`
}

// listAllDeployments 处理 GET /api/v1/deployments（v0.18.0）：跨模型聚合
// 全部部署影子；可选 query 过滤 model=<name>（精确匹配）、nodeID=<id>（精确
// 匹配），过滤后分页（X-Total-Count 报过滤后总数）。既有 per-model 端点
// （/models/{m}/deployments）不动——本端点是聚合视图。
func (a *modelAPI) listAllDeployments(w http.ResponseWriter, r *http.Request) {
	pp, err := parsePageParams(r)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	modelFilter := r.URL.Query().Get("model")
	if modelFilter != "" {
		if err := modelrepo.ValidateModelName(modelFilter); err != nil {
			badRequest(w, "%v", err)
			return
		}
	}
	nodeFilter := strings.TrimSpace(r.URL.Query().Get("nodeID"))
	all, err := a.store.ListAllDeployments(r.Context())
	if err != nil {
		modelError(w, err)
		return
	}
	filtered := make([]modelrepo.DeploymentState, 0, len(all))
	for _, d := range all {
		if modelFilter != "" && d.Model != modelFilter {
			continue
		}
		if nodeFilter != "" && d.NodeID != nodeFilter {
			continue
		}
		filtered = append(filtered, d)
	}
	page, total := slicePage(filtered, pp)
	writePageHeaders(w, total)
	items := make([]modelrepo.DeploymentState, 0, len(page))
	items = append(items, page...)
	writeJSON(w, http.StatusOK, deploymentFilterList{Kind: "DeploymentList", APIVersion: "v1", Items: items})
}
