# spec 0025 — 规模·韧性·基线（v0.52.0，G05 承接 + DR 差距落地 + NFR-04）

> 版本：v0.52.0 ｜ 缺口：G05（规模/性能基线）+ DR-ASSESSMENT 差距 #3/#4/#5 承接 ｜
> 上游：spec 0024（v0.51 区域协同层——告警聚合的地基）
> replan 验收锚：规模与基线（按本环境可达项裁剪，压测/信创以脚本机制承接）

## 背景与范围裁决

replan v0.52 原范围（100 节点压测 / 30min 长跑 / 麒麟统信验证 / C86 / HA 评估）
依赖真实硬件与 OS 环境，本版在当前开发环境内交付「可达的规模化与韧性增量」，
环境依赖项以脚本机制 + 文档基线承接（如实登记，不冒充实测）：

**裁决：本版交付四件——仓库卫生、告警聚合规模化（修 KI §52#1/#4）、灾备备份
管理面（DR 差距 #5）、演练 SOP + 分级配置基线 + 性能基线刷新机制（DR 差距
#4/#3 + NFR-04）。**

## 裁决

1. **仓库卫生**：移除 tests/e2e/data 运行时残留（61MB etcd WAL 误提交，
   GitHub >50MB 警告）；.gitignore 增加 /tests/e2e/data/。
2. **告警聚合规模化**：alarmstore 增 `CountByNodeState() map[nodeID]map[state]int64`
   （单锁单遍 O(n)，无 List 的 2000 截断）；hierarchyview 增可选接口
   `AlarmCountSource`，Summary/AlarmStats 优先走计数快照、未实现接口的替身
   回退 List 既有路径（只增不改，v0.51 行为不变）；KI §52#1/#4 修复。
3. **灾备备份管理面**：新包 cloud/pkg/drstore——备份对象 = 已配置治理面快照
   文件（region/tag/logstore，main.go 装配注入）；备份形态 `<DR_DIR>/<id>/`
   子目录（manifest.json：文件清单 + sha256 + 字节数 + Verified 位）；Create
   自检 + Verify(id) 复核（结果回写 manifest）；轮转保留 DR_KEEP 份（默认 7，
   钳 [1,100]）；**不设 API 恢复面**（恢复走演练 SOP，防误操作放大面）；
   etcd 数据面快照归演练脚本（本包范围外）；id 字符集 [0-9a-zA-Z-] 防路径穿越。
4. **DR API 3 端点**（119→122）：POST /api/v1/dr/backups（operator+；未启用 503）、
   GET /api/v1/dr/backups（viewer+；{enabled,backups}）、GET /api/v1/dr/backups/{id}
   （viewer+；?verify=true 触发复核）。**无 DELETE 面**（轮转自动）；
   manifest 损坏条目跳过不毒化清单。
5. **演练 SOP + 脚本**：docs/DR-RUNBOOK.md（恢复预案 SOP：备份/校验/恢复/验证
   四步 + etcd snapshot 路径）+ scripts/dr-drill.sh（backup/verify/restore 三模式编排）。
6. **分级配置基线**：docs/CONFIG-BASELINE.md——轻/标/重三档（单机演示/标准站点/
   规模部署），含强制持久化 env 清单（DR 差距 #3）、外部 etcd 集群部署基线
   （差距 #2）、认证/令牌/DR_DIR 强制项。
7. **性能基线刷新机制（NFR-04）**：scripts/bench-baseline.sh + Makefile
   `bench-baseline` 目标——跑 hack/collect-bench 输出吞吐/延迟，按模板更新
   docs/PERFORMANCE-BASELINE.md（时间戳 + 环境说明 + 数字），机制可重复执行
   （人工触发，CI 化留档不承诺）。
8. **兼容**：端点 119→122（+3）；消息 19 不变；既有端点契约零改动；
   alarmstore/hierarchyview 变更对既有调用方只增不改（可选接口回退）。
9. **非目标**：100 节点真实压测与 30min 长跑（脚本机制承接，结果待环境）、
   信创 OS/C86 验证（待环境）、多实例联邦/区域管理节点（待规模数据立项）、
   API 恢复面、异地副本自动同步、etcd snapshot 纳入 API 备份对象。

## 端点（+3，1 写面 2 读面）

| Method | Path | 语义 | RBAC |
|---|---|---|---|
| POST | /api/v1/dr/backups | 创建治理面快照备份 | operator+ |
| GET | /api/v1/dr/backups | 备份清单 {enabled,backups} | viewer+ |
| GET | /api/v1/dr/backups/{id} | 备份详情（?verify=true 复核） | viewer+ |

## 数据依赖

- drstore（自包含文件面）｜region/tag/logstore 快照 env（备份源清单）。
- 告警聚合：alarmstore.CountByNodeState（新）← hierarchyview（Summary/AlarmStats）。

## 交付锚

- 实际交付（2026-10-21）：单测 9 例（drstore 5 + hierarchyview 2 + alarmstore 2，
  race 全绿）；契约复验 13.742s 绿（122 条，119→122 + /api/v1/dr 探测分支）；
  定向 e2e run3 PASS 6.92s（run1/run2 US-5 测试账目修正——创建份数<keep 轮转
  永不触发）；全量 e2e 待 v0.56 后 R2 终验。
- 仓库卫生：tests/e2e/data 61MB etcd WAL 移除 + .gitignore（GitHub >50MB 警告
  归零）。
- 文档交付：docs/DR-RUNBOOK.md（演练 SOP）+ docs/CONFIG-BASELINE.md（轻/标/重
  三档强制项）+ bench-baseline 机制（Makefile + PERFORMANCE-BASELINE 分节追加）。
- 复核安排：v0.52+v0.53 合并 R1 独立复核检查点，结论回填
  .cluster/edgeflow-v052-056/review.md。
