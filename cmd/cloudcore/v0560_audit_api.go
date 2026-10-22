// 审计台账查询 API（v0.56.0，spec 0029）。
//
// 两端点（128→130）：
//
//	GET /api/v1/audit/entries  台账查询（operator/method/pathSub/status/from/to/
//	                           limit/offset；Ts 降序 新→旧；viewer+）
//	GET /api/v1/audit/stats    台账聚合（同过滤语义：总数/按操作者/按动作/状态桶）
//
// RBAC 由全局 middleware 统一管控（GET=read viewer+）；无写面/无 DELETE——
// 台账为追加写不可变记录，保留 30 天自动清理（audit 包既有语义）。
// 查询为文件扫描（量级：管理面请求频率 × 保留期，KB~MB 级；spec 裁决 2）。
package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"edgeflow/cloud/pkg/audit"
)

// registerAuditAPIs 注册审计台账查询 2 端点。
func registerAuditAPIs(mux *http.ServeMux, ledger *audit.Ledger) {
	// ─── 台账查询 ───
	mux.HandleFunc("GET /api/v1/audit/entries", func(w http.ResponseWriter, r *http.Request) {
		f, limit, offset, perr := auditFilterFrom(r)
		if perr != nil {
			writeErr(w, http.StatusBadRequest, perr.Error(), nil)
			return
		}
		entries, total, err := ledger.Query(f, limit, offset)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "台账查询失败: "+err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": total})
	})

	// ─── 台账聚合 ───
	mux.HandleFunc("GET /api/v1/audit/stats", func(w http.ResponseWriter, r *http.Request) {
		f, _, _, perr := auditFilterFrom(r)
		if perr != nil {
			writeErr(w, http.StatusBadRequest, perr.Error(), nil)
			return
		}
		st, err := ledger.Stats(f)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "台账聚合失败: "+err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
}

// auditFilterFrom 从查询串归一过滤器（from/to 毫秒时间戳；method 大写归一）。
// v0.61.0（R2 P3-2 处置）：非法数值参数（limit/offset/status 负值）由调用方
// 显式 400——本函数仅做语法转换，语义校验在 handler。
func auditFilterFrom(r *http.Request) (audit.QueryFilter, int, int, error) {
	q := r.URL.Query()
	status, serr := strconv.Atoi(q.Get("status"))
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	limit, lerr := strconv.Atoi(q.Get("limit"))
	offset, oerr := strconv.Atoi(q.Get("offset"))
	if (q.Get("status") != "" && serr != nil) || (q.Get("limit") != "" && lerr != nil) ||
		(q.Get("offset") != "" && oerr != nil) {
		return audit.QueryFilter{}, 0, 0, errors.New("status/limit/offset 须为整数")
	}
	if status < 0 || limit < 0 || offset < 0 {
		return audit.QueryFilter{}, 0, 0, errors.New("status/limit/offset 不可为负")
	}
	f := audit.QueryFilter{
		Operator: q.Get("operator"),
		Method:   strings.ToUpper(q.Get("method")),
		PathSub:  q.Get("pathSub"),
		Status:   status,
		FromMs:   from,
		ToMs:     to,
	}
	return f, limit, offset, nil
}
