# EdgeFlow v0.21.0 发布说明（安全默认值包 + 协议纵深包）

- **发布日期**：2026-08-28
- **版本基线**：v0.20.0 → v0.21.0
- **主题**：把审计台账的 P0 风险变成可见的开关与纵深防御——默认行为零改变，安全收紧全部 opt-in
- **兼容性**：HTTP 端点保持 **42** 不变；全部新开关默认关闭，缺省行为与 v0.20.0 逐字节一致；升级零迁移；老边缘零动作；**零新依赖**

## 一、核心能力

### 1. 安全默认值包（审计 SEC-01 / SEC-02 / CHN-06）

**设计原则：只提升可见性，不改变任何默认行为。** 三条告警全部不阻断启动；两个收紧开关默认关闭，显式开启才生效。

#### SEC-01：管理 API 认证关闭启动告警
- cloudcore 启动时若管理 API 认证（`EDGEFLOW_CLOUDCORE_AUTH`）未启用，输出 WARN：提示 `/api/v1/*` 端点对可达者无差别开放及生产建议。
- 可用 `EDGEFLOW_CLOUDCORE_AUTH_WARN=off` 显式静默（仅关闭本告警，不影响认证本身）；值缺省或任何非 `off` 值均视为开启。

#### SEC-02：云边接入令牌强校验开关
- 新增 Option `cloudhub.WithRequireNodeToken(bool)`，由环境变量 `EDGEFLOW_CLOUDHUB_REQUIRE_NODE_TOKEN=on` 接线。
- **生效语义**（服务端 `EDGEFLOW_CLOUDCORE_NODE_TOKEN` 为空时）：拒绝**携带令牌**的注册（防伪造令牌探测抢占同 ID 真节点），拒绝文案「服务端未配置令牌且已开启强制校验，拒绝携带令牌的注册」；**无令牌注册仍按裸奔兼容接受**（全面关闸 = 配置 nodeToken 或开启 mTLS，二者任一即可，见 CHN-06 告警提示）。
- 服务端已配 nodeToken 时既有校验照常执行，本开关无额外效果。
- 默认关闭：不设置时行为与 v0.20.0 逐字节一致。

#### CHN-06：裸奔组合聚合告警
- 云边通道无 mTLS **且**未配置节点令牌同时成立时，输出聚合 WARN：注册面与下发面均明文且无认证，仅限可信隔离网络；提示 mTLS 或令牌任一收紧路径。
- 与 SEC-02 告警互补：enforce=on 时该维度输出 INFO（生效中提示），避免告警噪音。

#### Helm 加固段
- values.yaml 新增（默认全关，不注入任何 env）：
  - `cloudcore.auth.enabled` / `cloudcore.auth.apiToken`：管理 API 认证配置透传
  - `cloudcore.auth.warnOff`：true 时注入 `EDGEFLOW_CLOUDCORE_AUTH_WARN=off`
  - `cloudcore.cloudhub.nodeToken` / `cloudcore.cloudhub.requireNodeToken`：节点令牌与强校验开关透传
- 模板 `cloudcore-deployment.yaml` 仅在显式配置时渲染对应 env，零配置渲染结果与 v0.20.0 一致。

### 2. 协议纵深包（审计 PRT-01 / PRT-03 / PRT-04 / PRT-14 / PRT-18）

#### PRT-01 / PRT-14：数组预分配放大防御
- Variant 数组（`decodeVariant`）与 StringList（`decodeStringList`）解码前按「声明元素数 × 元素类型最小编码字节数」对照剩余缓冲预检：**超过 1024 元素豁免阈值**（小规模声明预分配至多 16KB 无害）且声明需求超剩余缓冲 → 直接拒绝（`ErrTooLong`），不再按声明长度预分配。
- 恶意报文（如 20 字节声明 16M 元素）从「预分配 16M 空槽」降级为「立即报错」，内存峰值有界。
- 防误拒：合法小数组照常解码；**截断报文保持既有语义**（解码循环内 `io.ErrUnexpectedEOF`，既有测试零改动仍绿）。
- 新增 `variantMinElemSize`：内置类型元素最小编码字节数表（Boolean/Byte=1B … Guid=16B；String/ByteString 按长度定界符 4B 计）。

#### PRT-03：DiagnosticInfo 递归深度限制
- 新增 `MaxDiagnosticDepth = 100`：`decodeDiagnosticInfo` 按 InnerDiagnosticInfo 嵌套深度计数，超过即拒绝（`ErrTooLong`），封堵深嵌套报文耗栈攻击面。
- 全部调用点（ResponseHeader / OpenSecureChannelResponse / DataValue 通知内嵌诊断）统一传 depth。

#### PRT-04：订阅泵退出关闭发布通道
- `pumpLoop` 一切退出路径（连接级故障 / stopPump）在收尾 defer 中关闭 `pubCh`（`sync.Once` 防与 stopPump 双关），订阅方 `for range pubCh` 可感知退出，goroutine 不再悬挂泄漏。
- 退出时 `c.pubCh` 置 nil：下次 Subscribe 重建新通道，不复用已关闭通道。

#### PRT-18：mapper 订阅自愈
- OPC-UA mapper 感知订阅通道关闭（泵异常退出），自动重连并重建订阅，无需重启进程。

## 二、验证摘要（实测）

- `go build ./...` / `go vet ./...`：通过。
- `go test ./...`：37 包全绿（含 tests/contract 路由计数 42 守卫、文档一致性守卫）。
- 既有测试零改动仍绿（向后兼容硬约束）：`types_test.go` 截断数组契约（`io.ErrUnexpectedEOF`）、cloudhub 既有注册测试、auth 中间件五测试。
- 新增测试：SEC-02 enforce 四态（默认关兼容/enforce 拒伪造令牌/裸奔兼容不受影响/nodeToken 非空无额外效果）、auth WARN 开关四态表驱动、PRT-01 恶意大数组（Boolean 16M / Guid 10 万）分配前拒绝、PRT-01/14 小报文防误拒、PRT-03 深嵌套拒绝（`diagnostic_depth_test.go`）、PRT-04 泵退出 pubCh 关闭 + goroutine 回收、PRT-18 mapper 泵死自愈（`mapper_exit_test.go`）。
- `helm lint`：0 failed。

## 三、升级注意

1. **默认行为零改变**：所有新开关缺省关闭，直接升级无感知。
2. 收紧路径（均为显式 opt-in）：
   - 管理 API 认证：`EDGEFLOW_CLOUDCORE_AUTH=on` + `EDGEFLOW_CLOUDCORE_API_TOKEN=<token>`
   - 云边令牌全面校验：`EDGEFLOW_CLOUDCORE_NODE_TOKEN=<token>`（云端与边缘同值）
   - 仅拒绝伪造令牌探测：`EDGEFLOW_CLOUDHUB_REQUIRE_NODE_TOKEN=on`（服务端未配令牌时生效，无令牌注册仍兼容放行）
   - 静默告警：`EDGEFLOW_CLOUDCORE_AUTH_WARN=off`
3. Helm 侧对应 `cloudcore.auth.*` / `cloudcore.cloudhub.*` 键，默认全关。
4. `pkg/opcua` 协议栈对恶意/损坏报文的错误信息更明确（`ErrTooLong` 带量化上下文），调用方若按错误文本匹配需改用 `errors.Is`。

## 四、遗留（非阻断，见 KNOWN-ISSUES §21）

- OPC-UA 传输加密（Sign/SignAndEncrypt）仍未实现（PRT 域 P1 批次）：明文限制不变，仅限可信隔离网络。
- 安全默认值「fail-closed 全面关闸」形态（enforce=on 时连无令牌注册也拒绝）经评审判定与裸奔兼容底线冲突，留待引入维护窗口协商机制后再评估（决策异议单列）。
- 审计 P1×12 / P2×13 项留后续版本（清单见 `.cluster/edgeflow-audit/tasklist.csv`，不入库）。
