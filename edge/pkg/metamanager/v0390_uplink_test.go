// v0390_uplink_test.go：上行补传队列单测（spec 0012 US-1）。
package metamanager

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"edgeflow/pkg/protocol"
)

// openTestUplinkQueue 在临时目录打开 Store + UplinkQueue。
func openTestUplinkQueue(t *testing.T, maxRows int) (*UplinkQueue, *Store) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("打开测试 Store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q, err := NewUplinkQueue(st, maxRows)
	if err != nil {
		t.Fatalf("创建测试队列失败: %v", err)
	}
	return q, st
}

// testUplinkMsg 构造固定 ID 的上行测试消息。
func testUplinkMsg(t *testing.T, id string) *protocol.Message {
	t.Helper()
	m, err := protocol.NewMessage(protocol.TypeRuleEvent, "edge-1", "cloud", map[string]string{"k": id})
	if err != nil {
		t.Fatalf("构造测试消息失败: %v", err)
	}
	m.ID = id
	return m
}

// TestUplinkQueueOrderAndClamp 验证出队序（priority DESC, id ASC）与优先级归一。
func TestUplinkQueueOrderAndClamp(t *testing.T) {
	q, _ := openTestUplinkQueue(t, 0)
	// 入队序：低 a、普通 b、高 c、普通 d；外加越界优先级 clamp 检查。
	for _, tc := range []struct {
		priority int
		id       string
	}{
		{UplinkPriorityLow, "a"},
		{UplinkPriorityNormal, "b"},
		{UplinkPriorityHigh, "c"},
		{99, "d"}, // clamp 到高
	} {
		if _, err := q.EnqueueUplink(tc.priority, testUplinkMsg(t, tc.id)); err != nil {
			t.Fatalf("入队 %s 失败: %v", tc.id, err)
		}
	}
	items, err := q.DequeueUplinkBatch(10)
	if err != nil {
		t.Fatalf("出队失败: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("出队条数 = %d，期望 4", len(items))
	}
	// 高优先：c（id 小）先于 d（同级 FIFO）；然后普通 b；最后低 a。
	want := []string{"c", "d", "b", "a"}
	for i, it := range items {
		if it.Msg.ID != want[i] {
			t.Errorf("出队[%d] = %s，期望 %s", i, it.Msg.ID, want[i])
		}
	}
	if items[1].Priority != UplinkPriorityHigh {
		t.Errorf("clamp 后优先级 = %d，期望 %d", items[1].Priority, UplinkPriorityHigh)
	}
}

// TestUplinkQueueAckAndSentCount 验证 Ack 删除与 sent 计数（幂等）。
func TestUplinkQueueAckAndSentCount(t *testing.T) {
	q, _ := openTestUplinkQueue(t, 0)
	id1, _ := q.EnqueueUplink(UplinkPriorityNormal, testUplinkMsg(t, "m1"))
	id2, _ := q.EnqueueUplink(UplinkPriorityNormal, testUplinkMsg(t, "m2"))
	items, _ := q.DequeueUplinkBatch(10)
	if len(items) != 2 {
		t.Fatalf("出队条数 = %d，期望 2", len(items))
	}
	if err := q.AckUplink(id1); err != nil {
		t.Fatalf("Ack 失败: %v", err)
	}
	st, _ := q.UplinkDepth()
	if st.Total != 1 || st.Sent != 1 {
		t.Errorf("Ack 后 Total=%d Sent=%d，期望 1/1", st.Total, st.Sent)
	}
	// 重复 Ack（行不存在）幂等：计数不变。
	if err := q.AckUplink(id1); err != nil {
		t.Fatalf("重复 Ack 应静默成功: %v", err)
	}
	st2, _ := q.UplinkDepth()
	if st2.Sent != 1 {
		t.Errorf("重复 Ack 后 Sent=%d，期望仍为 1", st2.Sent)
	}
	_ = id2
}

// TestUplinkQueueCapacityDrop 验证超限丢弃序（低优先级最老先丢）与计数。
func TestUplinkQueueCapacityDrop(t *testing.T) {
	q, _ := openTestUplinkQueue(t, 3)
	// 入 5 条：low-a、low-b、normal-c、high-d、normal-e
	for _, tc := range []struct {
		priority int
		id       string
	}{
		{UplinkPriorityLow, "low-a"},
		{UplinkPriorityLow, "low-b"},
		{UplinkPriorityNormal, "normal-c"},
		{UplinkPriorityHigh, "high-d"},
		{UplinkPriorityNormal, "normal-e"},
	} {
		if _, err := q.EnqueueUplink(tc.priority, testUplinkMsg(t, tc.id)); err != nil {
			t.Fatalf("入队 %s 失败: %v", tc.id, err)
		}
	}
	st, _ := q.UplinkDepth()
	if st.Total != 3 || st.Dropped != 2 {
		t.Fatalf("Total=%d Dropped=%d，期望 3/2", st.Total, st.Dropped)
	}
	items, _ := q.DequeueUplinkBatch(10)
	got := make([]string, 0, len(items))
	for _, it := range items {
		got = append(got, it.Msg.ID)
	}
	// 丢弃序：low-a、low-b 被丢；剩余 normal-c、high-d、normal-e
	want := "high-d,normal-c,normal-e"
	if joined := strings.Join(got, ","); joined != want {
		t.Errorf("剩余队列 = %s，期望 %s", joined, want)
	}
}

// TestUplinkQueueRejectsOversize 验证超大消息拒绝。
func TestUplinkQueueRejectsOversize(t *testing.T) {
	q, _ := openTestUplinkQueue(t, 0)
	big := strings.Repeat("x", MaxUplinkMsgBytes+1024)
	m := testUplinkMsg(t, "big")
	m.Payload = []byte(fmt.Sprintf("%q", big)) // 展开到 Payload，整体 JSON 超 64KB
	if _, err := q.EnqueueUplink(UplinkPriorityNormal, m); err == nil {
		t.Fatal("超大消息应被拒绝")
	} else if !strings.Contains(err.Error(), "上限") {
		t.Errorf("报错信息应包含「上限」: %v", err)
	}
	st, _ := q.UplinkDepth()
	if st.Total != 0 {
		t.Errorf("拒绝后队列应为空，实际 %d", st.Total)
	}
}

// TestUplinkQueuePersistsAcrossReopen 验证重启（重开）后未 Ack 条目与计数保留。
func TestUplinkQueuePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	st1, err := Open(path)
	if err != nil {
		t.Fatalf("打开 Store 失败: %v", err)
	}
	q1, err := NewUplinkQueue(st1, 0)
	if err != nil {
		t.Fatalf("创建队列失败: %v", err)
	}
	id1, _ := q1.EnqueueUplink(UplinkPriorityNormal, testUplinkMsg(t, "keep-1"))
	id2, _ := q1.EnqueueUplink(UplinkPriorityNormal, testUplinkMsg(t, "keep-2"))
	if err := q1.AckUplink(id1); err != nil {
		t.Fatalf("Ack 失败: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("关闭 Store 失败: %v", err)
	}

	// 重开同路径（模拟进程重启）。
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("重开 Store 失败: %v", err)
	}
	defer func() { _ = st2.Close() }()
	q2, err := NewUplinkQueue(st2, 0)
	if err != nil {
		t.Fatalf("重开队列失败: %v", err)
	}
	st, _ := q2.UplinkDepth()
	if st.Total != 1 || st.Sent != 1 {
		t.Fatalf("重开后 Total=%d Sent=%d，期望 1/1", st.Total, st.Sent)
	}
	items, _ := q2.DequeueUplinkBatch(10)
	if len(items) != 1 || items[0].ID != id2 || items[0].Msg.ID != "keep-2" {
		t.Fatalf("重开后应剩余 keep-2（id=%d），实际 %+v", id2, items)
	}
}

// TestUplinkQueueBadRowSelfHeal 验证坏行跳过并清除（不卡队列）。
func TestUplinkQueueBadRowSelfHeal(t *testing.T) {
	q, st := openTestUplinkQueue(t, 0)
	if _, err := q.EnqueueUplink(UplinkPriorityLow, testUplinkMsg(t, "good")); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// 手工插入坏 JSON 行（优先级高，确保排头被首先遇到）。
	if _, err := st.db.Exec(
		`INSERT INTO uplink_queue(priority, msg, created_at) VALUES(?, ?, ?)`,
		UplinkPriorityHigh, "{not-json", 1); err != nil {
		t.Fatalf("插入坏行失败: %v", err)
	}
	items, err := q.DequeueUplinkBatch(10)
	if err != nil {
		t.Fatalf("出队失败: %v", err)
	}
	if len(items) != 1 || items[0].Msg.ID != "good" {
		t.Fatalf("应只返回 good，实际 %+v", items)
	}
	// 坏行应已被清除。
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM uplink_queue`).Scan(&count); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 1 {
		t.Errorf("坏行应被清除（剩余应为 1 条 good），实际 %d", count)
	}
}
