# spec 0029 — 审计台账查询面（v0.56.0）

> 版本：v0.56.0 ｜ 缺口：安全运维收口——审计台账自 WBS 7.5 落地以来只有追加写，
> 无查询/聚合 API（排查看文件）｜ 上游：spec 0025–0028
> 说明：本版为纯云端管理面只读版本：零新增消息（19 不变），端点 128→130（+2 全读面）。

## 背景与范围裁决

audit 包（WBS 7.5）的 Ledger 自早期版本起记录每次管理 API 调用（JSONL 追加写，
{ts,action,path,method,status,operator,ip}），但只有写入面——排查/合规检查依赖
登录机器 tail/grep。本版把台账变成一等 API 资产：

**裁决：Query（文件扫描过滤 + Ts 降序分页）+ Stats（单遍聚合：总数/按操作者/
按动作/状态桶）两个只读端点；ledger.Middleware 记录面零改动。**

## 裁决

1. **过滤语义**：operator 精确 / method 精确（大小写敏感）/ pathSub 子串 /
   status 精确 / from-to 毫秒窗（含边界）；零值字段不过滤。
2. **查询实现**：文件扫描（JSONL 逐行，半行/损坏行跳过不毒化）；Ts 降序；
   limit<=0 默认 50 钳 [1,500]；offset 越界返回空切片非 nil。量级口径：
   管理面请求频率 × 30 天保留，KB~MB 级——文件扫描成立；内存索引/游标留量级
   实测后再立项（KI）。
3. **聚合语义**：与 Query 同一过滤器；状态桶 2xx/4xx/5xx/other。
4. **RBAC**：read=viewer+；无写面、无 DELETE——台账追加写不可变，retention 30 天
   自动清理（既有语义）。
5. **只增不改**：既有 128 端点零改动；记录面（Middleware/Record）零改动；
   消息 19 不变。
6. **非目标**：台账导出下载（entries 分页已覆盖；文件本身可 tail/grep）、
   内存索引/增量游标、跨文件轮转查询、审计告警规则。

## 端点（+2，全读面）

| Method | Path | 语义 | RBAC |
|---|---|---|---|
| GET | /api/v1/audit/entries | 台账查询（过滤+分页） | viewer+ |
| GET | /api/v1/audit/stats | 台账聚合 | viewer+ |

## 数据依赖

- audit.Ledger（既有 JSONL；EDGEFLOW_CLOUDCORE_AUDIT_PATH，默认
  data/audit-ledger.jsonl）。

## 交付锚

- 实际交付（2026-10-21）：单测 3 例（过滤器/分页/聚合，race 2.119s 绿）；契约
  复验 14.525s 绿（130 条，128→130）；定向 e2e run2 PASS 2.88s（run1 用错 404
  端点——regions 无单查 GET；改 hierarchy 未知区域语义）；全量 e2e 待 R2 终验。
- R2（v0.54–v0.56 合并复核 + 全量 e2e 终验）随本版推送后执行。
