// 规则评估器（v0.37.0）：条件状态机与触发语义。
//
// 语义（与 spec 0010 US-2 一致）：
//   - 每 rule × 目标设备属性 维护独立状态机：idle → pending → firing；
//   - forSeconds=0：首次满足即触发；>0：满足持续 N 秒才触发（期间任一
//     采样不满足 → 回 idle 重新计时）；
//   - firing 后不重复触发；采样变为不满足 → 回 idle（可再次触发）；
//   - 规则 Enabled=false 不参与评估；NaN 输入视为不满足（防御）；
//   - ApplyRuleSet 替换规则集并重置全部评估状态（新版本从零开始收敛）。
//
// 并发安全：所有公开方法内部加锁（评估在采集/上报 goroutine，
// 规则包应用在消息处理 goroutine，两者可并发）。
package rules

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

// ruleState 是单条规则 × 目标设备属性的状态机。
type ruleState struct {
	pendingSince int64 // 首次满足的时间（毫秒）；0 = 未在 pending
	firing       bool  // 已触发（防重复触发，直至恢复）
}

// Evaluator 是规则评估器：持有当前生效规则集，对采样序列求值并产出事件。
// 零值不可用，须经 NewEvaluator 构造。
type Evaluator struct {
	mu      sync.Mutex
	version int64
	rules   []Rule
	states  map[string]*ruleState

	evaluated int64 // 规则-采样评估计数
	triggered int64 // 触发事件计数
}

// NewEvaluator 创建空评估器（无规则 → Observe 零开销直返）。
func NewEvaluator() *Evaluator {
	return &Evaluator{states: make(map[string]*ruleState)}
}

// ApplyRuleSet 校验并替换规则集，重置全部评估状态与版本号。
// 校验失败不改变现状（返回 error，调用方按处理失败路径回 Ack）。
func (e *Evaluator) ApplyRuleSet(rs *RuleSet) error {
	if rs == nil {
		return errors.New("规则包为 nil")
	}
	if err := rs.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.version = rs.Version
	e.rules = append([]Rule(nil), rs.Rules...)
	e.states = make(map[string]*ruleState)
	return nil
}

// Version 返回当前生效规则包版本（0 = 未应用任何规则包）。
func (e *Evaluator) Version() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.version
}

// RuleCount 返回当前生效的规则条数（含未启用规则）。
func (e *Evaluator) RuleCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.rules)
}

// Stats 返回评估计数（evaluated=规则-采样评估次数，triggered=触发次数）。
func (e *Evaluator) Stats() (evaluated, triggered int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.evaluated, e.triggered
}

// Observe 对一次采样求值（deviceName/property/value/ts 毫秒），返回本轮
// 触发的事件列表（无触发时为空切片）。
//
// 调用方约定（samplePipeline）：调控"有效值"——治理越界拦截的坏值
// 不调用本方法（坏值不改变规则状态）；死区/去抖拦截时以治理器的
// 有效值（lastAccepted）调用（时间推进不停顿）。
func (e *Evaluator) Observe(deviceName, namespace, property string, value float64, ts int64) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.rules) == 0 {
		return nil
	}
	ns := normNamespace(namespace)
	var events []Event
	for i := range e.rules {
		r := &e.rules[i]
		if !r.IsEnabled() || r.DeviceName != deviceName ||
			r.Property != property || r.EffectiveNamespace() != ns {
			continue
		}
		e.evaluated++
		key := r.RuleID + "\x00" + ns + "\x00" + deviceName
		st := e.states[key]
		if st == nil {
			st = &ruleState{}
			e.states[key] = st
		}
		satisfied := evalCondition(&r.Condition, value)
		if !satisfied {
			// 不满足：回 idle（若曾 pending/firing 则重置；firing 后
			// 第一次不满足即恢复，可再次触发）
			st.pendingSince = 0
			st.firing = false
			continue
		}
		if st.firing {
			continue // 已触发未恢复：不重复触发
		}
		if r.Condition.ForSeconds <= 0 {
			events = append(events, e.fire(r, ns, deviceName, value, ts))
			st.firing = true
			continue
		}
		if st.pendingSince == 0 {
			st.pendingSince = ts // 首次满足：进入 pending
			continue
		}
		if ts-st.pendingSince >= int64(r.Condition.ForSeconds)*1000 {
			events = append(events, e.fire(r, ns, deviceName, value, ts))
			st.firing = true
		}
	}
	return events
}

// fire 构造一条触发事件（调用方须持锁——读 e.version 与 triggered 计数）。
func (e *Evaluator) fire(r *Rule, ns, deviceName string, value float64, ts int64) Event {
	e.triggered++
	msg := RenderMessage(r.Action.Message, deviceName, r.Property, value)
	if msg == "" {
		msg = fmt.Sprintf("规则 %s 触发：%s.%s=%g", r.RuleID, deviceName, r.Property, value)
	}
	return Event{
		RuleID:         r.RuleID,
		RuleName:       r.Name,
		DeviceName:     deviceName,
		Namespace:      ns,
		Property:       r.Property,
		Value:          value,
		Severity:       r.Action.EffectiveSeverity(),
		Message:        msg,
		TriggeredAt:    ts,
		RuleSetVersion: e.version,
	}
}

// evalCondition 求值单条条件（NaN → 不满足；数值边界按闭/开区间语义）。
//
// 语义细节：
//   - gt/lt 为严格比较；gte/lte 含边界；
//   - between 含边界 [min,max]；outside 为 <min 或 >max（不含边界）。
func evalCondition(c *Condition, value float64) bool {
	if math.IsNaN(value) {
		return false
	}
	switch c.Type {
	case ConditionThreshold:
		switch c.Op {
		case OpGT:
			return value > c.Value
		case OpLT:
			return value < c.Value
		case OpGTE:
			return value >= c.Value
		case OpLTE:
			return value <= c.Value
		}
	case ConditionRange:
		switch c.Op {
		case OpBetween:
			return value >= c.Min && value <= c.Max
		case OpOutside:
			return value < c.Min || value > c.Max
		}
	}
	return false // 非法条件（Validate 已拦截；防御性返回不满足）
}
