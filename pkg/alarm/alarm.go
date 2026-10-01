// 告警模型（v0.40.0，spec 0013 US-1）：等级/生命周期/去重键与状态机。
//
// 云边共享的告警唯一事实形态：本结构体的 JSON 形态同时是
//   - TypeAlarmEvent 消息负载（边→云）；
//   - 边缘 alarm_ledger 台账行（alarm JSON 列）；
//   - 云端 alarmstore 存储行（etcd 值）。
//
// 生命周期（云侧收/派/闭环）：raised → acked / assigned → closed；
// closed 为终态；状态只进不退（StateRank 守卫乱序/迟到重发——边侧重发
// 仅合并 Count 与时间戳，不回退云端状态）。
// 边侧本版不自动产生 cleared（规则链无 cleared 事件，边界登记 KI §41）。
package alarm

import (
	"errors"
	"fmt"
	"strings"
)

// 严重级（等级）常量。等级语义与 rules.Event.Severity 一致。
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// 生命周期状态常量（云侧收/派/闭环）。
const (
	StateRaised   = "raised"
	StateAcked    = "acked"
	StateAssigned = "assigned"
	StateClosed   = "closed"
)

// 告警来源常量。本版告警产生源为规则触发（source=rule）；
// device/system 为后续版本的视频推理/系统健康源留位。
const (
	SourceRule   = "rule"
	SourceDevice = "device"
	SourceSystem = "system"
)

// StateRank 返回状态秩（只进不退守卫）：raised(0) < acked(1) < assigned(2) < closed(3)。
// 未知状态返回 -1（调用方按非法处理）。
func StateRank(state string) int {
	switch state {
	case StateRaised:
		return 0
	case StateAcked:
		return 1
	case StateAssigned:
		return 2
	case StateClosed:
		return 3
	default:
		return -1
	}
}

// CanTransition 判定 from→to 是否为合法迁移（closed 为终态，不可迁出）。
// 合法迁移：raised→acked、raised→assigned、acked→assigned、（非 closed）→closed。
func CanTransition(from, to string) bool {
	if from == StateClosed {
		return false // closed 终态：不可迁出（含 closed→closed）
	}
	if to == StateClosed {
		return true // 非 closed → closed 一律合法（闭环）
	}
	if StateRank(to) <= StateRank(from) {
		return false // 只进不退（含同状态自迁移拒绝）
	}
	// raised/acked/assigned 之间只允许升秩路径（raised→acked/assigned、acked→assigned）
	return from == StateRaised || from == StateAcked
}

// Alarm 是一条告警（云边三处同构：消息负载 / 边缘台账 / 云端存储）。
type Alarm struct {
	AlarmID    string `json:"alarmId"`              // 告警唯一 ID（边侧 episode 生成，云边一致）
	NodeID     string `json:"nodeId"`               // 产生节点
	Source     string `json:"source"`               // 来源：rule|device|system
	Namespace  string `json:"namespace,omitempty"`  // 命名空间（可空）
	DeviceName string `json:"deviceName,omitempty"` // 设备名（可空）
	RuleID     string `json:"ruleId,omitempty"`     // 规则 ID（source=rule 时非空）
	Severity   string `json:"severity"`             // 等级：critical|warning|info
	State      string `json:"state"`                // 生命周期状态
	Message    string `json:"message,omitempty"`    // 告警描述（最新一次触发渲染）
	Count      int64  `json:"count"`                // 聚合次数（同 episode 触发计数，≥1）
	RaisedAt   int64  `json:"raisedAt"`             // 首次触发时间（毫秒）
	UpdatedAt  int64  `json:"updatedAt"`            // 最近更新时间（毫秒；触发或状态变更）
	AckedBy    string `json:"ackedBy,omitempty"`    // 确认人
	AckedAt    int64  `json:"ackedAt,omitempty"`    // 确认时间（毫秒）
	AssignedTo string `json:"assignedTo,omitempty"` // 派单对象（工单集成点）
	TicketRef  string `json:"ticketRef,omitempty"`  // 工单引用（集成系统回填）
	ClosedBy   string `json:"closedBy,omitempty"`   // 闭环操作人
	ClosedAt   int64  `json:"closedAt,omitempty"`   // 闭环时间（毫秒）
}

// Validate 全量校验（上行接收与云端存储前调用）。
func (a *Alarm) Validate() error {
	if a == nil {
		return errors.New("告警为空（nil）")
	}
	if a.AlarmID == "" {
		return errors.New("告警缺少 alarmId")
	}
	if a.NodeID == "" {
		return errors.New("告警缺少 nodeId")
	}
	switch a.Source {
	case SourceRule, SourceDevice, SourceSystem:
	default:
		return fmt.Errorf("告警 source 非法: %q", a.Source)
	}
	switch a.Severity {
	case SeverityCritical, SeverityWarning, SeverityInfo:
	default:
		return fmt.Errorf("告警 severity 非法: %q", a.Severity)
	}
	if StateRank(a.State) < 0 {
		return fmt.Errorf("告警 state 非法: %q", a.State)
	}
	if a.Count < 1 {
		return errors.New("告警 count 必须 ≥1")
	}
	if a.RaisedAt <= 0 || a.UpdatedAt <= 0 {
		return errors.New("告警 raisedAt/updatedAt 必须为正毫秒时间戳")
	}
	if a.Source == SourceRule && a.RuleID == "" {
		return errors.New("source=rule 的告警缺少 ruleId")
	}
	return nil
}

// DedupKey 返回去重键：node|source|ruleID|namespace|device（空段保留占位）。
// 同一 episode（同节点同来源同规则同设备）的告警聚合为一条（count 递增）。
func (a *Alarm) DedupKey() string {
	return strings.Join([]string{a.NodeID, a.Source, a.RuleID, a.Namespace, a.DeviceName}, "|")
}

// Clone 返回深拷贝（调用方修改副本不影响原值；存储/回调边界用）。
func (a *Alarm) Clone() Alarm {
	if a == nil {
		return Alarm{}
	}
	return *a
}
