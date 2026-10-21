package e2e

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hmacHex 计算转发签名头期望值（X-EdgeFlow-Signature: sha256=<hex hmac>）。
func hmacHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// scenario_e2e_test.go（v0.54.0，spec 0027，G29）：三场景端到端验收——
// 场景 A 告警通知闭环（真告警 → 区域汇聚 → webhook 通知）/ 场景 B 维护预警闭环
// （预警 → ack → assign 工单 → close，区域矩阵三态）/ 场景 C 优化设定值闭环
// （建单审批 → 边缘执行 applied / 拒绝 rejected）。
// 全部走真实 cloudcore+edgecore 进程 + RBAC 环境；零 API 变更（契约 128 维持）。

// scenarioUsers 生成三凭证 users.json（三场景共用）。
func scenarioUsers(t *testing.T) string {
	t.Helper()
	usersFile := filepath.Join(t.TempDir(), "users.json")
	preUsers := `[{"id":"boot-admin","hash":"` + v0460Sha256("admin-pass-1") + `","role":"admin"},` +
		`{"id":"op-1","hash":"` + v0460Sha256("op-pass-1") + `","role":"operator"},` +
		`{"id":"view-1","hash":"` + v0460Sha256("view-pass-1") + `","role":"viewer"}]`
	if err := os.WriteFile(usersFile, []byte(preUsers), 0o600); err != nil {
		t.Fatal(err)
	}
	return usersFile
}

// scenarioWaitRegistered 等节点注册（RBAC on 环境：带 viewer token 轮询）。
func scenarioWaitRegistered(t *testing.T, base, nodeID, viewToken string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, body := v0490TermDo(t, http.MethodGet, base+"/api/v1/nodes", viewToken, nil)
		if code == http.StatusOK && strings.Contains(body, nodeID) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("节点 %s 60s 未注册: %d %s", nodeID, code, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitFwdEvent 从接收端通道等待指定事件投递。
func waitFwdEvent(t *testing.T, hooks chan fwdHookReq, event string, timeout time.Duration) fwdHookReq {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		select {
		case h := <-hooks:
			if h.Payload.Event == event {
				return h
			}
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待投递事件 %s 超时", event)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// scenarioCreateRegion 创建区域并归属节点。
func scenarioCreateRegion(t *testing.T, base, opToken, name, nodeID string) string {
	t.Helper()
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/regions", opToken, map[string]any{"name": name})
	if code != http.StatusCreated {
		t.Fatalf("建区域 %s → %d body=%s", name, code, body)
	}
	var reg struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(body), &reg)
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/regions/"+reg.ID+"/nodes", opToken,
		map[string]any{"nodeIds": []string{nodeID}}); code != http.StatusOK {
		t.Fatalf("归属 → %d body=%s", code, body)
	}
	return reg.ID
}

// scenarioCreateThresholdRule 创建阈值告警规则并下发节点。
func scenarioCreateThresholdRule(t *testing.T, base, opToken, nodeID, ruleID, op string, value float64, severity string) {
	t.Helper()
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/rules", opToken, map[string]any{
		"ruleId": ruleID, "name": "场景规则 " + ruleID, "deviceName": "sensor-01", "property": "temperature",
		"condition": map[string]any{"type": "threshold", "op": op, "value": value},
		"action":    map[string]any{"type": "event", "severity": severity, "message": ruleID + " 触发 ${value}"},
	})
	if code != http.StatusCreated {
		t.Fatalf("建规则 %s → %d body=%s", ruleID, code, body)
	}
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/nodes/"+nodeID+"/rules/sync", opToken, nil); code != http.StatusOK {
		t.Fatalf("规则下发 → %d body=%s", code, body)
	}
}

// scenarioAlarmWait 轮询告警列表直到谓词满足（RBAC on）。
func scenarioAlarmWait(t *testing.T, base, viewToken string, want func([]alarmJSON) bool, timeout time.Duration) []alarmJSON {
	t.Helper()
	var list struct {
		Alarms []alarmJSON `json:"alarms"`
	}
	deadline := time.Now().Add(timeout)
	for {
		code, body := v0490TermDo(t, http.MethodGet, base+"/api/v1/alarms?limit=500", viewToken, nil)
		if code == http.StatusOK {
			json.Unmarshal([]byte(body), &list)
			if want(list.Alarms) {
				return list.Alarms
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待告警条件超时（当前 %d 条）", len(list.Alarms))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// scenarioWaitSetpoint 轮询设定值状态（RBAC on）。
func scenarioWaitSetpoint(t *testing.T, base, viewToken, nodeID, setpointID, state string, timeout time.Duration) {
	t.Helper()
	var list struct {
		Setpoints []struct {
			SetpointID string `json:"setpointId"`
			State      string `json:"state"`
		} `json:"setpoints"`
	}
	deadline := time.Now().Add(timeout)
	for {
		code, body := v0490TermDo(t, http.MethodGet, base+"/api/v1/setpoints?nodeID="+nodeID+"&limit=500", viewToken, nil)
		if code == http.StatusOK {
			json.Unmarshal([]byte(body), &list)
			for _, sp := range list.Setpoints {
				if sp.SetpointID == setpointID && sp.State == state {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("设定值 %s 等待状态 %s 超时: %s", setpointID, state, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ─── 场景 A：告警通知闭环 ───
// 链路：阈值规则（critical gt 28）→ 真告警 → 区域汇聚（summary.alarmActiveCount）
// → 转发出口 webhook（HMAC 校验）。视频源等价声明见 spec 0027 裁决 1。
func TestScenarioAAlarmNotifyLoop(t *testing.T) {
	buildBinaries(t)
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"
	nodeID := "e2e-scen-a-1"

	hooks := make(chan fwdHookReq, 16)
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 8192)
		n, _ := r.Body.Read(body)
		req := fwdHookReq{Body: body[:n], Sig: r.Header.Get("X-EdgeFlow-Signature")}
		_ = json.Unmarshal(req.Body, &req.Payload)
		w.WriteHeader(http.StatusOK)
		hooks <- req
	}))
	defer rcv.Close()

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := v0520StartCloudcore(t, httpPort, hubPort, scenarioUsers(t),
		"EDGEFLOW_CLOUDCORE_FORWARD_FILE="+filepath.Join(t.TempDir(), "forward.json"))
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	edge := v0400StartEdgecore(t, t.TempDir(), nodeID, hubPort)
	defer edge.stop()
	scenarioWaitRegistered(t, base, nodeID, viewToken)

	// ① 区域归属 + ② 转发规则（通知系统等价物）+ ③ 阈值规则
	scenarioCreateRegion(t, base, opToken, "厂区A", nodeID)
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/forwards", opToken, map[string]any{
		"name": "场景A-通知", "targetUrl": rcv.URL, "secret": "scen-a-secret",
		"events": []string{"alarm.raised"}, "enabled": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("建转发规则 → %d body=%s", code, body)
	}
	scenarioCreateThresholdRule(t, base, opToken, nodeID, "scen-a-hi", "gt", 28, "critical")

	// ④ 真告警 → webhook 送达（通知闭环断言）
	hook := waitFwdEvent(t, hooks, "alarm.raised", 90*time.Second)
	if hook.Payload.NodeID != nodeID || hook.Payload.AlarmID == "" {
		t.Fatalf("场景A webhook payload 不符: %+v", hook.Payload)
	}
	sig := hmacHex("scen-a-secret", hook.Body)
	if hook.Sig != sig {
		t.Fatalf("场景A HMAC 签名校验失败: %s", hook.Sig)
	}

	// ⑤ 区域汇聚可见（summary.alarmActiveCount ≥ 1）
	var summary struct {
		Children []struct {
			Name             string `json:"name"`
			AlarmActiveCount int    `json:"alarmActiveCount"`
			NodeCount        int    `json:"nodeCount"`
		} `json:"children"`
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, body = v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/summary", viewToken, nil)
		if code != http.StatusOK {
			t.Fatalf("场景A summary → %d body=%s", code, body)
		}
		json.Unmarshal([]byte(body), &summary)
		hit := false
		for _, c := range summary.Children {
			if c.Name == "厂区A" && c.AlarmActiveCount >= 1 && c.NodeCount == 1 {
				hit = true
			}
		}
		if hit {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("场景A 区域汇聚 15s 未见告警: %s", body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("场景A 完成（告警 %s → 区域汇聚可见 → webhook 送达）", hook.Payload.AlarmID)
}

// ─── 场景 B：维护预警闭环 ───
// 链路：预警阈值（warning lt 28）→ 真告警 → ack → assign（维护班工单）→ close；
// 区域×状态告警矩阵三态迁移可见。
func TestScenarioBMaintenanceLoop(t *testing.T) {
	buildBinaries(t)
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"
	nodeID := "e2e-scen-b-1"

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := v0520StartCloudcore(t, httpPort, hubPort, scenarioUsers(t))
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	edge := v0400StartEdgecore(t, t.TempDir(), nodeID, hubPort)
	defer edge.stop()
	scenarioWaitRegistered(t, base, nodeID, viewToken)

	scenarioCreateRegion(t, base, opToken, "车间B", nodeID)
	scenarioCreateThresholdRule(t, base, opToken, nodeID, "scen-b-lo", "lt", 28, "warning")

	// ① 预警告警到达
	alarms := scenarioAlarmWait(t, base, viewToken, func(list []alarmJSON) bool {
		for _, a := range list {
			if a.NodeID == nodeID && a.Severity == "warning" && a.State == "raised" {
				return true
			}
		}
		return false
	}, 90*time.Second)
	var warn alarmJSON
	for _, a := range alarms {
		if a.NodeID == nodeID && a.Severity == "warning" {
			warn = a
			break
		}
	}
	t.Logf("场景B 预警到达（alarmId=%s）", warn.AlarmID)

	// 区域矩阵谓词：指定状态计数 ≥1
	matrixHas := func(state string) bool {
		var m struct {
			Regions []struct {
				RegionName string `json:"regionName"`
				Total      int    `json:"total"`
				Raised     int    `json:"raised"`
				Acked      int    `json:"acked"`
				Assigned   int    `json:"assigned"`
				Closed     int    `json:"closed"`
			} `json:"regions"`
		}
		code, body := v0490TermDo(t, http.MethodGet, base+"/api/v1/hierarchy/alarms", viewToken, nil)
		if code != http.StatusOK {
			return false
		}
		json.Unmarshal([]byte(body), &m)
		for _, r := range m.Regions {
			if r.RegionName == "车间B" {
				switch state {
				case "acked":
					return r.Acked >= 1
				case "assigned":
					return r.Assigned >= 1
				case "closed":
					return r.Closed >= 1
				}
			}
		}
		return false
	}
	matrixWait := func(state string, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			if matrixHas(state) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("场景B 矩阵 %s 态 %s 未见", "车间B", state)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// ② ack → ③ assign（维护班工单）→ ④ close，矩阵逐态可见
	if code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/alarms/"+warn.AlarmID+"/ack", opToken,
		map[string]string{"operator": "op-1"}); code != http.StatusOK {
		t.Fatalf("场景B ack → %d body=%s", code, body)
	}
	matrixWait("acked", 15*time.Second)
	if code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/alarms/"+warn.AlarmID+"/assign", opToken,
		map[string]any{"operator": "op-1", "assignee": "维护班", "ticketRef": "GD-SCEN-B"}); code != http.StatusOK {
		t.Fatalf("场景B assign → %d body=%s", code, body)
	}
	matrixWait("assigned", 15*time.Second)
	if code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/alarms/"+warn.AlarmID+"/close", opToken,
		map[string]string{"operator": "op-1"}); code != http.StatusOK {
		t.Fatalf("场景B close → %d body=%s", code, body)
	}
	matrixWait("closed", 15*time.Second)

	// ⑤ 终态确认（closed 不再活跃）
	scenarioAlarmWait(t, base, viewToken, func(list []alarmJSON) bool {
		for _, a := range list {
			if a.AlarmID == warn.AlarmID {
				return a.State == "closed"
			}
		}
		return false
	}, 10*time.Second)
	t.Logf("场景B 完成（预警 %s：ack→assign 工单 GD-SCEN-B→close，矩阵三态可见）", warn.AlarmID)
}

// ─── 场景 C：优化设定值闭环 ───
// 链路：设定值建单（requireApproval）→ approve → 边缘执行 → SetpointResult 回告
// applied；拒绝路径 → rejected 留痕。
func TestScenarioCSetpointLoop(t *testing.T) {
	buildBinaries(t)
	const opToken = "op-pass-1"
	const viewToken = "view-pass-1"
	nodeID := "e2e-scen-c-1"

	cloudDataDir = filepath.Join(t.TempDir(), "etcd")
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := v0520StartCloudcore(t, httpPort, hubPort, scenarioUsers(t))
	defer p.stop()
	base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	edge := v0400StartEdgecore(t, t.TempDir(), nodeID, hubPort)
	defer edge.stop()
	scenarioWaitRegistered(t, base, nodeID, viewToken)

	spBody := map[string]any{
		"namespace": "default", "deviceName": "sensor-01", "property": "targetTemp",
		"value": 26.5, "operator": "op-1", "requireApproval": true,
	}

	// ① 建单 + approve → 边缘执行 → applied
	code, body := v0490TermDo(t, http.MethodPost, base+"/api/v1/nodes/"+nodeID+"/setpoints", opToken, spBody)
	if code != http.StatusOK {
		t.Fatalf("场景C 建单 → %d body=%s", code, body)
	}
	var sp struct {
		SetpointID string `json:"setpointId"`
	}
	json.Unmarshal([]byte(body), &sp)
	if sp.SetpointID == "" {
		t.Fatalf("场景C 建单响应不符: %s", body)
	}
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/setpoints/"+sp.SetpointID+"/approval", opToken,
		map[string]string{"action": "approve", "operator": "boot-admin"}); code != http.StatusOK {
		t.Fatalf("场景C approve → %d body=%s", code, body)
	}
	scenarioWaitSetpoint(t, base, viewToken, nodeID, sp.SetpointID, "applied", 60*time.Second)

	// ② 拒绝路径（留痕终态 rejected）
	code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/nodes/"+nodeID+"/setpoints", opToken, spBody)
	if code != http.StatusOK {
		t.Fatalf("场景C 第二单 → %d body=%s", code, body)
	}
	var sp2 struct {
		SetpointID string `json:"setpointId"`
	}
	json.Unmarshal([]byte(body), &sp2)
	if code, body = v0490TermDo(t, http.MethodPost, base+"/api/v1/setpoints/"+sp2.SetpointID+"/approval", opToken,
		map[string]string{"action": "reject", "operator": "boot-admin"}); code != http.StatusOK {
		t.Fatalf("场景C reject → %d body=%s", code, body)
	}
	scenarioWaitSetpoint(t, base, viewToken, nodeID, sp2.SetpointID, "rejected", 15*time.Second)
	t.Logf("场景C 完成（%s applied / %s rejected 留痕）", sp.SetpointID, sp2.SetpointID)
}
