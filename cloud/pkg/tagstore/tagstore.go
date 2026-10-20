// 云端标签存储（v0.50.0，spec 0023 G04）：三类锚点（node/device/model）
// 的 KV 标签 + 可选 JSON 快照持久化。
//
// 职责边界：打标面与资源面解耦——锚点存在性不校验（资源删除后标签残留
// 为已知边界，KI 登记）；标签值语义 {k:v} upsert 幂等、{k:null} 删标；
// 本版组合查询只做节点面（设备/模型锚点可打标，查询按需扩，spec 裁决 5/7）。
// 持久化与 regionstore 同口径：全量快照 temp+rename，env 可选，空 = 纯内存。
package tagstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"edgeflow/pkg/log"
)

// 语义错误（API 层映射：校验类→400）。
var (
	ErrBadAnchorType = errors.New("anchorType 须为 node/device/model")
	ErrBadAnchorID   = errors.New("anchorId 不得为空且 ≤256B")
	ErrBadTagKey     = errors.New("标签 key 须匹配 [a-zA-Z0-9._-] 且 ≤64B")
	ErrTagValTooLong = errors.New("标签 value 超长（≤256B）")
	ErrTooManyKeys   = errors.New("单次批量 ≤32 键")
)

// 锚点与 KV 约束（spec 裁决 5/6）。
const (
	AnchorNode   = "node"
	AnchorDevice = "device"
	AnchorModel  = "model"
	MaxAnchorID  = 256
	MaxKeyBytes  = 64
	MaxValBytes  = 256
	MaxKeysBatch = 32
)

// ValidAnchor 判断锚点类型合法性。
func ValidAnchor(t string) bool {
	return t == AnchorNode || t == AnchorDevice || t == AnchorModel
}

// validKey 判断 key 字符集（[a-zA-Z0-9._-]）与长度。
func validKey(k string) bool {
	if k == "" || len(k) > MaxKeyBytes {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// snapshot 是持久化文档。
type snapshot struct {
	// type → anchorID → key → value
	Tags map[string]map[string]map[string]string `json:"tags"`
}

// Store 是标签存储。
type Store struct {
	mu    sync.RWMutex
	tags  map[string]map[string]map[string]string
	file  string
	clock func() int64 // 测试注入用（默认 time.Now().UnixMilli）
}

// New 创建存储；file 非空时尝试 Load（文件不存在视为空库起步）。
func New(file string) *Store {
	s := &Store{
		tags:  map[string]map[string]map[string]string{},
		file:  file,
		clock: func() int64 { return time.Now().UnixMilli() },
	}
	if file != "" {
		if err := s.load(); err != nil {
			log.Warnf("[tagstore] 快照加载失败（空库起步）: %v", err)
		}
	}
	return s
}

// SetTags 对锚点批量打标/删标（upsert 幂等；value 为 nil 删标）。
// 返回实际变更的键数（含删除）。
func (s *Store) SetTags(anchorType, anchorID string, kvs map[string]*string) (changed int, err error) {
	if !ValidAnchor(anchorType) {
		return 0, ErrBadAnchorType
	}
	if anchorID == "" || len(anchorID) > MaxAnchorID {
		return 0, ErrBadAnchorID
	}
	if len(kvs) == 0 || len(kvs) > MaxKeysBatch {
		return 0, ErrTooManyKeys
	}
	for k, v := range kvs {
		if !validKey(k) {
			return 0, fmt.Errorf("%w: %q", ErrBadTagKey, k)
		}
		if v != nil && len(*v) > MaxValBytes {
			return 0, fmt.Errorf("%w: key %q", ErrTagValTooLong, k)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	level, ok := s.tags[anchorType]
	if !ok {
		level = map[string]map[string]string{}
		s.tags[anchorType] = level
	}
	kv, ok := level[anchorID]
	if !ok {
		kv = map[string]string{}
		level[anchorID] = kv
	}
	for k, v := range kvs {
		if v == nil {
			if _, exists := kv[k]; exists {
				delete(kv, k)
				changed++
			}
			continue
		}
		if old, exists := kv[k]; !exists || old != *v {
			kv[k] = *v
			changed++
		}
	}
	// 空壳回收
	if len(kv) == 0 {
		delete(level, anchorID)
	}
	if len(level) == 0 {
		delete(s.tags, anchorType)
	}
	if changed > 0 {
		s.persistLocked()
	}
	return changed, nil
}

// TagsOf 返回锚点标签（拷贝；无标签 → 空 map）。
func (s *Store) TagsOf(anchorType, anchorID string) map[string]string {
	out := map[string]string{}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.tags[anchorType][anchorID] {
		out[k] = v
	}
	return out
}

// AnchorsWithTag 返回某锚点类型下命中过滤条件的锚点 ID（稳定排序）。
// tagKey 非空 = 必须存在该键；tagValue 非空 = 该键值精确匹配（须配 tagKey）。
// 两条件皆空 = 返回该类型下所有有标签的锚点。
func (s *Store) AnchorsWithTag(anchorType, tagKey, tagValue string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for anchorID, kv := range s.tags[anchorType] {
		if tagKey != "" {
			v, ok := kv[tagKey]
			if !ok {
				continue
			}
			if tagValue != "" && v != tagValue {
				continue
			}
		}
		out = append(out, anchorID)
	}
	sort.Strings(out)
	return out
}

// Count 返回（锚点类型, 锚点）二元组总数。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, level := range s.tags {
		n += len(level)
	}
	return n
}

// ─── 持久化（与 regionstore 同口径：全量快照原子写）─────────────────────

func (s *Store) persistLocked() {
	if s.file == "" {
		return
	}
	snap := snapshot{Tags: s.tags}
	data, err := json.Marshal(snap)
	if err != nil {
		log.Warnf("[tagstore] 快照序列化失败: %v", err)
		return
	}
	tmp := s.file + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.file), 0o755); err != nil {
		log.Warnf("[tagstore] 快照目录创建失败: %v", err)
		return
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Warnf("[tagstore] 快照写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.file); err != nil {
		log.Warnf("[tagstore] 快照替换失败: %v", err)
	}
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("快照解析失败: %w", err)
	}
	if snap.Tags != nil {
		s.tags = snap.Tags
	}
	return nil
}
