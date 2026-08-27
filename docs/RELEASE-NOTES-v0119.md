# EdgeFlow v0.19.0 发布说明（发布面智能运维第二批：failureBudget 运行中可调 + 发布审计快照 + 全局发布查询）

- **发布日期**：2026-08-27
- **版本基线**：v0.18.0 → v0.19.0
- **主题**：审计取证从拼装到一键、全局运维从循环到单查、失败预算从创建期锁定到运行中可调
- **兼容性**：HTTP 端点 38→**40** 只增不改；PATCH 白名单扩展复用既有端点；全部新参数 opt-in 缺省行为与 v0.18.0 逐字节一致；升级零迁移；老边缘零动作；**零新依赖**

## 一、核心能力

### 1. failureBudget 运行中可调（SI-A）
- PATCH `/api/v1/models/{m}/releases/{id}` 白名单扩展 `failureBudget`——与 batchSize/pauseBetween/failFast 同一运维动线、同批边界生效语义
- 改小→下一批后立即适用（剩余批次的刹车线）；`=0` 运行中关闸禁用自动暂停；值域 [0,10000] 护栏；终态仍 409
- 起算口径登记：AutoPause 判定读当前 head 预算+派生 failed 计数，"自当下起的剩余批次"非全程回溯

### 2. 发布审计快照（SI-B）
- `GET /api/v1/models/{m}/releases/{id}/snapshot`：一次拉全 kind=ReleaseSnapshot + generatedAt + release 头（含 events 时间线）+ summary 六计数实时现算 + nodes 恒非 nil
- 校验链 GetModel(404) 先行 → GetRelease(404) → 跨模型引用钉 head.Model 防目录穿越式枚举
- 明示非承诺语义：generatedAt 后写入不在快照内

### 3. 全局发布查询（SI-C）
- `GET /api/v1/releases`：跨模型聚合全部发布头（与 v0.18.0 /api/v1/deployments 对偶）
- status 七态逗号多值过滤（非法值 400）；limit 缺省 100 上限 500 + offset≥0；X-Total-Count 报过滤后总数；CreatedAt 降序稳定 tie-break by ID

## 二、验证摘要（实测）

- 全量 `go test -race ./...` **两轮多包全绿（37 包 0 FAIL）**；build/vet 干净；gofmt 触碰文件清零；lint 仅剩未触碰文件历史遗留
- 新增测试：V190 API ×3（PATCH budget 校验/混合补丁/关闸/终态 409/404；snapshot 全景与三类 404 链序钉住；全局端点排序/tie-break/多值过滤/分页边界/空库形态）+ **TestV190ReleaseIntelE2E 云端全链路 15.8s**（真实 cloudcore 子进程：预算创建→终态收敛→snapshot 全景断言→全局查询命中→终态过滤命中→400 参数族）
- 契约守卫三处联动绿：契约表 38→40（含 v0.17.0 Note 更新）、路由计数 24→26 与发布族 9→11 断言随版更新、API-SPEC §1.1 补行 ×2

## 三、升级注意

- **零迁移**：两个新端点均为只读查询，无键空间/schema 变化
- 行为差异面仅显式调用新端点或 PATCH failureBudget 时存在；缺省行为逐字节不变（budget 缺省 0、既有三参数补丁不受影响）
- 回滚 = 回退二进制即可；L7 网关若有路径策略需放行两条新 GET（均挂 Bearer Token 认证）

## 四、遗留（非阻断，见 KNOWN-ISSUES §19）

- 快照非承诺语义（generatedAt 快照口径已明示）；超大发布节点结果以分页列表为准
- AutoPause 剩余批次起算口径（非全程回溯）登记在案
- events 时间线仍是环形 32 条（v0.18.0 口径延续）
