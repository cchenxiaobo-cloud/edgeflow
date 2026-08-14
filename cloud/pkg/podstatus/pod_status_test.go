package podstatus

import (
	"sync"
	"testing"
)

// newPod 构造一条测试用 Pod 状态。
func newPod(nodeID, namespace, podName, phase string) PodStatus {
	return PodStatus{
		NodeID:          nodeID,
		PodName:         podName,
		Namespace:       namespace,
		Phase:           phase,
		Message:         "",
		LastReconcileAt: 1755168000000,
	}
}

// TestUpsertGet 验证写入后可按 (nodeID, namespace, podName) 精确查询。
func TestUpsertGet(t *testing.T) {
	s := NewStore()

	// 写入两条同 nodeID、不同 namespace 的 Pod（验证跨 namespace 不互相覆盖）
	s.Upsert("edge-001", newPod("", "default", "web-demo", PhaseRunning))
	s.Upsert("edge-001", newPod("", "kube-system", "web-demo", PhaseStopped))

	ps, ok := s.Get("edge-001", "default", "web-demo")
	if !ok {
		t.Fatal("default/web-demo 应存在")
	}
	if ps.Phase != PhaseRunning {
		t.Errorf("phase = %q，期望 Running", ps.Phase)
	}
	// 参数 nodeID 应覆盖 payload 中的 NodeID（消息来源即权威）
	if ps.NodeID != "edge-001" {
		t.Errorf("NodeID = %q，期望 edge-001", ps.NodeID)
	}

	ps, ok = s.Get("edge-001", "kube-system", "web-demo")
	if !ok {
		t.Fatal("kube-system/web-demo 应存在")
	}
	if ps.Phase != PhaseStopped {
		t.Errorf("phase = %q，期望 Stopped", ps.Phase)
	}

	// 不存在
	if _, ok := s.Get("edge-001", "default", "nope"); ok {
		t.Error("不存在的 Pod 不应命中")
	}
	if _, ok := s.Get("edge-002", "default", "web-demo"); ok {
		t.Error("其他节点的 Pod 不应命中")
	}
}

// TestUpsertDefaultNamespace 验证 namespace 缺省时按 default 存储。
func TestUpsertDefaultNamespace(t *testing.T) {
	s := NewStore()
	s.Upsert("edge-001", newPod("", "", "web-demo", PhaseRunning))

	ps, ok := s.Get("edge-001", "default", "web-demo")
	if !ok {
		t.Fatal("缺省 namespace 应按 default 存储")
	}
	if ps.Namespace != DefaultNamespace {
		t.Errorf("Namespace = %q，期望 %q", ps.Namespace, DefaultNamespace)
	}
}

// TestUpsertOverwrite 验证同键二次上报整体覆盖（以最新上报为准）。
func TestUpsertOverwrite(t *testing.T) {
	s := NewStore()
	s.Upsert("edge-001", newPod("", "default", "web-demo", PhaseRunning))
	s.Upsert("edge-001", PodStatus{
		PodName:         "web-demo",
		Namespace:       "default",
		Phase:           PhaseError,
		Message:         "OOMKilled",
		LastReconcileAt: 1755168100000,
	})

	ps, _ := s.Get("edge-001", "default", "web-demo")
	if ps.Phase != PhaseError || ps.Message != "OOMKilled" || ps.LastReconcileAt != 1755168100000 {
		t.Errorf("应整体覆盖为最新上报: %+v", ps)
	}
}

// TestUpsertIgnoreInvalid 验证空 nodeID/podName 的写入被忽略。
func TestUpsertIgnoreInvalid(t *testing.T) {
	s := NewStore()
	s.Upsert("", newPod("", "default", "web-demo", PhaseRunning))
	s.Upsert("edge-001", PodStatus{Namespace: "default"}) // podName 为空

	if len(s.ListAll()) != 0 {
		t.Errorf("非法写入应被忽略，实际有 %d 条", len(s.ListAll()))
	}
}

// TestDelete 验证删除：命中返回 true、不存在返回 false、删后查询不到。
func TestDelete(t *testing.T) {
	s := NewStore()
	s.Upsert("edge-001", newPod("", "default", "a", PhaseRunning))
	s.Upsert("edge-001", newPod("", "default", "b", PhaseStopped))

	if !s.Delete("edge-001", "default", "a") {
		t.Error("删除存在的 Pod 应返回 true")
	}
	if s.Delete("edge-001", "default", "a") {
		t.Error("重复删除应返回 false")
	}
	if _, ok := s.Get("edge-001", "default", "a"); ok {
		t.Error("删除后不应再查询到")
	}
	// 其余条目不受影响
	if _, ok := s.Get("edge-001", "default", "b"); !ok {
		t.Error("删除不应影响其他 Pod")
	}
	// 节点下全部删光后，节点条目应被清理
	if s.Delete("edge-001", "default", "b") && len(s.ListByNode("edge-001")) != 0 {
		t.Error("节点下无 Pod 时应清理空条目")
	}
}

// TestListOrdering 验证 ListAll/ListByNode 排序确定：nodeID → namespace → podName。
func TestListOrdering(t *testing.T) {
	s := NewStore()
	s.Upsert("edge-002", newPod("", "default", "b", PhaseRunning))
	s.Upsert("edge-001", newPod("", "kube-system", "a", PhaseRunning))
	s.Upsert("edge-001", newPod("", "default", "a", PhaseRunning))
	s.Upsert("edge-001", newPod("", "default", "b", PhaseRunning))

	all := s.ListAll()
	if len(all) != 4 {
		t.Fatalf("ListAll 条数 = %d，期望 4", len(all))
	}
	expect := []string{
		"edge-001/default/a",
		"edge-001/default/b",
		"edge-001/kube-system/a",
		"edge-002/default/b",
	}
	for i, e := range expect {
		got := all[i].NodeID + "/" + all[i].Namespace + "/" + all[i].PodName
		if got != e {
			t.Errorf("all[%d] = %q，期望 %q", i, got, e)
		}
	}

	node := s.ListByNode("edge-001")
	if len(node) != 3 {
		t.Fatalf("ListByNode 条数 = %d，期望 3", len(node))
	}
	if node[0].PodName != "a" || node[0].Namespace != "default" {
		t.Errorf("ListByNode 应按 namespace/podName 排序: %+v", node[0])
	}
}

// TestListEmpty 验证无数据时返回空切片而非 nil（JSON 编码为 [] 而非 null）。
func TestListEmpty(t *testing.T) {
	s := NewStore()
	if all := s.ListAll(); all == nil || len(all) != 0 {
		t.Errorf("ListAll 应为非 nil 空切片，实际 %#v", all)
	}
	if node := s.ListByNode("edge-001"); node == nil || len(node) != 0 {
		t.Errorf("ListByNode 应为非 nil 空切片，实际 %#v", node)
	}
}

// TestReturnedCopies 验证 Get/List 返回拷贝：外部修改不污染存储。
func TestReturnedCopies(t *testing.T) {
	s := NewStore()
	s.Upsert("edge-001", newPod("", "default", "web-demo", PhaseRunning))

	ps, _ := s.Get("edge-001", "default", "web-demo")
	ps.Phase = PhaseError
	again, _ := s.Get("edge-001", "default", "web-demo")
	if again.Phase != PhaseRunning {
		t.Error("修改 Get 返回值不应影响存储")
	}

	list := s.ListAll()
	list[0].Phase = PhaseError
	again, _ = s.Get("edge-001", "default", "web-demo")
	if again.Phase != PhaseRunning {
		t.Error("修改 List 返回值不应影响存储")
	}
}

// TestConcurrentUpsertList 并发读写压力测试（配合 -race 检测数据竞争）。
// 模拟 CloudHub 多连接处理 goroutine 与 HTTP 查询并发访问同一存储。
func TestConcurrentUpsertList(t *testing.T) {
	s := NewStore()

	const workers = 8
	const rounds = 200
	var wg sync.WaitGroup

	// 写入方：每个 worker 写自己专属 nodeID，交叉写公共 nodeID
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				nodeID := "edge-" + string(rune('a'+w))
				s.Upsert(nodeID, newPod("", "default", "web-demo", PhaseRunning))
				s.Upsert("edge-shared", newPod("", "default", "p"+string(rune('a'+w)), PhaseRunning))
			}
		}(w)
	}

	// 读取方：并发遍历全部数据
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				_ = s.ListAll()
				_ = s.ListByNode("edge-shared")
				_, _ = s.Get("edge-shared", "default", "pa")
				_ = s.Delete("edge-shared", "default", "pa")
			}
		}()
	}

	wg.Wait()

	// 最终一致性检查：共享节点应有 workers 条 Pod（删除可能使部分缺失，仅校验不 panic）
	if n := len(s.ListByNode("edge-shared")); n == 0 {
		t.Error("edge-shared 节点不应为空")
	}
}
