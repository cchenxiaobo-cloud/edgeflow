// Package protocol 定义云边通信的消息协议（WBS 4.1）。
// 设计：统一 JSON 信封，业务数据放 Payload；信封带 version 字段，
// 后续可平滑升级到 Protobuf（只换序列化层，协议语义不变）。
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 消息类型枚举（开放集合：新增类型不改信封结构）。
const (
	TypeRegister      = "Register"      // 边→云：节点注册（连接建立后第一条）
	TypeRegisterAck   = "RegisterAck"   // 云→边：注册结果
	TypeHeartbeat     = "Heartbeat"     // 边→云：心跳保活
	TypeHeartbeatAck  = "HeartbeatAck"  // 云→边：心跳应答
	TypePodSync       = "PodSync"       // 云→边：Pod 配置下发（M1/M2）
	TypeConfigSync    = "ConfigSync"    // 云→边：ConfigMap/Secret 下发（M2）
	TypePodStatus     = "PodStatus"     // 边→云：Pod 状态上报（M1/M2）
	TypeDeviceReport  = "DeviceReport"  // 边→云：设备数据/状态上报（M3）
	TypeDeviceCommand = "DeviceCommand" // 云→边：设备操作指令（M3）
	TypeNodeJob       = "NodeJob"       // 云→边：任务分发（已关闭：v0.1.0 范围外，保留协议占位）
	TypeNodeJobResult = "NodeJobResult" // 边→云：任务结果（已关闭：v0.1.0 范围外，保留协议占位）
	TypeAck           = "Ack"           // 双向：通用确认
)

// 协议版本。
const Version = "v1"

// Message 是云边通信的统一消息信封。
type Message struct {
	ID            string          `json:"id"`            // 消息唯一 ID（UUID），ACK 关联 + 幂等去重键
	Type          string          `json:"type"`          // 消息类型（见 Type* 常量）
	Version       string          `json:"version"`       // 协议版本
	Source        string          `json:"source"`        // 来源标识：cloud 或节点 ID
	Target        string          `json:"target"`        // 目标：cloud / 节点 ID / 广播组
	Timestamp     int64           `json:"timestamp"`     // 毫秒时间戳
	CorrelationID string          `json:"correlationId"` // 关联 ID：请求-响应配对
	Payload       json.RawMessage `json:"payload"`       // 类型相关负载
}

// NewMessage 创建一个带默认字段的消息（ID/Version/Timestamp 自动填充）。
func NewMessage(msgType, source, target string, payload any) (*Message, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	return &Message{
		ID:        newID(),
		Type:      msgType,
		Version:   Version,
		Source:    source,
		Target:    target,
		Timestamp: time.Now().UnixMilli(),
		Payload:   raw,
	}, nil
}

// DecodePayload 把负载解析到 out 结构。
func (m *Message) DecodePayload(out any) error {
	if len(m.Payload) == 0 {
		return errors.New("payload is empty")
	}
	return json.Unmarshal(m.Payload, out)
}

// 校验消息是否合法：类型、版本、来源、目标、时间戳。
func (m *Message) Validate() error {
	if m.Type == "" {
		return errors.New("type is required")
	}
	if m.Version == "" {
		return errors.New("version is required")
	}
	if m.Source == "" {
		return errors.New("source is required")
	}
	if m.Target == "" {
		return errors.New("target is required")
	}
	if m.ID == "" {
		return errors.New("id is required")
	}
	return nil
}
