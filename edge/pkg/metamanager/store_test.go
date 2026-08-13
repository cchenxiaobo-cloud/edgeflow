package metamanager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestStore 在临时目录中打开一个 Store，并注册清理。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "edgeflow.db"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestOpenCreatesDirAndWAL 验证：目录不存在时自动创建、WAL 模式已开启。
func TestOpenCreatesDirAndWAL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "a", "b", "edgeflow.db") // 两级不存在的目录
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(嵌套目录) 失败: %v", err)
	}
	defer func() { _ = s.Close() }()

	// WAL 模式是数据库文件级属性，重开依然生效
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("查询 journal_mode 失败: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q，期望 wal", mode)
	}
}

// TestOpenErrors 验证非法路径报错：空路径、父目录是普通文件。
func TestOpenErrors(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("Open(\"\") 期望报错，实际成功")
	}

	// 父目录是一个普通文件 → MkdirAll 应报 ENOTDIR
	notDir := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备测试文件失败: %v", err)
	}
	if _, err := Open(filepath.Join(notDir, "db.sqlite")); err == nil {
		t.Error("Open(父目录为文件) 期望报错，实际成功")
	}
}

// TestKVBasic 覆盖 Put/Get/Delete/List 全流程。
func TestKVBasic(t *testing.T) {
	s := newTestStore(t)

	// 初始为空
	entries, err := s.List("")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("初始 List 长度 = %d，期望 0", len(entries))
	}

	// Get 不存在的键 → (false, nil)
	if v, ok, err := s.Get("no-such-key"); err != nil || ok {
		t.Errorf("Get(不存在) = (%q, %v, %v)，期望 (\"\", false, nil)", v, ok, err)
	}

	// Put 后 Get
	if err := s.Put("k1", "v1"); err != nil {
		t.Fatalf("Put(k1) 失败: %v", err)
	}
	if v, ok, err := s.Get("k1"); err != nil || !ok || v != "v1" {
		t.Errorf("Get(k1) = (%q, %v, %v)，期望 (\"v1\", true, nil)", v, ok, err)
	}

	// Put 覆盖同 key
	if err := s.Put("k1", "v1-new"); err != nil {
		t.Fatalf("Put(k1, 覆盖) 失败: %v", err)
	}
	if v, _, _ := s.Get("k1"); v != "v1-new" {
		t.Errorf("Get(k1) 覆盖后 = %q，期望 %q", v, "v1-new")
	}

	// 更多键 + 前缀过滤
	if err := s.Put("k2", "v2"); err != nil {
		t.Fatalf("Put(k2) 失败: %v", err)
	}
	if err := s.Put("other", "v3"); err != nil {
		t.Fatalf("Put(other) 失败: %v", err)
	}
	entries, err = s.List("k")
	if err != nil {
		t.Fatalf("List(k) 失败: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List(k) 长度 = %d，期望 2", len(entries))
	}
	if entries[0].Key != "k1" || entries[1].Key != "k2" {
		t.Errorf("List(k) 顺序 = [%s, %s]，期望按 key 升序 [k1, k2]",
			entries[0].Key, entries[1].Key)
	}
	if entries[0].UpdatedAt <= 0 {
		t.Errorf("UpdatedAt 未写入: %d", entries[0].UpdatedAt)
	}

	// Delete 后 Get 不到，且 List 前缀为空
	if err := s.Delete("k1"); err != nil {
		t.Fatalf("Delete(k1) 失败: %v", err)
	}
	if _, ok, err := s.Get("k1"); err != nil || ok {
		t.Errorf("Delete 后 Get(k1) = (%v, %v)，期望 (false, nil)", ok, err)
	}
	// Delete 不存在的键：幂等不报错
	if err := s.Delete("k1"); err != nil {
		t.Errorf("Delete(不存在) 期望幂等成功，实际: %v", err)
	}
}

// TestNodeInfo 覆盖节点信息保存/读取/列出/删除。
func TestNodeInfo(t *testing.T) {
	s := newTestStore(t)

	info := NodeInfo{
		NodeID:          "edge-test-1",
		NodeName:        "mock-edge-test-1",
		CloudAddr:       "ws://127.0.0.1:10000",
		Arch:            "arm64",
		OS:              "linux",
		EdgeCoreVersion: "v0.1.0",
		RegisteredAt:    1723000000000,
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("序列化 NodeInfo 失败: %v", err)
	}

	if err := s.SaveNodeInfo(info.NodeID, string(data)); err != nil {
		t.Fatalf("SaveNodeInfo 失败: %v", err)
	}

	// 读回并反序列化比对
	raw, ok, err := s.GetNodeInfo(info.NodeID)
	if err != nil || !ok {
		t.Fatalf("GetNodeInfo = (%q, %v, %v)，期望存在", raw, ok, err)
	}
	var got NodeInfo
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if got != info {
		t.Errorf("读回 NodeInfo 不一致:\n期望 %+v\n实际 %+v", info, got)
	}

	// 覆盖保存（重连/重启后再次注册）：只保留最新一条
	info.NodeName = "mock-edge-test-1-v2"
	data2, _ := json.Marshal(info)
	if err := s.SaveNodeInfo(info.NodeID, string(data2)); err != nil {
		t.Fatalf("SaveNodeInfo(覆盖) 失败: %v", err)
	}
	infos, err := s.ListNodes()
	if err != nil {
		t.Fatalf("ListNodes 失败: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("ListNodes 长度 = %d，期望 1（同节点覆盖不新增）", len(infos))
	}
	if !strings.Contains(infos[0], "mock-edge-test-1-v2") {
		t.Errorf("ListNodes 内容不是最新记录: %s", infos[0])
	}

	// GetNodeInfo 不存在的节点 → (false, nil)
	if _, ok, err := s.GetNodeInfo("edge-unknown"); err != nil || ok {
		t.Errorf("GetNodeInfo(不存在) = (%v, %v)，期望 (false, nil)", ok, err)
	}

	// 删除
	if err := s.DeleteNodeInfo(info.NodeID); err != nil {
		t.Fatalf("DeleteNodeInfo 失败: %v", err)
	}
	if infos, err := s.ListNodes(); err != nil || len(infos) != 0 {
		t.Errorf("删除后 ListNodes = %v, %v，期望空", infos, err)
	}
}

// TestPersistenceAcrossReopen 是 M1 验收核心用例：
// 写入 → 关闭 Store → 重新 Open → 数据仍在（模拟 edgecore 重启）。
func TestPersistenceAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persist", "edgeflow.db")

	// 第一次打开：写入 KV 与节点信息
	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("第一次 Open 失败: %v", err)
	}
	if err := s1.Put("config:heartbeat", "30s"); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	info := NodeInfo{
		NodeID:       "edge-persist-1",
		NodeName:     "mock-edge-persist-1",
		CloudAddr:    "ws://127.0.0.1:10000",
		RegisteredAt: 1723000000000,
	}
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

	// KV 仍在
	if v, ok, err := s2.Get("config:heartbeat"); err != nil || !ok || v != "30s" {
		t.Errorf("重启后 Get(config:heartbeat) = (%q, %v, %v)，期望 (\"30s\", true, nil)", v, ok, err)
	}
	// 节点信息仍在
	infos, err := s2.ListNodes()
	if err != nil {
		t.Fatalf("重启后 ListNodes 失败: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("重启后 ListNodes 长度 = %d，期望 1（M1 验收：数据持久化）", len(infos))
	}
	if !strings.Contains(infos[0], "edge-persist-1") {
		t.Errorf("重启后节点信息内容异常: %s", infos[0])
	}
	// WAL 模式在重开后依然生效（文件级属性）
	var mode string
	if err := s2.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || !strings.EqualFold(mode, "wal") {
		t.Errorf("重启后 journal_mode = %q, %v，期望 wal", mode, err)
	}
}
