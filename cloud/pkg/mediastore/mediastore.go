// Package mediastore 实现云端媒资存储（v0.43.0，spec 0016 US-3）：边侧
// MediaUpload 分片的接收、幂等落盘、组装校验与对象存取。
//
// 布局：<dir>/incoming/<mediaId>/<seq>.part（在途分片）→ 组齐 + sha256
// 校验 → <dir>/objects/<mediaId>.bin（完成对象）。元数据（完成态）经 etcd
// 写穿（/edgeflow/media/<mediaId>）；Load 恢复索引。
//
// 幂等语义：重复分片覆盖（同 seq 重写）；完成后重复分片忽略；校验失败保留
// 分片（可重传修复）。至少一次上行的重复由本包幂等消化。
package mediastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/pkg/log"
	"edgeflow/pkg/mediaup"
)

// KeyPrefixMedia 是媒资元数据的 etcd 键前缀（/<prefix>/<mediaID>）。
const KeyPrefixMedia = "/edgeflow/media/"

// 媒资状态。
const (
	StatePending  = "pending"
	StateComplete = "complete"
)

// 业务错误。
var (
	ErrNotFound     = errors.New("mediastore: 媒资不存在")
	ErrIncomplete   = errors.New("mediastore: 媒资尚未完成")
	ErrInconsistent = errors.New("mediastore: 分片元数据不一致")
)

// Media 是一段媒资的元数据（完成态持久；在途态内存 + 分片文件）。
type Media struct {
	MediaID     string `json:"mediaId"`
	Kind        string `json:"kind"`
	NodeID      string `json:"nodeId"`
	DeviceName  string `json:"deviceName"`
	StreamName  string `json:"streamName"`
	ContentType string `json:"contentType"`
	FrameCount  int    `json:"frameCount"`
	ChunkTotal  int    `json:"chunkTotal,omitempty"`
	TotalBytes  int64  `json:"totalBytes"`
	SHA256      string `json:"sha256"`
	CapturedAt  int64  `json:"capturedAt"`
	State       string `json:"state"`
	CompletedAt int64  `json:"completedAt,omitempty"`
}

// Store 是媒资存储。
type Store struct {
	dir string
	kv  etcdstore.KVStore

	mu    sync.Mutex
	items map[string]*Media // 全部已知媒资（pending + complete）
}

// NewStore 创建媒资存储（dir 为数据目录；kv 为 nil 时元数据仅内存）。
func NewStore(dir string, kv etcdstore.KVStore) *Store {
	return &Store{dir: dir, kv: kv, items: make(map[string]*Media)}
}

// Load 启动恢复：扫描 etcd 前缀重建完成态索引。
func (s *Store) Load(ctx context.Context) error {
	if s.kv == nil {
		return nil
	}
	entries, err := s.kv.ListByPrefix(ctx, KeyPrefixMedia)
	if err != nil {
		return fmt.Errorf("扫描媒资存储失败: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	restored := 0
	for _, e := range entries {
		var m Media
		if err := json.Unmarshal(e.Value, &m); err != nil {
			log.Warnf("[mediastore] 跳过损坏条目 %s: %v", e.Key, err)
			continue
		}
		if m.MediaID != "" && m.State == StateComplete {
			s.items[m.MediaID] = &m
			restored++
		}
	}
	if restored > 0 {
		log.Infof("[mediastore] 已恢复 %d 条媒资索引", restored)
	}
	return nil
}

// objectPath 完成对象路径。
func (s *Store) objectPath(mediaID string) string {
	return filepath.Join(s.dir, "objects", mediaID+".bin")
}

// incomingDir 在途分片目录。
func (s *Store) incomingDir(mediaID string) string {
	return filepath.Join(s.dir, "incoming", mediaID)
}

// PutChunk 接收一个分片：落盘 →（组齐时）组装校验 → 完成对象 + 元数据。
// 返回 completed 表示本次调用使该媒资完成（供上层触发索引挂接）。
func (s *Store) PutChunk(ctx context.Context, up *mediaup.UploadChunk) (bool, error) {
	if err := up.Validate(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 已完成：重复分片忽略（幂等）。
	if m, ok := s.items[up.MediaID]; ok && m.State == StateComplete {
		return false, nil
	}
	// 在途一致性核验（同 mediaId 的分片元数据必须一致）。
	if m, ok := s.items[up.MediaID]; ok {
		if m.TotalBytes != int64(up.TotalBytes) || m.ChunkTotal != up.ChunkTotal ||
			(m.SHA256 != "" && m.SHA256 != up.SHA256) {
			return false, ErrInconsistent
		}
	} else {
		s.items[up.MediaID] = &Media{
			MediaID:     up.MediaID,
			Kind:        up.Kind,
			NodeID:      up.NodeID,
			DeviceName:  up.DeviceName,
			StreamName:  up.StreamName,
			ContentType: up.ContentType,
			FrameCount:  up.FrameCount,
			ChunkTotal:  up.ChunkTotal,
			TotalBytes:  int64(up.TotalBytes),
			SHA256:      up.SHA256,
			CapturedAt:  up.CapturedAt,
			State:       StatePending,
		}
	}

	// 写分片（覆盖幂等）。
	dir := s.incomingDir(up.MediaID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mediastore: 创建分片目录失败: %w", err)
	}
	part := filepath.Join(dir, strconv.Itoa(up.ChunkSeq)+".part")
	if err := os.WriteFile(part, up.ChunkData, 0o644); err != nil {
		return false, fmt.Errorf("mediastore: 写分片失败: %w", err)
	}

	// 组齐判定：分片文件数 == ChunkTotal。
	files, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("mediastore: 读分片目录失败: %w", err)
	}
	have := 0
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".part" {
			have++
		}
	}
	if have < up.ChunkTotal {
		return false, nil
	}

	// 顺序拼接 + 校验。
	var raw []byte
	for seq := 0; seq < up.ChunkTotal; seq++ {
		b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(seq)+".part"))
		if err != nil {
			return false, fmt.Errorf("mediastore: 读分片 %d 失败: %w", seq, err)
		}
		raw = append(raw, b...)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != up.SHA256 {
		// 校验失败：保留分片（可重传修复），拒绝完成。
		return false, fmt.Errorf("mediastore: sha256 校验失败（mediaId=%s）", up.MediaID)
	}
	// 原子落对象（临时文件 + rename）。
	objPath := s.objectPath(up.MediaID)
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		return false, fmt.Errorf("mediastore: 创建对象目录失败: %w", err)
	}
	tmp := objPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return false, fmt.Errorf("mediastore: 写对象失败: %w", err)
	}
	if err := os.Rename(tmp, objPath); err != nil {
		return false, fmt.Errorf("mediastore: 提交对象失败: %w", err)
	}
	_ = os.RemoveAll(dir) // 分片清理（尽力而为）

	m := s.items[up.MediaID]
	m.State = StateComplete
	m.CompletedAt = time.Now().UnixMilli()
	if s.kv != nil {
		rawMeta, err := json.Marshal(m)
		if err != nil {
			return false, fmt.Errorf("mediastore: 序列化元数据失败: %w", err)
		}
		if err := s.kv.Put(ctx, KeyPrefixMedia+m.MediaID, rawMeta); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Get 返回媒资元数据副本；不存在 ErrNotFound。
func (s *Store) Get(mediaID string) (*Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.items[mediaID]
	if !ok {
		return nil, ErrNotFound
	}
	c := *m
	return &c, nil
}

// Read 读取完成媒资的字节（未完成 ErrIncomplete）。
func (s *Store) Read(mediaID string) ([]byte, error) {
	s.mu.Lock()
	m, ok := s.items[mediaID]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	if m.State != StateComplete {
		s.mu.Unlock()
		return nil, ErrIncomplete
	}
	path := s.objectPath(mediaID)
	s.mu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mediastore: 读对象失败: %w", err)
	}
	return b, nil
}

// ListSorted 返回全部媒资（按完成时间排序；诊断用）。
func (s *Store) ListSorted() []*Media {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Media, 0, len(s.items))
	for _, m := range s.items {
		c := *m
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CompletedAt < out[j].CompletedAt })
	return out
}
