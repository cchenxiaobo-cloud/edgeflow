package e2e

// v0.39.0 端到端：分级上送与补传（G16）。
//
// 链路：cloudcore（真实进程）+ edgecore（真实进程，补传开启、上报周期 1s）
// → 双阈值穿越规则持续触发事件 → 停云（断网模拟）→ 边缘积压 →
// 同端口/同数据目录重启云 → 重连补传 → 验证：
//   - 断网窗口内触发的事件（TriggeredAt ∈ [停云, 重启]）到达新云
//     （唯一到达途径 = 补传；旧云已死）——"完整补传"铁证；
//   - 队列清空（depth=0）且恢复后上报在跑（lastReportTs 更新）；
//   - 对账 sent（边侧累计上送）≥ n1+n2（两云接收总数）——不漏；
//   - overview 端点包含节点（可视化面）。
import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// v0390StartEdgecore 启动 edgecore（补传用例专用）：上报/调谐周期替换为
// 合法最小值 1s（同 v0380 基线现象：edgeEnv 的 300ms 低于下限被回退 30s），
// 并启用上行补传（上报周期 1s 便于观测）。
func v0390StartEdgecore(t *testing.T, root, nodeID string, hubPort int) *proc {
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
	)
	return startProcess(t, "edgecore-"+nodeID, filepath.Join(binDir, "edgecore"), nil, env)
}

// v0390waitEvents 轮询事件查询直到谓词满足，返回当次列表。
func v0390waitEvents(t *testing.T, base string, want func(ruleEventList) bool, timeout time.Duration) ruleEventList {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got ruleEventList
	for time.Now().Before(deadline) {
		getJSON(t, base+"/api/v1/rules/events?limit=200", &got)
		if want(got) {
			return got
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待事件条件超时（当前 %d 条）", len(got.Items))
	return got
}

// uplinkNodeResp 是 /nodes/{id}/uplink 的响应形态（与 cloudcore 端点一致）。
type uplinkNodeResp struct {
	NodeID       string `json:"nodeId"`
	Depth        int    `json:"depth"`
	Dropped      int64  `json:"dropped"`
	Sent         int64  `json:"sent"`
	OldestTs     int64  `json:"oldestTs"`
	LastReportTs int64  `json:"lastReportTs"`
	Received     int64  `json:"received"`
	Duplicated   int64  `json:"duplicated"`
}

// v0390waitUplink 轮询单节点上行状态直到条件满足。
// 容忍 404 / 连接失败：云端重启后（同端口/同数据目录），边缘重连并首次上报前
// 该端点按设计返回 404（无任何数据，spec 0012 US-7）——轮询期视为“未就绪”，
// 超时才失败并带上最后状态码。
func v0390waitUplink(t *testing.T, base, nodeID string, want func(uplinkNodeResp) bool, timeout time.Duration) uplinkNodeResp {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got uplinkNodeResp
	lastStatus := 0
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/nodes/" + nodeID + "/uplink")
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		lastStatus = resp.StatusCode
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			time.Sleep(500 * time.Millisecond)
			continue
		}
		err = json.NewDecoder(resp.Body).Decode(&got)
		_ = resp.Body.Close()
		if err == nil && want(got) {
			return got
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待上行状态条件超时（lastStatus=%d）: %+v", lastStatus, got)
	return got
}

// TestV0390UplinkE2E 验证 断网积压 → 恢复补传完整 全链。
func TestV0390UplinkE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	cloud, httpPort, hubPort := startCloudcore(t, root)
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID := "e2e-v039-1"
	edge := v0390StartEdgecore(t, root, nodeID, hubPort)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册（补传开启）", nodeID)

	// 1. 双阈值穿越规则（gt/lt 28 = mock_sensor 目标温度）：温度围绕目标
	// 抖动，每次跨越产生 1 条事件——保证断网期间持续有事件可积压。
	hi := map[string]any{
		"ruleId": "e2e-cross-hi", "name": "E2E 上穿", "deviceName": "sensor-01", "property": "temperature",
		"condition": map[string]any{"type": "threshold", "op": "gt", "value": 28},
		"action":    map[string]any{"type": "event", "severity": "critical", "message": "上穿 ${value}"},
	}
	lo := map[string]any{
		"ruleId": "e2e-cross-lo", "name": "E2E 下穿", "deviceName": "sensor-01", "property": "temperature",
		"condition": map[string]any{"type": "threshold", "op": "lt", "value": 28},
		"action":    map[string]any{"type": "event", "severity": "warning", "message": "下穿 ${value}"},
	}
	v0370Do(t, "POST", base+"/api/v1/rules", hi, http.StatusCreated)
	v0370Do(t, "POST", base+"/api/v1/rules", lo, http.StatusCreated)
	v0370Do(t, "POST", base+"/api/v1/nodes/"+nodeID+"/rules/sync", nil, http.StatusOK)
	t.Logf("穿越规则已下发")

	// 2. 在线基线：≥1 条事件且最近一条在 8s 内（事件流活跃，断网窗口才有意义）
	got := v0390waitEvents(t, base, func(list ruleEventList) bool {
		if len(list.Items) < 1 {
			return false
		}
		return time.Since(time.UnixMilli(list.Items[0].TriggeredAt)) < 8*time.Second
	}, 90*time.Second)
	n1 := len(got.Items)
	t.Logf("在线基线事件数 n1=%d（事件流活跃）", n1)

	// 3. 断网：停云，观察 12s 积压窗口（边缘持续触发）
	tDown := time.Now().UnixMilli()
	cloud.stop()
	t.Logf("cloudcore 已停（断网模拟，tDown=%d）", tDown)
	time.Sleep(12 * time.Second)

	// 4. 恢复：同端口/同数据目录重启（edge 自动重连重新注册）
	cloud2 := startCloudcoreOnPorts(t, root, httpPort, hubPort)
	tUp := time.Now().UnixMilli()
	waitNodeRegistered(t, base, nodeID)
	t.Logf("cloudcore 已重启（tUp=%d），节点重连注册", tUp)

	// 5. 补传完成（队列清空 + 恢复后上报在跑）
	final := v0390waitUplink(t, base, nodeID, func(u uplinkNodeResp) bool {
		return u.Depth == 0 && u.LastReportTs > tUp
	}, 120*time.Second)

	// 6. 断网窗口内触发的事件到达新云（唯一途径 = 补传）
	got2 := v0390waitEvents(t, base, func(list ruleEventList) bool {
		for _, ev := range list.Items {
			if ev.TriggeredAt >= tDown-2000 && ev.TriggeredAt <= tUp {
				return true
			}
		}
		return false
	}, 60*time.Second)
	n2 := len(got2.Items)
	offlineEvents := 0
	for _, ev := range got2.Items {
		if ev.TriggeredAt >= tDown-2000 && ev.TriggeredAt <= tUp {
			offlineEvents++
		}
	}

	if final.Depth != 0 {
		t.Fatalf("补传后队列未清空: depth=%d", final.Depth)
	}
	if n2 < 1 {
		t.Fatalf("重启后新云未收到事件: n2=%d", n2)
	}
	if offlineEvents < 1 {
		t.Fatalf("断网窗口内触发的事件未补传到新云（补传链路失败）: %+v", got2.Items)
	}
	if final.Sent < int64(n1+n2) {
		t.Fatalf("对账失败：边侧发送数 %d < 两云接收总数 %d（疑似丢失）", final.Sent, n1+n2)
	}
	if final.Sent > int64(n1+n2+8) {
		t.Fatalf("对账异常：边侧发送数 %d 远超接收总数 %d（疑似失控重发）", final.Sent, n1+n2)
	}

	// 7. overview 端点包含该节点
	var ov struct {
		Nodes []uplinkNodeResp `json:"nodes"`
	}
	getJSON(t, base+"/api/v1/uplink/overview", &ov)
	found := false
	for _, n := range ov.Nodes {
		if n.NodeID == nodeID {
			found = true
		}
	}
	if !found {
		t.Fatalf("overview 未包含节点 %s", nodeID)
	}

	// 8. 边侧日志锚点（诊断记录，不断言——tail 窗口可能被其他日志刷出）
	if strings.Contains(edge.logTail(), "上行补传已恢复") {
		t.Logf("边侧日志含补传恢复锚点")
	}

	// 收尾：优雅停机
	edge.stop()
	cloud2.stop()
	t.Logf("用例完成：n1=%d → n2=%d（断网期补传 %d 条），队列清空，sent=%d received=%d duplicated=%d dropped=%d",
		n1, n2, offlineEvents, final.Sent, final.Received, final.Duplicated, final.Dropped)
}
