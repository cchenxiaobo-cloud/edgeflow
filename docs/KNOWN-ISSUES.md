# EdgeFlow 已知限制与问题台账（KNOWN-ISSUES）

> 最后更新：2026-08-18（v0.2.0 开发轮）
> 收录原则：只登记**已实现功能上的已知边界/脆弱点**；未实现功能见 `docs/ROADMAP.md §8-§10` 与 `docs/PROGRESS.md §5`。每轮开发/发布时复查本表，已闭环项移出。

---

## 1. v0.2.0 开发轮登记（2026-08-18）

| # | 位置 | 限制 | 影响 | 计划 |
|---|------|------|------|------|
| ① | `cmd/edgecore/device_mapper.go` `collectMapperReports` | 周期采集汇入影子时按 `default` 命名空间**硬编码**（Collect 接口只返回属性值、不携带 ns） | Modbus 多 ns 部署（如 `EDGEFLOW_MODBUS_NAMESPACE=plant-a`）时，云端设备列表出现 `default/mb-sensor-01` 与 `plant-a/mb-sensor-01` 双条目；指令路径（`Route` 按 ns 路由）正确，不受影响 | 采集/上报接口携带命名空间后消除（后续轮） |
| ② | `edge/pkg/edgehub/client_test.go` 退避重置测试 | 仍用**实时时间阈值**断言（≥500ms / <500ms，依赖 50ms vs 800ms 差异） | 慢 CI 机器上第二次断言有 flake 风险（本地 `-count=5` 稳定）；复核项 m3 本批跳过 | 需生产代码提供退避参数注入点后改为参数注入 + 计数断言 |
| ③ | `cmd/cloudcore/main.go` `syncPod` 400 分支 | `err.Error()` **裸拼 JSON** 响应（`` `{"error":"invalid resources: `+err.Error()+`"}` ``） | 当前校验错误文案不含 JSON 敏感字符，"碰巧不炸"；结构脆弱，任何含引号/反斜杠的路径都会产出非法 JSON | 下批改 `json.Marshal`（409 分支本批已修） |
| ④ | `EDGEFLOW_EDGECORE_RECONCILE_INTERVAL` / `EDGEFLOW_EDGECORE_REPORT_INTERVAL` / `EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL` | 合法区间 1s~10m；传 300ms 等越界值**静默回落默认值**（仅 Warn 日志，`durationFromEnv`） | 运维误配周期时不易察觉实际生效值与配置不符 | 启动日志明示生效值或严格失败（待定，需产品确认） |

## 2. 复查记录

- 2026-08-18（v0.2.0 开发轮）：新增 ①②③④ 四条；历史已知问题（v0.1.x）仍见 `docs/API-SPEC.md §8` 与 `docs/HANDOFF.md §10`，未迁移本表。
