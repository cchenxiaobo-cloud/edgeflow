package mediastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"edgeflow/pkg/mediaup"
)

// v0450 HardSample 检索测试（spec 0018 US-4）。

func v0450Chunk(mediaID string, kind string, data []byte, nodeID, device, alarmID string) *mediaup.UploadChunk {
	sum := sha256.Sum256(data)
	return &mediaup.UploadChunk{
		MediaID:     mediaID,
		Kind:        kind,
		NodeID:      nodeID,
		DeviceName:  device,
		AlarmID:     alarmID,
		CapturedAt:  1727000000000,
		ContentType: "image/jpeg",
		FrameCount:  1,
		TotalBytes:  len(data),
		SHA256:      hex.EncodeToString(sum[:]),
		ChunkSeq:    0,
		ChunkTotal:  1,
		ChunkData:   data,
	}
}

func TestV0450HardSampleStoreAndList(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "media")
	s := NewStore(dir, nil)
	ctx := context.Background()

	// 两份困难样本 + 一份快照（过滤隔离验证）。
	for i, id := range []string{"hs-a", "hs-b"} {
		if _, err := s.PutChunk(ctx, v0450Chunk(id, mediaup.KindHardSample, []byte{0xff, 0xd8, byte(i)}, "node-1", "cam-01", "alm-1")); err != nil {
			t.Fatalf("PutChunk(%s): %v", id, err)
		}
	}
	if _, err := s.PutChunk(ctx, v0450Chunk("snap-1", mediaup.KindSnapshot, []byte{1}, "node-1", "cam-01", "")); err != nil {
		t.Fatalf("PutChunk(snapshot): %v", err)
	}

	// 全量列表：只含 hard-sample。
	all := s.ListHardSamples(HardSampleFilter{})
	if len(all) != 2 {
		t.Fatalf("ListHardSamples 全量 = %d, want 2", len(all))
	}
	// 按告警过滤。
	byAlarm := s.ListHardSamples(HardSampleFilter{AlarmID: "alm-1"})
	if len(byAlarm) != 2 {
		t.Fatalf("按告警过滤 = %d, want 2", len(byAlarm))
	}
	none := s.ListHardSamples(HardSampleFilter{AlarmID: "alm-none"})
	if len(none) != 0 {
		t.Fatalf("未知告警过滤 = %d, want 0", len(none))
	}
	// 按节点过滤 + 快照不入列表。
	byNode := s.ListHardSamples(HardSampleFilter{NodeID: "node-1"})
	if len(byNode) != 2 {
		t.Fatalf("按节点过滤 = %d, want 2（快照隔离）", len(byNode))
	}
	// 内容读取（hardsample 前缀落盘）。
	b, err := s.Read("hs-a")
	if err != nil || len(b) != 3 {
		t.Fatalf("Read(hs-a) = %d, %v", len(b), err)
	}
	// AlarmID 元数据落库。
	m, err := s.Get("hs-a")
	if err != nil || m.AlarmID != "alm-1" {
		t.Fatalf("Get AlarmID = %q, %v", m.AlarmID, err)
	}
}

func TestV0450HardSampleValidateRejectsUnknownKindStillWorks(t *testing.T) {
	// 未知 kind 依旧拒绝（白名单扩容不放松校验）。
	c := v0450Chunk("bad", "video-mp4", []byte{1}, "n", "d", "")
	if err := c.Validate(); err == nil {
		t.Fatal("未知 kind 应拒绝")
	}
}
