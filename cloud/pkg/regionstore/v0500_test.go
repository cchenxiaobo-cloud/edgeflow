package regionstore

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestV0500RegionCreateAndTree US-1：根/子创建 + 树形视图。
func TestV0500RegionCreateAndTree(t *testing.T) {
	s := New("")
	root, err := s.Create("华东", "", "大区")
	if err != nil || root.ParentID != "" || root.ID == "" {
		t.Fatalf("创建根失败: %+v err=%v", root, err)
	}
	child, err := s.Create("车间A", root.ID, "")
	if err != nil || child.ParentID != root.ID {
		t.Fatalf("创建子失败: %+v err=%v", child, err)
	}
	if _, err := s.Create("孤儿", "rg-nothing", ""); !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("父不存在应拒绝: %v", err)
	}
	if s.Count() != 2 {
		t.Fatalf("区域数不符: %d", s.Count())
	}
	tree := s.Tree()
	if len(tree) != 1 || tree[0].ID != root.ID || len(tree[0].Children) != 1 || tree[0].Children[0].ID != child.ID {
		t.Fatalf("树形不符: %+v", tree)
	}
	if tree[0].NodeCount != 0 || tree[0].Children[0].NodeCount != 0 {
		t.Fatalf("nodeCount 初始应为 0")
	}
}

// TestV0500RegionUpdateMove US-1：改名/描述/移动。
func TestV0500RegionUpdateMove(t *testing.T) {
	s := New("")
	a, _ := s.Create("A", "", "")
	b, _ := s.Create("B", "", "")
	// 改名 + 改描述
	newName, newDesc := "A2", "备注"
	if _, err := s.Update(a.ID, UpdateOpts{Name: &newName, Description: &newDesc}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	got, _ := s.RegionOfNode("__none__") // 只为编译面；真实断言走 Tree
	_ = got
	tree := s.Tree()
	var a2 *RegionNode
	for i := range tree {
		if tree[i].ID == a.ID {
			a2 = &tree[i]
		}
	}
	if a2 == nil || a2.Name != "A2" || a2.Description != "备注" {
		t.Fatalf("更新未生效: %+v", tree)
	}
	// 移动 B → A 下
	if _, err := s.Update(b.ID, UpdateOpts{ParentID: &a.ID}); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	tree = s.Tree()
	if len(tree) != 1 || tree[0].ID != a.ID || len(tree[0].Children) != 1 {
		t.Fatalf("移动后树形不符: %+v", tree)
	}
	// 移到根
	rootID := ""
	if _, err := s.Update(b.ID, UpdateOpts{ParentID: &rootID}); err != nil {
		t.Fatalf("移到根失败: %v", err)
	}
	if n := len(s.Tree()); n != 2 {
		t.Fatalf("移到根后应两棵树: %d", n)
	}
	// 父不存在
	missing := "rg-nothing"
	if _, err := s.Update(b.ID, UpdateOpts{ParentID: &missing}); !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("父不存在应拒绝: %v", err)
	}
}

// TestV0500RegionCycleRejected 裁决 2：移动防环（含移入自身子树）。
func TestV0500RegionCycleRejected(t *testing.T) {
	s := New("")
	a, _ := s.Create("A", "", "")
	b, _ := s.Create("B", a.ID, "")
	c, _ := s.Create("C", b.ID, "")
	// A 移到 C 下（C 在 A 子树内）→ 环
	if _, err := s.Update(a.ID, UpdateOpts{ParentID: &c.ID}); !errors.Is(err, ErrCycle) {
		t.Fatalf("成环应拒绝: %v", err)
	}
	// B 移到自身 → 环
	if _, err := s.Update(b.ID, UpdateOpts{ParentID: &b.ID}); !errors.Is(err, ErrCycle) {
		t.Fatalf("自环应拒绝: %v", err)
	}
	// 合法移动不受影响：C 移到根
	rootID := ""
	if _, err := s.Update(c.ID, UpdateOpts{ParentID: &rootID}); err != nil {
		t.Fatalf("合法移动失败: %v", err)
	}
}

// TestV0500RegionDeleteConstraints 裁决 3：删除约束（有子拒/有节点拒/空区可删）。
func TestV0500RegionDeleteConstraints(t *testing.T) {
	s := New("")
	a, _ := s.Create("A", "", "")
	b, _ := s.Create("B", a.ID, "")
	// 有子
	if err := s.Delete(a.ID); !errors.Is(err, ErrHasChildren) {
		t.Fatalf("有子应拒绝: %v", err)
	}
	// 有节点
	if _, _, err := s.AssignNodes(b.ID, []string{"node-1"}); err != nil {
		t.Fatalf("归属失败: %v", err)
	}
	if err := s.Delete(b.ID); !errors.Is(err, ErrHasNodes) {
		t.Fatalf("有节点应拒绝: %v", err)
	}
	// 迁出后可删
	if _, _, err := s.AssignNodes(a.ID, []string{"node-1"}); err != nil {
		t.Fatalf("换区失败: %v", err)
	}
	if err := s.Delete(b.ID); err != nil {
		t.Fatalf("空区删除失败: %v", err)
	}
	// 不存在
	if err := s.Delete("rg-nothing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在应 404 语义: %v", err)
	}
}

// TestV0500RegionAssign US-2：批量归属/幂等换区/空 ID 跳过。
func TestV0500RegionAssign(t *testing.T) {
	s := New("")
	a, _ := s.Create("A", "", "")
	b, _ := s.Create("B", "", "")
	assigned, skipped, err := s.AssignNodes(a.ID, []string{"n1", "", "n2"})
	if err != nil || assigned != 2 || skipped != 1 {
		t.Fatalf("批量归属不符: assigned=%d skipped=%d err=%v", assigned, skipped, err)
	}
	// 幂等换区
	assigned, _, err = s.AssignNodes(b.ID, []string{"n1"})
	if err != nil || assigned != 1 {
		t.Fatalf("换区失败: %v", err)
	}
	if inA := s.NodesInRegion(a.ID); len(inA) != 1 || inA[0] != "n2" {
		t.Fatalf("旧区应只剩 n2: %v", inA)
	}
	if r, ok := s.RegionOfNode("n1"); !ok || r.ID != b.ID {
		t.Fatalf("n1 归属不符: %+v ok=%v", r, ok)
	}
	// 超限
	big := make([]string, MaxAssignBatch+1)
	if _, _, err := s.AssignNodes(a.ID, big); err == nil {
		t.Fatal("超限应拒绝")
	}
	// 区域不存在
	if _, _, err := s.AssignNodes("rg-nothing", []string{"n1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("区域不存在应拒绝: %v", err)
	}
}

// TestV0500RegionPersistRoundtrip US-6：快照写读重建（含孤儿归属清理）。
func TestV0500RegionPersistRoundtrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regions.json")
	s := New(file)
	a, _ := s.Create("A", "", "")
	b, _ := s.Create("B", a.ID, "")
	if _, _, err := s.AssignNodes(b.ID, []string{"n1", "n2"}); err != nil {
		t.Fatalf("归属失败: %v", err)
	}
	s2 := New(file)
	if s2.Count() != 2 {
		t.Fatalf("重建区域数不符: %d", s2.Count())
	}
	if r, ok := s2.RegionOfNode("n1"); !ok || r.ID != b.ID {
		t.Fatalf("重建归属不符: %+v", r)
	}
	tree := s2.Tree()
	if len(tree) != 1 || len(tree[0].Children) != 1 || tree[0].Children[0].NodeCount != 2 {
		t.Fatalf("重建树形不符: %+v", tree)
	}
	// 手工构造孤儿归属（regionID 不在库）→ Load 时清理
	raw := `{"regions":[{"id":"rg-x","name":"X","parentId":"","createdAt":1,"updatedAt":1}],"assignments":{"ghost":"rg-gone"}}`
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s3 := New(file)
	if _, ok := s3.RegionOfNode("ghost"); ok {
		t.Fatal("孤儿归属应被清理")
	}
}

// TestV0500RegionValidation 校验面（空名/超长名/超长描述）。
func TestV0500RegionValidation(t *testing.T) {
	s := New("")
	if _, err := s.Create("", "", ""); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("空名应拒绝: %v", err)
	}
	longName := make([]byte, MaxNameBytes+1)
	if _, err := s.Create(string(longName), "", ""); !errors.Is(err, ErrNameTooLong) {
		t.Fatalf("超长名应拒绝: %v", err)
	}
	longDesc := make([]byte, MaxDescBytes+1)
	if _, err := s.Create("ok", "", string(longDesc)); !errors.Is(err, ErrDescTooLong) {
		t.Fatalf("超长描述应拒绝: %v", err)
	}
	// 边界内通过
	if _, err := s.Create(string(make([]byte, MaxNameBytes)), "", string(make([]byte, MaxDescBytes))); err != nil {
		t.Fatalf("边界内应通过: %v", err)
	}
}

// TestV0500RegionConcurrent 并发读写（-race 面）。
func TestV0500RegionConcurrent(t *testing.T) {
	s := New("")
	a, _ := s.Create("A", "", "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _ = s.AssignNodes(a.ID, []string{"n" + string(rune('a'+i))})
			_ = s.Tree()
			_, _ = s.RegionOfNode("na")
		}(i)
	}
	wg.Wait()
	if got := len(s.NodesInRegion(a.ID)); got != 8 {
		t.Fatalf("并发归属应全部生效: %d", got)
	}
}

// v0.51.0 spec 0024 裁决 4：PUT null/{ } 清除语义——JSON null 必须放行（e2e
// run3 抓到类型守卫误拒 null 的缺陷后回归锚）。
func TestV0510PolicyNullClear(t *testing.T) {
	s := New("")
	r, err := s.Create("A", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPolicy(r.ID, []byte(`{"k":"v"}`), 1); err != nil {
		t.Fatal(err)
	}
	got, _ := s.RegionByID(r.ID)
	if len(got.Policy) == 0 {
		t.Fatal("策略应已设置")
	}
	// JSON null 清除
	if err := s.SetPolicy(r.ID, []byte(`null`), 2); err != nil {
		t.Fatalf("null 清除被误拒: %v", err)
	}
	got, _ = s.RegionByID(r.ID)
	if len(got.Policy) != 0 {
		t.Fatalf("null 清除后应空: %s", got.Policy)
	}
	// {} 清除
	if err := s.SetPolicy(r.ID, []byte(`{"k":"v"}`), 3); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPolicy(r.ID, []byte(`{}`), 4); err != nil {
		t.Fatalf("{} 清除被误拒: %v", err)
	}
	got, _ = s.RegionByID(r.ID)
	if len(got.Policy) != 0 {
		t.Fatalf("{} 清除后应空: %s", got.Policy)
	}
	// 非对象仍拒绝
	if err := s.SetPolicy(r.ID, []byte(`[1]`), 5); err != ErrPolicyInvalid {
		t.Fatalf("数组应 ErrPolicyInvalid: %v", err)
	}
}
