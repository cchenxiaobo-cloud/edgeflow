# spec 0036 — 告警 muted 标记与静默统计（v0.63.0）

> 版本：v0.63.0 ｜ 缺口：静默窗口可观测性收口 ｜ 上游：spec 0031/0035
> 说明：端点 135→136（+1 全读面）；/api/v1/alarms 响应 schema 只增字段
> （muted 只读标记，匿名嵌入 wrapper 保全部既有字段）。

## 背景与范围裁决

v0.62 静默联动通知抑制落地后，运维在 /api/v1/alarms 列表看到活跃告警却无法
区分「静默中的告警」（活跃口径已排除但列表仍显示）；静默窗口本身也缺统计面。

**裁决：/api/v1/alarms 列表每条增 muted 只读标记（实时静默状态）+
GET /api/v1/alerts/mutes/stats 静默统计端点。**

## 裁决

1. **muted 标记**：/api/v1/alarms 列表每条增 `muted` bool（实时判定
   muteStore.MutedEvent(nodeID, severity, now)）；匿名嵌入 wrapper
   （struct{alarm.Alarm; Muted bool}）保全部既有字段——schema 只增不改；
   未装配静默源时恒 false。
2. **静默统计**：GET /api/v1/alerts/mutes/stats（viewer+）——{total, active,
   byNode（生效中按节点聚合，全节点窗口计 "*"）, now}。
3. **只增不改**：既有 135 端点零改动；消息 19 不变。
4. **非目标**：静默历史趋势、按 severity 聚合统计、告警列表 severity 过滤
   联动静默。

## 端点（+1，全读面）

| Method | Path | 语义 | RBAC |
|---|---|---|---|
| GET | /api/v1/alerts/mutes/stats | 静默窗口统计 | viewer+ |

## 交付锚

- 实际交付（2026-10-22）：e2e 两段 run2 全绿（muted:true 历史保留 / stats 聚合 /
  解除后 muted:false；run1 DELETE 用 operator 403 → admin 修正）；契约复验
  （136 条，135→136）；匿名嵌入 wrapper 保 schema 兼容。
- R5（v0.62+v0.63 合并复核）随本版推送后执行。
