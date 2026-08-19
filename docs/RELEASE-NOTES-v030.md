# EdgeFlow v0.3.0 Release Notes

> 状态：✅ 已定稿（2026-08-19，v0.3.0 发布轮）。
> 版本决策：缺陷修复 + 新功能 → minor（v0.3.0）。
> 发布基线：HEAD `93458d6`（提交链 `714d5ba` → `93458d6`）；tag v0.3.0。
> 配套：docs/KNOWN-ISSUES.md（§1 闭环 + §3 新登记）、docs/ROADMAP.md §11、docs/RELEASE-LEDGER.md v0.3.0 区块。

---

## 一、主题概述

1. **已知问题台账闭环**：v0.2.0 登记的四条 KNOWN-ISSUES（采集命名空间硬编码、退避测试实时阈值 flake、400 分支裸拼 JSON、周期环境变量越界静默回落）全部修复，并新增测试基建 API（`pkg/log.SetOutput`）。commit `714d5ba`。
2. **OPC-UA 独立立项启动（WBS 5.2 第一阶段）**：零第三方依赖交付 UA Binary 协议栈核心（`pkg/opcua`，纯标准库，41 顶层测试），为后续 SecureChannel / Read-Write / Mapper 接入打下协议基础。commit `93458d6`。

## 二、KNOWN-ISSUES 四条修复明细（commit `714d5ba`）

| # | 项 | 修复内容 |
|---|----|----------|
| ① | 采集命名空间同源 | `collectMapperReports` 改按 mapper 自身命名空间写入影子（与注册路由同源判定）；仅显式设置 `EDGEFLOW_MODBUS_NAMESPACE` 时才改变 ns。默认部署行为与 v0.2.0 **逐字节一致**；多 ns 部署不再出现 `default/` 与 `plant-a/` 双条目 |
| ② | 退避测试注入点 | `edgehub.Options.BackoffSleepFunc`（nil=默认退避，非 nil 接管休眠，返回 false 中止重连）；测试改注入 + 计数断言，移除实时时间阈值，消除慢 CI flake 风险 |
| ③ | syncPod 400 JSON 安全 | 400 分支改 `json.Marshal` 结构化输出（与 409 分支同构）；畸形输入（含引号/反斜杠的错误文案）也产出合法 JSON；**响应结构不变**（`{"error":...}`），API 兼容 |
| ④ | 周期环境变量生效值明示 | `edgeCoreIntervalFromEnv` 启动日志三态明示：合法（来源：环境变量，请求值+生效值）/ 越界回落（Warn 说明）/ 未设置（来源：默认/配置文件）。启动即可核对实际生效值 |

测试基建：`pkg/log.SetOutput(io.Writer)`（nil 恢复 stderr，兼容 stdlib 约定）。

## 三、OPC-UA 协议栈第一阶段（WBS 5.2-M1，commit `93458d6`）

- **范围**：UA Binary 协议栈核心（OPC UA Part 6），新包 `pkg/opcua`，零第三方运行时依赖。
- **类型系统**：NodeId（Part 6 Table 5 全编码形式：两字节/四字节/Numeric/String/Guid/ByteString + 扩展 32 位 ns）、QualifiedName、LocalizedText、StatusCode（severity）、Guid、ByteString、ExtensionObject、DataValue（双时间戳+皮秒+掩码位）、Variant（type-mask，25 标量 + 22 数组形态 round-trip）、DateTime（1601-01-01 起 100ns tick）。
- **编解码器**：UA Binary 大端序；Int32 长度前缀（-1=null）；字符串解码端截断且保持流对齐；长度溢出在读取消息体之前拒绝。
- **消息与握手**：12 字节 MessageHeader（MessageType/ChunkSize/ChannelId）、Hello/Acknowledge/Error；`Dial`/`DialTimeout` 完成 TCP→HEL→ACK 三态协商（sendLimit/recvLimit 校验）返回就绪 Conn；`ReadMessage`/`WriteMessage` 单 chunk 帧级 I/O（中间 chunk 拒绝）。
- **测试**：41 顶层测试——类型 round-trip 表驱动全覆盖（含空串/超长截断/NodeId 各形式/Variant 数组/DataValue 空时间戳）、wire 级 golden 逐字节断言、真实 TCP 握手（自研 mock 对端验证 Hello 字段 + Ack 校验 + 回声往返）、错误路径（短包/坏 magic/长度溢出/保留编码字节拒绝）。

## 四、安全说明（必读）

- **SecurityPolicy None（明文）**：`pkg/opcua` 当前仅支持 None 策略，所有字节明文传输，**无认证、无完整性保护**。仅限可信隔离网络（封闭 OT 网段、本机模拟）使用；**严禁**将 SecurityPolicy None 端点暴露到不可信网络。Sign / SignAndEncrypt 安全策略为后续里程碑。

## 五、已知限制（v0.3.0 新增登记，详见 docs/KNOWN-ISSUES.md §3）

- `pkg/opcua` **未实现**：Read/Write/Subscribe 服务、SecureChannel 打开（Conn 为裸传输，ChannelId 恒 0）、UA 节点模型/对象树、Discovery 端点、Sign/SignAndEncrypt；
- **未与第三方 UA 栈**（open62541/node-opcua）互操作验证——本轮仅自研 mock 回环，真实设备接入前需完成互操作 cross-check；
- `DiagnosticInfo` 为空骨架；Variant 维度位仅解码不发射（当前无消费方）；DateTime 负数（1601-01-01 前）未测试；
- 周期 env 三态启动日志在**热重载**时重复输出三条 Info（低频、量级小，可接受）；
- 既有已知限制（v0.2.0 及以前）不变（见 docs/RELEASE-NOTES-v020.md）。

## 六、验证摘要

| 项目 | 结果 |
|------|------|
| 全仓测试 | ✅ 32 个含测试包全绿（`go list ./...` 共 38 包），exit=0 |
| pkg/opcua | ✅ 41 顶层测试全过，`-race` 干净 |
| 预发冒烟 | ✅ 10/10 PASS（基线 `93458d6`）：limit 端到端、超卖 409、400 JSON 合法性（畸形输入）、漂移重建、Mapper 装配开关、Modbus ns 隔离与影子同源、log SetLevel、周期 env 启动日志三态、OPC-UA 握手/编解码走查、退避测试 `-count=5` 稳定性 |
| 制品 | ✅ release/v0.3.0/ 14 文件；checksums 13/13 OK；双架构镜像 4/4 平台实测 `--version=v0.3.0 gitCommit=93458d6`；trivy 0 HIGH/0 CRITICAL；govulncheck 9/9 clean |
| 兼容性 | ✅ 400 响应结构不变；Mapper 默认行为与 v0.2.0 逐字节一致；无破坏性 API 变更（`BackoffSleepFunc` nil=旧行为；`SetOutput` 纯新增） |

## 七、后续里程碑（Roadmap）

- **OPC-UA**：SecureChannel 打开 → Read/Write 服务 → 第三方 UA 栈互操作 cross-check → Mapper 层接入（ROADMAP §11）；
- **既有 backlog** 不变（见 docs/ROADMAP.md、docs/PROGRESS.md §5）；
- **环境边界项**（沿用登记）：远程镜像推送、kind 集群实测、生产灰度、cosign 签名、100 节点压测。
