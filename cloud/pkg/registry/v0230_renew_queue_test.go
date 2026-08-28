// CHN-19 观测锚点测试：续约队列水位与丢弃计数（lease_registry.go）。
// 覆盖：RenewQueueDepth 水位反映队列待消费数、RenewQueueDropped 在队列满
// 入队丢弃时自增（既有背压语义不变——丢弃 + 降频 Warn，仅补观测）。
// 既有行为测试见 lease_registry_test.go / v0220_ghost_node_test.go（不改一行）。
package registry

import (
	"testing"
)

// TestRenewQueueObservabilityAccessors 队列未满时正常入队（丢弃计数为 0）；
// 塞满 renewQueueCapacity 后再入队被丢弃且计数自增；depth 为瞬时快照。
func TestRenewQueueObservabilityAccessors(t *testing.T) {
	f := newFakeExtKV()
	r, err := newLeaseReg(f)
	if err != nil {
		t.Fatalf("NewLeaseEtcdRegistry: %v", err)
	}
	defer r.Close()

	// 初始：空队列，零丢弃。
	if got := r.RenewQueueDepth(); got != 0 {
		t.Fatalf("初始 RenewQueueDepth = %d，期望 0", got)
	}
	if got := r.RenewQueueDropped(); got != 0 {
		t.Fatalf("初始 RenewQueueDropped = %d，期望 0", got)
	}

	// 未塞满：正常入队（普通 + 修复性两类请求混合），丢弃计数不动。
	for i := 0; i < 8; i++ {
		r.enqueueRenew("n-a")
	}
	r.enqueueRepairRenew("n-b")
	if got := r.RenewQueueDepth(); got != 9 {
		t.Fatalf("入队 9 条后 RenewQueueDepth = %d，期望 9", got)
	}
	if got := r.RenewQueueDropped(); got != 0 {
		t.Fatalf("未满队列 RenewQueueDropped = %d，期望 0", got)
	}

	// 塞满至容量（4096）：其余 4096-9 条成功入队。
	for i := 0; i < renewQueueCapacity-9; i++ {
		r.enqueueRenew("n-fill")
	}
	if got := r.RenewQueueDepth(); got != renewQueueCapacity {
		t.Fatalf("塞满后 RenewQueueDepth = %d，期望 %d", got, renewQueueCapacity)
	}
	if got := r.RenewQueueDropped(); got != 0 {
		t.Fatalf("恰好塞满（未溢出）RenewQueueDropped = %d，期望 0", got)
	}

	// 溢出 3 条：全部被丢弃（非阻塞背压），丢弃计数 = 3，水位不变。
	for i := 0; i < 3; i++ {
		r.enqueueRenew("n-overflow")
	}
	if got := r.RenewQueueDepth(); got != renewQueueCapacity {
		t.Fatalf("溢出后 RenewQueueDepth = %d，期望仍为 %d（丢弃不入队）", got, renewQueueCapacity)
	}
	if got := r.RenewQueueDropped(); got != 3 {
		t.Fatalf("溢出 3 条后 RenewQueueDropped = %d，期望 3", got)
	}

	// depth 是瞬时快照：消费一条后水位减一，丢弃计数不受影响。
	drained := <-r.renewCh
	if got := r.RenewQueueDepth(); got != renewQueueCapacity-1 {
		t.Fatalf("消费一条后 RenewQueueDepth = %d，期望 %d", got, renewQueueCapacity-1)
	}
	if got := r.RenewQueueDropped(); got != 3 {
		t.Fatalf("消费后 RenewQueueDropped = %d，期望仍为 3", got)
	}
	_ = drained // 混合入队仅验证水位/丢弃语义，不逐条断言内容
}
