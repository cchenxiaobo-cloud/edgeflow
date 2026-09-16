// 数据治理过滤器（v0.37.0）：range 越界拦截 → debounce 稳定确认 → deadband
// 微变抑制。拦截值不写影子（抑制上报），坏值不参与规则评估。
//
// 语义（与 spec 0010 US-3 一致）：
//   - 未配置策略的 device×property 恒直通（Accept=true，零行为路径）；
//   - range：越界值拦截（reason=out_of_range；值不更新）；
//   - debounce N（>=2）：新值需连续 N 次采样严格相等才获候选资格，
//     期间变化则计数重置；值为 lastAccepted 时直接通过；
//   - deadband D（>0）：候选中值与 lastAccepted 差 |Δ| < D 时拦截
//     （reason=deadband，值不更新）；
//   - 拦截时 Value 返回 lastAccepted（若有）并置 Valid，供规则评估以
//     "有效值"推进时间语义（坏值 out_of_range 除外——调用方应跳过评估）。
//
// 组合顺序固定（range → debounce → deadband），与测试锚一一对应。
// 并发安全：Filter 在采集 goroutine、ApplyPolicies 在消息处理 goroutine，
// 内部加锁。
package rules

import (
	"math"
	"sync"
)

// Decision 是一次治理过滤的裁决。
type Decision struct {
	Accept bool    // 是否采纳（true = 写入影子/参与评估）
	Valid  bool    // Value 是否为有效值（拦截时若有 lastAccepted 则为 true）
	Value  float64 // 采纳值；或拦截时的 lastAccepted（Valid=true 时有效）
	Reason string  // ""（采纳）| "out_of_range" | "debounce" | "deadband"
}

// 拦截原因常量（供统计与断言使用）。
const (
	ReasonOutOfRange = "out_of_range"
	ReasonDebounce   = "debounce"
	ReasonDeadband   = "deadband"
)

// GovernorStats 是治理过滤器累计计数（诊断用）。
type GovernorStats struct {
	Passed     int64 // 采纳次数（含无策略直通）
	OutOfRange int64 // 范围越界拦截
	Debounce   int64 // 稳定确认拦截
	Deadband   int64 // 死区抑制拦截
}

// govState 是单 device×property 的治理状态。
type govState struct {
	policy       GovernancePolicy
	hasLast      bool
	lastAccepted float64 // 最近一次采纳值
	pendingVal   float64 // debounce 候选值
	pendingCount int     // 候选值连续出现次数
}

// Governor 是数据治理过滤器：持有策略表并按固定顺序执行过滤链。
// 零值不可用，须经 NewGovernor 构造。
type Governor struct {
	mu       sync.Mutex
	policies map[string]*govState
	stats    GovernorStats
}

// NewGovernor 创建空治理器（无策略 → Filter 全直通）。
func NewGovernor() *Governor {
	return &Governor{policies: make(map[string]*govState)}
}

// ApplyPolicies 校验并全量替换治理策略（重置全部过滤状态）。
// 校验失败不改变现状。空列表合法（清空全部策略 → 回到直通）。
func (g *Governor) ApplyPolicies(ps []GovernancePolicy) error {
	next := make(map[string]*govState, len(ps))
	for i := range ps {
		p := &ps[i]
		if err := p.Validate(); err != nil {
			return err
		}
		k := govKeyOf(p.Namespace, p.DeviceName, p.Property)
		if _, dup := next[k]; dup {
			return errDuplicatePolicy(normNamespace(p.Namespace), p.DeviceName, p.Property)
		}
		next[k] = &govState{policy: *p}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.policies = next
	return nil
}

// errDuplicatePolicy 构造重复策略错误（与 RuleSet.Validate 同一文案口径）。
func errDuplicatePolicy(ns, deviceName, property string) error {
	return &duplicatePolicyError{ns: ns, deviceName: deviceName, property: property}
}

type duplicatePolicyError struct{ ns, deviceName, property string }

func (e *duplicatePolicyError) Error() string {
	return "治理策略重复（" + e.ns + "/" + e.deviceName + "." + e.property + "）"
}

// PolicyCount 返回当前生效的策略条数。
func (g *Governor) PolicyCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.policies)
}

// Stats 返回累计统计快照。
func (g *Governor) Stats() GovernorStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// Filter 对一次采样值执行治理过滤链，返回裁决。
// ts 为采样时间（毫秒）——当前实现未使用（debounce 为计数语义而非时间窗），
// 保留参数以固定调用契约（时间窗语义为后续版本扩展点）。
func (g *Governor) Filter(deviceName, namespace, property string, value float64, ts int64) Decision {
	_ = ts
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.policies[govKeyOf(namespace, deviceName, property)]
	if !ok {
		// 无策略：直通（不计数、零行为路径）
		return Decision{Accept: true, Valid: true, Value: value}
	}
	// NaN 为不可信值：按越界拦截语义处理（坏值）。
	if math.IsNaN(value) {
		g.stats.OutOfRange++
		return Decision{Valid: st.hasLast, Value: st.lastAccepted, Reason: ReasonOutOfRange}
	}
	// 无变化快速路径：值与最近采纳值一致 → 直接通过。
	// v0.37.0 as-built（复核 P1-2 处置）：同时重置 debounce 候选连续性——
	// 回到稳态值意味着此前候选被打断，跨打断累计属语义瑕疵（严格「连续 N 次」
	// 不允许中断）；deadband 不重复判定（Δ=0 无信息，重复拦截是计数噪音）。
	if st.hasLast && value == st.lastAccepted {
		g.stats.Passed++
		st.pendingVal = value
		st.pendingCount = 0
		return Decision{Accept: true, Valid: true, Value: value}
	}
	// 1) range 越界拦截（不污染 debounce 计数）
	if p := st.policy.Range; p != nil && (value < p.Min || value > p.Max) {
		g.stats.OutOfRange++
		return Decision{Valid: st.hasLast, Value: st.lastAccepted, Reason: ReasonOutOfRange}
	}
	// 2) debounce 稳定确认
	if st.policy.Debounce >= 2 {
		if value == st.pendingVal {
			st.pendingCount++
		} else {
			st.pendingVal = value
			st.pendingCount = 1
		}
		if st.pendingCount < st.policy.Debounce {
			g.stats.Debounce++
			return Decision{Valid: st.hasLast, Value: st.lastAccepted, Reason: ReasonDebounce}
		}
	}
	// 3) deadband 微变抑制（仅在有参照值时判定）
	if st.policy.Deadband > 0 && st.hasLast && math.Abs(value-st.lastAccepted) < st.policy.Deadband {
		g.stats.Deadband++
		return Decision{Valid: true, Value: st.lastAccepted, Reason: ReasonDeadband}
	}
	// 采纳：更新状态
	st.hasLast = true
	st.lastAccepted = value
	st.pendingVal = value
	st.pendingCount = 0
	g.stats.Passed++
	return Decision{Accept: true, Valid: true, Value: value}
}
