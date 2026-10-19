package e2e

// v0.48.0 日志链路 e2e（spec 0021，G32 采集 + G11 运维日志查看）：
//   - US-1 采集上行全链：文件 tail → LogSync → 云端 logstore → 检索 API 可见
//     （tail/概要/节点清单三形态）；
//   - US-2 断网不丢：停云 → 采集续跑（LogSync 入本地补传队列）→ 同端口同数据
//     重启云 → 补传续传 → 日志可检索（v0.39 补传语义对日志消息的同构验证）；
//   - US-3 检索过滤：level/source/关键字/时间窗/分页；
//   - US-4 轮转：节点环形容量淘汰（最旧先丢）+ DELETE 清空；
//   - US-5 性能预算：存量批注入后单查询 < 2s（spec US-4 宽松断言）。
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

// v0480StartEdgecore 启动 edgecore（日志用例专用）：开启补传（断网不丢语义）
// + 开启日志采集（文件源指向用例目录；批量阈值 8 条便于快速成批）。
func v0480StartEdgecore(t *testing.T, root, nodeID string, hubPort int, logDir string) *proc {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "edgecore.db")
	cloudAddr := "ws://127.0.0.1:" + strconv.Itoa(hubPort)
	env := edgeEnv(nodeID, cloudAddr, dbPath)
	for i, kv := range env {
		switch {
		case strings.HasPrefix(kv, "EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL="):
			env[i] = "EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=1s"
		case strings.HasPrefix(kv, "EDGEFLOW_EDGECORE_RECONCILE_INTERVAL="):
			env[i] = "EDGEFLOW_EDGECORE_RECONCILE_INTERVAL=1s"
		}
	}
	env = append(env,
		"EDGEFLOW_EDGECORE_UPLINK=on",
		"EDGEFLOW_EDGECORE_UPLINK_REPORT_SEC=1",
		"EDGEFLOW_EDGECORE_UPLINK_BATCH=16",
		"EDGEFLOW_EDGECORE_UPLINK_RATE=50",
		"EDGEFLOW_EDGED_LOGCOLLECTOR=on",
		"EDGEFLOW_EDGED_LOG_DIR="+logDir,
		"EDGEFLOW_EDGED_LOG_BATCH=200",
		"EDGEFLOW_EDGED_LOG_FLUSH_SEC=2",
	)
	return startProcess(t, "edgecore-"+nodeID, filepath.Join(binDir, "edgecore"), nil, env)
}

// v0480StartCloudcore 启动 cloudcore（日志用例专用）：节点环形容量压小（8 条）
// 供轮转断言；其余同基线。
func v0480StartCloudcore(t *testing.T, root string, httpPort, hubPort int) *proc {
	t.Helper()
	prev := os.Getenv("EDGEFLOW_CLOUDCORE_LOGSTORE_MAX")
	os.Setenv("EDGEFLOW_CLOUDCORE_LOGSTORE_MAX", "3000") // 节点环形容量（复核 P2-4：真实容量下测轮转与性能预算）
	t.Cleanup(func() {
		if prev == "" {
			os.Unsetenv("EDGEFLOW_CLOUDCORE_LOGSTORE_MAX")
		} else {
			os.Setenv("EDGEFLOW_CLOUDCORE_LOGSTORE_MAX", prev)
		}
	})
	return startCloudcoreOnPorts(t, root, httpPort, hubPort)
}

// v0480WaitLog 轮询检索直到条件满足。
func v0480WaitLog(t *testing.T, base, query string, cond func(v0480LogList) bool, timeout time.Duration) v0480LogList {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last v0480LogList
	for time.Now().Before(deadline) {
		last = v0480Query(t, base, query)
		if cond(last) {
			return last
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("等待日志条件超时（%s；最近 %d 条）", query, len(last.Entries))
	return last
}

type v0480LogList struct {
	Entries []struct {
		Ts      int64  `json:"ts"`
		Source  string `json:"source"`
		Level   string `json:"level"`
		Message string `json:"message"`
	} `json:"entries"`
}

func v0480Query(t *testing.T, base, query string) v0480LogList {
	t.Helper()
	status, body, _ := v0470Do(t, http.MethodGet, base+"/api/v1/logs"+query, nil)
	if status != http.StatusOK {
		t.Fatalf("日志查询 %s → %d body=%s", query, status, body)
	}
	var out v0480LogList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析日志响应失败: %v body=%s", err, body)
	}
	return out
}

// TestV0480LogPipelineE2E 验证 日志采集→上行→检索 全链 + 断网不丢 + 过滤 + 轮转。
func TestV0480LogPipelineE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	cloud := v0480StartCloudcore(t, root, httpPort, hubPort)
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID := "e2e-v048-1"

	logDir := t.TempDir()
	edge := v0480StartEdgecore(t, root, nodeID, hubPort, logDir)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册（日志采集开启，源目录 %s）", nodeID, logDir)

	appLog := filepath.Join(logDir, "app.log")

	// ── US-1 采集上行全链 ──
	f, err := os.Create(appLog)
	if err != nil {
		t.Fatalf("创建日志源失败: %v", err)
	}
	fmt.Fprintln(f, "[INFO] service v048 started")
	fmt.Fprintln(f, "[ERROR] disk almost full")
	fmt.Fprintln(f, "[WARN] slow query detected")
	fmt.Fprintln(f, "plain line no level")
	f.Close()

	v0480WaitLog(t, base, "?node="+nodeID, func(l v0480LogList) bool {
		return len(l.Entries) >= 4
	}, 20*time.Second)
	got := v0480Query(t, base, "?node="+nodeID+"&limit=100")
	// 尾部 4 条应为我们写入的行（采集启动从 EOF：之前设备上报不产生文件日志）
	n := len(got.Entries)
	tail4 := got.Entries[n-4:]
	if tail4[0].Message != "service v048 started" || tail4[0].Level != "info" || tail4[0].Source != "app" {
		t.Fatalf("US-1 [INFO] 行错误: %+v", tail4[0])
	}
	if tail4[1].Level != "error" || tail4[2].Level != "warn" {
		t.Fatalf("US-1 级别推断错误: %+v %+v", tail4[1], tail4[2])
	}
	if tail4[3].Level != "info" || tail4[3].Message != "plain line no level" {
		t.Fatalf("US-1 无前缀行错误: %+v", tail4[3])
	}
	t.Logf("US-1 全链绿（4 条采集→检索可见；ts/source/level 口径正确）")

	// tail/概要/节点清单三形态
	// tail
	status, body, _ := v0470Do(t, http.MethodGet, base+"/api/v1/logs/nodes/"+nodeID+"/tail?n=2", nil)
	if status != http.StatusOK {
		t.Fatalf("tail → %d body=%s", status, body)
	}
	var tl v0480LogList
	_ = json.Unmarshal([]byte(body), &tl)
	if len(tl.Entries) != 2 {
		t.Fatalf("tail?n=2 应 2 条: %d", len(tl.Entries))
	}
	// 概要
	status, body, _ = v0470Do(t, http.MethodGet, base+"/api/v1/logs/nodes/"+nodeID, nil)
	if status != http.StatusOK {
		t.Fatalf("概要 → %d body=%s", status, body)
	}
	if !strings.Contains(body, `"sources"`) || !strings.Contains(body, `"app"`) {
		t.Fatalf("概要缺 sources/app: %s", body)
	}
	// 节点清单
	status, body, _ = v0470Do(t, http.MethodGet, base+"/api/v1/logs/nodes", nil)
	if status != http.StatusOK || !strings.Contains(body, nodeID) {
		t.Fatalf("节点清单应含 %s: %d %s", nodeID, status, body)
	}
	t.Logf("US-1 tail/概要/节点清单三形态绿")

	// ── US-3 检索过滤 ──
	if l := v0480Query(t, base, "?node="+nodeID+"&level=error"); len(l.Entries) == 0 {
		t.Fatalf("US-3 level=error 应非空")
	}
	if l := v0480Query(t, base, "?node="+nodeID+"&q=DISK"); len(l.Entries) == 0 {
		t.Fatalf("US-3 关键字（大小写不敏感）应非空")
	}
	if l := v0480Query(t, base, "?node="+nodeID+"&source=app&limit=2&offset=1"); len(l.Entries) != 2 {
		t.Fatalf("US-3 分页应 2 条: %d", len(l.Entries))
	}
	t.Logf("US-3 过滤绿（level/关键字/分页）")

	// ── US-2 断网不丢：停云 → 采集续跑 → 重启云 → 补传可检索 ──
	cloud.stop()
	time.Sleep(500 * time.Millisecond)

	f2, _ := os.OpenFile(appLog, os.O_APPEND|os.O_WRONLY, 0600)
	fmt.Fprintln(f2, "[ERROR] offline marker alpha")
	fmt.Fprintln(f2, "[ERROR] offline marker beta")
	fmt.Fprintln(f2, "[WARN] offline marker gamma")
	f2.Close()
	t.Logf("已停云；离线窗口写入 3 条（走本地补传队列）")

	cloud2 := startCloudcoreOnPorts(t, root, httpPort, hubPort)
	defer cloud2.stop()
	waitNodeRegistered(t, base, nodeID)

	offlineSeen := v0480WaitLog(t, base, "?node="+nodeID+"&q=offline", func(l v0480LogList) bool {
		return len(l.Entries) >= 3
	}, 30*time.Second)
	if len(offlineSeen.Entries) < 3 {
		t.Fatalf("US-2 断网窗口日志应补传可见: %d", len(offlineSeen.Entries))
	}
	t.Logf("US-2 断网不丢绿（停云窗口 3 条经补传队列全量到达）")

	// ── US-4 轮转（节点环形容量 3000：注入 3010 行 → 总量封顶、最旧被淘汰；
	// 复核 P2-4：真实容量下验证环形淘汰语义）──
	f3, _ := os.OpenFile(appLog, os.O_APPEND|os.O_WRONLY, 0600)
	for i := 0; i < 3010; i++ {
		fmt.Fprintf(f3, "[INFO] rotate fill %04d\n", i)
	}
	f3.Close()
	var rotateTotal int
	deadline4 := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline4) {
		_, _, hdr := v0470Do(t, http.MethodGet, base+"/api/v1/logs?node="+nodeID+"&q=rotate&limit=1", nil)
		rotateTotal, _ = strconv.Atoi(hdr.Get("X-Total-Count"))
		if rotateTotal >= 2900 {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	allTotal, _ := strconv.Atoi(func() string {
		_, _, h := v0470Do(t, http.MethodGet, base+"/api/v1/logs?node="+nodeID+"&limit=1", nil)
		return h.Get("X-Total-Count")
	}())
	if rotateTotal < 2900 {
		t.Fatalf("US-4 rotate 注入应基本齐全（≥2900，burst 同步 flush 后不得吞行）: %d", rotateTotal)
	}
	if allTotal > 3000 {
		t.Fatalf("US-4 环形容量应封顶 3000: %d", allTotal)
	}
	// 淘汰方向断言需静默锚点：fill 3009（最新）到达后再断言 0000（最旧）被淘汰
	// （run6 教训：等待条件满足时尾部批次仍在途——过早断言方向恰好反转）
	t9 := 0
	deadline4b := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline4b) {
		_, _, h9 := v0470Do(t, http.MethodGet, base+"/api/v1/logs?node="+nodeID+"&q=fill%203009", nil)
		t9, _ = strconv.Atoi(h9.Get("X-Total-Count"))
		if t9 == 1 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if t9 != 1 {
		t.Fatalf("US-4 最新行 fill 3009 应到达: %d", t9)
	}
	_, _, hdr0 := v0470Do(t, http.MethodGet, base+"/api/v1/logs?node="+nodeID+"&q=fill%200000", nil)
	t0, _ := strconv.Atoi(hdr0.Get("X-Total-Count"))
	if t0 != 0 {
		t.Fatalf("US-4 最旧行 fill 0000 应被环形淘汰: %d", t0)
	}
	// DELETE 清空
	req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/logs/nodes/"+nodeID, nil)
	respDel, err := http.DefaultClient.Do(req)
	if err != nil || respDel.StatusCode != http.StatusOK {
		t.Fatalf("US-4 DELETE → %v %d", err, respDel.StatusCode)
	}
	respDel.Body.Close()
	after := v0480Query(t, base, "?node="+nodeID)
	if len(after.Entries) != 0 {
		t.Fatalf("US-4 清空后应 0 条: %d", len(after.Entries))
	}
	t.Logf("US-4 轮转绿（环形封顶 3000 + 最旧淘汰 + DELETE 清空）")

	// ── US-5 性能预算（复核 P2-4：真实存量 ~3000 条环上单查询 < 2s）──
	f4, _ := os.OpenFile(appLog, os.O_APPEND|os.O_WRONLY, 0600)
	for i := 0; i < 10; i++ {
		for j := 0; j < 200; j++ {
			fmt.Fprintf(f4, "[INFO] perf probe line %03d-%03d payload padding padding\n", i, j)
		}
	}
	f4.Close()
	deadline5 := time.Now().Add(45 * time.Second)
	var perfTotal int
	for time.Now().Before(deadline5) {
		_, _, hdr := v0470Do(t, http.MethodGet, base+"/api/v1/logs?node="+nodeID+"&q=perf&limit=1", nil)
		perfTotal, _ = strconv.Atoi(hdr.Get("X-Total-Count"))
		if perfTotal >= 1950 {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	if perfTotal < 1950 {
		t.Fatalf("US-5 perf 注入应基本齐全（≥1950）: %d", perfTotal)
	}
	qStart := time.Now()
	res := v0480Query(t, base, "?node="+nodeID+"&q=perf&limit=1000")
	elapsed := time.Since(qStart)
	if elapsed > 2*time.Second {
		t.Fatalf("US-5 查询超预算（%s > 2s）", elapsed)
	}
	if len(res.Entries) != 1000 {
		t.Fatalf("US-5 limit=1000 应满页: %d", len(res.Entries))
	}
	t.Logf("US-5 性能预算绿（perf 存量 %d 条，查询 %s）", perfTotal, elapsed)

	_ = edge // edge 进程由 startProcess 的 t.Cleanup 统一回收
}
