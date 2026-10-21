# spec 0024 — 区域协同层（v0.51.0，G01）

> 版本：v0.51.0 ｜ 缺口：G01「三级分级纳管（中心/区域/站点）」+ 灾备口径评估 ｜
> 上游：spec 0023（v0.50 区域树/标签——本版地基）
> replan 验收锚：三级视图演示 + 灾备评估报告（GB/T 20988-2025）

## 背景与路径裁决

replan §3.5 给出三条候选路径：分区多实例联邦 / 轻量区域管理节点 / 中心直管+标签分区等效。

**裁决：中心直管架构下提供「区域级视图 + 区域级策略 + 数据汇聚」能力面（路径 3 增强）。**
理由：单 cloudcore 事实架构未变（只增不改红线）；区域管理节点与联邦是架构级改造，
需先有规模数据（v0.52 100 节点压测）支撑决策——本版把协同**语义**立起来，多实例化
留立项评估。三级语义：中心 = 本 cloudcore；区域 = regionstore 树节点；节点 = registry。

## 裁决

1. **聚合视图**：`GET /api/v1/hierarchy/summary` —— 全树形汇总：每区域
   `{id,name,parentId,policySet,nodeCount,onlineCount,alarmActiveCount,children}`
   递归含子区域（自底向上聚合：子区域计数并入父）；根级另含 `unassignedCount`
   （未归属任何区域的注册节点数）。
2. **区域明细**：`GET /api/v1/hierarchy/regions/{id}/nodes` —— 该区域**含子树递归**
   的节点明细 `{nodeId,nodeName,region,online,ip}`；未知区域 404。
3. **告警汇聚**：`GET /api/v1/hierarchy/alarms` —— 按节点归属区域聚合计数：
   `{regions:[{regionId,regionName,total,raised,acked,assigned,closed}],unassigned:{...}}`
   （活跃 = 非 closed；status != Offline 记为 online 侧）。
4. **区域策略**：Region 增 `Policy json.RawMessage`（≤4KB 合法 JSON 对象）——
   `GET/PUT /api/v1/hierarchy/regions/{id}/policy`；PUT 幂等 upsert，body `null`
   或 `{}` 清除；未知区域 404；快照 schema 增字段（旧快照 Load 容错默认 null，
   向后兼容）。策略本版仅存储与视图透出，**不做下发执行**（非目标）。
5. **在线判定**：registry `NodeInfo.Status` 为唯一权威（Ready/Unknown=在线侧，
   Offline=离线）；本版不自建心跳窗口。
6. **RBAC**：读面（summary/nodes/alarms/policy GET）= viewer+；策略写（PUT）=
   operator+；全端点无 DELETE（规避 operator 无 delete 语义，v0.46 矩阵只增不改）。
7. **性能口径**：实时聚合（假设 ≤ 数百节点/≤2000 告警）；缓存化与 >1000 节点
   优化留 v0.52（规模版按实测定）。
8. **灾备评估**：`docs/DR-ASSESSMENT.md` 文档交付——GB/T 20988-2025 六级（RTO/RPO）
   对标、现状单实例架构的恢复能力评估、区域汇聚节点五级建议的差距登记、
   v0.52 验收项衔接；**不改代码**。
9. **只增不改**：零新增消息；既有 114 端点契约零改动；契约 114→119（+5）。
10. **非目标**：策略下发执行、跨实例联邦/区域管理节点、区域级 RBAC scope 联动、
    区域数据写路由。

## 端点（+5，全读面 1 写面）

| Method | Path | 语义 | RBAC |
|---|---|---|---|
| GET | /api/v1/hierarchy/summary | 三级树形汇总（递归聚合） | viewer+ |
| GET | /api/v1/hierarchy/regions/{id}/nodes | 区域节点明细（含子树） | viewer+ |
| GET | /api/v1/hierarchy/alarms | 区域×状态告警计数 | viewer+ |
| GET | /api/v1/hierarchy/regions/{id}/policy | 区域策略读取 | viewer+ |
| PUT | /api/v1/hierarchy/regions/{id}/policy | 区域策略 upsert/清除 | operator+ |

## 数据依赖

- regionstore（区域树+归属+Policy）｜registry（NodeInfo.List：状态/IP）｜
  alarmstore（List 全量计数，limit 2000 上限偏差登记 KI）。

## 交付锚

- 实际交付（2026-10-21）：单测 10 例（hierarchyview 9 + regionstore null 清除
  回归锚 1）；契约复验 13.656s 绿；定向 e2e run5 PASS 7.684s（run1 Unmarshal
  残留/run3 空态 regions null/run4 SetPolicy 误拒 JSON null——修复+回归锚固化）；
  race ×3 包绿（cloudcore TestSetpointFlushOnce 整包偶发复跑绿判非回归）；全量
  e2e 见 RN N5。
- 灾备评估：docs/DR-ASSESSMENT.md（现状第 2 级/差距 6 项/v0.52 承接）。
- 复核与推送：复核 PASS（P2×1+P3×4 已处置：快修 3/留档 1，回执 .cluster/edgeflow-v0510/review.md）；处置后定向 e2e run6 绿 + 全量 e2e 772.693s 绿，双提交推送。
