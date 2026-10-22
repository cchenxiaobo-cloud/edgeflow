# EdgeFlow v0.63.0 发布说明（告警 muted 标记与静默统计）

> 版本：v0.63.0 ｜ 日期：2026-10-22 ｜ spec：0036-alarm-muted-stats ｜ 缺口：静默窗口可观测性收口
> 基线：v0.62.0（837077d）。**本版为纯云端管理面只增版本：零新增消息（19 不变），端点 135→136（+1 全读面）。**

## N1 能力

- **告警 muted 标记**：/api/v1/alarms 列表每条增 `muted` bool（实时判定
  muteStore.MutedEvent(nodeID, severity, now)——运维一眼区分「静默中的活跃
  告警」）；**匿名嵌入 wrapper**（struct{alarm.Alarm; Muted bool}）保全部既有
  字段——schema 只增不改；未装配静默源时恒 false。
- **静默统计**（GET /api/v1/alerts/mutes/stats，viewer+）：{total, active,
  byNode（生效中按节点聚合，全节点窗口计 "*"）, now}。

## N2 兼容

- 端点 135→136（+1）：见 API-COMPATIBILITY §1 矩阵 136 与 v0.63.0 增量节。
- /api/v1/alarms 响应 schema 只增字段（muted）——既有字段零改动（匿名嵌入
  保全部字段）；未装配静默源时 muted 恒 false（v0.62 前行为等价）。
- 消息 19 不变；RBAC read=viewer+（stats）。

## N3 测试（e2e 两段）

- e2e `TestV0630MutedMarkE2E` 两段（真实告警管线）：US-1 静默生效期间列表
  muted:true 且告警历史数据保留（state=raised 不动）→ US-2 mutes/stats 聚合
  （total=1/active=1/byNode）→ 解除静默后 muted:false。

## N4 边界

- muted 为实时标注（不持久化）；severity 维度窗口在列表 muted 标记中生效
  （与转发过滤同口径——MutedEvent）。
- 本版无 KI 新增边界（纯只读标注面）。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- 契约：**136 条复验**（135→136 + /api/v1/alerts 探测既有）。
- 定向 e2e：`TestV0630MutedMarkE2E`（见交付锚回填）。

## N6 复核

- R1–R4 已处置收口；**R5（v0.62+v0.63 合并复核）**随本版推送后执行。
