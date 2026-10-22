# spec 0038 — 审计导出与转发规则导出/导入（v0.65.0）

> 版本：v0.65.0 ｜ 缺口：运维面导出/导入（台账审计留存 + 规则快照恢复）｜
> 上游：spec 0029（审计台账）、spec 0026（转发规则）
> 说明：本版为 API 扩展：端点 134→137，消息 19 不变。本 spec 为 R5 复核
> P2-2 处置补录（交付时遗漏落盘，现按实现+复核处置结果补记）。

## 裁决

1. GET /api/v1/audit/entries/export：审计台账 JSONL 原样导出，附件下载
   （Content-Disposition）；**operator 及以上**（JSONL 含 IP 敏感信息，
   viewer 拒绝——R5 复核 P1-1 处置：RBAC 执行层以 rbac.operatorPlusPaths
   路径级 facet 落地，非仅文档声明）。
2. GET /api/v1/forwards/export：转发规则脱敏 JSON 导出（hasSecret/
   hasMqttPass 布尔代替回显 + exportedAt 时间戳）；**仅 admin**
   （rbac.adminOnlyPaths）。
3. POST /api/v1/forwards/import：批量导入，**仅 admin**；逐条 Create 校验，
   失败条目跳过计数入 errors；返回 {imported, skipped, errors?}。
4. Import ID 语义（R5 复核 P2-1 处置修订）：源 ID 非空且不存在 → 按源 ID
   落库（导出→导入 round-trip 幂等，再导出 ID 不变）；ID 已存在 → 跳过
   计 skipped；源 ID 空 → 新生成。
5. mqtt sink 认证/TLS 字段（v0.64.0 顺带落 v0.65 通道）：POST/PUT body 接受
   mqttUsername/mqttPassword/mqttTLS；GET 面脱敏。
6. 非目标：导入不做跨实例合并策略（同名规则不去重，仅 ID 去重）；导出不
   含台账投递记录明细（那是 audit/export 的职责）。

## 交付锚

实现：cmd/cloudcore/v0650_export_api.go + cloud/pkg/forwardstore（Import/
insert）；契约 139 条（routes.go/API-SPEC/API-COMPATIBILITY 三源对齐）；
测试：cloud/pkg/rbac/v0650_test.go（RBAC 矩阵）、cloud/pkg/forwardstore/
v0650_test.go（Import round-trip/跳过/tls 校验）。
