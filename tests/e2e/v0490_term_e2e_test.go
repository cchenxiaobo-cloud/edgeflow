// v0.49.0 远程运维终端 e2e（spec 0022，G31+G11）。
//
// 验收锚（replan L204）：越权拒绝 + 审计完整。
//   - US-1 全链：operator 创建会话 → 执行白名单命令 → 结果回传 → 关闭 → 关闭后 409；
//   - US-2 越权拒绝：viewer create/exec/close 403；未认证 401；
//   - US-3 审计完整：create/exec/close 三动作审计可查（X-Total-Count 语义）；
//   - US-4 白名单：集外命令 rejected=true 不执行；集内 df 正常；
//   - US-5 超时：sleep 超过 timeoutSec 被终止返回 timeout（等待窗口内）；
//   - US-6 会话管理：列表过滤 + per-node 上限 429。
//
// 环境约定：RBAC on（凭证文件预置 boot-admin/op-1/view-1）；边缘
// EDGEFLOW_EDGED_TERM=on + 白名单含 uname/df/sleep/echo。
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// v0490TermDo 带 Bearer 的请求辅助（status, body）。
func v0490TermDo(t *testing.T, method, url, token string, body any) (int, string) {
	return v0460Do(t, method, url, token, body)
}

// v0490StartCloudcore：RBAC on + 终端审计落临时文件 + 小会话上限（US-6 用 2/节点）。
func v0490StartCloudcore(t *testing.T, root string, httpPort, hubPort int, usersFile, termAudit string) *proc {
	t.Helper()
	env := append(v0400CloudEnv(httpPort, hubPort),
		"EDGEFLOW_CLOUDCORE_RBAC=on",
		"EDGEFLOW_CLOUDCORE_RBAC_FILE="+usersFile,
		"EDGEFLOW_CLOUDCORE_TERM_AUDIT_PATH="+termAudit,
		"EDGEFLOW_CLOUDCORE_TERM_MAX_PER_NODE=2",
		"EDGEFLOW_CLOUDCORE_TERM_MAX_GLOBAL=4",
	)
	p := startProcess(t, "cloudcore-v049", filepath.Join(binDir, "cloudcore"), nil, env)
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	// 就绪等待（嵌入式 etcd 启动 ~10s；裸 startProcess 不等——waitNodeRegistered
	// 首发请求会 refused，run1 教训）。
	if !waitHTTP(t, 30*time.Second, base+"/healthz", nil) {
		p.stop()
		t.Fatalf("cloudcore-v049 未就绪（端口 %d/%d）", httpPort, hubPort)
	}
	return p
}

// v0490StartEdgecore：终端执行器开启 + 白名单扩展（sleep/echo 供超时与近界）。
func v0490StartEdgecore(t *testing.T, root, nodeID string, hubPort int, auditPath string) *proc {
	t.Helper()
	env := append(edgeEnv(nodeID, "ws://127.0.0.1:"+strconv.Itoa(hubPort), filepath.Join(t.TempDir(), "edge.db")),
		"EDGEFLOW_EDGED_TERM=on",
		"EDGEFLOW_EDGED_TERM_ALLOWLIST=uname,uptime,df,free,ps,sleep,echo",
		"EDGEFLOW_EDGED_TERM_AUDIT_PATH="+auditPath,
	)
	return startProcess(t, "edgecore-"+nodeID, filepath.Join(binDir, "edgecore"), nil, env)
}

type v0490Session struct {
	ID       string `json:"id"`
	NodeID   string `json:"nodeId"`
	Reason   string `json:"reason"`
	Operator string `json:"operator"`
	State    string `json:"state"`
}

type v0490TermResult struct {
	ExecID     string `json:"execId"`
	SessionID  string `json:"sessionId"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exitCode"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	Rejected   bool   `json:"rejected"`
	Reason     string `json:"reason"`
	DurationMs int64  `json:"durationMs"`
}

type v0490AuditList struct {
	Entries []struct {
		Ts        int64  `json:"ts"`
		Action    string `json:"action"`
		SessionID string `json:"sessionId"`
		NodeID    string `json:"nodeId"`
		Operator  string `json:"operator"`
		Cmd       string `json:"cmd"`
		Rejected  bool   `json:"rejected"`
	} `json:"entries"`
	Total int `json:"total"`
}

func TestV0490TermTerminalE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)

	// 凭证文件：admin / operator / viewer（v0460 同款预置）。
	usersFile := filepath.Join(t.TempDir(), "users.json")
	preUsers := `[{"id":"boot-admin","hash":"` + v0460Sha256("admin-pass-1") + `","role":"admin"},` +
		`{"id":"op-1","hash":"` + v0460Sha256("op-pass-1") + `","role":"operator"},` +
		`{"id":"view-1","hash":"` + v0460Sha256("view-pass-1") + `","role":"viewer"}]`
	if err := os.WriteFile(usersFile, []byte(preUsers), 0o600); err != nil {
		t.Fatalf("预写凭证文件: %v", err)
	}
	termAudit := filepath.Join(t.TempDir(), "term-audit.jsonl")

	cloud := v0490StartCloudcore(t, root, httpPort, hubPort, usersFile, termAudit)
	_ = cloud
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID := "e2e-v049-1"

	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"
	const adminToken = "admin-pass-1"

	edgeAudit := filepath.Join(t.TempDir(), "term-exec-audit.jsonl")
	edge := v0490StartEdgecore(t, root, nodeID, hubPort, edgeAudit)
	_ = edge
	// RBAC on：节点查询需认证——带 admin token 轮询（waitNodeRegistered 无认证版不适用）。
	{
		deadline := time.Now().Add(60 * time.Second)
		registered := false
		for time.Now().Before(deadline) {
			code, body := v0490TermDo(t, http.MethodGet, base+"/api/v1/nodes", adminToken, nil)
			if code == http.StatusOK && strings.Contains(body, nodeID) {
				registered = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !registered {
			t.Fatalf("节点 %s 60s 未注册", nodeID)
		}
	}
	t.Logf("节点 %s 已注册（终端执行器开启）", nodeID)

	// ── US-1 全链 ──
	var sess v0490Session
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions", opToken,
		map[string]string{"nodeId": nodeID, "reason": "e2e 巡检 US-1"})
	if code != http.StatusCreated {
		t.Fatalf("US-1 创建会话 → %d body=%s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &sess); err != nil || sess.ID == "" {
		t.Fatalf("US-1 会话响应解析失败: %v body=%s", err, body)
	}
	if sess.Operator != "op-1" || sess.State != "active" {
		t.Fatalf("US-1 会话字段不符: %+v", sess)
	}

	// exec uname -a → 结果回传（输出含内核标识；Darwin=macOS / Linux）。
	var res v0490TermResult
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+sess.ID+"/exec", opToken,
			map[string]any{"cmd": "uname", "args": []string{"-a"}, "timeoutSec": 10})
		if code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("US-1 exec 持续失败: %d body=%s", code, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("US-1 exec 响应解析失败: %v", err)
	}
	if res.Rejected || !res.OK || (!strings.Contains(res.Output, "Darwin") && !strings.Contains(res.Output, "Linux")) {
		t.Fatalf("US-1 uname 结果不符: %+v", res)
	}
	if res.ExitCode != 0 || res.DurationMs > 10000 {
		t.Fatalf("US-1 exit/时长不符: %+v", res)
	}

	// close → 200；关闭后再 exec → 409。
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+sess.ID+"/close", opToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-1 close → %d body=%s", code, body)
	}
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+sess.ID+"/exec", opToken,
		map[string]any{"cmd": "date", "timeoutSec": 5})
	if code != http.StatusConflict {
		t.Fatalf("US-1 关闭后 exec → %d（want 409）body=%s", code, body)
	}
	t.Logf("US-1 全链绿（创建→uname→关闭→409）")

	// ── US-2 越权拒绝 ──
	// viewer 创建/exec/close → 403。
	for _, tc := range []struct{ m, p string }{
		{http.MethodPost, "/api/v1/term/sessions"},
	} {
		if code, _ := v0490TermDo(t, tc.m, base+tc.p, viewToken,
			map[string]string{"nodeId": nodeID, "reason": "越权尝试"}); code != http.StatusForbidden {
			t.Fatalf("US-2 viewer create → %d（want 403）", code)
		}
	}
	// viewer 对已有会话 exec/close → 403（复核 P3-4：直接断言而非 RBAC 推定）。
	if code, _ := v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+sess.ID+"/exec", viewToken,
		map[string]any{"cmd": "date", "timeoutSec": 5}); code != http.StatusForbidden {
		t.Fatalf("US-2 viewer exec → %d（want 403）", code)
	}
	if code, _ := v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+sess.ID+"/close", viewToken, nil); code != http.StatusForbidden {
		t.Fatalf("US-2 viewer close → %d（want 403）", code)
	}
	// viewer 读审计/列表 → 200（read 面开放）。
	if code, _ := v0490TermDo(t, http.MethodGet, base+"/api/v1/term/audit", viewToken, nil); code != http.StatusOK {
		t.Fatalf("US-2 viewer 审计查询 → %d（want 200）", code)
	}
	// 未认证 → 401。
	if code, _ := v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions", "",
		map[string]string{"nodeId": nodeID, "reason": "匿名"}); code != http.StatusUnauthorized {
		t.Fatalf("US-2 未认证 create → %d（want 401）", code)
	}
	// 不存在会话 exec → 404。
	code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/ts-notexist/exec", opToken,
		map[string]any{"cmd": "date", "timeoutSec": 5})
	if code != http.StatusNotFound {
		t.Fatalf("US-2 未知会话 exec → %d（want 404）", code)
	}
	t.Logf("US-2 越权拒绝绿（viewer 写 403 / 匿名 401 / 未知会话 404）")

	// ── US-3 审计完整 ──
	var audit v0490AuditList
	deadline = time.Now().Add(15 * time.Second)
	for {
		code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/term/audit?nodeID="+nodeID+"&limit=200", opToken, nil)
		if code != http.StatusOK {
			t.Fatalf("US-3 审计查询 → %d body=%s", code, body)
		}
		audit = v0490AuditList{}
		if err := json.Unmarshal([]byte(body), &audit); err != nil {
			t.Fatalf("US-3 审计解析失败: %v", err)
		}
		hasCreate, hasExec, hasClose := false, false, false
		for _, e := range audit.Entries {
			switch e.Action {
			case "session-create":
				hasCreate = true
			case "exec":
				hasExec = hasExec || e.Cmd == "uname"
			case "session-close":
				hasClose = true
			}
		}
		if hasCreate && hasExec && hasClose {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("US-3 审计不全（create=%v exec=%v close=%v）", hasCreate, hasExec, hasClose)
		}
		time.Sleep(400 * time.Millisecond)
	}
	if audit.Total < 3 {
		t.Fatalf("US-3 审计 total=%d（want ≥3）", audit.Total)
	}
	t.Logf("US-3 审计完整绿（create/exec/close 三动作可查，total=%d）", audit.Total)

	// ── US-4 白名单 ──
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions", opToken,
		map[string]string{"nodeId": nodeID, "reason": "e2e 白名单 US-4"})
	if code != http.StatusCreated {
		t.Fatalf("US-4 建会话 → %d", code)
	}
	var s4 v0490Session
	_ = json.Unmarshal([]byte(body), &s4)
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+s4.ID+"/exec", opToken,
		map[string]any{"cmd": "reboot", "timeoutSec": 5})
	if code != http.StatusOK {
		t.Fatalf("US-4 集外命令 HTTP 层 → %d（want 200，业务语义在负载）", code)
	}
	res = v0490TermResult{}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("US-4 解析失败: %v", err)
	}
	if !res.Rejected || res.OK || !strings.Contains(res.Reason, "白名单") {
		t.Fatalf("US-4 集外命令应 rejected: %+v", res)
	}
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+s4.ID+"/exec", opToken,
		map[string]any{"cmd": "df", "args": []string{"-h"}, "timeoutSec": 10})
	res = v0490TermResult{}
	if err := json.Unmarshal([]byte(body), &res); err != nil || code != http.StatusOK {
		t.Fatalf("US-4 df → %d err=%v", code, err)
	}
	if !res.OK || !strings.Contains(res.Output, "Filesystem") && !strings.Contains(res.Output, "文件系统") && len(res.Output) == 0 {
		t.Fatalf("US-4 df 输出异常: %+v", res)
	}
	// 边侧本地审计（防篡改副账）已落盘。
	edgeData, err := os.ReadFile(edgeAudit)
	if err != nil || !strings.Contains(string(edgeData), "reboot") || !strings.Contains(string(edgeData), `"operator":"op-1"`) {
		t.Fatalf("US-4 边侧审计缺失或无端到端留痕: err=%v", err)
	}
	_, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+s4.ID+"/close", opToken, nil)
	t.Logf("US-4 白名单绿（reboot rejected + df 正常 + 边侧副账含 operator）")

	// ── US-5 超时终止 ──
	_, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions", opToken,
		map[string]string{"nodeId": nodeID, "reason": "e2e 超时 US-5"})
	var s5 v0490Session
	_ = json.Unmarshal([]byte(body), &s5)
	start := time.Now()
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+s5.ID+"/exec", opToken,
		map[string]any{"cmd": "sleep", "args": []string{"30"}, "timeoutSec": 2})
	elapsed := time.Since(start)
	res = v0490TermResult{}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("US-5 解析失败: %v", err)
	}
	if code != http.StatusOK || res.OK || !strings.Contains(res.Reason, "timeout") {
		t.Fatalf("US-5 超时结果不符: code=%d %+v", code, res)
	}
	// 等待窗口 = timeoutSec+5s；命令 2s 即被终止（边缘侧）+ 云端等待回传。
	if elapsed > 12*time.Second {
		t.Fatalf("US-5 超时路径耗时异常: %s", elapsed)
	}
	_, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+s5.ID+"/close", opToken, nil)
	t.Logf("US-5 超时终止绿（sleep 30 @2s 终止，耗时 %s）", elapsed)

	// ── US-6 会话管理（上限 2/节点）──
	var ids []string
	for i := 0; i < 2; i++ {
		code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions", opToken,
			map[string]string{"nodeId": nodeID, "reason": fmt.Sprintf("e2e 上限 US-6 #%d", i+1)})
		if code != http.StatusCreated {
			t.Fatalf("US-6 第 %d 个会话 → %d body=%s", i+1, code, body)
		}
		var s v0490Session
		_ = json.Unmarshal([]byte(body), &s)
		ids = append(ids, s.ID)
	}
	code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions", opToken,
		map[string]string{"nodeId": nodeID, "reason": "第 3 个应 429"})
	if code != http.StatusTooManyRequests {
		t.Fatalf("US-6 per-node 上限 → %d（want 429）", code)
	}
	// 关闭一个后配额释放。
	if code, _ := v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+ids[0]+"/close", opToken, nil); code != http.StatusOK {
		t.Fatalf("US-6 close → 非 200")
	}
	code, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions", opToken,
		map[string]string{"nodeId": nodeID, "reason": "释放后再建"})
	if code != http.StatusCreated {
		t.Fatalf("US-6 释放后再建 → %d（want 201）", code)
	}
	// 列表按节点过滤。
	code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/term/sessions?nodeID="+nodeID, adminToken, nil)
	if code != http.StatusOK {
		t.Fatalf("US-6 列表 → %d", code)
	}
	var list struct {
		Sessions []v0490Session `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("US-6 列表解析失败: %v", err)
	}
	// List 保留全部历史（US-1/4/5 已关 + US-6 两个 active）——设计行为：
	// 审计完整性优先，列表不做状态过滤（调用方按 state 自行筛选）。
	activeCount := 0
	for _, s := range list.Sessions {
		if s.State == "active" {
			activeCount++
		}
	}
	if len(list.Sessions) != 6 || activeCount != 2 {
		t.Fatalf("US-6 列表不符: total=%d active=%d body=%s", len(list.Sessions), activeCount, body)
	}
	t.Logf("US-6 会话管理绿（429 上限 + 释放重建 + 列表过滤）")

	// 清理：关闭剩余会话。
	for _, s := range list.Sessions {
		if s.State == "active" {
			_, _ = v0490TermDo(t, http.MethodPost, base+"/api/v1/term/sessions/"+s.ID+"/close", opToken, nil)
		}
	}
	t.Logf("v0.49.0 终端 e2e 全绿（验收锚：越权拒绝 + 审计完整）")
}
