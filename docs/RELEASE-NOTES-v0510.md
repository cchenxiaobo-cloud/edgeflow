# EdgeFlow v0.51.0 发布说明（区域协同层落地）

> 版本：v0.51.0 ｜ 日期：2026-10-21 ｜ spec：0024-hierarchy ｜ 缺口：G01（区域协同管理层）+ 灾备口径评估
> 基线：v0.50.0（f016df7）。**本版为纯云端管理面版本：无新增边云消息（消息数保持 19）。**

## N1 能力

- **路径裁决（spec 0024 裁决 1）**：中心直管架构下提供「区域级视图 + 区域级策略 +
  数据汇聚」能力面（replan 三候选路径之路径 3 增强）；多实例联邦/区域管理节点待
  v0.52 规模数据后专项立项。三级语义：中心 = 本 cloudcore；区域 = regionstore 树；
  节点 = registry。
- **三级树形汇总**：`GET /api/v1/hierarchy/summary` —— 全树形递归聚合（子区域
  nodeCount/onlineCount/alarmActiveCount 自底向上并入父）；根虚拟节点（name=center）
  另含 `unassignedCount`（未归属注册节点独立桶）；`policySet` 联动区域策略状态。
- **区域节点明细**：`GET /api/v1/hierarchy/regions/{id}/nodes` —— 含子树递归的
  节点明细（nodeId/nodeName/region/online/ip）；未知区域 404。
- **告警汇聚**：`GET /api/v1/hierarchy/alarms` —— 按节点归属区域聚合计数
  （raised/acked/assigned/closed 四态 + total）；未归属节点告警入 unassigned 桶；
  扫描上限 2000（超限偏差登记 KI）。
- **区域策略 KV**：Region.Policy（≤4KB 合法 JSON 对象）—— GET/PUT
  `/api/v1/hierarchy/regions/{id}/policy`；PUT 幂等 upsert；**JSON null / {} =
  清除**（e2e run4 抓到类型守卫误拒 null 后修复 + 单测回归锚）；非对象/超限 400；
  未设置读取返回 null。策略本版仅存储与视图透出，不下发执行（非目标）。
- **灾备评估报告**：`docs/DR-ASSESSMENT.md` —— GB/T 20988-2025 六级对标、现状
  恢复能力盘点（RTO/RPO 逐组件评估）、差距清单 6 项、版本承接（v0.52 承接 2-3 级
  落地项；5 级待规模数据后专项）。诚实性声明内嵌（未实测项登记为差距）。

## N2 兼容

- 端点 114→119（+5，全读面 + 1 写面）：见 API-COMPATIBILITY §1 矩阵 115-119 与
  v0.51.0 增量节。
- 消息 19 不变；契约四件套同步（routes/API-SPEC §1.1/API-COMPATIBILITY/契约测试
  扫描清单 + hierarchy 探测分支）。
- **RBAC**：read=viewer+ / write=operator+；**本面无 DELETE 端点**——设计上规避
  operator 无 delete 语义（v0.46 矩阵只增不改，v0.50 教训前置落实）。
- regionstore 快照 schema 只增字段（Region.Policy，omitempty）：旧快照 Load 默认
  nil，向后兼容。
- 在线判定以 registry NodeInfo.Status 为唯一权威（Ready/Unknown=在线侧），
  本版不自建心跳窗口。

## N3 测试（新增 10 例 + e2e 六段）

- `cloud/pkg/hierarchyview/v0510_test.go` 9 例：树形递归聚合（含 unassigned 独立桶
  口径）/告警 rollup（closed 不计）/在线判定权威（Unknown 在线侧）/策略标记联动/
  子树明细（含未知区域 404）/告警矩阵四态计数/全空库/扫描上限传递。
- `cloud/pkg/regionstore/v0500_test.go` +1（TestV0510PolicyNullClear）：null/{}
  清除语义回归锚（e2e run4 缺陷修复后固化）。
- e2e `TestV0510HierarchyE2E` 六段（**真实 edgecore 注册**，replan 验收锚「三级视图
  演示」）：US-1 注册→归属→递归聚合（轮询真节点上线）→ US-2 子树明细+404 →
  US-3 告警空态结构 → US-4 策略全路径（upsert/回读/summary 联动/null 清除/非对象
  400/超限 400/未知 404）→ US-5 越权（viewer PUT 403/匿名 401/viewer 读 200）→
  US-6 幂等（admin 写面）。
- 复核处置（PASS，P2×1+P3×4）：P2 spec 裁决 3 字段名 active→raised（文档对齐）；
  P3 快修 3——Summary() online map 死代码清理、normalizePolicy UseNumber 大整数
  保真、AlarmStats 遍历 `_, s` 风格；P3 留档 1——GET policy 透出手改快照无防御
  （KI §52#7）。处置后复验：两 store 单测绿 + run6 **PASS 10.967s**。
- e2e 修复轨迹：run1 Unmarshal 残留（循环内复用 struct + unassignedCount omitempty
  省略时保留旧值 → 条件永假——改循环内 fresh 变量）→ run3 空态 regions null
  （API 统一空数组）→ run4 SetPolicy 误拒 JSON null（真缺陷：类型守卫未放行
  null 清除语义——修复 + 单测锚）→ **run5 PASS 7.684s**。

## N4 边界（已知限制，详见 KI §52）

- 告警扫描上限 2000：超限节点计数偏差（实时聚合量级假设；缓存化/分页留 v0.52
  按规模实测定）。
- 策略仅存储与透出，不下发执行（非目标明示）。
- 实时聚合非同一快照（各 store 独立锁）：单次调用内计数微偏差可接受（包注释声明）。
- 灾备差距 6 项（DR-ASSESSMENT §3）：单实例/嵌入式 etcd 单点/快照 env 依赖/演练
  流程缺失/备份副本管理/长跑未实测——v0.52 承接。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race`：hierarchyview / regionstore 首轮绿；cmd/cloudcore 复跑
  **20.396s** 绿（TestSetpointFlushOnce 整包偶发，单跑 race 绿 + 复跑绿，判非
  回归——TestV0430 同型处置）。
- 契约：**119 条复验 13.656s** 绿（114→119 + 探测分支）。
- 定向 e2e：`TestV0510HierarchyE2E` **PASS 7.684s**（run5）→ 处置后 run6 复验 **PASS 10.967s**。
- 全量 e2e（处置后终验）：**ok 772.693s**（.cluster/edgeflow-v0510/e2e-full-v0510.log）。

## N6 复核（15min 硬时限）

- **PASS（6m37s）** → 处置后放行：P2×1——spec 0024 裁决 3 告警矩阵字段名 `active` 与实现 JSON `raised` 不一致，spec 一行对齐（发布面 API-SPEC/API-COMPATIBILITY 未枚举字段名，无需改代码）。
- P3×4——快修 3：Summary() `online` map 死代码清理 / normalizePolicy `UseNumber()` 大整数保真（>2^53 字面量不再静默改写）/ AlarmStats 遍历 `for _, s` 风格；留档 1：GET policy 直接透出快照原文、Load 侧不校验合法性（KI §52#7，正常写路径已 normalize）。
- 复核执行记录：`go build ./...` 零错误 / vet 零告警 / 契约端点实数 119 对齐 / RBAC 映射（GET=viewer+、PUT=operator+、无 DELETE）与声明一致 / 聚合 rollup·子树判定·策略语义·e2e 断言扎实性·DR-ASSESSMENT 诚实性逐项核查通过（回执 .cluster/edgeflow-v0510/review.md，含 review-original.md 全文）。
