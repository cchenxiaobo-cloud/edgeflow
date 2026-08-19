# EdgeFlow 已知限制与问题台账（KNOWN-ISSUES）

> 最后更新：2026-08-19（v0.3.0 开发轮）
> 收录原则：只登记**已实现功能上的已知边界/脆弱点**；未实现功能见 `docs/ROADMAP.md §8-§11` 与 `docs/PROGRESS.md §5`。每轮开发/发布时复查本表，已闭环项移出。

---

## 1. v0.2.0 开发轮登记（2026-08-18）

> §1 四条已于 v0.3.0 开发轮全部闭环（commit `714d5ba`，2026-08-19），详见各行「计划」列标注；原文保留备查。

| # | 位置 | 限制 | 影响 | 计划 |
|---|------|------|------|------|
| ① | `cmd/edgecore/device_mapper.go` `collectMapperReports` | 周期采集汇入影子时按 `default` 命名空间**硬编码**（Collect 接口只返回属性值、不携带 ns） | Modbus 多 ns 部署（如 `EDGEFLOW_MODBUS_NAMESPACE=plant-a`）时，云端设备列表出现 `default/mb-sensor-01` 与 `plant-a/mb-sensor-01` 双条目；指令路径（`Route` 按 ns 路由）正确，不受影响 | ✅ v0.3.0 已修（2026-08-19）：采集汇入影子改按 mapper 自身命名空间写入（与注册路由同源判定），仅显式设置 `EDGEFLOW_MODBUS_NAMESPACE` 时才改变 ns，默认行为与 v0.2.0 逐字节一致 |
| ② | `edge/pkg/edgehub/client_test.go` 退避重置测试 | 仍用**实时时间阈值**断言（≥500ms / <500ms，依赖 50ms vs 800ms 差异） | 慢 CI 机器上第二次断言有 flake 风险（本地 `-count=5` 稳定）；复核项 m3 本批跳过 | ✅ v0.3.0 已修（2026-08-19）：新增 `Options.BackoffSleepFunc` 注入点（nil=默认退避，非 nil 接管休眠），测试改为注入计数断言，移除实时时间阈值 |
| ③ | `cmd/cloudcore/main.go` `syncPod` 400 分支 | `err.Error()` **裸拼 JSON** 响应（`` `{"error":"invalid resources: `+err.Error()+`"}` ``） | 当前校验错误文案不含 JSON 敏感字符，"碰巧不炸"；结构脆弱，任何含引号/反斜杠的路径都会产出非法 JSON | ✅ v0.3.0 已修（2026-08-19）：400 分支改 `json.Marshal` 结构化输出（与 409 分支同构），畸形输入也产出合法 JSON，响应结构不变（`{"error":...}`） |
| ④ | `EDGEFLOW_EDGECORE_RECONCILE_INTERVAL` / `EDGEFLOW_EDGECORE_REPORT_INTERVAL` / `EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL` | 合法区间 1s~10m；传 300ms 等越界值**静默回落默认值**（仅 Warn 日志，`durationFromEnv`） | 运维误配周期时不易察觉实际生效值与配置不符 | ✅ v0.3.0 已修（2026-08-19）：`edgeCoreIntervalFromEnv` 启动日志三态明示——合法（来源：环境变量，请求值+生效值）/ 越界回落（Warn 说明）/ 未设置（来源：默认/配置文件），启动即可核对实际生效值 |

## 2. 复查记录

- 2026-08-18（v0.2.0 开发轮）：新增 ①②③④ 四条；历史已知问题（v0.1.x）仍见 `docs/API-SPEC.md §8` 与 `docs/HANDOFF.md §10`，未迁移本表。
- 2026-08-19（v0.3.0 开发轮）：§1 四条全部闭环并逐行标注（commit `714d5ba`）；新增 §3 登记四条（pkg/opcua 首阶段未实现边界 + 周期 env 日志热重载重复输出）。历史已知问题（v0.1.x）位置不变。

## 3. v0.3.0 开发轮登记（2026-08-19）

| # | 位置 | 限制 | 影响 | 计划 |
|---|------|------|------|------|
| ① | `pkg/opcua`（整体） | OPC-UA 首阶段仅交付 UA Binary 协议栈核心：**未实现** Read/Write/Subscribe 等任何服务请求、SecureChannel 打开（Conn 为裸传输，ChannelId 恒 0）、UA 节点模型/对象树、Discovery 端点、安全策略 Sign/SignAndEncrypt（仅 SecurityPolicy None 明文） | 当前 pkg/opcua 只能做底层编解码与 TCP 握手回环，无法直接驱动真实 OPC-UA 设备读写；明文传输无认证/完整性，**仅限可信隔离网络**（封闭 OT 网段/本机模拟） | 后续里程碑：SecureChannel 打开 → Read/Write 服务 → Mapper 层接入 → 安全策略 |
| ② | `pkg/opcua` 互操作 | 未与第三方 UA 栈（open62541/node-opcua）做互操作验证，本轮仅自研 mock 服务端回环（transport_test 真实 TCP 握手为自建对端） | wire 级编解码符合 Part 6 但未经验证真栈互认，存在字段约定偏差风险 | 下一里程碑安排 open62541/node-opcua 互操作 cross-check |
| ③ | `pkg/opcua/types.go` | `DiagnosticInfo` 仅空骨架（无字段/无位域语义）；Variant 解码维度位仅解析不发射（Encode 不写 Dimensions 位，仅 Decode 支持）；DateTime 负数（1601-01-01 之前）未测试 | 诊断信息无法承载；编码维度信息丢失（当前无消费方）；1601 年前时间戳行为未验证 | 随后续里程碑按需补齐 |
| ④ | `pkg/config/edgecore.go` 周期 env 日志 | 周期 env 三态启动日志（KNOWN-ISSUES §1 ④ 修复引入）在**热重载**（SIGHUP/mtime）时每次重载重复输出三条 Info | 日志量轻微增长（低频，量级小，仅热重载时） | 可接受，暂不处理；如后续日志规范收紧再评估 |

---

*表中「位置」为登记时的代码位置，代码演进后以 commit 为准。*
