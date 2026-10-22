# spec 0035 — 静默联动通知抑制（v0.62.0）

> 版本：v0.62.0 ｜ 缺口：spec 0031 裁决 6 承接（静默作用转发出口）｜
> 上游：spec 0031（v0.58 静默窗口）、spec 0032（v0.59 转发出口增强）
> 说明：本版为语义联动版本：零新增端点（135 维持）、零新增消息（19 不变）；
> mutestore schema 只增字段（severity，旧快照向后兼容）。

## 背景与范围裁决

v0.58 静默窗口落地时明确「静默不作用于转发出口」为非目标（spec 0031 裁决 6）。
运维实践中检修窗口的核心诉求正是**通知抑制**——面板可以红，手机不能响。本版
把该非目标转正：

**裁决：forwardstore Dispatch 投递前经 MuteSource 查询节点+级别静默状态；
生效中的静默事件不投递、不记台账（通知未发生即无事件）；mutestore 增 severity
可选维度（仅作用于转发过滤，层级活跃口径维持节点级）。**

## 裁决

1. **静默联动**：forwardstore 增本地 MuteSource 接口（MutedEvent(nodeID,
   severity, nowMs)——与 hierarchyview.MuteSource 同型但含 severity 维度，避免
   跨包依赖）+ SetMutes 装配；Dispatch 投递前判定，命中 → 该事件不投递、不记
   台账（通知未发生即无事件——台账只记录真实投递尝试）；未装配时行为不变。
2. **severity 维度**：Silence 增 severity 可选字段（空=全部级别；
   critical|warning|info）；MatchesEvent(nodeID, severity) = Active && nodeID
   匹配 &&（窗口未指定级别 || 级别相等）；**层级活跃口径维持节点级**（v0.58
   Muted 语义不变——severity 窗口不参与节点级 Muted 判定，避免 critical 静默
   误伤 warning 活跃计数；口径差异显式登记）。
3. **schema 扩展**：POST /api/v1/alerts/mutes body 增 severity 可选字段；
   快照 schema 只增字段（旧快照 Load 默认空 = 全级别，向后兼容）。
4. **Dispatch 语义变更**：静默中的事件不投递、不记台账——与「台账记录真实
   投递尝试」口径一致；该语义变更在 API-COMPATIBILITY 增量节显著登记
   （v0.62 前静默概念不存在，无行为回归）。
5. **只增不改**：既有 135 端点零改动；forwardstore.SetMutes 为纯新增；
   mutestore.Create 签名增 severity 参数（内部 API，调用面 main.go/测试全量
   适配）。
6. **非目标**：静默告警的「已抑制」台账记录（当前不记——抑制事件无副作用）、
   severity 窗口参与层级活跃口径（口径差异维持）、周期性静默、静默审批流。

## 端点

无新增（135 维持）；mutes POST body schema 扩展（severity 可选）。

## 交付锚

（交付后回填：单测 8 例 / e2e 两段 / 门禁数字 / R5 复核安排）
