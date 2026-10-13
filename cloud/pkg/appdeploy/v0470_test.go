package appdeploy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeDeployer 记录交付/卸载调用（可注入失败集；"*" = 全失败）。
// 并发安全（Cancel 的卸载在后台 goroutine 执行——Deployer 契约要求并发安全）。
type fakeDeployer struct {
	mu      sync.Mutex
	deliver []string
	removed []string
	failSet map[string]error
}

func (f *fakeDeployer) snapshot() (deliver, removed []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deliver...), append([]string(nil), f.removed...)
}

func TestV0470PlanStateMachine(t *testing.T) {
	dep := &fakeDeployer{failSet: map[string]error{}}
	now := time.Now
	c := NewController(dep, now)

	p, err := c.Create(&Plan{App: "my-app", Version: "v1.0.0", Kind: "container", BatchSize: 1},
		[]string{"n1", "n2", "n3"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.State != StatePending || p.ID == "" {
		t.Fatalf("初始态 = %s id=%s", p.State, p.ID)
	}
	// 去重：重复节点只算一次
	p2, _ := c.Create(&Plan{App: "a", Version: "v", Kind: "container", BatchSize: 1},
		[]string{"n1", "n1", "n2"})
	if len(p2.Nodes) != 2 {
		t.Fatalf("去重失败: %d", len(p2.Nodes))
	}
	// 非法转换
	if err := c.Pause(p.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending→pause 应冲突: %v", err)
	}
	// running → 逐批推进
	if err := c.Resume(p.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	c.Tick(context.Background())
	got, _ := c.Get(p.ID)
	if got.Nodes[0].State != "delivered" {
		t.Fatalf("批1 未交付: %+v", got.Nodes[0])
	}
	if got.Nodes[1].State != "pending" {
		t.Fatalf("批2 不应提前: %+v", got.Nodes[1])
	}
	// pause → resume
	if err := c.Pause(p.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	c.Tick(context.Background())
	got, _ = c.Get(p.ID)
	if got.Nodes[1].State != "pending" {
		t.Fatal("paused 不应推进")
	}
	_ = c.Resume(p.ID)
	for i := 0; i < 5; i++ {
		c.Tick(context.Background())
		got, _ = c.Get(p.ID)
		if got.State == StateSucceeded {
			break
		}
	}
	got, _ = c.Get(p.ID)
	if got.State != StateSucceeded {
		t.Fatalf("终态 = %s nodes=%+v", got.State, got.Nodes)
	}
	// 终态拒绝控制
	if err := c.Pause(p.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("终态应拒绝 pause")
	}
	// 全失败 → failed
	dep2 := &fakeDeployer{failSet: map[string]error{"*": errors.New("boom")}}
	c2 := NewController(dep2, now)
	p3, _ := c2.Create(&Plan{App: "a", Version: "v", Kind: "container", BatchSize: 1}, []string{"n1"})
	_ = c2.Resume(p3.ID)
	c2.Tick(context.Background())
	got3, _ := c2.Get(p3.ID)
	if got3.State != StateFailed || got3.Nodes[0].Reason == "" {
		t.Fatalf("全失败终态 = %s reason=%q", got3.State, got3.Nodes[0].Reason)
	}
}

func TestV0470Rollback(t *testing.T) {
	dep := &fakeDeployer{failSet: map[string]error{}}
	c := NewController(dep, time.Now)
	p, _ := c.Create(&Plan{App: "app", Version: "v2", Kind: "container", BatchSize: 2},
		[]string{"n1", "n2", "n3", "n4"})
	_ = c.Resume(p.ID)
	c.Tick(context.Background()) // 批1: n1,n2
	got, _ := c.Get(p.ID)
	if got.Nodes[0].State != "delivered" || got.Nodes[1].State != "delivered" {
		t.Fatalf("批1 状态: %+v %+v", got.Nodes[0], got.Nodes[1])
	}
	// 回滚到 v1（逆序：批内逆序由实现保证；这里断言回滚计划覆盖已交付节点）
	rb, err := c.Rollback(p.ID, "v1", "reg.io/app:v1")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rb.RollbackOf != p.ID || rb.Version != "v1" || len(rb.Nodes) != 2 {
		t.Fatalf("回滚计划 = %+v", rb)
	}
	_ = c.Resume(rb.ID)
	for i := 0; i < 3; i++ {
		c.Tick(context.Background())
		if gotRB, _ := c.Get(rb.ID); gotRB.State == StateSucceeded {
			break
		}
	}
	gotRB, _ := c.Get(rb.ID)
	if gotRB.State != StateSucceeded {
		t.Fatalf("回滚未完成: %s", gotRB.State)
	}
	// 回滚 = 对原已交付节点再下发目标版本（PodSync update 覆盖语义）
	delivered := map[string]bool{}
	for _, n := range gotRB.Nodes {
		if n.State == "delivered" {
			delivered[n.NodeID] = true
		}
	}
	if len(delivered) != 2 || !delivered["n1"] || !delivered["n2"] {
		t.Fatalf("回滚交付集 = %v", delivered)
	}
}

func TestV0470CancelUninstalls(t *testing.T) {
	dep := &fakeDeployer{failSet: map[string]error{}}
	c := NewController(dep, time.Now)
	p, _ := c.Create(&Plan{App: "app", Version: "v1", Kind: "container", BatchSize: 1},
		[]string{"n1", "n2"})
	_ = c.Resume(p.ID)
	c.Tick(context.Background())
	if err := c.Cancel(p.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got, _ := c.Get(p.ID)
	if got.State != StateCancelled {
		t.Fatalf("取消终态 = %s", got.State)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, removed := dep.snapshot(); len(removed) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, removed := dep.snapshot(); len(removed) == 0 {
		t.Fatal("取消应触发已交付节点卸载")
	}
}

// --- fake ---

func (f *fakeDeployer) Deliver(_ context.Context, plan *Plan, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failSet["*"]; ok {
		return err
	}
	if err, ok := f.failSet[nodeID]; ok {
		return err
	}
	f.deliver = append(f.deliver, nodeID)
	return nil
}

func (f *fakeDeployer) Remove(_ context.Context, plan *Plan, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, nodeID)
	return nil
}

var _ Deployer = (*fakeDeployer)(nil)
var _ = fmt.Sprintf
