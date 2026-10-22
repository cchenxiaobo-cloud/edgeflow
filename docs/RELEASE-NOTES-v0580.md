# EdgeFlow v0.58.0 发布说明（告警静默窗口）

> 版本：v0.58.0 ｜ 日期：2026-10-22 ｜ spec：0031-alarm-mutes ｜ 缺口：告警治理运维增强
> 基线：v0.57.0（cb89e47）。**本版为纯云端管理面只增版本：零新增消息（19 不变），端点 130→133（+3）。**

## N1 能力

- **静默窗口**（新包 cloud/pkg/mutestore）：{id, nodeId(可选，空=全部节点),
  from/to 毫秒窗（含边界）, reason ≤256B, createdBy, createdAt}；JSON 快照
  持久化（EDGEFLOW_CLOUDCORE_MUTE_FILE，空=内存，治理面惯例）。
- **窗口约束**：必须有界（to > from 且跨度 ≤30 天，防永久静默）；不可变
  （无 PUT——调整 = 删除重建）；总数 ≤200；过期惰性失效（active 实时计算，
  快照保留历史留痕）。
- **活跃口径**：hierarchy summary alarmActiveCount 排除生效中的静默节点告警
  （MuteSource 可选接口，SetMutes 启动期装配；未装配行为不变——v0.51 行为锚）。
  告警历史数据（/api/v1/alarms 列表）与 AlarmStats 四态计数不动（生命周期事实）。
- **API 3 端点（130→133）**：GET 清单（viewer+；每条附 active 实时标记）/
  POST 创建（operator+；durationSec 1..2592000）/ DELETE（admin）。

## N2 兼容

- 端点 130→133（+3）：见 API-COMPATIBILITY §1 矩阵 131-133 与 v0.58.0 增量节。
- 消息 19 不变；契约四件套同步（routes/API-SPEC §1.1/API-COMPATIBILITY/契约测试
  扫描清单 + /api/v1/alerts 前缀并入治理面探测语义）。
- **RBAC**：read=viewer+ / write=operator+ / delete=admin。
- hierarchyview 变更**只增不改**：MuteSource 为可选接口（SetMutes 未装配时
  Summary 行为与 v0.57 逐字节一致——既有测试替身无感知）。

## N3 测试（新增 8 例 + e2e 三段）

- `cloud/pkg/mutestore/v0580_test.go` 5 例：创建校验（reason 超限/空窗/超
  30 天/总数上限 + 全节点合法）/Active 时间边界与 Matches 定向/Muted 判定/
  删除与 List 降序/持久化 roundtrip/上限锚。
- `cloud/pkg/hierarchyview/v0580_test.go` 3 例：静默排除（计数源路径）/
  静默排除（回退路径）+ 解除恢复 / 无静默源行为不变锚（v0.57 等价）。
- e2e `TestV0580MutesE2E` 三段（真实告警管线）：US-1 创建窗口 → 真告警入库
  （/api/v1/alarms 可见）→ summary 活跃口径 0 → US-2 admin 删除 → 活跃恢复
  计数 → US-3 RBAC（viewer POST 403 / 匿名 401 / operator DELETE 403 /
  viewer GET 200 / admin DELETE 200）。

## N4 边界（已知限制，详见 KI §59）

- 静默不作用于转发出口（通知抑制留后续版本经 mutestore 注入）。
- 无 severity 维度过滤、无周期性静默（cron 型窗口）。
- 过期窗口快照保留（留痕语义），清单以 active 标记区分。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race -count=1`：mutestore **2.408s** / hierarchyview **1.794s** 绿。
- 契约：**133 条复验**（130→133 + /api/v1/alerts 探测）。
- 定向 e2e：`TestV0580MutesE2E`（见交付锚回填）。

## N6 复核

- R1/R2 已处置收口；**R3（v0.57+v0.58 合并复核）**随本版推送后执行。
