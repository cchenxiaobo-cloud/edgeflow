// 统一告警中心（v0.40.0，spec 0013 US-3）：云端告警存储与生命周期操作。
//
// 存储选型（与 rulestore 同构）：etcd 写穿（键 /edgeflow/alarms/<alarmID>，
// 值为 alarm.Alarm JSON）+ 内存索引；kv 为 nil 时纯内存（测试/内嵌形态）。
// 写穿语义：先写 etcd 成功才更新内存，失败返回 error 且内存不动。
//
// 生命周期（收/派/闭环）：raised → acked / assigned → closed（closed 终态）。
// 边侧迟到重发（AlarmEvent 同 alarmID）只合并 Count/Message/UpdatedAt，
// 状态不回退（alarm.StateRank 守卫）；closed 后的迟到重发整体忽略。
//
// 工单集成点：TicketSink 接口——assign 成功后回调（logTicketSink 留位实现，
// ITSM 对接后续版本接入）；回调失败仅告警不阻断派单。
package alarmstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/pkg/alarm"
	"edgeflow/pkg/log"
)

// KeyPrefixAlarms 是告警存储的 etcd 键前缀（/<prefix>/<alarmID>）。
const KeyPrefixAlarms = "/edgeflow/alarms/"

// 业务错误（API 层映射：ErrNotFound→404、ErrInvalidTransition→409）。
var (
	ErrNotFound          = errors.New("alarmstore: 告警不存在")
	ErrInvalidTransition = errors.New("alarmstore: 告警状态迁移非法")
)

// TicketSink 是工单集成点接口（标准接口留位）：assign 成功后同步回调。
// 实现方不得阻塞、不得反向调用 Store。
type TicketSink interface {
	OnAssign(a alarm.Alarm)
}

// TicketSinkFunc 是函数形态适配器。
type TicketSinkFunc func(a alarm.Alarm)

// OnAssign 实现 TicketSink。
func (f TicketSinkFunc) OnAssign(a alarm.Alarm) { f(a) }

// logTicketSink 是默认实现：日志留痕（ITSM 对接后续版本接入）。
type logTicketSink struct{}

// OnAssign 输出派单留痕日志。
func (logTicketSink) OnAssign(a alarm.Alarm) {
	log.Infof("[工单集成点] 告警 %s（%s/%s）派单给 %s（ticketRef=%s）——留位实现，ITSM 对接待后续版本",
		a.AlarmID, a.Namespace, a.DeviceName, a.AssignedTo, a.TicketRef)
}

// Store 是云端告警存储（内存索引 + etcd 写穿）。
type Store struct {
	mu      sync.Mutex
	kv      etcdstore.KVStore // nil = 纯内存（测试/内嵌形态）
	alarms  map[string]*alarm.Alarm
	tickets TicketSink
}

// NewStore 创建告警存储；kv 允许 nil（纯内存）；tickets 允许 nil（默认日志留痕）。
func NewStore(kv etcdstore.KVStore, tickets TicketSink) *Store {
	if tickets == nil {
		tickets = logTicketSink{}
	}
	return &Store{kv: kv, alarms: make(map[string]*alarm.Alarm), tickets: tickets}
}

// Load 启动恢复：扫描前缀重建内存索引（损坏条目跳过，不阻断）。
func (s *Store) Load(ctx context.Context) error {
	if s.kv == nil {
		return nil
	}
	entries, err := s.kv.ListByPrefix(ctx, KeyPrefixAlarms)
	if err != nil {
		return fmt.Errorf("扫描告警存储失败: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	restored := 0
	for _, e := range entries {
		var a alarm.Alarm
		if err := json.Unmarshal(e.Value, &a); err != nil || a.AlarmID == "" {
			log.Warnf("跳过损坏的告警条目（key=%s）", e.Key)
			continue
		}
		cp := a
		s.alarms[a.AlarmID] = &cp
		restored++
	}
	log.Infof("告警中心已加载：告警 %d 条", restored)
	return nil
}

// Upsert 边侧上报合并（AlarmEvent 接收路径）：新 ID 直接入库；已有 ID 合并
// Count/Message/UpdatedAt（状态只进不退，closed 后忽略迟到重发）。
func (s *Store) Upsert(ctx context.Context, a alarm.Alarm) error {
	if err := a.Validate(); err != nil {
		return fmt.Errorf("告警校验失败: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.alarms[a.AlarmID]
	if ok {
		if existing.State == alarm.StateClosed {
			return nil // 终态不可复活：迟到重发整体忽略（边界登记 KI §41）
		}
		merged := existing.Clone()
		if a.Count > merged.Count {
			merged.Count = a.Count
		}
		if a.UpdatedAt > merged.UpdatedAt {
			merged.UpdatedAt = a.UpdatedAt
			merged.Message = a.Message // 最新一次触发渲染
		}
		// 状态仅在来源秩更高且迁移合法时前移（edge 恒发 raised，通常不触发）。
		if alarm.StateRank(a.State) > alarm.StateRank(merged.State) && alarm.CanTransition(merged.State, a.State) {
			merged.State = a.State
		}
		a = merged
	}
	if err := s.putKV(ctx, a); err != nil {
		return err
	}
	cp := a
	s.alarms[a.AlarmID] = &cp
	return nil
}

// Ack 确认告警（operator 必填；raised → acked；assigned 后确认拒绝 409）。
func (s *Store) Ack(ctx context.Context, alarmID, operator string, nowMs int64) (alarm.Alarm, error) {
	return s.transition(ctx, alarmID, alarm.StateAcked, nowMs, func(a *alarm.Alarm) {
		a.AckedBy = operator
		a.AckedAt = nowMs
	})
}

// Assign 派单（operator/assignee 必填；raised/acked → assigned）；
// 成功后回调工单集成点（回调失败仅告警，不阻断派单结果）。
func (s *Store) Assign(ctx context.Context, alarmID, operator, assignee, ticketRef string, nowMs int64) (alarm.Alarm, error) {
	a, err := s.transition(ctx, alarmID, alarm.StateAssigned, nowMs, func(a *alarm.Alarm) {
		a.AssignedTo = assignee
		a.TicketRef = ticketRef
		_ = operator // operator 记入审计台账（API 层），不冗余入告警事实
	})
	if err != nil {
		return a, err
	}
	s.tickets.OnAssign(a)
	return a, nil
}

// Close 闭环（任意非 closed 状态可闭环；closed 终态）。
func (s *Store) Close(ctx context.Context, alarmID, operator string, nowMs int64) (alarm.Alarm, error) {
	return s.transition(ctx, alarmID, alarm.StateClosed, nowMs, func(a *alarm.Alarm) {
		a.ClosedBy = operator
		a.ClosedAt = nowMs
	})
}

// transition 状态迁移（写穿成功才更新内存；非法迁移返回原值 + ErrInvalidTransition）。
func (s *Store) transition(ctx context.Context, alarmID, to string, nowMs int64, decorate func(*alarm.Alarm)) (alarm.Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.alarms[alarmID]
	if !ok {
		return alarm.Alarm{}, ErrNotFound
	}
	if !alarm.CanTransition(existing.State, to) {
		return existing.Clone(), ErrInvalidTransition
	}
	updated := existing.Clone()
	updated.State = to
	decorate(&updated)
	updated.UpdatedAt = nowMs
	if err := s.putKV(ctx, updated); err != nil {
		return alarm.Alarm{}, err
	}
	s.alarms[alarmID] = &updated
	return updated.Clone(), nil
}

// List 按条件查询（UpdatedAt 降序；零值条件不过滤；limit<=0 默认 200）。
func (s *Store) List(nodeID, state, severity string, limit int) []alarm.Alarm {
	if limit <= 0 {
		limit = 200
	}
	s.mu.Lock()
	out := make([]alarm.Alarm, 0, len(s.alarms))
	for _, a := range s.alarms {
		if nodeID != "" && a.NodeID != nodeID {
			continue
		}
		if state != "" && a.State != state {
			continue
		}
		if severity != "" && a.Severity != severity {
			continue
		}
		out = append(out, a.Clone())
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].AlarmID < out[j].AlarmID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Get 单条查询（无 → ErrNotFound）。
func (s *Store) Get(alarmID string) (alarm.Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.alarms[alarmID]
	if !ok {
		return alarm.Alarm{}, ErrNotFound
	}
	return a.Clone(), nil
}

// Stats 统计（byState / bySeverity / total）。
type Stats struct {
	ByState    map[string]int64 `json:"byState"`
	BySeverity map[string]int64 `json:"bySeverity"`
	Total      int64            `json:"total"`
}

// Stats 返回当前统计快照。
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{ByState: map[string]int64{}, BySeverity: map[string]int64{}}
	for _, a := range s.alarms {
		st.ByState[a.State]++
		st.BySeverity[a.Severity]++
		st.Total++
	}
	return st
}

// Count 返回告警总数（测试与诊断用）。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.alarms)
}

// putKV etcd 写穿（kv=nil 跳过；调用方持锁）。
func (s *Store) putKV(ctx context.Context, a alarm.Alarm) error {
	if s.kv == nil {
		return nil
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("序列化告警失败: %w", err)
	}
	if err := s.kv.Put(ctx, KeyPrefixAlarms+a.AlarmID, raw); err != nil {
		return fmt.Errorf("写穿告警失败: %w", err)
	}
	return nil
}
