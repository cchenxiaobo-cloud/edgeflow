// v0.43.0（spec 0016 US-1）媒体上传单元测试：分片构建/重组一致性、spool
// 落盘与 Janitor 完成/超龄清理、载荷校验。
package mediaup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"edgeflow/pkg/protocol"
)

// fakeQueue 是 Enqueuer 的内存假实现（记录行 → 消息）。
type fakeQueue struct {
	mu   sync.Mutex
	rows map[int64]*protocol.Message
	next int64
}

func newFakeQueue() *fakeQueue { return &fakeQueue{rows: map[int64]*protocol.Message{}} }

func (f *fakeQueue) EnqueueUplink(_ int, msg *protocol.Message) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.rows[f.next] = msg
	return f.next, nil
}

func (f *fakeQueue) PendingUplinkIDs(ids []int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for _, id := range ids {
		if _, ok := f.rows[id]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeQueue) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeQueue) ackAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = map[int64]*protocol.Message{}
}

// TestMediaupChunkRoundTrip 覆盖分片构建 → 队列 → 重组一致性（内容/sha/序）。
func TestMediaupChunkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fq := newFakeQueue()
	var notified atomic.Int32
	u := NewUploader(dir, "node-1", fq, func() { notified.Add(1) }, time.Hour)
	u.Start()
	t.Cleanup(u.Stop)

	// 合成片段 ~120KB（3 帧，每帧 40000B 递增模式）。
	seg := make([]byte, 0, 120000)
	for f := 0; f < 3; f++ {
		frame := make([]byte, 40000)
		for i := range frame {
			frame[i] = byte(i*7 + f)
		}
		seg = append(seg, frame...)
	}
	u.HandleClip(Clip{DeviceName: "cam-01", Snapshot: []byte{0xFF, 0xD8, 1, 2, 3}, Segment: seg, FrameCount: 3, Detections: 2, WhenMs: 1759300000000})

	// 快照 1 片 + 片段 ceil(120000/32768)=4 片 → 共 5 片。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && fq.count() < 5 {
		time.Sleep(10 * time.Millisecond)
	}
	if fq.count() != 5 {
		t.Fatalf("分片数 = %d, want 5", fq.count())
	}
	if notified.Load() == 0 {
		t.Fatal("notify 应被调用（唤醒补传 worker）")
	}
	// 等待两段媒资的 meta 落盘（Janitor 清理判定以 meta 为准——防时序脆性）。
	waitMeta := func(want int) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			entries, _ := os.ReadDir(dir)
			n := 0
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".meta.json") {
					n++
				}
			}
			if n >= want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("等待 meta 文件超时（want %d）", want)
	}
	waitMeta(2)

	// 重组片段内容与元数据核验（锁内快照消息列表后处理——JanitorSweep 内部
	// 会再取 fakeQueue 锁，不能持锁调用）。
	fq.mu.Lock()
	msgs := make([]*protocol.Message, 0, len(fq.rows))
	for _, m := range fq.rows {
		msgs = append(msgs, m)
	}
	fq.mu.Unlock()
	var segChunks []UploadChunk
	var snapshotSeen bool
	for _, msg := range msgs {
		if msg.Type != protocol.TypeMediaUpload {
			t.Fatalf("消息类型 = %s, want MediaUpload", msg.Type)
		}
		var up UploadChunk
		if err := msg.DecodePayload(&up); err != nil {
			t.Fatalf("载荷解析失败: %v", err)
		}
		if err := up.Validate(); err != nil {
			t.Fatalf("载荷校验失败: %v", err)
		}
		if up.Kind == KindSnapshot {
			snapshotSeen = true
			if up.ChunkTotal != 1 || up.FrameCount != 1 {
				t.Fatalf("快照应 1 片 1 帧: %+v", up)
			}
		}
		if up.Kind == KindSegment {
			segChunks = append(segChunks, up)
		}
	}
	if !snapshotSeen {
		t.Fatal("快照分片缺失")
	}
	if len(segChunks) != 4 {
		t.Fatalf("片段分片数 = %d, want 4", len(segChunks))
	}
	sort.Slice(segChunks, func(i, j int) bool { return segChunks[i].ChunkSeq < segChunks[j].ChunkSeq })
	var re []byte
	for _, c := range segChunks {
		if c.MediaID != segChunks[0].MediaID {
			t.Fatal("分片 mediaId 不一致")
		}
		if c.ChunkTotal != 4 || c.TotalBytes != len(seg) || c.FrameCount != 3 {
			t.Fatalf("分片元数据不一致: %+v", c)
		}
		re = append(re, c.ChunkData...)
	}
	if !bytes.Equal(re, seg) {
		t.Fatalf("重组字节不一致: %d vs %d", len(re), len(seg))
	}

	// Janitor：分片仍在队列 → 保留；全部 Ack → 清理（flushed=2）。
	u.JanitorSweep()
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		t.Fatal("分片未离队时 spool 不应被清理")
	}
	fq.ackAll()
	u.JanitorSweep()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("全部离队后 spool 应清空，剩余 %d 个文件", len(entries))
	}
	if flushed, _, _, _ := u.Stats(); flushed != 2 {
		t.Fatalf("flushed = %d, want 2（快照+片段各一）", flushed)
	}
}

// TestMediaupJanitorExpire 覆盖超龄清理（未完成上传）。
func TestMediaupJanitorExpire(t *testing.T) {
	dir := t.TempDir()
	fq := newFakeQueue()
	u := NewUploader(dir, "node-1", fq, nil, time.Hour) // retention=1h
	// 手工构造超出保留期的 spool 项（bin + meta 各一）。
	mediaID := "m-snapshot-old-1"
	if err := os.WriteFile(filepath.Join(dir, mediaID+".bin"), []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	m := meta{MediaID: mediaID, Kind: KindSnapshot, CreatedAt: time.Now().Add(-2 * time.Hour).UnixMilli(), ChunkIDs: []int64{42}}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, mediaID+".meta.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	u.JanitorSweep()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("超龄项应被清理，剩余 %d", len(entries))
	}
	if _, expired, _, _ := u.Stats(); expired != 1 {
		t.Fatalf("expired = %d, want 1", expired)
	}
}

// TestUploadChunkValidate 覆盖载荷校验矩阵。
func TestUploadChunkValidate(t *testing.T) {
	base := UploadChunk{MediaID: "m-x", Kind: KindSnapshot, DeviceName: "d", ChunkSeq: 0, ChunkTotal: 1, ChunkData: []byte{1}, TotalBytes: 1}
	if err := base.Validate(); err != nil {
		t.Fatalf("合法载荷被拒: %v", err)
	}
	bad := base
	bad.Kind = "bogus"
	if err := bad.Validate(); err == nil {
		t.Fatal("未知 kind 应拒绝")
	}
	bad = base
	bad.ChunkSeq, bad.ChunkTotal = 2, 2
	if err := bad.Validate(); err == nil {
		t.Fatal("seq 越界应拒绝")
	}
	bad = base
	bad.ChunkData = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("空分片应拒绝")
	}
	bad = base
	bad.TotalBytes = MaxMediaBytes + 1
	if err := bad.Validate(); err == nil {
		t.Fatal("超上限应拒绝")
	}
}
