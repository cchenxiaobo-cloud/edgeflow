# EdgeFlow v0.15.0 发布说明（OPC-UA 里程碑第三阶段：Subscription 订阅推送 + Browse 浏览发现）

- **发布日期**：2026-08-27
- **版本基线**：v0.14.0（2026-08-27）→ v0.15.0
- **主题**：OPC-UA 里程碑第三阶段（ROADMAP WBS 5.2）——设备采集从"轮询批量读"升级为"订阅即时推送"，新增 Browse 一键点位发现；补齐互操作试解分派缺陷修复
- **兼容性**：**零新增端点**（总数维持 32）、新增 **1 个 env**（`EDGEFLOW_OPCUA_SUBSCRIPTION`，边缘侧 opt-in，缺省 off 与 v0.14.0 行为逐字节一致）、升级零迁移、老边缘零动作、**零新依赖**（go.mod 不变）
- **数值权威性**：全部订阅/Browse 服务 TypeId 经 OPC Foundation UA-Nodeset v1.04 官方 NodeIds.csv 核验（CreateSubscription=787/790、CreateMonitoredItems=751/754、Publish=826/829、Browse=527/530、DataChangeNotification 内容=811 等）

## 一、核心能力

### 1. Subscription 订阅推送端到端（OPC-D）

- **pkg/opcua**：CreateSubscription / CreateMonitoredItems / Publish / DeleteSubscriptions 全套消息编解码 + NotificationMessage（DataChange/StatusChange，EventList 占位跳过）+ 高层 `Subscribe/PubAck/DeleteSubscription` API
- **泵模式**：首次订阅启动唯一读 goroutine，帧按 RequestId 三级路由（waiter 表 → pending 兜底缓冲 → 在途 Publish）；未启用订阅时收发路径与 v0.14.0 逐字节一致
- **opcuasim 订阅引擎**：per-conn 会话态 + 步进评估变化推送 + KeepAlive 空通知 + store-and-forward 信封队列（≤32，满丢最旧 KeepAlive 保数据）+ 悬挂 Publish 回填 RequestId + DeleteSubscriptions/断开清理
- **mappers/opcua**：`EDGEFLOW_OPCUA_SUBSCRIPTION=on`——supervisor goroutine 消费推送→缓存快照→Collect 短路返回；HandleCommand 回读验证后同步刷新缓存；序列号跳变或断线自动重建订阅

### 2. Browse 点位发现（OPC-E）

- **pkg/opcua**：BrowseRequest/Response + ReferenceDescription + ExpandedNodeId 最小形式 + `Client.Browse`
- **opcuasim 两级最小目录**：Objects(i=85) → opcua-sim(ns=2;i=5000) → 6 变量节点
- **hack/opcua-browse CLI**：打印可直接粘进 `EDGEFLOW_OPCUA_NODES` 的 `name=nodeId` 行

### 3. 互操作缺陷修复（INTEROP/WIRE）

- **试解防误吞**：全部请求解码器强制"字节全消费"校验——此前 Browse 帧会被 Publish 试解器部分消费吞掉（实弹排障发现），现在形状不符即交回分派链下一分支
- **异步出站帧头**：模拟器 notifyPush 异步出站统一补 MSG 帧头（此前对称头+序列头直写导致客户端解析错位）

## 二、验证摘要

- 全量 `go test -race ./...` **37 包全绿**；go vet/gofmt 干净；新增文件 golangci-lint 0 issues
- 新增测试：进程内闭环 TestSubscriptionPushE2E（数据推送到达）/ TestBrowseDirectory（两级目录）/ TestSubscriptionWriteTriggers（写触发）；单包 round-trip 表测 ×9 结构 + 并发压测（-race）
- **TestOPCUASubscriptionE2E 云边闭环通过（71s）**：模拟器→edgecore 订阅装配→云端属性可见→指令写 setpoint=200→Desired 出现→Properties 收敛→模拟器侧确认
- 回归口径：既有 TestOPCUADeviceE2E（轮询模式）不修改且全量回归中保持通过（缺省行为零变化实证）

## 三、升级注意

- **零迁移**：无键空间/schema 变化；老边缘零动作；云端零改动
- **行为差异面（仅 EDGEFLOW_OPCUA_SUBSCRIPTION=on 时）**：
  - Collect 返回推送缓存快照而非实时批量 Read（新鲜度由服务器发布间隔决定，模拟器=步进周期）
  - 可写点位经 HandleCommand 写入后缓存立即刷新（回读验证通过后），不等服务器下一拍
- 缺省 off：轮询模式逐字节不变；回滚清 env 即回到 v0.14.0 行为；混跑禁令延续

## 四、文档同步

KNOWN-ISSUES §15 登记（OPC-D/E + INTEROP/WIRE 两缺陷闭环）、API-COMPATIBILITY v0.15.0 段、DEPLOYMENT §14.5 订阅模式说明、OPCUA-GUIDE §3.1/§4.1、README 当前版本+历史行、Chart 0.15.0、ROADMAP WBS 5.2 第三阶段完成、RELEASE-NOTES-v0115（本文）、两册手册版本推进

## 五、遗留（非阻断，见 KNOWN-ISSUES §15）

- Sign/SignAndEncrypt 安全策略（安全里程碑主候选）
- Republish 补帧（当前 gap 即重建，数据集无状态自愈够用）
- EventNotificationList 解码（事件订阅 Alarm&Condition 整体排除中）
- 第三方 UA 栈互操作 cross-check（node-opcua/open62541 环境就绪后）
