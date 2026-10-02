// v0.43.0（spec 0016 US-3）媒资存储单测：分片乱序组装/重复幂等/sha 校验
// 失败保留分片/在途一致性拒绝/完成态读取。
package mediastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"edgeflow/pkg/mediaup"
)

// mkChunks 把 data 切为 chunkBytes 大小的 UploadChunk 序列。
func mkChunks(mediaID string, data []byte, chunkBytes int) []mediaup.UploadChunk {
	sum := sha256.Sum256(data)
	total := (len(data) + chunkBytes - 1) / chunkBytes
	var out []mediaup.UploadChunk
	for seq := 0; seq < total; seq++ {
		lo := seq * chunkBytes
		hi := lo + chunkBytes
		if hi > len(data) {
			hi = len(data)
		}
		out = append(out, mediaup.UploadChunk{
			MediaID: mediaID, Kind: mediaup.KindSegment, DeviceName: "cam", StreamName: "cam",
			NodeID: "node-1", CapturedAt: 1000, ContentType: "video/x-mjpeg", FrameCount: 3,
			TotalBytes: len(data), SHA256: hex.EncodeToString(sum[:]),
			ChunkSeq: seq, ChunkTotal: total, ChunkData: data[lo:hi],
		})
	}
	return out
}

func TestMediaStoreAssembleOutOfOrder(t *testing.T) {
	ctx := context.Background()
	s := NewStore(t.TempDir(), nil)
	data := make([]byte, 100000)
	for i := range data {
		data[i] = byte(i * 13)
	}
	chunks := mkChunks("m-seg-1", data, 32768)
	if len(chunks) != 4 {
		t.Fatalf("分片数 = %d, want 4", len(chunks))
	}
	// 乱序到达：2, 0, 3, 1（最后一片才完成）。
	order := []int{2, 0, 3, 1}
	for i, idx := range order {
		completed, err := s.PutChunk(ctx, &chunks[idx])
		if err != nil {
			t.Fatalf("PutChunk(%d): %v", idx, err)
		}
		want := i == len(order)-1
		if completed != want {
			t.Fatalf("completed(%d) = %v, want %v", idx, completed, want)
		}
	}
	got, err := s.Read("m-seg-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != len(data) {
		t.Fatalf("读出长度 = %d, want %d", len(got), len(data))
	}
	for i := range got {
		if got[i] != data[i] {
			t.Fatalf("字节不一致 @%d", i)
		}
	}
	m, err := s.Get("m-seg-1")
	if err != nil || m.State != StateComplete || m.FrameCount != 3 {
		t.Fatalf("完成态元数据异常: %+v %v", m, err)
	}
	// 完成后重复分片 → 忽略（false, nil）。
	completed, err := s.PutChunk(ctx, &chunks[0])
	if err != nil || completed {
		t.Fatalf("完成后重复分片应忽略: %v %v", completed, err)
	}
}

func TestMediaStoreDupAndInconsistent(t *testing.T) {
	ctx := context.Background()
	s := NewStore(t.TempDir(), nil)
	data := make([]byte, 1000)
	chunks := mkChunks("m-a", data, 512) // 2 片
	// 重复第 0 片（覆盖幂等）。
	if done, err := s.PutChunk(ctx, &chunks[0]); err != nil || done {
		t.Fatalf("第 0 片: %v %v", done, err)
	}
	if done, err := s.PutChunk(ctx, &chunks[0]); err != nil || done {
		t.Fatalf("重复 0 片应幂等: %v %v", done, err)
	}
	// 在途一致性：同 mediaId 不同 TotalBytes → 拒绝。
	bad := chunks[1]
	bad.TotalBytes = 9999
	if _, err := s.PutChunk(ctx, &bad); !errors.Is(err, ErrInconsistent) {
		t.Fatalf("不一致分片应拒绝: %v", err)
	}
	// 正常第 1 片 → 完成。
	if done, err := s.PutChunk(ctx, &chunks[1]); err != nil || !done {
		t.Fatalf("第 1 片完成: %v %v", done, err)
	}
}

func TestMediaStoreSHAMismatchKeepsParts(t *testing.T) {
	ctx := context.Background()
	s := NewStore(t.TempDir(), nil)
	data := make([]byte, 800)
	chunks := mkChunks("m-b", data, 400) // 2 片
	// 篡改第 0 片内容（sha 元数据仍为原值）——组装时校验失败。
	tampered := chunks[0]
	tampered.ChunkData = append([]byte(nil), chunks[0].ChunkData...)
	tampered.ChunkData[0] ^= 0xFF
	if _, err := s.PutChunk(ctx, &tampered); err != nil {
		t.Fatalf("第 0 片(篡改)落盘: %v", err)
	}
	if _, err := s.PutChunk(ctx, &chunks[1]); err == nil {
		t.Fatal("组装 sha 校验应失败")
	}
	// 修复重传第 0 片 → 完成。
	if done, err := s.PutChunk(ctx, &chunks[0]); err != nil || !done {
		t.Fatalf("修复重传应完成: %v %v", done, err)
	}
	if _, err := s.Read("m-b"); err != nil {
		t.Fatalf("修复后读取: %v", err)
	}
}

func TestMediaStoreGetMissing(t *testing.T) {
	s := NewStore(t.TempDir(), nil)
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺失应 ErrNotFound: %v", err)
	}
	if _, err := s.Read("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("读缺失应 ErrNotFound: %v", err)
	}
}
