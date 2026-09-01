package mqttsim

// persistence.go — broker 端 QoS2 暂存持久化（v0.27.0）。
//
// 与 pkg/mqtt 的 client 端持久化（mqtt.ResumePending）共用文件格式：
// qos2-<4位hex PacketID>.json，单条 JSON 记录。差异点：
//   - broker 端 parked 消息按连接隔离（v0.26.0 P1 修复），但持久化记录
//     无法以 *simClient 为键（连接对象不跨重启）——记录只按 PacketID 存，
//     恢复时消息回到 broker 全局，重投递由恢复方（测试/上层）驱动。
//   - 复用同连接内的 PUBREL 完成投递时，内存删除 + 记录删除同步进行。
//   - unregister（连接断开）只清内存隔离表，不删记录：记录的生命周期
//     跨连接，这正是持久化的意义。
//
// 硬约束：persistDir 为空 = 全部跳过（v0.26.0 行为逐字一致）；零第三方
// 依赖；写盘 temp+rename 原子写。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"edgeflow/pkg/mqtt"
)

// brokerQoS2Record 是 broker 端 parked 消息的持久化记录。与 client 端
// qos2Record 字段对齐但 Kind 固定 'b'（broker parked），避免两端记录混放
// 同目录时误读。
type brokerQoS2Record struct {
	Kind    byte   `json:"kind"` // 恒 'b'
	PktID   uint16 `json:"pkt_id"`
	Topic   string `json:"topic"`
	Payload []byte `json:"payload"`
	Dup     byte   `json:"dup,omitempty"`
	QoS     byte   `json:"qos"`
}

// brokerSetPersistDir 启用 broker 的 QoS2 记录目录（空串=禁用）并装载
// 已有记录到孤儿表（重启恢复）。仅启动前调用（serve 循环无锁读相关
// 字段；与 NewBrokerWithOptions 一起在首个连接前完成）。
func (b *Broker) brokerSetPersistDir(dir string) {
	b.mu.Lock()
	b.persistDir = dir
	b.orphanQoS2 = make(map[uint16]*mqtt.Publish)
	b.mu.Unlock()
	if dir == "" {
		return
	}
	// 装载历史 parked 记录：坏记录删除，外来（client 侧）记录保留在盘。
	if pending, err := BrokerResumePending(dir); err == nil {
		b.mu.Lock()
		for i := range pending {
			p := pending[i]
			b.orphanQoS2[p.PacketID] = &p
		}
		b.mu.Unlock()
	}
}

// brokerQoS2Save 把一条 parked 消息写入记录目录（dir 空 = no-op）。
// 失败返回错误但不阻塞协议应答——调用方（serve park 分支）软失败。
func brokerQoS2Save(dir string, id uint16, p *mqtt.Publish) error {
	if dir == "" {
		return nil
	}
	rec := brokerQoS2Record{Kind: 'b', PktID: id, Topic: p.Topic, Payload: p.Payload, Dup: p.Dup, QoS: p.QoS}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("mqttsim: marshal qos2 record: %w", err)
	}
	name := filepath.Join(dir, mqtt.QoS2RecordFileName(id))
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("mqttsim: write qos2 record: %w", err)
	}
	if err := os.Rename(tmp, name); err != nil {
		return fmt.Errorf("mqttsim: commit qos2 record: %w", err)
	}
	return nil
}

// brokerQoS2Remove 删除 id 的记录（不存在视为成功；dir 空 = no-op）。
func brokerQoS2Remove(dir string, id uint16) error {
	if dir == "" {
		return nil
	}
	err := os.Remove(filepath.Join(dir, mqtt.QoS2RecordFileName(id)))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("mqttsim: remove qos2 record: %w", err)
	}
	return nil
}

// BrokerResumePending 扫描 dir 中全部 broker parked 记录并返回消息列表。
// 供测试/上层在 broker 重启后驱动重投递（当前版本不做自动 fanout——
// 重启后订阅关系已失，自动投递反而违背订阅语义；调用方自行决定）。
// 坏记录删除跳过。dir 为空串返回 nil。
func BrokerResumePending(dir string) ([]mqtt.Publish, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("mqttsim: read persistence dir: %w", err)
	}
	var out []mqtt.Publish
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "qos2-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("mqttsim: read qos2 record %s: %w", name, err)
		}
		var rec brokerQoS2Record
		if err := json.Unmarshal(raw, &rec); err != nil || rec.PktID == 0 {
			_ = os.Remove(path)
			continue
		}
		if rec.Kind != 'b' {
			// 外来记录（如 client 侧 'o'/'i'）：跳过且保留文件——两端可能
			// 共目录（误配），broker 无权删除对方的恢复凭据。
			continue
		}
		out = append(out, mqtt.Publish{QoS: rec.QoS, PacketID: rec.PktID, Topic: rec.Topic, Payload: rec.Payload, Dup: rec.Dup})
	}
	return out, nil
}
