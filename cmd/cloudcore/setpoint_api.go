// 设定值通道 API（v0.40.0，spec 0013 US-4/US-6）：3 个端点 + 投递 flush。
//
//	POST /api/v1/nodes/{nodeID}/setpoints         建单（审批开关/单笔 requireApproval）
//	POST /api/v1/setpoints/{setpointID}/approval  审批（approve|reject，仅 pending-approval 可审）
//	GET  /api/v1/setpoints                        列表（nodeID/state/limit 过滤，含执行反馈）
//
// 投递：flushOnce 扫描 pending-send → ReliableSend（QoS1，同 MsgID 幂等）→
// sent；节点离线留单下轮重投（断网恢复自动同步）；Ack error → failed 终态。
// 执行反馈：SetpointResult 回告 → setpointstore.ApplyResult（applied/failed）。
// 注册到 apiMux → auth/audit 链自动覆盖；operator 必填（审计留痕）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/setpointstore"
	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// 设定值环境变量（spec 0013 US-4/US-6 修订）。
const (
	envSetpointApproval  = "EDGEFLOW_CLOUDCORE_SETPOINT_APPROVAL"
	envSetpointFlushSec  = "EDGEFLOW_CLOUDCORE_SETPOINT_FLUSH_SEC"
	envSetpointRedeliver = "EDGEFLOW_CLOUDCORE_SETPOINT_REDELIVER_SEC"

	defaultSetpointFlushSec    = 30
	defaultSetpointRedeliverMs = 90 * 1000 // sent 未回告重投窗口（毫秒）；0 禁用重投
)

// setpointAPI 是设定值通道的 HTTP 处理器集合 + 投递器。
type setpointAPI struct {
	store          *setpointstore.Store
	hub            *cloudhub.Server
	approvalForced bool // 审批开关：on 时全部建单进入 pending-approval
	// nodeExists 是节点存在性校验（默认经注册表；测试注入 fake；
	// spec 0013 US-4：建单时节点不存在 → 404）。
	nodeExists func(nodeID string) bool
	// reliableSend 是可靠投递函数（默认 hub.ReliableSendContext；测试注入 fake，
	// 与 ruleAPI.reliableSend 同模式）。
	reliableSend func(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error
	// redeliverMs 是 sent 未回告重投窗口（毫秒；0 禁用；默认 90s）。
	redeliverMs int64
}

// Register 注册 3 条设定值端点。
func (a *setpointAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/setpoints", a.createSetpoint)
	mux.HandleFunc("POST /api/v1/setpoints/{setpointID}/approval", a.approveSetpoint)
	mux.HandleFunc("GET /api/v1/setpoints", a.listSetpoints)
}

// setpointCreateBody 是建单请求体（operator 必填）。
type setpointCreateBody struct {
	Namespace       string  `json:"namespace"`
	DeviceName      string  `json:"deviceName"`
	Property        string  `json:"property"`
	Value           float64 `json:"value"`
	Operator        string  `json:"operator"`
	RequireApproval bool    `json:"requireApproval"`
}

// createSetpoint 处理 POST /api/v1/nodes/{nodeID}/setpoints。
// 语义：建单即留痕（审计），投递异步（flush 循环）；审批开关 on 或单笔
// requireApproval → pending-approval（等待 /approval 端点审决）。
func (a *setpointAPI) createSetpoint(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	if a.nodeExists != nil && !a.nodeExists(nodeID) {
		http.Error(w, `{"error":"node not found: `+nodeID+`"}`, http.StatusNotFound)
		return
	}
	var body setpointCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"请求体解析失败"}`, http.StatusBadRequest)
		return
	}
	sp, err := a.store.Create(r.Context(), setpointstore.CreateInput{
		NodeID:          nodeID,
		Namespace:       body.Namespace,
		DeviceName:      body.DeviceName,
		Property:        body.Property,
		Value:           body.Value,
		Operator:        body.Operator,
		RequireApproval: body.RequireApproval,
		ApprovalForced:  a.approvalForced,
		NowMs:           time.Now().UnixMilli(),
	})
	switch {
	case err == nil:
		writeJSONAlarm(w, http.StatusOK, sp)
	case errors.Is(err, setpointstore.ErrValidation):
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
	default:
		http.Error(w, `{"error":"内部错误"}`, http.StatusInternalServerError)
	}
}

// setpointApprovalBody 是审批请求体。
type setpointApprovalBody struct {
	Action   string `json:"action"` // approve | reject
	Operator string `json:"operator"`
}

// approveSetpoint 处理 POST /api/v1/setpoints/{setpointID}/approval。
func (a *setpointAPI) approveSetpoint(w http.ResponseWriter, r *http.Request) {
	var body setpointApprovalBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"请求体解析失败"}`, http.StatusBadRequest)
		return
	}
	if body.Operator == "" {
		http.Error(w, `{"error":"缺少 operator（审计留痕必填）"}`, http.StatusBadRequest)
		return
	}
	var (
		sp  setpointstore.Setpoint
		err error
	)
	switch body.Action {
	case "approve":
		sp, err = a.store.Approve(r.Context(), r.PathValue("setpointID"), body.Operator, time.Now().UnixMilli())
	case "reject":
		sp, err = a.store.Reject(r.Context(), r.PathValue("setpointID"), body.Operator, time.Now().UnixMilli())
	default:
		http.Error(w, `{"error":"action 须为 approve 或 reject"}`, http.StatusBadRequest)
		return
	}
	switch {
	case err == nil:
		writeJSONAlarm(w, http.StatusOK, sp)
	case errors.Is(err, setpointstore.ErrNotFound):
		http.Error(w, `{"error":"设定值建单不存在"}`, http.StatusNotFound)
	case errors.Is(err, setpointstore.ErrInvalidState):
		http.Error(w, `{"error":"设定值状态不允许审批"}`, http.StatusConflict)
	default:
		http.Error(w, `{"error":"内部错误"}`, http.StatusInternalServerError)
	}
}

// listSetpoints 处理 GET /api/v1/setpoints?nodeID=&state=&limit=。
func (a *setpointAPI) listSetpoints(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	sps := a.store.List(r.URL.Query().Get("nodeID"), r.URL.Query().Get("state"), limit)
	writeJSONAlarm(w, http.StatusOK, map[string]any{
		"setpoints": sps,
		"count":     len(sps),
	})
}

// runFlushLoop 周期投递循环（ctx 取消退出）。
func (a *setpointAPI) runFlushLoop(ctx context.Context, sec int) {
	ticker := time.NewTicker(time.Duration(sec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.flushOnce(ctx)
		}
	}
}

// flushOnce 投递一轮：pending-send → ReliableSend（class=setpoint，同 MsgID
// 幂等）→ sent。节点离线留单下轮重投（断网恢复自动同步，spec 0013 US-6）；
// Ack error（边缘拒绝执行）→ failed 终态；AckTimeout 留单（重发同 ID，
// 边缘重复应用幂等）。汇总一行日志（避免逐条刷屏）。
func (a *setpointAPI) flushOnce(ctx context.Context) {
	pending := a.store.PendingForFlush()
	sentStale := a.store.SentForRedelivery(a.redeliverMs, time.Now().UnixMilli())
	if len(pending) == 0 && len(sentStale) == 0 {
		return
	}
	var sentOK, offline, ackTimeout, failed int
	deliver := func(sp setpointstore.Setpoint, msgID string, markSent bool) {
		msg, err := protocol.NewMessage(protocol.TypeDeviceCommand, "cloud", sp.NodeID,
			devicetwin.DeviceCommandPayload{
				DeviceName: sp.DeviceName,
				Namespace:  sp.Namespace,
				Property:   sp.Property,
				Value:      sp.Value,
				Class:      "setpoint",
				SetpointID: sp.SetpointID,
			})
		if err != nil {
			log.Warnf("[setpoint] 构造指令消息失败（setpointId=%s）: %v", sp.SetpointID, err)
			return
		}
		msg.ID = msgID
		err = a.reliableSend(ctx, sp.NodeID, msg, cloudhub.ReliableOptions{})
		now := time.Now().UnixMilli()
		switch {
		case err == nil:
			if markSent {
				if _, merr := a.store.MarkSent(ctx, sp.SetpointID, now); merr != nil {
					log.Warnf("[setpoint] MarkSent 失败（setpointId=%s）: %v", sp.SetpointID, merr)
				}
			} else {
				if _, merr := a.store.MarkRedelivered(ctx, sp.SetpointID, now); merr != nil {
					log.Warnf("[setpoint] MarkRedelivered 失败（setpointId=%s）: %v", sp.SetpointID, merr)
				}
			}
			sentOK++
		case errors.Is(err, cloudhub.ErrNodeOffline):
			offline++ // 留单下轮（节点恢复后自动同步）
		case errors.Is(err, cloudhub.ErrAckTimeout):
			ackTimeout++ // 留单：可能已送达（回告将闭环终态）
		case errors.Is(err, cloudhub.ErrAckFailed):
			failed++
			if _, merr := a.store.MarkFailed(ctx, sp.SetpointID, err.Error(), now); merr != nil {
				log.Warnf("[setpoint] MarkFailed 失败（setpointId=%s）: %v", sp.SetpointID, merr)
			}
		default:
			// shutting down 等：留单下轮
		}
	}
	for _, sp := range pending {
		deliver(sp, sp.MsgID, true) // 首投用建单 MsgID（可靠投递重试同 ID 幂等）
	}
	// sent 未回告重投（spec 0013 US-6 修订）：恒用新投递 ID（边缘 MsgID 去重会
	// 短路同 ID），边缘重复应用幂等 + 重新回告 → applied 闭环。
	for _, sp := range sentStale {
		deliver(sp, sp.RedeliveryMsgID(), false)
	}
	log.Infof("[setpoint] 投递轮：待投 %d（含 sent 重投 %d），成功 %d，离线 %d，超时 %d，失败 %d",
		len(pending)+len(sentStale), len(sentStale), sentOK, offline, ackTimeout, failed)
}

// parseSetpointFlushSec 读取 flush 周期 env（非法回退默认 30，告警）。
func parseSetpointFlushSec() int {
	v := os.Getenv(envSetpointFlushSec)
	if v == "" {
		return defaultSetpointFlushSec
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		log.Warnf("%s 非法（%q），回退默认 %d", envSetpointFlushSec, v, defaultSetpointFlushSec)
		return defaultSetpointFlushSec
	}
	return n
}

// parseSetpointRedeliverMs 读取 sent 未回告重投窗口 env（毫秒；非法回退默认
// 90s 并告警；0 = 禁用重投，登记 KI §41）。
func parseSetpointRedeliverMs() int64 {
	v := os.Getenv(envSetpointRedeliver)
	if v == "" {
		return defaultSetpointRedeliverMs
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		log.Warnf("%s 非法（%q），回退默认 %dms", envSetpointRedeliver, v, defaultSetpointRedeliverMs)
		return defaultSetpointRedeliverMs
	}
	return n
}
