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

// TestListPrefixRangeSemantics 验证 List 的范围扫描语义（替代 LIKE 后的
// 回归锚点，对应 CODE-REVIEW-M1B P2-1）：
//  1. prefix 含 %/_ 时按字面匹配，不作为通配符；
//  2. 大小写敏感（LIKE 对 ASCII 大小写不敏感，范围比较不会混同）；
//  3. 空前缀返回全表；
//  4. 前缀尾部 0xff 的极端形态仍精确覆盖"以 prefix 开头"。
func TestListPrefixRangeSemantics(t *testing.T) {
	s := newTestStore(t)

	keys := []string{
		"node:info:a_b",   // 含下划线
		"node:info:aXb",   // 下划线的"近亲"：LIKE 'a_b%' 会误匹配
		"node:info:a%bc",  // 含百分号
		"node:info:abc",   // 正常节点
		"node:info:ABC",   // 大小写变体：LIKE 'abc%' 会误匹配
		"other:node:info", // 前缀外
	}
	for i, k := range keys {
		if err := s.Put(k, "v"+string(rune('0'+i))); err != nil {
			t.Fatalf("Put(%q) 失败: %v", k, err)
		}
	}

	// 含通配符的前缀必须按字面精确匹配
	for _, prefix := range []string{"node:info:a_b", "node:info:a%bc"} {
		entries, err := s.List(prefix)
		if err != nil {
			t.Fatalf("List(%q) 失败: %v", prefix, err)
		}
		if len(entries) != 1 || entries[0].Key != prefix {
			t.Errorf("List(%q) = %v，期望只命中自身", prefix, entryKeys(entries))
		}
	}

	// 大小写敏感：List("node:info:abc") 只命中小写，不命中 ABC
	entries, err := s.List("node:info:abc")
	if err != nil {
		t.Fatalf("List(abc) 失败: %v", err)
	}
	if len(entries) != 1 || entries[0].Key != "node:info:abc" {
		t.Errorf("List(abc) = %v，期望只命中小写（大小写敏感）", entryKeys(entries))
	}

	// 空前缀 = 全表（与文档语义一致）
	entries, err = s.List("")
	if err != nil {
		t.Fatalf("List(\"\") 失败: %v", err)
	}
	if len(entries) != len(keys) {
		t.Errorf("List(\"\") 长度 = %d，期望 %d（全表）", len(entries), len(keys))
	}

	// 前缀尾部 0xff：范围上界进位逻辑（"k\xff" 的上界是 "l"）
	for _, k := range []string{"k\xff", "k\xff\x00", "k\xff\xff"} {
		if err := s.Put(k, "vxff"); err != nil {
			t.Fatalf("Put(%q) 失败: %v", k, err)
		}
	}
	if err := s.Put("k\xfe", "vfe"); err != nil { // 低于下界，不应命中
		t.Fatalf("Put(k\\xfe) 失败: %v", err)
	}
	entries, err = s.List("k\xff")
	if err != nil {
		t.Fatalf("List(k\\xff) 失败: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("List(k\\xff) 长度 = %d，期望 3（k\\xff、k\\xff\\x00、k\\xff\\xff），实际 %v",
			len(entries), entryKeys(entries))
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Key, "k\xff") {
			t.Errorf("List(k\\xff) 命中了非前缀 key: %q", e.Key)
		}
	}

	// 全 0xff 前缀：无有限上界分支（key >= "\xff" 恰为"以 \\xff 开头"）
	for _, k := range []string{"\xff", "\xff\x01", "\xff\xfe\xff"} {
		if err := s.Put(k, "vxff"); err != nil {
			t.Fatalf("Put(%q) 失败: %v", k, err)
		}
	}
	entries, err = s.List("\xff")
	if err != nil {
		t.Fatalf("List(\\xff) 失败: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("List(\\xff) 长度 = %d，期望 3（\\xff、\\xff\\x01、\\xff\\xfe\\xff），实际 %v", len(entries), entryKeys(entries))
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Key, "\xff") {
			t.Errorf("List(\\xff) 命中了非前缀 key: %q", e.Key)
		}
	}
}

// entryKeys 提取 KVEntry 的 key 列表（断言辅助）。
func entryKeys(entries []KVEntry) []string {
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	return keys
}
