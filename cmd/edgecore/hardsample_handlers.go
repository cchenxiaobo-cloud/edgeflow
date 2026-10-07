package main

// v0.45.0 困难样本收集（spec 0018 US-3，G25）：
//
// 设计（全部复用既有通道，零新队列）：
//   - 触发：alarmManager 新 episode 联动位（Linkage 接口实现，OnAlarm 不阻塞）；
//   - 帧源：视频 mapper 的 mediaSinkHolder 最新 JPEG 快照（弱关联：按设备名取，
//     无帧源/无帧时跳过登记，不阻塞告警链）；
//   - 上行：构造 mediaup.Clip（仅 Snapshot）交给既有 mediaUp Uploader——
//     spool 落盘 → 分片入补传队列（v0.39 断网留存/恢复重放/至少一次）；
//     Kind 用 hard-sample（云端 mediastore 独立前缀 objects/hardsample/）。
//   - 低带宽抽样：每告警 episode 最多 maxPerAlarm 张（默认 1）；
//     全局计数器在内存（重启清零，最多多传一张，无害——spec 0018 边界）。
//
// opt-in：EDGEFLOW_EDGECORE_HARDSAMPLE=on（默认 off 零行为——采集器不装配）。

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"edgeflow/pkg/alarm"
	"edgeflow/pkg/log"
	"edgeflow/pkg/mediaup"
)

// EnvHardSampleEnabled 困难样本总开关（默认 off）。
const EnvHardSampleEnabled = "EDGEFLOW_EDGECORE_HARDSAMPLE"

// EnvHardSampleMaxPerAlarm 每告警最多捕获张数（默认 1，合法域 1–5）。
const EnvHardSampleMaxPerAlarm = "EDGEFLOW_EDGECORE_HARDSAMPLE_MAX_PER_ALARM"

// EnvEdgeCoreAccels 加速卡能力清单（v0.45.0 US-5，G24）：逗号分隔
// <类型>:<标识>（如 "gpu:cuda-12.4,npu:rockchip-9996"）。
const EnvEdgeCoreAccels = "EDGEFLOW_EDGECORE_ACCELS"

// hardSampleCollector 是困难样本采集器（实现 alarm.Linkage；在告警新 episode
// 时捕获一帧快照入上行通道）。
type hardSampleCollector struct {
	nodeID       string
	holder       *mediaSinkHolder // 视频帧源（可能无 sink/无帧）
	uploader     *mediaup.Uploader
	maxPerAlarm  int
	mu           sync.Mutex
	seen         map[string]int // alarmID → 已捕获数（内存；重启清零无害）
	globalCount  atomic.Int64   // 全局累计捕获数（诊断）
	droppedCount atomic.Int64   // 无帧源/入队失败跳过数（诊断）
}

// newHardSampleCollector 解析 env 构造采集器；开关关闭返回 nil（零行为）。
// maxPerAlarm 非法（<1 或 >5）回退默认 1（Warn）。
func newHardSampleCollector(nodeID string, holder *mediaSinkHolder, up *mediaup.Uploader) *hardSampleCollector {
	if os.Getenv(EnvHardSampleEnabled) != "on" {
		return nil
	}
	max := 1
	if v := strings.TrimSpace(os.Getenv(EnvHardSampleMaxPerAlarm)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 5 {
			max = n
		} else {
			log.Warnf("[hardsample] %s 非法（%q），回退默认 1", EnvHardSampleMaxPerAlarm, v)
		}
	}
	return &hardSampleCollector{
		nodeID:      nodeID,
		holder:      holder,
		uploader:    up,
		maxPerAlarm: max,
	}
}

// OnAlarm 实现 alarm.Linkage：捕获困难样本。永不 panic、永不阻塞——
// 任何失败仅计数与日志（告警主链零影响）。
func (c *hardSampleCollector) OnAlarm(a alarm.Alarm) {
	if c == nil {
		return
	}
	// 低带宽抽样：每告警 episode 最多 maxPerAlarm 张。episode 身份 = AlarmID
	// （新 episode 才进 OnAlarm，聚合命中不触发），因此这里只需限制
	// "同一 AlarmID 的重复联动"——用每 AlarmID 计数（内存 map + 上限清理）。
	if !c.admit(a.AlarmID) {
		return
	}
	// 帧源：holder 当前 sink 的最新快照帧。无视频配置（sink nil）或环形空
	// → 跳过登记（spec 0018：无帧源时登记跳过，不阻塞告警链）。
	jpeg, whenMs, ok := c.holder.LatestSnapshot()
	if !ok {
		c.droppedCount.Add(1)
		log.Infof("[hardsample] 告警 %s 无可用快照帧，跳过", a.AlarmID)
		return
	}
	if c.uploader == nil {
		// 装配缺 uploader（理论不可达；上传面未启用时采集器也不应装配）——
		// 与无帧同语义：跳过计数，不阻塞告警链。
		c.droppedCount.Add(1)
		return
	}
	clip := mediaup.Clip{
		DeviceName: a.DeviceName,
		Snapshot:   jpeg,
		FrameCount: 1,
		WhenMs:     whenMs,
		AlarmID:    a.AlarmID,
	}
	// Uploader.HandleClip 的 processClip 走 spoolAndEnqueue——需要 kind 分叉：
	// 既有路径写 snapshot+segment 两种；困难样本只写 hard-sample 单段。
	// 为不改动既有 processClip 语义（冻结面），这里直接调用底层
	// spoolAndEnqueue 等价路径（经专用的 EnqueueHardSample 包装）。
	if err := c.uploader.EnqueueHardSample(clip); err != nil {
		c.droppedCount.Add(1)
		log.Warnf("[hardsample] 入队失败（alarm=%s）: %v", a.AlarmID, err)
		return
	}
	c.globalCount.Add(1)
	log.Infof("[hardsample] 已捕获困难样本（alarm=%s device=%s bytes=%d）",
		a.AlarmID, a.DeviceName, len(jpeg))
}

// admit 报告该 AlarmID 是否仍可捕获（每告警前 maxPerAlarm 次 true）。
// map 上限 1024 条（LRU 不做——超出即整体重置：高频告警的上限计数随之清零，
// 极低概率下同 ID 可能多传几张；与 spec 0018「重启清零最多多传一张」同边界，
// 无害。复核 P2-1 备忘留档）。
func (c *hardSampleCollector) admit(alarmID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]int, 8)
	}
	if len(c.seen) > 1024 {
		c.seen = make(map[string]int, 8)
	}
	if c.seen[alarmID] >= c.maxPerAlarm {
		return false
	}
	c.seen[alarmID]++
	return true
}

// Stats 返回诊断计数（captured, dropped）。
func (c *hardSampleCollector) Stats() (captured, dropped int64) {
	return c.globalCount.Load(), c.droppedCount.Load()
}

// envAccelList 解析 EDGEFLOW_EDGECORE_ACCELS（逗号分隔 <类型>:<标识>）为清单；
// 空串/未设置 → nil（不上报）。条目去空白；非法条目（无 ':'）剔除并 Warn。
// env 优先（模拟验证通道）；真实探测后续经可插拔探测器接入（spec 0018 US-5）。
func envAccelList() []string {
	v := strings.TrimSpace(os.Getenv(EnvEdgeCoreAccels))
	if v == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, ":") {
			log.Warnf("[accel] 忽略非法条目（缺 ':'）: %q", item)
			continue
		}
		out = append(out, item)
	}
	return out
}
