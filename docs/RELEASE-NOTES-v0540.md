# EdgeFlow v0.54.0 发布说明（三场景端到端验收）

> 版本：v0.54.0 ｜ 日期：2026-10-21 ｜ spec：0027-scenarios ｜ 缺口：G29 三场景端到端联调
> 基线：v0.53.0（2c8ee9d）。**本版零 API 变更（契约 128 维持、消息 19 不变）。**

## N1 能力

- **三条场景级 e2e**（真实 cloudcore+edgecore 进程 + RBAC 三凭证，
  tests/e2e/scenario_e2e_test.go）：
  - **场景 A 告警通知闭环**：阈值规则触发真告警（critical）→ hierarchy summary
    区域 alarmActiveCount 递归聚合可见 → webhook 送达接收端 + HMAC-SHA256 签名
    逐字节校验。视频源用告警事件等价（等价声明见 SCENARIO-E2E.md；视频接入链路
    已有独立 e2e v0.42–v0.44）。
  - **场景 B 维护预警闭环**：warning 预警 → ack → assign（维护班 + 工单号
    GD-SCEN-B）→ close；区域×状态告警矩阵三态迁移逐段可见。
  - **场景 C 优化设定值闭环**：建单（requireApproval）→ approve → 边缘执行 →
    SetpointResult 回告 applied；第二单 reject → rejected 留痕终态。
- **验收文档 docs/SCENARIO-E2E.md**：三场景输入/链路/断言/通过判据 + 验收记录 +
  等价物声明。
- **R1 复核处置**（v0.52+v0.53 合并独立复核 **PASS**，P2×1+P3×4，无 P1）：
  - P2：在制品 scenario 测试补齐 `hmacHex`（签名头期望值计算）——本版收尾。
  - P3 快修 3：forwardstore 台账 ID 随机化（`dl-<ms>-<随机 hex>`，同毫秒重号
    规避+注释修正）/ drstore Create 自检失败 Warnf 告警（不改变返回语义）/
    envDRKeep 措辞对齐（≤0→默认 7、>100→收敛 100）。
  - P3 留档 1：台账持久化写放大（KI §55#1）。

## N2 兼容

- 端点 128 不变、消息 19 不变；契约四件套零改动。
- 复核快修不改变任何 API 行为（台账 ID 形态仅审计可读性；drstore 仅加日志）。

## N3 测试（新增 3 场景 e2e + 处置复验）

- `TestScenarioAAlarmNotifyLoop` **10.19s** / `TestScenarioBMaintenanceLoop`
  **7.49s** / `TestScenarioCSetpointLoop` **7.30s**（run1 全绿，真实进程）。
- R1 处置后复验：`go test -race` drstore **2.045s** / forwardstore **5.644s** 绿。
- e2e 轨迹：R1 复核期间在制品缺 hmacHex（P2-1）→ 本版补齐后 run1 一次通过。

## N4 边界（已知限制，详见 KI §55）

- 送达台账持久化写放大（每次投递终态全量序列化快照；量级小 + 环形 100 封顶）。
- 场景告警触发依赖 mock 传感器穿越频率（轮询上限 90s，实测 ~10s 量级）。
- 通知渠道仅 webhook 形态；场景 A 视频源用告警事件等价。

## N5 门禁

- `go build ./...` / vet（drstore/forwardstore/tests/e2e）/ gofmt：净。
- `go test -race`：drstore **2.045s** / forwardstore **5.644s** 绿。
- 契约：128 条（R1 复验 **12.99s** 绿；本版零契约变更）。
- 场景 e2e：`TestScenario×3` **全绿 26.145s**（run1）。

## N6 复核

- **R1（v0.52+v0.53 合并复核）：PASS**——P2×1+P3×4 全部处置完毕（回执
  .cluster/edgeflow-v052-056/review.md，原始报告 subagent_R1_review.md）。
- R2（v0.54–v0.56 合并复核 + 全量 e2e 终验）随 v0.56 执行。
