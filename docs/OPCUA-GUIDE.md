# EdgeFlow OPC-UA 设备接入指南

> 适用于 EdgeFlow **v0.15.0+**（WBS 5.2 第二阶段）。本文介绍如何用自研 OPC-UA 协议栈（`pkg/opcua`）与模拟器（`pkg/opcuasim`）完成设备接入、采集上报与指令下发全链路。

> **v0.15.0 新增**：Subscription 订阅推送采集模式（值变化即时推送，不再依赖轮询批量 Read）与 Browse 点位发现 CLI。见 §3.1 与 §4.1。

## 1. 概述

EdgeFlow 的 OPC-UA 接入由三层组成，全部**零第三方依赖**（纯 Go 标准库）：

- `pkg/opcua`：UA Binary 协议栈 + 客户端（SecurityPolicy None，匿名会话）。v0.3.0 交付编解码/HEL/ACK；v0.14.0 补齐 SecureChannel（OPN/CLO）、Session（CreateSession/ActivateSession/CloseSession）、Read/Write 服务与高层 Client API
- `pkg/opcuasim`：自研模拟服务器（开发/联调用，6 点位动态模型）
- `mappers/opcua`：设备 Mapper——接入统一 Mapper 框架（采集/指令/台账），经 edgecore env 显式启用

数据流：

```
OPC-UA Server（真实/模拟）
   ↑↓ Read / Write（UA Binary, opc.tcp）
mappers/opcua（Collect 批量读点 → map[string]float64；HandleCommand 写点+回读验证）
   ↓ 影子 Twin.Reported → DeviceReport 周期上报
cloudcore devicestatus → GET /api/v1/devices 可见；POST device-command 下发
```

## 2. 快速开始（模拟器联调）

```bash
# 终端 1：起模拟器（默认 127.0.0.1:14840）
go run ./hack/opcua-sim

# 终端 2：起 edgecore（指向模拟器）
EDGEFLOW_OPCUA_ENDPOINT=opc.tcp://127.0.0.1:14840 \
EDGEFLOW_OPCUA_NODES="temperature=ns=2;i=1001,humidity=ns=2;i=1002,setpoint=ns=2;i=3001" \
./bin/edgecore
```

云端验证（默认 cloudcore :8080）：

```bash
# 设备属性可见
curl http://127.0.0.1:8080/api/v1/devices

# 写 setpoint=200（边缘 Ack 后 Mapper 回读验证一致才返回 200）
curl -X POST http://127.0.0.1:8080/api/v1/nodes/<nodeID>/device-command \
  -H 'Content-Type: application/json' \
  -d '{"deviceName":"opcua-device-01","namespace":"default","property":"setpoint","value":200}'
```

## 3. 环境变量（edgecore）

| 变量 | 默认 | 语义 |
|---|---|---|
| `EDGEFLOW_OPCUA_ENDPOINT` | 空（不启用） | 端点 `opc.tcp://host:port`；**非空即注册 Mapper** |
| `EDGEFLOW_OPCUA_NODES` | 空 | 点位表，逗号分隔 `name=nodeId`；解析失败仅告警跳过注册 |
| `EDGEFLOW_OPCUA_DEVICE_NAME` | `opcua-device-01` | 设备名 |
| `EDGEFLOW_OPCUA_NAMESPACE` | `default` | 设备命名空间 |

点位 NodeId 支持五种形式（与 UA 规范互逆）：`ns=2;i=1001`（数字）、`ns=0;s=name`（字符串）、`ns=1;g=<GUID>`、`ns=1;b=<HEX>`、纯数字（等价 `ns=0;i=`）。

### 3.1 订阅模式（v0.15.0 opt-in）

设置 `EDGEFLOW_OPCUA_SUBSCRIPTION=on` 启用（缺省 off 轮询，行为不变）：

- Mapper 建立 OPC-UA Subscription，点位变化由服务器**主动推送**进缓存；`Collect()` 返回缓存快照
- KeepAlive 空通知维持通道活性；序列号跳变或断线自动重建订阅；写点回读后缓存同步刷新
- 服务 TypeId 均经 OPC Foundation 官方 NodeIds.csv 核验

### 4.1 点位发现（v0.15.0）

```bash
go run ./hack/opcua-browse -endpoint opc.tcp://127.0.0.1:14840
# 输出示例：
# temperature=ns=2;i=1001,humidity=ns=2;i=1002,...
```

输出的 `name=nodeId` 行可直接粘进 `EDGEFLOW_OPCUA_NODES`。

## 4. 模拟器点位表（事实标准）

| NodeId | 名称 | 类型 | 读写 | 行为 |
|---|---|---|---|---|
| `ns=2;i=1001` | temperature | Double | 只读 | 向 setpoint 收敛（因子 0.2）±0.5°C 扰动 |
| `ns=2;i=1002` | humidity | Double | 只读 | ±0.8%/周期随机游走 |
| `ns=2;i=1003` | pressure | Double | 只读 | ±0.3 kPa/周期随机游走 |
| `ns=2;i=2001` | running | Boolean | 只读 | 恒 true |
| `ns=2;i=2002` | label | String | 只读 | "opcua-sim" |
| `ns=2;i=3001` | setpoint | Double | **可写** | 目标温度；写后温度向其收敛 |

波动周期默认 500ms（库 API `WithStep` 可调）；端口 env `OPCUA_SIM_PORT` 覆盖。

## 5. 属性转换策略（Collect 契约）

| Variant 类型 | 转换 |
|---|---|
| Int8/16/32/64、UInt8/16/32/64、Float、Double | 直接转 float64 |
| Boolean | true→1 / false→0 |
| String | ParseFloat 失败则跳过 + Warn |
| DateTime/Guid/ByteString/NodeId/StatusCode/QualifiedName/LocalizedText/ExtensionObject/数组 | 不支持，跳过 + Warn |

节点读取状态非 Good（如 BadNodeIdUnknown）→ 该属性本轮跳过，不影响其余点位。

## 6. 指令下发（写点 + 回读验证）

- `property` 必须命中点位表中的名称，否则报错（云端 502）
- 写入值为 Double；服务端接受后 Mapper **回读该节点**校验一致性（容差 1e-6），不一致视为失败
- 只读节点写入被服务端拒绝（BadNotWritable / BadNodeIdUnknown）→ 明确报错
- 每次操作记录边缘操作台账（direction=down）；采集记录 direction=up

台账查询：

```bash
sqlite3 data/edgecore.db "SELECT id,ts,device_id,direction,reg_addr,value,result FROM op_ledger ORDER BY id DESC LIMIT 20;"
```

## 7. 端到端自动验收入口

```bash
# 全自动单机验收：模拟器 → Mapper 采集/写点/回读 → 台账输出
go run ./hack/opcua-e2e

# 云边闭环（真实装配路径，无需 Docker）
go test -v -timeout 20m ./tests/e2e/ -run TestOPCUADeviceE2E
```

## 8. 接真实设备

将 `EDGEFLOW_OPCUA_ENDPOINT` 指向真实服务器、按实际点位改 `EDGEFLOW_OPCUA_NODES` 即可。注意：

1. v0.14.0 仅支持 SecurityPolicy None + 匿名会话——真实服务器若强制签名/加密策略将握手失败（返回错误并持续重连告警）
2. 服务器需允许匿名用户对目标节点的 Read/Write 权限
3. 单 chunk 消息上限 64KiB（批量读点位数受此约束，数百点位量级内无虞）

## 9. 安全边界（重要）

SecurityPolicy None **明文传输且无认证**，仅限可信隔离网络（本机模拟、封闭 OT 段）。生产环境暴露前必须等待安全策略里程碑（Sign/SignAndEncrypt）。模拟器默认只绑定回环地址。
