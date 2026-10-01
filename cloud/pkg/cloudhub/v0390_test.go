// v0390_test.go：RuleEvent 接收幂等单测（spec 0012 US-4）。
package cloudhub

import (
	"sync"
	"testing"
)

// TestRuleEventDedupCheckAndCount 验证首次/重复/空 ID 的判定与计数。
func TestRuleEventDedupCheckAndCount(t *testing.T) {
	d := newRuleEventDedup(0)
	if d.checkAndCount("n1", "id-1") {
		t.Fatal("首见应非重复")
	}
	if !d.checkAndCount("n1", "id-1") {
		t.Fatal("二次应判重复")
	}
	// 空 ID：不去重（放行 + 计数）
	if d.checkAndCount("n1", "") {
		t.Fatal("空 ID 应放行（不去重）")
	}
	if d.checkAndCount("n1", "") {
		t.Fatal("空 ID 二次仍放行")
	}
	c := d.perNode["n1"]
	if c == nil || c.Received != 3 || c.Duplicated != 1 {
		t.Fatalf("计数不符: %+v", c)
	}
}

// TestRuleEventDedupEviction 验证滚动窗口淘汰（超窗旧 ID 重新接收）。
func TestRuleEventDedupEviction(t *testing.T) {
	d := newRuleEventDedup(2)
	d.checkAndCount("n", "a")
	d.checkAndCount("n", "b")
	d.checkAndCount("n", "c") // 触发淘汰 a → 窗口 [b,c]
	if d.checkAndCount("n", "a") {
		t.Fatal("超窗旧 ID 应重新接收（淘汰语义）")
	}
	// 重收 a 后窗口 [c,a]：c 判重、b 已被挤出。
	if !d.checkAndCount("n", "c") {
		t.Fatal("窗口内 ID 应判重")
	}
	if d.checkAndCount("n", "b") {
		t.Fatal("被挤出条目应重新接收")
	}
}

// TestServerRuleEventStatsZeroValue 验证零值 Server 的懒初始化与统计快照。
func TestServerRuleEventStatsZeroValue(t *testing.T) {
	// 未登记时 stats 为空（不 panic）
	if st := (&Server{}).RuleEventStats(); len(st) != 0 {
		t.Fatalf("未登记时 stats 应为空: %+v", st)
	}
	s := &Server{}
	s.ruleEventCheckDup("n1", "x")
	s.ruleEventCheckDup("n1", "x")
	s.ruleEventCheckDup("n2", "y")
	stats := s.RuleEventStats()
	if c := stats["n1"]; c.Received != 1 || c.Duplicated != 1 {
		t.Fatalf("n1 计数不符: %+v", c)
	}
	if c := stats["n2"]; c.Received != 1 || c.Duplicated != 0 {
		t.Fatalf("n2 计数不符: %+v", c)
	}
}

// TestRuleEventDedupConcurrent 并发判定正确性（-race 下跑）。
func TestRuleEventDedupConcurrent(t *testing.T) {
	d := newRuleEventDedup(100)
	var wg sync.WaitGroup
	const workers = 16
	const rounds = 100
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				d.checkAndCount("node", "same-id")
			}
		}()
	}
	wg.Wait()
	c := d.perNode["node"]
	if c.Received != 1 || c.Duplicated != workers*rounds-1 {
		t.Fatalf("并发计数不符: %+v", c)
	}
}
