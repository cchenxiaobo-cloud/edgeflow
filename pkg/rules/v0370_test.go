// v0.37.0 测试锚：规则模型与校验（US-1）、评估器状态机（US-2）、
// 治理过滤器（US-3）。本文件随 spec 0010 落地，覆盖边界与组合语义。
package rules

import (
	"math"
	"strings"
	"testing"
)

// boolPtr 构造 *bool（Rule.Enabled 的三态：nil=缺省启用）。
func boolPtr(b bool) *bool { return &b }

// baseRule 构造一条合法基础规则（测试夹具）。
func baseRule(id, device, property string) Rule {
	return Rule{
		RuleID:     id,
		DeviceName: device,
		Property:   property,
		Condition:  Condition{Type: ConditionThreshold, Op: OpGT, Value: 80},
		Action:     Action{Type: ActionEvent, Severity: SeverityWarning},
	}
}

// ─────────────────────────── US-1 模型与校验 ───────────────────────────

func TestRuleValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Rule)
		wantErr string // 子串；空 = 期望通过
	}{
		{"基础合法", func(r *Rule) {}, ""},
		{"单字符ID合法", func(r *Rule) { r.RuleID = "a" }, ""},
		{"63字符ID合法", func(r *Rule) { r.RuleID = strings.Repeat("a", 63) }, ""},
		{"缺ruleId", func(r *Rule) { r.RuleID = "" }, "缺少 ruleId"},
		{"大写ID非法", func(r *Rule) { r.RuleID = "Rule-1" }, "ruleId 非法"},
		{"下划线ID非法", func(r *Rule) { r.RuleID = "rule_1" }, "ruleId 非法"},
		{"64字符ID非法", func(r *Rule) { r.RuleID = strings.Repeat("a", 64) }, "ruleId 非法"},
		{"保留字governance", func(r *Rule) { r.RuleID = "governance" }, "保留段"},
		{"保留字events", func(r *Rule) { r.RuleID = "events" }, "保留段"},
		{"缺deviceName", func(r *Rule) { r.DeviceName = "" }, "缺少 deviceName"},
		{"缺property", func(r *Rule) { r.Property = "" }, "缺少 property"},
		{"条件非法", func(r *Rule) { r.Condition.Op = "eq" }, "条件非法"},
		{"动作非法", func(r *Rule) { r.Action.Type = "command" }, "动作非法"},
		{"禁用合法", func(r *Rule) { r.Enabled = boolPtr(false) }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := baseRule("rule-1", "sensor-01", "temperature")
			tc.mutate(&r)
			err := r.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过，得到错误: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("期望错误包含 %q，得到: %v", tc.wantErr, err)
			}
		})
	}
}

func TestConditionValidate(t *testing.T) {
	cases := []struct {
		name    string
		cond    Condition
		wantErr string
	}{
		{"gt合法", Condition{Type: ConditionThreshold, Op: OpGT, Value: 1}, ""},
		{"lt合法", Condition{Type: ConditionThreshold, Op: OpLT, Value: 1}, ""},
		{"gte合法", Condition{Type: ConditionThreshold, Op: OpGTE}, ""},
		{"lte合法", Condition{Type: ConditionThreshold, Op: OpLTE}, ""},
		{"between合法", Condition{Type: ConditionRange, Op: OpBetween, Min: 1, Max: 2}, ""},
		{"outside合法", Condition{Type: ConditionRange, Op: OpOutside, Min: 1, Max: 2}, ""},
		{"forSeconds=0合法", Condition{Type: ConditionThreshold, Op: OpGT, ForSeconds: 0}, ""},
		{"forSeconds负数非法", Condition{Type: ConditionThreshold, Op: OpGT, ForSeconds: -1}, "forSeconds"},
		{"threshold op空非法", Condition{Type: ConditionThreshold}, "op 非法"},
		{"threshold op=between非法", Condition{Type: ConditionThreshold, Op: OpBetween}, "op 非法"},
		{"range op=gt非法", Condition{Type: ConditionRange, Op: OpGT, Min: 1, Max: 2}, "op 非法"},
		{"range min==max非法", Condition{Type: ConditionRange, Op: OpBetween, Min: 2, Max: 2}, "min 必须小于 max"},
		{"range min>max非法", Condition{Type: ConditionRange, Op: OpBetween, Min: 3, Max: 2}, "min 必须小于 max"},
		{"类型空非法", Condition{}, "条件类型非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cond.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过，得到错误: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("期望错误包含 %q，得到: %v", tc.wantErr, err)
			}
		})
	}
}

func TestActionValidate(t *testing.T) {
	cases := []struct {
		severity string
		wantErr  bool
	}{
		{"", false}, // 缺省（归一为 warning）
		{SeverityInfo, false},
		{SeverityWarning, false},
		{SeverityCritical, false},
		{"fatal", true},
	}
	for _, tc := range cases {
		a := Action{Type: ActionEvent, Severity: tc.severity}
		err := a.Validate()
		if tc.wantErr != (err != nil) {
			t.Fatalf("severity=%q 期望错误=%v，得到 %v", tc.severity, tc.wantErr, err)
		}
	}
	if got := (&Action{Type: ActionEvent}).EffectiveSeverity(); got != SeverityWarning {
		t.Fatalf("缺省 severity 应为 warning，得到 %q", got)
	}
	if got := (&Action{Type: ActionEvent, Severity: SeverityCritical}).EffectiveSeverity(); got != SeverityCritical {
		t.Fatalf("显式 severity 应保留，得到 %q", got)
	}
}

func TestGovernancePolicyValidate(t *testing.T) {
	cases := []struct {
		name    string
		pol     GovernancePolicy
		wantErr string
	}{
		{"deadband合法", GovernancePolicy{DeviceName: "d", Property: "p", Deadband: 0.5}, ""},
		{"debounce=3合法", GovernancePolicy{DeviceName: "d", Property: "p", Debounce: 3}, ""},
		{"range合法", GovernancePolicy{DeviceName: "d", Property: "p", Range: &Bounds{Min: 0, Max: 100}}, ""},
		{"组合合法", GovernancePolicy{DeviceName: "d", Property: "p", Deadband: 1, Debounce: 2, Range: &Bounds{Min: 0, Max: 100}}, ""},
		{"缺deviceName", GovernancePolicy{Property: "p", Deadband: 1}, "缺少 deviceName"},
		{"缺property", GovernancePolicy{DeviceName: "d", Deadband: 1}, "缺少 property"},
		{"全禁用非法", GovernancePolicy{DeviceName: "d", Property: "p"}, "未启用任何过滤器"},
		{"debounce=1非法", GovernancePolicy{DeviceName: "d", Property: "p", Debounce: 1}, "debounce 非法"},
		{"deadband负数非法", GovernancePolicy{DeviceName: "d", Property: "p", Deadband: -1}, "deadband 非法"},
		{"range边界非法", GovernancePolicy{DeviceName: "d", Property: "p", Range: &Bounds{Min: 5, Max: 5}}, "min 必须小于 max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.pol.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过，得到错误: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("期望错误包含 %q，得到: %v", tc.wantErr, err)
			}
		})
	}
}

func TestRuleSetValidate(t *testing.T) {
	ok := RuleSet{Version: 1, Rules: []Rule{baseRule("r1", "d", "p")}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("基础规则包应通过: %v", err)
	}
	if err := (&RuleSet{Version: 0}).Validate(); err == nil || !strings.Contains(err.Error(), "版本非法") {
		t.Fatalf("version=0 应非法，得到: %v", err)
	}
	dup := RuleSet{Version: 1, Rules: []Rule{baseRule("r1", "d", "p"), baseRule("r1", "d2", "p2")}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "ruleId 重复") {
		t.Fatalf("重复 ruleId 应非法，得到: %v", err)
	}
	dupGov := RuleSet{Version: 1, Governance: []GovernancePolicy{
		{DeviceName: "d", Property: "p", Deadband: 1},
		{DeviceName: "d", Property: "p", Deadband: 2},
	}}
	if err := dupGov.Validate(); err == nil || !strings.Contains(err.Error(), "治理策略重复") {
		t.Fatalf("重复治理策略应非法，得到: %v", err)
	}
	// 空规则 + 空治理 + 合法版本 = 合法（清空操作）
	if err := (&RuleSet{Version: 2}).Validate(); err != nil {
		t.Fatalf("空规则包应合法: %v", err)
	}
}

func TestRenderMessage(t *testing.T) {
	got := RenderMessage("设备 ${device} 的 ${property} 达 ${value}", "sensor-01", "temperature", 88.5)
	if got != "设备 sensor-01 的 temperature 达 88.5" {
		t.Fatalf("模板渲染不符: %q", got)
	}
	// 未知占位符保留
	if got := RenderMessage("a ${foo} b", "d", "p", 1); got != "a ${foo} b" {
		t.Fatalf("未知占位符应保留: %q", got)
	}
	// 空模板
	if got := RenderMessage("", "d", "p", 1); got != "" {
		t.Fatalf("空模板应返回空: %q", got)
	}
}

// ─────────────────────────── US-2 评估器 ───────────────────────────

// evalWith 构造带单条规则的评估器（助手）。
func evalWith(t *testing.T, r Rule) *Evaluator {
	t.Helper()
	e := NewEvaluator()
	if err := e.ApplyRuleSet(&RuleSet{Version: 1, Rules: []Rule{r}}); err != nil {
		t.Fatalf("应用规则包失败: %v", err)
	}
	return e
}

func TestEvaluatorThresholdOps(t *testing.T) {
	cases := []struct {
		op      string
		value   float64
		trigger float64 // 该采样应触发
		quiet   float64 // 该采样不应触发
	}{
		{OpGT, 80, 80.1, 80},
		{OpLT, 20, 19.9, 20},
		{OpGTE, 80, 80, 79.9},
		{OpLTE, 20, 20, 20.1},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			r := baseRule("r1", "sensor-01", "temperature")
			r.Condition = Condition{Type: ConditionThreshold, Op: tc.op, Value: tc.value}
			e := evalWith(t, r)
			if evs := e.Observe("sensor-01", "default", "temperature", tc.quiet, 1000); len(evs) != 0 {
				t.Fatalf("quiet 值不应触发: %+v", evs)
			}
			evs := e.Observe("sensor-01", "default", "temperature", tc.trigger, 2000)
			if len(evs) != 1 {
				t.Fatalf("trigger 值应触发一条事件，得到 %d", len(evs))
			}
			ev := evs[0]
			if ev.RuleID != "r1" || ev.DeviceName != "sensor-01" || ev.Property != "temperature" ||
				ev.Value != tc.trigger || ev.Severity != SeverityWarning || ev.TriggeredAt != 2000 || ev.RuleSetVersion != 1 {
				t.Fatalf("事件字段不符: %+v", ev)
			}
			if !strings.Contains(ev.Message, "sensor-01") {
				t.Fatalf("缺省消息应含设备名: %q", ev.Message)
			}
		})
	}
}

func TestEvaluatorRangeOps(t *testing.T) {
	r := baseRule("r1", "d1", "t")
	r.Condition = Condition{Type: ConditionRange, Op: OpBetween, Min: 10, Max: 20}
	e := evalWith(t, r)
	if evs := e.Observe("d1", "", "t", 9.9, 1); len(evs) != 0 {
		t.Fatalf("below 不应触发: %+v", evs)
	}
	if evs := e.Observe("d1", "", "t", 25, 2); len(evs) != 0 {
		t.Fatalf("above 不应触发: %+v", evs)
	}
	if evs := e.Observe("d1", "", "t", 15, 3); len(evs) != 1 {
		t.Fatalf("between 应触发: %+v", evs)
	}
	// between 含边界
	r2 := baseRule("r2", "d1", "t")
	r2.Condition = Condition{Type: ConditionRange, Op: OpBetween, Min: 10, Max: 20}
	e2 := evalWith(t, r2)
	if evs := e2.Observe("d1", "", "t", 10, 1); len(evs) != 1 {
		t.Fatalf("between 下边界应触发: %+v", evs)
	}
	// outside：边界外触发、边界上不触发
	r3 := baseRule("r3", "d1", "t")
	r3.Condition = Condition{Type: ConditionRange, Op: OpOutside, Min: 10, Max: 20}
	e3 := evalWith(t, r3)
	if evs := e3.Observe("d1", "", "t", 10, 1); len(evs) != 0 {
		t.Fatalf("outside 边界上不应触发: %+v", evs)
	}
	if evs := e3.Observe("d1", "", "t", 9.99, 2); len(evs) != 1 {
		t.Fatalf("outside 边界外应触发: %+v", evs)
	}
}

func TestEvaluatorForSeconds(t *testing.T) {
	r := baseRule("r1", "d1", "t")
	r.Condition = Condition{Type: ConditionThreshold, Op: OpGT, Value: 10, ForSeconds: 10}
	e := evalWith(t, r)
	// t=1000 首次满足 → pending（不触发）
	if evs := e.Observe("d1", "", "t", 11, 1000); len(evs) != 0 {
		t.Fatalf("pending 期不应触发: %+v", evs)
	}
	// t=6000 仍满足（差 5s < 10s）→ 不触发
	if evs := e.Observe("d1", "", "t", 12, 6000); len(evs) != 0 {
		t.Fatalf("未达持续期不应触发: %+v", evs)
	}
	// t=11000 满足（差 10s，恰好达标，>= 语义）→ 触发
	if evs := e.Observe("d1", "", "t", 13, 11000); len(evs) != 1 {
		t.Fatalf("达到持续期应触发: %+v", evs)
	}
	// 中断重置：新引擎，满足 → 不满足 → 再满足（重新计时）
	e2 := evalWith(t, r)
	e2.Observe("d1", "", "t", 11, 1000) // pending 起点 1000
	e2.Observe("d1", "", "t", 5, 5000)  // 不满足 → 回 idle
	if evs := e2.Observe("d1", "", "t", 11, 12000); len(evs) != 0 {
		t.Fatalf("中断后重新 pending，不应立即触发: %+v", evs)
	}
	if evs := e2.Observe("d1", "", "t", 11, 22000); len(evs) != 1 {
		t.Fatalf("重新累计达 10s 应触发: %+v", evs)
	}
}

func TestEvaluatorFiringOnceAndRecover(t *testing.T) {
	r := baseRule("r1", "d1", "t")
	e := evalWith(t, r)
	if evs := e.Observe("d1", "", "t", 81, 1); len(evs) != 1 {
		t.Fatalf("首满足应触发: %+v", evs)
	}
	// firing 中连续满足：不重复触发
	if evs := e.Observe("d1", "", "t", 82, 2); len(evs) != 0 {
		t.Fatalf("firing 中不应重复触发: %+v", evs)
	}
	if evs := e.Observe("d1", "", "t", 90, 3); len(evs) != 0 {
		t.Fatalf("firing 中不应重复触发: %+v", evs)
	}
	// 恢复（不满足）→ 再次满足：可再触发
	if evs := e.Observe("d1", "", "t", 50, 4); len(evs) != 0 {
		t.Fatalf("恢复不应产生事件: %+v", evs)
	}
	if evs := e.Observe("d1", "", "t", 81, 5); len(evs) != 1 {
		t.Fatalf("恢复后再次满足应触发: %+v", evs)
	}
	ev, tr := e.Stats()
	if ev != 5 || tr != 2 {
		t.Fatalf("统计不符: evaluated=%d triggered=%d（期望 5/2）", ev, tr)
	}
}

func TestEvaluatorDisabledAndMismatch(t *testing.T) {
	// 禁用规则：不评估、不触发
	r := baseRule("r1", "d1", "t")
	r.Enabled = boolPtr(false)
	e := evalWith(t, r)
	if evs := e.Observe("d1", "", "t", 999, 1); len(evs) != 0 {
		t.Fatalf("禁用规则不应触发: %+v", evs)
	}
	if ev, _ := e.Stats(); ev != 0 {
		t.Fatalf("禁用规则不应计入评估: %d", ev)
	}
	// 设备/属性/命名空间不匹配：不评估
	r2 := baseRule("r2", "d1", "t")
	r2.Namespace = "plant-a"
	e2 := evalWith(t, r2)
	e2.Observe("d2", "", "t", 999, 1)
	e2.Observe("d1", "", "other", 999, 2)
	e2.Observe("d1", "default", "t", 999, 3)
	if ev, _ := e2.Stats(); ev != 0 {
		t.Fatalf("不匹配采样不应计入评估: %d", ev)
	}
	// ns 归一：规则缺省 ns 匹配 Observe 的 ""/"default"
	r3 := baseRule("r3", "d1", "t")
	e3 := evalWith(t, r3)
	if evs := e3.Observe("d1", "", "t", 81, 1); len(evs) != 1 {
		t.Fatalf("空 ns 应归一匹配: %+v", evs)
	}
	if evs := e3.Observe("d1", "default", "t", 82, 2); len(evs) != 0 {
		t.Fatalf("firing 中不重复: %+v", evs)
	}
}

func TestEvaluatorApplyRuleSet(t *testing.T) {
	e := NewEvaluator()
	if err := e.ApplyRuleSet(&RuleSet{Version: 1, Rules: []Rule{baseRule("r1", "d1", "t")}}); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if e.Version() != 1 || e.RuleCount() != 1 {
		t.Fatalf("版本/条数不符: %d/%d", e.Version(), e.RuleCount())
	}
	// 校验失败不改现状
	if err := e.ApplyRuleSet(&RuleSet{Version: 2, Rules: []Rule{{RuleID: "R!"}}}); err == nil {
		t.Fatal("非法规则包应报错")
	}
	if e.Version() != 1 {
		t.Fatalf("校验失败不应改变版本: %d", e.Version())
	}
	// 状态重置：pending 不延续
	r := baseRule("r1", "d1", "t")
	r.Condition = Condition{Type: ConditionThreshold, Op: OpGT, Value: 10, ForSeconds: 10}
	if err := e.ApplyRuleSet(&RuleSet{Version: 2, Rules: []Rule{r}}); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	e.Observe("d1", "", "t", 11, 1000) // pending 起点 1000
	// 替换规则集（同规则、新版本）→ 状态清零
	if err := e.ApplyRuleSet(&RuleSet{Version: 3, Rules: []Rule{r}}); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if evs := e.Observe("d1", "", "t", 11, 11000); len(evs) != 0 {
		t.Fatalf("重置后 11000 应重新 pending（差 10s 从 11000 起算），不应触发: %+v", evs)
	}
	if evs := e.Observe("d1", "", "t", 11, 21000); len(evs) != 1 {
		t.Fatalf("重置后重新累计应触发: %+v", evs)
	}
	// 空规则包：合法且不触发
	if err := e.ApplyRuleSet(&RuleSet{Version: 4}); err != nil {
		t.Fatalf("空规则包应合法: %v", err)
	}
	if evs := e.Observe("d1", "", "t", 99, 1); len(evs) != 0 {
		t.Fatalf("空规则包不应触发: %+v", evs)
	}
	if got := e.Version(); got != 4 {
		t.Fatalf("空规则包版本应为 4: %d", got)
	}
}

func TestEvaluatorCustomMessageAndName(t *testing.T) {
	r := baseRule("r1", "d1", "t")
	r.Name = "高温告警"
	r.Action.Message = "设备 ${device} 温度 ${value} 超限"
	e := evalWith(t, r)
	evs := e.Observe("d1", "", "t", 88, 100)
	if len(evs) != 1 {
		t.Fatalf("应触发: %+v", evs)
	}
	if evs[0].Message != "设备 d1 温度 88 超限" || evs[0].RuleName != "高温告警" {
		t.Fatalf("事件消息/名称不符: %+v", evs[0])
	}
}

// ─────────────────────────── US-3 治理过滤器 ───────────────────────────

func TestGovernorDirectPath(t *testing.T) {
	g := NewGovernor()
	d := g.Filter("d1", "", "t", 42, 1)
	if !d.Accept || !d.Valid || d.Value != 42 || d.Reason != "" {
		t.Fatalf("无策略应直通: %+v", d)
	}
	if st := g.Stats(); st != (GovernorStats{}) {
		t.Fatalf("直通不计入统计: %+v", st)
	}
	if g.PolicyCount() != 0 {
		t.Fatalf("策略数应为 0: %d", g.PolicyCount())
	}
}

func TestGovernorRange(t *testing.T) {
	g := NewGovernor()
	if err := g.ApplyPolicies([]GovernancePolicy{
		{DeviceName: "d1", Property: "t", Range: &Bounds{Min: 10, Max: 20}},
	}); err != nil {
		t.Fatalf("应用策略失败: %v", err)
	}
	// 越界（首次，无 last）→ 拦截且 Valid=false
	d := g.Filter("d1", "", "t", 25, 1)
	if d.Accept || d.Valid || d.Reason != ReasonOutOfRange {
		t.Fatalf("越界应拦截: %+v", d)
	}
	// 界内 → 采纳
	d = g.Filter("d1", "", "t", 15, 2)
	if !d.Accept || d.Value != 15 {
		t.Fatalf("界内应采纳: %+v", d)
	}
	// 再越界 → 拦截且带回 lastAccepted
	d = g.Filter("d1", "", "t", 5, 3)
	if d.Accept || !d.Valid || d.Value != 15 || d.Reason != ReasonOutOfRange {
		t.Fatalf("越界应拦截并带回有效值: %+v", d)
	}
}

func TestGovernorDebounce(t *testing.T) {
	g := NewGovernor()
	if err := g.ApplyPolicies([]GovernancePolicy{{DeviceName: "d1", Property: "t", Debounce: 3}}); err != nil {
		t.Fatalf("应用策略失败: %v", err)
	}
	// 首个值需 3 次稳定才采纳
	for i, want := range []struct {
		accept bool
		reason string
	}{{false, ReasonDebounce}, {false, ReasonDebounce}, {true, ""}} {
		d := g.Filter("d1", "", "t", 10, int64(i))
		if d.Accept != want.accept || want.reason != d.Reason {
			t.Fatalf("第 %d 次: 期望 accept=%v reason=%q，得到 %+v", i+1, want.accept, want.reason, d)
		}
	}
	// 新值 11：中断重置（11 → 11.5 → 11）
	g.Filter("d1", "", "t", 11, 10)      // count=1 拦截
	g.Filter("d1", "", "t", 11.5, 11)    // 变化重置 count=1 拦截
	g.Filter("d1", "", "t", 11, 12)      // 再次变化重置 count=1 拦截
	d := g.Filter("d1", "", "t", 11, 13) // count=2 拦截
	if d.Accept || d.Reason != ReasonDebounce {
		t.Fatalf("count=2 应拦截: %+v", d)
	}
	d = g.Filter("d1", "", "t", 11, 14) // count=3 采纳
	if !d.Accept || d.Value != 11 {
		t.Fatalf("稳定 3 次应采纳: %+v", d)
	}
}

func TestGovernorDeadband(t *testing.T) {
	g := NewGovernor()
	if err := g.ApplyPolicies([]GovernancePolicy{{DeviceName: "d1", Property: "t", Deadband: 0.5}}); err != nil {
		t.Fatalf("应用策略失败: %v", err)
	}
	if d := g.Filter("d1", "", "t", 100, 1); !d.Accept {
		t.Fatalf("首个值应采纳: %+v", d)
	}
	if d := g.Filter("d1", "", "t", 100.3, 2); d.Accept || d.Reason != ReasonDeadband || d.Value != 100 {
		t.Fatalf("微变应抑制: %+v", d)
	}
	if d := g.Filter("d1", "", "t", 100.7, 3); !d.Accept || d.Value != 100.7 {
		t.Fatalf("超死区应采纳: %+v", d)
	}
	if d := g.Filter("d1", "", "t", 100.7, 4); !d.Accept {
		t.Fatalf("同值快速路径应采纳: %+v", d)
	}
}

func TestGovernorDebounceCandidateResetOnSteadyValue(t *testing.T) {
	// N=2：候选值被稳态同值打断后不得跨打断累计（复核 P1-2 修复锚，v0.37.0 as-built）
	g := NewGovernor()
	if err := g.ApplyPolicies([]GovernancePolicy{{DeviceName: "d1", Property: "t", Debounce: 2}}); err != nil {
		t.Fatal(err)
	}
	// 10 达成稳态（c1 拦截 → c2 接受）
	if d := g.Filter("d1", "", "t", 10, 1); d.Reason != ReasonDebounce {
		t.Fatalf("c1 应拦截: %+v", d)
	}
	if d := g.Filter("d1", "", "t", 10, 2); !d.Accept {
		t.Fatalf("c2 应接受: %+v", d)
	}
	// 11 候选首现 → c1 拦截
	if d := g.Filter("d1", "", "t", 11, 3); d.Reason != ReasonDebounce {
		t.Fatalf("11 首现应拦截: %+v", d)
	}
	// 回到稳态 10（快速路径）→ 候选连续性打断
	if d := g.Filter("d1", "", "t", 10, 4); !d.Accept {
		t.Fatalf("稳态同值应采纳: %+v", d)
	}
	// 11 再现：未修复时会被跨打断累计为 c2 错误接受；正确语义为重新 c1 拦截
	if d := g.Filter("d1", "", "t", 11, 5); d.Accept || d.Reason != ReasonDebounce {
		t.Fatalf("打断后 11 应重新计数（拦截）: %+v", d)
	}
	// 11 真正连续第二次 → c2 接受
	if d := g.Filter("d1", "", "t", 11, 6); !d.Accept || d.Value != 11 {
		t.Fatalf("连续两次后应接受: %+v", d)
	}
}

func TestGovernorCombinationOrder(t *testing.T) {
	g := NewGovernor()
	if err := g.ApplyPolicies([]GovernancePolicy{{
		DeviceName: "d1", Property: "t",
		Range: &Bounds{Min: 0, Max: 100}, Debounce: 2, Deadband: 1,
	}}); err != nil {
		t.Fatalf("应用策略失败: %v", err)
	}
	// range 优先：越界不进入 debounce 计数
	if d := g.Filter("d1", "", "t", 200, 1); d.Reason != ReasonOutOfRange {
		t.Fatalf("越界应优先拦截: %+v", d)
	}
	if d := g.Filter("d1", "", "t", 200, 2); d.Reason != ReasonOutOfRange {
		t.Fatalf("越界应持续拦截: %+v", d)
	}
	// 10：debounce 拦截（count=1）
	if d := g.Filter("d1", "", "t", 10, 3); d.Reason != ReasonDebounce {
		t.Fatalf("首次应 debounce: %+v", d)
	}
	// 10：count=2 → 候选 → deadband 无 last → 采纳
	if d := g.Filter("d1", "", "t", 10, 4); !d.Accept {
		t.Fatalf("稳定后应采纳: %+v", d)
	}
	// 10.5：count=1 拦截
	if d := g.Filter("d1", "", "t", 10.5, 5); d.Reason != ReasonDebounce {
		t.Fatalf("变化应重置 debounce: %+v", d)
	}
	// 10.5：count=2 → 候选 → deadband |0.5|<1 → 拦截
	if d := g.Filter("d1", "", "t", 10.5, 6); d.Reason != ReasonDeadband || d.Value != 10 {
		t.Fatalf("稳定但微变应 deadband 拦截: %+v", d)
	}
	// 12：count=1 拦截；12：count=2 → 候选 → deadband |2|>=1 → 采纳
	if d := g.Filter("d1", "", "t", 12, 7); d.Reason != ReasonDebounce {
		t.Fatalf("新值应先 debounce: %+v", d)
	}
	if d := g.Filter("d1", "", "t", 12, 8); !d.Accept || d.Value != 12 {
		t.Fatalf("稳定且超死区应采纳: %+v", d)
	}
}

func TestGovernorApplyPoliciesReset(t *testing.T) {
	g := NewGovernor()
	if err := g.ApplyPolicies([]GovernancePolicy{{DeviceName: "d1", Property: "t", Deadband: 10}}); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	g.Filter("d1", "", "t", 5, 1) // lastAccepted=5
	// 替换策略（同键新参数）→ 状态重置（lastAccepted 清空）
	if err := g.ApplyPolicies([]GovernancePolicy{{DeviceName: "d1", Property: "t", Deadband: 0.1}}); err != nil {
		t.Fatalf("替换失败: %v", err)
	}
	// 重置后首个值直接采纳（无 last 参照）
	if d := g.Filter("d1", "", "t", 5.05, 2); !d.Accept {
		t.Fatalf("重置后首个值应采纳: %+v", d)
	}
	// 非法策略：不改现状
	if err := g.ApplyPolicies([]GovernancePolicy{{DeviceName: "d1", Property: "t", Debounce: 1}}); err == nil {
		t.Fatal("非法策略应报错")
	}
	if g.PolicyCount() != 1 {
		t.Fatalf("非法替换不应改变策略数: %d", g.PolicyCount())
	}
	// 清空 → 直通
	if err := g.ApplyPolicies(nil); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if d := g.Filter("d1", "", "t", -999, 3); !d.Accept {
		t.Fatalf("清空后应直通: %+v", d)
	}
}

func TestGovernorNaN(t *testing.T) {
	g := NewGovernor()
	if err := g.ApplyPolicies([]GovernancePolicy{{DeviceName: "d1", Property: "t", Range: &Bounds{Min: 0, Max: 100}}}); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	d := g.Filter("d1", "", "t", math.NaN(), 1)
	if d.Accept || d.Reason != ReasonOutOfRange {
		t.Fatalf("NaN 应按坏值拦截: %+v", d)
	}
	if st := g.Stats(); st.OutOfRange != 1 {
		t.Fatalf("NaN 应计入越界统计: %+v", st)
	}
}

func TestGovernorStats(t *testing.T) {
	g := NewGovernor()
	if err := g.ApplyPolicies([]GovernancePolicy{{
		DeviceName: "d1", Property: "t",
		Range: &Bounds{Min: 0, Max: 100}, Debounce: 2, Deadband: 1,
	}}); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	g.Filter("d1", "", "t", 200, 1)  // out_of_range
	g.Filter("d1", "", "t", 10, 2)   // debounce（count=1）
	g.Filter("d1", "", "t", 10, 3)   // accept
	g.Filter("d1", "", "t", 10.5, 4) // debounce（count=1）
	g.Filter("d1", "", "t", 10.5, 5) // deadband
	g.Filter("d1", "", "t", 10, 6)   // 快速路径（同值）→ passed
	st := g.Stats()
	if st.OutOfRange != 1 || st.Debounce != 2 || st.Deadband != 1 || st.Passed != 2 {
		t.Fatalf("统计不符: %+v", st)
	}
}
