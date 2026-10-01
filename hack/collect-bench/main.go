// collect-bench：2000 点/1s 采集管道压测（v0.41.0，spec 0014 US-4 · G19）。
//
// 管道语义对齐 edgecore 采样管道（cmd/edgecore samplePipeline）：
//
//	治理过滤（governor.Filter，无策略=直通）→ 规则评估（evaluator.Observe，
//	含阈值规则集）→ 时序库写入（pkg/tsdb，v0.38 段模型，metric=device/ns/prop）。
//
// 输出：实际吞吐（点/s）、单轮处理延迟 P50/P99/max、tsdb 写入统计——
// 数字入 docs/PERFORMANCE-BASELINE.md（对照规划目标 2000 点/1s）。
//
// 用法：
//
//	go run ./hack/collect-bench -devices 200 -props 10 -interval 1s -duration 10s
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"edgeflow/pkg/rules"
	"edgeflow/pkg/tsdb"
)

func main() {
	devices := flag.Int("devices", 200, "模拟设备数")
	props := flag.Int("props", 10, "每设备属性数（总点数 = devices × props）")
	interval := flag.Duration("interval", time.Second, "采集周期")
	duration := flag.Duration("duration", 10*time.Second, "压测时长")
	tsdbDir := flag.String("tsdb-dir", "", "时序库目录（默认临时目录）")
	flag.Parse()

	totalPoints := *devices * *props
	fmt.Printf("=== EdgeFlow 采集管道压测 ===\n设备 %d × 属性 %d = %d 点/轮；周期 %s；时长 %s\n",
		*devices, *props, totalPoints, interval, duration)

	// 时序库（临时目录，随进程清理）。
	dir := *tsdbDir
	if dir == "" {
		dir = fmt.Sprintf("%s/collect-bench-%d", os.TempDir(), time.Now().UnixNano())
		defer func() { _ = os.RemoveAll(dir) }()
	}
	tdb, err := tsdb.Open(tsdb.Options{Dir: dir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "时序库打开失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = tdb.Close() }()

	eng := rules.NewEvaluator()
	// 规则集（每 10 台设备一条阈值规则，真实评估开销——P1-2 处置：
	// 规则必须应用在主压测 Evaluator 上，而非局部实例后丢弃）。
	if err := applyRules(*devices, eng); err != nil {
		fmt.Fprintf(os.Stderr, "规则集应用失败: %v\n", err)
		os.Exit(1)
	}

	gov := rules.NewGovernor()

	var latencies []time.Duration
	var tsdbWriteTime time.Duration
	totalSent := 0
	overrun := 0
	start := time.Now()
	tick := 0
	for time.Since(start) < *duration {
		batchStart := time.Now()
		ts := batchStart.UnixMilli()
		for d := 0; d < *devices; d++ {
			device := fmt.Sprintf("bench-%03d", d)
			for p := 0; p < *props; p++ {
				prop := fmt.Sprintf("p%02d", p)
				value := float64((d*31+p*7)%100) + float64(tick%5)*0.1
				dec := gov.Filter(device, "default", prop, value, ts)
				v := value
				if dec.Accept {
					v = dec.Value
				}
				_ = eng.Observe(device, "default", prop, v, ts)
				wStart := time.Now()
				_ = tdb.WriteOne(device+"/default/"+prop, ts, v)
				tsdbWriteTime += time.Since(wStart)
			}
		}
		lat := time.Since(batchStart)
		latencies = append(latencies, lat)
		totalSent += totalPoints
		tick++
		// 对齐下一周期边界（处理耗时 < 周期时休眠剩余时间；超周期轮标注）。
		if sleep := *interval - time.Since(batchStart); sleep > 0 {
			time.Sleep(sleep)
		} else {
			overrun++
		}
	}
	// 吞吐按调度口径计（批次数 × 周期）：末轮处理耗时 < 周期时，墙钟耗时
	// 会略超 duration（末轮 sleep 对齐），导致吞吐被低估（如 1999/2000）。
	wallElapsed := time.Since(start)
	scheduledElapsed := time.Duration(len(latencies)) * *interval

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		idx := int(float64(len(latencies)-1) * p)
		return latencies[idx]
	}

	st := tdb.Stats()
	fmt.Printf("\n=== 结果 ===\n")
	fmt.Printf("实际吞吐：      %d 点 / %s（调度口径 %d 轮 × %s）≈ %.0f 点/s（目标 2000 点/1s）\n",
		totalSent, wallElapsed.Round(time.Millisecond), len(latencies), interval,
		float64(totalSent)/scheduledElapsed.Seconds())
	fmt.Printf("单轮处理延迟：  P50=%s  P99=%s  max=%s（周期 %s）\n",
		pct(0.5).Round(time.Microsecond), pct(0.99).Round(time.Microsecond),
		latencies[len(latencies)-1].Round(time.Microsecond), interval)
	fmt.Printf("时序库：        序列 %d，点 %d（落库 %d），写耗时累计 %s（%.3f ms/点）\n",
		st.Series, st.Points, totalSent, tsdbWriteTime.Round(time.Millisecond),
		float64(tsdbWriteTime.Microseconds())/1000.0/float64(totalSent))
	if overrun > 0 {
		fmt.Printf("超周期轮：      %d 轮处理耗时 ≥ 周期（调度口径吞吐含其影响，见上）\n", overrun)
	}
	fmt.Printf("结论：          %s\n", verdict(float64(totalSent)/scheduledElapsed.Seconds(), pct(0.99), *interval))
}

// applyRules 生成阈值规则集并应用到主压测 Evaluator（每 10 台设备一条
// p00 > 50 规则，触发时 evaluator 产出事件——真实评估开销）。
func applyRules(devices int, eng *rules.Evaluator) error {
	type condition struct {
		Type  string  `json:"type"`
		Op    string  `json:"op"`
		Value float64 `json:"value"`
	}
	type action struct {
		Type     string `json:"type"`
		Severity string `json:"severity"`
		Message  string `json:"message"`
	}
	type rule struct {
		RuleID     string    `json:"ruleId"`
		Name       string    `json:"name"`
		DeviceName string    `json:"deviceName"`
		Property   string    `json:"property"`
		Condition  condition `json:"condition"`
		Action     action    `json:"action"`
	}
	rs := struct {
		Version int    `json:"version"`
		Rules   []rule `json:"rules"`
	}{Version: 1}
	for d := 0; d < devices; d += 10 {
		rs.Rules = append(rs.Rules, rule{
			RuleID:     fmt.Sprintf("bench-%03d", d),
			Name:       "bench",
			DeviceName: fmt.Sprintf("bench-%03d", d),
			Property:   "p00",
			Condition:  condition{Type: "threshold", Op: "gt", Value: 50},
			Action:     action{Type: "event", Severity: "warning", Message: "bench ${value}"},
		})
	}
	raw, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	var parsed rules.RuleSet
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return err
	}
	if err := eng.ApplyRuleSet(&parsed); err != nil {
		return err
	}
	fmt.Printf("规则集：已应用 %d 条阈值规则（每 10 台设备 1 条）\n", len(parsed.Rules))
	return nil
}

// verdict 输出达标结论。
func verdict(throughput float64, p99 time.Duration, interval time.Duration) string {
	if throughput >= 2000 && p99 < interval {
		return fmt.Sprintf("✅ 达标（吞吐 %.0f ≥ 2000 点/s，P99 %s < 周期 %s）", throughput, p99, interval)
	}
	if throughput >= 2000 {
		return fmt.Sprintf("⚠️ 吞吐达标但 P99 %s ≥ 周期 %s", p99, interval)
	}
	return fmt.Sprintf("❌ 未达标（吞吐 %.0f < 2000 点/s）", throughput)
}
