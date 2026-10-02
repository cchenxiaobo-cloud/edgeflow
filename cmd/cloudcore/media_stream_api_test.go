// v0.44.0（spec 0017 US-2/US-3）流媒体分发面单测：注册表/live.flv 拉流解析/
// live.ws 拉流/无源 404/告警片段检索窗口语义。
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"edgeflow/cloud/pkg/alarmstore"
	"edgeflow/cloud/pkg/videostream"
	"edgeflow/pkg/alarm"
	"edgeflow/pkg/flvremux"
	"edgeflow/pkg/mediaup"

	"github.com/gorilla/websocket"
)

func newMediaAPIForTest(t *testing.T) (*httptest.Server, *streamRegistry, *videostream.Store, *alarmstore.Store) {
	t.Helper()
	reg := newStreamRegistry()
	streams := videostream.NewStore(nil)
	alarms := alarmstore.NewStore(nil, nil)
	mux := http.NewServeMux()
	api := &mediaStreamAPI{srcs: reg, streams: streams, alarms: alarmViewAdapter{store: alarms}}
	api.RegisterStreams(mux) // /media/* 分发端点（根 mux 口径）
	api.RegisterAPI(mux)     // 告警片段检索（单测单 mux 一并挂载）
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, reg, streams, alarms
}

// parseFLVTags 解析 FLV 字节流的 tag 序列（type/dataSize/ts/data）。
func parseFLVTags(t *testing.T, b []byte) []struct {
	typ  byte
	ts   int
	data []byte
} {
	t.Helper()
	if len(b) < 13 || string(b[:3]) != "FLV" {
		t.Fatalf("FLV header 异常: % X", b[:min(8, len(b))])
	}
	var tags []struct {
		typ  byte
		ts   int
		data []byte
	}
	off := 13
	for off+11 <= len(b) {
		dataSz := int(b[off+1])<<16 | int(b[off+2])<<8 | int(b[off+3])
		if off+11+dataSz+4 > len(b) {
			break
		}
		ts := int(b[off+4])<<16 | int(b[off+5])<<8 | int(b[off+6]) | int(b[off+7])<<24
		tags = append(tags, struct {
			typ  byte
			ts   int
			data []byte
		}{typ: b[off], ts: ts, data: b[off+11 : off+11+dataSz]})
		off += 11 + dataSz + 4
	}
	return tags
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestMediaStreamRegistry404 无注册源 → 404。
func TestMediaStreamRegistry404(t *testing.T) {
	srv, _, _, _ := newMediaAPIForTest(t)
	resp, err := http.Get(srv.URL + "/media/streams/nope/live.flv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("无源应 404: %d", resp.StatusCode)
	}
}

// TestMediaStreamLiveFLV 注册演示源 → 拉流 → FLV 结构/时戳/帧数断言。
func TestMediaStreamLiveFLV(t *testing.T) {
	srv, reg, _, _ := newMediaAPIForTest(t)
	reg.Register("cam-live", flvremux.NewSyntheticH264Source(5)) // 200ms/帧

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/media/streams/cam-live/live.flv", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("拉流失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "video/x-flv" {
		t.Fatalf("Content-Type = %q", ct)
	}

	// 读至 ≥8 个 video tag（首帧立即，200ms/帧 → ~2s）。
	buf := make([]byte, 0, 1<<16)
	tmp := make([]byte, 8192)
	deadline := time.Now().Add(5 * time.Second)
	var videoTags int
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
		if videoTags = countVideoTags(buf); videoTags >= 8 {
			break
		}
	}
	if videoTags < 8 {
		t.Fatalf("video tag 不足: %d（读 %d 字节）", videoTags, len(buf))
	}
	tags := parseFLVTags(t, buf)
	if len(tags) < 3 {
		t.Fatalf("tag 总数异常: %d", len(tags))
	}
	// 首三 tag：seq header(video) / script(onMetaData) / 首帧 video——顺序按写入序。
	var hasScript, hasSeqHeader bool
	var lastTs, incCount int
	for _, tg := range tags {
		if tg.typ == 18 {
			hasScript = true
			if !strings.Contains(string(tg.data), "onMetaData") {
				t.Fatal("script tag 应含 onMetaData")
			}
		}
		if tg.typ == 9 {
			if tg.data[1] == 0x00 {
				hasSeqHeader = true
			}
			if tg.ts >= lastTs {
				incCount++
			}
			lastTs = tg.ts
		}
	}
	if !hasScript || !hasSeqHeader {
		t.Fatalf("script/seq header 缺失: script=%v seq=%v", hasScript, hasSeqHeader)
	}
	if incCount != countVideoTags(buf) {
		t.Fatalf("video tag 时戳应非降序")
	}
	cancel() // 断开（服务端 ctx 取消收口）
}

// countVideoTags 统计完整 video tag 数（遍历口径同 parseFLVTags）。
func countVideoTags(b []byte) int {
	n := 0
	off := 13
	for off+11 <= len(b) {
		dataSz := int(b[off+1])<<16 | int(b[off+2])<<8 | int(b[off+3])
		if off+11+dataSz+4 > len(b) {
			break
		}
		if b[off] == 9 {
			n++
		}
		off += 11 + dataSz + 4
	}
	return n
}

// TestMediaStreamLiveWS 注册源 → WS 拉流 → 首消息含 FLV header 与 seq header。
func TestMediaStreamLiveWS(t *testing.T) {
	srv, reg, _, _ := newMediaAPIForTest(t)
	reg.Register("cam-ws", flvremux.NewSyntheticH264Source(5))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media/streams/cam-ws/live.ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WS 拨号失败: %v", err)
	}
	defer func() { _ = ws.Close() }()

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("读首消息失败: %v", err)
	}
	if len(msg) < 13 || string(msg[:3]) != "FLV" {
		t.Fatalf("首消息应为 FLV 字节流: % X", msg[:min(8, len(msg))])
	}
	tags := parseFLVTags(t, msg)
	var hasSeq bool
	for _, tg := range tags {
		if tg.typ == 9 && tg.data[1] == 0x00 {
			hasSeq = true
		}
	}
	if !hasSeq {
		t.Fatal("WS 首消息应含 AVC sequence header")
	}
}

// TestAlarmSegmentsWindow 告警片段检索：窗内命中/窗外排除/告警缺失 404/无流空列表。
func TestAlarmSegmentsWindow(t *testing.T) {
	srv, _, streams, alarms := newMediaAPIForTest(t)
	now := time.Now().UnixMilli()

	// 告警（DeviceName=cam-02，RaisedAt=now）。
	al := alarm.Alarm{
		AlarmID: "alm-1", NodeID: "node-1", Source: alarm.SourceRule, RuleID: "e2e-rule",
		DeviceName: "cam-02", Severity: alarm.SeverityWarning, State: alarm.StateRaised,
		Message: "e2e", Count: 1, RaisedAt: now, UpdatedAt: now,
	}
	ctx := context.Background()
	if err := alarms.Upsert(ctx, al); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// 关联流 + 窗内/窗外片段。
	if _, err := streams.Create(ctx, &videostream.Stream{Name: "cam-02", NodeID: "node-1", DeviceName: "cam-02"}); err != nil {
		t.Fatal(err)
	}
	attach := func(id string, at int64) {
		_, err := streams.AttachMedia(ctx, mediaup.UploadChunk{
			MediaID: id, Kind: mediaup.KindSegment, DeviceName: "cam-02", StreamName: "cam-02",
			NodeID: "node-1", CapturedAt: at, FrameCount: 8, TotalBytes: 100, SHA256: "aa" + id,
		})
		if err != nil {
			t.Fatalf("AttachMedia(%s): %v", id, err)
		}
	}
	attach("m-in-1", now-4_000)   // 窗内（前 5s 界内）
	attach("m-in-2", now+10_000)  // 窗内（后 60s 界内）
	attach("m-out-1", now-30_000) // 窗外（早于 -5s）
	attach("m-out-2", now+120_000)
	// 其他流同名设备缺失 → 不参与。

	resp, err := http.Get(srv.URL + "/api/v1/alarms/alm-1/segments")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		AlarmID string `json:"alarmId"`
		Count   int    `json:"count"`
		Streams []struct {
			Stream   string                   `json:"stream"`
			Segments []videostream.SegmentRef `json:"segments"`
		} `json:"streams"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.AlarmID != "alm-1" || got.Count != 2 {
		t.Fatalf("检索应命中 2 条窗内片段: %+v", got)
	}
	for _, s := range got.Streams {
		if s.Stream != "cam-02" {
			t.Fatalf("流过滤异常: %s", s.Stream)
		}
		for _, seg := range s.Segments {
			if strings.HasPrefix(seg.MediaID, "m-out") {
				t.Fatalf("窗外片段不应命中: %s", seg.MediaID)
			}
		}
	}
	// 告警缺失 → 404。
	resp2, err := http.Get(srv.URL + "/api/v1/alarms/nope/segments")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("缺失告警应 404: %d", resp2.StatusCode)
	}
	// 无窗内片段 → 200 空列表（其他设备告警）。
	al2 := al
	al2.AlarmID = "alm-2"
	al2.DeviceName = "cam-none"
	if err := alarms.Upsert(ctx, al2); err != nil {
		t.Fatal(err)
	}
	resp3, err := http.Get(srv.URL + "/api/v1/alarms/alm-2/segments")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp3.Body.Close() }()
	var empty struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&empty); err != nil {
		t.Fatal(err)
	}
	if empty.Count != 0 {
		t.Fatalf("无关联流应空列表: %d", empty.Count)
	}
	_ = binary.BigEndian
}

// TestMediaStreamLiveFLVWriteDeadline（v0.44.0 复核修复回归）：流式端点必须豁免
// newHTTPServer 的 WriteTimeout——短写超时服务器下拉流超过超时窗仍持续收到帧。
func TestMediaStreamLiveFLVWriteDeadline(t *testing.T) {
	reg := newStreamRegistry()
	reg.Register("cam-deadline", flvremux.NewSyntheticH264Source(5))
	mux := http.NewServeMux()
	api := &mediaStreamAPI{srcs: reg}
	api.RegisterStreams(mux)
	// WriteTimeout 500ms：未豁免时直播在 500ms 后被掐；豁免后 1.5s 仍持续有帧。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv2 := &http.Server{Handler: mux, WriteTimeout: 500 * time.Millisecond}
	go func() { _ = srv2.Serve(ln) }()
	t.Cleanup(func() { _ = srv2.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 1600*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+ln.Addr().String()+"/media/streams/cam-deadline/live.flv", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("拉流: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 0, 1<<16)
	tmp := make([]byte, 16384)
	for {
		n, rerr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	video := 0
	for _, tag := range parseFLVTags(t, buf) {
		if tag.typ == 9 {
			video++
		}
	}
	if video < 8 { // 1.5s @ gop=5(200ms IDR)/25fps：≥7 帧；缓冲余量取 8
		t.Fatalf("写超时豁免失效（500ms 超时服务器下 1.5s 仅收 video=%d）", video)
	}
}
