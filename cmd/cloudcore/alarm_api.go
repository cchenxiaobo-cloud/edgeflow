// 统一告警中心 API（v0.40.0，spec 0013 US-3）：5 个端点（契约扩容轮）。
//
//	GET  /api/v1/alarms                     告警列表（nodeID/state/severity/limit 过滤）
//	GET  /api/v1/alarms/stats               统计（byState/bySeverity/total）
//	POST /api/v1/alarms/{alarmID}/ack       确认（raised → acked；assigned 后确认拒绝 409）
//	POST /api/v1/alarms/{alarmID}/assign    派单（工单集成点回调，raised/acked → assigned）
//	POST /api/v1/alarms/{alarmID}/close     闭环（任意非 closed → closed 终态）
//
// 注册到 apiMux → auth/audit 链自动覆盖；operator 必填（审计留痕）。
// 错误映射：alarmstore.ErrNotFound→404、ErrInvalidTransition→409、参数缺失→400。
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"edgeflow/cloud/pkg/alarmstore"
	"edgeflow/pkg/alarm"
)

// alarmAPI 是统一告警中心的 HTTP 处理器集合。
type alarmAPI struct {
	store *alarmstore.Store
}

// Register 注册 5 条告警端点（Go 1.22 路径参数模式，与 rules_api 同风格）。
func (a *alarmAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/alarms", a.listAlarms)
	mux.HandleFunc("GET /api/v1/alarms/stats", a.stats)
	mux.HandleFunc("POST /api/v1/alarms/{alarmID}/ack", a.ackAlarm)
	mux.HandleFunc("POST /api/v1/alarms/{alarmID}/assign", a.assignAlarm)
	mux.HandleFunc("POST /api/v1/alarms/{alarmID}/close", a.closeAlarm)
}

// listAlarms 处理 GET /api/v1/alarms?nodeID=&state=&severity=&limit=。
func (a *alarmAPI) listAlarms(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	alarms := a.store.List(
		r.URL.Query().Get("nodeID"),
		r.URL.Query().Get("state"),
		r.URL.Query().Get("severity"),
		limit)
	writeJSONAlarm(w, http.StatusOK, map[string]any{
		"alarms": alarms,
		"count":  len(alarms),
	})
}

// stats 处理 GET /api/v1/alarms/stats。
func (a *alarmAPI) stats(w http.ResponseWriter, r *http.Request) {
	writeJSONAlarm(w, http.StatusOK, a.store.Stats())
}

// alarmOpBody 是 ack/assign/close 的请求体（operator 必填）。
type alarmOpBody struct {
	Operator  string `json:"operator"`
	Assignee  string `json:"assignee,omitempty"`  // assign 专用
	TicketRef string `json:"ticketRef,omitempty"` // assign 专用（工单引用）
}

// ackAlarm 处理 POST /api/v1/alarms/{alarmID}/ack。
func (a *alarmAPI) ackAlarm(w http.ResponseWriter, r *http.Request) {
	var body alarmOpBody
	if !decodeAlarmBody(w, r, &body) {
		return
	}
	updated, err := a.store.Ack(r.Context(), r.PathValue("alarmID"), body.Operator, time.Now().UnixMilli())
	writeAlarmResult(w, updated, err)
}

// assignAlarm 处理 POST /api/v1/alarms/{alarmID}/assign。
func (a *alarmAPI) assignAlarm(w http.ResponseWriter, r *http.Request) {
	var body alarmOpBody
	if !decodeAlarmBody(w, r, &body) {
		return
	}
	if body.Assignee == "" {
		http.Error(w, `{"error":"缺少 assignee"}`, http.StatusBadRequest)
		return
	}
	updated, err := a.store.Assign(r.Context(), r.PathValue("alarmID"), body.Operator,
		body.Assignee, body.TicketRef, time.Now().UnixMilli())
	writeAlarmResult(w, updated, err)
}

// closeAlarm 处理 POST /api/v1/alarms/{alarmID}/close。
func (a *alarmAPI) closeAlarm(w http.ResponseWriter, r *http.Request) {
	var body alarmOpBody
	if !decodeAlarmBody(w, r, &body) {
		return
	}
	updated, err := a.store.Close(r.Context(), r.PathValue("alarmID"), body.Operator, time.Now().UnixMilli())
	writeAlarmResult(w, updated, err)
}

// decodeAlarmBody 解析请求体并校验 operator（失败已写响应，返回 false）。
func decodeAlarmBody(w http.ResponseWriter, r *http.Request, body *alarmOpBody) bool {
	if err := json.NewDecoder(r.Body).Decode(body); err != nil {
		http.Error(w, `{"error":"请求体解析失败"}`, http.StatusBadRequest)
		return false
	}
	if body.Operator == "" {
		http.Error(w, `{"error":"缺少 operator（审计留痕必填）"}`, http.StatusBadRequest)
		return false
	}
	return true
}

// writeAlarmResult 统一写操作结果（错误映射 404/409/500）。
func writeAlarmResult(w http.ResponseWriter, a alarm.Alarm, err error) {
	switch {
	case err == nil:
		writeJSONAlarm(w, http.StatusOK, a)
	case errors.Is(err, alarmstore.ErrNotFound):
		http.Error(w, `{"error":"告警不存在"}`, http.StatusNotFound)
	case errors.Is(err, alarmstore.ErrInvalidTransition):
		http.Error(w, `{"error":"告警状态迁移非法"}`, http.StatusConflict)
	default:
		http.Error(w, `{"error":"内部错误"}`, http.StatusInternalServerError)
	}
}

// writeJSONAlarm 写 JSON 响应（序列化失败降级 500）。
func writeJSONAlarm(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 头已写出：仅告警（连接关闭由 http 层处理）。
		_ = v
	}
}
