// v0620_test.go：静默窗口 severity 维度单测（v0.62.0，spec 0035 裁决 2）。
package mutestore

import (
	"path/filepath"
	"testing"
)

// US-1 severity 校验：合法（critical/warning/info/空）通过；非法值拒绝。
func TestV0620SeverityValidate(t *testing.T) {
	s := New("")
	for _, sev := range []string{"critical", "warning", "info", ""} {
		if _, err := s.Create("n1", sev, 1000, 2000, "r", "op-1", 500); err != nil {
			t.Fatalf("severity=%q 创建应通过: %v", sev, err)
		}
	}
	if _, err := s.Create("n1", "urgent", 1000, 2000, "r", "op-1", 500); err == nil {
		t.Fatalf("非法 severity 应拒绝")
	}
}

// US-2 MatchesEvent：severity 维度匹配（窗口未指定级别=全部级别命中；
// 指定级别仅同级命中）；nodeID 维度叠加生效。
func TestV0620MatchesEvent(t *testing.T) {
	s := New("")
	crit, _ := s.Create("n1", "critical", 1000, 2000, "只静默 critical", "op-1", 500)
	all, _ := s.Create("n2", "", 1000, 2000, "全级别", "op-1", 500)

	if !crit.MatchesEvent("n1", "critical") {
		t.Fatal("同级应命中")
	}
	if crit.MatchesEvent("n1", "warning") {
		t.Fatal("异级不应命中")
	}
	if !all.MatchesEvent("n2", "warning") {
		t.Fatal("全级别窗口应命中任意级别")
	}
	_ = all
	_ = crit
}

// US-3 Muted（节点级，层级活跃口径用）不受 severity 维度影响——
// severity 静默窗口不参与节点级判定（口径差异 spec 0035 裁决 4）。
func TestV0620MutedIgnoresSeverity(t *testing.T) {
	s := New("")
	if _, err := s.Create("n1", "critical", 1000, 2000, "r", "op-1", 500); err != nil {
		t.Fatal(err)
	}
	if s.Muted("n1", 1500) {
		t.Fatal("severity 维度窗口不应参与节点级 Muted 判定")
	}
}

// US-4 持久化 roundtrip 含 severity 字段。
func TestV0620SeverityPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mute.json")
	s := New(file)
	if _, err := s.Create("n1", "warning", 1000, 2000, "r", "op-1", 500); err != nil {
		t.Fatal(err)
	}
	s2 := New(file)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	list := s2.List()
	if len(list) != 1 || list[0].Severity != "warning" {
		t.Fatalf("severity 恢复不符: %+v", list)
	}
}
