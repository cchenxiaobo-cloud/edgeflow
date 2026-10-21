# EdgeFlow v0.52.0 发布说明（规模·韧性·基线）

> 版本：v0.52.0 ｜ 日期：2026-10-21 ｜ spec：0025-dr-baseline ｜ 缺口：G05 承接（环境可达项）+ DR-ASSESSMENT 差距 #3/#4/#5 落地
> 基线：v0.51.0（0840e56）。**本版为纯云端管理面版本：无新增边云消息（消息数保持 19）。**

## N1 能力

- **仓库卫生**：移除 tests/e2e/data 运行时残留（61MB etcd WAL 误提交，GitHub >50MB
  警告）；.gitignore 增加 /tests/e2e/data/。
- **告警聚合规模化（KI §52#1/#4 修复）**：alarmstore.CountByNodeState（单锁单遍
  O(n)，无 List 的 2000 截断）+ hierarchyview 新增可选接口 AlarmCountSource——
  Summary/AlarmStats 优先走计数快照，未实现接口的替身自动回退 List 既有路径
  （v0.51 行为不变，API 响应结构零改动）。
- **灾备备份管理面（DR 差距 #5）**：新包 cloud/pkg/drstore——备份对象 = 已配置
  治理面快照（region/tag/logstore，main.go 装配注入）；`<DR_DIR>/<id>/` 子目录 +
  manifest.json（文件清单 + sha256 + 字节数 + Verified 位）；Create 自检 + Verify
  复核（结果回写 manifest）；轮转保留 DR_KEEP 份（默认 7，钳 [1,100]）；id 字符集
  白名单 [0-9a-zA-Z-] 防路径穿越；**不设 API 恢复面**（恢复走 SOP，防误操作放大）。
- **DR API 3 端点（119→122）**：POST /api/v1/dr/backups（operator+；未启用 503）/
  GET /api/v1/dr/backups（viewer+；{enabled,backups}）/ GET /api/v1/dr/backups/{id}
  （viewer+；?verify=true 触发复核）。**无 DELETE 面**（轮转自动）。
- **演练 SOP（DR 差距 #4）**：docs/DR-RUNBOOK.md（backup→verify→restore→健康验证
  四步 + 治理面/数据面两条恢复线 RPO 口径 + 轮转与异地副本边界）+
  scripts/dr-drill.sh（backup/verify/restore 三模式；restore 检测 cloudcore 存活
  拒绝执行，防写竞争）。
- **分级配置基线（DR 差距 #3 + G33）**：docs/CONFIG-BASELINE.md 轻/标/重三档
  （强制持久化 env 清单、外部 etcd 集群部署基线、认证与节点令牌强制项；演示档
  豁免声明；信创/压测待回填节如实登记）。
- **性能基线刷新机制（NFR-04）**：scripts/bench-baseline.sh + Makefile
  `bench-baseline` 目标——collect-bench 结果按日期分节追加 docs/PERFORMANCE-BASELINE.md
  （append-only，可重复执行）。

## N2 兼容

- 端点 119→122（+3，1 写面 2 读面）：见 API-COMPATIBILITY §1 矩阵 120-122 与
  v0.52.0 增量节。
- 消息 19 不变；契约四件套同步（routes/API-SPEC §1.1/API-COMPATIBILITY/契约测试
  扫描清单 + /api/v1/dr 运行时探测分支：未启用 503 合法）。
- **RBAC**：read=viewer+ / write=operator+；**无 DELETE 面**（轮转自动保留）。
- alarmstore/hierarchyview 变更**只增不改**：AlarmCountSource 为可选接口，旧
  AlarmSource 替身（含全部既有测试替身）回退 List 路径。

## N3 测试（新增 9 例 + e2e 五段）

- `cloud/pkg/drstore/v0520_test.go` 5 例：创建+manifest（缺失源跳过/副本内容/
  Get 回读）/轮转 keep=2（最旧整目录删除+倒序清单）/复核篡改（verified=false
  回写持久化+恢复后 true）/未启用与防御（ErrDisabled 全路径/穿越 id/空清单非 nil）/
  并发创建（-race，keep=10 不触发轮转）。
- `cloud/pkg/hierarchyview/v0520_test.go` 2 例：计数源路径（>scan-limit 等价场景
  计数正确 + List 零调用）/回退路径（fixtureView 行为与 v0.51 一致，limit 传递不变）。
- `cloud/pkg/alarmstore/v0520_test.go` 2 例：节点×状态精确计数（含 closed/总数一致）/
  空库空 map + 状态迁移跟随（Ack → acked）。
- e2e `TestV0520DRE2E` 五段：US-1 未启用语义（GET enabled=false / POST·详情 503）→
  US-2 创建备份（region 快照落盘轮询 → manifest+sha256+自检位+副本存在）→ US-3
  清单详情（?verify=true 复核；篡改 → verified=false 回写；未知 id 404）→ US-4
  RBAC（viewer POST 403 / 匿名 401 / viewer GET 200 / operator POST 200）→ US-5
  轮转（共 4 份创建 keep=2 → 最旧被删，保留最新 2 份）。
- e2e 修复轨迹：run1/run2 US-5 **测试自身账目错误**（共创建 2 份 < keep=2，轮转
  永不触发——补 2 次创建并错开毫秒）→ run3 **PASS 6.92s**。

## N4 边界（已知限制，详见 KI §53）

- drstore 校验语义 =「副本与 manifest 一致」，非源时点一致（备份即冻结）。
- etcd 数据面快照不在备份对象内（两条恢复线：治理面 API 备份 / 数据面 etcd
  snapshot 演练线，DR-RUNBOOK §1）。
- 恢复无 API 面（SOP 手工四步；spec 0025 裁决 3 防误操作放大面）。
- 100 节点压测 / 30min 长跑 / 信创 OS 验证未实测（机制承接，不以仿真冒充）。

## N5 门禁

- `go build ./...` / vet（drstore/hierarchyview/alarmstore/cloudcore/contract/e2e）/
  gofmt：净。
- `go test -race`：drstore **4.239s** / hierarchyview **3.662s** / alarmstore
  **3.365s** 全绿。
- 契约：**122 条复验 13.742s** 绿（119→122 + /api/v1/dr 探测分支）。
- 定向 e2e：`TestV0520DRE2E` **PASS 6.92s**（run3）。

## N6 复核

- 复核安排：v0.52 与 v0.53 **合并走 R1 独立复核检查点**（15min 硬时限），结论与
  处置回填 .cluster/edgeflow-v052-056/review.md；发现问题以 fix(review) 提交留痕。
- 本版门禁证据链：单测 race ×3 包 + 契约 122 条 + 定向 e2e run3（日志
  .cluster/edgeflow-v052-056/e2e-v0520-run3.log）。
