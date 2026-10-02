// v0430_uplink_ids_test.go：PendingUplinkIDs 单测（v0.43.0，spec 0016 US-1
// 媒资 spool Janitor 完成判定依赖本方法）。
package metamanager

import "testing"

// TestPendingUplinkIDs 覆盖：命中子集返回/全部离队返回空/空输入 nil。
func TestPendingUplinkIDs(t *testing.T) {
	q, _ := openTestUplinkQueue(t, 0)
	id1, err := q.EnqueueUplink(UplinkPriorityNormal, testUplinkMsg(t, "id-1"))
	if err != nil {
		t.Fatalf("入队 1 失败: %v", err)
	}
	id2, err := q.EnqueueUplink(UplinkPriorityNormal, testUplinkMsg(t, "id-2"))
	if err != nil {
		t.Fatalf("入队 2 失败: %v", err)
	}
	id3, err := q.EnqueueUplink(UplinkPriorityNormal, testUplinkMsg(t, "id-3"))
	if err != nil {
		t.Fatalf("入队 3 失败: %v", err)
	}

	// 全在队：返回全部。
	got, err := q.PendingUplinkIDs([]int64{id1, id2, id3})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应返回 3 条在队，实际 %d", len(got))
	}

	// Ack 一条：返回剩余两条（不含已 Ack）。
	if err := q.AckUplink(id2); err != nil {
		t.Fatalf("Ack 失败: %v", err)
	}
	got, err = q.PendingUplinkIDs([]int64{id1, id2, id3})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 条在队，实际 %d", len(got))
	}
	for _, id := range got {
		if id == id2 {
			t.Fatalf("已 Ack 的 %d 不应在待发集", id2)
		}
	}

	// 全部离队：空集（Janitor 完成判定路径）。
	if err := q.AckUplink(id1); err != nil {
		t.Fatalf("Ack 失败: %v", err)
	}
	if err := q.AckUplink(id3); err != nil {
		t.Fatalf("Ack 失败: %v", err)
	}
	got, err = q.PendingUplinkIDs([]int64{id1, id2, id3})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("全部离队应返回空，实际 %d", len(got))
	}

	// 空输入：nil（无查询）。
	got, err = q.PendingUplinkIDs(nil)
	if err != nil || got != nil {
		t.Fatalf("空输入应 nil, nil: %v %v", got, err)
	}
}
