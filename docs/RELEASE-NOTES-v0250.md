# EdgeFlow v0.25.0 Release Notes —— MQTT 硬化轮

- 日期：2026-08-30
- 基线：v0.24.0（b706313）
- 主题：R-5/R-6 残余票闭环 ＋ MQTT TLS 加密传输全栈
- 兼容性：HTTP 端点保持 42；go.mod 零新依赖（crypto/* 全标准库）；默认行为与 v0.24.0 逐字兼容（TLS 全链路 opt-in）；既有测试零改动全绿

## 1. R-6 codec 收敛：客户端/broker/测试共享单一实现（净 -300 行）

- `pkg/mqtt` 导出 `EncodePacket` / `DecodePacket` / `ValidateTopicFilter`（薄包装；小写实现与既有调用、既有测试零改动）。
- `pkg/mqttsim` 删除本地 wire codec 与主题匹配器（sim.go 747→447 行）：编码/解码/过滤器校验/通配匹配全部改走 pkg/mqtt 单一实现。
- 保留三个同名薄垫片（`simMatchTopic`/`encodePacket`/`decodePacket`）：冻结测试 v0240_sim_test.go 直接引用这些符号，垫片保证既有测试零改动。
- **关键裁决（坏客户端 SUBSCRIBE 宽容通道）**：冻结负向测试需将非法 filter（"a/#/b"）放上电线并期望 broker 逐 filter 回 SUBACK 0x80 且连接保持；客户端级编解码器对非法 filter 双侧拒绝（对真实客户端正确）。sim 垫片对 SUBSCRIBE 分流出最小宽容路径（encode/decode permissive + varint 小助手），非 SUBSCRIBE 类型经 MultiReader 原样走 M1 严格管线——严格性零损失，详见 KNOWN-ISSUES §25.2。
- 新增 `pkg/mqtt/v0250_export_test.go`：导出面 parity 4 用例（roundtrip/坏输入/过滤器表驱动）。

## 2. R-5 契约守卫口径修正

- `TestContractRoutesNoExtraRoutesRegistered` 反向断言注释与静态解析背景注释改为「同扫 main.go 与 model_api.go」的实际口径。
- 守卫错误信息携带来源文件名（`registeredRoute` 增加 `file` 字段，`scanRouteFile` 传入基名）。
- 断言语义零变化，42 端点契约守卫全绿。

## 3. MQTT TLS 加密传输全栈（全 opt-in，零新依赖）

| 层 | 变更 |
|---|---|
| pkg/mqtt（client） | `Options.TLSConfig *tls.Config`（nil=明文，v0.24.0 行为逐字不变）；TLS 时走 `tls.DialWithDialer`；`ServerName` 为空时从 addr host 自动回填（SplitHostPort）；对调用方配置 `Clone()` 防突变 |
| pkg/mqttsim（测试 broker） | `NewBrokerTLS(tlsCfg)`：TLS 终止于 listener，serve/pump 仍见纯 `net.Conn`，broker 其余路径与明文逐字一致；nil 配置 fail-fast |
| mappers/mqtt（Mapper） | 新增 `EDGEFLOW_MQTT_TLS_CA`（PEM 文件路径 → RootCAs，读失败/解析失败 fail-fast 报错）与 `EDGEFLOW_MQTT_TLS_INSECURE`（"1"/"true"/"on" 大小写不敏感，开启打 WARN，开发/测试逃生通道）；两 env 均空 → 不注入 TLS（明文回归） |
| tests/e2e | `TestMQTTTLSDeviceE2E`：TLS broker + CA 文件装配，全环 TLS 闭环（上报→云端属性→指令下发→cmd 主题→Desired 收敛→回发收敛） |

测试增量：pkg/mqtt 5 个 TLS 用例（握手/持久化/错误 CA/INSECURE/ServerName 回填）、pkg/mqttsim 2 个（TLS 握手 CONNACK/nil fail-fast）、mappers/mqtt 4 个（全链路上报/CA fail-fast×2/INSECURE+明文回归）、e2e 1 个 TLS 闭环；全部 `-race` 绿。

## 4. 升级说明

- 明文用户：无需任何动作，行为与 v0.24.0 一致。
- 启用 TLS：设置 `EDGEFLOW_MQTT_TLS_CA=/path/to/ca.pem`（推荐）或开发/测试临时用 `EDGEFLOW_MQTT_TLS_INSECURE=1`。
- 生产 broker（EMQX/Mosquitto 等）：使用真实 CA 签发服务端证书；mapper 侧只需 CA 文件。

## 5. 已知边界

- mqttsim 为测试 broker：TLS 单自签证书、无认证体系，不承诺生产级投递保证。
- client mTLS（双向认证）未含，登记 ROADMAP §20 下轮候选；MQTT QoS2 仍不支持（client 层明确拒绝）。
- 详细裁决与残余项见 `docs/KNOWN-ISSUES.md` §25、`docs/ROADMAP.md` §20。
