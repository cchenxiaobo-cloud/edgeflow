// Pod 状态上报循环（WBS 6.3 边缘侧）。
//
// 职责：周期读取 Edged 的状态表，把每个 Pod 的状态构造成一条 PodStatus
// 消息发给云端。上报与调谐解耦——调谐负责"收敛状态"（edge/pkg/edged），
// 上报只负责"采样快照"，两者周期独立（默认 5s / 30s）。
package main

import (
	"os"
	"time"

	"edgeflow/edge/pkg/edged"
	"edgeflow/edge/pkg/edgehub"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// 上报循环常量。
const (
	// defaultReportInterval 是默认上报周期（与调谐 5s 解耦，见 runStatusReportLoop 注释）。
	defaultReportInterval = 30 * time.Second
	// minReportInterval / maxReportInterval 是上报周期的合法范围（env 保护）。
	minReportInterval = time.Second
	maxReportInterval = 10 * time.Minute
	// targetCloud 是上报消息 Target 的固定值（契约：云侧标识为 "cloud"）。
	targetCloud = "cloud"
)

// runStatusReportLoop 是 Pod 状态上报循环：
// 启动即上报一轮，之后每 interval 上报一轮，直到 stopCh 关闭。
//
// 设计决策（与调谐解耦，简化）：
//   - 不在 Edged.Trigger 后额外立即上报：调谐每 5s 收敛一次状态表，上报
//     每 30s 采样一轮——触发即刻上报只把最坏延迟从 30s 降到 5s，却让上报
//     节奏与调谐耦合（触发频率、并发与调试复杂度上升），收益有限；
//   - 固定周期含首次上报：edgecore 启动后云端无需等待即可看到 Pod 状态。
func runStatusReportLoop(client *edgehub.Client, edgedSvc *edged.Edged, nodeID string, interval time.Duration, stopCh <-chan struct{}) {
	reportPodStatuses(client, edgedSvc, nodeID) // 启动即上报一轮（含首次）

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			reportPodStatuses(client, edgedSvc, nodeID)
		case <-stopCh:
			log.Infof("Pod 状态上报循环已停止")
			return
		}
	}
}

// reportPodStatuses 上报一轮全部 Pod 状态：每条状态独立成一条 PodStatus 消息。
//
// QoS 语义（尽力而为、最终一致）：
//   - 发送失败只记 Warn，不重试、不阻塞主流程——EdgeHub 具备断线重连能力，
//     未上报的状态会在下一轮周期自动补报；
//   - 单条失败不影响本轮其余条目（循环继续）；
//   - 可靠性提升（消息缓存、Ack 确认、按序投递）属于 M3 可靠上报范围。
func reportPodStatuses(client *edgehub.Client, edgedSvc *edged.Edged, nodeID string) {
	for _, msg := range buildStatusMessages(nodeID, edgedSvc.BuildStatusPayload(nodeID)) {
		if err := client.Send(msg); err != nil {
			var p edged.PodStatusPayload
			_ = msg.DecodePayload(&p) // 自构造消息，解析不会失败
			log.Warnf("Pod 状态上报失败（namespace=%s, pod=%s, phase=%s）: %v",
				p.Namespace, p.PodName, p.Phase, err)
		}
	}
}

// buildStatusMessages 把状态负载数组构造成 PodStatus 消息列表（纯函数，便于单测）。
// 契约：Source=nodeID、Target="cloud"、Type=PodStatus、Payload=单条状态。
// 端到端链路（真实 WebSocket 通道 + 云端接收）由 hack/mock-cloudhub 覆盖。
func buildStatusMessages(nodeID string, payloads []edged.PodStatusPayload) []*protocol.Message {
	msgs := make([]*protocol.Message, 0, len(payloads))
	for _, p := range payloads {
		msg, err := protocol.NewMessage(protocol.TypePodStatus, nodeID, targetCloud, p)
		if err != nil {
			// 负载是可序列化结构体，正常不会失败；防御性跳过该条
			log.Warnf("构造 PodStatus 消息失败（namespace=%s, pod=%s）: %v",
				p.Namespace, p.PodName, err)
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// durationFromEnv 解析环境变量为时长；未设置/非法/非正数时记警告并返回默认值。
func durationFromEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			// 上下限保护（M2B 审查 P2-7）：周期过短打爆云边通道，
			// 过长导致状态陈旧；越界回退默认并告警。
			if d < minReportInterval || d > maxReportInterval {
				log.Warnf("%s=%s 超出合法范围（%v~%v），使用默认 %v",
					key, v, minReportInterval, maxReportInterval, def)
				return def
			}
			return d
		}
		log.Warnf("%s 非法（%q），使用默认 %v", key, v, def)
	}
	return def
}
