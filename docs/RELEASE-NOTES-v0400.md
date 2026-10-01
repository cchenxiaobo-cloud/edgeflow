# EdgeFlow v0.40.0 发布说明（告警事件全链 + 设定值通道）

- 发布日期：2026-10-01
- 基线：v0.39.0（a9165b5）
- 主题：告警事件与消息联动（发展规划 G26）——pkg/alarm 告警模型（等级/生命周期/去重聚合）、边缘告警管理器（规则触发源挂点、本地台账、本地联动留位、补传队列接入）、云侧统一告警中心（etcd 写穿、收/派/闭环、工单集成点留位、5 端点）、设定值通道强化（建单/审批选项/可靠投递/执行反馈闭环/留痕，3 端点）、断网语义（告警缓存补传不丢；设定值按缓存执行、恢复重投闭环）。零新依赖；默认零行为与 opt-in（审批默认 off、无规则时告警链零行为）；契约仅增（53→61 端点、活跃 13→15 消息）。

## N1 能力（新增）

### 1. 告警模型（pkg/alarm，共享包）
- `Alarm`：AlarmID/NodeID/Source（rule|device|system）/Namespace/DeviceName/RuleID/Severity（critical|warning|info）/State/Message/Count/RaisedAt/UpdatedAt + 操作人字段；JSON 形态即消息负载、边缘台账行、云端存储行（三处同构）。
- 生命周期：raised → acked / assigned → closed（closed 终态）；`CanTransition` + `StateRank` 守卫只进不退；`DedupKey` = node|source|ruleID|namespace|device。
- 边侧不自动产生 cleared（v0.37 规则链无 cleared 事件——边界登记 KI §41）。

### 2. 边缘告警管理器（cmd/edgecore + edge/pkg/metamanager）
- 挂点：规则事件 sink 链装饰器（先告警记账/联动，再走原事件出口——规则事件路径零改动）。
- 去重聚合：同 episode（dedupKey 命中）count++ / 消息刷新；重发节流 = 每 reannounceSec（默认 60s，env 可调）或每 10 次命中，重发保持同 alarmID（云端合并）；episode 过期（30min 无新触发）后新触发为新告警。
- 本地台账：metamanager `alarm_ledger` 表（SQLite，同 ID 覆盖写穿）——断网留痕 + 重启后聚合身份延续。
- 本地联动：`Linkage` 接口（声光/消息联动抽象），默认 logLinkage 日志留痕（留位，声光/消息联动后续版本接入）。
- 上行分发：UPLINK on → v0.39 补传队列（severity→priority：critical=2/warning=1/info=0）；off → 直发尽力而为（与规则事件同口径）；入队失败直发兜底。

### 3. 云侧统一告警中心（cloud/pkg/alarmstore + cloudhub + cloudcore）
- alarmstore：etcd 写穿（/edgeflow/alarms/<alarmID>）+ 内存索引 + Load 恢复；Upsert 合并（count/updatedAt 取大、状态只进不退、closed 后迟到重发忽略）；Ack/Assign/Close 状态机（operator 必填）；List 过滤 + Stats 统计。
- 工单集成点：`TicketSink` 接口（assign 成功回调），默认 logTicketSink 日志留痕（ITSM 对接留位）。
- cloudhub：TypeAlarmEvent 接收（未注册/校验失败 Ack 语义化；nodeID 以连接注册身份为准，防伪造）。
- API（5 端点，auth/audit 链自动覆盖）：GET /api/v1/alarms、GET /api/v1/alarms/stats、POST /api/v1/alarms/{alarmID}/ack、/assign、/close（错误映射 404/409）。

### 4. 设定值通道强化（cloud/pkg/setpointstore + cmd/cloudcore + cmd/edgecore）
- DeviceCommand 负载仅增可选字段：class（"setpoint"=设定值）、setpointId（建单关联键）；普通指令路径逐字节不变。
- 建单：POST /api/v1/nodes/{nodeID}/setpoints（operator 必填）——requireApproval 或审批开关（EDGEFLOW_CLOUDCORE_SETPOINT_APPROVAL=on，默认 off）→ pending-approval，否则 pending-send。
- 审批：POST /api/v1/setpoints/{setpointID}/approval（approve → pending-send / reject → rejected 终态；仅 pending-approval 可审，否则 409）。
- 投递：flush 循环（EDGEFLOW_CLOUDCORE_SETPOINT_FLUSH_SEC 默认 30）扫描 pending-send → ReliableSend（QoS1，同 MsgID 幂等）→ sent；节点离线留单重投（断网恢复自动同步）；AckFailed（边缘拒绝）→ failed 终态。
- 执行反馈闭环：边缘执行（executor + Twin.Desired，语义同既有指令）→ 写 setpoint_cache（断网缓存）→ TypeSetpointResult 回告 → 云端 applied/failed 终态；未知建单回告计数忽略（补传乱序窗口）。
- 列表：GET /api/v1/setpoints（含 outcome/error，反馈可查）。

### 5. 断网语义（验收核心）
- 告警：断网期间告警入台账 + 补传队列（双留痕），恢复后队列重放 → 云端合并（UpdatedAt 落断网窗口为证）；云端重启经 Load 恢复不丢。
- 设定值：断网/链路断开期间边缘按缓存值继续执行（Twin.Desired 驻留内存 + setpoint_cache 持久留痕；
  边缘进程重启后期望态不回填——KI §41）；恢复后 pending 重投（同 MsgID 幂等）与 sent 未回告重投
  （新投递 ID，绕开边缘 MsgID 去重短路）→ 回告闭环。

## N2 兼容与冻结
- **默认零行为**：无规则时告警链零行为；审批默认 off（不开时建单直接 pending-send）；无建单时 flush 空转；普通设备指令路径与 v0.39.0 逐字节一致（payload JSON 仅增）。
- 契约仅增：53→61 端点（告警 5 + 设定值 3）、活跃 13→15 消息（+AlarmEvent/+SetpointResult）；既有端点/消息逐字节不变；API-SPEC / API-COMPATIBILITY 已同步。
- v0240–v0350 冻结测试零改动；MQTT/OPC-UA/视频/模型面代码零触碰；pkg/rules 零改动；go.mod 零变化。

## N3 测试（grep 口径）
- pkg/alarm +4 例（校验矩阵/状态机迁移/状态秩/去重键与 JSON 往返）。
- edge/pkg/metamanager +3 例（台账写穿与活跃索引/重开持久/设定值缓存 UPSERT）。
- cmd/edgecore +6 例（首触建单与聚合节流/优先级映射/episode 过期/停止空转/设定值回告三路径/ID 唯一）。
- cloud/pkg/cloudhub +3 例（告警校验矩阵/回告负载契约/handler 注册与锁外回调）。
- cloud/pkg/alarmstore +4 例（合并语义/状态机与 409/过滤统计/写穿与 Load 恢复）。
- cloud/pkg/setpointstore +5 例（建单两路径/审批流/拒绝终态/反馈闭环与乱序/列表与失败路径）。
- cmd/cloudcore +6 例（告警 API 全链/统计/设定值 API 建单审批/flush 投递三脚本/env 解析）。
- e2e 1 例（真实双进程：三类 severity 告警全链 → ack/assign/close 生命周期 → 停云 30s 告警积压 → 重启云补传合并 → 设定值建单/审批/applied 闭环 + rejected 留痕）。
- 契约：61 端点契约守卫 + 静态扫描清单扩 alarm_api.go/setpoint_api.go；运行时/文档一致性守卫同步。

## N4 边界（登记 KNOWN-ISSUES §41）
- 边侧不自动产生 cleared（规则链无 cleared 事件，后续版本扩展恢复源）；
- 本地联动仅 logLinkage、工单集成点仅 logTicketSink（均为接口留位）；
- 控制类数据缓存/校验/恢复须经场站工艺与安全部门评审后启用；设定值通道不替代现场安全联锁；
- 告警重发节流（60s/10 次命中）内 count 聚合有延迟；云端状态只进不退（closed 后重发忽略）；
- 断网告警闭环需 UPLINK on（off 时尽力而为直发）；episode 30min 过期后新触发为新告警（云端闭环不回传边侧）；
- 设定值 flush 周期（默认 30s）内恢复同步有延迟；sent 未回告单按新投递 ID（MsgID-r<次数>）
  周期重投（默认 90s，env 可调；0 禁用）——同 ID 重投会被边缘 MsgID 去重短路；
  重复回告由云端状态机 409 消化；
- 边侧重启后告警 episode 不自动延续（重启即新告警）；断网/链路断开期间设定值按缓存值
  （Twin.Desired 驻留内存）继续执行，边缘进程重启后期望态不回填（回填为后续候选）；
- 审批与执行窗口无锁定（审批后设备状态可能已变化——快照校验留位后续版本）。

## N5 门禁（2026-10-01 全绿）
- vet（全仓）/ 全仓回归（除 e2e/契约，41 包全绿）/ race ×7 包（pkg/alarm 2.8s + metamanager 7.1s
  + edgecore 5.3s + alarmstore 3.6s + setpointstore 5.0s + cloudhub 8.8s + cloudcore 21.3s）/
  契约 12.1s / e2e 全量 556.0s：**全绿（0 FAIL）**。
- e2e 单跑 PASS 91.6s（三类 severity 告警全链 + 生命周期闭环 + 断网 30s 补传合并 +
  设定值 applied/rejected 双路径）；原始日志归档工作台（gates.log / e2e-debug*.log）。
- 复核结论见工作台 review.md（静态审查 + 定向验证）。
