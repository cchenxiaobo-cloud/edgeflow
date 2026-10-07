// 困难样本检索面（v0.45.0，spec 0018 US-4，G25）。
//
// 端点（挂 apiMux——auth/audit 覆盖）：
//
//	GET /api/v1/hardsamples?nodeID=&deviceName=&alarmId=&limit=
//	    → 200 {"items":[{mediaId,nodeId,deviceName,alarmId,capturedAt,bytes,sha256}],"count":n}
//	GET /api/v1/hardsamples/{mediaId}/content
//	    → 200 image/jpeg 字节流；404 未知/未完成
//
// 查询语义：全过滤条件可选；limit 默认 50、上限 200（mediastore.ListHardSamples
// 内截断）；完成时间倒序。样本对象由 mediastore 托管（objects/hardsample/ 前缀）。
package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"edgeflow/cloud/pkg/mediastore"
	"edgeflow/pkg/log"
)

// hardsampleItem 是列表项（Media 的检索视图——不暴露内部状态字段）。
type hardsampleItem struct {
	MediaID     string `json:"mediaId"`
	NodeID      string `json:"nodeId"`
	DeviceName  string `json:"deviceName"`
	AlarmID     string `json:"alarmId,omitempty"`
	CapturedAt  int64  `json:"capturedAt"`
	ContentType string `json:"contentType"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
}

// hardsampleAPI 困难样本检索 API（media 为 mediastore 实例）。
type hardsampleAPI struct {
	media *mediastore.Store
}

// Register 挂载路由（apiMux）。
func (a *hardsampleAPI) Register(mux routeRegistrar) {
	mux.HandleFunc("GET /api/v1/hardsamples", a.list)
	mux.HandleFunc("GET /api/v1/hardsamples/{mediaId}/content", a.content)
}

// list 处理 GET /api/v1/hardsamples。
func (a *hardsampleAPI) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := mediastore.HardSampleFilter{
		NodeID:     q.Get("nodeID"),
		DeviceName: q.Get("deviceName"),
		AlarmID:    q.Get("alarmId"),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	items := a.media.ListHardSamples(f)
	out := make([]hardsampleItem, 0, len(items))
	for _, m := range items {
		out = append(out, hardsampleItem{
			MediaID:     m.MediaID,
			NodeID:      m.NodeID,
			DeviceName:  m.DeviceName,
			AlarmID:     m.AlarmID,
			CapturedAt:  m.CapturedAt,
			ContentType: m.ContentType,
			Bytes:       m.TotalBytes,
			SHA256:      m.SHA256,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(map[string]any{"items": out, "count": len(out)}); err != nil {
		logEncodeError("hardsamples.list", err)
	}
}

// content 处理 GET /api/v1/hardsamples/{mediaId}/content：JPEG 字节流。
func (a *hardsampleAPI) content(w http.ResponseWriter, r *http.Request) {
	mediaID := r.PathValue("mediaId")
	b, err := a.media.Read(mediaID)
	if err != nil {
		code := http.StatusNotFound
		if err == mediastore.ErrIncomplete {
			code = http.StatusConflict
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "hardsample unavailable", "mediaId": mediaID})
		return
	}
	m, err := a.media.Get(mediaID)
	if err == nil && m.ContentType != "" {
		w.Header().Set("Content-Type", m.ContentType)
	} else {
		w.Header().Set("Content-Type", "image/jpeg")
	}
	if _, err := w.Write(b); err != nil {
		log.Warnf("[hardsamples] 写响应失败（mediaId=%s）: %v", mediaID, err)
	}
}
