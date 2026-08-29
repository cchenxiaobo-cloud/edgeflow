# EdgeFlow v0.24.0 发布说明（MQTT 功能轮：协议栈 + 订阅型 Mapper + 端到端验证）

> 日期：2026-08-29 ｜ 类型：功能开发轮（审计驱动阶段收官后的第一个功能版本）
> 范围：MQTT 3.1.1 协议栈（codec + client + 测试 broker）、MQTT 订阅型设备 Mapper、e2e 接线；§23 残余小项清理（F-4）
> 硬约束达成：**HTTP 端点保持 42、零新依赖、默认行为不变（全部 MQTT 能力 opt-in）、既有测试零改动**

## 一、MQTT 协议栈（`pkg/mqtt`，全新）

### 1. 报文编解码器（codec）
- MQTT 3.1.1 九种报文：CONNECT / CONNACK / PUBLISH / PUBACK / SUBSCRIBE / SUBACK / PINGREQ / PINGRESP / DISCONNECT。
- 剩余长度 varint 编解码（4 字节上限 268435455，与规范一致）；字符串 u16 大端长度前缀。
- 主题校验：`validateTopicName`（禁通配符/空主题/越界）与 `validateTopicFilter`（通配符位置约束，`+` 单层 `#` 末层）。
- CONNECT 可变头标志位按规范位布局（username/password/willRetain/willQoS/willFlag/cleanSession，bit0 恒 0）；QoS1 PUBLISH 必带 PacketID。
- 错误模型：`ErrMalformed` 哨兵错误族，`%w` 包装可 `errors.Is` 判别；decode 按剩余长度限量读，防超长报文 DoS。
- 测试：golden 字节逐字节断言（正确字节按规范推导，任务书初稿错误字节保留为负向 malformed 用例）、roundtrip、边界与坏输入表驱动。

### 2. 客户端（client）
- `Dial(addr, Options)`：CONNECT→CONNACK 校验（ReturnCode≠0 报错）；`Subscribe`（SUBACK code≥0x80 含码报错）、`Publish`（QoS0 直发 / QoS1 等 PUBACK 10s / QoS2 明确拒绝）、`Close`（DISCONNECT 尽力发送→关连接，sync.Once 幂等）。
- 读泵分发：PUBLISH 按主题匹配回调（读锁快照、锁外调用）；SUBACK/PUBACK 经 packetID 等待通道唤醒；KeepAlive ticker 独立 goroutine 发 PINGREQ。
- **设计裁决：client 不做自动重连**——断开后返回 `ErrClientClosed`，重连由上层 Mapper 监管循环负责（与 opcua mapper 的锁外重连模式同构）。
- 通配匹配 `MatchTopic`：MQTT-4.7 语义（`+` 单层含空层、`#` 末层多级且匹配父层、首尾 `/` 空层语义、`$` 系主题不被以 `+`/`#` 开头的 filter 命中）。
- 测试：26 用例（codec 18 + client 8），自包含 fake broker，`-race` 绿。

## 二、测试 Broker（`pkg/mqttsim`，全新）
- 进程内 MQTT broker：accept 循环、CONNECT 校验（协议名/level/空 ClientID 拒绝）、SUBSCRIBE→SUBACK、PUBLISH 分发（含 QoS1 PUBACK 回执）、PINGREQ→PINGRESP、DISCONNECT 清理；出站队列容量 32（满则丢弃+计数）。
- 断言辅助：`Received()`（收到 PUBLISH 深拷贝）、`PingCount()`；`Close` 幂等。
- 测试：9 用例（含坏报文半关连接、保留类型 0xF0 负向用例），`-race` 绿。
- **已知实现注记（R-6）**：M1 codec 函数未导出，sim 内建了语义对齐的最小本地 codec（~330 行）。收敛路径已登记 KNOWN-ISSUES §24（导出 codec + sim 切换，下轮处理）。

## 三、MQTT 设备 Mapper（`mappers/mqtt`，全新，订阅型采集）
- 与 modbus（轮询型）、opcua（订阅+轮询混合）并列的第三种接入方式：**订阅型**——设备属性经 broker 上报，mapper 订阅 filter 被动接收。
- 环境变量（全部 opt-in，`EDGEFLOW_MQTT_BROKER` 非空即注册）：

| 变量 | 含义 | 默认 |
|---|---|---|
| `EDGEFLOW_MQTT_BROKER` | broker 地址 `host:port`（opt-in 开关） | 未设置（不注册） |
| `EDGEFLOW_MQTT_TOPICS` | 逗号分隔订阅 filter | `devices/+/state` |
| `EDGEFLOW_MQTT_DEVICE_NAME` | 设备名 | `mqtt-device-01` |
| `EDGEFLOW_MQTT_NAMESPACE` | 设备命名空间 | `default` |
| `EDGEFLOW_MQTT_CMD_TOPIC` | 指令主题 | 按首个 filter 字面段推导 |

- 数据面：上报 payload 容错解析（JSON 数字 / 字符串数字 / `k=v` 文本，坏输入跳过不崩）→ 属性快照 → 采集/上报流转与 modbus 同构（台账触点 up/down 全覆盖）。
- 指令面：`HandleCommand` 序列化发布到指令主题，返回当前快照。
- 韧性：监管循环断线 2s 重连 + 重订阅（client 无自动重连，mapper 层负责）；Start/Stop 幂等。
- 测试：9 用例（真 broker + 真协议栈），`-race` 绿；装配与 GUIDE §11 同步落地。

## 四、端到端验证（`tests/e2e`）
- 新增 `TestMQTTDeviceE2E`（~69s）：mqttsim 起 broker → 真实装配路径注册 mapper → broker 推送上报→属性到达断言 → HandleCommand→broker 侧收到指令断言。
- e2e 全套 288s 绿（基线 228s + MQTT 69s，含套件固有开销）。

## 五、§23 残余小项清理（F-4）
- **R-1**：`tests/contract/api_contract_test.go` 静态路由断言文档注释口径修正（漂移实际在 27-31 行；断言零改动，守卫语义不变）。
- **SEC-03 附**：keadm join/batch 产物目录权限 opt-in——`EDGEFLOW_JOIN_DIR_MODE` 环境变量（8 进制，≤0o777，非法值 fail-fast），默认 0o755 行为不变；`resolveJoinDirMode()` 单点实现，11 例测试（含 umask 钉扎）。
- **OPCUA-GUIDE**：与 v0.23.0 行为变化逐点核查，无漂移。

## 六、行为变化明示
- **默认行为零变化**：MQTT 全链路（协议栈/mapper/sim）仅在 `EDGEFLOW_MQTT_BROKER` 设置后启用；keadm 产物目录权限默认 0755 不变。
- HTTP 端点 42 不变；`go.mod` 零新依赖；既有测试零改动（39 包全量绿 + 契约守卫绿 + e2e 全套绿）。

## 七、残余与后续（详见 KNOWN-ISSUES §24）
- **R-5**：契约守卫 `TestContractRoutesNoExtraRoutesRegistered` 文档注释仍写「遍历 main.go」（实际同扫两文件；本轮守卫行域禁触未动）。
- **R-6**：M1 codec 导出 + mqttsim 本地 codec 收敛（下轮或 v0.25.0）。
