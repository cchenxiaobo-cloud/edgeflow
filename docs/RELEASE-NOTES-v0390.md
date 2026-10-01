# EdgeFlow v0.39.0 发布说明（分级上送与补传）

- 发布日期：2026-10-01
- 基线：v0.38.0（5b23d2f）
- 主题：弱网上送与补传（发展规划 G16）——边缘持久补传队列（SQLite）、分级策略（优先级/批量/速率预算）、补传 worker（断网积压、恢复续传）、云端幂等接收（消息 ID 去重）、云端在途缓冲（etcd pending，重启不丢）、状态上报与可视化端点（+2）、e2e 断网恢复闭环。零新依赖；默认零行为（UPLINK 关闭时规则事件出口与 v0.38.0 逐字节兼容）；契约仅增（51→53 端点、12→13 消息）。

## N1 能力（新增）

### 1. 边缘持久队列（edge/pkg/metamanager）
- 表：`uplink_queue`（id 自增 / priority / msg JSON / created_at）+ `uplink_meta`（dropped/sent 持久计数）；基于既有 `Store` 单连接（SQLite，零新依赖）。
- API（`UplinkQueue` 类型）：`EnqueueUplink`（单条上限 64KB，超限拒绝）/ `DequeueUplinkBatch`（priority DESC, id ASC——高优先级先行、同级 FIFO）/ `AckUplink`（发送成功删除；未 Ack 集 = 待补传游标）/ `UplinkDepth`（Total/High/Normal/Low/Sent/Dropped/OldestTs）。
- 容量水位：默认 100000 行（`DefaultUplinkMaxRows`），超限丢「最低优先级中最老」逐条至达标；dropped 持久累计，丢弃告警限频。
- 崩溃自愈：重启重开队列，未 Ack 条目自动继续补传；损坏行跳过自愈（不阻断出队）。

### 2. 分级与策略（cmd/edgecore）
- 优先级映射：severity `critical→2 / warning→1 / info 及其它→0`。
- 批量（默认 32）/ 速率预算（默认 100 条/s，0=不限速；弱网调小）/ 状态上报周期（默认 30s）。
- 严格优先级序：高优先级持续积压时低优先级延迟（分级预期）。

### 3. 补传 worker（cmd/edgecore）
- 循环：入队唤醒（容量 1 非阻塞信号）+ 兜底轮询 2s → 批量出队 → 逐条 `client.Send` → 成功 `AckUplink`；失败本轮即停（不 Ack 留队下轮重试）——**至少一次**语义，重复由云端幂等消化。
- 离线/恢复限频日志（状态翻转触发，不逐轮刷屏）。
- 启动即消费残留队列（重启自动续传）；优雅停止留盘。

### 4. 云端幂等接收（cloud/pkg/cloudhub）
- 去重键 `protocol.Message.ID` 滚动集合（默认 10000，FIFO 淘汰最旧）；重复 → 丢弃不回调 + duplicated 计数；received/duplicated 提供 per-node 统计（`RuleEventStats`；全局可由 map 聚合得出）。
- 并发安全（-race 守卫用例）；对既有单发路径透明（不重发不命中）。

### 5. 云端在途缓冲（cloud/pkg/rulestore）
- `AppendEventBuffered`：etcd 写（`/edgeflow/ruleevents/<nodeID>/<key>`）→ 入内存 ring → 删键；云端在「已接收未处理完」窗口崩溃时，重启 `Load` 扫描恢复（不丢）。
- etcd 写失败返回错误，装配层降级内存环（Warn，不阻断事件管道）；kv=nil（测试/内嵌形态）退化为仅入环。

### 6. 边缘装配（opt-in，默认零行为）
- `EDGEFLOW_EDGECORE_UPLINK=on` 开启；参数：`_MAX_ROWS`（默认 100000）/ `_BATCH`（默认 32）/ `_RATE`（默认 100，0 不限）/ `_REPORT_SEC`（默认 30）；非法值告警回退默认。
- 开启后规则事件走补传 sink（Enqueue + 唤醒；规则事件台账共写不变）；Enqueue 失败 Warn + 直发兜底；队列初始化失败回退直发（不阻断启动）。
- 关闭（默认）时规则事件出口与 v0.38.0 直发路径逐字节等价（无新表写入、出口路径无新日志；仅当设置了非法参数 env 时启动期输出参数告警）。

### 7. 状态上报与云端可视化
- `UplinkReport` 消息（新增第 13 类，边→云）：depth/dropped/sent/oldestTs 周期上报（默认 30s；离线静默跳过，不进队列避免自引用）。
- 端点 +2（契约 51→53）：`GET /api/v1/uplink/overview`（各节点积压/丢弃/上送/接收计数聚合）、`GET /api/v1/nodes/{nodeID}/uplink`（单节点，无数据 404）；auth/audit 链自动覆盖。

## N2 兼容与冻结
- **默认零行为**：`EDGEFLOW_EDGECORE_UPLINK` 未开时，规则事件出口与 v0.38.0 直发逐字节一致。
- 云端幂等与在途缓冲对既有单发路径透明（不命中去重集；pending 写→入环→删闭环终态一致）；v0370 云端测试零改动。
- 契约仅增：51→53 端点、12→13 消息（+UplinkReport）；既有端点/消息逐字节不变；API-SPEC / API-COMPATIBILITY 已同步。
- v0240–v0350 冻结测试零改动；MQTT/OPC-UA/视频/模型面代码零触碰；pkg/rules 零改动；go.mod 零变化（队列复用既有 SQLite 依赖）。

## N3 测试（grep 口径）
- edge/pkg/metamanager +6 例（出队序与钳制 / Ack 与 sent 计数 / 容量丢弃序 / 超限拒绝 / 重开持久 / 坏行自愈）。
- cmd/edgecore +9 例（优先级映射 / env 解析回退 / 上报构造 / drain 成功 Ack 序 / 离线停轮不丢 / 速率上限 / 生命周期 / 入队 sink / Enqueue 失败兜底）。
- cloud/pkg/cloudhub +4 例（去重命中与计数 / 窗口淘汰 / 零值统计 / 并发 -race）。
- cloud/pkg/rulestore +4 例（写-删闭环 / etcd 错误降级 / Load 恢复 / nil-kv 内存形态）。
- cmd/cloudcore +4 例（overview 空态 / 合并排序 / 单节点 404 / 覆盖式更新）。
- e2e 1 例（真实双进程闭环：在线基线 → 停云断网 12s 持续触发积压 → 同端口/同数据目录重启云 → 补传完整（断网窗口事件全到达、队列清空、sent 对账 ≥ n1+n2）→ overview 可见；原始日志归档工作台）。
- 契约：53 端点契约守卫 + 静态路由扫描扩 uplink_api.go；运行时/文档一致性守卫同步。

## N4 边界（登记 KNOWN-ISSUES §40）
- 至少一次语义极端窗口：Send 写成功但未达（极端断连），云端幂等消化重复；
- 去重窗口容量（默认 10000）超窗后重复会被再接收；
- 容量丢弃序固定「低优先级中最老先丢」；高优先级持续积压挤占低优先级（分级预期）；
- 云端事件 ring 为内存窗口（重启后仅 pending 恢复，已处理历史不持久——既有边界延续）；
- etcd 写失败降级 ring-only（该条仅入内存环；边侧队列仍在）；
- 单条消息 64KB 上限（超限拒绝入队，经直发兑底尽力而为）；
- worker 速率/批量默认值面向常规网络，弱网需调小 `_RATE`；
- UPLINK off 时维持 v0.38 尽力而为直发语义（发送失败即丢——既有行为保留）。

## N5 门禁（2026-10-01 全绿）
- vet（全仓）/ 全仓回归（除 e2e/契约，41 包全绿）/ race（metamanager 8.8s + edgecore 6.8s + cloudhub 8.9s + rulestore 3.2s + cloudcore 23.7s）/ 契约 11.9s / e2e 全量 471.4s：**全绿（0 FAIL）**。
- 复核处置后复验：文档批修正（P1-1/P2-2/3/9/10）后契约复跑绿；e2e 修复后单跑 PASS 29.3s
  （n1=1 → n2=2 补传 2 条，队列清空，sent=3 received=2 duplicated=0）。
- 复核：P0=0 / P1=1（已修）/ P2×9（4 修文档 + 5 登记，详见工作台 review.md ⑥）。
- 原始日志归档：工作台 `.cluster/edgeflow-v0390/`（gates.log / e2e-debug.log / e2e-debug2.log）。
