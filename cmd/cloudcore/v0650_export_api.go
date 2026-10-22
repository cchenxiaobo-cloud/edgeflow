// 审计导出与转发规则导出/导入 API（v0.65.0，spec 0038）。
//
// 三端点（134→137）：
//
//	GET  /api/v1/audit/entries/export   审计台账 JSONL 导出（operator+——含 IP 敏感信息）
//	GET  /api/v1/forwards/export        转发规则脱敏 JSON 导出（admin）
//	POST /api/v1/forwards/import        转发规则 JSON 导入（admin；去重跳过已有 ID）
//
// 导出为流式只读（Content-Disposition 附件下载）；导入逐条校验（失败条目
// 跳过计数），返回 {imported, skipped}。RBAC 对齐 v0.46 矩阵。
package main

import (
	"io"
	"net/http"
	"os"
	"time"

	"edgeflow/cloud/pkg/audit"
	"edgeflow/cloud/pkg/forwardstore"
)

// registerExportAPIs 注册审计导出与转发规则导出/导入 3 端点。
func registerExportAPIs(mux *http.ServeMux, ledger *audit.Ledger, fs *forwardstore.Store) {
	// ─── 审计台账导出（JSONL 原样流式）───
	mux.HandleFunc("GET /api/v1/audit/entries/export", func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(ledger.Path())
		if err != nil {
			if os.IsNotExist(err) {
				w.Header().Set("Content-Type", "application/jsonl")
				w.WriteHeader(http.StatusOK)
				return
			}
			writeErr(w, http.StatusInternalServerError, "台账读取失败: "+err.Error(), nil)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/jsonl")
		w.Header().Set("Content-Disposition", "attachment; filename=audit-ledger-export.jsonl")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
	})

	// ─── 转发规则导出（脱敏 JSON）───
	mux.HandleFunc("GET /api/v1/forwards/export", func(w http.ResponseWriter, r *http.Request) {
		rules := fs.List()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", "attachment; filename=forward-rules-export.json")
		writeJSON(w, http.StatusOK, map[string]any{"exportedAt": time.Now().UnixMilli(), "rules": rules})
	})

	// ─── 转发规则导入 ───
	mux.HandleFunc("POST /api/v1/forwards/import", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Rules []forwardstore.ImportRule `json:"rules"`
		}
		if err := decodeForwardsBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), nil)
			return
		}
		result := fs.Import(body.Rules, time.Now().UnixMilli())
		writeJSON(w, http.StatusOK, result)
	})
}
