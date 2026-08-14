package audit

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// readEntries 读取台账文件全部记录（按行解析 JSON）。
func readEntries(t *testing.T, path string) []Entry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	defer func() { _ = f.Close() }()
	var entries []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("解析台账行失败 %q: %v", string(line), err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读取台账失败: %v", err)
	}
	return entries
}

// TestNewLedgerCreatesFile 验证台账创建：父目录自动创建、文件可写、权限 0600。
func TestNewLedgerCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "audit-ledger.jsonl")
	l, err := NewLedger(path)
	if err != nil {
		t.Fatalf("NewLedger 失败: %v", err)
	}
	defer func() { _ = l.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("台账文件不存在: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("台账权限 = %o，期望 %o（含敏感信息应 0600）", perm, filePerm)
	}
}

// TestRecordAppends 验证 Record 追加 JSONL：两条记录两行，字段完整。
func TestRecordAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-ledger.jsonl")
	l, err := NewLedger(path)
	if err != nil {
		t.Fatalf("NewLedger 失败: %v", err)
	}
	if err := l.Record(Entry{
		Ts:       1000,
		Action:   "GET /api/v1/nodes",
		Path:     "/api/v1/nodes",
		Method:   "GET",
		Status:   200,
		Operator: "token",
		IP:       "127.0.0.1",
	}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}
	if err := l.Record(Entry{
		Ts:       1001,
		Action:   "GET /api/v1/nodes/{nodeID}",
		Path:     "/api/v1/nodes/n1",
		Method:   "GET",
		Status:   404,
		Operator: "anonymous",
		IP:       "127.0.0.1",
	}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}
	_ = l.Close()

	entries := readEntries(t, path)
	if len(entries) != 2 {
		t.Fatalf("记录数 = %d，期望 2", len(entries))
	}
	if entries[0].Status != 200 || entries[0].Operator != "token" || entries[0].Action != "GET /api/v1/nodes" {
		t.Errorf("第一条记录字段不符: %+v", entries[0])
	}
	if entries[1].Status != 404 || entries[1].Operator != "anonymous" {
		t.Errorf("第二条记录字段不符: %+v", entries[1])
	}
}

// TestMiddlewareRecordsEntry 验证中间件：每个请求落一条记录，状态码/方法/路径正确，
// 未认证时 operator=anonymous，IP 不含端口。
func TestMiddlewareRecordsEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-ledger.jsonl")
	l, err := NewLedger(path)
	if err != nil {
		t.Fatalf("NewLedger 失败: %v", err)
	}
	defer func() { _ = l.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(l.Middleware(mux))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/nodes/node-1")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_ = resp.Body.Close()

	entries := readEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("记录数 = %d，期望 1", len(entries))
	}
	e := entries[0]
	if e.Method != "GET" {
		t.Errorf("method = %q，期望 GET", e.Method)
	}
	if e.Path != "/api/v1/nodes/node-1" {
		t.Errorf("path = %q，期望 /api/v1/nodes/node-1", e.Path)
	}
	if e.Action != "GET /api/v1/nodes/{nodeID}" {
		t.Errorf("action = %q，期望路由模式 GET /api/v1/nodes/{nodeID}", e.Action)
	}
	if e.Status != http.StatusNotFound {
		t.Errorf("status = %d，期望 404", e.Status)
	}
	if e.Operator != IdentityAnonymous {
		t.Errorf("operator = %q，期望 anonymous（未认证）", e.Operator)
	}
	if e.IP == "" || e.IP == "127.0.0.1:12345" {
		t.Errorf("ip = %q，期望不含端口", e.IP)
	}
	if e.Ts <= 0 {
		t.Errorf("ts = %d，期望毫秒时间戳", e.Ts)
	}
}

// TestMiddlewareIdentityFromSetter 验证身份槽机制：内层 handler（模拟认证中间件）
// 调用 SetIdentity 后，审计记录 operator 为写入值（认证通过的调用方）。
func TestMiddlewareIdentityFromSetter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-ledger.jsonl")
	l, err := NewLedger(path)
	if err != nil {
		t.Fatalf("NewLedger 失败: %v", err)
	}
	defer func() { _ = l.Close() }()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟认证中间件：校验通过后写入身份
		SetIdentity(r.Context(), "token")
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req.RemoteAddr = "10.0.0.8:5678"
	l.Middleware(next).ServeHTTP(rec, req)

	entries := readEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("记录数 = %d，期望 1", len(entries))
	}
	if entries[0].Operator != "token" {
		t.Errorf("operator = %q，期望 token（认证中间件写入）", entries[0].Operator)
	}
	if entries[0].IP != "10.0.0.8" {
		t.Errorf("ip = %q，期望 10.0.0.8（不含端口）", entries[0].IP)
	}
	if entries[0].Status != http.StatusOK {
		t.Errorf("status = %d，期望 200", entries[0].Status)
	}
}

// TestMiddlewareNilLedger 验证 nil 台账防御：退化为透传不 panic。
func TestMiddlewareNilLedger(t *testing.T) {
	var l *Ledger
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	l.Middleware(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("nil 台账状态码 = %d，期望 200（透传）", rec.Code)
	}
}

// TestReopenAppends 验证重启不丢记录：关闭后重新打开，继续追加而非覆盖。
func TestReopenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-ledger.jsonl")
	l, err := NewLedger(path)
	if err != nil {
		t.Fatalf("NewLedger 失败: %v", err)
	}
	if err := l.Record(Entry{Ts: 1, Action: "first"}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}
	_ = l.Close()

	l2, err := NewLedger(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer func() { _ = l2.Close() }()
	if err := l2.Record(Entry{Ts: 2, Action: "second"}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}
	_ = l2.Close()

	entries := readEntries(t, path)
	if len(entries) != 2 {
		t.Fatalf("记录数 = %d，期望 2（重启后应追加而非覆盖）", len(entries))
	}
}
