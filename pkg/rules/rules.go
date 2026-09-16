// Package rules 实现 EdgeFlow 规则引擎与数据治理的共享逻辑（v0.37.0）。
//
// 职责边界：本包只做"纯逻辑"——规则/治理策略的模型定义与校验、条件
// 评估器（状态机）、治理过滤器（死区/去抖/范围校验）。不感知任何传输、
// 存储或进程结构：云端（校验 + 下发组包）与边缘（实时评估）共用同一
// 份模型与语义，保证"云边规则语义一致"不靠约定靠代码。
//
// 零依赖：仅标准库。
//
// 语义要点（详见 specs/0010-rule-engine/spec.md）：
//   - 条件：threshold（gt/lt/gte/lte）与 range（between/outside）两类，
//     支持 forSeconds 持续时间（满足持续 N 秒才触发）；
//   - 触发：状态机 idle → pending → firing，触发一次，恢复后可再触发；
//   - 治理：range 越界拦截 → debounce 稳定确认 → deadband 微变抑制，
//     拦截值不写影子（抑制上报），坏值不参与规则评估。
package rules

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// ruleIDPattern 是 ruleId 的合法形态：小写字母/数字开头，允许小写字母、
// 数字与连字符，长度 1-63。保持宽松（不做 K8s 风格全校验），仅拒绝明显
// 非法字符与空值。
var ruleIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// reservedRuleIDs 是路由保留段（/api/v1/rules/{ruleID} 与 /api/v1/rules/
// events、/api/v1/rules/governance 的字面段冲突），规则 ID 不得使用。
var reservedRuleIDs = map[string]bool{"governance": true, "events": true}

// 条件类型/操作符/严重级/动作类型的白名单常量。
const (
	ConditionThreshold = "threshold"
	ConditionRange     = "range"

	OpGT      = "gt"
	OpLT      = "lt"
	OpGTE     = "gte"
	OpLTE     = "lte"
	OpBetween = "between"
	OpOutside = "outside"

	ActionEvent = "event"

	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"

	// DefaultNamespace 与设备影子/契约的缺省命名空间一致。
	DefaultNamespace = "default"
	// DefaultSeverity 是 severity 缺省值（与 spec 一致）。
	DefaultSeverity = SeverityWarning
)

// Rule 是一条规则的定义（云端存储与下发、边缘评估共用）。
type Rule struct {
	RuleID     string    `json:"ruleId"`
	Name       string    `json:"name,omitempty"`
	Namespace  string    `json:"namespace,omitempty"` // 缺省 default
	DeviceName string    `json:"deviceName"`
	Property   string    `json:"property"`
	Enabled    *bool     `json:"enabled,omitempty"` // nil = 启用（缺省 true）
	Condition  Condition `json:"condition"`
	Action     Action    `json:"action"`
}

// IsEnabled 返回规则启用状态（缺省 true）。
func (r *Rule) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// Validate 校验规则定义；错误信息带 ruleId 便于定位。
func (r *Rule) Validate() error {
	if r == nil {
		return errors.New("规则为 nil")
	}
	if r.RuleID == "" {
		return errors.New("规则缺少 ruleId")
	}
	if !ruleIDPattern.MatchString(r.RuleID) {
		return fmt.Errorf("规则 %q 的 ruleId 非法（小写字母/数字开头，允许小写字母、数字与连字符，1-63 字符）", r.RuleID)
	}
	if reservedRuleIDs[r.RuleID] {
		return fmt.Errorf("规则 %q 的 ruleId 为路由保留段（governance/events），不允许使用", r.RuleID)
	}
	if r.DeviceName == "" {
		return fmt.Errorf("规则 %q 缺少 deviceName", r.RuleID)
	}
	if r.Property == "" {
		return fmt.Errorf("规则 %q 缺少 property", r.RuleID)
	}
	if err := r.Condition.Validate(); err != nil {
		return fmt.Errorf("规则 %q 条件非法: %w", r.RuleID, err)
	}
	if err := r.Action.Validate(); err != nil {
		return fmt.Errorf("规则 %q 动作非法: %w", r.RuleID, err)
	}
	return nil
}

// EffectiveNamespace 返回归一化命名空间（空 → default）。
func (r *Rule) EffectiveNamespace() string { return normNamespace(r.Namespace) }

// Condition 是规则触发条件（阈值或区间 + 持续时间）。
type Condition struct {
	Type       string  `json:"type"`                 // threshold | range
	Op         string  `json:"op"`                   // gt/lt/gte/lte | between/outside
	Value      float64 `json:"value,omitempty"`      // threshold 阈值
	Min        float64 `json:"min,omitempty"`        // range 下界
	Max        float64 `json:"max,omitempty"`        // range 上界
	ForSeconds int     `json:"forSeconds,omitempty"` // 满足持续秒数（0=立即）
}

// Validate 校验条件（类型与操作符白名单、range 边界、forSeconds 非负）。
func (c *Condition) Validate() error {
	if c.ForSeconds < 0 {
		return fmt.Errorf("forSeconds 不能为负数（%d）", c.ForSeconds)
	}
	switch c.Type {
	case ConditionThreshold:
		switch c.Op {
		case OpGT, OpLT, OpGTE, OpLTE:
		default:
			return fmt.Errorf("threshold 条件的 op 非法（%q，支持 gt/lt/gte/lte）", c.Op)
		}
		if math.IsNaN(c.Value) || math.IsInf(c.Value, 0) {
			return errors.New("threshold 条件的 value 非法（NaN/Inf）")
		}
		return nil
	case ConditionRange:
		switch c.Op {
		case OpBetween, OpOutside:
		default:
			return fmt.Errorf("range 条件的 op 非法（%q，支持 between/outside）", c.Op)
		}
		if math.IsNaN(c.Min) || math.IsNaN(c.Max) || math.IsInf(c.Min, 0) || math.IsInf(c.Max, 0) {
			return errors.New("range 条件的 min/max 非法（NaN/Inf）")
		}
		if !(c.Min < c.Max) {
			return fmt.Errorf("range 条件的 min 必须小于 max（min=%g, max=%g）", c.Min, c.Max)
		}
		return nil
	default:
		return fmt.Errorf("条件类型非法（%q，支持 threshold/range）", c.Type)
	}
}

// Action 是规则触发时执行的动作（v0.37 唯一类型：event）。
type Action struct {
	Type     string `json:"type"`               // event
	Severity string `json:"severity,omitempty"` // info/warning/critical（缺省 warning）
	Message  string `json:"message,omitempty"`  // 模板：${device}/${property}/${value}
}

// Validate 校验动作。
func (a *Action) Validate() error {
	if a.Type != ActionEvent {
		return fmt.Errorf("动作类型非法（%q，当前仅支持 event）", a.Type)
	}
	switch a.Severity {
	case "", SeverityInfo, SeverityWarning, SeverityCritical:
		return nil
	default:
		return fmt.Errorf("severity 非法（%q，支持 info/warning/critical）", a.Severity)
	}
}

// EffectiveSeverity 返回归一化严重级（空 → warning）。
func (a *Action) EffectiveSeverity() string {
	if a.Severity == "" {
		return DefaultSeverity
	}
	return a.Severity
}

// RenderMessage 渲染事件消息模板：替换 ${device}/${property}/${value} 占位符。
// 未知占位符保持原文（不报错）；空模板返回空字符串（调用方决定缺省文案）。
// value 使用 %g 格式化（与日志/台账的可读性口径一致）。
func RenderMessage(tpl, deviceName, property string, value float64) string {
	if tpl == "" {
		return ""
	}
	return strings.NewReplacer(
		"${device}", deviceName,
		"${property}", property,
		"${value}", fmt.Sprintf("%g", value),
	).Replace(tpl)
}

// GovernancePolicy 是一条数据治理策略（作用于单 device×property 的采集值）。
// 三类过滤器可选组合，至少启用一项；未配置策略的属性恒直通（零行为）。
type GovernancePolicy struct {
	DeviceName string  `json:"deviceName"`
	Namespace  string  `json:"namespace,omitempty"` // 缺省 default
	Property   string  `json:"property"`
	Deadband   float64 `json:"deadband,omitempty"` // >0 启用：变化小于该值抑制
	Debounce   int     `json:"debounce,omitempty"` // >=2 启用：新值需稳定 N 次才采纳
	Range      *Bounds `json:"range,omitempty"`    // 越界值拦截（坏值标记）
}

// Bounds 是治理策略的范围校验边界（闭区间）。
type Bounds struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Validate 校验治理策略。
func (p *GovernancePolicy) Validate() error {
	if p.DeviceName == "" {
		return errors.New("治理策略缺少 deviceName")
	}
	if p.Property == "" {
		return errors.New("治理策略缺少 property")
	}
	if math.IsNaN(p.Deadband) || p.Deadband < 0 {
		return fmt.Errorf("治理策略 deadband 非法（%g，须 >=0）", p.Deadband)
	}
	if p.Debounce != 0 && p.Debounce < 2 {
		return fmt.Errorf("治理策略 debounce 非法（%d，须为 0（禁用）或 >=2）", p.Debounce)
	}
	if p.Range != nil {
		if math.IsNaN(p.Range.Min) || math.IsNaN(p.Range.Max) ||
			math.IsInf(p.Range.Min, 0) || math.IsInf(p.Range.Max, 0) {
			return errors.New("治理策略 range 的 min/max 非法（NaN/Inf）")
		}
		if !(p.Range.Min < p.Range.Max) {
			return fmt.Errorf("治理策略 range 的 min 必须小于 max（min=%g, max=%g）", p.Range.Min, p.Range.Max)
		}
	}
	if p.Deadband <= 0 && p.Debounce == 0 && p.Range == nil {
		return errors.New("治理策略未启用任何过滤器（deadband/debounce/range 至少一项）")
	}
	return nil
}

// RuleSet 是一次下发/应用的完整规则包（规则 + 治理策略 + 版本号）。
type RuleSet struct {
	Version    int64              `json:"version"`
	Rules      []Rule             `json:"rules"`
	Governance []GovernancePolicy `json:"governance,omitempty"`
}

// Validate 校验规则包（版本、逐条规则、ruleId 唯一、治理策略唯一键）。
func (rs *RuleSet) Validate() error {
	if rs == nil {
		return errors.New("规则包为 nil")
	}
	if rs.Version < 1 {
		return fmt.Errorf("规则包版本非法（%d，须 >=1）", rs.Version)
	}
	seen := make(map[string]bool, len(rs.Rules))
	for i := range rs.Rules {
		r := &rs.Rules[i]
		if err := r.Validate(); err != nil {
			return err
		}
		if seen[r.RuleID] {
			return fmt.Errorf("规则包中 ruleId 重复：%q", r.RuleID)
		}
		seen[r.RuleID] = true
	}
	gseen := make(map[string]bool, len(rs.Governance))
	for i := range rs.Governance {
		p := &rs.Governance[i]
		if err := p.Validate(); err != nil {
			return err
		}
		k := govKeyOf(p.Namespace, p.DeviceName, p.Property)
		if gseen[k] {
			return fmt.Errorf("规则包中治理策略重复（%s/%s.%s）", normNamespace(p.Namespace), p.DeviceName, p.Property)
		}
		gseen[k] = true
	}
	return nil
}

// Event 是一次规则触发的产出（边缘 → 云端上行；同时落边缘台账）。
type Event struct {
	RuleID         string  `json:"ruleId"`
	RuleName       string  `json:"ruleName,omitempty"`
	DeviceName     string  `json:"deviceName"`
	Namespace      string  `json:"namespace"`
	Property       string  `json:"property"`
	Value          float64 `json:"value"`
	Severity       string  `json:"severity"`
	Message        string  `json:"message"`
	TriggeredAt    int64   `json:"triggeredAt"`    // 触发时间（毫秒）
	RuleSetVersion int64   `json:"ruleSetVersion"` // 触发时生效的规则包版本
}

// normNamespace 把空命名空间归一为缺省值（与 devicetwin/契约一致）。
func normNamespace(ns string) string {
	if ns == "" {
		return DefaultNamespace
	}
	return ns
}

// govKeyOf 构造治理策略的唯一键：namespace/deviceName/property。
func govKeyOf(ns, deviceName, property string) string {
	return normNamespace(ns) + "/" + deviceName + "/" + property
}
