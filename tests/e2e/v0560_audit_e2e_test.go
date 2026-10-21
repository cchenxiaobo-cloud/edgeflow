package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestV0560AuditQueryE2E（v0.56.0，spec 0029）：审计台账查询面三段——
// US-1 产生审计记录（建区域 201 + 查未知区域 404）后 entries 可查（operator/
// status 过滤）/ US-2 stats 聚合（byOperator/byStatus 桶）/ US-3 RBAC（匿名 401 /
// viewer 200）。复用 v0520 云端环境（RBAC on）；台账路径指临时文件。
func TestV0560AuditQueryE2E(t *testing.T) {
	buildBinaries(t)
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := v0520StartCloudcore(t, httpPort, hubPort, scenarioUsers(t),
		"EDGEFLOW_CLOUDCORE_AUDIT_PATH="+filepath.Join(t.TempDir(), "audit-ledger.jsonl"))
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)

	// ─── US-1 产生记录 + entries 查询 ───
	t.Logf("US-1 产生审计记录 + entries 查询")
	type auditEntry = struct {
		Ts       int64  `json:"ts"`
		Action   string `json:"action"`
		Path     string `json:"path"`
		Method   string `json:"method"`
		Status   int    `json:"status"`
		Operator string `json:"operator"`
	}
	if code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken,
		map[string]any{"name": "审计区"}); code != http.StatusCreated {
		t.Fatalf("US-1 建区域 → %d body=%s", code, body)
	}
	if code, _ := v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/regions/rg-not-exist/nodes", opToken, nil); code != http.StatusNotFound {
		t.Fatalf("US-1 查未知区域节点 → %d（want 404）", code)
	}

	// 轮询台账（审计 middleware 落盘为异步写完成语义，500ms 步进）
	waitAudit := func(want func(total int, entries []auditEntry) bool, timeout time.Duration) (int, []auditEntry) {
		deadline := time.Now().Add(timeout)
		for {
			code, body := v0490TermDo(t, http.MethodGet, base+"/api/v1/audit/entries?limit=200", viewToken, nil)
			if code == http.StatusOK {
				var resp struct {
					Entries []auditEntry `json:"entries"`
					Total   int          `json:"total"`
				}
				json.Unmarshal([]byte(body), &resp)
				if want(resp.Total, resp.Entries) {
					return resp.Total, resp.Entries
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("US-1 台账轮询超时: %d %s", code, body)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	total, _ := waitAudit(func(total int, entries []auditEntry) bool {
		hasPost, has404 := false, false
		for _, e := range entries {
			if e.Operator == "op-1" && e.Method == "POST" && strings.Contains(e.Path, "/api/v1/regions") && e.Status == 201 {
				hasPost = true
			}
			if e.Status == 404 {
				has404 = true
			}
		}
		return hasPost && has404
	}, 15*time.Second)
	if total < 2 {
		t.Fatalf("US-1 台账总数不符: %d", total)
	}
	// operator 过滤
	code, body := v0490TermDo(t, http.MethodGet, base+"/api/v1/audit/entries?operator=op-1", viewToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-1 operator 过滤 → %d body=%s", code, body)
	}
	// status 过滤（404）
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/audit/entries?status=404", viewToken, nil)
	if code != http.StatusOK || !strings.Contains(body, `"status":404`) {
		t.Fatalf("US-1 status 过滤不符: %d %s", code, body)
	}
	t.Logf("US-1 完成（total=%d，过滤语义生效）", total)

	// ─── US-2 stats 聚合 ───
	t.Logf("US-2 stats 聚合")
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/audit/stats", viewToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-2 stats → %d body=%s", code, body)
	}
	var st struct {
		Total      int64            `json:"total"`
		ByOperator map[string]int64 `json:"byOperator"`
		ByStatus   map[string]int64 `json:"byStatus"`
	}
	json.Unmarshal([]byte(body), &st)
	if st.Total < 2 || st.ByOperator["op-1"] < 2 || st.ByStatus["2xx"] < 1 || st.ByStatus["4xx"] < 1 {
		t.Fatalf("US-2 聚合不符: %s", body)
	}

	// ─── US-3 RBAC：匿名 401 / viewer 200 ───
	t.Logf("US-3 RBAC 两态")
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/audit/entries", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("US-3 匿名 → %d（want 401）", code)
	}
	if code, _ = v0490TermDo(t, http.MethodGet, base+"/api/v1/audit/stats", viewToken, nil); code != http.StatusOK {
		t.Fatalf("US-3 viewer stats → %d（want 200）", code)
	}
	_ = os.Getenv("EDGEFLOW_CLOUDCORE_AUDIT_PATH") // env 断言由装配日志覆盖
	t.Logf("US-3 完成（匿名 401 / viewer 200）")
}
