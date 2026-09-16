// Package rulestore 实现规则包的云端存储（v0.37.0）。
//
// 存储形态与 devicestatus 同构：内存缓存 + 可选 etcd 写穿（kv 非 nil 时：
// 先写 etcd 成功才更新内存，失败返回 error 且内存不动；kv 为 nil 时纯内存，
// 供测试与内嵌形态使用）。
//
// 键空间（etcd，前缀 /edgeflow/ruleset）：
//   - /edgeflow/ruleset/items/<ruleID>   规则单条（rules.Rule JSON）；
//   - /edgeflow/ruleset/governance       治理策略全量（[]GovernancePolicy JSON）；
//   - /edgeflow/ruleset/version          规则包版本（int64 JSON，毫秒时间戳，
//     变更时取 max(now, 当前+1) 保证单调递增——边侧"陈旧版本拒绝"依赖此性质）。
//
// 事件 ring：最近 EventRingCapacity 条 RuleEvent（FIFO 滚动，内存不落盘）。
// 多副本聚合与持久化归档见 KNOWN-ISSUES §38。
package rulestore

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
	"edgeflow/pkg/rules"
)

// 键空间常量。
const (
	// KeyPrefixRuleStore 是规则存储在 etcd 键空间中的根前缀。
	KeyPrefixRuleStore = "/edgeflow/ruleset"
	keyVersion         = KeyPrefixRuleStore + "/version"
	keyGovernance      = KeyPrefixRuleStore + "/governance"
	keyItemsPrefix     = KeyPrefixRuleStore + "/items/"
)

// EventRingCapacity 是规则事件内存 ring 的容量（超出丢最旧）。
const EventRingCapacity = 500

// 哨兵错误：API 层据此映射 404/409/400。
var (
	// ErrExists 表示规则已存在（创建冲突）。
	ErrExists = errors.New("规则已存在")
	// ErrNotFound 表示规则不存在。
	ErrNotFound = errors.New("规则不存在")
	// ErrInvalid 表示输入校验失败（API 层映射 400）。
	ErrInvalid = errors.New("无效输入")
)

// KVStore 是本包对底层 KV 存储的消费接口（cloud/pkg/etcdstore 基础层
// 合同别名；切外部 etcd 时业务层零改动，与 devicestatus 同约定）。
type KVStore = etcdstore.KVStore

// Store 是规则包存储（内存缓存 + 可选 etcd 写穿）。
// 零值不可用，须经 New 构造。
type Store struct {
	mu      sync.RWMutex
	kv      KVStore
	rules   map[string]rules.Rule
	gov     []rules.GovernancePolicy
	version int64
	events  []rules.Event // FIFO ring（时间升序 append）
}

// New 创建存储；kv 为 nil 时纯内存（测试/内嵌形态）。
func New(kv KVStore) *Store {
	return &Store{kv: kv, rules: make(map[string]rules.Rule)}
}

// Load 从 etcd 恢复全量状态（启动时调用；kv 为 nil 时空操作）。
// 单条损坏数据跳过并 Warn（其余照常恢复）；version 缺失/非法时保持 0
// （下次变更 bump 取 max(now, 1) ≥ now，仍单调）。
func (s *Store) Load(ctx context.Context) error {
	if s.kv == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if raw, err := s.kv.Get(ctx, keyVersion); err != nil {
		return fmt.Errorf("读取规则版本失败: %w", err)
	} else if len(raw) > 0 {
		var v int64
		if err := json.Unmarshal(raw, &v); err == nil && v > 0 {
			s.version = v
		} else {
			log.Warnf("规则版本数据非法（%q），忽略", string(raw))
		}
	}
	if raw, err := s.kv.Get(ctx, keyGovernance); err != nil {
		return fmt.Errorf("读取治理策略失败: %w", err)
	} else if len(raw) > 0 {
		var ps []rules.GovernancePolicy
		if err := json.Unmarshal(raw, &ps); err == nil {
			s.gov = ps
		} else {
			log.Warnf("治理策略数据非法，忽略（%d 字节）", len(raw))
		}
	}
	entries, err := s.kv.ListByPrefix(ctx, keyItemsPrefix)
	if err != nil {
		return fmt.Errorf("扫描规则条目失败: %w", err)
	}
	restored := 0
	for _, e := range entries {
		var r rules.Rule
		if err := json.Unmarshal(e.Value, &r); err != nil || r.RuleID == "" {
			log.Warnf("跳过损坏的规则条目（key=%s）", e.Key)
			continue
		}
		s.rules[r.RuleID] = r
		restored++
	}
	log.Infof("规则存储已加载：规则 %d 条，治理策略 %d 条，版本 %d", restored, len(s.gov), s.version)
	return nil
}

// persistLocked 写穿单键（kv 为 nil 时直接成功；调用方持锁）。
func (s *Store) persistLocked(ctx context.Context, key string, value any) error {
	if s.kv == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	if err := s.kv.Put(ctx, key, raw); err != nil {
		return fmt.Errorf("写穿 etcd 失败: %w", err)
	}
	return nil
}

// bumpVersionLocked 递增版本号（max(now, 当前+1)）并尽力持久化
// （持久化失败不阻断：内存值已单调，逻辑安全；下次变更会重写）。
// 调用方持锁。
func (s *Store) bumpVersionLocked(ctx context.Context) {
	v := time.Now().UnixMilli()
	if v <= s.version {
		v = s.version + 1
	}
	s.version = v
	if s.kv != nil {
		if raw, err := json.Marshal(v); err == nil {
			if err := s.kv.Put(ctx, keyVersion, raw); err != nil {
				log.Warnf("规则版本持久化失败（内存已递增至 %d）: %v", v, err)
			}
		}
	}
}

// CreateRule 创建规则（校验 → 冲突检查 → 写穿 → 入内存 → 版本递增）。
func (s *Store) CreateRule(ctx context.Context, r rules.Rule) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rules[r.RuleID]; ok {
		return ErrExists
	}
	if err := s.persistLocked(ctx, keyItemsPrefix+r.RuleID, r); err != nil {
		return err
	}
	s.rules[r.RuleID] = r
	s.bumpVersionLocked(ctx)
	return nil
}

// UpdateRule 更新规则（校验 → 存在检查 → 写穿 → 替换 → 版本递增）。
func (s *Store) UpdateRule(ctx context.Context, r rules.Rule) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rules[r.RuleID]; !ok {
		return ErrNotFound
	}
	if err := s.persistLocked(ctx, keyItemsPrefix+r.RuleID, r); err != nil {
		return err
	}
	s.rules[r.RuleID] = r
	s.bumpVersionLocked(ctx)
	return nil
}

// DeleteRule 删除规则（存在检查 → etcd 删除 → 内存删除 → 版本递增）。
func (s *Store) DeleteRule(ctx context.Context, ruleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rules[ruleID]; !ok {
		return ErrNotFound
	}
	if s.kv != nil {
		if err := s.kv.Delete(ctx, keyItemsPrefix+ruleID); err != nil {
			return fmt.Errorf("删除 etcd 规则失败: %w", err)
		}
	}
	delete(s.rules, ruleID)
	s.bumpVersionLocked(ctx)
	return nil
}

// GetRule 查询单条规则。
func (s *Store) GetRule(ruleID string) (rules.Rule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rules[ruleID]
	return r, ok
}

// ListRules 返回全部规则（按 ruleId 排序，输出确定性）。
func (s *Store) ListRules() []rules.Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]rules.Rule, 0, len(s.rules))
	for _, r := range s.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleID < out[j].RuleID })
	return out
}

// CountRules 返回规则条数（诊断用）。
func (s *Store) CountRules() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.rules)
}

// SetGovernance 全量替换治理策略（逐条校验 + 唯一键检查 → 写穿 → 替换
// → 版本递增）。空列表合法（清空）。校验复用 rules.RuleSet.Validate 的
// 治理部分（含命名空间归一与重复键检查），保证与下发侧同一口径。
func (s *Store) SetGovernance(ctx context.Context, ps []rules.GovernancePolicy) error {
	if err := (&rules.RuleSet{Version: 1, Governance: ps}).Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistLocked(ctx, keyGovernance, ps); err != nil {
		return err
	}
	s.gov = append([]rules.GovernancePolicy(nil), ps...)
	s.bumpVersionLocked(ctx)
	return nil
}

// Governance 返回治理策略快照（拷贝）。
func (s *Store) Governance() []rules.GovernancePolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]rules.GovernancePolicy(nil), s.gov...)
}

// Version 返回当前规则包版本。
func (s *Store) Version() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// RuleSet 打包当前全量规则包（版本 + 规则 + 治理），供下发使用。
func (s *Store) RuleSet() rules.RuleSet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := rules.RuleSet{
		Version:    s.version,
		Rules:      make([]rules.Rule, 0, len(s.rules)),
		Governance: append([]rules.GovernancePolicy(nil), s.gov...),
	}
	for _, r := range s.rules {
		out.Rules = append(out.Rules, r)
	}
	sort.Slice(out.Rules, func(i, j int) bool { return out.Rules[i].RuleID < out.Rules[j].RuleID })
	return out
}

// AppendEvent 追加一条规则事件到内存 ring（超容量丢最旧）。
func (s *Store) AppendEvent(ev rules.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	if len(s.events) > EventRingCapacity {
		s.events = s.events[len(s.events)-EventRingCapacity:]
	}
}

// EventFilter 是 ListEvents 的过滤条件（零值不参与过滤）。
type EventFilter struct {
	RuleID     string
	DeviceName string
	Limit      int // <=0 时默认 50
}

// defaultEventLimit 是事件查询默认条数。
const defaultEventLimit = 50

// ListEvents 返回符合条件的事件（按时间倒序——最新在前；上限 Limit）。
func (s *Store) ListEvents(f EventFilter) []rules.Event {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultEventLimit
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]rules.Event, 0, min(limit, len(s.events)))
	for i := len(s.events) - 1; i >= 0 && len(out) < limit; i-- {
		ev := s.events[i]
		if f.RuleID != "" && ev.RuleID != f.RuleID {
			continue
		}
		if f.DeviceName != "" && ev.DeviceName != f.DeviceName {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// CountEvents 返回 ring 中事件条数（诊断用）。
func (s *Store) CountEvents() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.events)
}

// min 返回较小值（Go 1.21+ 内置 min 的本地包装，保持可读性）。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
