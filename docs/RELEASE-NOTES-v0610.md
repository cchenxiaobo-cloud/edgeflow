# EdgeFlow v0.61.0 发布说明（诊断聚合面）

> 版本：v0.61.0 ｜ 日期：2026-10-22 ｜ spec：0034-diagnostics ｜ 缺口：运维可观测性 + R2 P3-2 处置
> 基线：v0.60.0（3c18dda）。**本版为纯云端管理面只增版本：零新增消息（19 不变），端点 134→135（+1 全读面）。**

## N1 能力

- **诊断聚合一屏**（GET /api/v1/diagnostics/summary，viewer+）：{now, startedAt,
  uptimeSec, goVersion, nodes{total,online}, alarms{total,active},
  regions{total}, mutes{total,active}, forwards{total,enabled},
  etcd(embedded|external), audit{path,bytes}}——运维排查不再逐端点调用。
- **口径**：节点在线对齐 v0.51（Ready/Unknown=在线侧）；告警活跃 = 非 closed
  （Stats 推导）；静默 active 实时计算（mutestore 语义）；etcd 形态按 env 分叉。
- **audit 参数校验收口（R2 P3-2 处置）**：status/limit/offset 非数字或负数 →
  显式 400（此前静默归零）——行为收紧在 API-COMPATIBILITY 增量节登记。

## N2 兼容

- 端点 134→135（+1）：见 API-COMPATIBILITY §1 矩阵 135 与 v0.61.0 增量节。
- 消息 19 不变；契约四件套同步（routes/API-SPEC §1.1/API-COMPATIBILITY/契约测试
  扫描清单 + /api/v1/diagnostics 前缀并入治理面探测语义）。
- **RBAC**：read=viewer+；无写面。
- audit 参数语义收紧：此前静默容忍的非法参数现在 400（正常调用不受影响——
  合法参数语义零变化）。

## N3 测试（e2e 两段）

- e2e `TestV0610DiagnosticsE2E` 两段：US-1 summary 结构与聚合计数（造区域+
  静默后 regions=1/mutes=1+active=1/startedAt≤now）→ US-2 audit 三组非法参数
  显式 400（limit=-5/offset=abc/status=-1）。

## N4 边界（详见 KI §62）

- 聚合非同一快照（各 store 独立锁，单次调用计数微偏差——对齐 hierarchyview 口径）。
- 进程启动时点用包初始化近似（非精确 main 入口）。
- etcd 形态字段先行（集群成员/延迟深探留后续）。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- 契约：**135 条复验**（134→135 + /api/v1/diagnostics 探测）。
- 定向 e2e：`TestV0610DiagnosticsE2E` **PASS 3.18s**（run1 一次通过）。

## N6 复核

- **R4（v0.60+v0.61 合并复核 + 全量 e2e 终验 -timeout 30m）**随本版推送执行；
  R3（v0.57–v0.59）复核随 v0.59 推送已并行启动（报告 .cluster/edgeflow-v057-061/）。
