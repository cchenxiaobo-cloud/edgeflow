// Package mutestore 提供告警静默窗口管理（v0.58.0，spec 0031）。
//
// 职责：静默窗口（Silence）的 CRUD 与生效判定——窗口 = [from, to] 毫秒时间区间
// + 可选 nodeID 定向（空 = 全部节点）+ 原因留痕。语义：窗口生效期间的告警在
// 「活跃口径」（hierarchyview AlarmActiveCount）中排除——运维已知晓，不重复
// 计数；告警历史数据本身不动。
//
// 约束（spec 0031 裁决 2）：
//   - 窗口必须有界：to > from 且跨度 ≤30 天（防永久静默）；
//   - 窗口不可变：无更新面，调整 = 删除重建；
//   - 过期窗口惰性失效（查询/判定时按 now 判定），快照保留历史留痕；
//   - JSON 快照持久化（EDGEFLOW_CLOUDCORE_MUTE_FILE，空=内存，治理面惯例）。
//
// 并发：mu 串行化写路径；读走锁内快照。
package mutestore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 约束常量（spec 0031 裁决 2）。
const (
	maxReasonLen = 256                      // reason 字节上限
	maxWindowMs  = 30 * 24 * 60 * 60 * 1000 // 窗口上限 30 天
	maxSilences  = 200                      // 窗口总数上限（防无限增长）
)

// ErrInvalid 校验失败；ErrNotFound 不存在。
var (
	ErrInvalid  = errors.New("mutestore: 静默窗口校验失败")
	ErrNotFound = errors.New("mutestore: 静默窗口不存在")
)

// Silence 是一条静默窗口。
type Silence struct {
	ID        string `json:"id"`
	NodeID    string `json:"nodeId,omitempty"` // 空 = 全部节点
	FromMs    int64  `json:"from"`             // 窗口起点（毫秒，含）
	ToMs      int64  `json:"to"`               // 窗口终点（毫秒，含）
	Reason    string `json:"reason"`
	CreatedBy string `json:"createdBy"`
	CreatedAt int64  `json:"createdAt"`
}

// Active 报告窗口在 nowMs 时刻是否生效。
func (s Silence) Active(nowMs int64) bool {
	return nowMs >= s.FromMs && nowMs <= s.ToMs
}

// Matches 报告窗口是否作用于指定节点的告警（nodeID 空 = 全部节点）。
// 窗口未生效时恒 false（调用方通常先判 Active，本方法不重复判时间）。
func (s Silence) Matches(nodeID string) bool {
	return s.NodeID == "" || s.NodeID == nodeID
}

type snapshot struct {
	Version  int       `json:"version"`
	Silences []Silence `json:"silences"`
}

// Store 是静默窗口管理器。
type Store struct {
	mu   sync.Mutex
	file string
	data snapshot
}

// New 构建存储（file 空 = 纯内存；Load() 触发恢复）。
func New(file string) *Store {
	return &Store{file: file}
}

// Load 从快照恢复（缺失=空库；损坏=报错由调用方降级）。
func (s *Store) Load() error {
	if s.file == "" {
		return nil
	}
	data, err := os.ReadFile(s.file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("mutestore: 读取快照失败: %w", err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("mutestore: 快照解析失败: %w", err)
	}
	if snap.Silences == nil {
		snap.Silences = []Silence{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = snap
	return nil
}

// Create 创建静默窗口：nodeID 可选（空=全部）；窗口 [from,to] 必须有界
// （to > from 且 to-from ≤ 30 天）；reason ≤256B；总数 ≤200。
func (s *Store) Create(nodeID string, fromMs, toMs int64, reason, createdBy string, nowMs int64) (Silence, error) {
	nodeID = strings.TrimSpace(nodeID)
	reason = strings.TrimSpace(reason)
	if len(reason) > maxReasonLen {
		return Silence{}, fmt.Errorf("%w: reason 须 ≤%d 字节", ErrInvalid, maxReasonLen)
	}
	if toMs <= fromMs {
		return Silence{}, fmt.Errorf("%w: 窗口终点须晚于起点", ErrInvalid)
	}
	if toMs-fromMs > maxWindowMs {
		return Silence{}, fmt.Errorf("%w: 窗口上限 30 天", ErrInvalid)
	}
	id, err := randID("mute")
	if err != nil {
		return Silence{}, err
	}
	sil := Silence{ID: id, NodeID: nodeID, FromMs: fromMs, ToMs: toMs, Reason: reason, CreatedBy: createdBy, CreatedAt: nowMs}
	s.mu.Lock()
	defer s.mu.Unlock()
	// v0.61.0（R3 P2-1 处置）：总数上限检查移入临界区——原锁外读
	// len(s.data.Silences) 与 Load/Create/Delete 构成数据竞争。
	if len(s.data.Silences) >= maxSilences {
		return Silence{}, fmt.Errorf("%w: 静默窗口总数已达上限 %d", ErrInvalid, maxSilences)
	}
	s.data.Silences = append(s.data.Silences, sil)
	if err := s.persistLocked(); err != nil {
		s.data.Silences = s.data.Silences[:len(s.data.Silences)-1]
		return Silence{}, err
	}
	return sil, nil
}

// Delete 删除静默窗口（即时失效）；不存在返回 ErrNotFound。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sil := range s.data.Silences {
		if sil.ID == id {
			s.data.Silences = append(s.data.Silences[:i], s.data.Silences[i+1:]...)
			return s.persistLocked()
		}
	}
	return ErrNotFound
}

// List 返回全部窗口（创建时间降序，新→旧；空库返回空切片非 nil）。
func (s *Store) List() []Silence {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Silence, len(s.data.Silences))
	copy(out, s.data.Silences)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Muted 报告指定节点在 nowMs 时刻是否处于任一生效窗口内（空 nodeID 告警
// 仅被全节点窗口命中）。
func (s *Store) Muted(nodeID string, nowMs int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sil := range s.data.Silences {
		if sil.Active(nowMs) && sil.Matches(nodeID) {
			return true
		}
	}
	return false
}

// persistLocked 原子写快照（temp+rename；file 空=内存模式跳过）。调用方持锁。
func (s *Store) persistLocked() error {
	if s.file == "" {
		return nil
	}
	s.data.Version = 1
	data, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("mutestore: 快照序列化失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.file), 0o700); err != nil {
		return fmt.Errorf("mutestore: 创建快照目录失败: %w", err)
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("mutestore: 写快照失败: %w", err)
	}
	return os.Rename(tmp, s.file)
}

// randID 生成 <prefix>-<8 字节随机 hex>。
func randID(prefix string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("mutestore: 生成 id 失败: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(buf[:]), nil
}
