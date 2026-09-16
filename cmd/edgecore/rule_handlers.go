// 规则链装配（v0.37.0）：RuleSync 下发处理 + 规则包持久化/恢复 +
// 规则事件出口（台账 + 上行）+ 采样管道（治理过滤 → 规则评估）。
//
// 协作链路（与 spec 0010 US-6 一致）：
//
//	云端 rules/sync → 可靠投递 RuleSync → EdgeHub 回调 handleRuleSync
//	  → 校验 + 版本检查 → 评估器 ApplyRuleSet + 治理器 ApplyPolicies
//	  → 持久化 store["rules/current"]（重启恢复）；
//	采集循环 → samplePipeline.process：治理过滤（拦截值不写影子）
//	  → 规则评估（坏值跳过、其余以有效值推进）→ 事件出口：
//	    ① RuleLedger.SaveEvent（SQLite 留痕）；② RuleEvent 消息上行云端。
package main

import (
	"encoding/json"
	"fmt"

	"edgeflow/edge/pkg/edgehub"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/rules"
)

// ruleSetStoreKey 是规则包在 MetaManager KV 中的持久化键。
const ruleSetStoreKey = "rules/current"

// RuleSyncPayload 是 RuleSync 消息的负载（云→边）：
// ruleSet 为全量规则包（版本 + 规则 + 治理策略）。字段与云端契约一致。
type RuleSyncPayload struct {
	RuleSet rules.RuleSet `json:"ruleSet"`
}

// handleRuleSync 处理一条规则包下发消息（TypeRuleSync）。
//
// 处理流程：解析 → 校验（模型完整性）→ 版本检查（陈旧拒绝）→
// 应用（评估器 + 治理器）→ 持久化（失败返回 error 供云端重试，重试幂等）。
//
// 返回语义（与 EdgeHub 自动 Ack 联动）：nil → Ack code=ok（云端 200）；
// error → Ack code=error（云端 502，可重试同 ID 重新执行）。
func handleRuleSync(engine *rules.Evaluator, governor *rules.Governor, store *metamanager.Store, msg *protocol.Message) error {
	if msg == nil {
		return fmt.Errorf("RuleSync 消息为空（nil）")
	}
	var payload RuleSyncPayload
	if err := msg.DecodePayload(&payload); err != nil {
		return fmt.Errorf("解析 RuleSync 负载失败: %w", err)
	}
	rs := &payload.RuleSet
	if err := rs.Validate(); err != nil {
		return fmt.Errorf("规则包校验失败: %w", err)
	}
	if cur := engine.Version(); rs.Version < cur {
		return fmt.Errorf("规则包版本陈旧（收到 %d < 当前 %d），拒绝应用", rs.Version, cur)
	}
	if err := engine.ApplyRuleSet(rs); err != nil {
		return fmt.Errorf("应用规则包失败: %w", err)
	}
	if err := governor.ApplyPolicies(rs.Governance); err != nil {
		return fmt.Errorf("应用治理策略失败: %w", err)
	}
	// 持久化（重启恢复用）。失败返回 error：本次处理视为失败，云端可重试；
	// 重试时版本相同（== 接受）幂等执行。
	raw, err := json.Marshal(rs)
	if err != nil {
		return fmt.Errorf("序列化规则包失败: %w", err)
	}
	if store != nil {
		if err := store.Put(ruleSetStoreKey, string(raw)); err != nil {
			return fmt.Errorf("规则包持久化失败: %w", err)
		}
	}
	log.Infof("规则包已应用（version=%d，规则 %d 条，治理策略 %d 条）",
		rs.Version, len(rs.Rules), len(rs.Governance))
	return nil
}

// loadRuleSet 从 MetaManager 恢复持久化规则包（edgecore 启动时调用）。
// 无持久化数据（首次运行）静默跳过；数据损坏/校验失败 → Warn 并保持
// 空规则（安全降级：宁可无规则，不应用来路不明的配置），不阻断启动。
func loadRuleSet(engine *rules.Evaluator, governor *rules.Governor, store *metamanager.Store) {
	if store == nil {
		return
	}
	raw, ok, err := store.Get(ruleSetStoreKey)
	if err != nil {
		log.Warnf("读取持久化规则包失败（以空规则启动）: %v", err)
		return
	}
	if !ok {
		return // 无持久化规则包（首次运行或从未下发）
	}
	var rs rules.RuleSet
	if err := json.Unmarshal([]byte(raw), &rs); err != nil {
		log.Warnf("持久化规则包解析失败（以空规则启动）: %v", err)
		return
	}
	if err := engine.ApplyRuleSet(&rs); err != nil {
		log.Warnf("持久化规则包校验失败（以空规则启动）: %v", err)
		return
	}
	if err := governor.ApplyPolicies(rs.Governance); err != nil {
		log.Warnf("持久化治理策略校验失败（以空策略启动）: %v", err)
		return
	}
	log.Infof("已从持久化恢复规则包（version=%d，规则 %d 条，治理策略 %d 条）",
		rs.Version, len(rs.Rules), len(rs.Governance))
}

// buildRuleEventMessage 从触发事件构造 RuleEvent 上行消息（纯函数，便于单测）。
func buildRuleEventMessage(nodeID string, ev rules.Event) (*protocol.Message, error) {
	return protocol.NewMessage(protocol.TypeRuleEvent, nodeID, targetCloud, ev)
}

// newRuleEventSink 构造规则事件出口：① 落规则事件台账（失败 Warn 不阻断）；
// ② 上行 RuleEvent 消息（尽力而为——发送失败只 Warn，规则在后续触发时
// 会再次产出事件，与 DeviceReport 的"最终一致"口径一致）。
// client/ledger 允许为 nil（测试与降级装配）。
func newRuleEventSink(client *edgehub.Client, ledger *metamanager.RuleLedger, nodeID string) func(rules.Event) {
	return func(ev rules.Event) {
		if ledger != nil {
			rec := metamanager.RuleEventRecord{
				Ts:        ev.TriggeredAt,
				RuleID:    ev.RuleID,
				DeviceID:  ev.DeviceName,
				Namespace: ev.Namespace,
				Severity:  ev.Severity,
				Value:     fmt.Sprintf("%g", ev.Value),
				Message:   ev.Message,
			}
			if err := ledger.SaveEvent(rec); err != nil {
				log.Warnf("规则事件台账写入失败: %v", err)
			}
		}
		if client == nil {
			return
		}
		msg, err := buildRuleEventMessage(nodeID, ev)
		if err != nil {
			log.Warnf("构造 RuleEvent 消息失败: %v", err)
			return
		}
		if err := client.Send(msg); err != nil {
			log.Warnf("规则事件上报失败: %v", err)
		}
	}
}

// samplePipeline 组合治理过滤与规则评估，装配进采集管道（v0.37.0）。
//
// 语义（spec US-6）：
//   - 逐属性过治理过滤器：采纳值写入影子（返回给调用方）；
//   - 规则评估输入：坏值（out_of_range）跳过 → 不改变规则状态；
//     其余拦截以有效值（lastAccepted）推进时间语义；采纳以新值推进；
//   - 触发事件经 onEvent 出口（台账 + 上行）。
//
// 无策略无规则时：治理恒直通、评估器空转 → 与 v0.36.0 行为逐字节等价。
type samplePipeline struct {
	gov     *rules.Governor
	eng     *rules.Evaluator
	onEvent func(rules.Event)
	tsSink  func(device, ns, prop string, value float64, ts int64) // v0.38.0 可选；nil=零行为
}

// newSamplePipeline 构造采样管道（测试与装配共用）。
func newSamplePipeline(gov *rules.Governor, eng *rules.Evaluator, onEvent func(rules.Event)) *samplePipeline {
	return &samplePipeline{gov: gov, eng: eng, onEvent: onEvent}
}

// SetTSSink 设置时序存储写入出口（v0.38.0，spec 0011 US-6）：写入的是
// 准许写入影子的 accepted 值（与影子同源同值）；nil 保持关闭（零行为）。
func (p *samplePipeline) SetTSSink(fn func(device, ns, prop string, value float64, ts int64)) {
	if p == nil {
		return
	}
	p.tsSink = fn
}

// process 处理一台设备的一轮采集值，返回"准许写入影子"的属性集合。
// 治理器/评估器为 nil（未装配）时原样直通；时序 sink（v0.38.0）启用时
// accepted 值同时写入时序库。
func (p *samplePipeline) process(deviceName, ns string, props map[string]float64, ts int64) map[string]float64 {
	if p == nil {
		return props
	}
	if p.gov == nil || p.eng == nil {
		// 无规则/治理（或仅时序库）：直通；仅时序 sink 启用时记录采样值
		if p.tsSink != nil {
			for prop, value := range props {
				p.tsSink(deviceName, ns, prop, value, ts)
			}
		}
		return props
	}
	accepted := make(map[string]float64, len(props))
	for prop, value := range props {
		d := p.gov.Filter(deviceName, ns, prop, value, ts)
		switch {
		case d.Accept:
			accepted[prop] = d.Value
			if p.tsSink != nil {
				p.tsSink(deviceName, ns, prop, d.Value, ts)
			}
			p.observe(deviceName, ns, prop, d.Value, ts)
		case d.Reason == rules.ReasonOutOfRange:
			// 坏值：不写影子、不参与规则评估（不改变规则状态）
		case d.Valid:
			// 去抖/死区拦截：以有效值（lastAccepted）推进规则时间语义
			p.observe(deviceName, ns, prop, d.Value, ts)
		default:
			// 无有效值（策略刚生效/首个值被拦截）：跳过评估
		}
	}
	return accepted
}

// observe 对单属性求值并派发触发事件。
func (p *samplePipeline) observe(deviceName, ns, prop string, value float64, ts int64) {
	for _, ev := range p.eng.Observe(deviceName, ns, prop, value, ts) {
		if p.onEvent != nil {
			p.onEvent(ev)
		}
	}
}
