# spec 0034 — 诊断聚合面（v0.61.0）

> 版本：v0.61.0 ｜ 缺口：运维可观测性——系统健康一屏（聚合只读）+ R2 P3-2
> 处置（audit 参数校验）｜ 上游：spec 0029/0031/0032
> 说明：纯云端管理面只增版本：零新增消息（19 不变），端点 134→135（+1 全读面）。

## 背景与范围裁决

五轮迭代后平台资产分散在多个面（节点/告警/区域/静默/转发/etcd/审计），运维
排查需逐个端点调用。本版提供**诊断聚合一屏**：

**裁决：GET /api/v1/diagnostics/summary（viewer+）聚合各既有 store 快照 +
进程元数据；audit 查询参数非法值由静默归零改为显式 400（R2 P3-2 处置）。**

## 裁决

1. **summary 结构**：{now, startedAt, uptimeSec, goVersion, nodes{total,online},
   alarms{total,active}, regions{total}, mutes{total,active},
   forwards{total,enabled}, etcd(embedded|external), audit{path,bytes}}。
2. **口径**：节点在线对齐 v0.51（Ready/Unknown=在线侧）；告警活跃 = 非 closed
   （alarmstore.Stats 推导）；静默 active 实时计算（mutestore 语义）。
3. **实现边界**：聚合只读（各 store 自带锁，单次调用多次读非同一快照——计数
   微偏差可接受，对齐 hierarchyview 口径）；进程启动时点用包初始化近似。
4. **audit 参数校验（R2 P3-2 处置）**：status/limit/offset 非数字或负数 →
   显式 400（此前静默归零）；兼容性语义收紧在 API-COMPATIBILITY 增量节登记。
5. **只增不改**：既有 134 端点零改动；消息 19 不变。
6. **非目标**：Prometheus 指标扩展（/metrics 既有）、历史趋势、告警规则驱动的
   诊断、etcd 集群健康深探（成员/延迟——形态字段先行）。

## 端点（+1，全读面）

| Method | Path | 语义 | RBAC |
|---|---|---|---|
| GET | /api/v1/diagnostics/summary | 诊断聚合一屏 | viewer+ |

## 交付锚

- 实际交付（2026-10-22）：e2e 两段 run1 全绿（summary 结构/聚合计数/audit
  三组非法参数 400）；契约复验（135 条，134→135）；audit 参数语义收紧登记
  API-COMPATIBILITY 增量节。
- R4（v0.60+v0.61 合并复核 + 全量 e2e 终验 -timeout 30m）随本版推送执行。
