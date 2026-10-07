// Package videostream 实现云端 VideoStream 资源模型（v0.43.0，spec 0016 US-4）：
// 视频流索引（CRUD/状态/快照引用/片段索引），etcd 写穿 + 内存索引
// （与 rulestore/alarmstore 同构选型）。
//
// 关联模型：Stream 关联节点（nodeId）与设备（deviceName）；媒资到达时
// Ensure 自动建流（无则创建），快照替换（最新一帧）、片段追加（去重 + 界长）。
// Delete 不删除 mediastore 中的媒资文件（留存边界——KI §44 登记）。
package videostream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/pkg/log"
	"edgeflow/pkg/mediaup"
)

// KeyPrefixVideoStreams 是视频流存储的 etcd 键前缀（/<prefix>/<name>）。
const KeyPrefixVideoStreams = "/edgeflow/videostreams/"

// MaxSegments 是单流片段的索引界长（超出丢弃最老；媒资文件不随之删除）。
const MaxSegments = 200

// 业务错误（API 层映射：ErrNotFound→404、ErrExists→409）。
var (
	ErrNotFound = errors.New("videostream: 视频流不存在")
	ErrExists   = errors.New("videostream: 视频流已存在")
)

// SegmentRef 是一条片段索引（媒资存储中的完整对象引用）。
type SegmentRef struct {
	MediaID    string `json:"mediaId"`
	CapturedAt int64  `json:"capturedAt"`        // 采集时刻（毫秒）
	FrameCount int    `json:"frameCount"`        // 帧数
	Bytes      int64  `json:"bytes"`             // 字节数
	SHA256     string `json:"sha256,omitempty"`  // 整段校验和（上传元数据带回；回放完整性审计）
	AlarmID    string `json:"alarmId,omitempty"` // 告警关联（留位）
}

// Stream 是一个视频流资源（云边共管：边侧采集、云侧索引与回放编排）。
type Stream struct {
	Name            string       `json:"name"`                      // 唯一名（URL 路径参数）
	NodeID          string       `json:"nodeId"`                    // 所属节点
	DeviceName      string       `json:"deviceName"`                // 关联设备/流名
	SourceType      string       `json:"sourceType,omitempty"`      // rtsp|mjpeg|bridge|synthetic（登记用）
	Status          string       `json:"status"`                    // online|offline|unknown
	Description     string       `json:"description,omitempty"`     // 备注
	SnapshotMediaID string       `json:"snapshotMediaId,omitempty"` // 最新快照媒资 ID
	SnapshotAt      int64        `json:"snapshotAt,omitempty"`      // 快照采集时刻（毫秒）
	LastSeenAt      int64        `json:"lastSeenAt,omitempty"`      // 最近媒资到达（毫秒）
	Segments        []SegmentRef `json:"segments"`                  // 片段索引（界长 MaxSegments，老→新）
	CreatedAt       int64        `json:"createdAt"`
	UpdatedAt       int64        `json:"updatedAt"`
}

// clone 返回深副本（防调用方改动内存态）。
func (s *Stream) clone() *Stream {
	c := *s
	c.Segments = append([]SegmentRef(nil), s.Segments...)
	return &c
}

// Patch 是更新请求的可选字段（空值不修改）。
type Patch struct {
	DeviceName  string
	SourceType  string
	Status      string
	Description string
}

// Store 是视频流存储（etcd 写穿 + 内存索引；kv 为 nil 时纯内存）。
type Store struct {
	kv      etcdstore.KVStore
	mu      sync.Mutex
	streams map[string]*Stream
}

// NewStore 创建存储（kv 为 nil = 纯内存，测试/内嵌形态）。
func NewStore(kv etcdstore.KVStore) *Store {
	return &Store{kv: kv, streams: make(map[string]*Stream)}
}

// Load 启动恢复：扫描前缀重建内存索引（损坏条目跳过，不阻断）。
func (s *Store) Load(ctx context.Context) error {
	if s.kv == nil {
		return nil
	}
	entries, err := s.kv.ListByPrefix(ctx, KeyPrefixVideoStreams)
	if err != nil {
		return fmt.Errorf("扫描视频流存储失败: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	restored := 0
	for _, e := range entries {
		var st Stream
		if err := json.Unmarshal(e.Value, &st); err != nil {
			log.Warnf("[videostream] 跳过损坏条目 %s: %v", e.Key, err)
			continue
		}
		if st.Name == "" {
			continue
		}
		s.streams[st.Name] = &st
		restored++
	}
	if restored > 0 {
		log.Infof("[videostream] 已恢复 %d 条视频流索引", restored)
	}
	return nil
}

// persist 写穿（kv 为 nil 跳过）；调用方持锁。
func (s *Store) persist(ctx context.Context, st *Stream) error {
	if s.kv == nil {
		return nil
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("序列化视频流失败: %w", err)
	}
	return s.kv.Put(ctx, KeyPrefixVideoStreams+st.Name, raw)
}

// Create 创建视频流（name 唯一；重复 ErrExists）。NodeID/DeviceName 必填。
func (s *Store) Create(ctx context.Context, st *Stream) (*Stream, error) {
	if st == nil || st.Name == "" || st.NodeID == "" || st.DeviceName == "" {
		return nil, errors.New("videostream: name/nodeId/deviceName 必填")
	}
	if st.Status == "" {
		st.Status = "unknown"
	}
	now := time.Now().UnixMilli()
	st.CreatedAt, st.UpdatedAt = now, now
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.streams[st.Name]; ok {
		return nil, ErrExists
	}
	if err := s.persist(ctx, st); err != nil {
		return nil, err
	}
	s.streams[st.Name] = st.clone()
	return st.clone(), nil
}

// Get 返回流副本；不存在 ErrNotFound。
func (s *Store) Get(name string) (*Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.streams[name]
	if !ok {
		return nil, ErrNotFound
	}
	return st.clone(), nil
}

// List 返回全部流（按名排序；nodeID 非空时过滤）。
func (s *Store) List(nodeID string) []*Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Stream, 0, len(s.streams))
	for _, st := range s.streams {
		if nodeID != "" && st.NodeID != nodeID {
			continue
		}
		out = append(out, st.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Update 更新可字段（非空者生效）；返回更新后副本。
func (s *Store) Update(ctx context.Context, name string, p Patch) (*Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.streams[name]
	if !ok {
		return nil, ErrNotFound
	}
	if p.DeviceName != "" {
		st.DeviceName = p.DeviceName
	}
	if p.SourceType != "" {
		st.SourceType = p.SourceType
	}
	if p.Status != "" {
		st.Status = p.Status
	}
	if p.Description != "" {
		st.Description = p.Description
	}
	st.UpdatedAt = time.Now().UnixMilli()
	if err := s.persist(ctx, st); err != nil {
		return nil, err
	}
	return st.clone(), nil
}

// Delete 删除流索引（不删媒资文件——留存边界）；不存在 ErrNotFound。
func (s *Store) Delete(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.streams[name]; !ok {
		return ErrNotFound
	}
	if s.kv != nil {
		if err := s.kv.Delete(ctx, KeyPrefixVideoStreams+name); err != nil {
			return err
		}
	}
	delete(s.streams, name)
	return nil
}

// AttachMedia 处理一段完成上传的媒资（spec 0016 US-4）：
//   - 流不存在 → Ensure 自动建流（SourceType=unknown）；
//   - snapshot → 替换最新快照引用；segment → 追加片段索引（按 mediaID 去重、
//     界长 MaxSegments 丢最老）；
//   - 更新 LastSeenAt/UpdatedAt。
//
// 返回更新后的流副本。
func (s *Store) AttachMedia(ctx context.Context, up mediaup.UploadChunk) (*Stream, error) {
	if up.Kind == mediaup.KindHardSample {
		// 困难样本不挂接流索引（非流媒资；云端经 mediastore 检索面查询——
		// spec 0018 US-4）。返回 (nil, nil)：调用方以 nil Stream 跳过。
		return nil, nil
	}
	name := up.StreamName
	if name == "" {
		name = up.DeviceName
	}
	if name == "" {
		return nil, errors.New("videostream: 媒资缺 streamName/deviceName")
	}
	now := time.Now().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.streams[name]
	if !ok {
		st = &Stream{
			Name:       name,
			NodeID:     up.NodeID,
			DeviceName: up.DeviceName,
			SourceType: "unknown",
			Status:     "unknown",
			CreatedAt:  now,
		}
	}
	switch up.Kind {
	case mediaup.KindSnapshot:
		st.SnapshotMediaID = up.MediaID
		st.SnapshotAt = up.CapturedAt
	case mediaup.KindSegment:
		dup := false
		for _, seg := range st.Segments {
			if seg.MediaID == up.MediaID {
				dup = true
				break
			}
		}
		if !dup {
			st.Segments = append(st.Segments, SegmentRef{
				MediaID:    up.MediaID,
				CapturedAt: up.CapturedAt,
				FrameCount: up.FrameCount,
				Bytes:      int64(up.TotalBytes),
				SHA256:     up.SHA256,
				AlarmID:    up.AlarmID,
			})
			if len(st.Segments) > MaxSegments {
				st.Segments = st.Segments[len(st.Segments)-MaxSegments:]
			}
		}
	default:
		return nil, fmt.Errorf("videostream: 未知媒资种类 %q", up.Kind)
	}
	st.LastSeenAt = now
	st.UpdatedAt = now
	if err := s.persist(ctx, st); err != nil {
		return nil, err
	}
	s.streams[name] = st
	return st.clone(), nil
}
