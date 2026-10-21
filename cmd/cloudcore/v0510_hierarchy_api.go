// 区域协同层 API（v0.51.0，spec 0024，G01）。
//
// 五端点（114→119）：
//
//	GET /api/v1/hierarchy/summary            三级树形汇总（递归聚合，viewer+）
//	GET /api/v1/hierarchy/regions/{id}/nodes 区域节点明细（含子树，viewer+）
//	GET /api/v1/hierarchy/alarms             区域×状态告警计数（viewer+）
//	GET /api/v1/hierarchy/regions/{id}/policy 区域策略读取（viewer+）
//	PUT /api/v1/hierarchy/regions/{id}/policy 区域策略 upsert/清除（operator+）
//
// RBAC 由全局 middleware 统一管控（PUT=write operator+ / GET=read viewer+；
// 本面无 DELETE——规避 operator 无 delete 语义，v0.46 矩阵只增不改）；
// 越权在 middleware 层 403，本文件不重复判定。
// 路径裁决：中心直管架构下提供区域级视图+策略+数据汇聚能力面（replan 路径 3
// 增强）；聚合口径见 cloud/pkg/hierarchyview 包注释。
package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"edgeflow/cloud/pkg/hierarchyview"
	"edgeflow/cloud/pkg/regionstore"
)

// policyBodyLimit 是区域策略请求体上限（4KB，spec 裁决 4）。
const policyBodyLimit = 4096

// registerHierarchyAPIs 注册区域协同 5 端点。
func registerHierarchyAPIs(mux *http.ServeMux, regions *regionstore.Store, view *hierarchyview.View) {
	// ─── 三级树形汇总 ───
	mux.HandleFunc("GET /api/v1/hierarchy/summary", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, view.Summary())
	})

	// ─── 区域节点明细（含子树）───
	mux.HandleFunc("GET /api/v1/hierarchy/regions/{id}/nodes", func(w http.ResponseWriter, r *http.Request) {
		rows, ok := view.RegionNodes(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "区域不存在", nil)
			return
		}
		if rows == nil {
			rows = []hierarchyview.NodeRow{} // 空区域返回空数组而非 null
		}
		writeJSON(w, http.StatusOK, rows)
	})

	// ─── 区域告警汇聚 ───
	mux.HandleFunc("GET /api/v1/hierarchy/alarms", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, view.AlarmStats())
	})

	// ─── 区域策略读取 ───
	mux.HandleFunc("GET /api/v1/hierarchy/regions/{id}/policy", func(w http.ResponseWriter, r *http.Request) {
		reg, ok := regions.RegionByID(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "区域不存在", nil)
			return
		}
		// 无策略返回 null（语义：未设置）；有策略原样透出 JSON
		w.Header().Set("Content-Type", "application/json")
		if len(reg.Policy) == 0 {
			_, _ = w.Write([]byte("null\n"))
			return
		}
		_, _ = w.Write(append(reg.Policy, '\n'))
	})

	// ─── 区域策略 upsert/清除 ───
	mux.HandleFunc("PUT /api/v1/hierarchy/regions/{id}/policy", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, policyBodyLimit+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "请求体读取失败: "+err.Error(), nil)
			return
		}
		if len(body) > policyBodyLimit {
			writeErr(w, http.StatusBadRequest, "策略体超限（>4KB）", nil)
			return
		}
		trimmed := trimJSONSpace(body)
		if len(trimmed) == 0 {
			writeErr(w, http.StatusBadRequest, "策略体为空（null 或 {} 清除）", nil)
			return
		}
		if err := regions.SetPolicy(r.PathValue("id"), json.RawMessage(trimmed), time.Now().UnixMilli()); err != nil {
			switch {
			case errors.Is(err, regionstore.ErrNotFound):
				writeErr(w, http.StatusNotFound, err.Error(), nil)
			case errors.Is(err, regionstore.ErrPolicyInvalid):
				writeErr(w, http.StatusBadRequest, "策略须为合法 JSON 对象且 ≤4KB", nil)
			default:
				writeErr(w, http.StatusInternalServerError, err.Error(), nil)
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// trimJSONSpace 去除请求体首尾空白。
func trimJSONSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\n' || b[start] == '\r') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\n' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}
