# EdgeFlow v0.65.0 发布说明（审计导出与转发规则导出/导入）

> 版本：v0.65.0 ｜ 日期：2026-10-22 ｜ spec：0038 ｜ 缺口：运维数据迁移与合规导出
> 基线：v0.64.0（16e0ecd）。**本版为纯云端管理面只增版本：零新增消息（19 不变），端点 134→137（+3）。**

## N1 能力

- **审计台账导出**（GET /api/v1/audit/entries/export，operator+——含 IP 敏感
  信息）：JSONL 原样流式下载（Content-Disposition 附件）；台账文件不存在返回空。
- **转发规则脱敏导出**（GET /api/v1/forwards/export，admin）：规则 JSON 导出
  （Secret/MqttPassword 脱敏——hasSecret/hasMqttPass 代替回显）；exportedAt 时间戳。
- **转发规则导入**（POST /api/v1/forwards/import，admin）：批量导入规则（逐条
  校验，失败条目跳过计数；ID 已存在跳过）；返回 {imported, skipped, errors}。

## N2 兼容

- 端点 134→137（+3）：见 API-COMPATIBILITY §1 矩阵 135-137 与增量节。
- 消息 19 不变；RBAC 对齐 v0.46 矩阵（audit export=operator+ 含 IP 敏感信息；
  forwards export/import=admin）。
- Rule schema 零改动（导入走 Create 既有校验链）。

## N3 测试

- e2e 回归：TestV0630MutedMarkE2E + TestV0590ForwardsTestE2E 复跑绿（既有端点
  零回归）。
- forwardstore.Import 单测：正常导入/ID 去重跳过/校验失败跳过。

## N4 边界（详见 KI §67）

- 审计导出全文件一次性写入（30 天保留+管理面量级假设）。
- 转发规则导入 Secret 明文传输入口（同创建语义；HTTPS/内网场景）。
- 导入不校验 secret 强度（与 Create 同语义）。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race -count=1`：mutestore/forwardstore 全绿。
- 契约：137 条复验（134→137 + /api/v1/audit/entries/export + /api/v1/forwards/export + /api/v1/forwards/import 探测）。
- e2e 回归：v0630 + v0590 复跑绿。

## N6 复核

- R5（v0.62–v0.65 合并复核）随本版推送后执行；R6（v0.66 + 全量 e2e 终验）随
  v0.66 执行。
