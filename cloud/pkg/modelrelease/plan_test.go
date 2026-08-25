package modelrelease

// 算法纯函数单测（设计 §10.1 A1/A2；WBS-9）。
//
// A1 SelectPercentageNodes：0 台 Ready → error；1 台 → 1；3×50%→2；
// 23×10%→3；100×1%→1；10×100%→10；字典序确定性；archs 过滤
// （ApplyArchFilter / SelectPercentageNodesByArch）。
// A2 BuildBatches：batchSize=1/2/len/len+1 边界；批序号预分配正确。

import (
	"errors"
	"reflect"
	"testing"

	"edgeflow/cloud/pkg/modelrepo"
)

// ── A1：百分比选择 ─────────────────────────────────────────────────────

func TestSelectPercentageNodes_RoundUp(t *testing.T) {
	cases := []struct {
		name  string
		ready []string
		pct   int
		want  int // 期望节点数
	}{
		{"1 node any pct", []string{"a"}, 1, 1},
		{"1 node 100pct", []string{"a"}, 100, 1},
		{"3x50 -> 2 ceil", []string{"a", "b", "c"}, 50, 2},
		{"23x10 -> 3 ceil", mkNodes(23), 10, 3},
		{"100x1 -> 1", mkNodes(100), 1, 1},
		{"10x100 -> 10", mkNodes(10), 100, 10},
		{"5x30 -> 2 ceil(1.5)", mkNodes(5), 30, 2},
		{"2x40 -> 1", mkNodes(2), 40, 1},
		{"16x60 -> 10 ceil(9.6)", mkNodes(16), 60, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectPercentageNodes(tc.ready, tc.pct)
			if err != nil {
				t.Fatalf("SelectPercentageNodes(%d nodes, %d%%) 意外错误: %v", len(tc.ready), tc.pct, err)
			}
			if len(got) != tc.want {
				t.Fatalf("n = %d, want %d（ceil 口径）: got %v", len(got), tc.want, got)
			}
		})
	}
}

func TestSelectPercentageNodes_NoReady(t *testing.T) {
	_, err := SelectPercentageNodes(nil, 50)
	if !errors.Is(err, modelrepo.ErrNoReadyNodes) {
		t.Fatalf("0 台 Ready 应返回 ErrNoReadyNodes，got %v", err)
	}
	_, err = SelectPercentageNodes([]string{}, 1)
	if !errors.Is(err, modelrepo.ErrNoReadyNodes) {
		t.Fatalf("空切片应返回 ErrNoReadyNodes，got %v", err)
	}
}

func TestSelectPercentageNodes_InvalidPct(t *testing.T) {
	for _, pct := range []int{0, 101, -1} {
		if _, err := SelectPercentageNodes([]string{"a"}, pct); err == nil {
			t.Fatalf("pct=%d 应报参数错误", pct)
		}
	}
}

func TestSelectPercentageNodes_LexicographicDeterminism(t *testing.T) {
	// 跨副本可复现：不同输入顺序 → 同一输出（字典序前 n）
	unordered := []string{"n9", "n1", "n50", "n2", "n10"}
	other := []string{"n2", "n50", "n10", "n9", "n1"}
	got1, err := SelectPercentageNodes(unordered, 40)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := SelectPercentageNodes(other, 40)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got1, got2) {
		t.Fatalf("字典序确定性破坏: %v vs %v", got1, got2)
	}
	if !reflect.DeepEqual(got1, []string{"n1", "n10"}) {
		t.Fatalf("应取字典序前 2（n1<n10<n2<n50<n9），got %v", got1)
	}
	// 入参不被原地修改（纯函数）
	if !reflect.DeepEqual(unordered, []string{"n9", "n1", "n50", "n2", "n10"}) {
		t.Fatalf("纯函数不应修改入参: %v", unordered)
	}
}

func TestSelectPercentageNodes_100PctAll(t *testing.T) {
	got, err := SelectPercentageNodes([]string{"c", "a", "b"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("100%% 应取全部（字典序），got %v", got)
	}
}

func TestApplyArchFilter(t *testing.T) {
	nodes := []NodeRef{
		{NodeID: "n1", Arch: "amd64"},
		{NodeID: "n2", Arch: "arm64"},
		{NodeID: "n3", Arch: "amd64"},
		{NodeID: "n4", Arch: ""},
	}
	// archs 为空 = 不限制，返回拷贝
	all := ApplyArchFilter(nodes, nil)
	if len(all) != 4 {
		t.Fatalf("空 archs 应不过滤，got %v", all)
	}
	all[0].NodeID = "hacked" // 拷贝不被原切片影响
	if nodes[0].NodeID != "n1" {
		t.Fatal("ApplyArchFilter 应返回拷贝，修改输出污染了输入")
	}
	got := ApplyArchFilter(nodes, []string{"amd64"})
	want := []NodeRef{{NodeID: "n1", Arch: "amd64"}, {NodeID: "n3", Arch: "amd64"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("amd64 过滤: got %v want %v", got, want)
	}
	// 顺序保持
	got = ApplyArchFilter([]NodeRef{{NodeID: "b", Arch: "arm64"}, {NodeID: "a", Arch: "arm64"}}, []string{"arm64"})
	if got[0].NodeID != "b" || got[1].NodeID != "a" {
		t.Fatalf("过滤应保持原序，got %v", got)
	}
}

func TestSelectPercentageNodesByArch(t *testing.T) {
	ready := []NodeRef{
		{NodeID: "n1", Arch: "amd64"},
		{NodeID: "n2", Arch: "arm64"},
		{NodeID: "n3", Arch: "amd64"},
		{NodeID: "n4", Arch: "amd64"},
	}
	got, err := SelectPercentageNodesByArch(ready, 50, []string{"amd64"})
	if err != nil {
		t.Fatal(err)
	}
	// 过滤后 3 台 amd64，50% → ceil(1.5)=2，字典序前 2
	if !reflect.DeepEqual(got, []string{"n1", "n3"}) {
		t.Fatalf("amd64 50%% 应选 [n1 n3]，got %v", got)
	}
	// 过滤后 0 台 → ErrNoReadyNodes + 文案含 archs
	_, err = SelectPercentageNodesByArch(ready, 50, []string{"riscv"})
	if !errors.Is(err, modelrepo.ErrNoReadyNodes) {
		t.Fatalf("过滤后 0 台应 ErrNoReadyNodes，got %v", err)
	}
	if err != nil && len(err.Error()) < 10 {
		t.Fatal("错误文案应含架构信息")
	}
	// 0 台 Ready（未过滤）→ 纯 no ready nodes 文案
	_, err = SelectPercentageNodesByArch(nil, 50, []string{"amd64"})
	if !errors.Is(err, modelrepo.ErrNoReadyNodes) {
		t.Fatalf("0 台 Ready 应 ErrNoReadyNodes，got %v", err)
	}
	// 100% 取过滤后全部
	got, err = SelectPercentageNodesByArch(ready, 100, []string{"arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"n2"}) {
		t.Fatalf("arm64 100%% 应选 [n2]，got %v", got)
	}
}

// mkNodes 生成 n 个形如 n1..nn 的节点 ID 列表（乱序生成由调用方负责）。
func mkNodes(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "n"+string(rune('0'+i%10))+"x"+string(rune('a'+i%26)))
	}
	return out
}

// ── A2：批次规划 ───────────────────────────────────────────────────────

func TestBuildBatches_Boundaries(t *testing.T) {
	nodes := []string{"a", "b", "c", "d", "e"}
	cases := []struct {
		name      string
		batchSize int
		want      [][]string
	}{
		{"batchSize=1 → 每节点一批", 1, [][]string{{"a"}, {"b"}, {"c"}, {"d"}, {"e"}}},
		{"batchSize=2 → 2/2/1", 2, [][]string{{"a", "b"}, {"c", "d"}, {"e"}}},
		{"batchSize=len → 单批", 5, [][]string{{"a", "b", "c", "d", "e"}}},
		{"batchSize=len+1 → 单批", 6, [][]string{{"a", "b", "c", "d", "e"}}},
		{"batchSize<1 防御 → 按 1", 0, [][]string{{"a"}, {"b"}, {"c"}, {"d"}, {"e"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildBatches(nodes, tc.batchSize)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("BuildBatches(%v, %d) = %v, want %v", nodes, tc.batchSize, got, tc.want)
			}
		})
	}
	// 空输入 → nil；批内切片为拷贝（修改输出不污染 TargetNodes）
	empty := BuildBatches(nil, 2)
	if empty != nil {
		t.Fatalf("空输入应返回 nil，got %v", empty)
	}
	got := BuildBatches(nodes, 2)
	got[0][0] = "mutated"
	if nodes[0] != "a" {
		t.Fatal("BuildBatches 输出应拷贝节点 ID（修改污染 TargetNodes）")
	}
}

// TestBatchNumber_预分配 验证批序号口径与 BuildBatches 一致（设计 §2.3：
// perNode 创建时预分配 Batch 序号，不可变；控制器与 API 共用同一口径）。
func TestBatchNumber_预分配(t *testing.T) {
	cases := []struct {
		idx, size, want int
	}{
		{0, 2, 1}, {1, 2, 1}, {2, 2, 2}, {3, 2, 2}, {4, 2, 3},
		{0, 1, 1}, {9, 1, 10},
		{0, 5, 1}, {4, 5, 1}, {5, 5, 2},
		{0, 0, 1}, // 防御
	}
	for _, tc := range cases {
		if got := BatchNumber(tc.idx, tc.size); got != tc.want {
			t.Fatalf("BatchNumber(%d, %d) = %d, want %d", tc.idx, tc.size, got, tc.want)
		}
	}
	// 与 BuildBatches 切分对拍：每批节点序号一致
	nodes := []string{"a", "b", "c", "d", "e", "f", "g"}
	batches := BuildBatches(nodes, 3)
	for i, b := range batches {
		for j, nodeID := range b {
			if nodeID != nodes[i*3+j] {
				t.Fatalf("批次切分与序号口径不一致 at %d/%d", i, j)
			}
		}
	}
}
