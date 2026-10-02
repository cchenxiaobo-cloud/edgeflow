// v0.43.0（spec 0016 US-4）VideoStream 存储单测：CRUD/去重/界长/媒资挂接
// （Ensure 自动建流、快照替换、片段追加去重与界长）。kv=nil 纯内存形态。
package videostream

import (
	"context"
	"errors"
	"testing"

	"edgeflow/pkg/mediaup"
)

func TestVideoStreamCRUD(t *testing.T) {
	ctx := context.Background()
	s := NewStore(nil)
	st, err := s.Create(ctx, &Stream{Name: "cam-01", NodeID: "node-1", DeviceName: "cam-01", SourceType: "rtsp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if st.Status != "unknown" || st.CreatedAt == 0 {
		t.Fatalf("默认字段异常: %+v", st)
	}
	if _, err := s.Create(ctx, &Stream{Name: "cam-01", NodeID: "node-1", DeviceName: "cam-01"}); !errors.Is(err, ErrExists) {
		t.Fatalf("重复创建应 ErrExists: %v", err)
	}
	if _, err := s.Create(ctx, &Stream{Name: "x"}); err == nil {
		t.Fatal("缺 nodeId/deviceName 应拒绝")
	}
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺失应 ErrNotFound: %v", err)
	}
	// Update。
	up, err := s.Update(ctx, "cam-01", Patch{Status: "online", Description: "前门"})
	if err != nil || up.Status != "online" || up.Description != "前门" {
		t.Fatalf("Update 异常: %+v %v", up, err)
	}
	// List + nodeID 过滤。
	_, _ = s.Create(ctx, &Stream{Name: "cam-02", NodeID: "node-2", DeviceName: "cam-02"})
	if got := s.List(""); len(got) != 2 || got[0].Name != "cam-01" {
		t.Fatalf("List 全量排序异常: %+v", got)
	}
	if got := s.List("node-2"); len(got) != 1 || got[0].Name != "cam-02" {
		t.Fatalf("List 过滤异常: %+v", got)
	}
	// Delete。
	if err := s.Delete(ctx, "cam-01"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, "cam-01"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound: %v", err)
	}
}

func TestVideoStreamAttachMedia(t *testing.T) {
	ctx := context.Background()
	s := NewStore(nil)

	// 快照到达 → Ensure 自动建流。
	snap1 := mediaup.UploadChunk{MediaID: "m-snapshot-1", Kind: mediaup.KindSnapshot, DeviceName: "cam-x", StreamName: "cam-x", NodeID: "node-1", CapturedAt: 1000}
	st, err := s.AttachMedia(ctx, snap1)
	if err != nil {
		t.Fatalf("AttachMedia(snapshot): %v", err)
	}
	if st.Name != "cam-x" || st.SnapshotMediaID != "m-snapshot-1" || st.SnapshotAt != 1000 {
		t.Fatalf("快照挂接异常: %+v", st)
	}
	if st.SourceType != "unknown" || st.Status != "unknown" {
		t.Fatalf("自动建流默认值异常: %+v", st)
	}
	// 快照替换。
	st, _ = s.AttachMedia(ctx, mediaup.UploadChunk{MediaID: "m-snapshot-2", Kind: mediaup.KindSnapshot, DeviceName: "cam-x", StreamName: "cam-x", NodeID: "node-1", CapturedAt: 2000})
	if st.SnapshotMediaID != "m-snapshot-2" || st.SnapshotAt != 2000 {
		t.Fatalf("快照替换异常: %+v", st)
	}
	// 片段追加 + 去重。
	seg := func(id string, at int64) mediaup.UploadChunk {
		return mediaup.UploadChunk{MediaID: id, Kind: mediaup.KindSegment, DeviceName: "cam-x", StreamName: "cam-x", NodeID: "node-1", CapturedAt: at, FrameCount: 8, TotalBytes: 1234, SHA256: "aa" + id}
	}
	if st, _ = s.AttachMedia(ctx, seg("m-seg-1", 3000)); len(st.Segments) != 1 {
		t.Fatalf("片段追加异常: %+v", st.Segments)
	}
	if st, _ = s.AttachMedia(ctx, seg("m-seg-1", 3000)); len(st.Segments) != 1 {
		t.Fatalf("重复片段应去重: %+v", st.Segments)
	}
	ref := st.Segments[0]
	if ref.FrameCount != 8 || ref.Bytes != 1234 || ref.CapturedAt != 3000 || ref.SHA256 != "aam-seg-1" {
		t.Fatalf("片段引用字段异常: %+v", ref)
	}
	// 界长：追加 MaxSegments+5 → 保留最近 MaxSegments（最老被丢）。
	for i := 0; i < MaxSegments+5; i++ {
		st, _ = s.AttachMedia(ctx, seg("m-bulk-"+itoa64(i), int64(4000+i)))
	}
	if len(st.Segments) != MaxSegments {
		t.Fatalf("界长异常: %d", len(st.Segments))
	}
	if st.Segments[0].CapturedAt != int64(4000+5) {
		t.Fatalf("应丢最老保留最新: %+v", st.Segments[0])
	}
	// LastSeenAt 更新。
	if st.LastSeenAt == 0 {
		t.Fatal("LastSeenAt 应更新")
	}
}

// itoa64 小工具（避免引入 strconv 仅为测试）。
func itoa64(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
