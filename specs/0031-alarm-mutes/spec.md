# spec 0031 — 告警静默窗口（v0.58.0）

> 版本：v0.58.0 ｜ 缺口：告警治理运维增强（静默窗口）｜ 上游：spec 0023（区域树
> ——层级聚合的地基）、spec 0024（hierarchy 活跃口径）
> 说明：纯云端管理面只增版本：零新增消息（19 不变），端点 130→133（+3）。

## 背景与范围裁决

告警中心（v0.40）与层级聚合（v0.51）落地后，运维侧缺一个标准能力：**已知晓、
无需重复响应的告警（检修窗口/已知问题），在活跃口径中排除**——否则告警面板
长期红、通知渠道持续打扰。

**裁决：独立 mutestore（JSON 快照）承载静默窗口；hierarchy 活跃口径经可选
MuteSource 接口排除生效中的静默节点告警；告警历史数据与四态计数不动。**

## 裁决

1. **模型**：{id, nodeId(可选，空=全部节点), from/to(毫秒窗，含边界), reason
   (≤256B), createdBy, createdAt}；JSON 快照持久化（EDGEFLOW_CLOUDCORE_MUTE_FILE，
   空=内存，治理面惯例）。
2. **窗口约束**：必须有界（to > from 且跨度 ≤30 天）；不可变（无 PUT，调整=
   删除重建）；总数 ≤200（防无限增长）；过期惰性失效（active 实时计算，快照
   保留历史留痕）。
3. **API 3 端点**（130→133）：GET 清单（viewer+；每条附 active 实时标记）/
   POST 创建（operator+；body {nodeId?, durationSec, reason}，durationSec
   1..2592000）/ DELETE（admin——v0.46 矩阵既有语义）。
4. **活跃口径**：hierarchy summary alarmActiveCount 排除生效中的静默节点告警
   （MuteSource 可选接口，SetMutes 启动期装配；未装配行为不变——v0.51 行为
   锚）。告警历史数据（/api/v1/alarms 列表）与 AlarmStats 四态计数不动
   （生命周期事实）。
5. **只增不改**：既有 130 端点零改动；消息 19 不变。
6. **非目标**：静默作用于转发出口（通知抑制，后续版本经 mutestore 注入）、
   severity 维度过滤、静默审批流、周期性静默（cron 型窗口）、告警列表 muted
   标记（历史数据保持纯净）。

## 端点（+3，2 写面 1 读面）

| Method | Path | 语义 | RBAC |
|---|---|---|---|
| GET | /api/v1/alerts/mutes | 静默窗口清单（active 标记） | viewer+ |
| POST | /api/v1/alerts/mutes | 创建窗口 | operator+ |
| DELETE | /api/v1/alerts/mutes/{id} | 删除窗口 | admin |

## 数据依赖

- mutestore（自包含快照）← hierarchyview（MuteSource 活跃口径）。

## 交付锚

- 实际交付（2026-10-22）：单测 8 例（mutestore 5 + hierarchyview 3，race 2.408s/1.794s 绿）；契约复验 16.320s 绿（133 条，130→133）；定向 e2e run2 PASS 10.84s（run1 抓到 POST 响应缺 active 标记 → 响应视图修正）。
- R3（v0.57+v0.58 合并复核）随本版推送后执行。
