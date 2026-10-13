// Package appdeploy 实现云端应用级灰度部署（v0.47.0，spec 0020 US-2，发展规划 G09）。
//
// 设计要点（语义对齐 modelrelease 灰度状态机，实现独立零耦合）：
//   - 状态机：pending → running → (paused ⇄ running) → succeeded/failed；
//     running/paused → cancelled；终态不可再转换（断言拦截 + 回滚放弃写点）；
//   - 批次推进：nodes 按 batchSize 切批，批间 pauseBetweenSec（0 = 不停顿）；
//     批内逐节点 PodSync 下发（operation=add/update + pod 规格）；失败节点
//     记 perNode.Reason 并继续（批次内失败不阻塞后续批次——尽力交付语义，
//     汇总 succeeded/failed 计数；全失败 → 计划 failed）；
//   - 回滚：按批次逆序把已交付节点恢复到目标版本 Pod 规格（复用同一执行器）；
//   - 控制器：goroutine 扫描 running 计划 → 推进（测试可注入 Now/执行器）；
//   - 零第三方依赖；PodSync 下发函数由装配层注入（依赖倒置）。
package appdeploy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"edgeflow/pkg/log"
)

// 部署计划状态（低基数白名单）。
const (
	StatePending   = "pending"
	StateRunning   = "running"
	StatePaused    = "paused"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Terminal 报告状态是否终态（终态不可转换）。
func Terminal(s string) bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

// PerNode 是单节点交付记录。
type PerNode struct {
	NodeID string `json:"nodeID"`
	Batch  int    `json:"batch"`
	State  string `json:"state"` // pending|delivered|failed
	Reason string `json:"reason,omitempty"`
	At     int64  `json:"at,omitempty"` // Unix 毫秒（交付时刻）
}

// Plan 是一个应用部署计划。
type Plan struct {
	ID              string    `json:"id"`
	App             string    `json:"app"`
	Version         string    `json:"version"`
	Kind            string    `json:"kind"` // container|script（执行器分派依据）
	Image           string    `json:"image,omitempty"`
	ScriptRef       string    `json:"scriptRef,omitempty"`
	ScriptSHA       string    `json:"scriptSha,omitempty"`
	BatchSize       int       `json:"batchSize"`
	PauseBetweenSec int       `json:"pauseBetweenSec"`
	State           string    `json:"state"`
	CreatedAt       int64     `json:"createdAt"`
	UpdatedAt       int64     `json:"updatedAt"`
	Nodes           []PerNode `json:"nodes"`
	// RollbackOf 非空 = 本计划是一次回滚（引用原计划 ID）。
	RollbackOf string `json:"rollbackOf,omitempty"`
}

// Deployer 是节点交付执行器抽象（装配层注入——cloudcore 侧实现为 PodSync/
// ConfigSync 组合；测试注入 fake）。
type Deployer interface {
	// Deliver 把应用版本交付到单节点（同步；错误 → perNode failed）。
	Deliver(ctx context.Context, plan *Plan, nodeID string) error
	// Remove 从单节点移除应用（cancel/rollback 卸载路径；container = PodSync
	// delete；script = 生命周期 uninstall）。
	Remove(ctx context.Context, plan *Plan, nodeID string) error
}

// Controller 是灰度部署控制器（内存计划表 + 可选文件持久化由 API 层装配）。
type Controller struct {
	mu    sync.Mutex
	plans map[string]*Plan
	order []string // 创建序（列表用）
	dep   Deployer
	now   func() time.Time
	seq   int64 // planID 序号
}

// NewController 构造（dep 必填——nil fail-fast 由调用方保证；此处防御缺省）。
func NewController(dep Deployer, now func() time.Time) *Controller {
	if now == nil {
		now = time.Now
	}
	return &Controller{plans: map[string]*Plan{}, dep: dep, now: now}
}

// Create 创建部署计划（nodeIDs 去重保序；batchSize ≤0 → 1；节点非空）。
func (c *Controller) Create(plan *Plan, nodeIDs []string) (*Plan, error) {
	if plan == nil || plan.App == "" || plan.Version == "" {
		return nil, errors.New("计划缺 app/version")
	}
	if len(nodeIDs) == 0 {
		return nil, errors.New("计划节点列表为空")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	plan.ID = fmt.Sprintf("appdep-%d-%04d", c.now().UnixMilli(), c.seq)
	plan.State = StatePending
	plan.CreatedAt = c.now().UnixMilli()
	plan.UpdatedAt = plan.CreatedAt
	if plan.BatchSize <= 0 {
		plan.BatchSize = 1
	}
	// 节点去重保序 + 初始化 perNode
	seen := map[string]bool{}
	nodes := make([]PerNode, 0, len(nodeIDs))
	for i, n := range nodeIDs {
		if seen[n] {
			continue
		}
		seen[n] = true
		nodes = append(nodes, PerNode{NodeID: n, Batch: i / plan.BatchSize, State: "pending"})
	}
	plan.Nodes = nodes
	c.plans[plan.ID] = plan
	c.order = append(c.order, plan.ID)
	return plan, nil
}

// Get 取计划（副本）。
func (c *Controller) Get(id string) (*Plan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.plans[id]
	if !ok {
		return nil, false
	}
	cp := *p
	cp.Nodes = append([]PerNode(nil), p.Nodes...)
	return &cp, true
}

// List 返回全部计划（创建时间倒序；副本）。
func (c *Controller) List() []*Plan {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Plan, 0, len(c.order))
	for i := len(c.order) - 1; i >= 0; i-- {
		if p, ok := c.plans[c.order[i]]; ok {
			cp := *p
			cp.Nodes = append([]PerNode(nil), p.Nodes...)
			out = append(out, &cp)
		}
	}
	return out
}

// setState 转换状态（终态断言；非法转换返回错误——调用方转 409）。
func (c *Controller) setState(id, to string) error {
	p, ok := c.plans[id]
	if !ok {
		return ErrNotFound
	}
	if Terminal(p.State) {
		return fmt.Errorf("计划已终态（%s），拒绝转换 %s → %s", p.State, p.State, to)
	}
	switch {
	case to == StateRunning && p.State != StatePending && p.State != StatePaused:
		return fmt.Errorf("非法转换 %s → %s", p.State, to)
	case to == StatePaused && p.State != StateRunning:
		return fmt.Errorf("非法转换 %s → %s", p.State, to)
	case to == StateCancelled && p.State == StatePending:
		// pending 可直接取消
	case to == StateCancelled && p.State != StateRunning && p.State != StatePaused:
		return fmt.Errorf("非法转换 %s → %s", p.State, to)
	}
	p.State = to
	p.UpdatedAt = c.now().UnixMilli()
	return nil
}

// 状态控制错误（API 层映射 404/409）。
var ErrNotFound = errors.New("plan not found")
var ErrConflict = errors.New("state conflict")

// Pause / Resume / Cancel 状态控制。
func (c *Controller) Pause(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.setState(id, StatePaused); err != nil {
		return mapCtlErr(err)
	}
	return nil
}

func (c *Controller) Resume(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.setState(id, StateRunning); err != nil {
		return mapCtlErr(err)
	}
	return nil
}

func (c *Controller) Cancel(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.setState(id, StateCancelled); err != nil {
		return mapCtlErr(err)
	}
	// 已交付节点逆序卸载（尽力；失败记日志不影响取消终态）。
	// 持锁内构造快照副本再传 goroutine（v0.47 复核 P1-3：传活指针 + goroutine
	// 内无锁读构成数据竞争）。
	p := c.plans[id]
	snap := *p
	snap.Nodes = append([]PerNode(nil), p.Nodes...)
	go c.rollbackNodes(&snap, deliveredNodes(p))
	return nil
}

func mapCtlErr(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	return ErrConflict
}

// deliveredNodes 返回已交付节点 ID（按批次倒序）。
func deliveredNodes(p *Plan) []string {
	var out []string
	for i := len(p.Nodes) - 1; i >= 0; i-- {
		if p.Nodes[i].State == "delivered" {
			out = append(out, p.Nodes[i].NodeID)
		}
	}
	return out
}

// Rollback 创建回滚计划：把原计划已交付节点逆序恢复到目标版本。
// 目标版本规格由调用方查目录后传入（newPlan 携带目标 version/image）。
func (c *Controller) Rollback(origID string, targetVersion, targetImage string) (*Plan, error) {
	c.mu.Lock()
	orig, ok := c.plans[origID]
	if !ok {
		c.mu.Unlock()
		return nil, ErrNotFound
	}
	if Terminal(orig.State) && orig.State != StateSucceeded && orig.State != StateFailed {
		c.mu.Unlock()
		return nil, fmt.Errorf("计划态 %s 不可回滚", orig.State)
	}
	delivered := deliveredNodes(orig)
	if len(delivered) == 0 {
		c.mu.Unlock()
		return nil, errors.New("无已交付节点可回滚")
	}
	c.mu.Unlock()

	np := &Plan{
		App: orig.App, Version: targetVersion, Kind: orig.Kind,
		Image: targetImage, ScriptRef: orig.ScriptRef, ScriptSHA: orig.ScriptSHA,
		BatchSize:       len(delivered), // 回滚一批完成（逆序在执行器内保证）
		PauseBetweenSec: orig.PauseBetweenSec,
		RollbackOf:      origID,
	}
	created, err := c.Create(np, delivered)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	created.RollbackOf = origID
	c.mu.Unlock()
	return created, nil
}

// Tick 推进一轮（控制器 goroutine 周期调用；测试直接调用）：
// 拾起 pending 新计划（→ running）并对 running 计划各推进一步（一批）；
// 扫描周期即批间节奏（pauseBetweenSec 语义：批次完成后额外等待窗，
// 由 advance 内判定）。paused 不被拾起——恢复必须显式 Resume。
func (c *Controller) Tick(ctx context.Context) {
	c.mu.Lock()
	var ids []string
	for _, id := range c.order {
		if p := c.plans[id]; p.State == StateRunning || p.State == StatePending {
			ids = append(ids, id)
		}
	}
	c.mu.Unlock()
	for _, id := range ids {
		// 新计划拾起：pending → running（首个扫描周期自动开跑；
		// paused 不在扫描集——暂停必须显式 Resume）
		c.mu.Lock()
		if p, ok := c.plans[id]; ok && p.State == StatePending {
			if err := c.setState(id, StateRunning); err != nil {
				c.mu.Unlock()
				continue
			}
		}
		c.mu.Unlock()
		c.advance(ctx, id)
	}
}

// advance 推进单计划一批；返回是否实际推进（false = 无 pending/已终态）。
func (c *Controller) advance(ctx context.Context, id string) bool {
	c.mu.Lock()
	p, ok := c.plans[id]
	if !ok || p.State != StateRunning {
		c.mu.Unlock()
		return false
	}
	// 找下一批（最小 batch 号的 pending 节点）
	var nextBatch = -1
	for _, n := range p.Nodes {
		if n.State == "pending" && (nextBatch < 0 || n.Batch < nextBatch) {
			nextBatch = n.Batch
		}
	}
	if nextBatch < 0 {
		// 无 pending：按成败汇总终态（写点前复查终态——v0.47 复核 P1-3：
		// 直写 p.State 绕过 setState 终态断言，会覆盖 Cancel 已置的 cancelled）
		if Terminal(p.State) {
			c.mu.Unlock()
			return false
		}
		failed := 0
		for _, n := range p.Nodes {
			if n.State == "failed" {
				failed++
			}
		}
		if failed == len(p.Nodes) {
			p.State = StateFailed
		} else {
			p.State = StateSucceeded
		}
		p.UpdatedAt = c.now().UnixMilli()
		c.mu.Unlock()
		return false
	}
	// 收集本批节点 + 执行（持锁拷贝，解锁后交付——Deliver 可能慢）
	var batch []string
	for i := range p.Nodes {
		if p.Nodes[i].Batch == nextBatch && p.Nodes[i].State == "pending" {
			p.Nodes[i].State = "delivering"
			batch = append(batch, p.Nodes[i].NodeID)
		}
	}
	planCopy := *p
	planCopy.Nodes = append([]PerNode(nil), p.Nodes...)
	c.mu.Unlock()

	for _, nodeID := range batch {
		err := c.dep.Deliver(ctx, &planCopy, nodeID)
		c.mu.Lock()
		for i := range p.Nodes {
			if p.Nodes[i].NodeID == nodeID && p.Nodes[i].State == "delivering" {
				if err != nil {
					p.Nodes[i].State = "failed"
					p.Nodes[i].Reason = truncate(err.Error(), 200)
				} else {
					p.Nodes[i].State = "delivered"
				}
				p.Nodes[i].At = c.now().UnixMilli()
			}
		}
		// 批次收尾即做终态汇总（无 pending：全失败 → failed，否则 succeeded）。
		// 写点前复查终态（v0.47 复核 P1-3）：Deliver 在途时 Cancel 可已置
		// cancelled——不覆盖终态；本批已 delivered 节点不在 Cancel 快照中，
		// 补偿性直接卸载（否则应用残留节点永不清理）。
		if Terminal(p.State) {
			var leaked []string
			for i := range p.Nodes {
				if p.Nodes[i].State == "delivered" {
					leaked = append(leaked, p.Nodes[i].NodeID)
				}
			}
			if len(leaked) > 0 {
				snap := *p
				snap.Nodes = append([]PerNode(nil), p.Nodes...)
				log.Infof("[appdeploy] 计划 %s 已 %s（Cancel 竞态）——补偿卸载本批交付节点 %v", p.ID, p.State, leaked)
				go c.rollbackNodes(&snap, leaked)
			}
			p.UpdatedAt = c.now().UnixMilli()
			c.mu.Unlock()
			return true
		}
		pendingLeft := false
		for _, n := range p.Nodes {
			if n.State == "pending" || n.State == "delivering" {
				pendingLeft = true
				break
			}
		}
		if !pendingLeft {
			failed := 0
			for _, n := range p.Nodes {
				if n.State == "failed" {
					failed++
				}
			}
			if failed == len(p.Nodes) {
				p.State = StateFailed
			} else {
				p.State = StateSucceeded
			}
		}
		p.UpdatedAt = c.now().UnixMilli()
		c.mu.Unlock()
	}
	return true
}

// rollbackNodes 逆序卸载已交付节点（cancel/Cancel 竞态补偿路径；
// p 必须为快照副本——本函数在 goroutine 中无锁读）。
func (c *Controller) rollbackNodes(p *Plan, nodes []string) {
	if p == nil || len(nodes) == 0 {
		return
	}
	for _, n := range nodes {
		_ = c.dep.Remove(context.Background(), p, n)
	}
}

// Run 启动控制器循环（扫描周期由调用方传入；ctx 取消即退出）。
func (c *Controller) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Tick(ctx)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
