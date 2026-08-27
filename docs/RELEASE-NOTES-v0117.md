# EdgeFlow v0.17.0 发布说明（发布任务运维深化：运行中可调参数 + 列表 status 过滤 + dryRun 预检）

- **发布日期**：2026-08-27
- **版本基线**：v0.16.0 → v0.17.0
- **主题**：灰度发布面操作纵深收口——任务跑起来之后"看得准（过滤）、调得动（PATCH）、试得了（dryRun）"
- **兼容性**：HTTP 端点 36→**37** 只增不改；status 过滤与 dryRun 复用既有端点不新增路由；全部新参数 opt-in 缺省行为与 v0.16.0 逐字节一致；升级零迁移；老边缘零动作；**零新依赖**

## 一、核心能力

### 1. 发布运行中可调参数（RO-A）
- `PATCH /api/v1/models/{m}/releases/{id}`：部分更新语义（全指针字段，未提供保持不变）
- 可改 `batchSize` / `pauseBetween` / `failFast`；身份段（Version/Target/TargetNodes）不可变
- 生效语义：控制器 BuildBatches 每轮按最新 head 重切批次 → **下一批边界生效**，不中断在途批、无追溯歧义
- pending/running/paused 均可改（暂停期观察→调整→resume 运维动线）；终态 409；CAS 安全（复用 UpdateReleaseHead，重试 ≤3）

### 2. 发布列表 status 过滤（RO-B）
- `GET .../releases?status=running,pending`（逗号多值，合法枚举校验失败 → 400）
- 与 limit/offset 正交：先过滤后分页；`X-Total-Count` 报过滤后总数

### 3. dryRun 预检（RO-C）
- 创建请求体 `dryRun:true`：全量走真实校验链（模型存在/内容校验/窗口钟漂护栏/版本 active/镜像探活/节点物化）+ guard 等价只读判定
- 响应 200 + `{kind:"DryRunPreview", wouldCreate, blockReason, targetNodes, prevActive, inFlightReleaseId}`
- **零落盘、零 guard 键、零 perNode 预写**；错误响应码与真实创建同因同报
- 口径明示：预检结论为 TOCTOU 快照非承诺语义——真实创建以 CreateRelease guard CAS 兜底

## 二、验证摘要（实测）

- 全量 `go test -race ./...` **37 包两轮全绿**；build/vet 干净；本轮触碰文件 gofmt/lint 清零（存量历史遗留文件沿用"不扩大战线"裁决）
- 新增测试：V170 API 契约 ×3（PATCH 全字段/部分字段/非法值/终态 409/404；status 单值/多值/全枚举/非法值；dryRun 零落盘/非 active blockReason/缺省路径不受污染）
- **TestV170ReleaseOpsE2E 云端全链路通过（真实 cloudcore 子进程，20.2s）**：dryRun 预检零落盘 → 窗口发布 pending 期 PATCH 放慢节奏 → 到点自动 running → pause → paused 期 PATCH batchSize → resume 收敛 succeeded → status 过滤断言
- 缺省行为零变化实证：全部既有用例不改一行仍绿（路由计数断言随版更新惯例 22→23、契约表 36→37）

## 三、升级注意

- **零迁移**：无键空间/schema 变化；老边缘零动作；云端重启即得新能力
- PATCH 端点为新方法动词（v0.7.0 以来首个非 GET/POST/PUT/DELETE），反代/网关需放行 PATCH 方法（若有 L7 策略）
- 行为差异面仅显式使用新端点/新参数时存在；回滚 = 回退二进制即可

## 四、遗留（非阻断，见 KNOWN-ISSUES §17）

- 身份段不可变维持裁定（变更目标 = 取消重建）；批量补建独立发布暂无必要
- wouldCreate 非承诺语义（TOCTOU 快照已标注）；如需预约锁语义需引入预占键（不做——复杂度不值）
