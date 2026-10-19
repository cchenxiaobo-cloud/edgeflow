package logstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"edgeflow/pkg/protocol"
)

func entry(ts int64, source, level, msg string) protocol.LogEntry {
	return protocol.LogEntry{Ts: ts, Source: source, Level: level, Message: msg}
}

func TestV0480RingEvictionAndQuery(t *testing.T) {
	s, err := New(5, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 8; i++ { // 8 条进容量 5 的环 → 最旧 3 条淘汰
		s.Append("n1", []protocol.LogEntry{entry(int64(1000+i), "app", "info", fmt.Sprintf("line-%d", i))})
	}
	got, _ := s.Query(QueryOpts{NodeID: "n1"})
	if len(got) != 5 {
		t.Fatalf("环形淘汰后应剩 5 条，得 %d", len(got))
	}
	if got[0].Message != "line-3" || got[4].Message != "line-7" {
		t.Fatalf("淘汰口径错误（应丢 line-0..2）: %+v", got)
	}
}

func TestV0480QueryFilters(t *testing.T) {
	s, _ := New(0, "")
	s.Append("n1", []protocol.LogEntry{
		entry(1000, "app", "info", "hello world"),
		entry(2000, "app", "error", "boom happened"),
		entry(3000, "scriptrunner", "warn", "script slow"),
		entry(4000, "app", "error", "boom again"),
	})
	if got, _ := s.Query(QueryOpts{NodeID: "n1", Level: "error"}); len(got) != 2 {
		t.Fatalf("level 过滤应 2 条: %d", len(got))
	}
	if got, _ := s.Query(QueryOpts{NodeID: "n1", Q: "BOOM"}); len(got) != 2 {
		t.Fatalf("关键字（大小写不敏感）应 2 条: %d", len(got))
	}
	if got, _ := s.Query(QueryOpts{NodeID: "n1", Source: "scriptrunner"}); len(got) != 1 {
		t.Fatalf("source 过滤应 1 条: %d", len(got))
	}
	if got, _ := s.Query(QueryOpts{NodeID: "n1", Since: 1500, Until: 3500}); len(got) != 2 {
		t.Fatalf("时间窗应 2 条: %d", len(got))
	}
	// 分页：limit 2 offset 1（时间序）
	got, _ := s.Query(QueryOpts{NodeID: "n1", Limit: 2, Offset: 1})
	if len(got) != 2 || got[0].Ts != 2000 || got[1].Ts != 3000 {
		t.Fatalf("分页口径错误: %+v", got)
	}
	// 跨节点（node 缺省）：n2 追加后按 ts 归并
	s.Append("n2", []protocol.LogEntry{entry(2500, "app", "info", "other node")})
	all, _ := s.Query(QueryOpts{Limit: 100})
	if len(all) != 5 {
		t.Fatalf("跨节点应 5 条: %d", len(all))
	}
	if all[1].Ts != 2000 || all[2].Ts != 2500 || all[3].Ts != 3000 {
		t.Fatalf("跨节点未按 ts 归并: %+v", all)
	}
}

func TestV0480SummaryTailDelete(t *testing.T) {
	s, _ := New(0, "")
	if s.Summary("ghost") != nil || s.Tail("ghost", 10) != nil {
		t.Fatalf("空节点应 nil")
	}
	if s.Delete("ghost") {
		t.Fatalf("删除不存在节点应 false")
	}
	s.Append("n1", []protocol.LogEntry{
		entry(1000, "app", "info", "a"),
		entry(2000, "app", "error", "b"),
		entry(3000, "sys", "error", "c"),
	})
	sum := s.Summary("n1")
	if sum == nil || sum.Count != 3 || sum.Sources["app"] != 2 || sum.Levels["error"] != 2 || sum.Latest != 3000 {
		t.Fatalf("概要口径错误: %+v", sum)
	}
	tail := s.Tail("n1", 2)
	if len(tail) != 2 || tail[0].Ts != 2000 || tail[1].Ts != 3000 {
		t.Fatalf("tail 口径错误: %+v", tail)
	}
	if !s.Delete("n1") {
		t.Fatalf("删除应返回 true")
	}
	afterDel, _ := s.Query(QueryOpts{NodeID: "n1"})
	if len(afterDel) != 0 {
		t.Fatalf("删除后应空 %d", len(afterDel))
	}
}

func TestV0480JSONLPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.jsonl")
	s, err := New(10, path)
	if err != nil {
		t.Fatalf("New with file: %v", err)
	}
	s.Append("n1", []protocol.LogEntry{entry(1000, "app", "info", "persist-me")})
	s.Append("n2", []protocol.LogEntry{entry(2000, "app", "warn", "node-two")})
	// 模拟重启：新建 Store 回放同一文件
	s2, err := New(10, path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	gotReload, _ := s2.Query(QueryOpts{NodeID: "n1"})
	if len(gotReload) != 1 || gotReload[0].Message != "persist-me" {
		t.Fatalf("回放缺失: %+v", gotReload)
	}
	if s2.Summary("n2") == nil {
		t.Fatalf("n2 回放缺失")
	}
	// 损坏行容忍：追加垃圾行后仍可加载
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("this-is-not-json\n")
	f.Close()
	if _, err := New(10, path); err != nil {
		t.Fatalf("损坏行应跳过: %v", err)
	}
}

func TestV0480JSONLRotate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.jsonl")
	s, _ := New(1000, path)
	// 单条凑大近阈值 → 触发一轮 rotate
	big := strings.Repeat("x", 3*1024) // 3KB/条
	for i := 0; i < 6000; i++ {        // ~18MB > 16MB 阈值
		s.Append("n1", []protocol.LogEntry{entry(int64(i), "app", "info", big)})
	}
	if _, err := os.Stat(path + ".old"); err != nil {
		t.Fatalf("rotate 未发生: %v", err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() >= jsonlRotateBytes {
		t.Fatalf("主文件应回到阈值下: %+v", st)
	}
}

func TestV0480JSONLPayloadRoundTrip(t *testing.T) {
	// LogSync 负载与存储行同构：协议批 → JSON → Validate → 存储追加
	batch := protocol.LogBatch{Entries: []protocol.LogEntry{
		{Ts: 1, Source: "app", Level: protocol.LogLevelInfo, Message: "ok"},
	}}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back protocol.LogBatch
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	s, _ := New(0, "")
	s.Append("n1", back.Entries)
	got2, _ := s.Query(QueryOpts{NodeID: "n1"})
	if len(got2) != 1 {
		t.Fatalf("round-trip 后应可检索")
	}
}

func TestV0480QueryFilteredTotal(t *testing.T) {
	// 复核 P2-3：Query 返回过滤后总数（供 X-Total-Count 分页语义）
	s, _ := New(0, "")
	for i := 0; i < 10; i++ {
		s.Append("n1", []protocol.LogEntry{entry(int64(1000+i), "app", "info", "match-"+itoa2(i))})
	}
	s.Append("n1", []protocol.LogEntry{entry(2000, "app", "warn", "no-match")})
	got, total := s.Query(QueryOpts{NodeID: "n1", Q: "match-", Limit: 3})
	if len(got) != 3 || total != 10 {
		t.Fatalf("页内应 3 条/总数应 10: %d/%d", len(got), total)
	}
	if _, total = s.Query(QueryOpts{NodeID: "n1", Q: "match-", Offset: 9, Limit: 3}); total != 10 {
		t.Fatalf("offset 不影响 total: %d", total)
	}
}

func itoa2(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
