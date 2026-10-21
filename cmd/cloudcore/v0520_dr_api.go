// 灾备备份管理 API（v0.52.0，spec 0025，DR-ASSESSMENT 差距 #5/#4 承接）。
//
// 三端点（119→122）：
//
//	POST /api/v1/dr/backups      创建备份（operator+；未启用 503）
//	GET  /api/v1/dr/backups      备份清单（viewer+；{enabled,backups}）
//	GET  /api/v1/dr/backups/{id} 备份详情（viewer+；?verify=true 触发 sha256 复核）
//
// RBAC 由全局 middleware 统一管控（POST=write operator+ / GET=read viewer+）；
// 无 DELETE 面——轮转自动保留 Keep 份（spec 裁决 4）；恢复执行走演练 SOP
// （scripts/dr-drill.sh + docs/DR-RUNBOOK.md），不设 API 恢复面（防误操作放大）。
// 备份对象与并发模型见 cloud/pkg/drstore 包注释。
package main

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"edgeflow/cloud/pkg/drstore"
)

// registerDRAPIs 注册灾备备份 3 端点。
func registerDRAPIs(mux *http.ServeMux, dr *drstore.Store) {
	// ─── 创建备份 ───
	mux.HandleFunc("POST /api/v1/dr/backups", func(w http.ResponseWriter, r *http.Request) {
		if !dr.Enabled() {
			writeErr(w, http.StatusServiceUnavailable, "灾备备份未启用（EDGEFLOW_CLOUDCORE_DR_DIR 未配置）", nil)
			return
		}
		b, err := dr.Create("manual", time.Now().UnixMilli())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "备份创建失败: "+err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusOK, b)
	})

	// ─── 备份清单 ───
	mux.HandleFunc("GET /api/v1/dr/backups", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"enabled": dr.Enabled(), "backups": []drstore.Backup{}}
		if dr.Enabled() {
			list, err := dr.List()
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "备份清单读取失败: "+err.Error(), nil)
				return
			}
			resp["backups"] = list
		}
		writeJSON(w, http.StatusOK, resp)
	})

	// ─── 备份详情（可选完整性复核）───
	mux.HandleFunc("GET /api/v1/dr/backups/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !dr.Enabled() {
			writeErr(w, http.StatusServiceUnavailable, "灾备备份未启用（EDGEFLOW_CLOUDCORE_DR_DIR 未配置）", nil)
			return
		}
		id := r.PathValue("id")
		if r.URL.Query().Get("verify") == "true" {
			b, err := dr.Verify(id)
			if err != nil {
				writeDRErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, b)
			return
		}
		b, err := dr.Get(id)
		if err != nil {
			writeDRErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, b)
	})
}

// writeDRErr 归一 drstore 错误到状态码。
func writeDRErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, drstore.ErrDisabled):
		writeErr(w, http.StatusServiceUnavailable, "灾备备份未启用（EDGEFLOW_CLOUDCORE_DR_DIR 未配置）", nil)
	case errors.Is(err, drstore.ErrNotFound):
		writeErr(w, http.StatusNotFound, "备份不存在", nil)
	default:
		writeErr(w, http.StatusInternalServerError, err.Error(), nil)
	}
}

// envDRKeep 读取备份保留份数（DR_KEEP；≤0 → 默认 7；>100 → 收敛 100）。
func envDRKeep() int {
	n, _ := strconv.Atoi(os.Getenv("EDGEFLOW_CLOUDCORE_DR_KEEP"))
	if n <= 0 {
		n = 7
	}
	if n > 100 {
		n = 100
	}
	return n
}
