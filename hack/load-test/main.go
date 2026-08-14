// load-test 是 EdgeFlow 节点注册/心跳压测工具（WBS 8.4 缺口 G5 补做）。
// 模拟 N 个 edgecore 客户端并发连接 cloudcore：注册 + 周期心跳，
// 统计注册成功率、注册延迟与心跳延迟。
//
// 用法：
//
//	go run ./hack/load-test                  # 默认 10 节点、cloudcore :10000
//	LOAD_TEST_NODES=50 go run ./hack/load-test
//	LOAD_TEST_CLOUD=ws://127.0.0.1:10000 go run ./hack/load-test
//
// 输出：控制台摘要 + JSON 到 stdout（-json 标志）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"edgeflow/edge/pkg/edgehub"
)

type nodeResult struct {
	NodeID     string  `json:"nodeID"`
	Registered bool    `json:"registered"`
	RegisterMs float64 `json:"registerMs"`
	Heartbeats int     `json:"heartbeats"`
	AvgBeatMs  float64 `json:"avgHeartbeatMs"`
}

type summary struct {
	Nodes          int          `json:"nodes"`
	Registered     int          `json:"registered"`
	SuccessRate    float64      `json:"successRate"`
	AvgRegisterMs  float64      `json:"avgRegisterMs"`
	P95RegisterMs  float64      `json:"p95RegisterMs"`
	TotalBeats     int          `json:"totalHeartbeats"`
	AvgHeartbeatMs float64      `json:"avgHeartbeatMs"`
	DurationSec    float64      `json:"durationSec"`
	Results        []nodeResult `json:"results"`
}

func main() {
	cloudAddr := os.Getenv("LOAD_TEST_CLOUD")
	if cloudAddr == "" {
		cloudAddr = "ws://127.0.0.1:10000/v1/edge"
	}
	nodes := 10
	if v := os.Getenv("LOAD_TEST_NODES"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &nodes)
	}
	beatSec := 2 // 心跳间隔（压测用短间隔，正常 30s）
	if v := os.Getenv("LOAD_TEST_BEAT_SEC"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &beatSec)
	}
	durSec := 10
	if v := os.Getenv("LOAD_TEST_DURATION_SEC"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &durSec)
	}
	jsonOut := flag.Bool("json", false, "输出 JSON")
	flag.Parse()

	// cloudcore 需要 mTLS off（默认）——压测走明文通道
	var mu sync.Mutex
	results := make([]nodeResult, 0, nodes)
	var wg sync.WaitGroup

	for i := 0; i < nodes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			nodeID := fmt.Sprintf("load-%03d", i)
			client := edgehub.New(edgehub.Options{
				CloudAddr:         cloudAddr,
				NodeID:            nodeID,
				HeartbeatInterval: time.Duration(beatSec) * time.Second,
			})
			regStart := time.Now()
			client.Start()
			// 等待注册确认：轮询 LastHeartbeatAckTime 或直接按连接+注册时序等待
			registered := false
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				// 注册成功判据：RegisterAck 到达后 nodeName 非空
				// （不能用 LastHeartbeatAckTime——心跳周期默认 30s，压测
				// 短时窗口内不会有 Ack，会误判 0/10 注册成功）。
				if client.NodeName() != "" {
					registered = true
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			regMs := float64(time.Since(regStart).Milliseconds())

			// 心跳周期统计
			beats := 0
			beatTotal := time.Duration(0)
			beatEnd := time.Now().Add(time.Duration(durSec) * time.Second)
			lastAck := client.LastHeartbeatAckTime()
			for time.Now().Before(beatEnd) {
				time.Sleep(500 * time.Millisecond)
				ack := client.LastHeartbeatAckTime()
				if ack.After(lastAck) {
					beats++
					beatTotal += time.Since(lastAck)
					lastAck = ack
				}
			}
			avgBeat := 0.0
			if beats > 0 {
				avgBeat = float64(beatTotal.Milliseconds()) / float64(beats)
			}
			client.Stop()

			mu.Lock()
			results = append(results, nodeResult{
				NodeID: nodeID, Registered: registered, RegisterMs: regMs,
				Heartbeats: beats, AvgBeatMs: avgBeat,
			})
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// 汇总
	sum := summary{Nodes: nodes, DurationSec: float64(durSec), Results: results}
	for _, r := range results {
		if r.Registered {
			sum.Registered++
			sum.AvgRegisterMs += r.RegisterMs
		}
		sum.TotalBeats += r.Heartbeats
		sum.AvgHeartbeatMs += r.AvgBeatMs
	}
	if sum.Registered > 0 {
		sum.AvgRegisterMs /= float64(sum.Registered)
		sum.AvgHeartbeatMs /= float64(sum.Registered)
	}
	sum.SuccessRate = float64(sum.Registered) / float64(nodes) * 100
	// P95 注册延迟
	regs := make([]float64, 0)
	for _, r := range results {
		if r.Registered {
			regs = append(regs, r.RegisterMs)
		}
	}
	if len(regs) > 0 {
		// 简单近似：排序取 95 分位
		for i := 0; i < len(regs); i++ {
			for j := i + 1; j < len(regs); j++ {
				if regs[j] < regs[i] {
					regs[i], regs[j] = regs[j], regs[i]
				}
			}
		}
		idx := int(float64(len(regs)) * 0.95)
		if idx >= len(regs) {
			idx = len(regs) - 1
		}
		sum.P95RegisterMs = regs[idx]
	}

	if *jsonOut {
		data, _ := json.MarshalIndent(sum, "", "  ")
		fmt.Println(string(data))
		return
	}
	fmt.Printf("=== EdgeFlow 负载测试摘要 ===\n")
	fmt.Printf("节点数: %d  注册成功: %d（%.1f%%）\n", sum.Nodes, sum.Registered, sum.SuccessRate)
	fmt.Printf("平均注册延迟: %.1f ms  P95: %.1f ms\n", sum.AvgRegisterMs, sum.P95RegisterMs)
	fmt.Printf("总心跳: %d  平均心跳间隔: %.1f ms\n", sum.TotalBeats, sum.AvgHeartbeatMs)
	fmt.Printf("时长: %.0fs\n", sum.DurationSec)
}
