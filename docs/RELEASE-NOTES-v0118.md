# EdgeFlow v0.18.0 发布说明（发布面智能运维：失败预算自动暂停 + 发布事件时间线 + 全局部署影子查询）

- **发布日期**：2026-08-27
- **版本基线**：v0.17.0 → v0.18.0
- **主题**：从被动盯盘到主动干预——失败自动刹车站住等人、流转全程留痕可查、跨模型影子一眼聚合
- **兼容性**：HTTP 端点 37→**38** 只增不改；failureBudget/events 复用既有端点；全部新字段 opt-in 缺省行为与 v0.17.0 逐字节一致；升级零迁移；老边缘零动作；**零新依赖**

## 一、核心能力

### 1. 失败预算自动暂停（SO-A）
- 创建参数 `failureBudget`（≥1 启用；0=缺省=禁用）：批完成后 failed 计数达预算且发布未终态 → **自动 pause**
- 复用 v0.16.0 paused 状态机与 guard 语义：AutoPausedAt 标记 + autopause 事件 + 人可介入后 resume 续跑剩余批次
- 与 failFast 的语义边界：failFast=true 首败即中止置 failed（无累计窗口），预算只在 failFast=false 跑完判定模式下生效

### 2. 发布事件时间线（SO-B）
- `ModelRelease.Events`：created / claimed / paused / resumed / cancelled / terminal / autopause / batch_done / rollback_requested 开放事件集
- 追加发生在 UpdateReleaseHead mutate 闭包内 = **CAS 保护下并发不丢**；环形上限 32 条丢最旧保最新
- 随 release 详情返回；随 export/import 目录快照迁移（审计链不因环境切换断裂）

### 3. 全局部署影子查询（SO-C）
- `GET /api/v1/deployments`：跨模型聚合全部影子，model/nodeID 精确过滤可选
- 过滤后分页、X-Total-Count 同步；先 Model 再 NodeID 双字典序确定性排序
- 既有 per-model 端点不动；Memory/Etcd 双实现同一读路径（etcd 走 watch 缓存）

## 二、验证摘要（实测）

- 全量 `go test -race ./...` **两轮 37 包全绿**；build/vet/gofmt 干净（触碰文件清零）；lint 仅剩未触碰文件历史遗留
- 新增测试：V180 API ×5（budget 校验/透传持久化/时间线响应形态/环形截断口径/全局端点过滤分页）+ 控制器汇总计数 ×2 + **TestV180ReleaseOpsTimelineE2E 云端全链路（15.8s）**：created→batch_done/terminal 时间线断言、全局 deployments nodeID 过滤观察到真实边缘上报
- 契约守卫三处联动绿：契约表 37→38、路由计数断言 23→24、API-SPEC §1.1/§7.8 与 API-COMPATIBILITY §1 补行
- 缺省行为零变化实证：failureBudget 缺省 0 零开销跳过、Events 缺省 nil 不序列化；全部既有用例不改一行仍绿

## 三、升级注意

- **零迁移**：无键空间/schema 变化（Events 内嵌 release 头键，新旧代码可互读——旧代码忽略未知字段）
- 行为差异面仅显式使用 failureBudget≥1 时存在；全局 deployments 为新查询面无副作用
- 回滚 = 回退二进制即可；多副本无需额外动作

## 四、遗留（非阻断，见 KNOWN-ISSUES §18）

- 预算创建后只读（如需运行中调参进 PATCH 白名单，需先裁定预算属意图面还是执行面）
- 高频事件完整审计长尾依赖外层台账（键值正文环形 32 条已登记口径）
- 事件 Kind 为开放集合：消费方应按前缀匹配并对未知值容忍
