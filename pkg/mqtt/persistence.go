package mqtt

// persistence.go — QoS2 in-flight 持久化（v0.27.0）。
//
// 目标：把 v0.26.0 的「进程内 exactly-once」边界推进到「跨重连恢复」：
//   - 上行：PUBLISH 已发未收 PUBREC，或 PUBREL 已发未收 PUBCOMP 的交换
//     落盘；重连后上层调 ResumePending 回放（Phase 1 重发 PUBLISH(Dup=1)，
//     Phase 2 重发 PUBREL），broker 侧协议语义仍保证恰好一次。
//   - 下行：已回 PUBREC、parked 等 PUBREL 的消息落盘；重连后如果 broker
//     重发 PUBREL（新连接 PacketID 可能复用，broker 3.1.1 无跨连接会话，
//     是否重发由 broker 决定），readPump 照常完成投递并清记录；若 broker
//     不再重发，客户端无法独立完成该交换 —— 3.1.1 协议边界，登记
//     KNOWN-ISSUES §27。
//
// 硬约束：PersistenceDir 为空 = 全部跳过（v0.26.0 内存态行为逐字一致）；
// 零第三方依赖（标准库 JSON）；文件名 = qos2-<4位hex PacketID>.json；
// 写盘走 temp+rename 原子写；单文件损坏不影响其余记录（回放时跳过并
// 删除坏文件）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// qos2Record 是一条 QoS2 in-flight 交换的持久化记录。
//
// Kind 取值：'o' = 上行（outbound），'i' = 下行 parked（inbound）。
// 上行 Phase：1 = PUBLISH 已发等 PUBREC；2 = PUBREL 已发等 PUBCOMP。
// 下行 Phase 恒为 1（已回 PUBREC 等 PUBREL）。
type qos2Record struct {
	Kind    byte   `json:"kind"`
	Phase   int    `json:"phase"`
	PktID   uint16 `json:"pkt_id"`
	Topic   string `json:"topic"`
	Payload []byte `json:"payload"`
}

// qos2FileName 是记录文件名（PacketID 四位十六进制，零填充）。
func qos2FileName(id uint16) string {
	return fmt.Sprintf("qos2-%04x.json", id)
}

// QoS2RecordFileName 是 QoS2 持久化记录文件名（v0.27.0 导出，供
// pkg/mqttsim broker 端持久化复用同一命名规则，两端记录不混放同目录）。
func QoS2RecordFileName(id uint16) string {
	return qos2FileName(id)
}

// qos2Save 把一条记录原子写入 dir（dir 为空串时是 no-op，返回 nil）。
func qos2Save(dir string, rec qos2Record) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mqtt: create persistence dir: %w", err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("mqtt: marshal qos2 record: %w", err)
	}
	name := filepath.Join(dir, qos2FileName(rec.PktID))
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("mqtt: write qos2 record tmp: %w", err)
	}
	if err := os.Rename(tmp, name); err != nil {
		return fmt.Errorf("mqtt: commit qos2 record: %w", err)
	}
	return nil
}

// qos2Remove 删除 id 的记录（不存在视为成功）。
func qos2Remove(dir string, id uint16) error {
	if dir == "" {
		return nil
	}
	err := os.Remove(filepath.Join(dir, qos2FileName(id)))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("mqtt: remove qos2 record: %w", err)
	}
	return nil
}

// QoS2Pending 描述一条待恢复的 QoS2 交换（ResumePending 的产出）。
type QoS2Pending struct {
	Inbound bool // true = 下行 parked（等 broker PUBREL）；false = 上行
	Phase   int  // 上行：1=重发 PUBLISH(Dup)，2=重发 PUBREL
	PktID   uint16
	Topic   string
	Payload []byte
}

// ResumePending 扫描 dir 中全部 QoS2 记录并返回待恢复清单。调用方
// （上层重连逻辑）逐条回放后必须对每条调用 ResumeComplete 或
// qos2Remove，避免下次重复回放。坏记录（无法解析）删除并跳过，不阻塞
// 其余恢复。dir 为空串时返回 nil（无持久化 = 无可恢复）。
func ResumePending(dir string) ([]QoS2Pending, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("mqtt: read persistence dir: %w", err)
	}
	var out []QoS2Pending
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "qos2-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("mqtt: read qos2 record %s: %w", name, err)
		}
		var rec qos2Record
		if err := json.Unmarshal(b, &rec); err != nil || rec.PktID == 0 {
			// 坏记录：删除跳过（不阻塞整体恢复）。
			_ = os.Remove(path)
			continue
		}
		switch rec.Kind {
		case 'o':
			if rec.Phase != 1 && rec.Phase != 2 {
				_ = os.Remove(path)
				continue
			}
			out = append(out, QoS2Pending{Inbound: false, Phase: rec.Phase, PktID: rec.PktID, Topic: rec.Topic, Payload: rec.Payload})
		case 'i':
			out = append(out, QoS2Pending{Inbound: true, Phase: 1, PktID: rec.PktID, Topic: rec.Topic, Payload: rec.Payload})
		default:
			_ = os.Remove(path)
		}
	}
	return out, nil
}

// ResumeComplete 报告一条已回放的记录完成（删除文件）。dir 为空串 no-op。
func ResumeComplete(dir string, id uint16) error {
	return qos2Remove(dir, id)
}

// Resume 回放本目录中全部未完成的上行 QoS2 交换（Phase1 重发
// PUBLISH(Dup=1) 后重走 PUBREC/PUBREL/PUBCOMP 等待；Phase2 直接重发
// PUBREL 等 PUBCOMP）。下行 parked 记录不在此回放：它们等的是 broker
// 的 PUBREL，若 broker 重发（PacketID 可能复用），readPump 会走正常
// 投递路径并清记录；若 broker 不重发，记录保留，不影响新交换（文件名
// 冲突时新交换的写入会覆盖旧记录，语义仍一致）。
//
// 建议在 Dial 成功后、常规发布开始前调用一次；也可跳过（完全可选）。
// 返回成功回放的条数；单条回放失败立即中止并返回错误（调用方可重试
// 或放弃，已回放条目不回滚）。
func (c *Client) Resume() (int, error) {
	if c.persistDir == "" || !c.enableQoS2 {
		return 0, nil
	}
	pending, err := ResumePending(c.persistDir)
	if err != nil {
		return 0, err
	}
	replayed := 0
	var maxID uint32
	for _, p := range pending {
		if p.Inbound {
			continue // 下行记录等 broker PUBREL，不主动回放
		}
		if err := c.replayOutbound(p); err != nil {
			return replayed, fmt.Errorf("mqtt: resume qos2 packet %d: %w", p.PktID, err)
		}
		if err := ResumeComplete(c.persistDir, p.PktID); err != nil {
			return replayed, err
		}
		if uint32(p.PktID) > maxID {
			maxID = uint32(p.PktID)
		}
		replayed++
	}
	// 快进计数器：回放已消费 [1,maxID] 中的这些 id，新连接计数器却从
	// 1 重新计数；若计数器落后于 maxID，下个 nextID 会撞上已回放的 id。
	// 单调 CAS 推进到 maxID（nextID 随后返回 maxID+1，不再碰撞）。
	if maxID > 0 {
		for {
			cur := atomic.LoadUint32(&c.packetID)
			if cur >= maxID {
				break
			}
			if atomic.CompareAndSwapUint32(&c.packetID, cur, maxID) {
				break
			}
		}
	}
	return replayed, nil
}

// replayOutbound 完成一条上行回放：注册 ack 等待器，按 Phase 补完剩余
// 握手脚（Phase1 从 PUBLISH(Dup) 开始，Phase2 从 PUBREL 开始）。
func (c *Client) replayOutbound(p QoS2Pending) error {
	if err := validateTopicName(p.Topic); err != nil {
		return err
	}
	ch := c.registerAck(p.PktID)
	defer c.unregisterAck(p.PktID)
	if p.Phase == 1 {
		pk := &Publish{QoS: 2, PacketID: p.PktID, Topic: p.Topic, Payload: p.Payload, Dup: 1}
		if err := c.write(pk); err != nil {
			return err
		}
		rec, err := c.waitAck(ch)
		if err != nil {
			return err
		}
		if _, ok := rec.(*Pubrec); !ok {
			return fmt.Errorf("mqtt: expected PUBREC for packet id %d", p.PktID)
		}
		if err := qos2Save(c.persistDir, qos2Record{Kind: 'o', Phase: 2, PktID: p.PktID, Topic: p.Topic, Payload: p.Payload}); err != nil {
			return err
		}
	}
	if err := c.write(&Pubrel{PacketID: p.PktID}); err != nil {
		return err
	}
	comp, err := c.waitAck(ch)
	if err != nil {
		return err
	}
	if _, ok := comp.(*Pubcomp); !ok {
		return fmt.Errorf("mqtt: expected PUBCOMP for packet id %d", p.PktID)
	}
	return nil
}
