# spec 0021 — 日志链路（G32 采集 + G11 运维日志查看）v0.48.0

## 1. 背景与目标

方案缺口 O4「日志采集链路：无」与 A5「运行监控/日志查看：日志面缺」。本版打通
**边缘采集 → 上行补传 → 云端存储 → 检索 API** 最小闭环：

- US-1 采集上行：边缘日志（文件 tail + 程序内事件）批量上行云端，断网走补传队列。
- US-2 云端存储：per-node 环形缓冲（默认 5000 条/节点）+ 可选 JSONL 持久化（16MB 轮转）。
- US-3 检索 API：查询/节点清单/节点概要/尾部快取/清空，共 5 端点。
- US-4 轮转与预算：边缘缓冲上限丢最旧、云端环形淘汰即轮转；查询性能预算。

## 2. 用户故事与验收

### US-1 边缘采集与上行

- 边缘 `logcollector`（**opt-in** `EDGEFLOW_EDGED_LOGCOLLECTOR=on`，默认关零行为）：
  - 源 A 文件 tail：`EDGEFLOW_EDGED_LOG_DIR` 下 `*.log` 追加 tail；启动从 EOF
    （不回放历史）；truncate 检测（size < offset）重置从头；1s 轮询。
  - 源 B 程序内 `Emit(source, level, message)`：scriptrunner 生命周期接线示范。
  - 行解析：`[LEVEL]` 前缀推断级别（debug|info|warn|error，缺省 info）；ts 取采集时刻。
  - 批量：满 batchN（默认 50，env 可调）或 5s flush；单条 message 截断 4KB；
    缓冲上限 1000 条，满丢最旧 + dropped 计数。
  - 上行：注入 uplinkDispatch（UPLINK on 入补传队列 / off 直发）——**断网不丢复用
    既有补传语义，零新队列**；发送写出成功即删行，重复由云端 msgID 去重消化。
- 验收：写入文件 → 云端检索可见（e2e）；Uplink 队列预注入 LogSync → 补传 → 可见。

### US-2 云端接收与存储

- 消息 `LogSync`（边→云，批量）：`{entries: [{ts, source, level, message}]}`；
  校验：entries 1..200、level 白名单、source ≤64 字符、message ≤4KB（截断容忍由边侧保证）。
- CloudHub `SetLogSyncHandler`：与 AlarmEvent 同构（not_registered/invalid Ack；
  通过 → msgID 滚动去重（容量 10000 FIFO，同 ruleEventDedup）→ 锁外回调）。
- `logstore.Store`：per-node 环形（`EDGEFLOW_CLOUDCORE_LOGSTORE_MAX` 默认 5000，
  淘汰最旧）；可选 JSONL 追加持久化（`EDGEFLOW_CLOUDCORE_LOGSTORE_FILE`，
  >16MB rotate `.old`）；启动 Load 回放（损坏行跳过）。

### US-3 检索 API（5 端点，契约 97→102）

- `GET /api/v1/logs`：node/source/level/q/since/until/limit/offset（node 缺省跨节点；
  limit 默认 200 上限 1000；q 子串匹配 message）。
- `GET /api/v1/logs/nodes`：有日志节点清单 + 条数 + 最新时间。
- `GET /api/v1/logs/nodes/{id}`：节点概要（sources/levels 计数）。
- `GET /api/v1/logs/nodes/{id}/tail?n=`：尾部 N 条（默认 100 上限 1000）。
- `DELETE /api/v1/logs/nodes/{id}`：清空节点日志（admin）。

### US-4 轮转与性能预算

- 边缘：缓冲满丢最旧（dropped 计数可观测）；云端：环形淘汰即轮转 + DELETE 手动清。
- 性能预算：5000 条存量下单查询 P50 < 1s（e2e 宽松断言 <2s 并记录实际值）。

## 3. 设计裁决

1. **消息只增不改**：新增 TypeLogSync（16→17）；既有 16 条零改动。
2. **复用通道零新队列**：上行复用 uplink 补传队列；「断网不丢」= 队列持久化 +
   恢复续传，语义与 v0.39 完全同构。
3. **删除行时机**：写出成功即 AckUplink（与 alarm/rule 同口径）——应用层处理失败
   不重发（单向流式）；msgID 去重窗口消化 Ack 失败窗口的重复。
4. **文件行无可靠 ts**：统一取采集时刻（文件 mtime/行内时间解析不做——登记边界）。
5. **cloud 端无条件启用**（内存模默认 on）；文件持久化可选。无 503 分支。
6. **scriptrunner 事件接线**：deploy 成功/失败/卸载三态 Emit（sink 可选注入，nil 安全）；
   appdeploy 边缘侧本版不接。
7. **offset 不持久化**：重启从 EOF（丢重启窗口内新增行——轻量采集定位，登记）。
8. **保留天数**：本版环形容量即保留策略；按天清理留后续（KI 登记）。

## 4. 交付锚

- 测试：logcollector 6 例 / logstore 6 例 / cloudhub 1 例 / e2e 1 例（US-1..US-4）。
- 契约：端点 97→**102**（routes.go +5 = 102）；消息 16→**17**（LogSync）；
  四件套对齐 + 探测分支（/api/v1/logs 接受 200/400/404）。
- env 清单：EDGEFLOW_EDGED_LOGCOLLECTOR / EDGEFLOW_EDGED_LOG_DIR /
  EDGEFLOW_EDGED_LOG_BATCH / EDGEFLOW_EDGED_LOG_FLUSH_SEC /
  EDGEFLOW_CLOUDCORE_LOGSTORE_MAX / EDGEFLOW_CLOUDCORE_LOGSTORE_FILE。
## 5. 交付锚（2026-10-19，v0.48.0）

- 端点 97→**102**（日志面 5：GET /api/v1/logs、GET /api/v1/logs/nodes、
  GET /api/v1/logs/nodes/{nodeID}、GET /api/v1/logs/nodes/{nodeID}/tail、
  DELETE /api/v1/logs/nodes/{nodeID}）；消息 16→**17**（LogSync）。
- 代码落点：edge/pkg/logcollector（采集）、pkg/protocol/log.go（LogBatch/LogEntry+校验）、
  cloud/pkg/logstore（环形+JSONL）、cloud/pkg/cloudhub/log_event.go（接收+dedup）、
  cmd/cloudcore/v0480_log_api.go（API+装配）、cmd/edgecore/main.go（dispatch+sink 接线）、
  edge/pkg/edged/scriptrunner.go（SetEventSink 三态 emit）。
- 测试 14 例全绿（logcollector 6/logstore 6/cloudhub 1/e2e 1）；race ×6 包绿；
  契约 13.87s（102 端点/17 消息）；定向 e2e 22.284s（US-1..5）。
- e2e 期修复：①q 参数 URL 字面空格 400（改单词过滤）；②LOGSTORE_MAX env 守卫
  n<10 误拒小值（放开为 n≤0 回退默认）。
- 复核处置（FAIL→放行，P1×3/P2×6 全修 + P3 快修 6 留档 3 + 自查 burst 背压）：P1——Query 递归 RLock 死锁（节点收集内联）/首见文件全量回放（启动快照二分：scanInitial 登记既有 EOF、新文件从 0）/64KB 无换行热自旋（offset 恒前进 + carry 半行缓冲 + 1MB 单行上限）；P2——UTF-8 净化（sanitizeMessage，GBK 不再整批静默丢失）/source rune 对齐/X-Total-Count=过滤后总数/e2e 真实容量重构/背压内容断言/LogSync 逐消息 nodeID 比对；自查——tailOnce 内 maybeFlush 同步放水（burst 超 1000 缓冲不吞行）。「启动从 EOF」语义固化为启动快照二分（启动前=历史跳过，启动后=增量收集）。
