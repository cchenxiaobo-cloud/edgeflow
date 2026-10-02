// 流媒体分发面（v0.44.0，spec 0017 US-2/US-3）：
//
//	GET /media/streams/{name}/live.flv   HTTP-FLV（chunked，video/x-flv）
//	GET /media/streams/{name}/live.ws    WS-FLV（二进制帧透传 FLV 字节流）
//	GET /api/v1/alarms/{alarmID}/segments 告警片段检索（DeviceName+RaisedAt 窗 ×
//	                                       videostream 片段索引联合查询）
//
// 帧源注册表：stream name → H264Source（并发安全；演示源经
// EDGEFLOW_CLOUDCORE_DEMO_H264_STREAMS 环境变量 opt-in 注册，KI §45 演示面）。
// 无注册源 → 分发端点 404（默认零行为）。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"edgeflow/cloud/pkg/alarmstore"
	"edgeflow/cloud/pkg/videostream"
	"edgeflow/pkg/flvremux"
	"edgeflow/pkg/log"

	"github.com/gorilla/websocket"
)

// H264Source 是持续 H.264 AnnexB 帧源（分发面输入，可插拔——spec 0017 F0）。
type H264Source interface {
	// Params 返回 SPS/PPS（AVC sequence header 用）；未就绪返回错误。
	Params() (sps, pps []byte, err error)
	// Next 阻塞产出下一帧 AnnexB（含起始码）；ctx 取消返回错误。
	Next(ctx context.Context) (annexb []byte, tsMs uint64, err error)
}

// streamRegistry 是帧源注册表（并发安全）。
type streamRegistry struct {
	mu   sync.RWMutex
	srcs map[string]H264Source
}

func newStreamRegistry() *streamRegistry {
	return &streamRegistry{srcs: make(map[string]H264Source)}
}

// Register 注册/替换帧源。
func (r *streamRegistry) Register(name string, src H264Source) {
	r.mu.Lock()
	r.srcs[name] = src
	r.mu.Unlock()
}

// Unregister 注销帧源（在拉流的 ctx 不受影响——由源自身语义决定）。
func (r *streamRegistry) Unregister(name string) {
	r.mu.Lock()
	delete(r.srcs, name)
	r.mu.Unlock()
}

// Get 取帧源（无 → nil）。
func (r *streamRegistry) Get(name string) H264Source {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.srcs[name]
}

// errAlarmViewNotFound 是告警检索的缺失哨兵（适配层映射 alarmstore.ErrNotFound）。
var errAlarmViewNotFound = errors.New("mediastream: 告警不存在")

// mediaStreamAPI 是流媒体分发面的 HTTP 处理器集合。
type mediaStreamAPI struct {
	srcs    *streamRegistry
	streams *videostream.Store
	alarms  alarmStoreView
}

// alarmStoreView 是告警检索需要的最小读面（解耦 alarmstore 具体类型——
// 由装配注入适配器：Get→ErrNotFound 映射 errAlarmViewNotFound）。
type alarmStoreView interface {
	GetView(alarmID string) (deviceName string, raisedAt int64, err error)
}

// RegisterStreams 注册分发端点到根 mux（/media/* 流端点——非 /api/v1/*
// 管理面：与 /ocsp 同类的协议端点定位，不挂 Bearer Token 认证；防盗链/
// 签名 URL 登记 KI §45。根 mux 直挂——apiMux 仅服务 /api/v1/ 前缀）。
func (a *mediaStreamAPI) RegisterStreams(mux *http.ServeMux) {
	mux.HandleFunc("GET /media/streams/{name}/live.flv", a.serveLiveFLV)
	mux.HandleFunc("GET /media/streams/{name}/live.ws", a.serveLiveWS)
}

// RegisterAPI 注册告警片段检索端点到 apiMux（auth/audit 链自动覆盖）。
func (a *mediaStreamAPI) RegisterAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/alarms/{alarmID}/segments", a.alarmSegments)
}

// serveLiveFLV 处理 GET /media/streams/{name}/live.flv：HTTP chunked 拉流。
// 无注册源 → 404；客户端断开/源错误 → 结束响应。
func (a *mediaStreamAPI) serveLiveFLV(w http.ResponseWriter, r *http.Request) {
	src := a.srcs.Get(r.PathValue("name"))
	if src == nil {
		writeErr(w, http.StatusNotFound, "该流未注册 H.264 帧源", nil)
		return
	}
	sps, pps, err := src.Params()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "帧源参数未就绪: "+err.Error(), nil)
		return
	}
	rem := flvremux.New()
	if err := rem.SequenceHeader(sps, pps); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "序列头构造失败: "+err.Error(), nil)
		return
	}
	rem.MetaData(320, 240) // 演示分辨率口径（onMetaData 最小面）

	w.Header().Set("Content-Type", "video/x-flv")
	w.Header().Set("Cache-Control", "no-store")
	// 长连接豁免：newHTTPServer 的 ReadHeaderTimeout 5s/ReadTimeout 10s/
	// WriteTimeout 15s 均面向短 JSON 响应设计——读侧 deadline 在 background
	// read 命中后服务端会 cancel 请求上下文（e2e 实证：+5s ctx canceled），
	// 写侧到点掐断直播。流式端点按连接同时清除读+写超时
	// （NewResponseController 沿 Unwrap 链穿透 metrics statusRecorder 到底层 conn）。
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		log.Warnf("[flv] 清除读超时失败（流 %s）: %v", r.PathValue("name"), err)
	}
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		log.Warnf("[flv] 清除写超时失败（流 %s）: %v", r.PathValue("name"), err)
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(rem.Full()); err != nil {
		log.Warnf("[flv] 首写失败（流 %s）: %v", r.PathValue("name"), err)
		return
	}
	rem.Sync() // Full 取走全部 → 后续 Delta 只含新增（防头部重放）
	// Flusher 探测：直连 writer 或经中间件包装（Unwrap 解包链——metrics
	// statusRecorder 等包装器实现 Unwrap 供能力探测）。
	flusher, ok := flusherOf(w)
	if !ok {
		log.Warnf("[flv] 非 Flusher（流 %s）", r.PathValue("name"))
		return
	}
	flusher.Flush()
	log.Infof("[flv] 头部已下发（流 %s，%d 字节），进入帧循环", r.PathValue("name"), len(rem.Full()))

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		annexb, tsMs, err := src.Next(ctx)
		if err != nil {
			log.Warnf("[flv] 拉流循环退出（流 %s）: nextErr=%v ctxErr=%v", r.PathValue("name"), err, ctx.Err())
			return // 客户端断开/源终止：结束响应
		}
		if err := rem.WriteAnnexB(annexb, uint32(tsMs)); err != nil {
			log.Warnf("[flv] 封装失败（流 %s）: %v", r.PathValue("name"), err)
			return
		}
		if _, err := w.Write(rem.Delta()); err != nil {
			log.Warnf("[flv] 写帧失败（流 %s）: %v", r.PathValue("name"), err)
			return
		}
		flusher.Flush()
	}
}

// serveLiveWS 处理 GET /media/streams/{name}/live.ws：WS-FLV 二进制透传。
func (a *mediaStreamAPI) serveLiveWS(w http.ResponseWriter, r *http.Request) {
	src := a.srcs.Get(r.PathValue("name"))
	if src == nil {
		writeErr(w, http.StatusNotFound, "该流未注册 H.264 帧源", nil)
		return
	}
	up := websocket.Upgrader{Subprotocols: []string{"flv"}}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade 已写错误响应
	}
	defer func() { _ = ws.Close() }()
	// Hijack 继承服务端武装的 WriteTimeout 15s deadline——WS 长连接必须清除
	// （此后生命周期由本 handler 管控，避免 15s 后 WriteMessage 报 i/o timeout）。
	_ = ws.UnderlyingConn().SetDeadline(time.Time{})

	sps, pps, err := src.Params()
	if err != nil {
		_ = ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "帧源参数未就绪"))
		return
	}
	rem := flvremux.New()
	if err := rem.SequenceHeader(sps, pps); err != nil {
		_ = ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "序列头构造失败"))
		return
	}
	rem.MetaData(320, 240)
	if err := ws.WriteMessage(websocket.BinaryMessage, rem.Full()); err != nil {
		return
	}
	rem.Sync() // Full 取走全部 → 后续 Delta 只含新增（与 HTTP-FLV 口径一致，防头部重放）

	// 读泵：消化客户端 close/ping（不处理客户端数据语义）。
	go func() {
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ctx := r.Context()
	for {
		annexb, tsMs, err := src.Next(ctx)
		if err != nil {
			return
		}
		if err := rem.WriteAnnexB(annexb, uint32(tsMs)); err != nil {
			return
		}
		if err := ws.WriteMessage(websocket.BinaryMessage, rem.Delta()); err != nil {
			return
		}
	}
}

// flusherOf 探测 ResponseWriter 的 Flusher 能力（沿 Unwrap 链逐层解包，
// 兼容 metrics statusRecorder 等统计/审计包装器）。
func flusherOf(w http.ResponseWriter) (http.Flusher, bool) {
	for cur := w; cur != nil; {
		if f, ok := cur.(http.Flusher); ok {
			return f, true
		}
		u, ok := cur.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil, false
		}
		cur = u.Unwrap()
	}
	return nil, false
}

// 告警片段检索默认窗（毫秒）：[RaisedAt-5s, RaisedAt+60s]。
const (
	alarmSegWindowBeforeMs = int64(5 * 1000)
	alarmSegWindowAfterMs  = int64(60 * 1000)
)

// alarmSegments 处理 GET /api/v1/alarms/{alarmID}/segments：按告警
// （DeviceName + RaisedAt）检索关联流的窗内片段（spec 0017 US-3）。
func (a *mediaStreamAPI) alarmSegments(w http.ResponseWriter, r *http.Request) {
	deviceName, raisedAt, err := a.alarms.GetView(r.PathValue("alarmID"))
	if err != nil {
		if errors.Is(err, errAlarmViewNotFound) {
			writeErr(w, http.StatusNotFound, "告警不存在", nil)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	lo, hi := raisedAt-alarmSegWindowBeforeMs, raisedAt+alarmSegWindowAfterMs
	type streamSegments struct {
		Stream   string                   `json:"stream"`
		Segments []videostream.SegmentRef `json:"segments"`
	}
	var out []streamSegments
	count := 0
	for _, st := range a.streams.List("") {
		if st.DeviceName != deviceName {
			continue
		}
		var hits []videostream.SegmentRef
		for _, seg := range st.Segments {
			if seg.CapturedAt >= lo && seg.CapturedAt <= hi {
				hits = append(hits, seg)
			}
		}
		if len(hits) > 0 {
			out = append(out, streamSegments{Stream: st.Name, Segments: hits})
			count += len(hits)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"alarmId": r.PathValue("alarmID"),
		"window":  map[string]int64{"fromMs": lo, "toMs": hi},
		"streams": out,
		"count":   count,
	})
}

// alarmViewAdapter 把 *alarmstore.Store 适配为 alarmStoreView（读面解耦）。
type alarmViewAdapter struct {
	store *alarmstore.Store
}

// GetView 实现 alarmStoreView：返回告警的 DeviceName 与 RaisedAt。
func (a alarmViewAdapter) GetView(alarmID string) (string, int64, error) {
	al, err := a.store.Get(alarmID)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", errAlarmViewNotFound, err)
	}
	return al.DeviceName, al.RaisedAt, nil
}
