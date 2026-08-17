// 消息路由：云端控制器向边缘节点下发消息的入口（WBS 4.3）。
//
// 路由规则（与协议草案 §4.4 对齐）：
//   - Target == "*"（TargetBroadcast）→ 广播给所有已注册节点
//   - 其他非空 Target → 单播给对应 nodeID 的节点
//
// 云发往边缘的消息约定：Source 恒为 "cloud"，Target 为节点 ID 或 "*"。
// 广播组（主题订阅式路由）属后续版本（WBS 4.7），本版只做节点级路由：
// 订阅表、主题过滤、按组下发均未实现，需要时在 Deliver 内扩展即可。
package cloudhub

import (
	"errors"
	"fmt"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// TargetBroadcast 是广播目标：消息 Target 为该值时，路由层向所有已注册节点发送。
// 未来引入广播组（主题订阅）后，组名与 "*" 可并存，规则在 Deliver 中扩展。
const TargetBroadcast = "*"

// 路由层错误（调用方用 errors.Is 判断）。
var (
	// ErrNodeOffline 表示目标节点不在线：未注册，或连接已断开/发送缓冲已满。
	ErrNodeOffline = errors.New("cloudhub: node offline")
	// ErrEmptyTarget 表示消息缺少 Target 字段，路由层无法决定投递目标。
	ErrEmptyTarget = errors.New("cloudhub: message target is empty")
)

// SendToNode 向指定节点发送一条消息（云→边单播）。
//
// 语义：
//   - 节点未注册或连接不可用 → 返回 ErrNodeOffline（errors.Is 可判定）
//   - 投递走现有发送缓冲 channel（写循环串行写出），不直接写连接，避免并发写
//   - Source/Target 为空的字段按云→边约定补全（Source="cloud"、Target=nodeID）；
//     补全发生在浅拷贝上，不修改调用方传入的消息
//   - 消息进入发送缓冲即视为成功（异步写出，不等待对端确认）
func (s *Server) SendToNode(nodeID string, msg *protocol.Message) error {
	if msg == nil {
		return fmt.Errorf("cloudhub: 消息为空（nil）")
	}
	s.mu.RLock()
	c := s.registry[nodeID]
	s.mu.RUnlock()
	if c == nil {
		return fmt.Errorf("%w: 节点 %q 未注册", ErrNodeOffline, nodeID)
	}

	m := normalizeOutbound(msg, nodeID)
	// 按连接协商结果编码（旧边缘未协商 → 明文，见 server.go encodeFor）
	data, err := s.encodeFor(c, m)
	if err != nil {
		return fmt.Errorf("cloudhub: 编码发往节点 %q 的消息失败: %w", nodeID, err)
	}
	if !c.trySend(data) {
		// 连接已关闭或发送缓冲已满（慢客户端）：trySend 已关闭该连接，
		// 节点将很快从注册表移除，此刻视为不在线。
		return fmt.Errorf("%w: 节点 %q 连接不可用（已关闭或发送缓冲已满）", ErrNodeOffline, nodeID)
	}
	return nil
}

// Broadcast 向所有已注册节点广播消息，返回成功送达数。
//
// 语义：
//   - 逐节点非阻塞投递：慢客户端或已断开连接只影响自身计数，不阻塞其他节点
//   - 无节点在线时返回 0（不算错误，由调用方决定是否关注）
//   - 广播期间新注册的节点可能收不到本条消息（按发送时刻的注册表快照投递）
func (s *Server) Broadcast(msg *protocol.Message) int {
	if msg == nil {
		return 0
	}
	// 先取注册表快照再投递：避免持有读锁期间执行 trySend
	// （trySend 在缓冲满时会关闭连接，属于较慢操作，不应占锁）。
	s.mu.RLock()
	targets := make([]*conn, 0, len(s.registry))
	for _, c := range s.registry {
		targets = append(targets, c)
	}
	s.mu.RUnlock()

	m := normalizeOutbound(msg, TargetBroadcast)
	sent := 0
	for _, c := range targets {
		// 逐连接按协商结果编码：不同节点可能处于不同压缩协商状态
		// （旧边缘明文 / 新边缘压缩），编码失败仅影响该节点计数
		data, err := s.encodeFor(c, m)
		if err != nil {
			log.Errorf("编码发往节点的广播消息失败: %v", err)
			continue
		}
		if c.trySend(data) {
			sent++
		}
	}
	return sent
}

// Deliver 是云端控制器向边缘下发消息的统一路由入口（WBS 4.3）。
//
// 路由规则：
//   - 消息为 nil → 返回错误
//   - Target 为空 → 返回 ErrEmptyTarget
//   - Target == "*" → 广播（无节点在线时静默成功，广播本就是尽力而为）
//   - 其他 Target → 按节点 ID 单播；节点不在线返回 ErrNodeOffline
//
// 广播的尽力而为语义（P2-4 闭环）：广播不因个别节点失败而报错——慢客户端
// 或已断开连接只影响自身计数，无节点在线时送达 0 也算成功。Broadcast 返回
// 的送达计数仅用于观测（此处记日志），调用方无需处理。
//
// 后续云端控制器（EdgeController）只调用本方法，不直接接触连接层。
func (s *Server) Deliver(msg *protocol.Message) error {
	if msg == nil {
		return fmt.Errorf("cloudhub: 消息为空（nil）")
	}
	if msg.Target == "" {
		return ErrEmptyTarget
	}
	if msg.Target == TargetBroadcast {
		sent := s.Broadcast(msg)
		log.Infof("广播消息投递完成: 送达 %d 个节点（type=%s msgID=%s）", sent, msg.Type, msg.ID)
		return nil
	}
	return s.SendToNode(msg.Target, msg)
}

// normalizeOutbound 返回消息的浅拷贝并按云→边约定补全路由字段：
// Source 为空补 "cloud"（云发往边缘的消息 Source 恒为 cloud），
// Target 为空补默认目标。原消息不被修改。
func normalizeOutbound(msg *protocol.Message, defaultTarget string) *protocol.Message {
	m := *msg
	if m.Source == "" {
		m.Source = "cloud"
	}
	if m.Target == "" {
		m.Target = defaultTarget
	}
	return &m
}
