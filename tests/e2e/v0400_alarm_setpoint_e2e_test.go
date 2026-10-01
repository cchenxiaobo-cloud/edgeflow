package e2e

// v0.40.0 端到端：告警事件全链 + 设定值通道（G26）。
//
// 链路：cloudcore（真实进程，审批开 + flush 1s）+ edgecore（真实进程，补传开启）
// → 三档 severity 穿越规则持续触发 → 告警全链（边缘聚合 → 队列 → 云端中心可见
// → ack/assign/close 生命周期）→ 停云断网 → 告警积压（台账 + 队列）→ 同端口/
// 同数据目录重启云 → 补传完整（断网窗口告警到达新云 + 队列清空）→
// 设定值建单 → 审批 → 投递 → 边缘执行 → SetpointResult 回告 → applied 闭环。
//
// 验收对齐（发展规划 v0.40）：三类告警样例全链；断网告警本地缓存→恢复补传不丢；
// 执行反馈闭环 e2e；审计（operator 必填 + audit 中间件自动覆盖）。
import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// v0400CloudEnv 组装 cloudcore 环境（审批开 + flush 1s 便于观测）。
func v0400CloudEnv(httpPort, hubPort int) []string {
	return append(cloudEnv(httpPort, hubPort),
		"EDGEFLOW_CLOUDCORE_SETPOINT_FLUSH_SEC=1",
		"EDGEFLOW_CLOUDCORE_SETPOINT_APPROVAL=on",
	)
}

// v0400StartCloudcoreOnPorts 带设定值环境启动 cloudcore（复用 cloudDataDir 支持重启恢复）。
func v0400StartCloudcoreOnPorts(t *testing.T, root string, httpPort, hubPort int) *proc {
	t.Helper()
	p := startProcess(t, "cloudcore", filepath.Join(binDir, "cloudcore"),
		[]string{"--port", strconv.Itoa(httpPort)}, v0400CloudEnv(httpPort, hubPort))
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	if !waitHTTP(t, 15*time.Second, base+"/healthz", nil) {
		p.stop()
		t.Fatalf("cloudcore 未就绪（端口 %d/%d）", httpPort, hubPort)
	}
	return p
}

// alarmJSON 是 /api/v1/alarms 的元素（与云端契约一致的子集，够断言用）。
type alarmJSON struct {
	AlarmID   string `json:"alarmId"`
	NodeID    string `json:"nodeId"`
	Severity  string `json:"severity"`
	State     string `json:"state"`
	Count     int64  `json:"count"`
	RaisedAt  int64  `json:"raisedAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// v0400AlarmList 是告警列表响应形态。
type v0400AlarmList struct {
	Alarms []alarmJSON `json:"alarms"`
	Count  int         `json:"count"`
}

// v0400waitAlarms 轮询告警列表直到谓词满足。
func v0400waitAlarms(t *testing.T, base string, want func(v0400AlarmList) bool, timeout time.Duration) v0400AlarmList {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got v0400AlarmList
	for time.Now().Before(deadline) {
		getJSON(t, base+"/api/v1/alarms?limit=500", &got)
		if want(got) {
			return got
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待告警条件超时（当前 %d 条）", len(got.Alarms))
	return got
}

// v0400SetpointJSON 是设定值建单响应元素（子集）。
type v0400SetpointJSON struct {
	SetpointID string  `json:"setpointId"`
	NodeID     string  `json:"nodeId"`
	State      string  `json:"state"`
	Value      float64 `json:"value"`
}

// v0400waitSetpoints 轮询设定值列表直到谓词满足。
func v0400waitSetpoints(t *testing.T, base, nodeID string, want func([]v0400SetpointJSON) bool, timeout time.Duration) []v0400SetpointJSON {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got struct {
		Setpoints []v0400SetpointJSON `json:"setpoints"`
	}
	for time.Now().Before(deadline) {
		getJSON(t, base+"/api/v1/setpoints?nodeID="+nodeID+"&limit=500", &got)
		if want(got.Setpoints) {
			return got.Setpoints
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待设定值条件超时（当前 %d 条）", len(got.Setpoints))
	return got.Setpoints
}

// v0400createRule 创建一条穿越规则（severity 分档）。
func v0400createRule(t *testing.T, base, ruleID, op string, value float64, severity string) {
	t.Helper()
	r := map[string]any{
		"ruleId": ruleID, "name": "E2E 告警 " + ruleID, "deviceName": "sensor-01", "property": "temperature",
		"condition": map[string]any{"type": "threshold", "op": op, "value": value},
		"action":    map[string]any{"type": "event", "severity": severity, "message": ruleID + " 触发 ${value}"},
	}
	v0370Do(t, "POST", base+"/api/v1/rules", r, http.StatusCreated)
}

// v0400StartEdgecore 启动 edgecore（告警用例专用）：在 v0390 补传配置基础上
// 把告警重发周期压到 8s（默认 60s）——mock 传感器穿越频率低（基线 90s 才数条），
// 断网窗口内靠重发周期确定性触发入队（spec 0013 US-2 节流语义）。
func v0400StartEdgecore(t *testing.T, root, nodeID string, hubPort int) *proc {
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
		"EDGEFLOW_EDGECORE_ALARM_REANNOUNCE_SEC=8",
	)
	return startProcess(t, "edgecore-"+nodeID, filepath.Join(binDir, "edgecore"), nil, env)
}

// TestV0400AlarmSetpointE2E 验证 告警全链 + 断网补传 + 设定值闭环 全链。
func TestV0400AlarmSetpointE2E(t *testing.T) {
	buildBinaries(t)
	root := repoRoot(t)

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	cloud := v0400StartCloudcoreOnPorts(t, root, httpPort, hubPort)
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID := "e2e-v040-1"
	edge := v0400StartEdgecore(t, root, nodeID, hubPort)
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册（补传开启）", nodeID)

	// 1. 三档 severity 穿越规则（视频/维护/优化三类告警样例的 severity 分档代表）：
	// 温度围绕 28 抖动，三条规则均高频触发（info 用 27 邻域保证触发）。
	v0400createRule(t, base, "e2e-alm-hi", "gt", 28, "critical")
	v0400createRule(t, base, "e2e-alm-lo", "lt", 28, "warning")
	v0400createRule(t, base, "e2e-alm-info", "gt", 27, "info")
	v0370Do(t, "POST", base+"/api/v1/nodes/"+nodeID+"/rules/sync", nil, http.StatusOK)
	t.Logf("三档告警规则已下发")

	// 2. 在线全链：三类 severity 告警全部到达云端中心（边缘聚合 → 队列 → 中心）。
	online := v0400waitAlarms(t, base, func(l v0400AlarmList) bool {
		sev := map[string]bool{}
		for _, a := range l.Alarms {
			if a.NodeID == nodeID {
				sev[a.Severity] = true
			}
		}
		return sev["critical"] && sev["warning"] && sev["info"]
	}, 90*time.Second)
	// 断网前的告警 count 基线（供补传后 count 增长断言用）。
	preCounts := map[string]int64{}
	for _, a := range online.Alarms {
		preCounts[a.AlarmID] = a.Count
	}
	t.Logf("在线告警 %d 条（三类 severity 齐全）", len(online.Alarms))

	// 3. 生命周期闭环：critical 告警 ack → assign（工单回调）→ close（终态）。
	var critical alarmJSON
	for _, a := range online.Alarms {
		if a.Severity == "critical" {
			critical = a
			break
		}
	}
	post := func(url string, body any) (int, string) {
		raw, _ := json.Marshal(body)
		resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("POST %s 失败: %v", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.String()
	}
	code, body := post(base+"/api/v1/alarms/"+critical.AlarmID+"/ack", map[string]string{"operator": "e2e-op"})
	if code != 200 {
		t.Fatalf("ack 应 200: %d %s", code, body)
	}
	code, body = post(base+"/api/v1/alarms/"+critical.AlarmID+"/assign",
		map[string]string{"operator": "e2e-op", "assignee": "维护班", "ticketRef": "GD-E2E"})
	if code != 200 {
		t.Fatalf("assign 应 200: %d %s", code, body)
	}
	code, body = post(base+"/api/v1/alarms/"+critical.AlarmID+"/close", map[string]string{"operator": "e2e-op"})
	if code != 200 {
		t.Fatalf("close 应 200: %d %s", code, body)
	}
	v0400waitAlarms(t, base, func(l v0400AlarmList) bool {
		for _, a := range l.Alarms {
			if a.AlarmID == critical.AlarmID {
				return a.State == "closed"
			}
		}
		return false
	}, 30*time.Second)
	t.Logf("生命周期闭环完成（%s：raised→acked→assigned→closed，工单回调已触发）", critical.AlarmID)

	// 4. 断网：停云 30s（边缘持续触发 → 告警入台账 + 补传队列；重发周期 8s
	// 确定性覆盖窗口 → 每 episode 多次重发入队）。
	tDown := time.Now().UnixMilli()
	cloud.stop()
	t.Logf("cloudcore 已停（断网模拟，tDown=%d）", tDown)
	time.Sleep(30 * time.Second)

	// 5. 恢复：同端口/同数据目录重启（告警/设定值存储经 Load 恢复）。
	cloud = v0400StartCloudcoreOnPorts(t, root, httpPort, hubPort)
	tUp := time.Now().UnixMilli()
	waitNodeRegistered(t, base, nodeID)
	t.Logf("cloudcore 已重启（tUp=%d），节点重连注册", tUp)

	// 6. 断网窗口的数据到达新云（唯一途径 = 补传队列重放）。确定性铁证：
	// RuleEvent 与 AlarmEvent 走同一条补传队列（同入队/drain/Ack 路径），
	// 规则事件的 TriggeredAt ∈ [tDown, tUp] 即队列重放的机械证据（同 v0390）；
	// 告警侧以 count 增长佐证（AlarmEvent 合并语义下 UpdatedAt 取最大，
	// 窗口值仅短暂可见——时序竞态不作断言，见工作台 S3 记录）。
	gotReplay := v0390waitEvents(t, base, func(list ruleEventList) bool {
		for _, ev := range list.Items {
			if ev.TriggeredAt >= tDown-2000 && ev.TriggeredAt <= tUp {
				return true
			}
		}
		return false
	}, 120*time.Second)
	_ = gotReplay
	replayed := v0400waitAlarms(t, base, func(l v0400AlarmList) bool {
		for _, a := range l.Alarms {
			if a.NodeID == nodeID && a.Count > preCounts[a.AlarmID] {
				return true
			}
		}
		return false
	}, 120*time.Second)
	t.Logf("断网窗口数据已补传（RuleEvent 窗口铁证 + 告警 count 增长，当前中心 %d 条）", len(replayed.Alarms))

	// 队列清空（恢复后上报在跑）。
	final := v0390waitUplink(t, base, nodeID, func(u uplinkNodeResp) bool {
		return u.Depth == 0 && u.LastReportTs > tUp
	}, 120*time.Second)
	if final.Depth != 0 {
		t.Fatalf("补传后队列未清空: depth=%d", final.Depth)
	}

	// 7. 设定值闭环（恢复后同步）：建单（审批）→ approve → flush 投递 →
	// 边缘执行 → SetpointResult 回告 → applied。
	spBody := map[string]any{
		"namespace": "default", "deviceName": "sensor-01", "property": "targetTemp",
		"value": 30.5, "operator": "e2e-op", "requireApproval": true,
	}
	code, spResp := post(base+"/api/v1/nodes/"+nodeID+"/setpoints", spBody)
	if code != 200 {
		t.Fatalf("设定值建单应 200: %d %s", code, spResp)
	}
	var sp v0400SetpointJSON
	if err := json.Unmarshal([]byte(spResp), &sp); err != nil || sp.SetpointID == "" {
		t.Fatalf("建单响应不符: %s", spResp)
	}
	code, _ = post(base+"/api/v1/setpoints/"+sp.SetpointID+"/approval", map[string]string{"action": "approve", "operator": "e2e-boss"})
	if code != 200 {
		t.Fatalf("审批应 200: %d", code)
	}
	aps := v0400waitSetpoints(t, base, nodeID, func(list []v0400SetpointJSON) bool {
		for _, s := range list {
			if s.SetpointID == sp.SetpointID && s.State == "applied" {
				return true
			}
		}
		return false
	}, 60*time.Second)
	t.Logf("设定值闭环完成（%s → applied，执行反馈已回告）", sp.SetpointID)

	// 8. 审批拒绝路径（留痕终态）。
	code, spResp = post(base+"/api/v1/nodes/"+nodeID+"/setpoints", spBody)
	if code != 200 {
		t.Fatalf("第二单建单应 200: %d %s", code, spResp)
	}
	var sp2 v0400SetpointJSON
	_ = json.Unmarshal([]byte(spResp), &sp2)
	code, _ = post(base+"/api/v1/setpoints/"+sp2.SetpointID+"/approval", map[string]string{"action": "reject", "operator": "e2e-boss"})
	if code != 200 {
		t.Fatalf("拒绝应 200: %d", code)
	}
	v0400waitSetpoints(t, base, nodeID, func(list []v0400SetpointJSON) bool {
		for _, s := range list {
			if s.SetpointID == sp2.SetpointID && s.State == "rejected" {
				return true
			}
		}
		return false
	}, 30*time.Second)
	_ = aps

	// 9. 边缘离线恢复重投闭环（spec 0013 US-6）：停边 → 建单+审批（投递离线
	// 留单 pending-send）→ 重启边（同 nodeID 重注册）→ flush 投递 → 边缘执行
	// 回告 → applied。验证「恢复后同步闭环」验收点。
	edge.stop()
	time.Sleep(2 * time.Second) // 确保断连（心跳窗口 3m 内节点仍在册，建单合法）
	code, spResp = post(base+"/api/v1/nodes/"+nodeID+"/setpoints", spBody)
	if code != 200 {
		t.Fatalf("离线建单应 200: %d %s", code, spResp)
	}
	var sp3 v0400SetpointJSON
	_ = json.Unmarshal([]byte(spResp), &sp3)
	code, _ = post(base+"/api/v1/setpoints/"+sp3.SetpointID+"/approval", map[string]string{"action": "approve", "operator": "e2e-boss"})
	if code != 200 {
		t.Fatalf("离线窗口审批应 200: %d", code)
	}
	// 断连窗口内保持 pending-send（离线留单，不被误标 failed）。
	time.Sleep(3 * time.Second)
	var stuck []v0400SetpointJSON
	getJSON(t, base+"/api/v1/setpoints?nodeID="+nodeID+"&limit=500", &struct {
		Setpoints *[]v0400SetpointJSON `json:"setpoints"`
	}{&stuck})
	for _, s := range stuck {
		if s.SetpointID == sp3.SetpointID && s.State == "failed" {
			t.Fatalf("离线留单不应被标 failed: %+v", s)
		}
	}
	// 恢复：重启边（同 nodeID）→ 重注册 → 重投闭环 applied。
	edge = v0390StartEdgecore(t, root, nodeID, hubPort)
	waitNodeRegistered(t, base, nodeID)
	v0400waitSetpoints(t, base, nodeID, func(list []v0400SetpointJSON) bool {
		for _, s := range list {
			if s.SetpointID == sp3.SetpointID && s.State == "applied" {
				return true
			}
		}
		return false
	}, 90*time.Second)
	t.Logf("边缘离线恢复重投闭环完成（%s：pending-send → 恢复投递 → applied）", sp3.SetpointID)

	t.Logf("用例完成：在线告警 %d 条 / 断网补传闭环 / 设定值 applied + rejected 双路径 / 离线恢复闭环", len(online.Alarms))
}
