// v0.43.0（spec 0016 US-4）视频管理面 API 单测：8 端点全路径（内存存储 +
// 临时媒资目录，不依赖 etcd）——CRUD/409/404/快照字节/片段列表/片段回放。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"edgeflow/cloud/pkg/mediastore"
	"edgeflow/cloud/pkg/videostream"
	"edgeflow/pkg/mediaup"
)

// newVideoAPIForTest 组装内存形态的 videoAPI + httptest 服务。
func newVideoAPIForTest(t *testing.T) (*httptest.Server, *mediastore.Store, *videostream.Store) {
	t.Helper()
	streams := videostream.NewStore(nil)
	media := mediastore.NewStore(t.TempDir(), nil)
	mux := http.NewServeMux()
	(&videoAPI{streams: streams, media: media}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, media, streams
}

// ingestMedia 模拟云端接收一段媒资（分片入 media 后挂接流索引）。
func ingestMedia(t *testing.T, media *mediastore.Store, streams *videostream.Store, up mediaup.UploadChunk, data []byte, chunkBytes int) {
	t.Helper()
	sum := sha256.Sum256(data)
	total := (len(data) + chunkBytes - 1) / chunkBytes
	for seq := 0; seq < total; seq++ {
		lo := seq * chunkBytes
		hi := lo + chunkBytes
		if hi > len(data) {
			hi = len(data)
		}
		c := up
		c.TotalBytes = len(data)
		c.SHA256 = hex.EncodeToString(sum[:])
		c.ChunkSeq, c.ChunkTotal, c.ChunkData = seq, total, data[lo:hi]
		completed, err := media.PutChunk(context.Background(), &c)
		if err != nil {
			t.Fatalf("PutChunk(%d): %v", seq, err)
		}
		if completed {
			if _, err := streams.AttachMedia(context.Background(), c); err != nil {
				t.Fatalf("AttachMedia: %v", err)
			}
		}
	}
}

// doJSON 发起请求并读回响应（体 ≤ 2MB）。
func videoDo(t *testing.T, method, url string, body any, wantCode int) []byte {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantCode {
		t.Fatalf("%s %s = %d, want %d（体: %s）", method, url, resp.StatusCode, wantCode, string(b))
	}
	return b
}

func TestVideoAPICRUD(t *testing.T) {
	srv, _, _ := newVideoAPIForTest(t)
	base := srv.URL

	// 缺失 → 404。
	videoDo(t, "GET", base+"/api/v1/videostreams/cam-01", nil, http.StatusNotFound)
	// 创建。
	videoDo(t, "POST", base+"/api/v1/videostreams", map[string]any{
		"name": "cam-01", "nodeId": "node-1", "deviceName": "cam-01", "sourceType": "rtsp",
	}, http.StatusCreated)
	// 重复 → 409；缺字段 → 400。
	videoDo(t, "POST", base+"/api/v1/videostreams", map[string]any{
		"name": "cam-01", "nodeId": "node-1", "deviceName": "cam-01",
	}, http.StatusConflict)
	videoDo(t, "POST", base+"/api/v1/videostreams", map[string]any{"name": "x"}, http.StatusBadRequest)
	// 详情 + 更新。
	b := videoDo(t, "GET", base+"/api/v1/videostreams/cam-01", nil, http.StatusOK)
	var st videostream.Stream
	if err := json.Unmarshal(b, &st); err != nil || st.Name != "cam-01" || st.Status != "unknown" {
		t.Fatalf("详情异常: %s (%v)", string(b), err)
	}
	videoDo(t, "PUT", base+"/api/v1/videostreams/cam-01", map[string]any{"status": "online"}, http.StatusOK)
	b = videoDo(t, "GET", base+"/api/v1/videostreams/cam-01", nil, http.StatusOK)
	_ = json.Unmarshal(b, &st)
	if st.Status != "online" {
		t.Fatalf("更新未生效: %s", string(b))
	}
	// 列表。
	b = videoDo(t, "GET", base+"/api/v1/videostreams", nil, http.StatusOK)
	var list struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(b, &list)
	if list.Count != 1 {
		t.Fatalf("列表 count = %d", list.Count)
	}
	// 删除 + 再取 404。
	videoDo(t, "DELETE", base+"/api/v1/videostreams/cam-01", nil, http.StatusOK)
	videoDo(t, "GET", base+"/api/v1/videostreams/cam-01", nil, http.StatusNotFound)
}

func TestVideoAPISnapshotAndSegment(t *testing.T) {
	srv, media, streams := newVideoAPIForTest(t)
	base := srv.URL
	videoDo(t, "POST", base+"/api/v1/videostreams", map[string]any{
		"name": "cam-02", "nodeId": "node-1", "deviceName": "cam-02",
	}, http.StatusCreated)

	// 无快照 → 404。
	videoDo(t, "GET", base+"/api/v1/videostreams/cam-02/snapshot", nil, http.StatusNotFound)

	// 注入快照（1 片）。
	snap := []byte{0xFF, 0xD8, 0x01, 0x02, 0x03, 0xFF, 0xD9}
	ingestMedia(t, media, streams, mediaup.UploadChunk{
		MediaID: "m-snap-1", Kind: mediaup.KindSnapshot, DeviceName: "cam-02", StreamName: "cam-02",
		NodeID: "node-1", CapturedAt: 1000, ContentType: "image/jpeg", FrameCount: 1,
	}, snap, 1<<20)
	// 快照回放（字节与头）。
	resp, err := http.Get(base + "/api/v1/videostreams/cam-02/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, snap) {
		t.Fatalf("快照回放异常: %d, %d 字节", resp.StatusCode, len(got))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("快照 Content-Type = %q", ct)
	}
	if fc := resp.Header.Get("X-Frame-Count"); fc != "1" {
		t.Fatalf("快照 X-Frame-Count = %q", fc)
	}

	// 注入片段（3 片，~100KB）。
	seg := make([]byte, 100000)
	for i := range seg {
		seg[i] = byte(i * 31)
	}
	ingestMedia(t, media, streams, mediaup.UploadChunk{
		MediaID: "m-seg-1", Kind: mediaup.KindSegment, DeviceName: "cam-02", StreamName: "cam-02",
		NodeID: "node-1", CapturedAt: 2000, ContentType: "video/x-mjpeg", FrameCount: 8,
	}, seg, 32768)

	// 片段列表。
	b := videoDo(t, "GET", base+"/api/v1/videostreams/cam-02/segments", nil, http.StatusOK)
	var segList struct {
		Segments []videostream.SegmentRef `json:"segments"`
		Count    int                      `json:"count"`
	}
	_ = json.Unmarshal(b, &segList)
	if segList.Count != 1 || segList.Segments[0].MediaID != "m-seg-1" || segList.Segments[0].FrameCount != 8 {
		t.Fatalf("片段列表异常: %s", string(b))
	}

	// 片段回放（字节一致 + 帧数头）。
	resp, err = http.Get(base + "/api/v1/videostreams/cam-02/segments/m-seg-1")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, seg) {
		t.Fatalf("片段回放异常: %d, %d 字节", resp.StatusCode, len(got))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/x-mjpeg" {
		t.Fatalf("片段 Content-Type = %q", ct)
	}
	if fc := resp.Header.Get("X-Frame-Count"); fc != "8" {
		t.Fatalf("片段 X-Frame-Count = %q", fc)
	}

	// 非本流 mediaID → 404；未完成媒资 → 404（构造一个未完成分片态）。
	videoDo(t, "GET", base+"/api/v1/videostreams/cam-02/segments/m-nope", nil, http.StatusNotFound)
	pending := mediaup.UploadChunk{
		MediaID: "m-pending", Kind: mediaup.KindSegment, DeviceName: "cam-02", StreamName: "cam-02",
		NodeID: "node-1", CapturedAt: 3000, ContentType: "video/x-mjpeg", FrameCount: 1,
		TotalBytes: 10, SHA256: "00", ChunkSeq: 0, ChunkTotal: 2, ChunkData: []byte{1, 2, 3},
	}
	if _, err := media.PutChunk(context.Background(), &pending); err != nil {
		t.Fatalf("pending PutChunk: %v", err)
	}
	// 手工挂一个未完成引用的流（模拟索引先行场景由 API 层 404 兜底）。
	_, _ = streams.AttachMedia(context.Background(), mediaup.UploadChunk{
		MediaID: "m-pending", Kind: mediaup.KindSegment, DeviceName: "cam-02", StreamName: "cam-02", NodeID: "node-1",
	})
	videoDo(t, "GET", base+"/api/v1/videostreams/cam-02/segments/m-pending", nil, http.StatusNotFound)
}
