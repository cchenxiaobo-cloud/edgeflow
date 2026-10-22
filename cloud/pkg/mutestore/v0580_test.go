// v0580_test.go：mutestore 静默窗口单测（v0.58.0，spec 0031）。
package mutestore

import (
	"path/filepath"
	"strings"
	"testing"
)

// US-1 创建校验：合法通过；reason 超限/空窗/窗口超 30 天/总数上限 拒绝。
func TestV0580CreateValidate(t *testing.T) {
	s := New("")
	if _, err := s.Create("n1", "", 1000, 2000, "检修窗口", "op-1", 500); err != nil {
		t.Fatalf("合法创建失败: %v", err)
	}
	if _, err := s.Create("n1", "", 1000, 2000, strings.Repeat("r", 257), "op-1", 500); err == nil {
		t.Fatalf("reason 超限应拒绝")
	}
	if _, err := s.Create("n1", "", 2000, 2000, "r", "op-1", 500); err == nil {
		t.Fatalf("空窗应拒绝")
	}
	if _, err := s.Create("n1", "", 1000, 1000+30*24*60*60*1000+1, "r", "op-1", 500); err == nil {
		t.Fatalf("超 30 天应拒绝")
	}
	// 全节点窗口（nodeID 空）合法
	if _, err := s.Create("", "", 1000, 2000, "全站", "op-1", 500); err != nil {
		t.Fatalf("全节点创建失败: %v", err)
	}
}

// US-2 生效与匹配：Active 时间边界（含两端）；Matches nodeID 定向与全节点；
// 过期窗口不生效。
func TestV0580ActiveAndMatches(t *testing.T) {
	s := New("")
	sil, err := s.Create("n1", "", 1000, 2000, "检修", "op-1", 500)
	if err != nil {
		t.Fatal(err)
	}
	if !sil.Active(1000) || !sil.Active(2000) || sil.Active(999) || sil.Active(2001) {
		t.Fatalf("Active 边界不符: %+v", sil)
	}
	if !sil.Matches("n1") || sil.Matches("n2") {
		t.Fatalf("Matches 定向不符")
	}
	all, _ := s.Create("", "", 1000, 2000, "全站", "op-1", 500)
	if !all.Matches("any-node") {
		t.Fatalf("全节点窗口应命中任意节点")
	}
	if s.Muted("n1", 1500) != true || s.Muted("n1", 3000) != false {
		t.Fatalf("Muted 判定不符")
	}
	if s.Muted("n9", 1500) != true { // 全节点窗口命中任意节点
		t.Fatalf("全节点 Muted 不符")
	}
}

// US-3 删除与 List：删除即时失效；未知 404；清单创建时间降序。
func TestV0580DeleteAndList(t *testing.T) {
	s := New("")
	a, _ := s.Create("n1", "", 1000, 2000, "a", "op-1", 1000)
	b, _ := s.Create("n2", "", 1000, 2000, "b", "op-1", 2000)
	c, _ := s.Create("n3", "", 1000, 2000, "c", "op-1", 3000)
	list := s.List()
	if len(list) != 3 || list[0].ID != c.ID || list[2].ID != a.ID {
		t.Fatalf("清单降序不符: %+v", list)
	}
	if err := s.Delete(b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(s.List()) != 2 {
		t.Fatalf("删除后应 2 条")
	}
	if err := s.Delete(b.ID); err != ErrNotFound {
		t.Fatalf("重复删除应 ErrNotFound: %v", err)
	}
}

// US-4 持久化 roundtrip：重启恢复（窗口数据完整）。
func TestV0580PersistenceRoundtrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mute.json")
	s := New(file)
	if err := s.Load(); err != nil {
		t.Fatalf("空载 Load: %v", err)
	}
	if _, err := s.Create("n1", "", 1000, 2000, "检修", "op-1", 500); err != nil {
		t.Fatal(err)
	}
	s2 := New(file)
	if err := s2.Load(); err != nil {
		t.Fatalf("恢复 Load: %v", err)
	}
	list := s2.List()
	if len(list) != 1 || list[0].NodeID != "n1" || list[0].Reason != "检修" {
		t.Fatalf("恢复不符: %+v", list)
	}
}

// US-5 总数上限：200 条后拒绝创建（防御性上限锚，抽样验证）。
func TestV0580CapLimit(t *testing.T) {
	s := New("")
	for i := 0; i < 200; i++ {
		if _, err := s.Create("n", "", int64(i*10), int64(i*10+5), "r", "op-1", int64(i)); err != nil {
			t.Fatalf("第 %d 条创建失败: %v", i+1, err)
		}
	}
	if _, err := s.Create("n", "", 1, 2, "r", "op-1", 1); err == nil {
		t.Fatalf("超上限应拒绝")
	}
}
