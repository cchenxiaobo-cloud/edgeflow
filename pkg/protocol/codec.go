package protocol

import (
	"encoding/json"
	"fmt"
)

// Encode 把消息序列化为 JSON 字节（带换行，便于流式读取）。
func Encode(m *Message) ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	return append(data, '\n'), nil
}

// Decode 从 JSON 字节解析消息并做基础校验。
func Decode(data []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal message: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
