// v0.37.0 测试锚（边缘侧）：RuleSync 处理与持久化、启动恢复、采样管道
// （治理 + 评估 + 事件出口）、规则事件台账接线。与 spec 0010 US-6 对应。
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/rules"
)

// newTestStore 创建临时元数据存储（test cleanup 自动关闭）。
func newTestStore(t *testing.T) *metamanager.Store {
	t.Helper()
	store, err := metamanager.Open(t.TempDir() + "/meta.db")
	if err != nil {
		t.Fatalf("打开测试存储失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// ruleSyncMsg 构造一条 RuleSync 消息（payload 为完整规则包）。
func ruleSyncMsg(t *testing.T, rs rules.RuleSet) *protocol.Message {
	t.Helper()
	msg, err := protocol.NewMessage(protocol.TypeRuleSync, "cloud", "edge-1", RuleSyncPayload{RuleSet: rs})
	if err != nil {
		t.Fatalf("构造 RuleSync 消息失败: %v", err)
	}
	return msg
}

// sampleRule 构造一条测试规则（temperature > 80 → warning 事件）。
func sampleRule(id string) rules.Rule {
	return rules.Rule{
		RuleID:     id,
		DeviceName: "sensor-01",
		Property:   "temperature",
		Condition:  rules.Condition{Type: rules.ConditionThreshold, Op: rules.OpGT, Value: 80},
		Action:     rules.Action{Type: rules.ActionEvent},
	}
}

func TestHandleRuleSyncAppliesAndPersists(t *testing.T) {
	store := newTestStore(t)
	engine := rules.NewEvaluator()
	governor := rules.NewGovernor()
	rs := rules.RuleSet{
		Version: 100,
		Rules:   []rules.Rule{sampleRule("hot")},
		Governance: []rules.GovernancePolicy{
			{DeviceName: "sensor-01", Property: "temperature", Range: &rules.Bounds{Min: -50, Max: 150}},
		},
	}
	if err := handleRuleSync(engine, governor, store, ruleSyncMsg(t, rs)); err != nil {
		t.Fatalf("应用规则包失败: %v", err)
	}
	if engine.Version() != 100 || engine.RuleCount() != 1 {
		t.Fatalf("引擎状态不符: version=%d rules=%d", engine.Version(), engine.RuleCount())
	}
	if governor.PolicyCount() != 1 {
		t.Fatalf("治理策略未应用: %d", governor.PolicyCount())
	}
	// 持久化断言：store 中可按 key 读回
	raw, ok, err := store.Get(ruleSetStoreKey)
	if err != nil || !ok {
		t.Fatalf("规则包未持久化: ok=%v err=%v", ok, err)
	}
	var got rules.RuleSet
	if err := jsonUnmarshal(raw, &got); err != nil {
		t.Fatalf("持久化数据不可解析: %v", err)
	}
	if got.Version != 100 || len(got.Rules) != 1 {
		t.Fatalf("持久化内容不符: %+v", got)
	}
}

func TestHandleRuleSyncRejectsStaleVersion(t *testing.T) {
	store := newTestStore(t)
	engine := rules.NewEvaluator()
	governor := rules.NewGovernor()
	if err := handleRuleSync(engine, governor, store, ruleSyncMsg(t, rules.RuleSet{Version: 10})); err != nil {
		t.Fatalf("首次应用失败: %v", err)
	}
	err := handleRuleSync(engine, governor, store, ruleSyncMsg(t, rules.RuleSet{Version: 9}))
	if err == nil || !containsStr(err.Error(), "陈旧") {
		t.Fatalf("陈旧版本应拒绝: %v", err)
	}
	if engine.Version() != 10 {
		t.Fatalf("拒绝后版本不应变化: %d", engine.Version())
	}
}

func TestHandleRuleSyncIdempotentSameVersion(t *testing.T) {
	store := newTestStore(t)
	engine := rules.NewEvaluator()
	governor := rules.NewGovernor()
	rs := rules.RuleSet{Version: 5, Rules: []rules.Rule{sampleRule("hot")}}
	for i := 0; i < 2; i++ {
		if err := handleRuleSync(engine, governor, store, ruleSyncMsg(t, rs)); err != nil {
			t.Fatalf("第 %d 次应用失败（== 版本应幂等接受）: %v", i+1, err)
		}
	}
	if engine.Version() != 5 {
		t.Fatalf("版本不符: %d", engine.Version())
	}
}

func TestHandleRuleSyncRejectsInvalid(t *testing.T) {
	store := newTestStore(t)
	engine := rules.NewEvaluator()
	governor := rules.NewGovernor()
	bad := rules.RuleSet{Version: 5, Rules: []rules.Rule{{RuleID: "BAD ID"}}}
	if err := handleRuleSync(engine, governor, store, ruleSyncMsg(t, bad)); err == nil {
		t.Fatal("非法规则包应拒绝")
	}
	if engine.Version() != 0 || engine.RuleCount() != 0 {
		t.Fatalf("拒绝后引擎不应变化: version=%d", engine.Version())
	}
	// 解析失败路径
	msg, _ := protocol.NewMessage(protocol.TypeRuleSync, "cloud", "edge-1", map[string]string{"x": "1"})
	if err := handleRuleSync(engine, governor, store, msg); err == nil {
		t.Fatal("合法 JSON 但缺 ruleSet 字段应报错")
	}
	// 空版本规则包（version=0）拒绝（复核 P2-4 边角：空库直接 sync 场景）
	if err := handleRuleSync(engine, governor, store, ruleSyncMsg(t, rules.RuleSet{Version: 0})); err == nil {
		t.Fatal("version=0 规则包应拒绝")
	}
}

func TestLoadRuleSetRestores(t *testing.T) {
	store := newTestStore(t)
	// 预置持久化规则包
	rs := rules.RuleSet{
		Version: 7,
		Rules:   []rules.Rule{sampleRule("hot")},
		Governance: []rules.GovernancePolicy{
			{DeviceName: "sensor-01", Property: "temperature", Deadband: 1},
		},
	}
	raw, err := jsonMarshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ruleSetStoreKey, raw); err != nil {
		t.Fatal(err)
	}
	engine := rules.NewEvaluator()
	governor := rules.NewGovernor()
	loadRuleSet(engine, governor, store)
	if engine.Version() != 7 || engine.RuleCount() != 1 || governor.PolicyCount() != 1 {
		t.Fatalf("恢复失败: version=%d rules=%d policies=%d",
			engine.Version(), engine.RuleCount(), governor.PolicyCount())
	}
}

func TestLoadRuleSetCorruptStaysEmpty(t *testing.T) {
	store := newTestStore(t)
	if err := store.Put(ruleSetStoreKey, "{not-json"); err != nil {
		t.Fatal(err)
	}
	engine := rules.NewEvaluator()
	governor := rules.NewGovernor()
	loadRuleSet(engine, governor, store) // 不应 panic
	if engine.Version() != 0 || engine.RuleCount() != 0 {
		t.Fatalf("损坏数据应安全降级为空规则: version=%d", engine.Version())
	}
	// 无持久化数据（首次运行）路径
	store2 := newTestStore(t)
	loadRuleSet(engine, governor, store2)
	if engine.Version() != 0 {
		t.Fatalf("空存储不应改变引擎: %d", engine.Version())
	}
}

func TestSamplePipelineGovernanceAndEvaluation(t *testing.T) {
	gov := rules.NewGovernor()
	if err := gov.ApplyPolicies([]rules.GovernancePolicy{
		{DeviceName: "sensor-01", Property: "temperature", Range: &rules.Bounds{Min: 0, Max: 100}},
	}); err != nil {
		t.Fatal(err)
	}
	eng := rules.NewEvaluator()
	if err := eng.ApplyRuleSet(&rules.RuleSet{Version: 1, Rules: []rules.Rule{{
		RuleID:     "hot",
		DeviceName: "sensor-01",
		Property:   "temperature",
		Condition:  rules.Condition{Type: rules.ConditionThreshold, Op: rules.OpGT, Value: 50},
		Action:     rules.Action{Type: rules.ActionEvent, Severity: rules.SeverityCritical},
	}}}); err != nil {
		t.Fatal(err)
	}
	var fired []rules.Event
	p := newSamplePipeline(gov, eng, func(ev rules.Event) { fired = append(fired, ev) })

	// 1) 正常值：采纳 + 触发（60 > 50）
	out := p.process("sensor-01", "default", map[string]float64{"temperature": 60, "humidity": 40}, 1000)
	if out["temperature"] != 60 || out["humidity"] != 40 {
		t.Fatalf("正常值应全部采纳: %v", out)
	}
	if len(fired) != 1 || fired[0].RuleID != "hot" || fired[0].Severity != rules.SeverityCritical {
		t.Fatalf("应触发 hot: %+v", fired)
	}

	// 2) 坏值越界：不写影子、不触发（firing 保持不变）
	out = p.process("sensor-01", "default", map[string]float64{"temperature": 200}, 2000)
	if _, ok := out["temperature"]; ok {
		t.Fatalf("坏值不应写入影子: %v", out)
	}
	if len(fired) != 1 {
		t.Fatalf("坏值不应触发事件: %+v", fired)
	}

	// 3) 恢复（40 不满足）→ 再次满足（70）→ 第二次触发
	out = p.process("sensor-01", "default", map[string]float64{"temperature": 40}, 3000)
	if out["temperature"] != 40 {
		t.Fatalf("正常值应采纳: %v", out)
	}
	p.process("sensor-01", "default", map[string]float64{"temperature": 70}, 4000)
	if len(fired) != 2 {
		t.Fatalf("恢复后应再次触发: %+v", fired)
	}
}

func TestSamplePipelineDeadbandAndForSeconds(t *testing.T) {
	gov := rules.NewGovernor()
	if err := gov.ApplyPolicies([]rules.GovernancePolicy{
		{DeviceName: "d1", Property: "t", Deadband: 5},
	}); err != nil {
		t.Fatal(err)
	}
	eng := rules.NewEvaluator()
	if err := eng.ApplyRuleSet(&rules.RuleSet{Version: 1, Rules: []rules.Rule{{
		RuleID:     "sustained",
		DeviceName: "d1",
		Property:   "t",
		Condition:  rules.Condition{Type: rules.ConditionThreshold, Op: rules.OpGT, Value: 10, ForSeconds: 10},
		Action:     rules.Action{Type: rules.ActionEvent},
	}}}); err != nil {
		t.Fatal(err)
	}
	var fired []rules.Event
	p := newSamplePipeline(gov, eng, func(ev rules.Event) { fired = append(fired, ev) })

	// t=1000：11 → 采纳 + pending 起点
	p.process("d1", "", map[string]float64{"t": 11}, 1000)
	// t=6000：11（同值）→ 采纳并推进（差 5s < 10s）
	p.process("d1", "", map[string]float64{"t": 11}, 6000)
	if len(fired) != 0 {
		t.Fatalf("未达持续期不应触发: %+v", fired)
	}
	// t=11000：11 → 差 10s → 触发
	p.process("d1", "", map[string]float64{"t": 11}, 11000)
	if len(fired) != 1 {
		t.Fatalf("达到持续期应触发: %+v", fired)
	}
	// 死区拦截：11.5 与 lastAccepted(11) 差 0.5 < 5 → 拦截、不写影子
	out := p.process("d1", "", map[string]float64{"t": 11.5}, 11500)
	if _, ok := out["t"]; ok {
		t.Fatalf("死区应拦截: %v", out)
	}
	// 超死区变化：变 20（差 9）→ 采纳；20>10 且 forSeconds 重新计时（firing 后曾恢复？不——firing 中，
	// 先给一个不满足值恢复）
	out = p.process("d1", "", map[string]float64{"t": 20}, 12000)
	if out["t"] != 20 {
		t.Fatalf("超死区变化应采纳: %v", out)
	}
}

func TestSamplePipelineDirectWhenUnconfigured(t *testing.T) {
	// 空治理 + 空规则：全直通、零事件（默认零行为锚）
	gov := rules.NewGovernor()
	eng := rules.NewEvaluator()
	p := newSamplePipeline(gov, eng, nil)
	in := map[string]float64{"a": 1, "b": 2}
	out := p.process("d1", "", in, 1000)
	if len(out) != 2 || out["a"] != 1 || out["b"] != 2 {
		t.Fatalf("未配置时不应改变值: %v", out)
	}
	// nil 管道（pipe 未装配路径）
	var nilPipe *samplePipeline
	if got := nilPipe.process("d1", "", in, 1000); len(got) != 2 {
		t.Fatalf("nil 管道应直通: %v", got)
	}
}

func TestRuleEventSinkWritesLedger(t *testing.T) {
	store := newTestStore(t)
	rl, err := metamanager.NewRuleLedger(store)
	if err != nil {
		t.Fatalf("创建规则事件台账失败: %v", err)
	}
	sink := newRuleEventSink(nil, rl, "edge-1") // client=nil：仅落台账路径
	sink(rules.Event{
		RuleID: "hot", DeviceName: "sensor-01", Namespace: "default",
		Severity: "critical", Value: 88.5, Message: "温度超限", TriggeredAt: 1234,
	})
	recs, err := rl.ListRuleEvents(metamanager.RuleEventFilter{})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("应落 1 条记录: %d", len(recs))
	}
	got := recs[0]
	if got.RuleID != "hot" || got.DeviceID != "sensor-01" || got.Severity != "critical" ||
		got.Value != "88.5" || got.Ts != 1234 || got.Message != "温度超限" {
		t.Fatalf("台账记录不符: %+v", got)
	}
}

func TestBuildRuleEventMessage(t *testing.T) {
	ev := rules.Event{
		RuleID: "hot", DeviceName: "sensor-01", Namespace: "default",
		Property: "temperature", Value: 88.5, Severity: "warning",
		Message: "m", TriggeredAt: 999, RuleSetVersion: 3,
	}
	msg, err := buildRuleEventMessage("edge-1", ev)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if msg.Type != protocol.TypeRuleEvent || msg.Source != "edge-1" || msg.Target != "cloud" {
		t.Fatalf("信封不符: type=%s source=%s target=%s", msg.Type, msg.Source, msg.Target)
	}
	var got rules.Event
	if err := msg.DecodePayload(&got); err != nil {
		t.Fatalf("payload 解码失败: %v", err)
	}
	if got != ev {
		t.Fatalf("payload 不符: %+v", got)
	}
}

// ── 测试辅助（本文件私有，避免与产品代码/其他测试命名冲突）──

// jsonMarshal 序列化为字符串。
func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// jsonUnmarshal 从字符串反序列化。
func jsonUnmarshal(raw string, v any) error { return json.Unmarshal([]byte(raw), v) }

// containsStr 子串判定。
func containsStr(s, sub string) bool { return strings.Contains(s, sub) }
