// 设定值通道存储（v0.40.0，spec 0013 US-4/US-5）：云端建单、审批与执行反馈状态机。
//
// 存储选型（与 alarmstore 同构）：etcd 写穿（键 /edgeflow/setpoints/<setpointID>，
// 值为 Setpoint JSON）+ 内存索引；kv 为 nil 时纯内存（测试/内嵌形态）。
//
// 状态机：pending-approval →（approve）pending-send →（ReliableSend 成功）sent
// →（SetpointResult）applied | failed；pending-approval →（reject）rejected（终态）；
// pending-send/sent →（Ack error/边缘拒绝）failed；applied/failed/rejected 终态。
// 断网语义：pending-send 由 flush 循环重投（同 MsgID 幂等），节点恢复后自动同步。
package setpointstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
	"edgeflow/pkg/log"
)

// KeyPrefixSetpoints 是设定值存储的 etcd 键前缀。
const KeyPrefixSetpoints = "/edgeflow/setpoints/"

// 生命周期状态常量。
const (
	StatePendingApproval = "pending-approval"
	StatePendingSend     = "pending-send"
	StateSent            = "sent"
	StateApplied         = "applied"
	StateFailed          = "failed"
	StateRejected        = "rejected"
)

// 业务错误（API 层映射：ErrNotFound→404、ErrInvalidState→409、ErrValidation→400）。
var (
	ErrNotFound     = errors.New("setpointstore: 设定值建单不存在")
	ErrInvalidState = errors.New("setpointstore: 设定值状态迁移非法")
	ErrValidation   = errors.New("setpointstore: 设定值参数校验失败")
)

// Setpoint 是一条设定值建单（云端事实记录，含执行反馈）。
type Setpoint struct {
	SetpointID      string  `json:"setpointId"`          // 建单唯一 ID（服务端生成）
	NodeID          string  `json:"nodeId"`              // 目标节点
	Namespace       string  `json:"namespace"`           // 命名空间
	DeviceName      string  `json:"deviceName"`          // 目标设备
	Property        string  `json:"property"`            // 目标属性
	Value           float64 `json:"value"`               // 设定值
	State           string  `json:"state"`               // 生命周期状态
	Operator        string  `json:"operator"`            // 创建人（审计留痕）
	RequireApproval bool    `json:"requireApproval"`     // 是否要求审批（建单时决定）
	MsgID           string  `json:"msgId"`               // 首次投递消息 ID（可靠投递重试同 ID 幂等；重投派生新 ID，见 Attempts）
	Attempts        int     `json:"attempts"`            // 重投次数（0=首次；第 n 次重投投递 ID = MsgID + "-r" + n）
	Error           string  `json:"error,omitempty"`     // 失败原因（failed 时）
	CreatedAt       int64   `json:"createdAt"`           // 建单时间（毫秒）
	UpdatedAt       int64   `json:"updatedAt"`           // 最近更新时间（毫秒）
	SentAt          int64   `json:"sentAt,omitempty"`    // 首次投递成功时间（毫秒）
	OutcomeAt       int64   `json:"outcomeAt,omitempty"` // 终态时间（毫秒）
}

// Store 是云端设定值存储（内存索引 + etcd 写穿）。
type Store struct {
	mu  sync.Mutex
	kv  etcdstore.KVStore // nil = 纯内存
	sps map[string]*Setpoint
}

// NewStore 创建设定值存储；kv 允许 nil（纯内存）。
func NewStore(kv etcdstore.KVStore) *Store {
	return &Store{kv: kv, sps: make(map[string]*Setpoint)}
}

// Load 启动恢复（损坏条目跳过）。
func (s *Store) Load(ctx context.Context) error {
	if s.kv == nil {
		return nil
	}
	entries, err := s.kv.ListByPrefix(ctx, KeyPrefixSetpoints)
	if err != nil {
		return fmt.Errorf("扫描设定值存储失败: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	restored := 0
	for _, e := range entries {
		var sp Setpoint
		if err := json.Unmarshal(e.Value, &sp); err != nil || sp.SetpointID == "" {
			log.Warnf("跳过损坏的设定值条目（key=%s）", e.Key)
			continue
		}
		cp := sp
		s.sps[sp.SetpointID] = &cp
		restored++
	}
	log.Infof("设定值存储已加载：建单 %d 条", restored)
	return nil
}

// CreateInput 是 Create 的入参。
type CreateInput struct {
	NodeID          string
	Namespace       string
	DeviceName      string
	Property        string
	Value           float64
	Operator        string
	RequireApproval bool
	ApprovalForced  bool // 审批开关（EDGEFLOW_CLOUDCORE_SETPOINT_APPROVAL=on）时为 true
	NowMs           int64
}

// Create 建单：requireApproval 或审批开关 on → pending-approval；否则 pending-send。
func (s *Store) Create(ctx context.Context, in CreateInput) (Setpoint, error) {
	if in.NodeID == "" || in.DeviceName == "" || in.Property == "" {
		return Setpoint{}, fmt.Errorf("%w: 缺少 nodeId/deviceName/property", ErrValidation)
	}
	if in.Operator == "" {
		return Setpoint{}, fmt.Errorf("%w: 缺少 operator（审计留痕必填）", ErrValidation)
	}
	if in.NowMs == 0 {
		in.NowMs = time.Now().UnixMilli()
	}
	sp := Setpoint{
		SetpointID:      newSetpointID(in.NowMs),
		NodeID:          in.NodeID,
		Namespace:       in.Namespace,
		DeviceName:      in.DeviceName,
		Property:        in.Property,
		Value:           in.Value,
		Operator:        in.Operator,
		RequireApproval: in.RequireApproval || in.ApprovalForced,
		MsgID:           "",
		CreatedAt:       in.NowMs,
		UpdatedAt:       in.NowMs,
	}
	sp.MsgID = "spmsg-" + sp.SetpointID // 重发同 ID 幂等（建单即定，持久化）
	if sp.RequireApproval {
		sp.State = StatePendingApproval
	} else {
		sp.State = StatePendingSend
	}
	if err := s.putKV(ctx, sp); err != nil {
		return Setpoint{}, err
	}
	s.mu.Lock()
	s.sps[sp.SetpointID] = &sp
	s.mu.Unlock()
	return sp, nil
}

// Approve 审批通过（仅 pending-approval → pending-send）。
func (s *Store) Approve(ctx context.Context, setpointID, operator string, nowMs int64) (Setpoint, error) {
	return s.transition(ctx, setpointID, StatePendingSend, nowMs, func(sp *Setpoint) {
		_ = operator // 审批操作人记审计台账（API 层）
	})
}

// Reject 审批拒绝（仅 pending-approval → rejected 终态）。
func (s *Store) Reject(ctx context.Context, setpointID, operator string, nowMs int64) (Setpoint, error) {
	return s.transition(ctx, setpointID, StateRejected, nowMs, func(sp *Setpoint) {})
}

// MarkSent 投递成功（仅 pending-send → sent；SentAt 首次生效）。
func (s *Store) MarkSent(ctx context.Context, setpointID string, nowMs int64) (Setpoint, error) {
	return s.transitionForced(ctx, setpointID, StateSent, nowMs, func(sp *Setpoint) {
		if sp.SentAt == 0 {
			sp.SentAt = nowMs
		}
	}, StatePendingSend)
}

// MarkFailed 投递/执行被拒（pending-send/sent → failed 终态，记录原因）。
func (s *Store) MarkFailed(ctx context.Context, setpointID, reason string, nowMs int64) (Setpoint, error) {
	return s.transitionForced(ctx, setpointID, StateFailed, nowMs, func(sp *Setpoint) {
		sp.Error = reason
	}, StatePendingSend, StateSent)
}

// ApplyResult 执行反馈闭环（SetpointResult 回告）：sent/pending-send → applied | failed。
// 未知 setpointID 返回 ErrNotFound（调用方计数忽略——边侧缓存重发可能早于建单可见）。
func (s *Store) ApplyResult(ctx context.Context, setpointID string, ok bool, errMsg string, nowMs int64) (Setpoint, error) {
	to := StateFailed
	if ok {
		to = StateApplied
	}
	return s.transitionForced(ctx, setpointID, to, nowMs, func(sp *Setpoint) {
		if !ok {
			sp.Error = errMsg
		}
	}, StatePendingSend, StateSent)
}

// transition 状态迁移（仅允许 pending-approval 起点或闭环路径——按 to 判定）。
func (s *Store) transition(ctx context.Context, setpointID, to string, nowMs int64, decorate func(*Setpoint)) (Setpoint, error) {
	return s.transitionForced(ctx, setpointID, to, nowMs, decorate, StatePendingApproval)
}

// transitionForced 状态迁移（fromStates 列出合法起点；写穿成功才更新内存）。
func (s *Store) transitionForced(ctx context.Context, setpointID, to string, nowMs int64, decorate func(*Setpoint), fromStates ...string) (Setpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.sps[setpointID]
	if !ok {
		return Setpoint{}, ErrNotFound
	}
	allowed := false
	for _, st := range fromStates {
		if existing.State == st {
			allowed = true
			break
		}
	}
	if !allowed || isSetpointTerminal(existing.State) {
		return existing.Clone(), ErrInvalidState
	}
	updated := existing.Clone()
	updated.State = to
	decorate(&updated)
	updated.UpdatedAt = nowMs
	if isSetpointTerminal(to) {
		updated.OutcomeAt = nowMs
	}
	if err := s.putKV(ctx, updated); err != nil {
		return Setpoint{}, err
	}
	s.sps[setpointID] = &updated
	return updated.Clone(), nil
}

// isSetpointTerminal 判定终态（applied/failed/rejected）。
func isSetpointTerminal(state string) bool {
	return state == StateApplied || state == StateFailed || state == StateRejected
}

// List 按条件查询（CreatedAt 降序；limit<=0 默认 200）。
func (s *Store) List(nodeID, state string, limit int) []Setpoint {
	if limit <= 0 {
		limit = 200
	}
	s.mu.Lock()
	out := make([]Setpoint, 0, len(s.sps))
	for _, sp := range s.sps {
		if nodeID != "" && sp.NodeID != nodeID {
			continue
		}
		if state != "" && sp.State != state {
			continue
		}
		out = append(out, sp.Clone())
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].SetpointID < out[j].SetpointID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// PendingForFlush 返回待重投建单（pending-send，按 CreatedAt 升序——先到先投）。
func (s *Store) PendingForFlush() []Setpoint {
	s.mu.Lock()
	out := make([]Setpoint, 0, len(s.sps))
	for _, sp := range s.sps {
		if sp.State == StatePendingSend {
			out = append(out, sp.Clone())
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// RedeliveryMsgID 返回下一次重投应使用的消息 ID（MsgID + "-r" + 次数+1）：
// 重投必须恒用新 ID——首次投递的原 MsgID 已在边缘 MsgID 持久去重表内，
// 同 ID 重投会被静默短路（不重执行不出回告）；新 ID 使边缘重复应用
// （幂等）并重新回告 → 云端 applied 闭环（spec 0013 US-6 修订）。
func (sp *Setpoint) RedeliveryMsgID() string {
	return fmt.Sprintf("%s-r%d", sp.MsgID, sp.Attempts+1)
}

// SentForRedelivery 返回「已投递但回告超时」的建单（state=sent 且
// nowMs-SentAt > redeliverMs），按 SentAt 升序。回告丢失（边缘崩溃于
// 应用与入队之间 / UPLINK off 直发丢失）的建单经此进入重投闭环：
// 重投使用新投递 ID，边缘重复应用幂等 + 重新回告 → 云端 applied 闭环。
func (s *Store) SentForRedelivery(redeliverMs int64, nowMs int64) []Setpoint {
	s.mu.Lock()
	out := make([]Setpoint, 0, len(s.sps))
	for _, sp := range s.sps {
		if sp.State == StateSent && nowMs-sp.SentAt > redeliverMs {
			out = append(out, sp.Clone())
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].SentAt < out[j].SentAt })
	return out
}

// MarkRedelivered 记一次重投（Attempts++；state 保持 sent；供下次派生新 ID）。
func (s *Store) MarkRedelivered(ctx context.Context, setpointID string, nowMs int64) (Setpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.sps[setpointID]
	if !ok {
		return Setpoint{}, ErrNotFound
	}
	if existing.State != StateSent {
		return existing.Clone(), ErrInvalidState
	}
	updated := existing.Clone()
	updated.Attempts++
	updated.UpdatedAt = nowMs
	if err := s.putKV(ctx, updated); err != nil {
		return Setpoint{}, err
	}
	s.sps[setpointID] = &updated
	return updated.Clone(), nil
}

// Get 单条查询。
func (s *Store) Get(setpointID string) (Setpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.sps[setpointID]
	if !ok {
		return Setpoint{}, ErrNotFound
	}
	return sp.Clone(), nil
}

// Count 返回建单总数（测试与诊断用）。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sps)
}

// Clone 返回深拷贝（调用方修改副本不影响原值；存储/回调边界用）。
func (sp *Setpoint) Clone() Setpoint {
	if sp == nil {
		return Setpoint{}
	}
	return *sp
}

// newSetpointID 生成建单 ID（sp-<毫秒>-<rand4>）。
func newSetpointID(nowMs int64) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sp-%d", nowMs)
	}
	return fmt.Sprintf("sp-%d-%s", nowMs, hex.EncodeToString(b[:]))
}

// putKV etcd 写穿（kv=nil 跳过；调用方持锁）。
func (s *Store) putKV(ctx context.Context, sp Setpoint) error {
	if s.kv == nil {
		return nil
	}
	raw, err := json.Marshal(sp)
	if err != nil {
		return fmt.Errorf("序列化设定值失败: %w", err)
	}
	if err := s.kv.Put(ctx, KeyPrefixSetpoints+sp.SetpointID, raw); err != nil {
		return fmt.Errorf("写穿设定值失败: %w", err)
	}
	return nil
}
