// 上行补传与状态上报（v0.39.0，spec 0012 US-2/US-3/US-6）。
//
// 组合：持久队列（metamanager.UplinkQueue）+ 分级策略（severity→优先级）
// + 补传 worker（断网积压、恢复续传、批量/限速）+ 周期状态上报。
// 开关 EDGEFLOW_EDGECORE_UPLINK=on 时由 main.go 装配；关闭时规则事件
// 出口保持 v0.38 直发路径（逐字节等价）。
package main

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"edgeflow/edge/pkg/edgehub"
	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/rules"
)

// 上行补传环境变量（spec 0012 US-6）。
const (
	envUplinkEnabled   = "EDGEFLOW_EDGECORE_UPLINK"
	envUplinkMaxRows   = "EDGEFLOW_EDGECORE_UPLINK_MAX_ROWS"
	envUplinkBatch     = "EDGEFLOW_EDGECORE_UPLINK_BATCH"
	envUplinkRate      = "EDGEFLOW_EDGECORE_UPLINK_RATE"
	envUplinkReportSec = "EDGEFLOW_EDGECORE_UPLINK_REPORT_SEC"

	defaultUplinkBatch     = 32
	defaultUplinkRate      = 100
	defaultUplinkReportSec = 30

	// uplinkWorkerTickSec 是 worker 兜底轮询周期（秒）——除入队唤醒外，
	// 保证断网恢复后即使没有新事件也能自动续传。
	uplinkWorkerTickSec = 2
)

// uplinkOptions 是补传装配参数（解析自环境变量）。
type uplinkOptions struct {
	Enabled   bool
	MaxRows   int
	Batch     int
	Rate      int // 条/秒（弱网调小）
	ReportSec int // 状态上报周期（秒）
}

// parseUplinkOptionsFromEnv 解析开关与参数（非法值告警并回退默认）。
func parseUplinkOptionsFromEnv() uplinkOptions {
	o := uplinkOptions{
		Enabled:   os.Getenv(envUplinkEnabled) == "on",
		MaxRows:   metamanager.DefaultUplinkMaxRows,
		Batch:     defaultUplinkBatch,
		Rate:      defaultUplinkRate,
		ReportSec: defaultUplinkReportSec,
	}
	o.MaxRows = envUplinkInt(envUplinkMaxRows, o.MaxRows, 1)
	o.Batch = envUplinkInt(envUplinkBatch, o.Batch, 1)
	o.Rate = envUplinkInt(envUplinkRate, o.Rate, 0) // 0 = 不限速
	o.ReportSec = envUplinkInt(envUplinkReportSec, o.ReportSec, 1)
	return o
}

// envUplinkInt 读取整数环境变量；缺省/非法/小于下限时回退默认（告警）。
func envUplinkInt(name string, def, min int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		log.Warnf("%s 非法（%q），回退默认 %d", name, v, def)
		return def
	}
	return n
}

// uplinkPriorityForSeverity 把事件严重级映射为队列优先级（spec 0012 US-2）。
func uplinkPriorityForSeverity(severity string) int {
	switch severity {
	case "critical":
		return metamanager.UplinkPriorityHigh
	case "warning":
		return metamanager.UplinkPriorityNormal
	default:
		return metamanager.UplinkPriorityLow
	}
}

// recordRuleEventLedger 把触发事件落规则事件台账（失败 Warn 不阻断）。
// v0.37.0 直发出口与新补传出口共用。
func recordRuleEventLedger(ledger *metamanager.RuleLedger, ev rules.Event) {
	if ledger == nil {
		return
	}
	rec := metamanager.RuleEventRecord{
		Ts:        ev.TriggeredAt,
		RuleID:    ev.RuleID,
		DeviceID:  ev.DeviceName,
		Namespace: ev.Namespace,
		Severity:  ev.Severity,
		Value:     fmt.Sprintf("%g", ev.Value),
		Message:   ev.Message,
	}
	if err := ledger.SaveEvent(rec); err != nil {
		log.Warnf("规则事件台账写入失败: %v", err)
	}
}

// uplinkRelay 组合队列、发送与状态上报（装配层唯一持有者）。
type uplinkRelay struct {
	q         *metamanager.UplinkQueue
	send      func(*protocol.Message) error
	nodeID    string
	batch     int
	rate      int
	reportSec int

	// offline 是补传离线态（首次失败置位、一轮全成功清除），
	// 用于离线/恢复日志限频（避免逐轮刷屏）。
	offline atomic.Bool

	wake   chan struct{} // 入队唤醒（容量 1，非阻塞信号）
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// newUplinkRelay 构造补传中继（send 通常为 client.Send 方法值；测试可注入 fake）。
func newUplinkRelay(q *metamanager.UplinkQueue, send func(*protocol.Message) error, o uplinkOptions, nodeID string) *uplinkRelay {
	return &uplinkRelay{
		q:         q,
		send:      send,
		nodeID:    nodeID,
		batch:     o.Batch,
		rate:      o.Rate,
		reportSec: o.ReportSec,
		wake:      make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
	}
}

// Notify 唤醒补传 worker（非阻塞：已有待处理信号时直接返回）。
func (r *uplinkRelay) Notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Start 启动补传 worker 与状态上报循环。
func (r *uplinkRelay) Start() {
	r.wg.Add(2)
	go r.workerLoop()
	go r.reportLoop()
}

// Stop 停止 worker 与上报（残留条目留盘：下次启动继续补传）。
func (r *uplinkRelay) Stop() {
	close(r.stopCh)
	r.wg.Wait()
}

// workerLoop 补传循环：唤醒信号或兜底轮询触发一轮 drain。
func (r *uplinkRelay) workerLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Duration(uplinkWorkerTickSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-r.wake:
		case <-ticker.C:
		}
		r.drain()
	}
}

// drain 执行一轮补传：批量出队 → 逐条发送（按速率预算节流）→ 成功 Ack。
// 发送失败（未连接/写错）立即停止本轮——条目不删除，留在队列下轮重试
// （至少一次语义；重复由云端幂等消化）。
func (r *uplinkRelay) drain() {
	items, err := r.q.DequeueUplinkBatch(r.batch)
	if err != nil {
		log.Warnf("上行补传出队失败: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}
	start := time.Now()
	for i, it := range items {
		select {
		case <-r.stopCh:
			return
		default:
		}
		if r.rate > 0 && i > 0 {
			// 速率预算：第 i 条不早于 start + i/rate 发送。
			want := start.Add(time.Duration(i) * time.Second / time.Duration(r.rate))
			if d := time.Until(want); d > 0 {
				time.Sleep(d)
			}
		}
		if err := r.send(it.Msg); err != nil {
			// 离线/写失败：整批留待下轮；首次进入离线态时告警（限频）。
			if r.offline.CompareAndSwap(false, true) {
				log.Warnf("上行补传暂不可达（本批 %d 条留队列，恢复后自动续传）: %v", len(items), err)
			}
			return
		}
		if err := r.q.AckUplink(it.ID); err != nil {
			// Ack 失败会导致重复上送（下次出队再发）：云端幂等消化。
			log.Warnf("上行补传确认失败（可能重复上送，云端幂等消化）: %v", err)
		}
	}
	// 本轮全部成功：若此前处于离线态，输出恢复日志（限频）。
	if r.offline.CompareAndSwap(true, false) {
		log.Infof("上行补传已恢复（连接正常，继续续传）")
	}
}

// reportLoop 周期上报队列状态（UplinkReport）；离线发送失败静默跳过
// （上报本身不进队列——避免自引用与放大）。
func (r *uplinkRelay) reportLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Duration(r.reportSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
		}
		st, err := r.q.UplinkDepth()
		if err != nil {
			log.Warnf("读取上行队列统计失败: %v", err)
			continue
		}
		msg, err := buildUplinkReportMessage(r.nodeID, st)
		if err != nil {
			log.Warnf("构造上行状态上报失败: %v", err)
			continue
		}
		_ = r.send(msg) // 离线静默跳过（下轮再报）
	}
}

// uplinkReportPayload 是 UplinkReport 消息负载（spec 0012 US-6）。
type uplinkReportPayload struct {
	Depth    int   `json:"depth"`    // 当前积压总数
	Dropped  int64 `json:"dropped"`  // 累计容量丢弃
	Sent     int64 `json:"sent"`     // 累计成功上送
	OldestTs int64 `json:"oldestTs"` // 最老积压条目入队时间（毫秒；空为 0）
}

// buildUplinkReportMessage 构造上行状态上报消息（纯函数，便于单测）。
func buildUplinkReportMessage(nodeID string, st metamanager.UplinkStats) (*protocol.Message, error) {
	return protocol.NewMessage(protocol.TypeUplinkReport, nodeID, targetCloud, uplinkReportPayload{
		Depth:    st.Total,
		Dropped:  st.Dropped,
		Sent:     st.Sent,
		OldestTs: st.OldestTs,
	})
}

// newUplinkRuleEventSink 构造"补传模式"规则事件出口（spec 0012 US-2/US-6）：
// ① 落规则事件台账（失败 Warn 不阻断）；② 按 severity 分级入持久补传队列
// （Enqueue 失败 → Warn + 直发兜底，不丢事件）；③ 唤醒补传 worker 立即发送。
func newUplinkRuleEventSink(relay *uplinkRelay, ledger *metamanager.RuleLedger, client *edgehub.Client, nodeID string) func(rules.Event) {
	return func(ev rules.Event) {
		recordRuleEventLedger(ledger, ev)
		msg, err := buildRuleEventMessage(nodeID, ev)
		if err != nil {
			log.Warnf("构造 RuleEvent 消息失败: %v", err)
			return
		}
		if _, err := relay.q.EnqueueUplink(uplinkPriorityForSeverity(ev.Severity), msg); err != nil {
			log.Warnf("规则事件入队失败（直发兜底）: %v", err)
			if client != nil {
				if err := client.Send(msg); err != nil {
					log.Warnf("规则事件上报失败: %v", err)
				}
			}
			return
		}
		relay.Notify()
	}
}
