// 规则管理 API（v0.37.0）：规则 CRUD + 治理策略 + 规则包下发 + 事件查询。
//
// 9 个端点（契约扩容轮，注册到 apiMux → auth/audit 链自动覆盖）：
//
//	POST   /api/v1/rules                      创建规则（201）
//	GET    /api/v1/rules                      规则列表
//	GET    /api/v1/rules/{ruleID}             规则详情（404）
//	PUT    /api/v1/rules/{ruleID}             更新规则（404）
//	DELETE /api/v1/rules/{ruleID}             删除规则（404）
//	GET    /api/v1/rules/events               规则事件查询（ruleId/device/limit 过滤）
//	GET    /api/v1/rules/governance           治理策略列表（含规则包版本）
//	PUT    /api/v1/rules/governance           治理策略全量替换
//	POST   /api/v1/nodes/{nodeID}/rules/sync  规则包下发（可靠投递，五态语义同 syncConfig）
//
// 路由说明：路径中的字面段（events/governance）在 Go 1.22 ServeMux 语义下
// 优先于 {ruleID} 通配；ruleID 校验层已拒绝 governance/events 保留字，
// 两种匹配不可能同名歧义。
package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/cloud/pkg/rulestore"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/rules"
)

// ruleAPI 是规则管理 API 的处理器集合（依赖注入，模式与 nodeAPI/modelAPI 一致）。
type ruleAPI struct {
	// store 是规则包存储（内存 + etcd 写穿；与 RuleEvent 接收回调共享同一实例）。
	store *rulestore.Store
	// reg 是节点注册表（sync 前校验节点存在性；404 语义与既有 API 一致）。
	reg registry.Store
	// reliableSend 是可靠投递函数（默认 hub.ReliableSendContext；测试注入 fake）。
	reliableSend func(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error
}

// RuleSyncPayload 是 RuleSync 消息的负载（云→边）：全量规则包。
// 与边缘侧 cmd/edgecore 的 RuleSyncPayload 字段契约一致。
type RuleSyncPayload struct {
	RuleSet rules.RuleSet `json:"ruleSet"`
}

// Register 注册全部规则路由到给定 mux（挂在 apiMux → auth/audit 链覆盖）。
func (a *ruleAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/rules", a.createRule)
	mux.HandleFunc("GET /api/v1/rules", a.listRules)
	mux.HandleFunc("GET /api/v1/rules/events", a.listEvents)
	mux.HandleFunc("GET /api/v1/rules/governance", a.getGovernance)
	mux.HandleFunc("PUT /api/v1/rules/governance", a.putGovernance)
	mux.HandleFunc("GET /api/v1/rules/{ruleID}", a.getRule)
	mux.HandleFunc("PUT /api/v1/rules/{ruleID}", a.updateRule)
	mux.HandleFunc("DELETE /api/v1/rules/{ruleID}", a.deleteRule)
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/rules/sync", a.syncRules)
}

// writeRuleError 输出 {"error": "..."} 错误体（与既有 API 错误体同构）。
func writeRuleError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeRuleBody 解析写请求体；失败时输出 400/413 并返回 false。
func decodeRuleBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := decodeWriteBody(w, r, dst); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeRuleError(w, http.StatusRequestEntityTooLarge, "request body too large (limit 1MiB)")
			return false
		}
		writeRuleError(w, http.StatusBadRequest, "invalid json body")
		return false
	}
	return true
}

// storeErrStatus 把存储层错误映射为 HTTP 状态码。
func storeErrStatus(err error) (int, string) {
	switch {
	case errors.Is(err, rulestore.ErrInvalid):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, rulestore.ErrExists):
		return http.StatusConflict, err.Error()
	case errors.Is(err, rulestore.ErrNotFound):
		return http.StatusNotFound, err.Error()
	default:
		return http.StatusInternalServerError, "store error: " + err.Error()
	}
}

// ruleList 是 GET /api/v1/rules 的响应形态（K8s List 风格）。
type ruleList struct {
	Kind       string       `json:"kind"`
	APIVersion string       `json:"apiVersion"`
	Items      []rules.Rule `json:"items"`
}

// ruleEventList 是 GET /api/v1/rules/events 的响应形态。
type ruleEventList struct {
	Kind       string        `json:"kind"`
	APIVersion string        `json:"apiVersion"`
	Items      []rules.Event `json:"items"`
}

// governanceResponse 是治理策略查询/替换的响应形态。
type governanceResponse struct {
	Kind       string                   `json:"kind"`
	APIVersion string                   `json:"apiVersion"`
	Version    int64                    `json:"version"` // 当前规则包版本
	Items      []rules.GovernancePolicy `json:"items"`
}

// syncRuleResponse 是规则包下发的成功响应。
type syncRuleResponse struct {
	Status     string `json:"status"`
	NodeID     string `json:"nodeID"`
	Version    int64  `json:"version"`
	Rules      int    `json:"rules"`
	Governance int    `json:"governance"`
}

// createRule 处理 POST /api/v1/rules：校验 → 创建 → 201。
// 重复 ruleId → 409；校验失败 → 400（存储层 ErrInvalid 同映射）。
func (a *ruleAPI) createRule(w http.ResponseWriter, r *http.Request) {
	var rr rules.Rule
	if !decodeRuleBody(w, r, &rr) {
		return
	}
	if err := a.store.CreateRule(r.Context(), rr); err != nil {
		status, msg := storeErrStatus(err)
		writeRuleError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusCreated, rr)
}

// listRules 处理 GET /api/v1/rules：返回全部规则（ruleId 排序）。
func (a *ruleAPI) listRules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, ruleList{
		Kind:       "RuleList",
		APIVersion: "v1",
		Items:      a.store.ListRules(),
	})
}

// getRule 处理 GET /api/v1/rules/{ruleID}；不存在 → 404。
func (a *ruleAPI) getRule(w http.ResponseWriter, r *http.Request) {
	ruleID := r.PathValue("ruleID")
	rr, ok := a.store.GetRule(ruleID)
	if !ok {
		writeRuleError(w, http.StatusNotFound, "rule not found: "+ruleID)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

// updateRule 处理 PUT /api/v1/rules/{ruleID}：body 的 ruleId 缺省补路径值，
// 与路径不一致 → 400；不存在 → 404。
func (a *ruleAPI) updateRule(w http.ResponseWriter, r *http.Request) {
	ruleID := r.PathValue("ruleID")
	var rr rules.Rule
	if !decodeRuleBody(w, r, &rr) {
		return
	}
	if rr.RuleID == "" {
		rr.RuleID = ruleID
	}
	if rr.RuleID != ruleID {
		writeRuleError(w, http.StatusBadRequest, "body ruleId 与路径不一致")
		return
	}
	if err := a.store.UpdateRule(r.Context(), rr); err != nil {
		status, msg := storeErrStatus(err)
		writeRuleError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

// deleteRule 处理 DELETE /api/v1/rules/{ruleID}；不存在 → 404。
func (a *ruleAPI) deleteRule(w http.ResponseWriter, r *http.Request) {
	ruleID := r.PathValue("ruleID")
	if err := a.store.DeleteRule(r.Context(), ruleID); err != nil {
		status, msg := storeErrStatus(err)
		writeRuleError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "ruleId": ruleID})
}

// listEvents 处理 GET /api/v1/rules/events：ruleId/device/limit 过滤，
// 按时间倒序；limit 非法（非整数/负）→ 400，超上限（200）按 200 截断。
func (a *ruleAPI) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeRuleError(w, http.StatusBadRequest, "limit 非法（须为非负整数）")
			return
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}
	evs := a.store.ListEvents(rulestore.EventFilter{
		RuleID:     q.Get("ruleId"),
		DeviceName: q.Get("device"),
		Limit:      limit,
	})
	writeJSON(w, http.StatusOK, ruleEventList{
		Kind:       "RuleEventList",
		APIVersion: "v1",
		Items:      evs,
	})
}

// getGovernance 处理 GET /api/v1/rules/governance：返回策略列表与当前版本。
func (a *ruleAPI) getGovernance(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, governanceResponse{
		Kind:       "GovernancePolicyList",
		APIVersion: "v1",
		Version:    a.store.Version(),
		Items:      a.store.Governance(),
	})
}

// putGovernance 处理 PUT /api/v1/rules/governance：全量替换策略列表。
// body 形态：{"policies": [...]}；校验失败 → 400。
func (a *ruleAPI) putGovernance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policies []rules.GovernancePolicy `json:"policies"`
	}
	if !decodeRuleBody(w, r, &req) {
		return
	}
	if err := a.store.SetGovernance(r.Context(), req.Policies); err != nil {
		status, msg := storeErrStatus(err)
		writeRuleError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, governanceResponse{
		Kind:       "GovernancePolicyList",
		APIVersion: "v1",
		Version:    a.store.Version(),
		Items:      a.store.Governance(),
	})
}

// syncRules 处理 POST /api/v1/nodes/{nodeID}/rules/sync：把当前全量规则包
// 可靠投递到指定节点。响应五态与 syncConfig 一致：200=边缘已确认；
// 404=节点未注册/离线；502=边缘回 error Ack（已送达但拒绝，如版本陈旧）；
// 504=确认超时重试耗尽；500=其他发送失败。
func (a *ruleAPI) syncRules(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	if _, ok := a.reg.Get(nodeID); !ok {
		writeRuleError(w, http.StatusNotFound, "node not found: "+nodeID)
		return
	}
	rs := a.store.RuleSet()
	msg, err := protocol.NewMessage(protocol.TypeRuleSync, "cloud", nodeID, RuleSyncPayload{RuleSet: rs})
	if err != nil {
		writeRuleError(w, http.StatusInternalServerError, "build message failed")
		return
	}
	if err := a.reliableSend(r.Context(), nodeID, msg, cloudhub.ReliableOptions{}); err != nil {
		if errors.Is(err, cloudhub.ErrNodeOffline) {
			writeRuleError(w, http.StatusNotFound, "node offline or not registered")
			return
		}
		if errors.Is(err, cloudhub.ErrAckTimeout) {
			writeRuleError(w, http.StatusGatewayTimeout, "ack timeout after retries")
			return
		}
		if errors.Is(err, cloudhub.ErrAckFailed) {
			writeRuleError(w, http.StatusBadGateway, "edge rejected rule sync")
			return
		}
		writeRuleError(w, http.StatusInternalServerError, "send failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, syncRuleResponse{
		Status:     "ok",
		NodeID:     nodeID,
		Version:    rs.Version,
		Rules:      len(rs.Rules),
		Governance: len(rs.Governance),
	})
}
