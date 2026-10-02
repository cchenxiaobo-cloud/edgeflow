// 视频管理面 API（v0.43.0，spec 0016 US-4）：8 个端点（契约扩容轮）。
//
//	GET    /api/v1/videostreams                         视频流列表（nodeID 过滤）
//	POST   /api/v1/videostreams                         创建视频流（name/nodeId/deviceName 必填）
//	GET    /api/v1/videostreams/{name}                  详情（含片段索引）
//	PUT    /api/v1/videostreams/{name}                  更新（deviceName/sourceType/status/description）
//	DELETE /api/v1/videostreams/{name}                  删除索引（不删媒资文件）
//	GET    /api/v1/videostreams/{name}/snapshot         最新快照（image/jpeg）
//	GET    /api/v1/videostreams/{name}/segments         片段索引列表
//	GET    /api/v1/videostreams/{name}/segments/{mediaID} 片段回放（video/x-mjpeg）
//
// 注册到 apiMux → auth/audit 链自动覆盖。
// 错误映射：videostream.ErrNotFound→404、ErrExists→409、参数缺失→400、
// 媒资未完成/缺失→404。
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"edgeflow/cloud/pkg/mediastore"
	"edgeflow/cloud/pkg/videostream"
)

// videoAPI 是视频管理面的 HTTP 处理器集合。
type videoAPI struct {
	streams *videostream.Store
	media   *mediastore.Store
}

// Register 注册 8 条视频管理端点（Go 1.22 路径参数模式，与 alarm_api 同风格）。
func (a *videoAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/videostreams", a.listStreams)
	mux.HandleFunc("POST /api/v1/videostreams", a.createStream)
	mux.HandleFunc("GET /api/v1/videostreams/{name}", a.getStream)
	mux.HandleFunc("PUT /api/v1/videostreams/{name}", a.updateStream)
	mux.HandleFunc("DELETE /api/v1/videostreams/{name}", a.deleteStream)
	mux.HandleFunc("GET /api/v1/videostreams/{name}/snapshot", a.getSnapshot)
	mux.HandleFunc("GET /api/v1/videostreams/{name}/segments", a.listSegments)
	mux.HandleFunc("GET /api/v1/videostreams/{name}/segments/{mediaID}", a.getSegment)
}

// videoStreamError 把存储错误映射为状态码。
func videoStreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, videostream.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error(), nil)
	case errors.Is(err, videostream.ErrExists):
		writeErr(w, http.StatusConflict, err.Error(), nil)
	default:
		writeErr(w, http.StatusInternalServerError, err.Error(), nil)
	}
}

// listStreams 处理 GET /api/v1/videostreams?nodeID=。
func (a *videoAPI) listStreams(w http.ResponseWriter, r *http.Request) {
	streams := a.streams.List(r.URL.Query().Get("nodeID"))
	writeJSON(w, http.StatusOK, map[string]any{"streams": streams, "count": len(streams)})
}

// createStreamBody 是创建请求体。
type createStreamBody struct {
	Name        string `json:"name"`
	NodeID      string `json:"nodeId"`
	DeviceName  string `json:"deviceName"`
	SourceType  string `json:"sourceType,omitempty"`
	Status      string `json:"status,omitempty"`
	Description string `json:"description,omitempty"`
}

// createStream 处理 POST /api/v1/videostreams。
func (a *videoAPI) createStream(w http.ResponseWriter, r *http.Request) {
	var b createStreamBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		badRequest(w, "请求体解析失败: %v", err)
		return
	}
	if b.Name == "" || b.NodeID == "" || b.DeviceName == "" {
		badRequest(w, "name/nodeId/deviceName 必填")
		return
	}
	st := &videostream.Stream{
		Name:        b.Name,
		NodeID:      b.NodeID,
		DeviceName:  b.DeviceName,
		SourceType:  b.SourceType,
		Status:      b.Status,
		Description: b.Description,
	}
	created, err := a.streams.Create(r.Context(), st)
	if err != nil {
		videoStreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// getStream 处理 GET /api/v1/videostreams/{name}。
func (a *videoAPI) getStream(w http.ResponseWriter, r *http.Request) {
	st, err := a.streams.Get(r.PathValue("name"))
	if err != nil {
		videoStreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// updateStreamBody 是更新请求体（非空字段生效）。
type updateStreamBody struct {
	DeviceName  string `json:"deviceName,omitempty"`
	SourceType  string `json:"sourceType,omitempty"`
	Status      string `json:"status,omitempty"`
	Description string `json:"description,omitempty"`
}

// updateStream 处理 PUT /api/v1/videostreams/{name}。
func (a *videoAPI) updateStream(w http.ResponseWriter, r *http.Request) {
	var b updateStreamBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		badRequest(w, "请求体解析失败: %v", err)
		return
	}
	st, err := a.streams.Update(r.Context(), r.PathValue("name"), videostream.Patch{
		DeviceName:  b.DeviceName,
		SourceType:  b.SourceType,
		Status:      b.Status,
		Description: b.Description,
	})
	if err != nil {
		videoStreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// deleteStream 处理 DELETE /api/v1/videostreams/{name}（索引删除；媒资文件保留）。
func (a *videoAPI) deleteStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := a.streams.Delete(r.Context(), name); err != nil {
		videoStreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "name": name})
}

// getSnapshot 处理 GET /api/v1/videostreams/{name}/snapshot：返回最新快照字节。
func (a *videoAPI) getSnapshot(w http.ResponseWriter, r *http.Request) {
	st, err := a.streams.Get(r.PathValue("name"))
	if err != nil {
		videoStreamError(w, err)
		return
	}
	if st.SnapshotMediaID == "" {
		writeErr(w, http.StatusNotFound, "该流暂无快照", nil)
		return
	}
	writeMediaBytes(w, a.media, st.SnapshotMediaID, "image/jpeg")
}

// listSegments 处理 GET /api/v1/videostreams/{name}/segments。
func (a *videoAPI) listSegments(w http.ResponseWriter, r *http.Request) {
	st, err := a.streams.Get(r.PathValue("name"))
	if err != nil {
		videoStreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segments": st.Segments, "count": len(st.Segments)})
}

// getSegment 处理 GET /api/v1/videostreams/{name}/segments/{mediaID}：
// 返回片段字节（MJPEG 拼接）。媒资须存在、已完成、且归属于该流。
func (a *videoAPI) getSegment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	mediaID := r.PathValue("mediaID")
	st, err := a.streams.Get(name)
	if err != nil {
		videoStreamError(w, err)
		return
	}
	found := false
	for _, seg := range st.Segments {
		if seg.MediaID == mediaID {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "片段不存在于该流", nil)
		return
	}
	writeMediaBytes(w, a.media, mediaID, "video/x-mjpeg")
}

// writeMediaBytes 从媒资存储读取完成对象并写出（含帧数头）。
func writeMediaBytes(w http.ResponseWriter, media *mediastore.Store, mediaID, contentType string) {
	m, err := media.Get(mediaID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "媒资不存在", nil)
		return
	}
	if m.State != mediastore.StateComplete {
		writeErr(w, http.StatusNotFound, "媒资尚未完成上传", nil)
		return
	}
	b, err := media.Read(mediaID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("X-Frame-Count", strconv.Itoa(m.FrameCount))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
