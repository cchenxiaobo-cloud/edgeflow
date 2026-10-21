# EdgeFlow v0.56.0 发布说明（审计台账查询面）

> 版本：v0.56.0 ｜ 日期：2026-10-21 ｜ spec：0029-audit-query ｜ 缺口：安全运维收口（WBS 7.5 查询面）
> 基线：v0.55.0（7d78b30）。**本版为纯云端管理面只读版本：零新增消息（19 不变），端点 128→130（+2 全读面）。**

## N1 能力

- **台账查询**（GET /api/v1/audit/entries，viewer+）：operator 精确 / method 精确
  （大小写敏感）/ pathSub 子串 / status 精确 / from-to 毫秒窗（含边界）六维过滤 +
  Ts 降序分页（limit<=0 默认 50 钳 [1,500]）；空台账/无命中返回空数组非 null。
- **台账聚合**（GET /api/v1/audit/stats，viewer+）：与 Query 同一过滤语义；
  总数 / 按操作者 / 按动作（方法+路由模式）/ 状态桶（2xx/4xx/5xx/other）。
- **实现语义**（spec 0029 裁决 2）：文件扫描（JSONL 逐行，半行/损坏行跳过不毒化
  其余记录）；量级口径 = 管理面请求频率 × 30 天保留 = KB~MB 级，文件扫描成立；
  内存索引/游标留量级实测后立项（KI §57）。
- **记录面零改动**：ledger.Middleware/Record 追加写语义不变；台账追加写不可变，
  无写面/无 DELETE——retention 30 天自动清理（既有语义）。

## N2 兼容

- 端点 128→130（+2，全读面）：见 API-COMPATIBILITY §1 矩阵 129-130 与 v0.56.0
  增量节。
- 消息 19 不变；契约四件套同步（routes/API-SPEC §1.1/API-COMPATIBILITY/契约测试
  扫描清单 + /api/v1/audit 前缀并入治理面探测语义）。
- **RBAC**：read=viewer+；无写面/无 DELETE。
- audit 包既有调用方零感知（新增 Query/Stats/QueryFilter/Stats 为纯增量）。

## N3 测试（新增 3 例 + e2e 三段）

- `cloud/pkg/audit/v0560_test.go` 3 例：过滤器六维（含损坏行跳过/降序/大小写
  敏感）/分页（末页/越界空非 nil/limit 钳）/聚合（操作者×动作×状态桶 + 过滤
  语义一致性）。
- e2e `TestV0560AuditQueryE2E` 三段：US-1 产生真实审计记录（建区域 201 + 查未知
  404）→ entries 轮询可见（operator/status 过滤生效）→ US-2 stats 聚合
  （byOperator/byStatus 桶）→ US-3 RBAC（匿名 401 / viewer 200）。

## N4 边界（已知限制，详见 KI §57）

- 查询为全文件扫描（无索引/游标）——台账量大时线性增长（量级口径内成立）。
- 无导出下载端点（entries 分页 + 文件本身可 tail/grep/jq 已覆盖）。
- 损坏行静默跳过（崩溃残留半行不计入查询结果）。

## N5 门禁

- `go build ./...` / vet（audit/cloudcore/contract/e2e）/ gofmt：净。
- `go test -race -count=1 ./cloud/pkg/audit/`：**ok 2.119s**。
- 契约：**130 条复验 14.525s** 绿（128→130 + /api/v1/audit 探测）。
- 定向 e2e：`TestV0560AuditQueryE2E` **PASS 2.88s**（run2；run1 用错 404 端点
  —— regions 无单查 GET 返回 405，改 hierarchy 未知区域节点 404 语义端点）。

## N6 复核

- R1（v0.52+v0.53）**PASS** 已处置（回执 .cluster/edgeflow-v052-056/review.md）。
- **R2（v0.54–v0.56 合并复核）：PASS**（无 P1/P2，P3×3 留档——QoS2 跨相迟到
  PUBREC 窄窗口 / audit 参数静默归零 / US-4 帧数断言增强，登记 KI §57#4-6；
  复核实测 race mqtt 6.0s/audit 3.0s/drstore 1.5s/forwardstore 5.0s + 契约
  130 条 13.1s 全绿；报告 subagent_R2_review.md）。
- 全量 e2e 终验随 R2 收口执行（结果回填 .cluster/edgeflow-v052-056/e2e-full-v0560.log）。
