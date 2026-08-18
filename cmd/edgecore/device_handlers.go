// 设备链路装配（WBS 3.5/5.3 边缘侧）：DeviceCommand 下发处理 +
// DeviceReport 周期上报。
//
// 职责边界：
//   - 本文件只做"消息 ↔ 影子存储"的转换与周期驱动；影子本体
//     （Twin/TwinStore）与消息契约在 edge/pkg/devicetwin；
//   - 设备指令的实际执行（下发到物理设备）经 DeviceCommandExecutor 接口
//     注入，由装配层（main.go）把 Mapper 注册表适配成该接口后接入。
//     Mapper 框架由另一 Agent 并行开发（edge/pkg/mapper），当前以 nil
//     占位：指令只更新 Twin.Desired（骨架路径），见 handleDeviceCommand。
package main

import (
	"errors"
	"fmt"
	"time"

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/edge/pkg/edgehub"
	"edgeflow/edge/pkg/mapper"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// handleDeviceCommand 处理一条云端下发的设备指令（TypeDeviceCommand）。
//
// 处理流程：解析负载 → 校验必填字段 → 按 deviceName 路由到设备执行器
// （DeviceCommandExecutor，Mapper 适配器；未接入时跳过并告警）→ 无论
// 执行结果如何都把期望值写入 Twin.Desired（数字孪生期望态以云端声明
// 为准，实际值由设备侧后续上报收敛）→ 返回。
//
// 返回语义（与 EdgeHub 自动 Ack 联动，见 edgehub/ack.go）：
//   - nil → 回 Ack code=ok，云端下发 API 返回 200；
//   - error → 回 Ack code=error，云端下发 API 映射 502（消息已送达但
//     处理失败；云端重试同 ID 时允许重新执行，符合 QoS 1 语义）。
func handleDeviceCommand(twins *devicetwin.TwinStore, exec devicetwin.DeviceCommandExecutor, msg *protocol.Message) error {
	if msg == nil {
		return errors.New("DeviceCommand 消息为空（nil）")
	}
	var cmd devicetwin.DeviceCommandPayload
	if err := msg.DecodePayload(&cmd); err != nil {
		return fmt.Errorf("解析 DeviceCommand 负载失败: %w", err)
	}
	if cmd.DeviceName == "" || cmd.Property == "" {
		return errors.New("DeviceCommand 缺少 deviceName 或 property 字段")
	}
	if cmd.Namespace == "" {
		cmd.Namespace = devicetwin.DefaultNamespace
	}

	// 路由到设备执行器（Mapper 适配器）。无论执行成败，期望值都写入
	// 影子——指令语义是"声明期望态"，实际收敛由设备侧上报闭环完成，
	// 不能因单次执行失败丢失云端期望；执行失败时返回 error（云端收
	// 502），云端可据此重试（重试同 ID 时边缘允许重新执行，QoS 1）。
	execErr := error(nil)
	if exec != nil {
		if err := exec.ExecuteCommand(cmd.DeviceName, cmd.Namespace, cmd.Property, cmd.Value); err != nil {
			log.Errorf("设备指令执行失败（%s/%s.%s=%v）: %v",
				cmd.Namespace, cmd.DeviceName, cmd.Property, cmd.Value, err)
			execErr = fmt.Errorf("执行设备指令失败: %w", err)
		}
	} else {
		// 骨架路径（Mapper 未装配/已关闭）：只更新 Twin.Desired。
		// Mapper 装配开关关闭或注册表为空时进入此分支，指令仅修改
		// 期望态，实际收敛由设备侧后续上报闭环完成。
		log.Warnf("设备指令执行器未接入（Mapper 装配关闭或未装配），仅更新 Twin.Desired: %s/%s.%s=%v",
			cmd.Namespace, cmd.DeviceName, cmd.Property, cmd.Value)
	}

	twins.SetDesired(cmd.DeviceName, cmd.Namespace, cmd.Property, cmd.Value)
	if execErr != nil {
		return execErr
	}
	log.Infof("设备指令已处理（Desired 更新）: %s/%s.%s=%v",
		cmd.Namespace, cmd.DeviceName, cmd.Property, cmd.Value)
	return nil
}

// runDeviceReportLoop 是设备数据上报循环（仿 Pod 状态上报循环）：
// 启动即上报一轮，之后每 intervalFn() 返回的周期上报一轮，直到 stopCh 关闭。
// 每轮流程：Mapper 采集汇入影子（collectMapperReports）→ 从 Twin 快照
// 生成 DeviceReport 消息，每条影子一条消息。reg 可传 nil（纯影子模式）。
// 周期支持热重载（WBS 2.7）：每次 tick 后重新读取 intervalFn，
// 周期变化即重置 ticker（下一轮起按新周期）。
func runDeviceReportLoop(client *edgehub.Client, reg *mapper.MapperRegistry, twins *devicetwin.TwinStore, nodeID string, intervalFn func() time.Duration, stopCh <-chan struct{}) {
	reportDeviceReports(client, reg, twins, nodeID) // 启动即上报一轮（含首次）

	interval := safeInterval(intervalFn())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			reportDeviceReports(client, reg, twins, nodeID)
		case <-stopCh:
			log.Infof("设备上报循环已停止")
			return
		}
		// 热重载：周期变更后重置 ticker（同一 goroutine 内、tick 之后调用）
		if d := safeInterval(intervalFn()); d != interval {
			interval = d
			ticker.Reset(d)
			log.Infof("设备上报周期热更新: %v", d)
		}
	}
}

// reportDeviceReports 上报一轮全部设备影子：先把 Mapper 采集值汇入影子
// （影子是上报的唯一数据源），再把每条影子独立成一条 DeviceReport 消息。
//
// QoS 语义（尽力而为、最终一致，与 Pod 上报一致）：
//   - 发送失败只记 Warn，不重试、不阻塞主流程——EdgeHub 具备断线重连
//     能力，未上报的状态会在下一轮周期自动补报；
//   - 单条失败不影响本轮其余条目（循环继续）。
func reportDeviceReports(client *edgehub.Client, reg *mapper.MapperRegistry, twins *devicetwin.TwinStore, nodeID string) {
	collectMapperReports(reg, twins, time.Now().UnixMilli())
	for _, msg := range buildDeviceReportMessages(nodeID, twins, time.Now().UnixMilli()) {
		if err := client.Send(msg); err != nil {
			log.Warnf("设备上报失败: %v", err)
		}
	}
}

// buildDeviceReportMessages 从 Twin 快照构造 DeviceReport 消息列表（纯函数，便于单测）。
//
// 契约：Source=nodeID、Target="cloud"、Type=DeviceReport、Payload=单条
// DeviceReportPayload（properties=影子 Reported 快照，reportedAt=本次
// 上报时间——即参数 reportedAt，由调用方注入以便测试固定时间戳）。
// 每条影子独立成一条消息（与 PodStatus 逐 Pod 上报同构，便于云端按设备落库）。
func buildDeviceReportMessages(nodeID string, twins *devicetwin.TwinStore, reportedAt int64) []*protocol.Message {
	snaps := twins.SnapshotAll()
	msgs := make([]*protocol.Message, 0, len(snaps))
	for _, t := range snaps {
		msg, err := protocol.NewMessage(protocol.TypeDeviceReport, nodeID, targetCloud, devicetwin.DeviceReportPayload{
			DeviceName: t.DeviceName,
			Namespace:  t.Namespace,
			Properties: t.Reported,
			ReportedAt: reportedAt,
		})
		if err != nil {
			// 负载是可序列化结构体，正常不会失败；防御性跳过该条
			log.Warnf("构造 DeviceReport 消息失败（%s/%s）: %v", t.Namespace, t.DeviceName, err)
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs
}
