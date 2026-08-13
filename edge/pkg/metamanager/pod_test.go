package metamanager

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// podJSON 构造一条符合契约的 Pod JSON 字符串。
func podJSON(name, image string, replicas int) string {
	data, _ := json.Marshal(Pod{
		Name:      name,
		Namespace: "default",
		Image:     image,
		Replicas:  replicas,
	})
	return string(data)
}

// TestPodLifecycle 覆盖 SavePod/ListPods/DeletePod 全流程（M2 前置）：
// 保存 → 列出 → 覆盖更新 → 删除；非法输入报错。
func TestPodLifecycle(t *testing.T) {
	s := newTestStore(t)

	// 初始为空
	pods, err := s.ListPods()
	if err != nil {
		t.Fatalf("ListPods 失败: %v", err)
	}
	if len(pods) != 0 {
		t.Errorf("初始 ListPods 长度 = %d，期望 0", len(pods))
	}

	// 保存两条 Pod
	nginx := podJSON("nginx", "nginx:1.25", 1)
	redis := podJSON("redis", "redis:7.0", 1)
	if err := s.SavePod(nginx); err != nil {
		t.Fatalf("SavePod(nginx) 失败: %v", err)
	}
	if err := s.SavePod(redis); err != nil {
		t.Fatalf("SavePod(redis) 失败: %v", err)
	}

	pods, err = s.ListPods()
	if err != nil {
		t.Fatalf("ListPods 失败: %v", err)
	}
	if len(pods) != 2 {
		t.Fatalf("ListPods 长度 = %d，期望 2", len(pods))
	}
	if pods[0] != nginx || pods[1] != redis {
		t.Errorf("ListPods 内容/顺序异常: %v", pods)
	}
	// value 是 Pod 的 JSON 原样保存（不裁剪、不改写）
	if !strings.Contains(pods[0], `"image":"nginx:1.25"`) {
		t.Errorf("Pod JSON 未原样保存: %s", pods[0])
	}

	// 同 name 覆盖保存（云端重复下发同 Pod）：不新增记录
	if err := s.SavePod(podJSON("nginx", "nginx:1.26", 2)); err != nil {
		t.Fatalf("SavePod(nginx 覆盖) 失败: %v", err)
	}
	pods, err = s.ListPods()
	if err != nil {
		t.Fatalf("ListPods 失败: %v", err)
	}
	if len(pods) != 2 {
		t.Fatalf("覆盖后 ListPods 长度 = %d，期望 2（同 name 覆盖不新增）", len(pods))
	}
	if !strings.Contains(pods[0], `"image":"nginx:1.26"`) {
		t.Errorf("覆盖后内容不是最新记录: %s", pods[0])
	}

	// 删除
	if err := s.DeletePod("nginx"); err != nil {
		t.Fatalf("DeletePod(nginx) 失败: %v", err)
	}
	pods, err = s.ListPods()
	if err != nil {
		t.Fatalf("ListPods 失败: %v", err)
	}
	if len(pods) != 1 || pods[0] != redis {
		t.Errorf("删除后 ListPods = %v，期望只剩 redis", pods)
	}
	// 删除不存在的 Pod：幂等不报错
	if err := s.DeletePod("nginx"); err != nil {
		t.Errorf("DeletePod(不存在) 期望幂等成功，实际: %v", err)
	}
}

// TestSavePodErrors 验证非法输入报错：非 JSON、缺 name 字段、空 name 删除。
func TestSavePodErrors(t *testing.T) {
	s := newTestStore(t)

	if err := s.SavePod("not-json{"); err == nil {
		t.Error("SavePod(非法 JSON) 期望报错，实际成功")
	}
	if err := s.SavePod(`{"namespace":"default","image":"x"}`); err == nil {
		t.Error("SavePod(缺 name) 期望报错，实际成功")
	}
	if err := s.SavePod(""); err == nil {
		t.Error("SavePod(空串) 期望报错，实际成功")
	}
	if err := s.DeletePod(""); err == nil {
		t.Error("DeletePod(空 name) 期望报错，实际成功")
	}
	// 出错时不应留下脏数据
	pods, err := s.ListPods()
	if err != nil {
		t.Fatalf("ListPods 失败: %v", err)
	}
	if len(pods) != 0 {
		t.Errorf("出错后 ListPods 长度 = %d，期望 0（不落脏数据）", len(pods))
	}
}

// TestPodPersistenceAcrossReopen 是 M2 前置验收核心用例：
// 保存 Pod → 关闭 Store → 重新 Open → Pod 元数据仍在（模拟 edgecore 重启，
// 与节点信息同库共存，重启后 Edged 可从 SQLite 恢复）。
func TestPodPersistenceAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persist", "edgeflow.db")

	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("第一次 Open 失败: %v", err)
	}
	nginx := podJSON("nginx", "nginx:1.25", 1)
	if err := s1.SavePod(nginx); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
	info := NodeInfo{NodeID: "edge-persist-1", NodeName: "mock-edge-persist-1"}
	data, _ := json.Marshal(info)
	if err := s1.SaveNodeInfo(info.NodeID, string(data)); err != nil {
		t.Fatalf("SaveNodeInfo 失败: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("第一次 Close 失败: %v", err)
	}

	// 模拟 edgecore 重启：重新 Open 同一路径
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("重新 Open 失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	pods, err := s2.ListPods()
	if err != nil {
		t.Fatalf("重启后 ListPods 失败: %v", err)
	}
	if len(pods) != 1 || pods[0] != nginx {
		t.Fatalf("重启后 ListPods = %v，期望 [%s]（M2 前置：Pod 元数据持久化）", pods, nginx)
	}
	// 与节点信息互不干扰
	infos, err := s2.ListNodes()
	if err != nil {
		t.Fatalf("重启后 ListNodes 失败: %v", err)
	}
	if len(infos) != 1 {
		t.Errorf("重启后 ListNodes 长度 = %d，期望 1", len(infos))
	}
}
