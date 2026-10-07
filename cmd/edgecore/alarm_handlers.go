// 告警管理与设定值回告（v0.40.0，spec 0013 US-2/US-5）。
//
// 组合：pkg/alarm 告警模型 + 去重聚合（同 episode count 递增、节流重发）
// + 本地台账（alarm_ledger 写穿，断网留痕）+ 本地联动接口（Linkage，默认
// logLinkage 留位）+ 上行分发（UPLINK on → v0.39 补传队列；off → 直发尽力而为）。
//
// 告警产生源（本版）：规则触发（ObserveRuleEvent 挂在规则事件 sink 链上，
// 先告警记账再走原事件出口，规则事件路径零改动）；device/system 源为
// 后续版本留位（Raise 公开入口）。
//
// 设定值回告：handleDeviceCommand 执行 class=setpoint 指令后，写
// setpoint_cache（断网缓存语义）并回 TypeSetpointResult（执行反馈闭环）。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/alarm"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/rules"
)

// 告警环境变量（spec 0013 US-2）。
const (
	envAlarmReannounceSec = "EDGEFLOW_EDGECORE_ALARM_REANNOUNCE_SEC"

	defaultAlarmReannounceSec = 60

	// alarmAggThreshold 是聚合计数的强制重发阈值：同 episode 每累计
	// 10 次命中（无论是否到达重发周期）重发一次，保证云端 count 有界滞后。
	alarmAggThreshold = 10

	// alarmEpisodeExpireSec 是 episode 过期秒数：超过该窗口无新触发即结束
	// 聚合（下次触发新建 episode/新 alarmID）。云端闭环（close）不回传边侧
	// ——以时间窗收敛聚合身份（边界登记 KI §41）。
	alarmEpisodeExpireSec = 1800
)

// setpointClass 是设定值指令的类别标记（DeviceCommandPayload.Class）。
const setpointClass = "setpoint"

// handleSetpointAccepted 设定值受理后的边缘动作（spec 0013 US-5）：
// 仅当指令受理成功（Desired 已更新、无执行失败）且为 class=setpoint 建单指令时，
// 写 setpoint_cache（断网缓存语义）并回 TypeSetpointResult（执行反馈闭环）。
// 受理失败（解析/校验/执行失败）不回告——由 Ack error 路径（云端 ErrAckFailed）
// 记录失败并终态化建单。ctx 允许 nil（降级跳过）。
func handleSetpointAccepted(ctx *setpointContext, msg *protocol.Message, execErr error) {
	if ctx == nil || execErr != nil || msg == nil {
		return
	}
	var cmd devicetwin.DeviceCommandPayload
	if err := msg.DecodePayload(&cmd); err != nil {
		return // 已在 handleDeviceCommand 校验过；防御性静默
	}
	if cmd.Class != setpointClass || cmd.SetpointID == "" {
		return // 普通指令：零变化
	}
	recordSetpointCache(ctx, cmd)
	sendSetpointResult(ctx, cmd, nil)
}

// uplinkDispatch 是上行分发函数：UPLINK on 时入补传队列（priority 按 severity
// 映射），off 时直发尽力而为。由 main.go 装配注入；失败内部告警，不向上传播
// （告警/回告均为尽力而为面，不阻断采集与规则链）。
type uplinkDispatch func(priority int, msg *protocol.Message)

// Linkage 是本地联动接口（声光/消息联动抽象，spec 0013 US-2 留位）。
// OnAlarm 在告警产生（新 episode）时同步调用；实现方不得阻塞、不得反向
// 调用管理器。本版唯一实现 logLinkage（日志留痕）；声光/消息联动后续版本接入。
type Linkage interface {
	OnAlarm(a alarm.Alarm)
}

// logLinkage 是本地联动的默认实现（日志留痕）。
type logLinkage struct{}

// OnAlarm 输出联动留痕日志。
func (logLinkage) OnAlarm(a alarm.Alarm) {
	log.Infof("[本地联动] 告警 %s（%s/%s severity=%s）count=%d：%s",
		a.AlarmID, a.Namespace, a.DeviceName, a.Severity, a.Count, a.Message)
}

// alarmEpisode 是一个聚合窗口内的告警 episode（同 dedupKey 聚合身份）。
type alarmEpisode struct {
	a          alarm.Alarm
	lastSentAt int64 // 最近一次重发时间（毫秒；0=尚未上行过）
	hits       int64 // 自上次重发以来的命中数
}

// alarmManager 是边缘告警管理器（装配层唯一持有者）。
type alarmManager struct {
	nodeID        string
	ledger        *metamanager.AlarmLedger // 允许 nil（台账初始化失败降级，仅内存聚合）
	linkage       Linkage                  // 允许 nil（无联动）
	dispatch      uplinkDispatch           // 必填（直发或入队由装配决定）
	reannounceSec int

	mu      sync.Mutex
	active  map[string]*alarmEpisode // dedupKey → episode
	stopped bool
}

// newAlarmManager 构造告警管理器；台账创建失败降级为 nil ledger（Warn 不阻断）。
// reannounceSec 非法（<1）回退默认 60（Warn）。
func newAlarmManager(nodeID string, store *metamanager.Store, dispatch uplinkDispatch, reannounceSec int) *alarmManager {
	if reannounceSec < 1 {
		log.Warnf("%s 非法（%d），回退默认 %d", envAlarmReannounceSec, reannounceSec, defaultAlarmReannounceSec)
		reannounceSec = defaultAlarmReannounceSec
	}
	var ledger *metamanager.AlarmLedger
	if store != nil {
		if l, err := metamanager.NewAlarmLedger(store); err != nil {
			log.Warnf("告警台账初始化失败（降级为仅内存聚合）: %v", err)
		} else {
			ledger = l
		}
	}
	if dispatch == nil {
		// 理论不可达（装配必注入）；防御：空实现。
		dispatch = func(int, *protocol.Message) {}
	}
	m := &alarmManager{
		nodeID:        nodeID,
		ledger:        ledger,
		linkage:       logLinkage{},
		dispatch:      dispatch,
		reannounceSec: reannounceSec,
		active:        make(map[string]*alarmEpisode),
	}
	return m
}

// AddLinkage 追加联动实现（v0.45.0：困难样本采集器经此挂入；新 episode 时
// 与默认日志联动同序触发，实现方各自保证不阻塞）。
func (m *alarmManager) AddLinkage(l Linkage) {
	if l == nil {
		return
	}
	// 包装为顺序联动：保持既有单一 linkage 字段语义（零结构变更）。
	prev := m.linkage
	m.linkage = multiLinkage{prev, l}
}

// multiLinkage 顺序调用两个联动实现（first 后 second；均不阻塞约定）。
type multiLinkage struct {
	first  Linkage
	second Linkage
}

func (ml multiLinkage) OnAlarm(a alarm.Alarm) {
	if ml.first != nil {
		ml.first.OnAlarm(a)
	}
	if ml.second != nil {
		ml.second.OnAlarm(a)
	}
}

// ObserveRuleEvent 规则触发入口（挂在规则事件 sink 链上，先告警后原出口）。
// 规则事件 → 告警转换：source=rule、severity 直映射、dedupKey 按
// node|rule|namespace|device 聚合；任何失败只 Warn，不阻断规则链。
func (m *alarmManager) ObserveRuleEvent(ev rules.Event) {
	if m == nil {
		return
	}
	now := time.Now().UnixMilli()
	a := alarm.Alarm{
		NodeID:     m.nodeID,
		Source:     alarm.SourceRule,
		Namespace:  ev.Namespace,
		DeviceName: ev.DeviceName,
		RuleID:     ev.RuleID,
		Severity:   ev.Severity,
		State:      alarm.StateRaised,
		Message:    ev.Message,
		RaisedAt:   now,
		UpdatedAt:  now,
	}
	m.Observe(a)
}

// Observe 通用告警入口（去重聚合 + 台账 + 联动 + 上行）。
// 同 episode（dedupKey 命中活跃表）→ count++ / 消息刷新 / 节流重发；
// 新 episode → 生成 AlarmID → 台账写穿 → 联动 → 上行。
func (m *alarmManager) Observe(a alarm.Alarm) {
	if a.Severity != alarm.SeverityCritical && a.Severity != alarm.SeverityWarning && a.Severity != alarm.SeverityInfo {
		a.Severity = alarm.SeverityInfo // 未知等级归一为 info（与规则映射口径一致）
	}
	key := a.DedupKey()
	now := time.Now().UnixMilli()

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	// episode 过期检查：超窗清理（下次触发新建 episode）。
	for k, ep := range m.active {
		if now-ep.a.UpdatedAt > alarmEpisodeExpireSec*1000 {
			delete(m.active, k)
		}
	}
	ep, ok := m.active[key]
	if ok {
		ep.a.Count++
		ep.a.UpdatedAt = now
		ep.a.Message = a.Message // 最新一次触发渲染
		ep.hits++
		a = ep.a
		shouldSend := now-ep.lastSentAt >= int64(m.reannounceSec)*1000 || ep.hits >= alarmAggThreshold
		if shouldSend {
			ep.lastSentAt = now
			ep.hits = 0
		}
		m.mu.Unlock()
		m.persist(a)
		if shouldSend {
			m.send(a)
		}
		return
	}
	// 新 episode：生成 AlarmID（alm-<毫秒>-<rand4>）。
	a.AlarmID = newAlarmID(now)
	a.Count = 1
	m.active[key] = &alarmEpisode{a: a, lastSentAt: now, hits: 0}
	m.mu.Unlock()

	m.persist(a)
	if m.linkage != nil {
		m.linkage.OnAlarm(a) // 联动仅在新 episode 触发（聚合命中不重复联动）
	}
	m.send(a)
}

// persist 台账写穿（失败 Warn 不阻断；nil ledger 跳过）。
func (m *alarmManager) persist(a alarm.Alarm) {
	if m.ledger == nil {
		return
	}
	if err := m.ledger.UpsertAlarm(a); err != nil {
		log.Warnf("告警台账写入失败（alarmId=%s）: %v", a.AlarmID, err)
	}
}

// send 上行分发（severity → priority 映射同规则事件；失败由 dispatch 内部告警）。
func (m *alarmManager) send(a alarm.Alarm) {
	msg, err := buildAlarmEventMessage(m.nodeID, a)
	if err != nil {
		log.Warnf("构造 AlarmEvent 消息失败: %v", err)
		return
	}
	m.dispatch(uplinkPriorityForSeverity(a.Severity), msg)
}

// Stop 标记停止（停止后 Observe 空转；活跃 episode 不落终态——告警闭环在云侧操作）。
func (m *alarmManager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
}

// newAlarmID 生成告警 ID（alm-<毫秒>-<rand4>，进程内唯一）。
func newAlarmID(nowMs int64) string {
	var b [4]byte // 4 字节后缀（v0.44 门禁发现 2 字节在 100 次/同毫秒生成下约 7.5% 生日碰撞——TestNewAlarmIDUnique 偶发 FAIL）
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("alm-%d", nowMs) // 极端降级：毫秒时间戳（同毫秒碰撞概率可忽略）
	}
	return fmt.Sprintf("alm-%d-%s", nowMs, hex.EncodeToString(b[:]))
}

// buildAlarmEventMessage 构造告警上行消息（payload 即 alarm.Alarm JSON，三处同构）。
func buildAlarmEventMessage(nodeID string, a alarm.Alarm) (*protocol.Message, error) {
	return protocol.NewMessage(protocol.TypeAlarmEvent, nodeID, targetCloud, a)
}

// setpointResultPayload 是设定值执行反馈消息的负载（边→云，spec 0013 US-5）。
type setpointResultPayload struct {
	SetpointID string  `json:"setpointId"`      // 云侧建单 ID（关联键）
	OK         bool    `json:"ok"`              // 执行结果（false 时 error 非空）
	Value      float64 `json:"value"`           // 本次应用的期望值
	Error      string  `json:"error,omitempty"` // 失败原因（执行失败时）
	Ts         int64   `json:"ts"`              // 执行时间（毫秒）
}

// buildSetpointResultMessage 构造设定值执行反馈消息。
func buildSetpointResultMessage(nodeID, setpointID string, ok bool, value float64, execErr error) (*protocol.Message, error) {
	payload := setpointResultPayload{
		SetpointID: setpointID,
		OK:         ok,
		Value:      value,
		Ts:         time.Now().UnixMilli(),
	}
	if execErr != nil {
		payload.Error = execErr.Error()
	}
	return protocol.NewMessage(protocol.TypeSetpointResult, nodeID, targetCloud, payload)
}

// setpointContext 是设定值处理的边缘侧依赖（main.go 装配；nil = 降级跳过回告）。
type setpointContext struct {
	nodeID   string
	cache    *metamanager.SetpointCache // 允许 nil（缓存初始化失败降级）
	dispatch uplinkDispatch             // 回告分发（UPLINK on 入队 / off 直发）
}

// recordSetpointCache 写设定值缓存（断网缓存语义；失败 Warn 不阻断）。
func recordSetpointCache(ctx *setpointContext, cmd devicetwin.DeviceCommandPayload) {
	if ctx == nil || ctx.cache == nil {
		return
	}
	rec := metamanager.SetpointCacheRecord{
		Namespace:  cmd.Namespace,
		Property:   cmd.Property,
		DeviceName: cmd.DeviceName,
		Value:      cmd.Value,
		SetpointID: cmd.SetpointID,
	}
	if err := ctx.cache.SaveSetpointCache(rec); err != nil {
		log.Warnf("设定值缓存写入失败（%s/%s.%s）: %v", cmd.Namespace, cmd.DeviceName, cmd.Property, err)
	}
}

// sendSetpointResult 回告执行结果（执行反馈闭环；离线时 UPLINK on 入队补传，
// off 静默尽力而为）。构造失败仅告警。
func sendSetpointResult(ctx *setpointContext, cmd devicetwin.DeviceCommandPayload, execErr error) {
	if ctx == nil || cmd.SetpointID == "" {
		return
	}
	msg, err := buildSetpointResultMessage(ctx.nodeID, cmd.SetpointID, execErr == nil, cmd.Value, execErr)
	if err != nil {
		log.Warnf("构造 SetpointResult 消息失败（setpointId=%s）: %v", cmd.SetpointID, err)
		return
	}
	ctx.dispatch(metamanager.UplinkPriorityNormal, msg)
}
