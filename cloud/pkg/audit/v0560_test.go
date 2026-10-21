// v0560_test.go：台账查询面单测（v0.56.0，spec 0029）。
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLedger 直写 JSONL 文件（绕过追加面，可构造乱序/损坏行），再开 Ledger。
func writeLedger(t *testing.T, lines []string) *Ledger {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := NewLedger(p)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// line 编码一条 Entry 为 JSONL 行。
func line(t *testing.T, e Entry) string {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// US-1 过滤器：operator/method/status/时间窗/pathSub 各维独立生效；损坏行跳过；
// 结果 Ts 降序（新→旧）。
func TestV0560QueryFilters(t *testing.T) {
	l := writeLedger(t, []string{
		line(t, Entry{Ts: 100, Action: "GET /api/v1/nodes", Method: "GET", Path: "/api/v1/nodes", Status: 200, Operator: "op-1"}),
		"not-a-json-line", // 损坏行：跳过
		line(t, Entry{Ts: 200, Action: "POST /api/v1/regions", Method: "POST", Path: "/api/v1/regions", Status: 201, Operator: "op-2"}),
		line(t, Entry{Ts: 300, Action: "GET /api/v1/regions", Method: "GET", Path: "/api/v1/regions", Status: 200, Operator: "op-1"}),
	})
	// 全量（损坏行不计）：降序 [300,200,100]
	entries, total, err := l.Query(QueryFilter{}, 0, 0)
	if err != nil || total != 3 || len(entries) != 3 {
		t.Fatalf("全量查询不符: total=%d err=%v", total, err)
	}
	if entries[0].Ts != 300 || entries[2].Ts != 100 {
		t.Fatalf("降序不符: %d,%d,%d", entries[0].Ts, entries[1].Ts, entries[2].Ts)
	}
	// operator 精确
	if _, total, _ = l.Query(QueryFilter{Operator: "op-1"}, 0, 0); total != 2 {
		t.Fatalf("operator 过滤不符: %d", total)
	}
	// status 精确
	if _, total, _ = l.Query(QueryFilter{Status: 201}, 0, 0); total != 1 {
		t.Fatalf("status 过滤不符: %d", total)
	}
	// method 精确（大小写敏感）
	if _, total, _ = l.Query(QueryFilter{Method: "POST"}, 0, 0); total != 1 {
		t.Fatalf("method 过滤不符: %d", total)
	}
	if _, total, _ = l.Query(QueryFilter{Method: "post"}, 0, 0); total != 0 {
		t.Fatalf("method 小写应不命中: %d", total)
	}
	// pathSub 子串
	if _, total, _ = l.Query(QueryFilter{PathSub: "/regions"}, 0, 0); total != 2 {
		t.Fatalf("pathSub 过滤不符: %d", total)
	}
	// 时间窗（含边界）
	if _, total, _ = l.Query(QueryFilter{FromMs: 200, ToMs: 300}, 0, 0); total != 2 {
		t.Fatalf("时间窗过滤不符: %d", total)
	}
}

// US-2 分页：limit/offset 作用于降序序列；limit 钳 [1,500]；越界返回空非 nil。
func TestV0560QueryPagination(t *testing.T) {
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, line(t, Entry{Ts: int64(i * 100), Action: "GET /x", Method: "GET", Path: "/x", Status: 200, Operator: "op-1"}))
	}
	l := writeLedger(t, lines)
	page, total, err := l.Query(QueryFilter{}, 3, 0)
	if err != nil || total != 10 || len(page) != 3 || page[0].Ts != 1000 || page[2].Ts != 800 {
		t.Fatalf("第一页不符: total=%d err=%v page0=%d", total, err, page[0].Ts)
	}
	page, _, _ = l.Query(QueryFilter{}, 3, 9)
	if len(page) != 1 || page[0].Ts != 100 {
		t.Fatalf("末页不符: %+v", page)
	}
	page, total, _ = l.Query(QueryFilter{}, 3, 10)
	if total != 10 || len(page) != 0 || page == nil {
		t.Fatalf("越界应返回空切片非 nil: total=%d page=%v", total, page)
	}
	page, _, _ = l.Query(QueryFilter{}, 1000, 0)
	if len(page) != 10 {
		t.Fatalf("limit 应钳 500（10 条全回）: %d", len(page))
	}
}

// US-3 聚合：总数/按操作者/按动作/按状态桶。
func TestV0560Stats(t *testing.T) {
	l := writeLedger(t, []string{
		line(t, Entry{Ts: 100, Action: "GET /api/v1/nodes", Method: "GET", Path: "/api/v1/nodes", Status: 200, Operator: "op-1"}),
		line(t, Entry{Ts: 200, Action: "POST /api/v1/regions", Method: "POST", Path: "/api/v1/regions", Status: 201, Operator: "op-2"}),
		line(t, Entry{Ts: 300, Action: "GET /api/v1/nope", Method: "GET", Path: "/api/v1/nope", Status: 404, Operator: "op-1"}),
		line(t, Entry{Ts: 400, Action: "GET /api/v1/nodes", Method: "GET", Path: "/api/v1/nodes", Status: 200, Operator: "anonymous"}),
	})
	st, err := l.Stats(QueryFilter{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Total != 4 || st.ByOperator["op-1"] != 2 || st.ByOperator["op-2"] != 1 || st.ByOperator["anonymous"] != 1 {
		t.Fatalf("聚合不符: %+v", st)
	}
	if st.ByStatus["2xx"] != 3 || st.ByStatus["4xx"] != 1 {
		t.Fatalf("状态桶不符: %+v", st.ByStatus)
	}
	if st.ByAction["GET /api/v1/nodes"] != 2 {
		t.Fatalf("动作计数不符: %+v", st.ByAction)
	}
	// 过滤语义与 Query 一致
	st, err = l.Stats(QueryFilter{Operator: "op-1"})
	if err != nil || st.Total != 2 || st.ByStatus["4xx"] != 1 {
		t.Fatalf("过滤聚合不符: %+v err=%v", st, err)
	}
}
