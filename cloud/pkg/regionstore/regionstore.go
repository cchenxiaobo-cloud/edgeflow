// 云端区域存储（v0.50.0，spec 0023 G03）：区域树 + 节点归属索引 + 可选
// JSON 快照持久化。
//
// 职责边界：区域归属是**静态管理面**数据（允许预配置离线节点），与
// registry 的运行视角（NodeInfo/心跳/在线态）正交——registry 不侵入，
// 查询视图在 API 层 join。持久化选型与 logstore 轻量口径一致：管理面
// 量级（区域 ≤ 数百、归属 ≤ 数千）不需要 etcd 写穿，全量快照
// （temp + rename 原子替换）+ 启动 Load 即可；多副本共享留后续（spec
// 裁决 8）。
//
// 并发模型：RWMutex 全局锁；变更方法持锁改内存并持锁写全量快照（复核
// P3 对齐注释：管理面量级 KB 级快照毫秒级写，持锁可接受；异步化留后续）
// ——写失败仅记日志，不影响内存权威态。
package regionstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"edgeflow/pkg/log"
)

// 语义错误（API 层映射：NotFound→404，Cycle/HasChildren/HasNodes→409，
// 校验类→400）。
var (
	ErrNotFound       = errors.New("区域不存在")
	ErrCycle          = errors.New("移动目标在自身子树内（会成环）")
	ErrHasChildren    = errors.New("区域存在子区域，不可删除")
	ErrHasNodes       = errors.New("区域存在节点归属，不可删除")
	ErrPolicyInvalid  = errors.New("policy invalid")
	ErrParentNotFound = errors.New("父区域不存在")
	ErrEmptyName      = errors.New("区域名不得为空")
	ErrNameTooLong    = errors.New("区域名超长（≤128B）")
	ErrDescTooLong    = errors.New("描述超长（≤512B）")
)

// 约束常量（spec 裁决 1/4）。
const (
	MaxNameBytes     = 128
	MaxDescBytes     = 512
	MaxAssignBatch   = 64
	MaxDescInputName = "desc"
)

// Region 是一个区域节点（树形单父）。
type Region struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ParentID    string `json:"parentId"` // 根为 ""
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
	// Policy 是区域级策略对象（v0.51.0，spec 0024）：≤4KB 合法 JSON 对象，
	// 本版仅存储与视图透出（不下发执行）；旧快照无此字段 Load 默认 nil。
	Policy json.RawMessage `json:"policy,omitempty"`
}

// RegionNode 是树形视图节点（GET /api/v1/regions 响应元素）。
type RegionNode struct {
	Region
	Children  []RegionNode `json:"children"`
	NodeCount int          `json:"nodeCount"` // 直接归属节点数（不含子孙）
}

// UpdateOpts 是区域更新选项（指针区分「不改」与「清空/改到根」）。
type UpdateOpts struct {
	Name        *string
	Description *string
	ParentID    *string // "" = 移到根
}

// snapshot 是持久化文档（单 JSON 对象，原子重写）。
type snapshot struct {
	Regions     []Region          `json:"regions"`
	Assignments map[string]string `json:"assignments"` // nodeID → regionID
}

// Store 是区域存储。
type Store struct {
	mu         sync.RWMutex
	regions    map[string]*Region
	nodeAssign map[string]string // nodeID → regionID
	file       string            // 快照路径；"" = 纯内存
}

// New 创建存储；file 非空时尝试 Load（文件不存在视为空库起步）。
func New(file string) *Store {
	s := &Store{regions: map[string]*Region{}, nodeAssign: map[string]string{}, file: file}
	if file != "" {
		if err := s.load(); err != nil {
			log.Warnf("[regionstore] 快照加载失败（空库起步）: %v", err)
		}
	}
	return s
}

func nowMs() int64 { return time.Now().UnixMilli() }

// newRegionID 在给定集合内防撞生成 rg- + 8hex（锁内调用；量级下冲突概率可忽略）。
func newRegionID(taken map[string]*Region) string {
	for {
		id := fmt.Sprintf("rg-%08x", rand.Uint32())
		if _, ok := taken[id]; !ok {
			return id
		}
	}
}

// validateNameDesc 校验名称与描述。
func validateNameDesc(name, desc string) error {
	if len(name) == 0 {
		return ErrEmptyName
	}
	if len(name) > MaxNameBytes {
		return ErrNameTooLong
	}
	if len(desc) > MaxDescBytes {
		return ErrDescTooLong
	}
	return nil
}

// Create 创建区域。parentID 为 "" 表示根。
func (s *Store) Create(name, parentID, desc string) (Region, error) {
	if err := validateNameDesc(name, desc); err != nil {
		return Region{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if parentID != "" {
		if _, ok := s.regions[parentID]; !ok {
			return Region{}, ErrParentNotFound
		}
	}
	r := Region{
		ID:          newRegionID(s.regions),
		Name:        name,
		ParentID:    parentID,
		Description: desc,
		CreatedAt:   nowMs(),
		UpdatedAt:   nowMs(),
	}
	s.regions[r.ID] = &r
	s.persistLocked()
	return r, nil
}

// isDescendant 判断 target 是否在 ancestor 的子树内（含相等）。
func (s *Store) isDescendant(ancestorID, targetID string) bool {
	cur := targetID
	for cur != "" {
		if cur == ancestorID {
			return true
		}
		r, ok := s.regions[cur]
		if !ok {
			return false
		}
		cur = r.ParentID
	}
	return false
}

// Update 更新区域（改名/描述/移动）。移动防环：newParent 在自身子树内 →
// ErrCycle；newParent 不存在 → ErrParentNotFound。
func (s *Store) Update(id string, opts UpdateOpts) (Region, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.regions[id]
	if !ok {
		return Region{}, ErrNotFound
	}
	if opts.Name != nil {
		if err := validateNameDesc(*opts.Name, ""); err != nil {
			return Region{}, err
		}
	}
	if opts.Description != nil && len(*opts.Description) > MaxDescBytes {
		return Region{}, ErrDescTooLong
	}
	if opts.ParentID != nil {
		np := *opts.ParentID
		if np != "" {
			if _, ok := s.regions[np]; !ok {
				return Region{}, ErrParentNotFound
			}
			if s.isDescendant(id, np) {
				return Region{}, ErrCycle
			}
		}
		r.ParentID = np
	}
	if opts.Name != nil {
		r.Name = *opts.Name
	}
	if opts.Description != nil {
		r.Description = *opts.Description
	}
	r.UpdatedAt = nowMs()
	out := *r
	s.persistLocked()
	return out, nil
}

// Delete 删除区域（必须为空：无子区域、无节点归属）。归属记录随区域删除。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.regions[id]; !ok {
		return ErrNotFound
	}
	for _, r := range s.regions {
		if r.ParentID == id {
			return ErrHasChildren
		}
	}
	for _, rid := range s.nodeAssign {
		if rid == id {
			return ErrHasNodes
		}
	}
	delete(s.regions, id)
	s.persistLocked()
	return nil
}

// AssignNodes 批量把节点指派到区域（幂等换区：已在其他区域的自动迁出）。
// 返回 assigned（实际写入数）与 skipped（空 nodeId 数）。批量上限 64。
func (s *Store) AssignNodes(regionID string, nodeIDs []string) (assigned, skipped int, err error) {
	if len(nodeIDs) > MaxAssignBatch {
		return 0, 0, fmt.Errorf("批量上限 %d", MaxAssignBatch)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.regions[regionID]; !ok {
		return 0, 0, ErrNotFound
	}
	for _, nid := range nodeIDs {
		if nid == "" {
			skipped++
			continue
		}
		s.nodeAssign[nid] = regionID
		assigned++
	}
	if assigned > 0 {
		s.persistLocked()
	}
	return assigned, skipped, nil
}

// RegionByID 按直取区域（无归属关系，存在性检查用）。
func (s *Store) RegionByID(id string) (Region, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.regions[id]
	if !ok {
		return Region{}, false
	}
	return *r, true
}

// AllAssignedNodes 返回全部有归属的节点 ID（稳定排序；组合查询全量视图用）。
func (s *Store) AllAssignedNodes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.nodeAssign))
	for nid := range s.nodeAssign {
		out = append(out, nid)
	}
	sort.Strings(out)
	return out
}

// All 返回全量区域值拷贝（稳定排序；层级视图聚合用）。
func (s *Store) All() []Region {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Region, 0, len(s.regions))
	for id := range s.regions {
		out = append(out, *s.regions[id])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Assignments 返回归属表值拷贝（nodeID → regionID；层级视图聚合用）。
func (s *Store) Assignments() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.nodeAssign))
	for nid, rid := range s.nodeAssign {
		out[nid] = rid
	}
	return out
}

// NodesInRegion 返回直接归属该区域的节点 ID（稳定排序）。
func (s *Store) NodesInRegion(regionID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for nid, rid := range s.nodeAssign {
		if rid == regionID {
			out = append(out, nid)
		}
	}
	sort.Strings(out)
	return out
}

// RegionOfNode 返回节点归属的区域（无归属 → false）。
func (s *Store) RegionOfNode(nodeID string) (Region, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rid, ok := s.nodeAssign[nodeID]
	if !ok {
		return Region{}, false
	}
	r, ok := s.regions[rid]
	if !ok {
		return Region{}, false // 不应发生（Delete 已清理归属）
	}
	return *r, true
}

// Tree 返回全量树形（兄弟按 ID 序）。nodeCount 为直接归属数。
// 两阶段构建：先建父子索引，再从根递归按值构建（避免顺序依赖的浅拷贝问题）。
func (s *Store) Tree() []RegionNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	counts := map[string]int{}
	for _, rid := range s.nodeAssign {
		counts[rid]++
	}
	var ids []string
	for id := range s.regions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	childrenOf := map[string][]string{}
	var rootIDs []string
	for _, id := range ids {
		pid := s.regions[id].ParentID
		if pid == "" || s.regions[pid] == nil {
			rootIDs = append(rootIDs, id) // 防御：孤儿父视为根
		} else {
			childrenOf[pid] = append(childrenOf[pid], id)
		}
	}
	var build func(id string) RegionNode
	build = func(id string) RegionNode {
		n := RegionNode{Region: *s.regions[id], Children: []RegionNode{}, NodeCount: counts[id]}
		for _, cid := range childrenOf[id] {
			n.Children = append(n.Children, build(cid))
		}
		return n
	}
	var roots []RegionNode
	for _, id := range rootIDs {
		roots = append(roots, build(id))
	}
	return roots
}

// Count 返回区域总数。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.regions)
}

// ─── 持久化（spec 裁决 8：全量快照 temp+rename 原子写）──────────────────

// SetPolicy 原子更新区域策略（v0.51.0，spec 0024）：policy 为 nil/空对象语义为
// 清除（落 nil）；非对象（数组/标量）或超 4KB 返回 ErrPolicyInvalid。未知区域
// ErrNotFound。快照即时持久化（持锁内写，口径同其余变更方法）。
func (s *Store) SetPolicy(id string, policy json.RawMessage, nowMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.regions[id]
	if !ok {
		return ErrNotFound
	}
	if len(policy) > 0 {
		var probe any
		if err := json.Unmarshal(policy, &probe); err != nil {
			return ErrPolicyInvalid
		}
		if probe != nil { // JSON null = 清除语义（spec 裁决 4），放行落 nil
			if _, isObj := probe.(map[string]any); !isObj {
				return ErrPolicyInvalid
			}
			if len(policy) > 4096 {
				return ErrPolicyInvalid
			}
		}
	}
	r.Policy = normalizePolicy(policy)
	r.UpdatedAt = nowMs
	s.regions[id] = r
	s.persistLocked()
	return nil
}

// normalizePolicy 规整策略存储形态：nil/空对象 → nil；其余对象 → 紧凑 JSON。
func normalizePolicy(policy json.RawMessage) json.RawMessage {
	if len(policy) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(policy))
	dec.UseNumber() // 大整数保真（复核 P3-②）：避免 float64 静默改写 >2^53 字面量
	var probe any
	if err := dec.Decode(&probe); err != nil {
		return nil
	}
	if obj, ok := probe.(map[string]any); !ok || len(obj) == 0 {
		return nil
	}
	b, _ := json.Marshal(probe)
	return json.RawMessage(b)
}

func (s *Store) persistLocked() {
	if s.file == "" {
		return
	}
	snap := snapshot{Regions: make([]Region, 0, len(s.regions)), Assignments: map[string]string{}}
	for _, r := range s.regions {
		snap.Regions = append(snap.Regions, *r)
	}
	for nid, rid := range s.nodeAssign {
		snap.Assignments[nid] = rid
	}
	data, err := json.Marshal(snap)
	if err != nil {
		log.Warnf("[regionstore] 快照序列化失败: %v", err)
		return
	}
	tmp := s.file + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.file), 0o755); err != nil {
		log.Warnf("[regionstore] 快照目录创建失败: %v", err)
		return
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Warnf("[regionstore] 快照写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.file); err != nil {
		log.Warnf("[regionstore] 快照替换失败: %v", err)
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
	for i := range snap.Regions {
		r := snap.Regions[i]
		s.regions[r.ID] = &r
	}
	if snap.Assignments != nil {
		for nid, rid := range snap.Assignments {
			if _, ok := s.regions[rid]; ok {
				s.nodeAssign[nid] = rid
			}
		}
	}
	// 复核 P3-③：Load 后校验父链——手改快照可能引入环，isDescendant 沿父链
	// 死循环会持锁挂死。发现环则断链该区域（ParentID 置根）并告警。
	for id := range s.regions {
		seen := map[string]struct{}{}
		cur := id
		for cur != "" {
			if _, cyc := seen[cur]; cyc {
				log.Warnf("[regionstore] 快照父链成环（已断链为根）: %s", id)
				s.regions[id].ParentID = ""
				break
			}
			seen[cur] = struct{}{}
			r, ok := s.regions[cur]
			if !ok {
				break
			}
			cur = r.ParentID
		}
	}
	return nil
}
