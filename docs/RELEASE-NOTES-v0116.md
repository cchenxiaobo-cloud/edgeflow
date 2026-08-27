# EdgeFlow v0.16.0 发布说明（AI 模型管理深化：定时维护窗口 + 发布暂停恢复 + 模型目录导出导入）

- **发布日期**：2026-08-27
- **版本基线**：v0.15.0 → v0.16.0
- **主题**：人工智能模型管理深化——把 v0.7.0 落地的模型仓库/灰度发布（F41/F42）推向运维纵深：发布可预约、可暂停观察后恢复、模型台账可迁移可灾备
- **兼容性**：HTTP 端点 32→**36** 只增不改；全部新参数 opt-in 缺省行为与 v0.15.0 逐字节一致；升级零迁移；老边缘零动作；**零新依赖**

## 一、核心能力

### 1. 定时维护窗口发布（MR-W）
- `POST /api/v1/models/{m}/releases` 新增 `notBeforeMs`（Unix 毫秒；0=立即）
- 控制器扫描窗口未到：不认领、不占领跑锁；到点自动启动
- 窗口期 InFlight 守 guard 不变（同模型不并发）；校验 ≥now-5min 钟漂护栏

### 2. 运行中发布暂停/恢复（MR-P）
- `POST .../releases/{id}/pause|resume`：running⇄paused，状态机表驱动只增不改
- 批边界生效：不中断在途下发；paused 保 active 身份续租领跑锁（多副本接管语义不变）
- resume 后 NextBatchAt 保持原节奏（PauseBetween 不重置）；paused 可直接 cancel；rollback 拒 paused

### 3. 模型目录导出/导入（MR-X）
- `GET /api/v1/models/export`：models+versions 全量快照 JSON（schemaVersion=1 + exportedAt）
- `POST /api/v1/models/import`：幂等 upsert——同 (model,version) 跳过计数；active 经 draft+activate 直通灾备语义；孤儿版本自动补建空壳模型
- releases/deployments/guards 明确不可迁移（环境绑定态）

## 二、验证摘要（实测）

- 全量 `go test -race ./...` **37 包全绿**；build/vet 干净；新增改动 golangci-lint 零告警
- 新增测试 17 例：存储层状态机与 Pause/Resume/Cancel(paused)/rollback 拒 paused ×5、控制器窗口门控与保锁续跑 ×3、API 契约守卫与幂等回环 ×3、E2E 主链路 ×1（真实 cloudcore 子进程 22.8s）+ 既有契约守卫联动
- **TestV160ReleaseWindowPauseResumeE2E 云端全链路通过**：窗口内保持 pending → 到点自动 running → pause 保锁 → resume 收敛 succeeded → export/import 幂等回环
- 缺省行为零变化实证：全部既有用例不改一行仍绿（路由计数断言按发布口径随版更新惯例 18→22、契约表 32→36）

## 三、升级注意

- **零迁移**：无键空间/schema 变化；老边缘零动作；云端重启即得新端点
- 行为差异面仅显式使用新参数时存在（notBeforeMs>0 / 调用 pause/resume/export/import）
- 多副本部署无额外动作；回滚 = 回退二进制即可（新字段 omitempty 向后兼容旧代码读取）

## 四、遗留（非阻断，见 KNOWN-ISSUES §16）

- 待调度队列观测视图（列表 NotBeforeMs 可见即可用，专门视图后续候选）
- import 大目录分页/流式（1MiB 上限内规模当前可用）
- 导出包含 deployments 的可选模式（需先定义跨环境节点映射语义）
