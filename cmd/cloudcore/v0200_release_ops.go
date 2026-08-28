package main

// v0.20.0 发布生命周期运维收口：失败节点重试（retry 克隆新发布）+
// 终态发布手动归档删除 + releaseNotes 发布元数据（创建期定死）。
//
// 要点：
//   - retry 只对终态发布开放；克隆新发布（新 ID + RetryOf 回指原发布），
//     Copy 版本/Target 交集/执行参数；CreatedAt 重置、Status=pending、
//     时间线带 created+retried 两事件；新发布正常走 guard/批次全流程。
//   - delete 仅终态可删（非终态 409 族）；双存储同步删头+子键。
//   - releaseNotes ≤1024 字节，创建期写入后不可变（PATCH 白名单不含）。

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"edgeflow/cloud/pkg/modelrelease"
	"edgeflow/cloud/pkg/modelrepo"
	"edgeflow/pkg/log"
)

// retryReleaseRequest 是 POST .../releases/{releaseID}/retry 的请求体：
// nodeIDs 可选（缺省 = 原发布全部 failed 节点）；显式指定时须是原发布
// failed 节点的非空子集（否则 400）；重复节点去重。
type retryReleaseRequest struct {
	NodeIDs []string `json:"nodeIDs,omitempty"`
}

// handleRetryRelease 处理 POST /api/v1/models/{modelName}/releases/{releaseID}/retry
// （v0.20.0）：仅终态发布可重试——失败节点克隆新发布补发。
// 校验链序（对齐 C-4 纪律）：GetModel(404) 先行 → GetRelease(404) →
// 终态检查（非终态 409）→ failed 节点核对（body 校验 400）→ 目标版本
// active 复查（422）→ CreateRelease（guard 冲突 409）→ perNode 预写 →
// 202 + 新发布响应（含 retryOf 回指）。
func (a *modelAPI) handleRetryRelease(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	releaseID := r.PathValue("releaseID")
	if _, err := a.store.GetModel(r.Context(), modelName); err != nil {
		modelError(w, err)
		return
	}
	orig, err := a.store.GetRelease(r.Context(), releaseID)
	if err != nil {
		modelError(w, err)
		return
	}
	if orig.Model != modelName {
		modelError(w, fmt.Errorf("%w: %s", modelrepo.ErrReleaseNotFound, releaseID))
		return
	}
	if !orig.Status.IsTerminal() {
		modelError(w, fmt.Errorf("%w: %s (status=%s)", modelrepo.ErrReleaseTerminal, releaseID, orig.Status))
		return
	}
	var req retryReleaseRequest
	// v0.23.0（CLD-10）：EOF 判定改 errors.Is（http.MaxBytesReader 包装
	// 或未来 Decode 换实现时，字符串等值判定会漏配）。
	if err := decodeWriteBody(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		// 空 body 容忍（缺省 nodeIDs=全部 failed 节点）；坏 JSON 才 400
		badRequest(w, "invalid json body")
		return
	}
	// 失败节点集合（原发布 perNode 现算；克隆只继承 failed 子集）
	results, err := a.store.ListNodeResults(r.Context(), releaseID)
	if err != nil {
		modelError(w, err)
		return
	}
	failed := make([]string, 0)
	for _, nr := range results {
		if nr.Status == modelrepo.NodeRelFailed {
			failed = append(failed, nr.NodeID)
		}
	}
	if len(failed) == 0 {
		writeErr(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("release %s has no failed nodes to retry", releaseID), nil)
		return
	}
	// 目标节点 = 显式子集或全部 failed；去重保序
	var target []string
	if len(req.NodeIDs) > 0 {
		seen := map[string]bool{}
		for _, n := range req.NodeIDs {
			if seen[n] {
				continue
			}
			seen[n] = true
			if !containsString(failed, n) {
				writeErr(w, http.StatusBadRequest,
					fmt.Sprintf("node %s is not in failed set of release %s", n, releaseID),
					map[string]any{"failedNodes": failed})
				return
			}
			target = append(target, n)
		}
		if len(target) == 0 {
			badRequest(w, "nodeIDs must be non-empty when specified")
			return
		}
	} else {
		target = append([]string(nil), failed...)
	}
	// 目标版本 active 复查（v0.9.0 R-1 语义）：原发布执行期间版本可能被
	// 归档/删除——422 阻断克隆，避免"补发一个不存在的版本"。
	version, err := a.store.GetVersion(r.Context(), modelName, orig.Version)
	if err != nil {
		modelError(w, err)
		return
	}
	if version.Status != modelrepo.VersionStatusActive {
		// v0.23.0（CLD-12）：422 文案按 orig.Status 细分（校验链序不变：
		// 终态检查先行 → 本分支 orig 恒为终态；orig 状态只进文案，
		// 状态码与 422 语义维持 v0.20.0 不变）。
		writeErr(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("retry of release %s (status=%s): target version %s/%s must be active (status=%s)",
				releaseID, orig.Status, modelName, orig.Version, version.Status), nil)
		return
	}
	// 克隆新发布：执行参数全量继承（预算等 opt-in 字段也继承——与"补发
	// 原失败的批"同语义）；NotBeforeMs 清零（重试即现在，不走原窗口）；
	// PrevActive 继承（版本未变，回滚链一致）。
	now := time.Now().UnixMilli()
	batchSize := orig.BatchSize
	if batchSize < 1 {
		batchSize = 1 // 旧种子/异常头归一化（缺省 1，与 createRelease applyDefaults 同口径）
	}
	// v0.23.0（CHN-16）：rand 失败即报错（500），不再有时间戳兑底 ID。
	cloneID, err := newReleaseID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	rel := &modelrepo.ModelRelease{
		ID:            cloneID,
		Model:         modelName,
		Version:       orig.Version,
		Target:        modelrepo.ReleaseTarget{Type: "nodeIDs", NodeIDs: target},
		TargetNodes:   target,
		BatchSize:     batchSize,
		PauseBetween:  orig.PauseBetween,
		FailFast:      orig.FailFast,
		PrevActive:    orig.PrevActive,
		FailureBudget: orig.FailureBudget,
		ReleaseNotes:  orig.ReleaseNotes,
		RetryOf:       releaseID,
	}
	if err := rel.ValidateCreate(); err != nil {
		badRequest(w, "%v", err)
		return
	}
	rel.AppendEvent(modelrepo.EventCreated, "retry clone created via API", now)
	rel.AppendEvent(modelrepo.EventRetried, "retry of "+releaseID, now)
	if err := a.store.CreateRelease(r.Context(), rel); err != nil {
		modelError(w, err)
		return
	}
	for i, nodeID := range rel.TargetNodes {
		if err := a.store.SetNodeResult(r.Context(), rel.ID, nodeID, &modelrepo.NodeReleaseResult{
			NodeID: nodeID, Status: modelrepo.NodeRelPending, Batch: modelrelease.BatchNumber(i, batchSize),
		}); err != nil {
			log.Warnf("[modelAPI] retry 发布 %s perNode 预写失败（node=%s，控制器按 pending 收敛）: %v",
				rel.ID, nodeID, err)
		}
	}
	log.Infof("[modelAPI] retry 克隆发布 %s（model=%s version=%s nodes=%d retryOf=%s）",
		rel.ID, modelName, rel.Version, len(target), releaseID)
	writeJSON(w, http.StatusAccepted, releaseResponse{
		ModelRelease: *rel,
		Summary:      modelrelease.NodeSummary{Total: len(target), Pending: len(target)},
	})
}

// handleDeleteRelease 处理 DELETE /api/v1/models/{modelName}/releases/{releaseID}
// （v0.20.0）：手动归档清理单条终态发布（succeeded/failed/canceled/rolled_back）。
// 校验链：GetModel(404) 先行 → GetRelease(404) → 终态守卫（非终态 409 族，
// 与 GC 同源语义"在途绝不删"）→ DeleteRelease 双存储删除 → 200 + 被删快照
// 摘要（id/status/retryOf，供审计）。
func (a *modelAPI) handleDeleteRelease(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("modelName")
	releaseID := r.PathValue("releaseID")
	if _, err := a.store.GetModel(r.Context(), modelName); err != nil {
		modelError(w, err)
		return
	}
	rel, err := a.store.GetRelease(r.Context(), releaseID)
	if err != nil {
		modelError(w, err)
		return
	}
	if rel.Model != modelName {
		modelError(w, fmt.Errorf("%w: %s", modelrepo.ErrReleaseNotFound, releaseID))
		return
	}
	if !rel.Status.IsTerminal() {
		modelError(w, fmt.Errorf("%w: %s (status=%s)", modelrepo.ErrReleaseConflict, releaseID, rel.Status))
		return
	}
	deleted, err := a.store.DeleteRelease(r.Context(), releaseID)
	if err != nil {
		modelError(w, err)
		return
	}
	log.Infof("[modelAPI] 归档删除发布 %s（model=%s version=%s status=%s）",
		deleted.ID, deleted.Model, deleted.Version, deleted.Status)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  deleted.Status,
		"id":      deleted.ID,
		"model":   deleted.Model,
		"version": deleted.Version,
		"retryOf": deleted.RetryOf,
	})
}

// containsString 报告 s 是否在 list 中（retry 失败节点子集核对；小集合
// 线性扫描即可——failed 节点数受发布规模约束）。
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
