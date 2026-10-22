// 告警静默窗口 API（v0.58.0，spec 0031）。
//
// 三端点（130→133）：
//
//	GET    /api/v1/alerts/mutes       静默窗口清单（viewer+；每条附 active 实时标记）
//	POST   /api/v1/alerts/mutes       创建窗口（operator+；durationSec 秒，1..2592000）
//	DELETE /api/v1/alerts/mutes/{id}  删除窗口（admin——对齐 v0.46 矩阵）
//
// 窗口不可变（无 PUT——调整 = 删除重建）；过期惰性失效（active 实时计算，
// 快照保留历史留痕）；语义边界见 cloud/pkg/mutestore 包注释与 spec 0031。
package main

import (
	"errors"
	"net/http"
	"time"

	"edgeflow/cloud/pkg/audit"
	"edgeflow/cloud/pkg/mutestore"
)

// registerMuteAPIs 注册静默窗口 3 端点。
func registerMuteAPIs(mux *http.ServeMux, ms *mutestore.Store) {
	// ─── 清单 ───
	mux.HandleFunc("GET /api/v1/alerts/mutes", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UnixMilli()
		list := ms.List()
		type muteView struct {
			mutestore.Silence
			Active bool `json:"active"` // 实时生效标记（过期窗口保留留痕）
		}
		out := make([]muteView, 0, len(list))
		for _, sil := range list {
			out = append(out, muteView{Silence: sil, Active: sil.Active(now)})
		}
		writeJSON(w, http.StatusOK, map[string]any{"mutes": out, "now": now})
	})

	// ─── 创建 ───
	mux.HandleFunc("POST /api/v1/alerts/mutes", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NodeID      string `json:"nodeId"`      // 空 = 全部节点
			Severity    string `json:"severity"`    // v0.62.0：空 = 全部级别（仅作用于转发过滤）
			DurationSec int64  `json:"durationSec"` // 窗口时长（秒；1..2592000）
			Reason      string `json:"reason"`
		}
		if err := decodeForwardsBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), nil)
			return
		}
		now := time.Now().UnixMilli()
		sil, err := ms.Create(body.NodeID, body.Severity, now, now+body.DurationSec*1000, body.Reason, audit.IdentityOf(r.Context()), now)
		if err != nil {
			writeMuteErr(w, err)
			return
		}
		// 响应带 active 实时标记（与 GET 清单视图同型；新窗口必然 active）。
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": sil.ID, "nodeId": sil.NodeID, "severity": sil.Severity, "from": sil.FromMs, "to": sil.ToMs,
			"reason": sil.Reason, "createdBy": sil.CreatedBy, "createdAt": sil.CreatedAt,
			"active": sil.Active(now),
		})
	})

	// ─── 统计（v0.63.0，spec 0036）───
	mux.HandleFunc("GET /api/v1/alerts/mutes/stats", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UnixMilli()
		list := ms.List()
		active, byNode := 0, map[string]int64{}
		for _, sil := range list {
			if sil.Active(now) {
				active++
				key := sil.NodeID
				if key == "" {
					key = "*"
				}
				byNode[key]++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total": len(list), "active": active, "byNode": byNode, "now": now,
		})
	})

	// ─── 删除（admin）───
	mux.HandleFunc("DELETE /api/v1/alerts/mutes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := ms.Delete(r.PathValue("id")); err != nil {
			writeMuteErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// writeMuteErr 归一 mutestore 错误到状态码。
func writeMuteErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mutestore.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
	case errors.Is(err, mutestore.ErrNotFound):
		writeErr(w, http.StatusNotFound, "静默窗口不存在", nil)
	default:
		writeErr(w, http.StatusInternalServerError, err.Error(), nil)
	}
}
