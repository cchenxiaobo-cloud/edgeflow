# Spec 0013：告警事件全链 + 设定值通道（G26）

- 版本归属：v0.40.0（基线 a9165b5 = v0.39.0）
- 规范依据：《EdgeFlow 后续开发计划（对齐智能边缘计算平台解决方案）》v0.40 定义（差距 G26
  告警事件与消息联动）+ 用户方向「继续开发后续功能，形成新的版本」
- 裁定结论（本 spec 前置决策）：
  - 全链交付：pkg/alarm 告警模型（等级/生命周期/去重）→ 边缘告警管理器（规则触发源挂点、
    去重聚合、本地台账、本地联动接口留位、补传队列接入）→ 云侧统一告警中心
    （etcd 写穿、收/派/闭环、统计、ack/assign/close API、工单集成点留位）→
    设定值通道强化（建单/审批选项/可靠投递/执行反馈闭环/留痕）→ 断网语义（告警断网缓存
    补传不丢；设定值断网缓存执行、恢复重投同步）→ e2e 全链与断网闭环。
  - 告警生命周期（云侧）：raised → acked / assigned → closed（closed 终态；状态只进不退；
    边侧重发仅合并 count 与时间戳，不回退状态）。边侧不自动产生 cleared（v0.37 规则链
    无 cleared 事件——边界登记，后续版本扩展恢复源）。
  - 默认零行为与 opt-in：告警链随规则启用自然生效（无规则时零行为）；设定值审批默认 off
    （EDGEFLOW_CLOUDCORE_SETPOINT_APPROVAL=on 开启）；本地联动仅 logLinkage（声光/消息
    联动为接口留位，不在本版承诺）。
  - 断网告警闭环依赖 v0.39 补传（EDGEFLOW_EDGECORE_UPLINK=on）；off 时告警与规则事件同为
    尽力而为直发（边界登记）。
  - 设定值通道不替代现场控制系统安全联锁；控制类数据缓存/校验/恢复须经场站工艺与安全部门
    评审后启用（随文档登记 KI §41）。
  - 零第三方依赖（宪法 II）；冻结测试（v0240–v0350）零改动；MQTT/OPC-UA/视频/模型面零触碰；
    go.mod 零变化。

## 用户故事与验收

### US-1 告警模型（pkg/alarm，共享包）
- `Alarm`：AlarmID / NodeID / Source（rule|device|system）/ Namespace / DeviceName / RuleID /
  Severity（critical|warning|info）/ State / Message / Count / RaisedAt / UpdatedAt /
  AckedBy+AckedAt / AssignedTo+TicketRef / ClosedBy+ClosedAt。
- 状态机：raised→acked、raised→assigned、acked→assigned、（非 closed）→closed；closed 终态；
  非法迁移返回错误；`StateRank`（raised=0 < acked=1 < assigned=2 < closed=3）供乱序守卫。
- `DedupKey()` = node|source|ruleID|namespace|device；Validate 全量校验。
- 验收（单测）：状态机合法/非法迁移、Validate、DedupKey、JSON 往返。

### US-2 边缘告警台账与去重聚合（edge/pkg/metamanager + cmd/edgecore）
- AlarmLedger（SQLite，照 RuleLedger 模式）：alarm_ledger 表（alarm_id PK / dedup_key /
  state / severity / alarm JSON / ts）+ 索引；Save/Upsert（同 ID 覆盖）、ActiveByDedupKey、
  ListRecent（as-built 修订：以 CountAlarms 诊断计数替代，按 dedup_key 活跃查询覆盖聚合需要）。
- alarmManager（cmd/edgecore）：规则事件入口 `ObserveRuleEvent(ev)`（severity 映射等级；
  source=rule）→ active episode（dedupKey 命中且 state≠closed）→ count++ + UpdatedAt 刷新 +
  重发节流（每 reannounceSec 默认 60s 或每 10 次命中重发同 alarmID）；新 episode → 生成
  AlarmID（dedupKey+首触时间散列）→ 台账写穿 → dispatch 上行。
- 本地联动：`Linkage` 接口（OnAlarm(Alarm)）+ logLinkage 默认实现（告警留痕日志）；
  接口留位：声光/消息联动后续版本接入。
- dispatch(priority, msg)：UPLINK on → UplinkQueue（priority 同 severity 映射）+ 唤醒；
  off → client.Send 尽力而为（与规则事件 v0.38 语义一致）。
- 验收（单测）：首触建单、聚合计数、重发节流、台账写穿、dispatch 两路径、联动回调。

### US-3 云侧统一告警中心（cloud/pkg/alarmstore + cloudhub + cloudcore）
- alarmstore：etcd 写穿（/edgeflow/alarms/<alarmID>，扁平键与 rulestore 风格一致；as-built 修订）+ 内存索引 + Load 恢复
  （损坏条目跳过）；Upsert 状态守卫（StateRank 只进不退；closed 不可复活；count 合并取大）；
  Ack/Assign/Close（operator 必填，写入操作者与时间戳）；List(nodeID/state/severity/limit)；
  Stats（byState/bySeverity）；kv=nil 内存形态（测试/内嵌）。
- 工单集成点：`TicketSink` 接口（OnAssign(Alarm) error）——默认 logTicketSink（留位：ITSM
  对接后续版本）；assign 成功后回调，回调失败仅告警不阻断。
- cloudhub：TypeAlarmEvent（边→云）接收——未注册 → Ack not_registered；解析/校验失败 →
  Ack invalid_message；成功 → 回调注入（不另回 Ack，同 RuleEvent 约定）。
- API（5 端点，operator 必填，auth/audit 链自动覆盖）：
  - GET /api/v1/alarms?nodeID=&state=&severity=&limit= → 列表（按 UpdatedAt 降序）
  - GET /api/v1/alarms/stats → {"byState":{...},"bySeverity":{...},"total":N}
  - POST /api/v1/alarms/{alarmID}/ack {"operator"} → 200/404/409
  - POST /api/v1/alarms/{alarmID}/assign {"operator","assignee","ticketRef"?} → 200/404/409
  - POST /api/v1/alarms/{alarmID}/close {"operator"} → 200/404/409
- 验收（单测）：Upsert 状态守卫与 count 合并、Load 恢复、List 过滤、Stats、操作 API 状态机
  409、工单回调、etcd 降级路径。

### US-4 设定值建单与审批（cmd/cloudcore setpoint_api.go + cloud/pkg/setpointstore）
- Setpoint 记录：SetpointID（服务端生成）/ NodeID / Namespace / DeviceName / Property /
  Value / State（pending-approval|pending-send|sent|applied|failed|rejected）/ Operator /
  RequireApproval / CreatedAt / UpdatedAt / Error / OutcomeAt；etcd 写穿
  /edgeflow/setpoints/<setpointID>（扁平键，as-built 修订）+ Load 恢复；kv=nil 内存形态。
- POST /api/v1/nodes/{nodeID}/setpoints {"namespace","deviceName","property","value",
  "operator","requireApproval"?}：requireApproval 或审批开关 on → pending-approval；
  否则 pending-send；节点不存在 → 404；校验失败 → 400。
- POST /api/v1/setpoints/{setpointID}/approval {"action":"approve"|"reject","operator"}：
  仅 pending-approval 可审（否则 409）；approve → pending-send；reject → rejected（终态）。
- GET /api/v1/setpoints?nodeID=&state=&limit= → 列表（含 outcome/error，执行反馈可查）。
- 投递：flush 循环（EDGEFLOW_CLOUDCORE_SETPOINT_FLUSH_SEC 默认 30，e2e 可调小）扫描
  pending-send → ReliableSend(DeviceCommand{class=setpoint,setpointId,...}) → 成功 MarkSent
  （SentAt）；节点离线（ErrNodeOffline）留单下轮重投；ErrAckTimeout 留单（同 ID 重发幂等）；
  ErrAckFailed → failed（执行被边缘拒绝，记录 error）。
- 审批开关：EDGEFLOW_CLOUDCORE_SETPOINT_APPROVAL=on 时全部建单进入 pending-approval
  （默认 off 零变化；requireApproval=true 单笔强制审批）。
- 验收（单测）：建单两路径、审批状态机（409/终态）、flush 重投（离线留单、AckTimeout 留单、
  AckFailed 记失败）、Load 恢复、列表过滤。

### US-5 执行反馈闭环（edge/pkg/devicetwin + cmd/edgecore + cloudhub）
- DeviceCommandPayload 仅增可选字段：Class（"setpoint"=设定值，缺省普通指令零变化）、
  SetpointID（云侧建单 ID，回告关联键）。
- handleDeviceCommand：class=setpoint 且 SetpointID 非空 → 执行（executor + Twin.Desired，
  语义同既有指令）→ 写 metamanager setpoint_cache（namespace|property UPSERT：value/
  setpointID/ts——断网缓存语义）→ 构造 TypeSetpointResult{setpointId, ok, value, error?, ts}
  → dispatch 上行（UPLINK on 入队 priority=normal；off 直发尽力而为）。
  普通指令（无 setpointID）路径逐字节不变。
- cloudhub：TypeSetpointResult 接收（未注册/解析失败 Ack 语义化；成功回调注入）→
  setpointstore.ApplyResult（sent/pending-send→applied/failed 终态；未知 setpointID 忽略并记 Info 日志——as-built 修订）。
- 验收（单测）：回告构造、缓存 UPSERT、普通指令零变化、云端 ApplyResult 状态机与未知 ID。

### US-6 断网语义（验收核心）
- 告警：UPLINK on 断网 → AlarmEvent 入 UplinkQueue（本地 alarm_ledger 同时留痕）→ 恢复后
  补传（v0.39 relay）；云端重启不丢（在途缓冲沿用 v0.39 pending 面——AlarmEvent 走
  RuleEvent 同款接收持久化路径？否：AlarmEvent 直接 Upsert alarmstore（etcd 写穿已持久），
  在途窗口 = 接收回调内完成写穿，重启恢复由 Load 保障）。
- 设定值：断网期间边缘按缓存执行（Twin.Desired 与 setpoint_cache 保持最近值；执行器语义
  为"声明期望态"，实际收敛由设备上报闭环——既有边界）；恢复后 flush 重投 pending-send/
  sent 未回告单（同 ID 幂等）→ 边缘重复应用幂等 + 回告 → 云端 applied 闭环。
- 验收（e2e）：断网窗口内告警恢复后全量到达云端告警中心；断网前已 sent 的设定值在恢复后
  重投闭环（applied 可查）；边缘断网期间缓存值不被清除。

### US-7 契约扩容与 e2e
- 契约：53→61 端点（告警 5 + 设定值 3）、活跃 13→15 消息（+AlarmEvent/+SetpointResult，
  仅增不改）；API-SPEC/API-COMPATIBILITY 同步；routes.go 唯一事实源 + 静态扫描清单扩
  alarm_api.go/setpoint_api.go。
- e2e（真实双进程）：规则下发 → 三档 severity 告警样例全链（边缘聚合 → 队列 → 云端中心
  可见 → ack/assign/close API 闭环 + 工单回调日志）；断网：停云 → 告警积压（台账+队列）→
  重启云 → 全量补传入中心；设定值：建单 → 边缘执行 → SetpointResult → applied 可查。

## 冻结兼容
- 普通设备指令（无 class/setpointId）路径逐字节不变（payload JSON 仅增可选字段，旧边缘忽略）。
- 契约只增：53→61 端点、活跃 13→15 消息（总口径 15→17，含 2 占位）；既有端点/消息逐字节不变。
- v0240–v0350 冻结测试零改动；MQTT/OPC-UA/视频/模型面代码零触碰；go.mod 零变化。
- 设定值审批默认 off：不开关时建单直接 pending-send（对既有 device-command 语义零影响——
  既有端点路径零触碰）。

## 测试锚（as-built 回填，2026-10-01）

- pkg/alarm +4：TestAlarmValidate / TestAlarmCanTransition / TestAlarmStateRank / TestAlarmDedupKeyJSON。
- edge/pkg/metamanager +3：TestAlarmLedgerUpsertAndActive / TestAlarmLedgerPersistAcrossReopen /
  TestSetpointCacheUpsertAndGet。
- cmd/edgecore +6：TestAlarmManagerFirstRaiseAndAggregate / TestAlarmManagerPriorityMapping /
  TestAlarmManagerEpisodeExpire / TestAlarmManagerStop / TestHandleSetpointAccepted / TestNewAlarmIDUnique。
- cloud/pkg/cloudhub +3：TestAlarmEventValidationMatrix / TestSetpointResultPayloadFields /
  TestServerHandlerRegistration。
- cloud/pkg/alarmstore +4：TestAlarmStoreUpsertMerge / TestAlarmStoreTransitions /
  TestAlarmStoreListAndStats / TestAlarmStoreLoadAndWriteThrough。
- cloud/pkg/setpointstore +5：TestSetpointCreateTwoPaths / TestSetpointApprovalFlow /
  TestSetpointRejectTerminal / TestSetpointResultLoop / TestSetpointListAndFailPath。
- cmd/cloudcore +6：TestAlarmAPIListAndStats / TestAlarmAPITransitionEndpoints /
  TestSetpointAPICreateAndList / TestSetpointAPIApprovalEndpoints / TestSetpointFlushOnce /
  TestParseSetpointFlushSec。
- tests/e2e +1：TestV0400AlarmSetpointE2E（真实双进程：三类 severity 告警全链 → 生命周期闭环 →
  停云 30s 告警积压 → 重启云补传合并 → 设定值建单/审批/applied 闭环 + rejected 留痕）。
- 合计：单测 31 例 + e2e 1 例（grep 口径，与 RELEASE-NOTES-v0400 N3 一致）。

## 边界登记（随文档落 KI §41）
- 边侧不自动产生 cleared（规则链无 cleared 事件；后续版本扩展恢复源）；
- 本地联动仅 logLinkage（声光/消息为接口留位）；工单集成点仅 logTicketSink（ITSM 对接留位）；
- 控制类数据缓存/校验/恢复须经场站工艺与安全部门评审后启用；设定值通道不替代现场安全联锁；
- 告警重发节流窗口内 count 聚合有延迟（reannounceSec/10 次阈值）；云端状态只进不退；
- 断网告警闭环需 UPLINK on（off 时尽力而为直发——与规则事件同口径）；
- 设定值 flush 重投周期（默认 30s）内恢复同步有延迟；同 ID 重发边缘重复应用幂等（回告可能
  重复，云端 ApplyResult 状态机幂等消化）；
- 审批默认 off（仅 requireApproval 单笔强制）；审批与执行窗口无锁定（审批后设备状态可能
  已变化——边界登记，后续版本可加快照校验）。
