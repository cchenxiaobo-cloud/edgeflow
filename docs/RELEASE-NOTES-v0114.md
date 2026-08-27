# EdgeFlow v0.14.0 发布说明（OPC-UA 里程碑：端到端 Mapper v1）

- **发布日期**：2026-08-27
- **版本基线**：v0.13.0（2026-08-26）→ v0.14.0
- **主题**：OPC-UA 里程碑第二阶段（ROADMAP WBS 5.2，唯一 🟨 进行中模块）——把 v0.3.0 的 UA Binary 协议栈核心（仅编解码/HEL/ACK）补成**端到端设备接入**：SecureChannel → Session → Read/Write 服务 → Client API → Mapper → 边缘采集管线 → 云侧设备属性可见
- **兼容性**：**零新增端点**（总数维持 32）、新增 **4 个 env**（边缘侧显式 opt-in，缺省零行为变化）、升级零迁移、老边缘零动作、**零新依赖**（go.mod 不变，全标准库）
- **设计**：设计 Agent 定稿 design.md（566 行六节；实测勘察 pkg/opcua 四缺口：SecureChannel/服务层/DiagnosticInfo/Mapper Client API）

## 一、核心能力

### 1. pkg/opcua 补全为端到端协议栈（OPC-A，全标准库）

- **SecureChannel 层**（securechannel.go）：OPN/CLO 通道生命周期 + Asymmetric/Symmetric 安全头 + SequenceHeader 序列号与 RequestId 响应关联；OPN 成功后 conn.channelId 生效——**既有导出 API 零变更**
- **Session 匿名会话**：CreateSession → ActivateSession（AnonymousIdentityToken）→ CloseSession
- **Read/Write 服务**：批量读点（Results 与请求一一对应）/ 单点写 + 复合类型（RequestHeader/ResponseHeader/ApplicationDescription/EndpointDescription 等）
- **DiagnosticInfo 位域补全**（diagnostic.go）：ResponseHeader 解码必需；Variant 内嵌 0x19 保持不支持（v1 边界）
- **ParseNodeID**（nodeid.go）：`ns=2;i=1001` 等五形式解析，与 NodeId.String() 互逆
- **server_api.go**：服务端互操作导出面（供模拟器/未来服务器适配）

### 2. 自研 OPC-UA 模拟服务器（pkg/opcuasim + hack/opcua-sim）

SecurityPolicy None 匿名会话全协议服务端（HEL/ACK→OPN/OPNF→MSG 服务分派→CLO）；6 点位动态模型（温度向 setpoint 收敛+扰动、湿度/压力游走、running/label 只读、setpoint 可写）；默认只绑 `127.0.0.1:14840`。自研客户端 × 自研服务端双向交叉验证（与 modbussim 同构）。

### 3. mappers/opcua Mapper（OPC-B 读链路 + OPC-C 写链路）

- **Collect 批量读点**：Variant→float64 转换契约（数值全支持 / Boolean→0,1 / String 尝试 ParseFloat；其余类型跳过+Warn）；节点 Status 非 Good（如 BadNodeIdUnknown）该属性跳过不阻塞
- **HandleCommand 写点回读**：命中点位名 → Write(Double) → 回读验证（容差 1e-6）；只读节点写入被服务端拒绝（BadNotWritable）透传 502
- **台账**：全部操作落 op_ledger（SQLite，30 天保留）；断线重连一次重试
- **装配**：`EDGEFLOW_OPCUA_ENDPOINT/NODES/DEVICE_NAME/NAMESPACE` 四 env opt-in；ENDPOINT 缺省空 → 不注册 → 存量部署逐字节不变

### 4. E2E 云边闭环（真实装配路径）

`TestOPCUADeviceE2E`：测试内起模拟器 → edgecore env 装配 → DeviceReport 上报 → `/api/v1/devices` 可见 opcua-device-01 全部点位 → 下发 device-command 写 setpoint=200 → Desired 出现 → Properties 收敛 200 → 模拟器侧确认写入。

### 5. E2E 基建修复（E2E-BASE）

cloudcore 嵌入式 etcd 数据目录原为相对 cwd 共享路径（data/etcd），跨用例残留旧台账导致注册/在线判定串台——cloudEnv 为每个用例注入独立 `EDGEFLOW_CLOUDCORE_ETCD_DATA_DIR`（同用例重启共享保持台账恢复语义）。

## 二、验证摘要

- 全量 `go test -race ./...` **37 包全绿**（v0.13.0 为 35 包：新增 pkg/opcuasim + mappers/opcua）；go vet 干净；gofmt 干净；新增包 golangci-lint 0 issues
- 新增单测：pkg/opcua（DiagnosticInfo round-trip/保留位拒绝/列表、ParseNodeID 表测+互逆、服务消息 round-trip、mock 服务端全链路 Open→Read→Write→Close/BadNode/OPN 拒绝）、opcuasim 6 用例（全协议闭环/未知节点/只读写/收敛/连接上限/点位表）、mappers/opcua 9 用例（转换策略/Bad 跳过/写点回读/未知属性/幂等/断线恢复/ParseNodes）、edgecore 装配门控 3 分支
- `hack/opcua-e2e` 全自动通过（采集 5 点位 → 写 setpoint=200 回读一致 → 台账 up/down/up 三条记录）
- 既有 41 个 opcua 协议栈测试原样通过（导出面零变更验证）

## 三、升级注意

- **零迁移**：无键空间/schema 变化；台账复用既有 op_ledger 表
- **老边缘零动作**：OPC-UA Mapper 缺省不启用（EDGEFLOW_OPCUA_ENDPOINT 空）；云侧零改动
- **行为差异面（仅启用 OPC-UA 时）**：
  - SecurityPolicy None 明文无认证，**仅限可信隔离网络**（延续 doc.go 既有边界）；生产暴露需等 Sign/SignAndEncrypt 里程碑
  - Variant→float64 转换边界（DateTime/Guid 等类型属性不上报，Warn 日志提示）
  - edgecore 每 report 周期对配置点位发起一次批量 Read（网络量与点位数成正比）
- 回滚 v0.14.0 → v0.13.0：清掉 EDGEFLOW_OPCUA_* env 即回到 v0.13.0 行为；混跑禁令延续（全停再全起）

## 四、文档同步

KNOWN-ISSUES（§14 登记 OPC-A/B/C/E2E-BASE + §13 残余批注登记后续）、API-COMPATIBILITY（v0.14.0 段：4 新 env 登记）、DEPLOYMENT（§14 OPC-UA 设备接入：env 表/快速开始/安全边界/接真实设备）、README（当前版本 v0.14.0 + 版本历史行）、Chart 0.14.0、ROADMAP（WBS 5.2 ✅ 第二阶段完成）、docs/OPCUA-GUIDE.md（新建）、RELEASE-NOTES-v0114（本文）、两册手册同步版本

## 五、遗留（非阻断，见 KNOWN-ISSUES §14）

- Browse 节点发现 / Subscription 订阅推送 / Sign·SignAndEncrypt 安全策略：登记后续里程碑（v0.15.0 候选）
- 第三方 UA 栈互操作 cross-check（node-opcua/open62541）：本机环境缺失，登记后续
- §13 两运维残余（跨前缀 txn 原子化 / offlineAt 持久化）：设计评估后登记 v0.15.0 候选
