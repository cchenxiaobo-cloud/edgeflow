# Spec 0012：分级上送与补传（G16）

- 版本归属：v0.39.0（基线 5b23d2f = v0.38.0）
- 规范依据：《EdgeFlow 后续开发计划（对齐智能边缘计算平台解决方案）》v0.39 定义（差距 G16 弱网上送与补传）
  + 用户方向「继续开发后续功能，形成新的版本」
- 裁定结论（本 spec 前置决策）：
  - 全链交付：边缘持久补传队列（metamanager 新表）→ 分级策略（优先级/批量/速率预算）→
    补传 worker（断网积压、恢复续传）→ 云端幂等接收（`protocol.Message.ID` 去重）→
    云端在途缓冲（etcd pending，重启不丢）→ 可视化（+2 端点 +1 消息）→ e2e 断网恢复闭环。
  - 投递语义：**至少一次**（发送失败不删、恢复重发；重复由云端幂等消化）。
    不引入云端逐条确认（保持 RuleEvent 单向流式语义，避免既有行为变更）。
  - 默认零行为：`EDGEFLOW_EDGECORE_UPLINK` 未开时，规则事件出口与 v0.38 直发路径逐字节等价。
  - 云端幂等与在途缓冲对既有单发路径透明（不命中去重集；pending 写→入环→删闭环，终态一致）。
  - 零第三方依赖（宪法 II）；冻结测试（v0240–v0350）零改动；MQTT 路径零触碰。
  - 范围边界：本版队列/上报面服务于**规则事件链路**（视频告警等后续复用同一设施）。

## 用户故事与验收

### US-1 边缘持久队列（edge/pkg/metamanager）
- 表：`uplink_queue(id INTEGER PRIMARY KEY AUTOINCREMENT, priority INTEGER NOT NULL DEFAULT 0,
  msg TEXT NOT NULL, created_at INTEGER NOT NULL)` + `uplink_meta(k TEXT PRIMARY KEY, v INTEGER NOT NULL)`。
- API（`UplinkQueue` 类型，基于既有 `Store` 单连接）：
  - `NewUplinkQueue(store *Store, maxRows int) (*UplinkQueue, error)`（maxRows<=0 → 默认 100000；建表幂等）；
  - `EnqueueUplink(priority int, msg *protocol.Message) (int64, error)`（msg 存 JSON——含 ID，
    幂等键随消息保留）；单条上限 64KB（超限拒绝 + 错误）；
  - `DequeueUplinkBatch(limit int) ([]UplinkItem, error)`：`UplinkItem{ID int64; Priority int; Msg *protocol.Message}`；
    排序 `priority DESC, id ASC`（高优先级先行、同级 FIFO）；limit<=0 用默认 32；
  - `AckUplink(id int64) error`（发送成功删除——未 Ack 集=待补传集合，即游标语义）；
  - `UplinkDepth() (UplinkStats, error)`：`UplinkStats{Total, High, Normal, Low int; Sent int64; Dropped int64; OldestTs int64}`。
- 容量：入库后若行数超 maxRows → 丢弃 `priority ASC, id ASC` 序首条（最低优先级中最老），
  逐条至达标；`dropped` 持久累计（uplink_meta）；丢弃限频 Warn。
- 计数：`sent` 持久累计（Ack 时 +1）。
- 验收（单测）：入队/出队序（优先级 + 同级 FIFO）/Ack 删除与 sent 计数/深度统计/超限丢弃（低优最老先丢）/
  拒超大消息/重开持久（未 Ack 仍在）/计数持久。

### US-2 分级与策略（cmd/edgecore）
- 优先级映射：severity `critical→2 / warning→1 / info 及其它→0`。
- 批量：worker 每轮最多 `batch` 条（默认 32）。
- 速率预算：每秒最多 `rate` 条（默认 100；弱网调小）——跨轮次睡眠控制。
- 饿死语义：严格优先级序；高优持续积压时低优延迟（分级预期，边界登记）。
- 乱序语义：不同优先级交错到达属预期；云端幂等不依赖到达序。

### US-3 补传 worker（cmd/edgecore）
- 循环：唤醒信号（新入队，容量 1 非阻塞）或兜底周期（默认 2s）→ 批量出队 → 逐条
  `client.Send` → 成功 `AckUplink`；失败（未连接/写错）→ 本轮停止（不 Ack，留队下轮重试）。
- 发送成功定义：`Send` 返回 nil（已写入连接）；极端断连窗口（写成功未达）不在保证内（KI 登记），
  云端幂等消化可能重复。
- 启动即运行：重启后残留队列自动继续消费；优雅关闭：stop 退出（残留留盘）。
- 验收（单测，fake client）：成功 Ack 序列、离线停止不丢、恢复续传、速率上限生效、stop 退出。

### US-4 云端幂等接收（cloud/pkg/cloudhub）
- 去重键 `protocol.Message.ID`；滚动集合（容量默认 10000，FIFO 淘汰最旧；超窗重复会被再接收——边界登记）。
- 行为：重复 → 丢弃（不回调 handler）+ `duplicated` 计数；首次 → 正常回调 + `received` 计数。
- 计数：per-node + 全局；`(s *Server) RuleEventStats() map[string]RuleEventCounters`
  （`RuleEventCounters{Received, Duplicated int64}`）。
- 对既有单发路径透明（不重发不命中）。
- 验收（单测）：重复丢弃、计数、新 ID 通过、并发安全（-race）。

### US-5 云端在途缓冲（cloud/pkg/rulestore）
- 接收持久：`AppendEventBuffered(ctx, nodeID string, ev rules.Event) error`：
  生成 pending key（`/edgeflow/ruleevents/<nodeID>/<unixNano>-<rand4>`，唯一性由时间+随机保证）→
  写 etcd → 入 ring → 删 pending。
- 启动恢复：`Load` 扩展——扫描 `/edgeflow/ruleevents/` 前缀 → 逐条 AppendEvent（恢复）→ 删除 → 记日志。
- 降级：etcd 写失败 → 返回错误；装配层 Warn 并回退 `AppendEvent(ev)`（ring-only，不阻断管道；边界登记）。
- ring 保持内存窗口语义（重启后仅 pending 恢复；已处理历史不持久——既有边界）。
- 验收（单测）：写-删闭环（无残留）、预置 pending → Load 恢复 → 清除、etcd 错降级路径。

### US-6 边缘装配（opt-in）
- 开关：`EDGEFLOW_EDGECORE_UPLINK=on`（默认 off/其它值 → v0.38 直发逐字节等价）。
- 参数：`EDGEFLOW_EDGECORE_UPLINK_MAX_ROWS`（默认 100000）、`_BATCH`（默认 32）、
  `_RATE`（默认 100）、`_REPORT_SEC`（默认 30）；非法值告警回退默认。
- 装配：on 时 sink 走 `newUplinkRuleEventSink`（Enqueue + 唤醒；Enqueue 失败 → Warn + 直发兜底）；
  worker 随启动、随停机序列关闭（残留留盘）；队列初始化失败 → Warn + 回退直发（不阻断）。
- 状态上报：`UplinkReport` 消息（TypeUplinkReport；周期 `reportSec`；payload
  `{depth, dropped, sent, oldestTs}`）——离线发送失败静默跳过（不进队列，避免自引用）。
- 验收（单测）：开关解析/参数回退/on 走队列/Enqueue 失败兜底/worker 生命周期/上报构造。

### US-7 云端可视化端点（契约扩容）
- `GET /edgeflow/api/v1/uplink/overview` → `{"nodes":[{"nodeId","depth","dropped","sent","oldestTs",
  "lastReportTs","received","duplicated"}]}`（有上报或计数的节点）。
- `GET /edgeflow/api/v1/nodes/{nodeID}/uplink` → 单节点同结构；无任何数据 → 404。
- 数据源：UplinkReport 缓存（cloudcore 装配，覆盖式含 lastReportTs）+ hub 计数。
- UplinkReport 处理（cloudhub）：未注册 → Ack not_registered；解析失败 → invalid_message；
  成功 → 缓存（回调注入）不另回 Ack（与 RuleEvent 同约定）。
- 验收（单测 + 契约测试）：overview 数组/单节点/404/上报更新/契约四组守卫。

## 冻结兼容
- 默认（UPLINK off）：edgecore 规则事件出口 = v0.38 直发路径（逐字节）；无新行为。
- 云端：幂等与缓冲对单发路径透明（终态一致）；v0370 云端测试零改动。
- 契约只增：51→53 端点、14→15 消息（总口径含 2 占位，对应活跃口径 12→13；仅增不改）；API-SPEC/API-COMPATIBILITY 同步。
- v0240–v0350 冻结测试零改动；pkg/mqtt 零触碰；pkg/rules 零改动；go.mod 零变化。

## 测试锚（as-built 回填，2026-10-01）

- edge/pkg/metamanager +6：TestUplinkQueueOrderAndClamp / TestUplinkQueueAckAndSentCount /
  TestUplinkQueueCapacityDrop / TestUplinkQueueRejectsOversize / TestUplinkQueuePersistsAcrossReopen /
  TestUplinkQueueBadRowSelfHeal。
- cmd/edgecore +9：TestUplinkPriorityForSeverity / TestParseUplinkOptionsFromEnv /
  TestBuildUplinkReportMessage / TestRelayDrainSuccess / TestRelayDrainOfflineStops /
  TestRelayRateLimit / TestRelayStartNotifyStop / TestUplinkSinkEnqueue /
  TestUplinkSinkFallsBackOnEnqueueError。
- cloud/pkg/cloudhub +4：TestRuleEventDedupCheckAndCount / TestRuleEventDedupEviction /
  TestServerRuleEventStatsZeroValue / TestRuleEventDedupConcurrent。
- cloud/pkg/rulestore +4：TestAppendEventBufferedWriteDelete / TestAppendEventBufferedPutError /
  TestLoadRestoresPendingEvents / TestAppendEventBufferedNilKV。
- cmd/cloudcore +4：TestV0390UplinkOverviewEmpty / TestV0390UplinkStateMergeAndSort /
  TestV0390UplinkNodeNotFound / TestV0390UplinkStateOverwrite。
- tests/e2e +1：TestV0390UplinkE2E（真实双进程：在线基线 → 停云 12s 持续触发 →
  同端口/同数据目录重启云 → 断网窗口事件全补传 + 队列清空 + sent 对账 ≥ n1+n2 + overview 可见）。
- 合计：单测 27 例 + e2e 1 例（grep 口径，与 RELEASE-NOTES-v0390 N3 一致）。

## 边界登记（随文档落 KI §40）
- 至少一次语义的极端窗口（Send 成功未达）；
- 去重窗口容量（超窗重复再接收）；
- 丢弃序（低优最老先丢）与高优挤占低优（预期）；
- ring 内存窗口（重启后仅 pending 恢复）；
- etcd 写失败降级（ring-only）；
- 单条 64KB 上限；
- worker 速率/批量默认值（弱网调小）；
- UPLINK off 时维持 v0.38 尽力而为语义（发送失败即丢——既有行为保留）。
